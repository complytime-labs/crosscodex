-- retention_holds was created in migration 001 without row-level security.
-- Add tenant isolation matching every other tenant table.
--
-- tenant_id is NULLABLE: a NULL tenant_id is a GLOBAL hold that applies to all
-- tenants (mirrors HoldScope wildcard semantics in pkg/retention/hold.go). The
-- USING clause therefore lets a tenant see its own holds AND global holds, so a
-- global hold correctly blocks every tenant's purge. The WITH CHECK clause is
-- STRICTER: it forbids app_user from INSERTing/UPDATEing a row for any tenant
-- other than the current one (and forbids creating global NULL-tenant holds via
-- app_user) — global holds are a privileged-path concept, never created over the
-- tenant-scoped admin API.
ALTER TABLE public.retention_holds ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON public.retention_holds
    USING (
        tenant_id = current_setting('app.current_tenant', true)
        OR tenant_id IS NULL
    )
    WITH CHECK (
        tenant_id = current_setting('app.current_tenant', true)
    );
