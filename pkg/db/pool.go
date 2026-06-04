package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type pgPool struct {
	db         *sql.DB
	extensions []string
	closed     bool
	mu         sync.Mutex

	// Telemetry (optional, nil-safe)
	tracer       trace.Tracer
	meter        metric.Meter
	queryCounter metric.Int64Counter
	queryLatency metric.Int64Histogram
	txCounter    metric.Int64Counter
	connGauge    metric.Int64Gauge
}

func NewPool(cfg PoolConfig, opts ...Option) (Pool, error) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	if cfg.MaxIdleConns > 0 {
		o.maxIdleConns = cfg.MaxIdleConns
	}
	if cfg.ConnMaxLife > 0 {
		o.connMaxLife = cfg.ConnMaxLife
	}

	redacted := redactDSN(cfg.DSN)

	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open database (%s): %w", redacted, err)
	}
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	db.SetMaxIdleConns(o.maxIdleConns)
	db.SetConnMaxLifetime(o.connMaxLife)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping database (%s): %w", redacted, err)
	}

	p := &pgPool{
		db:         db,
		extensions: cfg.Extensions,
		tracer:     o.tracer,
		meter:      o.meter,
	}

	if o.meter != nil {
		var err error
		p.queryCounter, err = o.meter.Int64Counter("db.queries.total",
			metric.WithDescription("Total database queries executed"))
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to create query counter: %w", err)
		}
		p.queryLatency, err = o.meter.Int64Histogram("db.query.duration_ms",
			metric.WithDescription("Database query duration in milliseconds"))
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to create query latency histogram: %w", err)
		}
		p.txCounter, err = o.meter.Int64Counter("db.transactions.total",
			metric.WithDescription("Total database transactions started"))
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to create transaction counter: %w", err)
		}
		p.connGauge, err = o.meter.Int64Gauge("db.pool.open_connections",
			metric.WithDescription("Current number of open database connections"))
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to create connection gauge: %w", err)
		}
	}

	return p, nil
}

func NewPoolConfigFrom(dsn string, graphDSN string, maxConns int, sslMode string, extensions []string) PoolConfig {
	return PoolConfig{
		DSN:          dsn,
		GraphDSN:     graphDSN,
		MaxOpenConns: maxConns,
		SSLMode:      sslMode,
		Extensions:   extensions,
	}
}

func (p *pgPool) Begin(ctx context.Context) (Transaction, error) {
	ctx, span := p.startSpan(ctx, "db.Begin")
	defer span.End()

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	if p.txCounter != nil {
		p.txCounter.Add(ctx, 1)
	}
	span.SetStatus(codes.Ok, "")
	return &pgTx{tx: tx}, nil
}

func (p *pgPool) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	start := time.Now()
	ctx, span := p.startSpan(ctx, "db.Query")
	defer span.End()

	rows, err := p.db.QueryContext(ctx, query, args...)
	if p.queryCounter != nil {
		p.queryCounter.Add(ctx, 1)
	}
	if p.queryLatency != nil {
		p.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return &pgRows{rows: rows}, nil
}

// QueryRow instruments the call with a span and counter. Note: sql.QueryRowContext
// defers the actual database round-trip until .Scan(), so span timing and status
// reflect only the call setup, not query execution. Callers needing accurate
// per-query observability should prefer Query + scan.
func (p *pgPool) QueryRow(ctx context.Context, query string, args ...any) Row {
	_, span := p.startSpan(ctx, "db.QueryRow")
	defer span.End()

	if p.queryCounter != nil {
		p.queryCounter.Add(ctx, 1)
	}
	span.SetStatus(codes.Ok, "")
	return p.db.QueryRowContext(ctx, query, args...)
}

func (p *pgPool) Exec(ctx context.Context, query string, args ...any) error {
	start := time.Now()
	ctx, span := p.startSpan(ctx, "db.Exec")
	defer span.End()

	_, err := p.db.ExecContext(ctx, query, args...)
	if p.queryCounter != nil {
		p.queryCounter.Add(ctx, 1)
	}
	if p.queryLatency != nil {
		p.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p *pgPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.db.Close()
}

// pgTx wraps *sql.Tx to implement Transaction.
type pgTx struct {
	tx *sql.Tx
}

func (t *pgTx) Commit() error   { return t.tx.Commit() }
func (t *pgTx) Rollback() error { return t.tx.Rollback() }

func (t *pgTx) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := t.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &pgRows{rows: rows}, nil
}

func (t *pgTx) QueryRow(ctx context.Context, query string, args ...any) Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

func (t *pgTx) Exec(ctx context.Context, query string, args ...any) error {
	_, err := t.tx.ExecContext(ctx, query, args...)
	return err
}

// pgRows wraps *sql.Rows to implement Rows.
type pgRows struct {
	rows *sql.Rows
}

func (r *pgRows) Next() bool             { return r.rows.Next() }
func (r *pgRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *pgRows) Close() error           { return r.rows.Close() }
func (r *pgRows) Err() error             { return r.rows.Err() }

func (p *pgPool) startSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	if p.tracer != nil {
		return p.tracer.Start(ctx, name)
	}
	return trace.SpanFromContext(ctx).TracerProvider().Tracer("db").Start(ctx, name)
}

// kvPasswordRe matches the password field in a keyword=value DSN.
// It handles both unquoted values (non-whitespace) and single-quoted values.
var kvPasswordRe = regexp.MustCompile(`(?i)(password\s*=\s*)('[^']*'|\S+)`)

// redactDSN replaces the password in a DSN with "REDACTED" and returns the
// sanitised string. It supports both URI-style DSNs
// (postgres://user:pass@host/db) and PostgreSQL keyword=value DSNs
// (host=localhost password=secret dbname=mydb).
//
// If the DSN cannot be parsed, it returns the literal "<unparseable-dsn>"
// to avoid leaking credentials from malformed connection strings.
func redactDSN(dsn string) string {
	// Keyword=value format: contains "key=value" pairs without a URI scheme.
	// PostgreSQL keyword=value DSNs always contain "=" but never "://".
	if strings.Contains(dsn, "=") && !strings.Contains(dsn, "://") {
		return kvPasswordRe.ReplaceAllString(dsn, "${1}REDACTED")
	}

	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		return "<unparseable-dsn>"
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
		}
	}
	return u.String()
}
