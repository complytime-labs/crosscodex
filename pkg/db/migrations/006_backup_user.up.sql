-- backup_user runs `crosscodexd admin backup`. It needs REPLICATION so
-- WAL-G can stream BASE_BACKUP, pg_read_all_settings because WAL-G reads
-- data_directory (a superuser-only setting) before every backup, and it
-- must enumerate tenant IDs so the objects step can open each tenant's
-- object store. It gets nothing else:
-- no tenant data, no writes, no RLS bypass. The password is set by the
-- operator (ALTER ROLE backup_user PASSWORD ...), never by a migration.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'backup_user') THEN
        CREATE ROLE backup_user LOGIN REPLICATION;
    END IF;
END
$$;

-- Read-only: lets the role see settings, not change them.
GRANT pg_read_all_settings TO backup_user;

-- Column-level grant: tenant_id only, so display_name and status stay hidden.
GRANT SELECT (tenant_id) ON public.tenants TO backup_user;

-- tenants has RLS: tenant_isolation matches zero rows without
-- app.current_tenant. backup_user must see every tenant, so it gets its own
-- SELECT-only policy. A policy filters rows; the grant above is what
-- permits the read at all.
CREATE POLICY backup_list_tenants ON public.tenants
    FOR SELECT TO backup_user
    USING (true);
