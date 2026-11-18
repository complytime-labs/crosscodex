package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// tenantRouter implements Provider by delegating each call to the provider
// that open returned for the tenant on the call's context.
type tenantRouter struct {
	open func(tenantID string) (Provider, error)

	mu        sync.Mutex
	providers map[string]Provider
	closed    bool
}

// NewTenantRouter returns a Provider that serves every tenant from its own
// backing Provider. The tenant comes from tenant.FromContext(ctx), so callers
// must put it on the context with tenant.WithTenant; a context without a
// tenant fails with an error wrapping tenant.ErrNoTenant and never reaches
// open. tenant.WithTenant validates the ID, and the providers NewLocal and
// NewS3 return validate it again.
//
// open is called at most once per tenant, on that tenant's first call; the
// router caches the result. A failed open is returned wrapped with the tenant
// ID and is not cached, so the next call retries.
//
// The router owns the providers it opens: Close closes all of them and joins
// their errors. Afterwards every method returns ErrProviderClosed. It is safe
// for concurrent use.
func NewTenantRouter(open func(tenantID string) (Provider, error)) Provider {
	return &tenantRouter{open: open, providers: make(map[string]Provider)}
}

// provider returns the provider for the tenant on ctx, opening it on first use.
func (r *tenantRouter) provider(ctx context.Context) (Provider, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("selecting object storage: %w. Put the tenant on the context with tenant.WithTenant", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrProviderClosed
	}
	if p, ok := r.providers[tenantID]; ok {
		return p, nil
	}
	p, err := r.open(tenantID)
	if err != nil {
		return nil, fmt.Errorf("opening object storage for tenant %q: %w", tenantID, err)
	}
	r.providers[tenantID] = p
	return p, nil
}

func (r *tenantRouter) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := r.provider(ctx)
	if err != nil {
		return nil, err
	}
	return p.Get(ctx, key)
}

func (r *tenantRouter) Put(ctx context.Context, key string, data io.Reader) error {
	p, err := r.provider(ctx)
	if err != nil {
		return err
	}
	return p.Put(ctx, key, data)
}

func (r *tenantRouter) Delete(ctx context.Context, key string) error {
	p, err := r.provider(ctx)
	if err != nil {
		return err
	}
	return p.Delete(ctx, key)
}

func (r *tenantRouter) List(ctx context.Context, prefix string) ([]ObjectMetadata, error) {
	p, err := r.provider(ctx)
	if err != nil {
		return nil, err
	}
	return p.List(ctx, prefix)
}

func (r *tenantRouter) Exists(ctx context.Context, key string) (bool, error) {
	p, err := r.provider(ctx)
	if err != nil {
		return false, err
	}
	return p.Exists(ctx, key)
}

func (r *tenantRouter) Stat(ctx context.Context, key string) (*ObjectMetadata, error) {
	p, err := r.provider(ctx)
	if err != nil {
		return nil, err
	}
	return p.Stat(ctx, key)
}

// Close closes every opened provider and joins their errors. A second Close
// returns nil.
func (r *tenantRouter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var errs []error
	for id, p := range r.providers {
		if err := p.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing object storage for tenant %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
