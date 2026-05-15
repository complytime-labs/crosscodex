-- Graph lifecycle management for tenant isolation in Apache AGE.
-- Each tenant gets a dedicated named graph (crosscodex_{tenant_id}).
-- This is the graph layer's equivalent of RLS — isolation by namespace
-- rather than by row policy, because AGE manages its own internal tables
-- that RLS cannot reach.

LOAD 'age';
SET search_path = ag_catalog, "$user", public;

-- Returns the graph name for the current tenant context.
-- Uses current_setting WITHOUT the true fallback so that graph operations
-- fail loudly if no tenant is set, rather than silently targeting a
-- nonexistent graph.
CREATE OR REPLACE FUNCTION public.tenant_graph_name()
RETURNS TEXT AS $$
    SELECT 'crosscodex_' || current_setting('app.current_tenant');
$$ LANGUAGE sql STABLE;

-- Validates that a graph name matches the current tenant context.
-- Called by pkg/graphdb as a defensive assertion before cypher queries.
CREATE OR REPLACE FUNCTION public.assert_tenant_graph(graph_name TEXT)
RETURNS VOID AS $$
BEGIN
    IF graph_name != 'crosscodex_' || current_setting('app.current_tenant') THEN
        RAISE EXCEPTION 'graph "%" does not match current tenant context (expected "crosscodex_%")',
            graph_name, current_setting('app.current_tenant')
            USING ERRCODE = 'insufficient_privilege';
    END IF;
END;
$$ LANGUAGE plpgsql;

-- Automatically create a graph when a tenant is provisioned.
CREATE OR REPLACE FUNCTION public.create_tenant_graph()
RETURNS TRIGGER AS $$
BEGIN
    PERFORM ag_catalog.create_graph('crosscodex_' || NEW.tenant_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER tenant_graph_create
    AFTER INSERT ON tenants
    FOR EACH ROW EXECUTE FUNCTION public.create_tenant_graph();

-- Automatically drop a graph when a tenant is deleted.
CREATE OR REPLACE FUNCTION public.drop_tenant_graph()
RETURNS TRIGGER AS $$
BEGIN
    PERFORM ag_catalog.drop_graph('crosscodex_' || OLD.tenant_id, true);
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER tenant_graph_drop
    BEFORE DELETE ON tenants
    FOR EACH ROW EXECUTE FUNCTION public.drop_tenant_graph();

-- Grant execute on helper functions to app_user.
GRANT EXECUTE ON FUNCTION public.tenant_graph_name() TO app_user;
GRANT EXECUTE ON FUNCTION public.assert_tenant_graph(TEXT) TO app_user;
