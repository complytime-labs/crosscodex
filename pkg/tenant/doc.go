// Package tenant provides tenant context propagation for multi-tenant
// services. It owns the single source of truth for tenant identity in
// [context.Context] — all packages that need tenant-scoped behavior
// use these functions rather than maintaining their own context keys.
//
// Tenant IDs are validated at the point of injection ([WithTenant]),
// so any ID extracted via [FromContext] is guaranteed to be well-formed.
//
// Usage:
//
//	ctx, err := tenant.WithTenant(ctx, "acme-corp")
//	if err != nil {
//	    // handle invalid tenant ID
//	}
//	id, err := tenant.FromContext(ctx)
package tenant
