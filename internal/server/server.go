package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/olusolaa/paybook/internal/payments"
	"github.com/olusolaa/paybook/internal/reconciliation"
)

type Config struct {
	HTTPAddr        string
	MetricsAddr     string
	HMACSecret      []byte
	ShutdownTimeout time.Duration
	Logger          *slog.Logger
	Pool            *pgxpool.Pool
	Payments        *payments.Service
	Reconciliation  *reconciliation.Service
	Registry        prometheus.Registerer
}

type Server struct {
	cfg            Config
	log            *slog.Logger
	pool           *pgxpool.Pool
	metrics        *Metrics
	payments       *payments.Service
	reconciliation *reconciliation.Service

	httpServer    *http.Server
	metricsServer *http.Server
}

func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
	if cfg.Registry == nil {
		cfg.Registry = prometheus.DefaultRegisterer
	}
	s := &Server{
		cfg:            cfg,
		log:            cfg.Logger,
		pool:           cfg.Pool,
		payments:       cfg.Payments,
		reconciliation: cfg.Reconciliation,
		metrics:        NewMetrics(cfg.Registry),
	}
	s.httpServer = &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           s.mainRouter(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	s.metricsServer = &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           s.metricsRouter(cfg.Registry),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Run starts both the public api server and the metrics server, waits for
// ctx to cancel, then drains in-flight requests with ShutdownTimeout.
// Returns nil on clean shutdown; returns the underlying listen error on
// startup failure.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		s.log.Info("api listening", "addr", s.cfg.HTTPAddr)
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	go func() {
		defer wg.Done()
		s.log.Info("metrics listening", "addr", s.cfg.MetricsAddr)
		if err := s.metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		s.log.Info("shutdown requested")
	case err := <-errCh:
		s.log.Error("server exited unexpectedly", "err", err)
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()

	var firstErr error
	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		s.log.Error("api shutdown", "err", err)
		firstErr = err
	}
	if err := s.metricsServer.Shutdown(shutdownCtx); err != nil {
		s.log.Error("metrics shutdown", "err", err)
		if firstErr == nil {
			firstErr = err
		}
	}
	wg.Wait()
	return firstErr
}

// HTTPHandler returns the fully-middlewared public router. Tests use it
// to drive the server via httptest.NewServer without starting a listener.
func (s *Server) HTTPHandler() http.Handler {
	return s.mainRouter()
}

func (s *Server) mainRouter() http.Handler {
	// HMAC only guards the write path. Balance and health endpoints are
	// readable without a signature so SRE can probe them directly.
	unsigned := http.NewServeMux()
	unsigned.HandleFunc("GET /customers/{id}/balance", s.handleBalance)
	unsigned.HandleFunc("GET /healthz", s.handleHealthz)
	unsigned.HandleFunc("GET /readyz", s.handleReadyz)

	signedApply := HMAC(s.cfg.HMACSecret, s.log)(http.HandlerFunc(s.handleApply))

	root := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/payments" {
			signedApply.ServeHTTP(w, r)
			return
		}
		unsigned.ServeHTTP(w, r)
	}))

	root = s.metrics.HTTPMiddleware(pathLabel)(root)
	root = Logger(s.log)(root)
	root = Recover(s.log)(root)
	root = RequestID(root)
	return root
}

func (s *Server) metricsRouter(reg prometheus.Registerer) http.Handler {
	mux := http.NewServeMux()
	gatherer, ok := reg.(prometheus.Gatherer)
	if !ok {
		gatherer = prometheus.DefaultGatherer
	}
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

// pathLabel maps a request path to a bounded-cardinality metric label so
// per-customer URLs do not blow up the prometheus series count.
func pathLabel(r *http.Request) string {
	p := r.URL.Path
	switch {
	case p == "/payments" && r.Method == http.MethodPost:
		return "/payments"
	case strings.HasPrefix(p, "/customers/") && strings.HasSuffix(p, "/balance"):
		return "/customers/:id/balance"
	case p == "/healthz":
		return "/healthz"
	case p == "/readyz":
		return "/readyz"
	default:
		return "other"
	}
}
