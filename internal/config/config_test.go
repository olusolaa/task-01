package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("HMAC_SECRET", "a_secret_long_enough_16")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.MetricsAddr != ":9090" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("default log level wrong: %v", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Fatalf("default shutdown timeout wrong: %v", cfg.ShutdownTimeout)
	}
	if cfg.DBMaxConns != 25 || cfg.DBMinConns != 5 {
		t.Fatalf("pool defaults wrong: %+v", cfg)
	}
}

func TestLoad_RequiredMissing(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("HMAC_SECRET", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") ||
		!strings.Contains(err.Error(), "HMAC_SECRET") {
		t.Fatalf("expected aggregated errors, got: %v", err)
	}
}

func TestLoad_HMACSecretTooShort(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("HMAC_SECRET", "short")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "at least 16 bytes") {
		t.Fatalf("expected too-short error, got: %v", err)
	}
}

func TestLoad_InvalidPoolBounds(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("HMAC_SECRET", "a_secret_long_enough_16")
	t.Setenv("DB_MIN_CONNS", "50")
	t.Setenv("DB_MAX_CONNS", "10")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "DB_MIN_CONNS") {
		t.Fatalf("expected min>max error, got: %v", err)
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("HMAC_SECRET", "a_secret_long_enough_16")
	t.Setenv("LOG_LEVEL", "silly")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("expected log-level error, got: %v", err)
	}
}
