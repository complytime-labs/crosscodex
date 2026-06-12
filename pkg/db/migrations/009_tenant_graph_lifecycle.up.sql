-- Graph lifecycle management for tenant isolation in Apache AGE.
-- Each tenant gets a dedicated named graph (crosscodex_{tenant_id}).
-- This is the graph layer's equivalent of RLS — isolation by namespace
-- rather than by row policy, because AGE manages its own internal tables
-- that RLS cannot reach.

-- LOAD is safe here because migrations run as the postgres superuser.
-- Application code must NOT use LOAD — see pkg/graphdb/client.go for why.
LOAD 'age';
SET LOCAL search_path = ag_catalog, "$user", public;

-- ---------------------------------------------------------------------------
-- graph_user: dedicated role for graph operations
-- ---------------------------------------------------------------------------
-- Why a separate role? AGE cypher commands internally perform DDL — creating
-- label tables, updating sequences — inside per-tenant graph schemas.
-- PostgreSQL requires the calling role to OWN those objects for DDL.
-- GRANT INSERT/UPDATE/DELETE is not sufficient.
--
-- Giving app_user ownership would let it DROP tables, ALTER schema, and
-- GRANT others access — far beyond the SELECT/INSERT/UPDATE/DELETE that
-- the relational RLS model permits. A dedicated graph_user role keeps the
-- blast radius contained: it owns graph schemas but has zero access to
-- relational tables in the public schema, and vice versa for app_user.
--
-- See pkg/db/doc.go for the full three-role security model.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'graph_user') THEN
        CREATE ROLE graph_user LOGIN;
    END IF;
END
$$;

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

-- ---------------------------------------------------------------------------
-- Tenant graph lifecycle triggers
-- ---------------------------------------------------------------------------

-- Automatically create a graph when a tenant is provisioned.
--
-- Ownership transfer is critical: this trigger runs as the postgres
-- superuser (tenant provisioning is an admin operation), so
-- ag_catalog.create_graph() creates the schema owned by postgres.
-- graph_user needs to own the schema and all objects inside it because
-- AGE cypher commands internally perform DDL (CREATE TABLE for new labels,
-- ALTER SEQUENCE for IDs). Without the ownership transfer, every cypher
-- CREATE/MERGE from graph_user fails with:
--   ERROR: must be owner of table _ag_label_vertex
--   ERROR: permission denied for schema crosscodex_<tenant_id>
CREATE OR REPLACE FUNCTION public.create_tenant_graph()
RETURNS TRIGGER AS $$
DECLARE
    schema_name TEXT := 'crosscodex_' || NEW.tenant_id;
    tbl RECORD;
    seq RECORD;
BEGIN
    PERFORM ag_catalog.create_graph(schema_name);
    EXECUTE format('ALTER SCHEMA %I OWNER TO graph_user', schema_name);
    FOR tbl IN SELECT tablename FROM pg_tables WHERE schemaname = schema_name LOOP
        EXECUTE format('ALTER TABLE %I.%I OWNER TO graph_user', schema_name, tbl.tablename);
    END LOOP;
    FOR seq IN SELECT sequencename FROM pg_sequences WHERE schemaname = schema_name LOOP
        EXECUTE format('ALTER SEQUENCE %I.%I OWNER TO graph_user', schema_name, seq.sequencename);
    END LOOP;
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

-- ---------------------------------------------------------------------------
-- Grants for graph_user
-- ---------------------------------------------------------------------------

-- Helper functions in public schema — graph_user calls these for tenant
-- context validation but has no other access to public schema tables.
GRANT EXECUTE ON FUNCTION public.tenant_graph_name() TO graph_user;
GRANT EXECUTE ON FUNCTION public.assert_tenant_graph(TEXT) TO graph_user;

-- ag_catalog access: AGE stores graph metadata (ag_graph, ag_label) in this
-- schema. graph_user needs USAGE to resolve objects, SELECT to check graph
-- existence (CreateGraph's idempotency check queries ag_catalog.ag_graph),
-- and EXECUTE to call ag_catalog.cypher() and ag_catalog.create_graph().
-- Without these grants, every graph operation fails with
-- "permission denied for schema ag_catalog" because the schema is owned
-- by the postgres superuser who ran CREATE EXTENSION age.
GRANT USAGE ON SCHEMA ag_catalog TO graph_user;
GRANT SELECT ON ALL TABLES IN SCHEMA ag_catalog TO graph_user;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA ag_catalog TO graph_user;
