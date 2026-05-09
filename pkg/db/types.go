package db

// IsolationLevel defines transaction isolation levels.
type IsolationLevel string

const (
	// IsolationReadUncommitted allows dirty reads.
	IsolationReadUncommitted IsolationLevel = "READ UNCOMMITTED"

	// IsolationReadCommitted prevents dirty reads.
	IsolationReadCommitted IsolationLevel = "READ COMMITTED"

	// IsolationRepeatableRead prevents non-repeatable reads.
	IsolationRepeatableRead IsolationLevel = "REPEATABLE READ"

	// IsolationSerializable provides full isolation.
	IsolationSerializable IsolationLevel = "SERIALIZABLE"
)

// Row represents a single query result row.
type Row interface {
	// Scan copies the columns in the current row into the values pointed at by dest.
	Scan(dest ...any) error
}

// Rows represents a result set from a query.
type Rows interface {
	// Next prepares the next result row for reading with Scan.
	// Returns false when there are no more rows or an error occurs.
	Next() bool

	// Scan copies the columns in the current row into the values pointed at by dest.
	Scan(dest ...any) error

	// Close closes the result set and releases resources.
	Close() error

	// Err returns any error that occurred during iteration.
	Err() error
}
