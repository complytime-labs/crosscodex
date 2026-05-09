package tenant

import "errors"

var (
	// ErrNoTenant indicates no tenant ID is present in the context.
	ErrNoTenant = errors.New("no tenant in context")

	// ErrInvalidTenant indicates the tenant ID is malformed or invalid.
	ErrInvalidTenant = errors.New("invalid tenant ID")

	// ErrTenantMismatch indicates a tenant ID mismatch across contexts.
	ErrTenantMismatch = errors.New("tenant ID mismatch")
)
