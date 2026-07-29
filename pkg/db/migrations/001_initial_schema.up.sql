-- Initial schema for CrossCodex.
-- Squashed from migrations 001–013 (pre-release).

-- ============================================================================
-- Roles
-- ============================================================================

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

-- Dedicated role for graph operations. AGE cypher commands internally
-- perform DDL (creating label tables, altering sequences) inside
-- per-tenant graph schemas. PostgreSQL requires the calling role to
-- OWN those objects for DDL. A dedicated role keeps the blast radius
-- contained: it owns graph schemas but has zero access to relational
-- tables, and vice versa for app_user.
-- See pkg/db/doc.go for the full three-role security model.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'graph_user') THEN
        CREATE ROLE graph_user LOGIN;
    END IF;
END
$$;

-- ============================================================================
-- Tables
-- ============================================================================

CREATE TABLE IF NOT EXISTS tenants (
    tenant_id    TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON tenants
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON tenants TO app_user;

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS jobs (
    job_id        TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL REFERENCES tenants(tenant_id),
    status        TEXT NOT NULL DEFAULT 'pending',
    config        JSONB,
    created_by    TEXT NOT NULL,
    error_message TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;

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

GRANT SELECT, INSERT, UPDATE, DELETE ON jobs TO app_user;

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS job_stages (
    job_id        TEXT NOT NULL REFERENCES jobs(job_id),
    stage_name    TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',
    started_at    TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ,
    tenant_id     TEXT NOT NULL REFERENCES tenants(tenant_id),
    retry_count   INTEGER NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (job_id, stage_name)
);

ALTER TABLE job_stages ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON job_stages
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON job_stages TO app_user;

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS catalogs (
    catalog_id        TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL REFERENCES tenants(tenant_id),
    name              TEXT NOT NULL,
    version           TEXT NOT NULL,
    source_type       TEXT NOT NULL,
    object_path       TEXT NOT NULL,
    source_uri        TEXT,
    content_hash      TEXT,
    content_size      BIGINT,
    format            TEXT,
    output_hash       TEXT,
    extractor_name    TEXT,
    extractor_version TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE catalogs ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON catalogs
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON catalogs TO app_user;

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS classifications (
    catalog_id TEXT NOT NULL REFERENCES catalogs(catalog_id),
    control_id TEXT NOT NULL,
    type       TEXT NOT NULL,
    level      TEXT NOT NULL,
    tenant_id  TEXT NOT NULL REFERENCES tenants(tenant_id),
    PRIMARY KEY (catalog_id, control_id, type)
);

ALTER TABLE classifications ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON classifications
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON classifications TO app_user;

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS vote_summaries (
    job_id     TEXT NOT NULL REFERENCES jobs(job_id),
    source_id  TEXT NOT NULL,
    target_id  TEXT NOT NULL,
    consensus  TEXT NOT NULL,
    confidence DOUBLE PRECISION NOT NULL,
    viability  DOUBLE PRECISION NOT NULL,
    tenant_id  TEXT NOT NULL REFERENCES tenants(tenant_id),
    PRIMARY KEY (job_id, source_id, target_id)
);

ALTER TABLE vote_summaries ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON vote_summaries
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON vote_summaries TO app_user;

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS embeddings (
    catalog_id TEXT NOT NULL REFERENCES catalogs(catalog_id),
    control_id TEXT NOT NULL,
    model      TEXT NOT NULL,
    vector     vector(2000) NOT NULL,
    tenant_id  TEXT NOT NULL REFERENCES tenants(tenant_id),
    PRIMARY KEY (catalog_id, control_id, model)
);

CREATE INDEX IF NOT EXISTS idx_embeddings_vector
    ON embeddings USING ivfflat (vector vector_cosine_ops)
    WITH (lists = 100);

ALTER TABLE embeddings ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON embeddings
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON embeddings TO app_user;

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS controls (
    tenant_id     TEXT NOT NULL REFERENCES tenants(tenant_id),
    control_id    TEXT NOT NULL,
    catalog_id    TEXT NOT NULL REFERENCES catalogs(catalog_id),
    identifier    TEXT NOT NULL,
    title         TEXT,
    statement     TEXT,
    class         TEXT,
    parent_id     TEXT,
    group_id      TEXT,
    props         JSONB,
    search_vector TSVECTOR GENERATED ALWAYS AS (
        setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(statement, '')), 'B') ||
        setweight(to_tsvector('english', coalesce(identifier, '')), 'A')
    ) STORED,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, control_id)
);

CREATE INDEX idx_controls_catalog ON controls(catalog_id);
CREATE INDEX idx_controls_parent ON controls(parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX idx_controls_class ON controls(class);
CREATE INDEX idx_controls_search ON controls USING GIN(search_vector);

ALTER TABLE controls ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON controls
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON controls TO app_user;

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS requires_candidates (
    tenant_id        TEXT NOT NULL,
    job_id           TEXT NOT NULL,
    source_id        TEXT NOT NULL,
    target_id        TEXT NOT NULL,
    aggregate_score  REAL NOT NULL,
    provenance       JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, job_id, source_id, target_id)
);

CREATE INDEX idx_requires_candidates_job ON requires_candidates(tenant_id, job_id);

ALTER TABLE requires_candidates ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON requires_candidates
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON requires_candidates TO app_user;

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS requires_votes (
    tenant_id        TEXT NOT NULL,
    job_id           TEXT NOT NULL,
    source_id        TEXT NOT NULL,
    target_id        TEXT NOT NULL,
    model            TEXT NOT NULL,
    sample_index     INT NOT NULL,
    requires         BOOLEAN,
    justification    TEXT,
    confidence       TEXT,
    raw_response     TEXT,
    vote_weight      REAL NOT NULL DEFAULT 1.0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, job_id, source_id, target_id, model, sample_index)
);

CREATE INDEX idx_requires_votes_pair ON requires_votes(tenant_id, job_id, source_id, target_id);

ALTER TABLE requires_votes ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON requires_votes
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON requires_votes TO app_user;

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS requires_consensus (
    tenant_id              TEXT NOT NULL,
    job_id                 TEXT NOT NULL,
    source_id              TEXT NOT NULL,
    target_id              TEXT NOT NULL,
    requires               BOOLEAN NOT NULL,
    confidence_fraction    REAL NOT NULL,
    unanimous              BOOLEAN NOT NULL,
    valid_vote_count       INT NOT NULL,
    total_vote_count       INT NOT NULL,
    total_weight           REAL NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, job_id, source_id, target_id)
);

CREATE INDEX idx_requires_consensus_decision ON requires_consensus(tenant_id, job_id, requires);

ALTER TABLE requires_consensus ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON requires_consensus
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON requires_consensus TO app_user;

-- ============================================================================
-- Immutability triggers
-- ============================================================================
-- Error messages are actionable: they tell the operator what happened,
-- why it was blocked, and what to do instead.

CREATE OR REPLACE FUNCTION prevent_completed_job_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status = 'completed' THEN
        RAISE EXCEPTION 'cannot modify job %: status is "completed". To retry, create a new job instead of resetting this one.',
            OLD.job_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION prevent_completed_job_delete()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status = 'completed' THEN
        RAISE EXCEPTION 'cannot delete job %: status is "completed". Completed jobs are retained for audit. See retention policy (ticket #34).',
            OLD.job_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jobs_immutable_update
    BEFORE UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION prevent_completed_job_mutation();

CREATE TRIGGER jobs_immutable_delete
    BEFORE DELETE ON jobs
    FOR EACH ROW EXECUTE FUNCTION prevent_completed_job_delete();

CREATE OR REPLACE FUNCTION prevent_completed_job_child_mutation()
RETURNS TRIGGER AS $$
DECLARE
    parent_status TEXT;
BEGIN
    SELECT status INTO parent_status FROM jobs WHERE job_id = OLD.job_id;
    IF parent_status = 'completed' THEN
        RAISE EXCEPTION 'cannot modify % record: parent job % is "completed". Completed job data is immutable.',
            TG_TABLE_NAME, OLD.job_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION prevent_completed_job_child_delete()
RETURNS TRIGGER AS $$
DECLARE
    parent_status TEXT;
BEGIN
    SELECT status INTO parent_status FROM jobs WHERE job_id = OLD.job_id;
    IF parent_status = 'completed' THEN
        RAISE EXCEPTION 'cannot delete % record: parent job % is "completed". Completed job data is immutable.',
            TG_TABLE_NAME, OLD.job_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER job_stages_immutable_update
    BEFORE UPDATE ON job_stages
    FOR EACH ROW EXECUTE FUNCTION prevent_completed_job_child_mutation();

CREATE TRIGGER job_stages_immutable_delete
    BEFORE DELETE ON job_stages
    FOR EACH ROW EXECUTE FUNCTION prevent_completed_job_child_delete();

CREATE TRIGGER vote_summaries_immutable_update
    BEFORE UPDATE ON vote_summaries
    FOR EACH ROW EXECUTE FUNCTION prevent_completed_job_child_mutation();

CREATE TRIGGER vote_summaries_immutable_delete
    BEFORE DELETE ON vote_summaries
    FOR EACH ROW EXECUTE FUNCTION prevent_completed_job_child_delete();

CREATE OR REPLACE FUNCTION prevent_classification_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'cannot modify classification (catalog_id=%, control_id=%, type=%): classifications are write-once. Insert a new version instead.',
        OLD.catalog_id, OLD.control_id, OLD.type
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER classifications_immutable_update
    BEFORE UPDATE ON classifications
    FOR EACH ROW EXECUTE FUNCTION prevent_classification_mutation();

CREATE TRIGGER classifications_immutable_delete
    BEFORE DELETE ON classifications
    FOR EACH ROW EXECUTE FUNCTION prevent_classification_mutation();

-- ============================================================================
-- Apache AGE graph lifecycle
-- ============================================================================
-- Each tenant gets a dedicated named graph (crosscodex_{tenant_id}).
-- This is the graph layer's equivalent of RLS — isolation by namespace
-- rather than by row policy, because AGE manages its own internal tables
-- that RLS cannot reach.

-- LOAD is safe here because migrations run as the postgres superuser.
-- Application code must NOT use LOAD — see pkg/graphdb/client.go for why.
LOAD 'age';
SET LOCAL search_path = ag_catalog, "$user", public;

CREATE OR REPLACE FUNCTION public.tenant_graph_name()
RETURNS TEXT AS $$
    SELECT 'crosscodex_' || current_setting('app.current_tenant');
$$ LANGUAGE sql STABLE;

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
-- Ownership transfer is critical: this trigger runs as the postgres
-- superuser, so ag_catalog.create_graph() creates the schema owned by
-- postgres. graph_user needs to own the schema and all objects inside it
-- because AGE cypher commands internally perform DDL.
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

GRANT EXECUTE ON FUNCTION public.tenant_graph_name() TO graph_user;
GRANT EXECUTE ON FUNCTION public.assert_tenant_graph(TEXT) TO graph_user;

GRANT USAGE ON SCHEMA ag_catalog TO graph_user;
GRANT SELECT ON ALL TABLES IN SCHEMA ag_catalog TO graph_user;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA ag_catalog TO graph_user;
