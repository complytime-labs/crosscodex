package tenant

import "context"

type tenantKey struct{}
type userKey struct{}

// WithTenant returns a new context carrying the given tenant ID.
// The tenant ID is validated before storage — invalid IDs produce an error
// and the original context is returned unchanged.
func WithTenant(ctx context.Context, tenantID string) (context.Context, error) {
	if err := ValidateTenantID(tenantID); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, tenantKey{}, tenantID), nil
}

// FromContext extracts the tenant ID from the context.
// Returns ErrNoTenant if no tenant is present.
func FromContext(ctx context.Context) (string, error) {
	v, ok := ctx.Value(tenantKey{}).(string)
	if !ok || v == "" {
		return "", ErrNoTenant
	}
	return v, nil
}

// WithUser returns a new context carrying the given user ID.
// If userID is empty, the original context is returned unchanged.
func WithUser(ctx context.Context, userID string) context.Context {
	if userID == "" {
		return ctx
	}
	return context.WithValue(ctx, userKey{}, userID)
}

// UserFromContext extracts the user ID from the context.
// Returns an empty string if no user is present — user is optional.
func UserFromContext(ctx context.Context) string {
	v, _ := ctx.Value(userKey{}).(string)
	return v
}
