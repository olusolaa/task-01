package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/olusolaa/paybook/internal/config"
	"github.com/olusolaa/paybook/internal/payments"
	"github.com/olusolaa/paybook/internal/reconciliation"
	"github.com/olusolaa/paybook/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = cfg.DBMaxConns
	poolCfg.MinConns = cfg.DBMinConns

	pool, err := pgxpool.NewWithConfig(rootCtx, poolCfg)
	if err != nil {
		return fmt.Errorf("open pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(rootCtx); err != nil {
		return fmt.Errorf("ping db: %w", err)
	}
	logger.Info("db pool ready",
		"max_conns", cfg.DBMaxConns,
		"min_conns", cfg.DBMinConns,
	)

	paymentsRepo := payments.NewRepo(pool)
	paymentsSvc := payments.NewService(paymentsRepo, cfg.ClockSkewGrace)
	reconSvc := reconciliation.NewService(pool)

	srv := server.New(server.Config{
		HTTPAddr:        cfg.HTTPAddr,
		MetricsAddr:     cfg.MetricsAddr,
		HMACSecret:      cfg.HMACSecret,
		ShutdownTimeout: cfg.ShutdownTimeout,
		Logger:          logger,
		Pool:            pool,
		Payments:        paymentsSvc,
		Reconciliation:  reconSvc,
	})

	if err := srv.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("bye")
	return nil
}
