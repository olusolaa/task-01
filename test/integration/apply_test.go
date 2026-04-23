package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestApply_HappyPath(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	payload := defaultPayload(f, 10_000)
	resp := post(t, ts, payload)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", resp.StatusCode, readAll(t, resp.Body))
	}
	if replay := resp.Header.Get("Idempotent-Replayed"); replay != "" {
		t.Fatalf("unexpected Idempotent-Replayed header on first apply: %q", replay)
	}

	var body map[string]any
	if err := json.Unmarshal(readAll(t, resp.Body), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "applied" {
		t.Fatalf("body status = %v", body["status"])
	}
	if got := int64(body["balance_after_kobo"].(float64)); got != valueKobo-10_000 {
		t.Fatalf("balance_after = %d", got)
	}
	if stored := getBalance(t, pool, f.DeploymentID); stored != valueKobo-10_000 {
		t.Fatalf("db balance = %d", stored)
	}
}

func TestApply_IdempotentReplay_ByteIdentical(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	payload := defaultPayload(f, 25_000)

	first := post(t, ts, payload)
	firstBody := readAll(t, first.Body)
	firstStatus := first.StatusCode

	second := post(t, ts, payload)
	secondBody := readAll(t, second.Body)

	if second.StatusCode != firstStatus {
		t.Fatalf("replay status %d != original %d", second.StatusCode, firstStatus)
	}
	if !bytes.Equal(firstBody, secondBody) {
		t.Fatalf("replay body differs\nfirst:  %s\nsecond: %s", firstBody, secondBody)
	}
	if second.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("missing Idempotent-Replayed: true on replay")
	}
	if cnt := paymentCount(t, pool, f.DeploymentID); cnt != 1 {
		t.Fatalf("expected 1 payment, got %d", cnt)
	}
	if stored := getBalance(t, pool, f.DeploymentID); stored != valueKobo-25_000 {
		t.Fatalf("balance drifted: %d", stored)
	}
}

func TestApply_ConcurrentDuplicates_ExactlyOneApplied(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	payload := defaultPayload(f, 50_000)

	const workers = 100
	var wg sync.WaitGroup
	var applied, replayed atomic.Int64
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			resp := post(t, ts, payload)
			if resp.Header.Get("Idempotent-Replayed") == "true" {
				replayed.Add(1)
			} else if resp.StatusCode == http.StatusCreated {
				applied.Add(1)
			}
		}()
	}
	wg.Wait()

	if applied.Load() != 1 {
		t.Fatalf("applied = %d, want 1", applied.Load())
	}
	if applied.Load()+replayed.Load() != workers {
		t.Fatalf("applied+replayed = %d+%d != %d", applied.Load(), replayed.Load(), workers)
	}
	if stored := getBalance(t, pool, f.DeploymentID); stored != valueKobo-50_000 {
		t.Fatalf("balance = %d, want %d", stored, valueKobo-50_000)
	}
	if cnt := paymentCount(t, pool, f.DeploymentID); cnt != 1 {
		t.Fatalf("expected 1 payment row, got %d", cnt)
	}
}

func TestApply_ConcurrentDifferentRefs_BalanceConverges(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	const workers = 50
	const each = int64(1_000)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			p := defaultPayload(f, each)
			resp := post(t, ts, p)
			if resp.StatusCode != http.StatusCreated {
				t.Errorf("unexpected status: %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	if stored := getBalance(t, pool, f.DeploymentID); stored != valueKobo-workers*each {
		t.Fatalf("balance = %d, want %d", stored, valueKobo-workers*each)
	}
	if cnt := paymentCount(t, pool, f.DeploymentID); cnt != workers {
		t.Fatalf("payment count = %d, want %d", cnt, workers)
	}
}

func TestApply_UnknownCustomer(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)

	payload := Payload{
		CustomerID:           "GIG_DOES_NOT_EXIST_XYZ",
		PaymentStatus:        "COMPLETE",
		TransactionAmount:    "100",
		TransactionDate:      time.Now().UTC().Format("2006-01-02 15:04:05"),
		TransactionReference: randomRef(),
	}
	resp := post(t, ts, payload)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", resp.StatusCode, readAll(t, resp.Body))
	}
}

func TestApply_Overpayment(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	payload := defaultPayload(f, valueKobo+1)
	resp := post(t, ts, payload)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var body map[string]any
	_ = json.Unmarshal(readAll(t, resp.Body), &body)
	if body["error"] != "overpayment" {
		t.Fatalf("error = %v, want overpayment", body["error"])
	}
	if stored := getBalance(t, pool, f.DeploymentID); stored != valueKobo {
		t.Fatalf("balance changed: %d", stored)
	}

	// The rejected payment is still recorded for audit.
	if cnt := paymentCount(t, pool, f.DeploymentID); cnt != 1 {
		t.Fatalf("expected rejected row recorded, got %d", cnt)
	}
}

func TestApply_NonCompleteStatus_Recorded(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	payload := defaultPayload(f, 1_000)
	payload.PaymentStatus = "PENDING"
	resp := post(t, ts, payload)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if stored := getBalance(t, pool, f.DeploymentID); stored != valueKobo {
		t.Fatalf("balance changed: %d", stored)
	}
	if cnt := paymentCount(t, pool, f.DeploymentID); cnt != 1 {
		t.Fatalf("expected recorded row, got %d", cnt)
	}
}

func TestApply_BadHMAC_NothingPersisted(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	payload := defaultPayload(f, 10_000)
	resp := postRaw(t, ts, payload, "wrong_secret_wrong_secret")

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if cnt := paymentCount(t, pool, f.DeploymentID); cnt != 0 {
		t.Fatalf("expected 0 payments after bad HMAC, got %d", cnt)
	}
	if stored := getBalance(t, pool, f.DeploymentID); stored != valueKobo {
		t.Fatalf("balance changed after bad HMAC: %d", stored)
	}
}

func TestApply_InvalidAmount(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	cases := []string{"", "0", "-1", "10.00", "1e4", "abc", " 10", "01"}
	for _, amount := range cases {
		t.Run(amount, func(t *testing.T) {
			p := defaultPayload(f, 1)
			p.TransactionAmount = amount
			resp := post(t, ts, p)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("amount %q: status = %d, want 400", amount, resp.StatusCode)
			}
		})
	}
}

func TestApply_FullRepaymentTransitionsState(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	// 50 weekly installments of 2_000_000 kobo (₦20,000) pays off the ₦1m.
	const weeks = 50
	const each = int64(2_000_000)
	for i := range weeks {
		p := defaultPayload(f, each)
		resp := post(t, ts, p)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("week %d: status %d; body %s", i+1, resp.StatusCode, readAll(t, resp.Body))
		}
	}

	if stored := getBalance(t, pool, f.DeploymentID); stored != 0 {
		t.Fatalf("balance = %d, want 0 after full repayment", stored)
	}

	var state string
	_ = pool.QueryRow(context.Background(),
		`SELECT state::text FROM deployments WHERE id = $1`, f.DeploymentID,
	).Scan(&state)
	if state != "FULLY_REPAID" {
		t.Fatalf("state = %s, want FULLY_REPAID", state)
	}

	// Next payment rejected because deployment no longer active.
	followUp := defaultPayload(f, 100)
	resp := post(t, ts, followUp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("post-closure status = %d, want 409", resp.StatusCode)
	}
}

func TestBalance_ReportsStoredComputedAndDrift(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	// Apply a payment so balance is non-trivial.
	post(t, ts, defaultPayload(f, 15_000))

	resp, err := ts.Client().Get(ts.URL + "/customers/" + f.CustomerID + "/balance")
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var out struct {
		CustomerID  string `json:"customer_id"`
		Deployments []struct {
			State               string `json:"state"`
			ValueKobo           int64  `json:"value_kobo"`
			StoredBalanceKobo   int64  `json:"stored_balance_kobo"`
			ComputedBalanceKobo int64  `json:"computed_balance_kobo"`
			DriftKobo           int64  `json:"drift_kobo"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(readAll(t, resp.Body), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Deployments) != 1 {
		t.Fatalf("got %d deployments", len(out.Deployments))
	}
	d := out.Deployments[0]
	if d.StoredBalanceKobo != valueKobo-15_000 {
		t.Fatalf("stored = %d", d.StoredBalanceKobo)
	}
	if d.ComputedBalanceKobo != valueKobo-15_000 {
		t.Fatalf("computed = %d", d.ComputedBalanceKobo)
	}
	if d.DriftKobo != 0 {
		t.Fatalf("drift = %d, want 0", d.DriftKobo)
	}
}

// TestBalance_DetectsDrift deliberately poisons the stored balance cache to
// prove the reconciliation endpoint surfaces drift rather than papering over
// it. In production this would fire an alert, not just show up in JSON.
func TestBalance_DetectsDrift(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	post(t, ts, defaultPayload(f, 10_000))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`UPDATE deployments SET current_balance_kobo = current_balance_kobo - 500 WHERE id = $1`,
		f.DeploymentID,
	)
	if err != nil {
		t.Fatalf("poison balance: %v", err)
	}

	resp, err := ts.Client().Get(ts.URL + "/customers/" + f.CustomerID + "/balance")
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	defer resp.Body.Close()

	var out struct {
		Deployments []struct {
			DriftKobo int64 `json:"drift_kobo"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(readAll(t, resp.Body), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Deployments[0].DriftKobo != -500 {
		t.Fatalf("drift = %d, want -500", out.Deployments[0].DriftKobo)
	}
}

func TestApply_MissingSignature(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	payload := defaultPayload(f, 1_000)
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/payments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// deliberately no X-Signature

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
