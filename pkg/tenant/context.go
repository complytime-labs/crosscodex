package tenant

import (
	"context"
)

type tenantKey struct{}

// WithTenant adds a tenant ID to the context
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenantID)
}

// FromContext extracts the tenant ID from a context.
// Returns ErrNoTenant if no tenant ID is present.
func FromContext(ctx context.Context) (string, error) {
	tenantID, ok := ctx.Value(tenantKey{}).(string)
	if !ok || tenantID == "" {
		return "", ErrNoTenant
	}
	return tenantID, nil
}
