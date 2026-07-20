package graph

import (
	"context"
	"fmt"
	"strings"
)

// ResourceResolver resolves a ResourceRef to its raw data.
// Implementations are scheme-specific (pg://, s3://, etc.).
type ResourceResolver interface {
	// Resolve fetches the resource data identified by ref.
	// The context carries a deadline — implementations must respect it.
	Resolve(ctx context.Context, ref ResourceRef) ([]byte, error)

	// Scheme returns the URI scheme this resolver handles (e.g., "pg", "s3").
	Scheme() string
}

// ResourceRef identifies a resource to resolve.
type ResourceRef struct {
	// Type describes the resource kind (e.g., "analysis_result").
	Type string
	// ID is the primary identifier (e.g., job ID).
	ID string
	// URI is the full locator (e.g., "pg://results/job-123/relationship").
	// The scheme determines which resolver handles it.
	URI string
}

// SchemeFromURI extracts the scheme from a URI (e.g., "pg" from "pg://results/...").
func SchemeFromURI(uri string) string {
	idx := strings.Index(uri, "://")
	if idx < 0 {
		return ""
	}
	return uri[:idx]
}

// ResolverRegistry maps URI schemes to resolvers.
// When adding failure-rate-aware resolvers, implement per-scheme circuit breaking here.
type ResolverRegistry struct {
	resolvers map[string]ResourceResolver
}

// NewResolverRegistry creates an empty registry.
func NewResolverRegistry() *ResolverRegistry {
	return &ResolverRegistry{resolvers: make(map[string]ResourceResolver)}
}

// Register adds a resolver for its scheme. Overwrites any existing resolver
// for the same scheme.
func (r *ResolverRegistry) Register(resolver ResourceResolver) {
	r.resolvers[resolver.Scheme()] = resolver
}

// Resolve dispatches to the resolver matching the ref's URI scheme.
func (r *ResolverRegistry) Resolve(ctx context.Context, ref ResourceRef) ([]byte, error) {
	scheme := SchemeFromURI(ref.URI)
	resolver, ok := r.resolvers[scheme]
	if !ok {
		return nil, fmt.Errorf("scheme %q: %w", scheme, ErrResolverNotFound)
	}
	return resolver.Resolve(ctx, ref)
}
