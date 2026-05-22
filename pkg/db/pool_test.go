package db

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultOptions(t *testing.T) {
	t.Parallel()

	o := defaultOptions()
	if o.maxIdleConns != 5 {
		t.Errorf("maxIdleConns = %d, want 5", o.maxIdleConns)
	}
	if o.connMaxLife != 30*time.Minute {
		t.Errorf("connMaxLife = %v, want 30m", o.connMaxLife)
	}
}

func TestWithMaxIdleConns(t *testing.T) {
	t.Parallel()

	o := defaultOptions()
	WithMaxIdleConns(10)(&o)
	if o.maxIdleConns != 10 {
		t.Errorf("maxIdleConns = %d, want 10", o.maxIdleConns)
	}
}

func TestWithConnMaxLifetime(t *testing.T) {
	t.Parallel()

	o := defaultOptions()
	WithConnMaxLifetime(5 * time.Minute)(&o)
	if o.connMaxLife != 5*time.Minute {
		t.Errorf("connMaxLife = %v, want 5m", o.connMaxLife)
	}
}

func TestNewPoolConfigFrom(t *testing.T) {
	t.Parallel()

	cfg := NewPoolConfigFrom("postgres://localhost/test", "postgres://localhost/test_graph", 20, "require", []string{"age", "vector"}) // DevSkim: ignore DS162092 — test fixture
	if cfg.DSN != "postgres://localhost/test" {                                                                                        // DevSkim: ignore DS162092 — test fixture
		t.Errorf("DSN = %q, want postgres://localhost/test", cfg.DSN) // DevSkim: ignore DS162092 — test fixture
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
	t.Parallel()

	_, err := NewPool(PoolConfig{DSN: "not-a-valid-dsn"})
	if err == nil {
		t.Error("NewPool(invalid DSN) should return error")
	}
}

func TestNewPool_ErrorDoesNotLeakPassword(t *testing.T) {
	t.Parallel()

	password := "s3cret-passw0rd!"
	dsn := "postgres://admin:" + password + "@unreachable-host:5432/mydb?sslmode=disable" // DevSkim: ignore DS162092 — test fixture

	_, err := NewPool(PoolConfig{DSN: dsn})
	if err == nil {
		t.Fatal("expected error connecting to unreachable host")
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, password) {
		t.Errorf("error message contains password %q: %s", password, errMsg)
	}
	if !strings.Contains(errMsg, "REDACTED") {
		t.Errorf("error message should contain REDACTED: %s", errMsg)
	}
}

func TestRedactDSN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dsn        string
		want       string
		notContain string
	}{
		{
			name:       "password redacted",
			dsn:        "postgres://user:secret@localhost:5432/db?sslmode=disable",
			want:       "postgres://user:REDACTED@localhost:5432/db?sslmode=disable",
			notContain: "secret",
		},
		{
			name: "no password unchanged",
			dsn:  "postgres://user@localhost:5432/db", // DevSkim: ignore DS162092 — test fixture
			want: "postgres://user@localhost:5432/db",
		},
		{
			name: "unparseable dsn",
			dsn:  "not-a-valid-dsn",
			want: "<unparseable-dsn>",
		},
		{
			name: "empty dsn",
			dsn:  "",
			want: "<unparseable-dsn>",
		},
		{
			name:       "keyword=value with password",
			dsn:        "host=localhost port=5432 user=admin password=secret dbname=mydb sslmode=disable",
			want:       "host=localhost port=5432 user=admin password=REDACTED dbname=mydb sslmode=disable",
			notContain: "secret",
		},
		{
			name: "keyword=value without password",
			dsn:  "host=localhost port=5432 user=admin dbname=mydb sslmode=disable",
			want: "host=localhost port=5432 user=admin dbname=mydb sslmode=disable",
		},
		{
			name:       "keyword=value with quoted password",
			dsn:        "host=localhost password='my secret' dbname=mydb",
			want:       "host=localhost password=REDACTED dbname=mydb",
			notContain: "my secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := redactDSN(tt.dsn)
			if got != tt.want {
				t.Errorf("redactDSN(%q) = %q, want %q", tt.dsn, got, tt.want)
			}
			if tt.notContain != "" && strings.Contains(got, tt.notContain) {
				t.Errorf("redactDSN(%q) contains %q", tt.dsn, tt.notContain)
			}
		})
	}
}

func TestPgPool_CloseIdempotent(t *testing.T) {
	t.Parallel()

	p := &pgPool{closed: true}
	if err := p.Close(); err != nil {
		t.Errorf("Close() on already-closed pool returned error: %v", err)
	}
}
