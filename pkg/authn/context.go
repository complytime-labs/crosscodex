package authn

import "context"

// identityKey is the context key for storing authn.Identity.
// Shared between gateway auth interceptor and downstream services.
type identityKey struct{}

// WithIdentity injects an Identity into the context.
// Used by the gateway auth interceptor after successful authentication.
func WithIdentity(ctx context.Context, identity *Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

// IdentityFromContext extracts the Identity from the context.
// Returns nil if no identity is present.
func IdentityFromContext(ctx context.Context) *Identity {
	id, _ := ctx.Value(identityKey{}).(*Identity)
	return id
}
