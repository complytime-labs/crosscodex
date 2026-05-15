package db

import "context"

type Connection interface {
	Begin(ctx context.Context) (Transaction, error)
	Query(ctx context.Context, query string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) Row
	Exec(ctx context.Context, query string, args ...any) error
	Close() error
}

type Transaction interface {
	Commit() error
	Rollback() error
	Query(ctx context.Context, query string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) Row
	Exec(ctx context.Context, query string, args ...any) error
}

// Pool extends Connection with lifecycle management and observability.
type Pool interface {
	Connection
	Health(ctx context.Context) (*HealthStatus, error)
	VerifyExtensions(ctx context.Context) error
}

// Migrator manages database schema migrations.
type Migrator interface {
	Up(ctx context.Context) error
	Version(ctx context.Context) (version uint, dirty bool, err error)
	Close() error
}

// TenantConnection wraps a Connection with tenant-scoped RLS enforcement.
// Every transaction started via Begin() automatically sets
// app.current_tenant for the duration of that transaction via SET LOCAL.
// Query, QueryRow, and Exec are rejected with ErrTenantRequired because
// SET LOCAL has no effect outside a transaction.
type TenantConnection interface {
	Connection
}
