package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ---------------------------------------------------------------------------
// Request ID
// ---------------------------------------------------------------------------

type ctxKey struct{ name string }

var requestIDKey = ctxKey{"request_id"}

// RequestID middleware ensures every request has a hex request id. It honors
// an incoming X-Request-ID header so upstream correlators (load balancer,
// webhook broker) can stitch logs across hops.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the request id set by the RequestID middleware,
// or "" if there is none (e.g. a handler called outside the HTTP chain).
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Request logging
// ---------------------------------------------------------------------------

// Logger logs one line per request with status, duration, and request id.
// It runs before Metrics in the chain so status codes are captured the same
// way by both (via statusRecorder).
func Logger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			log.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", RequestIDFromContext(r.Context()),
			)
		})
	}
}

// ---------------------------------------------------------------------------
// Panic recovery
// ---------------------------------------------------------------------------

func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic",
						"err", rec,
						"path", r.URL.Path,
						"request_id", RequestIDFromContext(r.Context()),
					)
					writeError(w, http.StatusInternalServerError, "internal", "")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------------
// HMAC signature verification
// ---------------------------------------------------------------------------

// HMAC verifies the request body matches the X-Signature header, computed
// as hex(HMAC-SHA256(secret, body)). A missing, malformed, or mismatched
// signature returns 401 without invoking downstream handlers. The body is
// fully read and restored so handlers can decode it normally.
func HMAC(secret []byte, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sig := r.Header.Get("X-Signature")
			if sig == "" {
				writeError(w, http.StatusUnauthorized, "missing_signature", "")
				return
			}
			sigBytes, err := hex.DecodeString(sig)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "invalid_signature", "")
				return
			}

			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				writeError(w, http.StatusBadRequest, "body_read", err.Error())
				return
			}
			mac := hmac.New(sha256.New, secret)
			mac.Write(body)
			expected := mac.Sum(nil)

			if !hmac.Equal(sigBytes, expected) {
				log.Warn("hmac rejected",
					"path", r.URL.Path,
					"request_id", RequestIDFromContext(r.Context()),
				)
				writeError(w, http.StatusUnauthorized, "invalid_signature", "")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------------
// Prometheus metrics
// ---------------------------------------------------------------------------

// Metrics holds the collectors for HTTP traffic and payment outcomes.
// Callers register it with a prometheus.Registerer once at startup.
type Metrics struct {
	httpRequests    *prometheus.CounterVec
	httpLatency     *prometheus.HistogramVec
	paymentsByState *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		httpRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "paybook_http_requests_total",
				Help: "HTTP requests by method, path, status",
			},
			[]string{"method", "path", "status"},
		),
		httpLatency: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "paybook_http_request_duration_seconds",
				Help:    "HTTP request latency",
				Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
			},
			[]string{"method", "path"},
		),
		paymentsByState: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "paybook_payments_total",
				Help: "Payment outcomes (APPLIED, RECORDED, REJECTED, REPLAYED)",
			},
			[]string{"result"},
		),
	}
	reg.MustRegister(m.httpRequests, m.httpLatency, m.paymentsByState)
	return m
}

func (m *Metrics) ObservePayment(result string) {
	m.paymentsByState.WithLabelValues(result).Inc()
}

// HTTPMiddleware instruments every request. pathLabel maps the request to a
// bounded-cardinality label (e.g. "/payments", "/customers/:id/balance") so
// the metric series doesn't explode on path variables.
func (m *Metrics) HTTPMiddleware(pathLabel func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			path := pathLabel(r)
			m.httpRequests.WithLabelValues(r.Method, path, strconv.Itoa(rec.status)).Inc()
			m.httpLatency.WithLabelValues(r.Method, path).Observe(time.Since(start).Seconds())
		})
	}
}

// statusRecorder captures the first-written status so middleware can record it.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.wroteHeader {
		return
	}
	s.status = status
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}
