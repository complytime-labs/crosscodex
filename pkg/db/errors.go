package db

import (
	"errors"
	"fmt"
)

var (
	ErrNoRows           = errors.New("no rows in result set")
	ErrTxDone           = errors.New("transaction already completed")
	ErrConnClosed       = errors.New("connection closed")
	ErrTenantRequired   = errors.New("tenant ID required in context")
	ErrExtensionMissing = errors.New("required PostgreSQL extension not available")
	ErrMigrationDirty   = errors.New("migration state is dirty")
	ErrPoolNotReady     = errors.New("connection pool not ready")
	ErrImmutableRecord  = errors.New("completed records cannot be modified")
)

type ExtensionError struct {
	Missing []string
}

func (e *ExtensionError) Error() string {
	return fmt.Sprintf("%s: %v", ErrExtensionMissing, e.Missing)
}

func (e *ExtensionError) Unwrap() error {
	return ErrExtensionMissing
}

// pgErrorCode extracts a PostgreSQL error code from a driver error.
func pgErrorCode(err error) string {
	if err == nil {
		return ""
	}
	type pgErr interface {
		SQLState() string
	}
	var pe pgErr
	if errors.As(err, &pe) {
		return pe.SQLState()
	}
	return ""
}

// ClassifyPgError maps PostgreSQL error codes from triggers/RLS to sentinel errors.
func ClassifyPgError(err error) error {
	code := pgErrorCode(err)
	switch code {
	case "23001":
		return fmt.Errorf("%w: %s", ErrImmutableRecord, err)
	default:
		return err
	}
}
