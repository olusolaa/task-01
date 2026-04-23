// Package config loads service configuration from environment variables.
// It fails at startup on any missing required value so the process never
// accepts traffic with a half-formed config.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr        string
	MetricsAddr     string
	DatabaseURL     string
	HMACSecret      []byte
	LogLevel        slog.Level
	DBMaxConns      int32
	DBMinConns      int32
	ShutdownTimeout time.Duration
	ClockSkewGrace  time.Duration
}

func Load() (*Config, error) {
	cfg := &Config{
		HTTPAddr:        getenv("HTTP_ADDR", ":8080"),
		MetricsAddr:     getenv("METRICS_ADDR", ":9090"),
		ShutdownTimeout: 30 * time.Second,
		ClockSkewGrace:  24 * time.Hour,
		DBMaxConns:      25,
		DBMinConns:      5,
	}

	var errs []error

	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}

	secret := os.Getenv("HMAC_SECRET")
	if secret == "" {
		errs = append(errs, errors.New("HMAC_SECRET is required"))
	} else if len(secret) < 16 {
		errs = append(errs, errors.New("HMAC_SECRET must be at least 16 bytes"))
	}
	cfg.HMACSecret = []byte(secret)

	level, err := parseLogLevel(getenv("LOG_LEVEL", "info"))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.LogLevel = level

	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("DB_MAX_CONNS: %q is not a positive integer", v))
		} else {
			cfg.DBMaxConns = int32(n)
		}
	}
	if v := os.Getenv("DB_MIN_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("DB_MIN_CONNS: %q is not a non-negative integer", v))
		} else {
			cfg.DBMinConns = int32(n)
		}
	}
	if cfg.DBMinConns > cfg.DBMaxConns {
		errs = append(errs, fmt.Errorf("DB_MIN_CONNS (%d) must be <= DB_MAX_CONNS (%d)",
			cfg.DBMinConns, cfg.DBMaxConns))
	}

	if v := os.Getenv("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err))
		} else {
			cfg.ShutdownTimeout = d
		}
	}
	if v := os.Getenv("CLOCK_SKEW_GRACE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("CLOCK_SKEW_GRACE: %w", err))
		} else {
			cfg.ClockSkewGrace = d
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseLogLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("LOG_LEVEL: %q is not one of debug|info|warn|error", s)
}
