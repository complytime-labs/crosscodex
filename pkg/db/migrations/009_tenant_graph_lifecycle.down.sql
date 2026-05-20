DROP TRIGGER IF EXISTS tenant_graph_drop ON tenants;
DROP FUNCTION IF EXISTS public.drop_tenant_graph();

DROP TRIGGER IF EXISTS tenant_graph_create ON tenants;
DROP FUNCTION IF EXISTS public.create_tenant_graph();

DROP FUNCTION IF EXISTS public.assert_tenant_graph(TEXT);
DROP FUNCTION IF EXISTS public.tenant_graph_name();

-- graph_user was created by the up migration. Safe to drop here because
-- all objects it owned (per-tenant graph schemas) were already dropped by
-- the tenant_graph_drop trigger when tenants were deleted, or will be
-- cleaned up by a full database drop.
DROP ROLE IF EXISTS graph_user;
