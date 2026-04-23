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

// TestShutdown_DrainsInFlight verifies that a graceful shutdown in the
// middle of concurrent payment traffic leaves the ledger in a consistent
// state: every request that returned 2xx was actually applied, and the
// stored balance matches value_kobo − (successes × amount). Requests that
// were in the kernel's accept queue when shutdown started may return
// connection errors; that's acceptable. What is not acceptable is a
// request that returned 201 but never reduced the balance, or a balance
// that dropped without a matching success response.
func TestShutdown_DrainsInFlight(t *testing.T) {
	pool := mustPool(t)
	ts := newServer(t, pool)
	f := newFixture(t, pool)

	const workers = 200
	const amount = int64(500)

	var wg sync.WaitGroup
	var applied, replayed, failed atomic.Int64
	start := make(chan struct{})
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			<-start
			body, _ := json.Marshal(defaultPayload(f, amount))
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/payments", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Signature", sign(body, testHMACSecret))
			resp, err := ts.Client().Do(req)
			if err != nil {
				failed.Add(1)
				return
			}
			defer resp.Body.Close()
			switch {
			case resp.Header.Get("Idempotent-Replayed") == "true":
				replayed.Add(1)
			case resp.StatusCode == http.StatusCreated:
				applied.Add(1)
			default:
				failed.Add(1)
			}
		}()
	}

	close(start)
	// Let some requests enter the server, then initiate graceful shutdown.
	time.Sleep(25 * time.Millisecond)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ts.Config.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("graceful shutdown returned error: %v", err)
	}

	wg.Wait()

	stored := getBalance(t, pool, f.DeploymentID)
	expected := valueKobo - applied.Load()*amount
	if stored != expected {
		t.Fatalf("balance mismatch after shutdown: stored=%d, expected=%d (applied=%d, replayed=%d, failed=%d)",
			stored, expected, applied.Load(), replayed.Load(), failed.Load())
	}
	if cnt := paymentCount(t, pool, f.DeploymentID); cnt != int(applied.Load()) {
		t.Fatalf("payment rows = %d, want %d", cnt, applied.Load())
	}
	t.Logf("shutdown drain: applied=%d replayed=%d failed=%d total=%d",
		applied.Load(), replayed.Load(), failed.Load(), workers)
	if applied.Load()+replayed.Load()+failed.Load() != int64(workers) {
		t.Fatalf("accounting: applied+replayed+failed != %d", workers)
	}
	// At least some requests must have completed successfully, otherwise
	// the test didn't actually exercise the drain path.
	if applied.Load() == 0 {
		t.Fatal("no requests completed; shutdown fired before any apply — tune the sleep")
	}
}
