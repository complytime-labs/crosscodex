package analyzer

import "context"

type catalogIDKey struct{}

// WithCatalogID returns a new context carrying the given catalog ID.
func WithCatalogID(ctx context.Context, catalogID string) context.Context {
	return context.WithValue(ctx, catalogIDKey{}, catalogID)
}

// CatalogIDFromContext extracts the catalog ID from the context.
// Returns ok=false if no catalog ID is present.
func CatalogIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(catalogIDKey{}).(string)
	return v, ok
}
