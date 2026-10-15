package db

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// tenantPool wraps a Pool and enforces tenant-scoped Row-Level Security.
// All operations require a tenant ID in the context (set via tenant.WithTenant).
type tenantPool struct {
	pool Pool
	// scopeSQL takes the tenant ID as $1 and scopes the transaction to it.
	scopeSQL string
}

// enterTenantSQL is the scope statement of every app path. public.enter_tenant
// refuses a deleted tenant.
const enterTenantSQL = "SELECT public.enter_tenant($1)"

// retentionScopeSQL is the scope statement of NewRetentionTenantPool only.
const retentionScopeSQL = "SELECT set_config('app.current_tenant', $1, true)"

// NewTenantPool creates a TenantConnection that enforces RLS on every transaction.
// Its Begin refuses a deleted tenant.
func NewTenantPool(pool Pool) TenantConnection {
	return &tenantPool{pool: pool, scopeSQL: enterTenantSQL}
}

// NewRetentionTenantPool is NewTenantPool without the deleted-tenant check.
// Use it for retention only: a deleted tenant's job data still follows
// retention, so scans and holds must reach it. Judgements (artifact pair
// verdicts) have no retention grant, so retention cannot touch them. Never
// use it on an app path; those must refuse deleted tenants.
func NewRetentionTenantPool(pool Pool) TenantConnection {
	return &tenantPool{pool: pool, scopeSQL: retentionScopeSQL}
}

// Begin starts a transaction with tenant-scoped RLS.
// The tenant ID is extracted from the context via tenant.FromContext.
// Returns ErrTenantRequired if no tenant is present in the context.
// NewTenantPool enters the tenant through public.enter_tenant, which refuses
// a deleted tenant (SQLSTATE 42501, "admin tenant restore") so no app path
// can read or write a deleted tenant's data until an admin restores it.
// NewRetentionTenantPool does not make that check.
func (tp *tenantPool) Begin(ctx context.Context) (Transaction, error) {
	ctx, span := otel.GetTracerProvider().Tracer("crosscodex/pkg/db").Start(ctx, "db.TenantBegin")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.SetStatus(codes.Error, "tenant context missing")
		return nil, ErrTenantRequired
	}
	span.SetAttributes(attribute.String("tenant.id", tenantID))

	tx, err := tp.pool.Begin(ctx)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("begin tenant transaction: %w", err)
	}

	// Set the tenant for RLS policies
	if err := tx.Exec(ctx, tp.scopeSQL, tenantID); err != nil {
		_ = tx.Rollback()
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("enter tenant: %w", err)
	}

	// Set the user if present (optional for RLS)
	if userID := tenant.UserFromContext(ctx); userID != "" {
		if err := tx.Exec(ctx, "SELECT set_config('app.current_user', $1, true)", userID); err != nil {
			_ = tx.Rollback()
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("set user session variable: %w", err)
		}
	}

	span.SetStatus(codes.Ok, "")
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

// Close releases the underlying connection pool.
func (tp *tenantPool) Close() error {
	return tp.pool.Close()
}

// errRow implements Row for rejected QueryRow calls.
type errRow struct {
	err error
}

func (r *errRow) Scan(_ ...any) error {
	return r.err
}
