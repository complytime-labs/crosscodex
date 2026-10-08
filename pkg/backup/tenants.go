package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// SQLTenantLister reads tenant IDs as backup_user, which sees every row of
// tenants (tenant_id only) through its own RLS policy (migration 006).
type SQLTenantLister struct {
	db *sql.DB
}

// OpenSQLTenantLister prepares a single-connection pool on dsn; it connects on first use.
func OpenSQLTenantLister(dsn string) (*SQLTenantLister, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open backup database connection: %w", err)
	}
	db.SetMaxOpenConns(1)
	return &SQLTenantLister{db: db}, nil
}

// TenantIDs returns every tenant ID in ascending order. An invalid ID fails
// the call: it cannot be used as an object-store tenant slot.
func (l *SQLTenantLister) TenantIDs(ctx context.Context) (ids []string, err error) {
	rows, err := l.db.QueryContext(ctx, "SELECT tenant_id FROM tenants ORDER BY tenant_id")
	if err != nil {
		return nil, fmt.Errorf("list tenants as backup_user: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read tenant ID: %w", err)
		}
		if err := tenant.ValidateTenantID(id); err != nil {
			return nil, fmt.Errorf("tenants holds an invalid tenant ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenants as backup_user: %w", err)
	}
	return ids, nil
}

// Close closes the pool.
func (l *SQLTenantLister) Close() error { return l.db.Close() }
