package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/olusolaa/paybook/internal/payments"
	"github.com/olusolaa/paybook/internal/reconciliation"
	"github.com/olusolaa/paybook/internal/server"
)

const (
	testHMACSecret = "test_secret_sixteen_bytes_or_more"
	valueKobo      = int64(100_000_000) // ₦1,000,000
	termWeeks      = 50
)

// dbURL returns TEST_DATABASE_URL or skips the test.
func dbURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return url
}

// mustPool opens a pool against the test database. It applies the migration
// idempotently so a naked postgres can be used as the test target.
func mustPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL(t))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	applyMigration(t, pool)
	t.Cleanup(pool.Close)
	return pool
}

func applyMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	content, err := os.ReadFile(filepath.Join(root, "migrations", "0001_initial.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, string(content)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
}

// Fixture is a freshly seeded customer+deployment+virtual-account tuple
// with unique IDs derived from the test name and a monotonic counter.
type Fixture struct {
	CustomerID   string
	DeploymentID string
	VANumber     string
}

var fixtureCounter atomic.Uint64

// newFixture creates a unique customer with one active deployment of
// valueKobo and its own virtual account. Cleanup is not required because
// IDs are unique per test run.
func newFixture(t *testing.T, pool *pgxpool.Pool) Fixture {
	t.Helper()
	n := fixtureCounter.Add(1)
	suffix := "_" + strconv.FormatUint(n, 10) + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	safe := strings.ReplaceAll(t.Name(), "/", "_")
	if len(safe) > 40 {
		safe = safe[:40]
	}
	customerID := "TEST_" + safe + suffix

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx,
		`INSERT INTO customers (id) VALUES ($1)`, customerID,
	); err != nil {
		t.Fatalf("seed customer: %v", err)
	}

	var depID string
	err := pool.QueryRow(ctx, `
		INSERT INTO deployments (customer_id, value_kobo, term_weeks, current_balance_kobo, started_at)
		VALUES ($1, $2, $3, $2, now() - '1 day'::interval)
		RETURNING id
	`, customerID, valueKobo, termWeeks).Scan(&depID)
	if err != nil {
		t.Fatalf("seed deployment: %v", err)
	}

	vaNumber := "VA" + customerID
	if _, err := pool.Exec(ctx,
		`INSERT INTO virtual_accounts (va_number, deployment_id) VALUES ($1, $2)`,
		vaNumber, depID,
	); err != nil {
		t.Fatalf("seed va: %v", err)
	}

	return Fixture{CustomerID: customerID, DeploymentID: depID, VANumber: vaNumber}
}

// newServer builds a server wired to the given pool and returns an
// httptest server running its public handler. A fresh prometheus registry
// is used so repeated calls from different tests do not collide on
// collector registration.
func newServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()
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
	return ts
}

// Payload is the JSON payload shape used in tests.
type Payload struct {
	CustomerID           string `json:"customer_id"`
	PaymentStatus        string `json:"payment_status"`
	TransactionAmount    string `json:"transaction_amount"`
	TransactionDate      string `json:"transaction_date"`
	TransactionReference string `json:"transaction_reference"`
}

// defaultPayload returns a valid payload for the given fixture.
func defaultPayload(f Fixture, amountKobo int64) Payload {
	return Payload{
		CustomerID:           f.CustomerID,
		PaymentStatus:        "COMPLETE",
		TransactionAmount:    strconv.FormatInt(amountKobo, 10),
		TransactionDate:      time.Now().UTC().Format("2006-01-02 15:04:05"),
		TransactionReference: randomRef(),
	}
}

func randomRef() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "VPAY" + hex.EncodeToString(b[:])
}

func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// post sends a signed POST to /payments against the test server.
func post(t *testing.T, ts *httptest.Server, payload Payload) *http.Response {
	t.Helper()
	return postRaw(t, ts, payload, testHMACSecret)
}

// postRaw allows an overridden secret (for testing HMAC rejection).
func postRaw(t *testing.T, ts *httptest.Server, payload Payload, secret string) *http.Response {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/payments", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", sign(body, secret))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

// getBalance is the stored balance on the deployment row.
func getBalance(t *testing.T, pool *pgxpool.Pool, depID string) int64 {
	t.Helper()
	var b int64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := pool.QueryRow(ctx,
		`SELECT current_balance_kobo FROM deployments WHERE id = $1`, depID,
	).Scan(&b)
	if err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return b
}

// paymentCount is the total payments stored for a deployment.
func paymentCount(t *testing.T, pool *pgxpool.Pool, depID string) int {
	t.Helper()
	var n int
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payments WHERE deployment_id = $1`, depID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count payments: %v", err)
	}
	return n
}
