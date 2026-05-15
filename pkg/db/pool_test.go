package db

import (
	"testing"
	"time"
)

func TestDefaultOptions(t *testing.T) {
	o := defaultOptions()
	if o.maxIdleConns != 5 {
		t.Errorf("maxIdleConns = %d, want 5", o.maxIdleConns)
	}
	if o.connMaxLife != 30*time.Minute {
		t.Errorf("connMaxLife = %v, want 30m", o.connMaxLife)
	}
}

func TestWithMaxIdleConns(t *testing.T) {
	o := defaultOptions()
	WithMaxIdleConns(10)(&o)
	if o.maxIdleConns != 10 {
		t.Errorf("maxIdleConns = %d, want 10", o.maxIdleConns)
	}
}

func TestWithConnMaxLifetime(t *testing.T) {
	o := defaultOptions()
	WithConnMaxLifetime(5 * time.Minute)(&o)
	if o.connMaxLife != 5*time.Minute {
		t.Errorf("connMaxLife = %v, want 5m", o.connMaxLife)
	}
}

func TestNewPoolConfigFrom(t *testing.T) {
	cfg := NewPoolConfigFrom("postgres://localhost/test", 20, "require", []string{"age", "vector"}) // DevSkim: ignore DS162092 — test fixture
	if cfg.DSN != "postgres://localhost/test" {                                                     // DevSkim: ignore DS162092 — test fixture
		t.Errorf("DSN = %q, want postgres://localhost/test", cfg.DSN)                               // DevSkim: ignore DS162092 — test fixture
	}
	if cfg.MaxOpenConns != 20 {
		t.Errorf("MaxOpenConns = %d, want 20", cfg.MaxOpenConns)
	}
	if cfg.SSLMode != "require" {
		t.Errorf("SSLMode = %q, want require", cfg.SSLMode)
	}
	if len(cfg.Extensions) != 2 {
		t.Errorf("Extensions len = %d, want 2", len(cfg.Extensions))
	}
}

func TestNewPool_InvalidDSN(t *testing.T) {
	_, err := NewPool(PoolConfig{DSN: "not-a-valid-dsn"})
	if err == nil {
		t.Error("NewPool(invalid DSN) should return error")
	}
}

func TestPgPool_CloseIdempotent(t *testing.T) {
	p := &pgPool{closed: true}
	if err := p.Close(); err != nil {
		t.Errorf("Close() on already-closed pool returned error: %v", err)
	}
}
