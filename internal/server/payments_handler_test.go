package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/olusolaa/paybook/internal/payments"
	"github.com/olusolaa/paybook/internal/reconciliation"
)

// fakePayments lets each handler test script the ValidateAndParse and
// Apply calls without standing up a real service or database.
type fakePayments struct {
	validateFn func(payments.RawInput) (payments.Payment, error)
	applyFn    func(context.Context, payments.Payment) (*payments.Outcome, error)
	applyCalls int
}

func (f *fakePayments) ValidateAndParse(in payments.RawInput) (payments.Payment, error) {
	if f.validateFn != nil {
		return f.validateFn(in)
	}
	return payments.Payment{
		CustomerID:           in.CustomerID,
		TransactionReference: in.TransactionReference,
		Status:               payments.StatusComplete,
	}, nil
}

func (f *fakePayments) Apply(ctx context.Context, p payments.Payment) (*payments.Outcome, error) {
	f.applyCalls++
	if f.applyFn != nil {
		return f.applyFn(ctx, p)
	}
	return nil, nil
}

type fakeReconciliation struct{}

func (f *fakeReconciliation) ForCustomer(context.Context, string) (*reconciliation.CustomerBalance, error) {
	return nil, nil
}

const testSecret = "test_secret_sixteen_bytes_or_more"

func newHandlerTestServer(t *testing.T, fp *fakePayments) *httptest.Server {
	t.Helper()
	srv := New(Config{
		HTTPAddr:       ":0",
		MetricsAddr:    ":0",
		HMACSecret:     []byte(testSecret),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Payments:       fp,
		Reconciliation: &fakeReconciliation{},
		Registry:       prometheus.NewRegistry(),
	})
	ts := httptest.NewServer(srv.HTTPHandler())
	t.Cleanup(ts.Close)
	return ts
}

func signed(body []byte) (*bytes.Reader, string) {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return bytes.NewReader(body), hex.EncodeToString(mac.Sum(nil))
}

func validRawBody() []byte {
	b, _ := json.Marshal(map[string]string{
		"customer_id":           "GIG00001",
		"payment_status":        "COMPLETE",
		"transaction_amount":    "10000",
		"transaction_date":      "2025-11-07 14:54:16",
		"transaction_reference": "HANDLERTEST1",
	})
	return b
}

func TestHandleApply_MapsCustomerNotFoundTo404(t *testing.T) {
	fp := &fakePayments{
		applyFn: func(context.Context, payments.Payment) (*payments.Outcome, error) {
			return nil, payments.ErrCustomerNotFound
		},
	}
	ts := newHandlerTestServer(t, fp)

	body, sig := signed(validRawBody())
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/payments", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", sig)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if fp.applyCalls != 1 {
		t.Fatalf("apply called %d times, want 1", fp.applyCalls)
	}
}

func TestHandleApply_MapsInvalidAmountTo400(t *testing.T) {
	fp := &fakePayments{
		validateFn: func(payments.RawInput) (payments.Payment, error) {
			return payments.Payment{}, payments.ErrInvalidAmount
		},
	}
	ts := newHandlerTestServer(t, fp)

	body, sig := signed(validRawBody())
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/payments", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", sig)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if fp.applyCalls != 0 {
		t.Fatalf("apply called after validation failure: %d", fp.applyCalls)
	}
}

func TestHandleApply_ReturnsOutcomeBytesVerbatim(t *testing.T) {
	canned := []byte(`{"status":"applied","deployment_id":"X","balance_after_kobo":1}`)
	fp := &fakePayments{
		applyFn: func(context.Context, payments.Payment) (*payments.Outcome, error) {
			return &payments.Outcome{
				Replayed: false,
				Result:   payments.ResultApplied,
				Status:   http.StatusCreated,
				Body:     canned,
			}, nil
		},
	}
	ts := newHandlerTestServer(t, fp)

	body, sig := signed(validRawBody())
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/payments", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", sig)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if !bytes.Equal(got, canned) {
		t.Fatalf("body = %q, want %q", got, canned)
	}
	if resp.Header.Get("Idempotent-Replayed") != "" {
		t.Fatal("Idempotent-Replayed header set on non-replay")
	}
}

func TestHandleApply_ReplaySetsHeader(t *testing.T) {
	canned := []byte(`{"status":"applied"}`)
	fp := &fakePayments{
		applyFn: func(context.Context, payments.Payment) (*payments.Outcome, error) {
			return &payments.Outcome{
				Replayed: true,
				Result:   payments.ResultApplied,
				Status:   http.StatusCreated,
				Body:     canned,
			}, nil
		},
	}
	ts := newHandlerTestServer(t, fp)

	body, sig := signed(validRawBody())
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/payments", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", sig)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("missing Idempotent-Replayed: true on replay")
	}
}

func TestHandleApply_RejectsBadHMACBeforeService(t *testing.T) {
	fp := &fakePayments{}
	ts := newHandlerTestServer(t, fp)

	body := validRawBody()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/payments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", hex.EncodeToString(make([]byte, 32)))

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if fp.applyCalls != 0 {
		t.Fatal("apply was called despite HMAC failure")
	}
}

func TestHandleApply_RejectsMalformedJSONBefore400(t *testing.T) {
	fp := &fakePayments{}
	ts := newHandlerTestServer(t, fp)

	body, sig := signed([]byte(`{not json`))
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/payments", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", sig)

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if fp.applyCalls != 0 {
		t.Fatal("apply was called on malformed JSON")
	}
}
