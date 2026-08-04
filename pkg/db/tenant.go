package db

import (
	"context"
	"fmt"
)

// EnsureTenant idempotently provisions a tenant row. It MUST be called on a
// connection with privileges to bypass the tenants row-level security policy —
// i.e. the migration superuser/owner connection, not a TenantConnection.
//
// The tenants tenant_isolation policy has no explicit WITH CHECK, so PostgreSQL
// applies its USING expression (tenant_id = current_setting('app.current_tenant'))
// as the INSERT check. Under the RLS-enforced app_user role, that blocks inserts
// outside a tenant-scoped transaction, which is why provisioning is deliberately
// a privileged operation (see pkg/db/doc.go).
//
// Inserting a tenant fires the tenant_graph_create AFTER INSERT trigger, which
// provisions the per-tenant graph schema. ON CONFLICT (tenant_id) DO NOTHING
// makes repeat calls safe across restarts.
func EnsureTenant(ctx context.Context, conn Connection, tenantID, displayName string) error {
	if tenantID == "" {
		return fmt.Errorf("ensure tenant: empty tenant ID")
	}
	const q = `INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT (tenant_id) DO NOTHING`
	if err := conn.Exec(ctx, q, tenantID, displayName); err != nil {
		return fmt.Errorf("ensure tenant %q: %w", tenantID, err)
	}
	return nil
}
