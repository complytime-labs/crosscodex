package db

import "context"

// Connection represents a database connection pool.
//
// Implementations must be safe for concurrent use.
type Connection interface {
	// Begin starts a new transaction.
	Begin(ctx context.Context) (Transaction, error)

	// Query executes a query that returns rows.
	Query(ctx context.Context, query string, args ...any) (Rows, error)

	// QueryRow executes a query that returns at most one row.
	QueryRow(ctx context.Context, query string, args ...any) Row

	// Exec executes a query that does not return rows.
	Exec(ctx context.Context, query string, args ...any) error

	// Close closes the connection pool and releases resources.
	Close() error
}

// Transaction provides ACID guarantees for a set of operations.
//
// Implementations must not be used concurrently.
type Transaction interface {
	// Commit commits the transaction.
	Commit() error

	// Rollback aborts the transaction.
	Rollback() error

	// Query executes a query within the transaction.
	Query(ctx context.Context, query string, args ...any) (Rows, error)

	// QueryRow executes a query that returns at most one row.
	QueryRow(ctx context.Context, query string, args ...any) Row

	// Exec executes a query that does not return rows.
	Exec(ctx context.Context, query string, args ...any) error
}
