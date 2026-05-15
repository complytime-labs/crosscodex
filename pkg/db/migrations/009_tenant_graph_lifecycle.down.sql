DROP TRIGGER IF EXISTS tenant_graph_drop ON tenants;
DROP FUNCTION IF EXISTS public.drop_tenant_graph();

DROP TRIGGER IF EXISTS tenant_graph_create ON tenants;
DROP FUNCTION IF EXISTS public.create_tenant_graph();

DROP FUNCTION IF EXISTS public.assert_tenant_graph(TEXT);
DROP FUNCTION IF EXISTS public.tenant_graph_name();
