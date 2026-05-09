// Package tenant provides multi-tenant context propagation.
//
// Handles tenant isolation across service boundaries by propagating
// tenant IDs through context.Context.
//
// Example usage:
//
//	ctx := tenant.WithTenant(ctx, "acme-corp")
//
//	tenantID, err := tenant.FromContext(ctx)
//	if err != nil {
//	    return err
//	}
//
//	// Use tenantID to scope database queries, NATS subjects, etc.
package tenant
