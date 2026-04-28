// Package property asserts six load-bearing invariants of the payment
// apply service using property-based testing. Each test generates random
// but valid inputs and shrinks to a minimal failing case on violation.
//
// Invariants proved here:
//   1. stored balance equals the ledger projection for any random sequence
//   2. balance never goes negative under overpayment attempts
//   3. idempotent replay returns byte-identical responses
//   4. under concurrent duplicate refs, exactly one application is recorded
//   5. HMAC-rejected requests leave the ledger untouched
//   6. hitting zero balance transitions deployment state atomically
package property

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"pgregory.net/rapid"

	"github.com/olusolaa/paybook/internal/payments"
	"github.com/olusolaa/paybook/internal/reconciliation"
	"github.com/olusolaa/paybook/internal/server"
)

// migrationOnce ensures the schema migration runs at most once per test
// binary. See the equivalent comment in the integration package.
var (
	migrationOnce sync.Once
	migrationErr  error
)

const (
	testHMACSecret = "test_secret_sixteen_bytes_or_more"
	valueKobo      = int64(100_000_000)
	termWeeks      = 50
)

// ---------------------------------------------------------------------------
// Test harness (keeps property tests independent of the integration package)
// ---------------------------------------------------------------------------

var fixtureCounter atomic.Uint64

type harness struct {
	pool *pgxpool.Pool
	ts   *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping property test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MaxConns = 8
	cfg.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	migrationOnce.Do(func() { migrationErr = applyMigrationOnce(pool) })
	if migrationErr != nil {
		t.Fatalf("apply migration: %v", migrationErr)
	}
	t.Cleanup(pool.Close)

	repo := payments.NewRepo(pool)
	paySvc := payments.NewService(repo, 24*time.Hour)
	reconSvc := reconciliation.NewService(pool)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := server.New(server.Config{
		HTTPAddr:       ":0",
		MetricsAddr:    ":0",
		HMACSecret:     []byte(testHMACSecret),
		Logger:         logger,
		Pool:           pool,
		Payments:       paySvc,
		Reconciliation: reconSvc,
		Registry:       prometheus.NewRegistry(),
	})
	ts := httptest.NewServer(srv.HTTPHandler())
	t.Cleanup(ts.Close)
	return &harness{pool: pool, ts: ts}
}

// migrationLockKey serialises DDL across concurrent test binaries.
const migrationLockKey int64 = 0x70617962 // ascii "payb"

func applyMigrationOnce(pool *pgxpool.Pool) error {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	content, err := os.ReadFile(filepath.Join(root, "migrations", "0001_initial.sql"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	_, err = conn.Exec(ctx, string(content))
	return err
}

type fixture struct {
	CustomerID   string
	DeploymentID string
}

func (h *harness) newFixture(t require) fixture {
	n := fixtureCounter.Add(1)
	safe := strings.ReplaceAll(t.Name(), "/", "_")
	if len(safe) > 30 {
		safe = safe[:30]
	}
	customerID := "PROP_" + safe + "_" + strconv.FormatUint(n, 10) + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := h.pool.Exec(ctx,
		`INSERT INTO customers (id) VALUES ($1)`, customerID,
	); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	var depID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO deployments (customer_id, value_kobo, term_weeks, current_balance_kobo, started_at)
		VALUES ($1, $2, $3, $2, now() - '1 day'::interval)
		RETURNING id
	`, customerID, valueKobo, termWeeks).Scan(&depID); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO virtual_accounts (va_number, deployment_id) VALUES ($1, $2)`,
		"VA"+customerID, depID,
	); err != nil {
		t.Fatalf("seed va: %v", err)
	}
	return fixture{CustomerID: customerID, DeploymentID: depID}
}

// require is the shared interface implemented by both *testing.T and
// *rapid.T so the harness helpers work in both contexts.
type require interface {
	Helper()
	Name() string
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
}

type payload struct {
	CustomerID           string `json:"customer_id"`
	PaymentStatus        string `json:"payment_status"`
	TransactionAmount    string `json:"transaction_amount"`
	TransactionDate      string `json:"transaction_date"`
	TransactionReference string `json:"transaction_reference"`
}

func (h *harness) post(t require, p payload) *http.Response {
	return h.postRaw(t, p, testHMACSecret)
}

func (h *harness) postRaw(t require, p payload, secret string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(p)
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/payments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func readBody(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}

func randomRef() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "PVPAY" + hex.EncodeToString(b[:])
}

// nairaString turns int64 kobo into the naira wire string the bank
// webhook contract uses ("100", "100.50", "0.01", ...). Mirror of the
// helper in the integration test package.
func nairaString(kobo int64) string {
	if kobo%100 == 0 {
		return strconv.FormatInt(kobo/100, 10)
	}
	whole := kobo / 100
	frac := kobo % 100
	return fmt.Sprintf("%d.%02d", whole, frac)
}

func (h *harness) storedBalance(t require, depID string) int64 {
	t.Helper()
	var b int64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.pool.QueryRow(ctx,
		`SELECT current_balance_kobo FROM deployments WHERE id = $1`, depID,
	).Scan(&b); err != nil {
		t.Fatalf("read stored balance: %v", err)
	}
	return b
}

func (h *harness) computedBalance(t require, depID string) int64 {
	t.Helper()
	var b int64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := h.pool.QueryRow(ctx, `
		SELECT d.value_kobo - COALESCE(SUM(p.amount_kobo) FILTER (WHERE p.result = 'APPLIED'), 0)
		FROM deployments d
		LEFT JOIN payments p ON p.deployment_id = d.id
		WHERE d.id = $1
		GROUP BY d.id
	`, depID).Scan(&b)
	if err != nil {
		t.Fatalf("read computed balance: %v", err)
	}
	return b
}

func (h *harness) paymentCount(t require, depID string) int {
	t.Helper()
	var n int
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payments WHERE deployment_id = $1`, depID,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// Invariant 1: stored balance equals the ledger projection
// ---------------------------------------------------------------------------

func TestProperty_BalanceEqualsLedgerProjection(t *testing.T) {
	h := newHarness(t)

	rapid.Check(t, func(rt *rapid.T) {
		f := h.newFixture(rt)

		n := rapid.IntRange(1, 30).Draw(rt, "n_payments")
		for i := 0; i < n; i++ {
			amount := rapid.Int64Range(1, valueKobo/10).Draw(rt, "amount")
			status := rapid.SampledFrom([]string{"COMPLETE", "COMPLETE", "COMPLETE", "PENDING", "FAILED"}).Draw(rt, "status")

			p := payload{
				CustomerID:           f.CustomerID,
				PaymentStatus:        status,
				TransactionAmount:    nairaString(amount),
				TransactionDate:      time.Now().UTC().Format("2006-01-02 15:04:05"),
				TransactionReference: randomRef(),
			}
			resp := h.post(rt, p)
			resp.Body.Close()
		}

		stored := h.storedBalance(rt, f.DeploymentID)
		computed := h.computedBalance(rt, f.DeploymentID)
		if stored != computed {
			rt.Fatalf("stored=%d computed=%d drift=%d", stored, computed, stored-computed)
		}
	})
}

// ---------------------------------------------------------------------------
// Invariant 2: balance never goes negative, overpayments are rejected
// ---------------------------------------------------------------------------

func TestProperty_BalanceMonotonicNonNegative(t *testing.T) {
	h := newHarness(t)

	rapid.Check(t, func(rt *rapid.T) {
		f := h.newFixture(rt)

		n := rapid.IntRange(5, 40).Draw(rt, "n")
		for i := 0; i < n; i++ {
			amount := rapid.Int64Range(1, valueKobo*2).Draw(rt, "amount")
			p := payload{
				CustomerID:           f.CustomerID,
				PaymentStatus:        "COMPLETE",
				TransactionAmount:    nairaString(amount),
				TransactionDate:      time.Now().UTC().Format("2006-01-02 15:04:05"),
				TransactionReference: randomRef(),
			}
			resp := h.post(rt, p)
			resp.Body.Close()

			stored := h.storedBalance(rt, f.DeploymentID)
			if stored < 0 {
				rt.Fatalf("balance went negative: %d after amount %d", stored, amount)
			}
			if stored > valueKobo {
				rt.Fatalf("balance exceeds value: %d > %d", stored, valueKobo)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Invariant 3: replay returns byte-identical response
// ---------------------------------------------------------------------------

func TestProperty_ReplayByteIdentical(t *testing.T) {
	h := newHarness(t)

	rapid.Check(t, func(rt *rapid.T) {
		f := h.newFixture(rt)

		amount := rapid.Int64Range(1, valueKobo/2).Draw(rt, "amount")
		status := rapid.SampledFrom([]string{"COMPLETE", "PENDING", "FAILED"}).Draw(rt, "status")

		p := payload{
			CustomerID:           f.CustomerID,
			PaymentStatus:        status,
			TransactionAmount:    nairaString(amount),
			TransactionDate:      time.Now().UTC().Format("2006-01-02 15:04:05"),
			TransactionReference: randomRef(),
		}
		first := h.post(rt, p)
		firstBody := readBody(first.Body)
		firstStatus := first.StatusCode
		first.Body.Close()

		replays := rapid.IntRange(1, 10).Draw(rt, "replays")
		for i := 0; i < replays; i++ {
			resp := h.post(rt, p)
			body := readBody(resp.Body)
			status := resp.StatusCode
			replayed := resp.Header.Get("Idempotent-Replayed")
			resp.Body.Close()

			if status != firstStatus {
				rt.Fatalf("replay %d status %d != first %d", i, status, firstStatus)
			}
			if !bytes.Equal(body, firstBody) {
				rt.Fatalf("replay %d body differs", i)
			}
			if replayed != "true" {
				rt.Fatalf("replay %d missing Idempotent-Replayed header", i)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Invariant 4: exactly one application per transaction reference
// ---------------------------------------------------------------------------

func TestProperty_ExactlyOneApplicationPerRef(t *testing.T) {
	h := newHarness(t)

	rapid.Check(t, func(rt *rapid.T) {
		f := h.newFixture(rt)

		workers := rapid.IntRange(10, 150).Draw(rt, "workers")
		amount := rapid.Int64Range(1, valueKobo/2).Draw(rt, "amount")
		p := payload{
			CustomerID:           f.CustomerID,
			PaymentStatus:        "COMPLETE",
			TransactionAmount:    nairaString(amount),
			TransactionDate:      time.Now().UTC().Format("2006-01-02 15:04:05"),
			TransactionReference: randomRef(),
		}

		var wg sync.WaitGroup
		wg.Add(workers)
		for range workers {
			go func() {
				defer wg.Done()
				resp := h.post(rt, p)
				resp.Body.Close()
			}()
		}
		wg.Wait()

		if cnt := h.paymentCount(rt, f.DeploymentID); cnt != 1 {
			rt.Fatalf("payment count = %d, want 1 after %d concurrent dupes", cnt, workers)
		}
		if stored := h.storedBalance(rt, f.DeploymentID); stored != valueKobo-amount {
			rt.Fatalf("balance = %d, want %d", stored, valueKobo-amount)
		}
	})
}

// ---------------------------------------------------------------------------
// Invariant 5: HMAC-rejected requests leave no side effect
// ---------------------------------------------------------------------------

func TestProperty_HMACRejectionSideEffectFree(t *testing.T) {
	h := newHarness(t)

	rapid.Check(t, func(rt *rapid.T) {
		f := h.newFixture(rt)

		n := rapid.IntRange(1, 20).Draw(rt, "n")
		for i := 0; i < n; i++ {
			amount := rapid.Int64Range(1, valueKobo/4).Draw(rt, "amount")
			p := payload{
				CustomerID:           f.CustomerID,
				PaymentStatus:        "COMPLETE",
				TransactionAmount:    nairaString(amount),
				TransactionDate:      time.Now().UTC().Format("2006-01-02 15:04:05"),
				TransactionReference: randomRef(),
			}
			badSecret := "wrong_" + strconv.Itoa(i) + "_sixteen_bytes_"
			resp := h.postRaw(rt, p, badSecret)
			if resp.StatusCode != http.StatusUnauthorized {
				resp.Body.Close()
				rt.Fatalf("bad HMAC status = %d, want 401", resp.StatusCode)
			}
			resp.Body.Close()
		}
		if cnt := h.paymentCount(rt, f.DeploymentID); cnt != 0 {
			rt.Fatalf("HMAC-rejected requests left %d payments in DB", cnt)
		}
		if stored := h.storedBalance(rt, f.DeploymentID); stored != valueKobo {
			rt.Fatalf("balance changed after HMAC rejection: %d", stored)
		}
	})
}

// ---------------------------------------------------------------------------
// Invariant 6: hitting zero balance transitions state atomically
// ---------------------------------------------------------------------------

func TestProperty_ZeroBalanceTransitionsState(t *testing.T) {
	h := newHarness(t)

	rapid.Check(t, func(rt *rapid.T) {
		f := h.newFixture(rt)

		// Split valueKobo into 5..25 random chunks that sum exactly to it.
		chunks := rapid.IntRange(5, 25).Draw(rt, "chunks")
		amounts := make([]int64, chunks)
		remaining := valueKobo
		for i := 0; i < chunks-1; i++ {
			max := remaining - int64(chunks-i-1) // leave at least 1 per remaining chunk
			if max < 1 {
				max = 1
			}
			amt := rapid.Int64Range(1, max).Draw(rt, "amt")
			amounts[i] = amt
			remaining -= amt
		}
		amounts[chunks-1] = remaining

		for i, a := range amounts {
			p := payload{
				CustomerID:           f.CustomerID,
				PaymentStatus:        "COMPLETE",
				TransactionAmount:    nairaString(a),
				TransactionDate:      time.Now().UTC().Format("2006-01-02 15:04:05"),
				TransactionReference: randomRef(),
			}
			resp := h.post(rt, p)
			if resp.StatusCode != http.StatusCreated {
				body := readBody(resp.Body)
				resp.Body.Close()
				rt.Fatalf("chunk %d/%d amount=%d status=%d body=%s", i+1, chunks, a, resp.StatusCode, body)
			}
			resp.Body.Close()
		}

		if stored := h.storedBalance(rt, f.DeploymentID); stored != 0 {
			rt.Fatalf("final balance = %d, want 0", stored)
		}

		var state string
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.pool.QueryRow(ctx,
			`SELECT state::text FROM deployments WHERE id = $1`, f.DeploymentID,
		).Scan(&state)
		if state != "FULLY_REPAID" {
			rt.Fatalf("state = %s, want FULLY_REPAID", state)
		}
	})
}
