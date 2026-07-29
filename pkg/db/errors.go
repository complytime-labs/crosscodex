package db

import (
	"errors"
	"fmt"
)

var (
	ErrNoRows               = errors.New("no rows in result set")
	ErrTxDone               = errors.New("transaction already completed")
	ErrConnClosed           = errors.New("connection closed")
	ErrTenantRequired       = errors.New("tenant ID required in context")
	ErrExtensionMissing     = errors.New("required PostgreSQL extension not available")
	ErrMigrationDirty       = errors.New("migration state is dirty")
	ErrPoolNotReady         = errors.New("connection pool not ready")
	ErrImmutableRecord      = errors.New("completed records cannot be modified")
	ErrTenantGraphViolation = errors.New("tenant graph context mismatch")
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

// IsPgErrorCode checks if err carries the given PostgreSQL SQLSTATE code.
// It returns true when err unwraps to a type with SQLState() that matches code.
func IsPgErrorCode(err error, code string) bool {
	if err == nil {
		return false
	}
	type pgErr interface {
		SQLState() string
	}
	var pe pgErr
	if errors.As(err, &pe) {
		return pe.SQLState() == code
	}
	return false
}

// ClassifyPgError maps PostgreSQL error codes from triggers/RLS to sentinel errors.
func ClassifyPgError(err error) error {
	switch {
	case IsPgErrorCode(err, "23001"):
		return fmt.Errorf("%w: %s", ErrImmutableRecord, err)
	case IsPgErrorCode(err, "42501"):
		return fmt.Errorf("%w: %s", ErrTenantGraphViolation, err)
	default:
		return err
	}
}
