-- Application role for all non-migration database access.
-- RLS policies are not enforced for the table owner (superuser).
-- Application connections must use this role so that tenant isolation
-- is enforced by PostgreSQL rather than trusted to application code.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_user') THEN
        CREATE ROLE app_user LOGIN;
    END IF;
END
$$;

GRANT SELECT, INSERT, UPDATE, DELETE
    ON ALL TABLES IN SCHEMA public
    TO app_user;

ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_stages ENABLE ROW LEVEL SECURITY;
ALTER TABLE catalogs ENABLE ROW LEVEL SECURITY;
ALTER TABLE classifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE vote_summaries ENABLE ROW LEVEL SECURITY;
ALTER TABLE embeddings ENABLE ROW LEVEL SECURITY;

-- Tenant isolation: current_setting('app.current_tenant', true) returns
-- empty string when not set. No row has empty tenant_id, so unset
-- variable matches zero rows (fail closed).
CREATE POLICY tenant_isolation ON tenants
    USING (tenant_id = current_setting('app.current_tenant', true));

-- Jobs: combined tenant + optional user ownership policy.
-- When app.current_user is set, both must match.
-- When app.current_user is not set (NULL or empty), only tenant isolation applies.
-- COALESCE is required because current_setting(..., true) returns NULL (not
-- empty string) when the variable has never been set in the session.
CREATE POLICY tenant_isolation ON jobs
    USING (
        tenant_id = current_setting('app.current_tenant', true)
        AND (
            created_by = current_setting('app.current_user', true)
            OR COALESCE(current_setting('app.current_user', true), '') = ''
        )
    )
    WITH CHECK (
        tenant_id = current_setting('app.current_tenant', true)
        AND (
            created_by = current_setting('app.current_user', true)
            OR COALESCE(current_setting('app.current_user', true), '') = ''
        )
    );

CREATE POLICY tenant_isolation ON job_stages
    USING (tenant_id = current_setting('app.current_tenant', true));

CREATE POLICY tenant_isolation ON catalogs
    USING (tenant_id = current_setting('app.current_tenant', true));

CREATE POLICY tenant_isolation ON classifications
    USING (tenant_id = current_setting('app.current_tenant', true));

CREATE POLICY tenant_isolation ON vote_summaries
    USING (tenant_id = current_setting('app.current_tenant', true));

CREATE POLICY tenant_isolation ON embeddings
    USING (tenant_id = current_setting('app.current_tenant', true));
