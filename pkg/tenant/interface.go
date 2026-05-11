package tenant

import "context"

// Context handles tenant context propagation.
//
// Implementations must ensure tenant IDs are validated and
// cannot be spoofed across service boundaries.
type Context interface {
	// TenantID returns the tenant ID for this context.
	// Implementations should validate the tenant ID on first access.
	TenantID() string

	// WithTenant creates a new context with the specified tenant ID.
	WithTenant(ctx context.Context, tenantID string) context.Context

	// FromContext extracts the tenant ID from a context.
	// Returns ErrNoTenant if no tenant ID is present.
	FromContext(ctx context.Context) (string, error)
}
