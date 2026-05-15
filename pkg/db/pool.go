package db

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type pgPool struct {
	db         *sql.DB
	extensions []string
	closed     bool
	mu         sync.Mutex
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

	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	db.SetMaxIdleConns(o.maxIdleConns)
	db.SetConnMaxLifetime(o.connMaxLife)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &pgPool{
		db:         db,
		extensions: cfg.Extensions,
	}, nil
}

func NewPoolConfigFrom(dsn string, maxConns int, sslMode string, extensions []string) PoolConfig {
	return PoolConfig{
		DSN:          dsn,
		MaxOpenConns: maxConns,
		SSLMode:      sslMode,
		Extensions:   extensions,
	}
}

func (p *pgPool) Begin(ctx context.Context) (Transaction, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	return &pgTx{tx: tx}, nil
}

func (p *pgPool) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &pgRows{rows: rows}, nil
}

func (p *pgPool) QueryRow(ctx context.Context, query string, args ...any) Row {
	return p.db.QueryRowContext(ctx, query, args...)
}

func (p *pgPool) Exec(ctx context.Context, query string, args ...any) error {
	_, err := p.db.ExecContext(ctx, query, args...)
	return err
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
