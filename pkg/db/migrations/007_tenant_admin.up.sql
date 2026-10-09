-- tenant_admin runs `crosscodexd admin tenant`. It exists so operators can
-- provision and suspend tenants without the owner or superuser credential.
-- It holds no table, sequence or schema grant: it may only EXECUTE
-- provision_tenant, set_tenant_status and list_tenants below. Those are
-- SECURITY DEFINER, so the insert, and the tenant_graph_create trigger it
-- fires (not DEFINER; ag_catalog.create_graph needs owner rights), run as
-- the migration owner. tenant_admin itself cannot read tenant data or
-- bulk-UPDATE, DELETE or TRUNCATE tenants. The password is set by the
-- operator (ALTER ROLE tenant_admin PASSWORD ...), never by a migration.
--
-- Every function pins search_path with pg_temp last (PostgreSQL's guidance
-- for SECURITY DEFINER, so a caller's temporary table cannot shadow a
-- relation), schema-qualifies its tables, and has EXECUTE revoked from
-- PUBLIC, which PostgreSQL grants by default.
--
-- The functions rely on the owner bypassing RLS on tenants (RLS is enabled
-- but not FORCEd). If FORCE ROW LEVEL SECURITY is ever enabled,
-- tenant_is_active returns false for every tenant and provision_tenant
-- fails the policy check.

-- Every row crosscodex writes holds the column default 'active'. A
-- hand-edited row with another value fails this statement; fix that row
-- first. pkg/db TenantStatusActive/TenantStatusSuspended mirror this list.
ALTER TABLE public.tenants
    ADD CONSTRAINT tenants_status_check CHECK (status IN ('active', 'suspended'));

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tenant_admin') THEN
        CREATE ROLE tenant_admin LOGIN;
    END IF;
END
$$;

-- provision_tenant creates a tenant (and, via the trigger, its graph) or
-- renames an existing one, leaving its status alone. Returns true if it
-- inserted. The ID regex must match pkg/tenant tenantIDPattern exactly.
CREATE FUNCTION public.provision_tenant(p_id text, p_name text)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_id IS NULL OR p_id !~ '^[a-z][a-z0-9-]{1,50}[a-z0-9]$' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'tenant ID %L is invalid: it must be 3-52 characters, lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen. Choose an ID that matches',
            left(p_id, 64));
    END IF;
    IF p_name IS NULL OR p_name ~ '^\s*$' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'tenant %L: display name is empty; give the tenant a human-readable name', left(p_id, 64));
    END IF;
    IF p_name ~ '[\u0001-\u001F\u007F-\u009F]' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'tenant %L: display name contains a control character; use printable text only', left(p_id, 64));
    END IF;

    INSERT INTO public.tenants (tenant_id, display_name) VALUES (p_id, p_name)
        ON CONFLICT (tenant_id) DO NOTHING;
    IF FOUND THEN
        RETURN true;
    END IF;

    UPDATE public.tenants SET display_name = p_name WHERE tenant_id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = '40001', MESSAGE = format(
            'tenant %L was deleted while it was being provisioned; nothing was changed. Run the command again', left(p_id, 64));
    END IF;
    RETURN false;
END
$$;

-- set_tenant_status sets a tenant's status and returns the previous one.
-- Setting the current status is a no-op that still returns it.
CREATE FUNCTION public.set_tenant_status(p_id text, p_status text)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_previous text;
BEGIN
    IF p_status IS NULL OR p_status NOT IN ('active', 'suspended') THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'tenant status %L is invalid: it must be ''active'' or ''suspended''', left(p_status, 64));
    END IF;

    SELECT status INTO v_previous FROM public.tenants WHERE tenant_id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'tenant %L does not exist; nothing was changed. Create it with ''crosscodexd admin tenant create --tenant <id> --display-name <name>''',
            left(p_id, 64));
    END IF;

    UPDATE public.tenants SET status = p_status WHERE tenant_id = p_id;
    RETURN v_previous;
END
$$;

-- list_tenants returns every tenants row; it never touches child tables.
CREATE FUNCTION public.list_tenants()
RETURNS TABLE (tenant_id text, display_name text, status text, created_at timestamptz)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT t.tenant_id, t.display_name, t.status, t.created_at
    FROM public.tenants t
    ORDER BY t.tenant_id
$$;

-- tenant_is_active lets the gateway (app_user) check one tenant without
-- app.current_tenant, under which the tenants RLS policy hides every row.
-- An unknown tenant is not active.
CREATE FUNCTION public.tenant_is_active(p_id text)
RETURNS boolean
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT EXISTS (
        SELECT 1 FROM public.tenants t WHERE t.tenant_id = p_id AND t.status = 'active'
    )
$$;

REVOKE ALL ON FUNCTION public.provision_tenant(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.set_tenant_status(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.list_tenants() FROM PUBLIC;
REVOKE ALL ON FUNCTION public.tenant_is_active(text) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION public.provision_tenant(text, text) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.set_tenant_status(text, text) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.list_tenants() TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.tenant_is_active(text) TO app_user;
