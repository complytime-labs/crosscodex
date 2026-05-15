package db

import (
	"context"
	"fmt"
)

type tenantKey struct{}
type userKey struct{}

// ContextWithTenant returns a context carrying the given tenant ID.
func ContextWithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenantID)
}

// ContextWithUser returns a context carrying the given user ID.
// When set, RLS job ownership policies restrict visibility to the user's
// own jobs. When not set, service-level access sees all jobs in the tenant.
func ContextWithUser(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userKey{}, userID)
}

// TenantFromContext extracts the tenant ID from a context.
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantKey{}).(string)
	return v
}

// UserFromContext extracts the user ID from a context.
func UserFromContext(ctx context.Context) string {
	v, _ := ctx.Value(userKey{}).(string)
	return v
}

type tenantPool struct {
	pool Pool
}

// NewTenantPool creates a TenantConnection that enforces tenant-scoped
// RLS on every transaction.
func NewTenantPool(pool Pool) TenantConnection {
	return &tenantPool{pool: pool}
}

func (tp *tenantPool) Begin(ctx context.Context) (Transaction, error) {
	tenantID := TenantFromContext(ctx)
	if tenantID == "" {
		return nil, ErrTenantRequired
	}

	tx, err := tp.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}

	if err := tx.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("failed to set tenant context: %w", err)
	}

	userID := UserFromContext(ctx)
	if userID != "" {
		if err := tx.Exec(ctx, "SELECT set_config('app.current_user', $1, true)", userID); err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("failed to set user context: %w", err)
		}
	}

	return tx, nil
}

// Query is rejected on TenantPool. SET LOCAL has no effect outside a
// transaction, so allowing non-transactional queries would silently
// bypass RLS. All tenant-scoped work must go through Begin().
func (tp *tenantPool) Query(_ context.Context, _ string, _ ...any) (Rows, error) {
	return nil, ErrTenantRequired
}

// QueryRow is rejected on TenantPool. See Query for rationale.
func (tp *tenantPool) QueryRow(_ context.Context, _ string, _ ...any) Row {
	return &errRow{err: ErrTenantRequired}
}

// Exec is rejected on TenantPool. See Query for rationale.
func (tp *tenantPool) Exec(_ context.Context, _ string, _ ...any) error {
	return ErrTenantRequired
}

func (tp *tenantPool) Close() error {
	return tp.pool.Close()
}

type errRow struct {
	err error
}

func (r *errRow) Scan(_ ...any) error {
	return r.err
}
