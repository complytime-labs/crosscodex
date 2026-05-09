package db

import "errors"

var (
	// ErrNoRows indicates a query returned no rows.
	ErrNoRows = errors.New("no rows in result set")

	// ErrTxDone indicates the transaction has already been committed or rolled back.
	ErrTxDone = errors.New("transaction already completed")

	// ErrConnClosed indicates the connection has been closed.
	ErrConnClosed = errors.New("connection closed")
)
