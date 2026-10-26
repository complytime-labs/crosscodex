-- Initial schema for CrossCodex: the baseline every database starts from.
--
-- Migrations run as the schema owner (`crosscodexd migrate`). The daemon and
-- every service connect as the restricted roles created below, never as the
-- owner: row-level security is enabled but not forced, so the owner and
-- superusers bypass it, and tenant isolation would otherwise rest on
-- application code alone. pkg/db/doc.go describes the role model.
--
-- Refusal triggers (refuse_change and the immutability triggers) fire for
-- every role, including the owner and superusers. Only a role able to disable
-- them can bypass them: the table owner (ALTER TABLE ... DISABLE TRIGGER) or a
-- superuser (that, or session_replication_role = replica). This relies on no
-- application role owning these tables or being superuser.
--
-- Error messages raised here are actionable: they say what happened, why it
-- was blocked, and what to do instead.

-- ============================================================================
-- Roles
-- ============================================================================
-- Roles are cluster-wide and survive a dropped database, so each is created
-- only if missing. Passwords are set by the operator
-- (ALTER ROLE ... PASSWORD ...), never by a migration.

-- app_user serves network requests and owns nothing. RLS policies are not
-- enforced for the table owner, so application connections use this role
-- and PostgreSQL, not application code, enforces tenant isolation.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_user') THEN
        CREATE ROLE app_user LOGIN;
    END IF;
END
$$;

-- graph_user runs graph operations. AGE cypher commands internally perform
-- DDL (creating label tables, altering sequences) inside per-tenant graph
-- schemas, and PostgreSQL requires the calling role to OWN those objects for
-- DDL. A dedicated role keeps the blast radius contained: it owns graph
-- schemas but has zero access to relational tables, and vice versa for
-- app_user.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'graph_user') THEN
        CREATE ROLE graph_user LOGIN;
    END IF;
END
$$;

-- retention_purge is a NOLOGIN group role whose membership authorises the
-- delete-path carve-out in the immutability triggers (pg_has_role check).
-- Role membership is engine-enforced and cannot be self-granted; app_user is
-- NOT a member.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'retention_purge') THEN
        CREATE ROLE retention_purge NOLOGIN;
    END IF;
END
$$;

-- purge_user is the LOGIN role of the retention-purge service. It holds only
-- the SELECT and DELETE grants the purge needs (see "Retention") and does
-- NOT inherit app_user's broader INSERT/UPDATE grants.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'purge_user') THEN
        CREATE ROLE purge_user LOGIN;
    END IF;
END
$$;

GRANT retention_purge TO purge_user;

-- backup_user runs `crosscodexd admin backup`. It needs REPLICATION so
-- WAL-G can stream BASE_BACKUP, pg_read_all_settings because WAL-G reads
-- data_directory (a superuser-only setting) before every backup, and it
-- must enumerate tenant IDs so the objects step can open each tenant's
-- object store (see "Tenants"). It gets nothing else: no tenant data, no
-- writes, no RLS bypass.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'backup_user') THEN
        CREATE ROLE backup_user LOGIN REPLICATION;
    END IF;
END
$$;

-- Read-only: lets the role see settings, not change them.
GRANT pg_read_all_settings TO backup_user;

-- tenant_admin runs `crosscodexd admin tenant`, `admin reviewer`,
-- `admin identity` and `admin retention scan --all-tenants`, so operators
-- can manage tenants and reviewers without the owner or superuser
-- credential. It holds no table, sequence or schema
-- grant: it may only EXECUTE the SECURITY DEFINER functions in "Tenant
-- administration". It cannot read tenant data or bulk-UPDATE, DELETE or
-- TRUNCATE tenants.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tenant_admin') THEN
        CREATE ROLE tenant_admin LOGIN;
    END IF;
END
$$;

-- ============================================================================
-- Refusal trigger
-- ============================================================================
-- refuse_change raises its trigger argument as an insufficient_privilege
-- error. The permanent tables below attach it to DELETE, UPDATE and TRUNCATE
-- with a message naming the supported alternative.

CREATE FUNCTION public.refuse_change() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE = '42501', MESSAGE = TG_ARGV[0];
END
$$;
REVOKE ALL ON FUNCTION public.refuse_change() FROM PUBLIC;

-- ============================================================================
-- Tenants and identity
-- ============================================================================
-- Judgements are never deleted; tenants and reviewers are soft-deleted, can
-- be hidden behind a pseudonym an admin can trace, and every identity change
-- is recorded in identity_events with a signed in-toto link
-- (docs/dev/design-principles.md "Identity lifecycle").
--
-- app_user and graph_user serve network requests, so a compromised or buggy
-- service must not be able to erase or rewrite who decided what. app_user
-- has no DELETE or UPDATE on tenants (no app code needs them; UPDATE would
-- let a tenant-scoped session un-delete its own tenant) and only SELECT on
-- reviewers. identity_events has no grant to any role.

-- Tenant lifecycle. pkg/db TenantStatus* mirror the status list. A deleted
-- tenant keeps its row and data; it may additionally be hidden, after which
-- list_tenants shows only its pseudonym. The pseudonym is kept on unhide and
-- restore so hiding again yields the same one and old reports stay
-- traceable.
CREATE TABLE IF NOT EXISTS tenants (
    tenant_id    TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at   timestamptz,
    deleted_by   text,
    pseudonym    text UNIQUE,
    hidden_at    timestamptz,
    CONSTRAINT tenants_status_check CHECK (status IN ('active', 'suspended', 'deleted')),
    CONSTRAINT tenants_deleted_consistent
        CHECK ((status = 'deleted') = (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)),
    CONSTRAINT tenants_hidden_consistent
        CHECK (hidden_at IS NULL OR (status = 'deleted' AND pseudonym IS NOT NULL)),
    CONSTRAINT tenants_pseudonym_format
        CHECK (pseudonym IS NULL OR pseudonym ~ '^hidden-tenant-[0-9a-f]{16}$'),
    -- Tenant IDs starting with 'hidden-' are reserved for pseudonyms of
    -- hidden tenants. A constraint, not only provision_tenant's check,
    -- because EnsureTenant inserts tenants directly.
    CONSTRAINT tenants_id_not_reserved CHECK (tenant_id NOT LIKE 'hidden-%')
);

ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON tenants
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT ON tenants TO app_user;

-- Column-level grant: tenant_id only, so display_name and status stay hidden
-- from backup_user.
GRANT SELECT (tenant_id) ON public.tenants TO backup_user;

-- tenant_isolation matches zero rows without app.current_tenant, and
-- backup_user must see every tenant, so it gets its own SELECT-only policy.
-- A policy filters rows; the grant above is what permits the read at all.
CREATE POLICY backup_list_tenants ON public.tenants
    FOR SELECT TO backup_user
    USING (true);

CREATE TRIGGER tenants_refuse_delete
    BEFORE DELETE ON public.tenants
    FOR EACH ROW EXECUTE FUNCTION public.refuse_change(
        'tenants are never hard-deleted because their judgements are permanent. Run ''crosscodexd admin tenant delete --tenant <id> --by <operator> --reason <text>'' to soft-delete instead.');
CREATE TRIGGER tenants_refuse_truncate
    BEFORE TRUNCATE ON public.tenants
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_change(
        'tenants are never hard-deleted because their judgements are permanent, so TRUNCATE is refused. Run ''crosscodexd admin tenant delete --tenant <id> --by <operator> --reason <text>'' to soft-delete each tenant instead.');

-- identity_events is the append-only audit log of identity changes and
-- traces. request holds the canonical request JSON; attestation is a signed
-- in-toto link whose single material digest is sha256(request), so a
-- verifier needs only the row and the public key. No role is granted any
-- privilege on it: rows are written only by the SECURITY DEFINER functions
-- in "Tenant administration", and tenant_admin reads them only through
-- export_identity_events.
CREATE TABLE public.identity_events (
    event_id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at        timestamptz NOT NULL DEFAULT now(),
    action             text NOT NULL CHECK (action IN ('add', 'delete', 'restore', 'hide', 'unhide', 'trace')),
    kind               text NOT NULL CHECK (kind IN ('tenant', 'reviewer')),
    tenant_id          text NOT NULL REFERENCES public.tenants (tenant_id) ON DELETE RESTRICT,
    identity_id        text NOT NULL,
    actor              text NOT NULL,
    db_user            text NOT NULL DEFAULT session_user,
    reason             text NOT NULL,
    request            bytea NOT NULL,
    attestation        bytea NOT NULL,
    attestation_sha256 text GENERATED ALWAYS AS (encode(sha256(attestation), 'hex')) STORED
);
REVOKE ALL ON public.identity_events FROM PUBLIC;
CREATE TRIGGER identity_events_refuse_update
    BEFORE UPDATE OR DELETE ON public.identity_events
    FOR EACH ROW EXECUTE FUNCTION public.refuse_change(
        'identity_events is an append-only audit log; rows are never changed or removed.');
CREATE TRIGGER identity_events_refuse_truncate
    BEFORE TRUNCATE ON public.identity_events
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_change(
        'identity_events is an append-only audit log; rows are never changed or removed.');

-- reviewers registers, per tenant, the people who record human verdicts:
-- determined_by on a human verdict holds reviewer_id. Rows are soft-deleted
-- and hidden like tenants. app_user reads it to show a hidden reviewer's
-- pseudonym; every change goes through the tenant_admin functions.
-- reviewers_id_format mirrors db.ValidateReviewerID (MaxReviewerBytes
-- = 256); Go stays the boundary check that also refuses format characters.
CREATE TABLE public.reviewers (
    tenant_id   text NOT NULL REFERENCES public.tenants (tenant_id) ON DELETE RESTRICT,
    reviewer_id text NOT NULL,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deleted')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  text NOT NULL,
    deleted_at  timestamptz,
    deleted_by  text,
    pseudonym   text UNIQUE,
    hidden_at   timestamptz,
    PRIMARY KEY (tenant_id, reviewer_id),
    CONSTRAINT reviewers_id_format
        CHECK (reviewer_id !~ '^\s*$' AND octet_length(reviewer_id) <= 256
               AND reviewer_id !~ '[\u0001-\u001F\u007F-\u009F]' AND reviewer_id = btrim(reviewer_id)),
    -- IDs starting with 'hidden-' are reserved, so output can never show a
    -- real reviewer ID that looks like a pseudonym.
    CONSTRAINT reviewers_id_not_reserved CHECK (reviewer_id NOT LIKE 'hidden-%'),
    CONSTRAINT reviewers_pseudonym_format
        CHECK (pseudonym IS NULL OR pseudonym ~ '^hidden-reviewer-[0-9a-f]{16}$'),
    CONSTRAINT reviewers_deleted_consistent
        CHECK ((status = 'deleted') = (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)),
    CONSTRAINT reviewers_hidden_consistent
        CHECK (hidden_at IS NULL OR (status = 'deleted' AND pseudonym IS NOT NULL))
);
ALTER TABLE public.reviewers ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.reviewers
    USING (tenant_id = current_setting('app.current_tenant', true));
GRANT SELECT ON public.reviewers TO app_user;
CREATE TRIGGER reviewers_refuse_delete
    BEFORE DELETE ON public.reviewers
    FOR EACH ROW EXECUTE FUNCTION public.refuse_change(
        'reviewers are never deleted because their judgements are permanent. Run ''crosscodexd admin reviewer delete'' to soft-delete instead.');
CREATE TRIGGER reviewers_refuse_truncate
    BEFORE TRUNCATE ON public.reviewers
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_change(
        'reviewers are never deleted because their judgements are permanent, so TRUNCATE is refused. Run ''crosscodexd admin reviewer delete'' to soft-delete each reviewer instead.');

-- ============================================================================
-- Jobs and pipeline results
-- ============================================================================

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

-- vote_summaries holds one row per analyzer contribution to a (source, target)
-- pair, not one row per pair. analyzer_name names the analyzer that produced
-- the row, as in analysis_results.analyzer_name ('requires' or 'relationship'
-- today); a pair both analyzers report has two rows. Synthesis ranks one row
-- per contribution and writes each row's viability back to the row of the
-- same analyzer, so the key must include it: a per-pair key would make two
-- synthesis rows target one table row. There is no DEFAULT, so a writer that
-- omits the analyzer fails. The only CHECK forbids the empty string, which
-- would otherwise pass NOT NULL and key a nameless contribution; there is no
-- list of allowed values, because analyzers are extensible.
CREATE TABLE IF NOT EXISTS vote_summaries (
    job_id            TEXT NOT NULL REFERENCES jobs(job_id),
    source_id         TEXT NOT NULL,
    target_id         TEXT NOT NULL,
    analyzer_name     TEXT NOT NULL CHECK (analyzer_name <> ''),
    consensus         TEXT NOT NULL,
    confidence        DOUBLE PRECISION NOT NULL,
    viability         DOUBLE PRECISION NOT NULL,
    tenant_id         TEXT NOT NULL REFERENCES tenants(tenant_id),
    PRIMARY KEY (job_id, source_id, target_id, analyzer_name)
);

ALTER TABLE vote_summaries ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON vote_summaries
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON vote_summaries TO app_user;

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

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS analysis_results (
    tenant_id     TEXT        NOT NULL REFERENCES tenants(tenant_id),
    job_id        TEXT        NOT NULL REFERENCES jobs(job_id),
    analyzer_name TEXT        NOT NULL,
    result_data   JSONB       NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, job_id, analyzer_name)
);

ALTER TABLE analysis_results ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON analysis_results
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON analysis_results TO app_user;

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS relationship_candidates (
    tenant_id        TEXT        NOT NULL REFERENCES tenants(tenant_id),
    job_id           TEXT        NOT NULL REFERENCES jobs(job_id),
    source_id        TEXT        NOT NULL,
    target_id        TEXT        NOT NULL,
    similarity_score REAL        NOT NULL,             -- [0, 100] from embedding similarity matrix (see relationship.CandidatePair.SimilarityScore) -- NOTE: differs in scale from requires_candidates.aggregate_score, which is [0.0, 1.0]
    provenance       JSONB       NOT NULL DEFAULT '[]',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, job_id, source_id, target_id)
);

CREATE INDEX IF NOT EXISTS idx_relationship_candidates_job
    ON relationship_candidates (tenant_id, job_id);

ALTER TABLE relationship_candidates ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON relationship_candidates
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON relationship_candidates TO app_user;

-- ---------------------------------------------------------------------------
-- Completed jobs are immutable. Deletion is allowed only to members of
-- retention_purge, so retention can purge a completed job and its children.

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
        IF NOT pg_has_role(current_user, 'retention_purge', 'MEMBER') THEN
            RAISE EXCEPTION 'cannot delete job %: status is "completed". Completed jobs are retained for audit. See retention policy (ticket #34).',
                OLD.job_id
                USING ERRCODE = 'restrict_violation';
        END IF;
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
        IF NOT pg_has_role(current_user, 'retention_purge', 'MEMBER') THEN
            RAISE EXCEPTION 'cannot delete % record: parent job % is "completed". Completed job data is immutable.',
                TG_TABLE_NAME, OLD.job_id
                USING ERRCODE = 'restrict_violation';
        END IF;
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

-- ============================================================================
-- Catalogs and their dependents
-- ============================================================================
-- Every catalog dependent is tied to a catalog of its own tenant. Foreign-key
-- checks ignore row-level security, and catalog_id is unique across all
-- tenants, so a single-column key would let a row of one tenant reference
-- another tenant's catalog. Referencing (catalog_id, tenant_id) makes the
-- database refuse that for every role, the owner included. catalog_id stays
-- the primary key; catalogs_catalog_tenant_key exists only as the target of
-- the composite foreign keys. ON DELETE is NO ACTION: catalog purge deletes
-- the dependents before the catalog.

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
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT catalogs_catalog_tenant_key UNIQUE (catalog_id, tenant_id)
);

ALTER TABLE catalogs ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON catalogs
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON catalogs TO app_user;

-- ---------------------------------------------------------------------------

-- classifications needs no tenant in its key: its rows are write-once, so no
-- upsert can take one over, and the composite foreign key binds its
-- catalog_id to one tenant.
CREATE TABLE IF NOT EXISTS classifications (
    catalog_id TEXT NOT NULL,
    control_id TEXT NOT NULL,
    type       TEXT NOT NULL,
    level      TEXT NOT NULL,
    tenant_id  TEXT NOT NULL REFERENCES tenants(tenant_id),
    PRIMARY KEY (catalog_id, control_id, type),
    CONSTRAINT classifications_catalog_tenant_fkey
        FOREIGN KEY (catalog_id, tenant_id) REFERENCES public.catalogs (catalog_id, tenant_id)
);

ALTER TABLE classifications ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON classifications
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON classifications TO app_user;

CREATE OR REPLACE FUNCTION prevent_classification_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF NOT pg_has_role(current_user, 'retention_purge', 'MEMBER') THEN
        RAISE EXCEPTION 'cannot modify classification (catalog_id=%, control_id=%, type=%): classifications are write-once. Insert a new version instead.',
            OLD.catalog_id, OLD.control_id, OLD.type
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER classifications_immutable_update
    BEFORE UPDATE ON classifications
    FOR EACH ROW EXECUTE FUNCTION prevent_classification_mutation();

CREATE TRIGGER classifications_immutable_delete
    BEFORE DELETE ON classifications
    FOR EACH ROW EXECUTE FUNCTION prevent_classification_mutation();

-- ---------------------------------------------------------------------------

-- The vector column is model-agnostic (dimensionless): a bare `vector` with no
-- fixed width, so embeddings of any model's dimensionality coexist in one table.
-- A fixed width (e.g. vector(2000)) was rejected — see
-- docs/dev/adr/0001-model-agnostic-embedding-storage.md. The ivfflat index
-- requires a fixed dimension, so it is deliberately omitted; every <=> query is
-- model-scoped (single width per query), so similarity falls back to an exact
-- scan, which is correct at current scale. When scale demands ANN, add per-model
-- partial expression indexes per deployment (see docs/dev/embeddings.md).
--
-- The key includes tenant_id so one tenant's upsert can never collide with
-- another tenant's row (as the owner it would take the row over; under RLS
-- it would fail with an opaque policy error). tenant_id leads so the key
-- serves the tenant-scoped reads; the composite foreign key is what ties
-- each catalog_id to one tenant, so the tenant column adds no uniqueness.
CREATE TABLE IF NOT EXISTS embeddings (
    catalog_id TEXT NOT NULL,
    control_id TEXT NOT NULL,
    model      TEXT NOT NULL,
    vector     vector NOT NULL,
    tenant_id  TEXT NOT NULL REFERENCES tenants(tenant_id),
    PRIMARY KEY (tenant_id, catalog_id, control_id, model),
    CONSTRAINT embeddings_catalog_tenant_fkey
        FOREIGN KEY (catalog_id, tenant_id) REFERENCES public.catalogs (catalog_id, tenant_id)
);

ALTER TABLE embeddings ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON embeddings
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON embeddings TO app_user;

-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS controls (
    tenant_id     TEXT NOT NULL REFERENCES tenants(tenant_id),
    control_id    TEXT NOT NULL,
    catalog_id    TEXT NOT NULL,
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
    PRIMARY KEY (tenant_id, control_id),
    CONSTRAINT controls_catalog_tenant_fkey
        FOREIGN KEY (catalog_id, tenant_id) REFERENCES public.catalogs (catalog_id, tenant_id)
);

CREATE INDEX idx_controls_catalog ON controls(catalog_id);
CREATE INDEX idx_controls_parent ON controls(parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX idx_controls_class ON controls(class);
CREATE INDEX idx_controls_search ON controls USING GIN(search_vector);

ALTER TABLE controls ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON controls
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON controls TO app_user;

-- ============================================================================
-- Retention
-- ============================================================================
-- retention_holds tracks active legal or compliance holds that prevent purge
-- of matching rows. A hold is active while released_at IS NULL; releasing it
-- sets released_at and released_by, but keeps the row for audit purposes.
--
-- The partial unique index enforces that at most one active hold may carry a
-- given name at any time, while allowing multiple released holds with the
-- same name across time.
--
-- tenant_id is NULLABLE: a NULL tenant_id is a GLOBAL hold that applies to
-- all tenants (mirrors HoldScope wildcard semantics in pkg/retention/hold.go).
-- The USING clause therefore lets a tenant see its own holds AND global
-- holds, so a global hold correctly blocks every tenant's purge. The WITH
-- CHECK clause is STRICTER: it forbids app_user from INSERTing/UPDATEing a
-- row for any tenant other than the current one (and forbids creating global
-- NULL-tenant holds via app_user) — global holds are a privileged-path
-- concept, never created over the tenant-scoped admin API.

CREATE TABLE IF NOT EXISTS public.retention_holds (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT        NOT NULL,
    tenant_id       TEXT        NULL,
    created_before  TIMESTAMPTZ NULL,
    job_id          TEXT        NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by      TEXT        NOT NULL,
    expires_at      TIMESTAMPTZ NULL,
    released_at     TIMESTAMPTZ NULL,
    released_by     TEXT        NULL
);

-- Only one active hold per name (released_at IS NULL) is permitted.
CREATE UNIQUE INDEX IF NOT EXISTS retention_holds_name_active_uidx
    ON public.retention_holds (name)
    WHERE released_at IS NULL;

ALTER TABLE public.retention_holds ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON public.retention_holds
    USING (
        tenant_id = current_setting('app.current_tenant', true)
        OR tenant_id IS NULL
    )
    WITH CHECK (
        tenant_id = current_setting('app.current_tenant', true)
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.retention_holds TO app_user;

-- purge_user may read and delete exactly what retention purges: a completed
-- job with its children (FK children without cascade, plus
-- requires_candidates, which is associated by job_id without an FK), and a
-- catalog with its dependents. Tables without an immutability delete trigger
-- need only the grant; the others also rely on retention_purge membership.
GRANT USAGE ON SCHEMA public TO purge_user;
GRANT SELECT, DELETE ON public.jobs                    TO purge_user;
GRANT SELECT, DELETE ON public.job_stages              TO purge_user;
GRANT SELECT, DELETE ON public.catalogs                TO purge_user;
GRANT SELECT, DELETE ON public.classifications         TO purge_user;
GRANT SELECT, DELETE ON public.embeddings              TO purge_user;
GRANT SELECT, DELETE ON public.vote_summaries          TO purge_user;
GRANT SELECT, DELETE ON public.analysis_results        TO purge_user;
GRANT SELECT, DELETE ON public.relationship_candidates TO purge_user;
GRANT SELECT, DELETE ON public.controls                TO purge_user;
GRANT SELECT, DELETE ON public.requires_candidates     TO purge_user;

-- ============================================================================
-- Artifact pair verdicts
-- ============================================================================
-- artifact_pair_verdicts is the work queue and verdict record for SAME_AS
-- candidates between ArtifactGroup nodes (issue #170). The reconciler
-- enqueues token-overlap matches; `crosscodexd admin adjudicate artifacts`
-- leases pending rows, runs an LLM panel and records the verdict. The graph
-- is a projection of this table, never the other way round.
--
-- determination_type ranks authority: human > llm_panel > token_overlap.
-- Automation never leases or overwrites a human row (#180's `decide` writes
-- them); projection passes do project human rows into the graph.
--
-- low_group_id < high_group_id is compared under COLLATE "C" so the check
-- matches Go's byte-wise string order whatever the database collation.
--
-- Verdicts are permanent judgements: the tenant foreign key is RESTRICT and
-- DELETE and TRUNCATE are refused for every role.
CREATE TABLE IF NOT EXISTS artifact_pair_verdicts (
    tenant_id          TEXT NOT NULL REFERENCES tenants(tenant_id) ON DELETE RESTRICT,
    pair_key           TEXT NOT NULL,
    low_group_id       TEXT NOT NULL,
    high_group_id      TEXT NOT NULL,
    candidate_edge_id  TEXT NOT NULL,
    similarity_score   DOUBLE PRECISION NOT NULL,
    context            JSONB NOT NULL,
    status             TEXT NOT NULL DEFAULT 'pending',
    determination_type TEXT NOT NULL DEFAULT 'token_overlap',
    determined_by      TEXT NOT NULL,
    confidence         DOUBLE PRECISION,
    evidence           JSONB,
    dissent            JSONB,
    graph_applied      BOOLEAN NOT NULL DEFAULT false,
    attempts           INT NOT NULL DEFAULT 0,
    next_attempt_at    TIMESTAMPTZ NOT NULL,
    leased_until       TIMESTAMPTZ,
    last_error         TEXT,
    created_at         TIMESTAMPTZ NOT NULL,
    decided_at         TIMESTAMPTZ,
    -- A projection pass holds a decided row until this time while it writes
    -- the verdict to the graph; no other pass projects the row meanwhile, so
    -- a newer verdict is never projected before an older one's graph writes.
    -- NULL or past: unclaimed.
    projection_claimed_until TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, pair_key),
    CONSTRAINT artifact_pair_verdicts_order
        CHECK (low_group_id COLLATE "C" < high_group_id COLLATE "C"),
    CONSTRAINT artifact_pair_verdicts_status
        CHECK (status IN ('pending', 'confirmed', 'rejected', 'undecided', 'abandoned')),
    CONSTRAINT artifact_pair_verdicts_determination
        CHECK (determination_type IN ('token_overlap', 'llm_panel', 'human')),
    CONSTRAINT artifact_pair_verdicts_context_object
        CHECK (jsonb_typeof(context) = 'object'),
    CONSTRAINT artifact_pair_verdicts_evidence_object
        CHECK (evidence IS NULL OR jsonb_typeof(evidence) = 'object'),
    CONSTRAINT artifact_pair_verdicts_dissent_object
        CHECK (dissent IS NULL OR jsonb_typeof(dissent) = 'object')
);

-- Serves the adjudicator's lease query and, through its (tenant_id, status)
-- prefix, the reconciler's closed-pair load and the dry-run counts.
CREATE INDEX IF NOT EXISTS idx_artifact_pair_verdicts_queue
    ON artifact_pair_verdicts (tenant_id, status, next_attempt_at);

ALTER TABLE artifact_pair_verdicts ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON artifact_pair_verdicts
    USING (tenant_id = current_setting('app.current_tenant', true));

-- No DELETE: verdicts are a permanent record; nothing in the app removes one.
GRANT SELECT, INSERT, UPDATE ON artifact_pair_verdicts TO app_user;

CREATE TRIGGER artifact_pair_verdicts_refuse_delete
    BEFORE DELETE ON public.artifact_pair_verdicts
    FOR EACH ROW EXECUTE FUNCTION public.refuse_change(
        'artifact_pair_verdicts rows are permanent judgements and are never deleted. To retire a tenant run ''crosscodexd admin tenant delete'' (soft delete); to retire a reviewer run ''crosscodexd admin reviewer delete''.');
CREATE TRIGGER artifact_pair_verdicts_refuse_truncate
    BEFORE TRUNCATE ON public.artifact_pair_verdicts
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_change(
        'artifact_pair_verdicts rows are permanent judgements and are never deleted, so TRUNCATE is refused. To retire a tenant run ''crosscodexd admin tenant delete'' (soft delete); to retire a reviewer run ''crosscodexd admin reviewer delete''.');

-- ============================================================================
-- Tenant administration
-- ============================================================================
-- These functions are tenant_admin's only access. They are SECURITY DEFINER,
-- so they and the tenant_graph_create trigger a tenant insert fires (not
-- DEFINER; ag_catalog.create_graph needs owner rights) run as the schema
-- owner.
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

-- provision_tenant creates a tenant (and, via the trigger, its graph) or
-- renames an existing one, leaving its status alone. Returns true if it
-- inserted. It refuses the reserved 'hidden-' ID prefix and renaming a
-- deleted tenant. The ID regex must match pkg/tenant tenantIDPattern
-- exactly.
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
    IF p_id LIKE 'hidden-%' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'tenant ID %L is invalid: tenant IDs starting with ''hidden-'' are reserved for pseudonyms of hidden tenants; choose another ID',
            left(p_id, 64));
    END IF;
    IF EXISTS (SELECT 1 FROM public.tenants WHERE tenant_id = p_id AND status = 'deleted') THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is deleted; nothing was changed. Restore it with ''crosscodexd admin tenant restore'' before renaming it', left(p_id, 64));
    END IF;

    INSERT INTO public.tenants (tenant_id, display_name) VALUES (p_id, p_name)
        ON CONFLICT (tenant_id) DO NOTHING;
    IF FOUND THEN
        RETURN true;
    END IF;

    -- The status filter closes the race with a concurrent delete_tenant
    -- between the check above and this UPDATE; tenants are never removed,
    -- so NOT FOUND here means exactly that.
    UPDATE public.tenants SET display_name = p_name WHERE tenant_id = p_id AND status <> 'deleted';
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L was deleted while it was being renamed; nothing was changed. Restore it with ''crosscodexd admin tenant restore'' before renaming it', left(p_id, 64));
    END IF;
    RETURN false;
END
$$;

-- set_tenant_status sets a tenant's status and returns the previous one.
-- Setting the current status is a no-op that still returns it. It refuses a
-- deleted tenant: only restore_tenant leaves 'deleted', so the delete and
-- restore events stay paired.
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
    IF v_previous = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is deleted; nothing was changed. Run ''crosscodexd admin tenant restore --tenant %s --by <operator> --reason <text>'' first',
            left(p_id, 64), left(p_id, 64));
    END IF;

    UPDATE public.tenants SET status = p_status WHERE tenant_id = p_id;
    RETURN v_previous;
END
$$;

-- list_tenants returns every tenants row; it never touches child tables.
CREATE FUNCTION public.list_tenants()
RETURNS TABLE (tenant_id text, display_name text, status text, created_at timestamptz,
               deleted_at timestamptz, hidden boolean)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
    -- A hidden tenant is shown only by its pseudonym; 'crosscodexd admin
    -- identity trace' maps it back.
    SELECT CASE WHEN t.hidden_at IS NULL THEN t.tenant_id ELSE t.pseudonym END,
           CASE WHEN t.hidden_at IS NULL THEN t.display_name ELSE '' END,
           t.status, t.created_at, t.deleted_at, t.hidden_at IS NOT NULL
    FROM public.tenants t
    ORDER BY 1
$$;

REVOKE ALL ON FUNCTION public.provision_tenant(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.set_tenant_status(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.list_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.provision_tenant(text, text) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.set_tenant_status(text, text) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.list_tenants() TO tenant_admin;

-- record_identity_event validates the operator input every action takes and
-- appends one event. It is not SECURITY DEFINER and no role may EXECUTE it:
-- only the definer functions below, running as the owner, call it.
--
-- The signed request must name this exact change, so a request signed for
-- one tenant or identity cannot be replayed against another. p_subject is
-- the identity the request names: p_identity for every action except
-- 'trace', whose request names the pseudonym being traced and, because the
-- operator does not know them yet, leaves kind and tenant_id empty.
CREATE FUNCTION public.record_identity_event(
    p_action text, p_kind text, p_tenant text, p_identity text, p_subject text,
    p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_req      jsonb;
    v_expected jsonb;
    v_differs  text;
BEGIN
    IF p_actor IS NULL OR p_actor ~ '^\s*$' OR octet_length(p_actor) > 256
       OR p_actor ~ '[\u0001-\u001F\u007F-\u009F]' OR p_actor <> btrim(p_actor) THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'actor (--by) must be 1-256 bytes of printable text without leading or trailing spaces; nothing was changed';
    END IF;
    IF p_reason IS NULL OR p_reason ~ '^\s*$' OR octet_length(p_reason) > 500
       OR p_reason ~ '[\u0001-\u001F\u007F-\u009F]' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'reason (--reason) must be 1-500 bytes of printable text; nothing was changed';
    END IF;
    IF p_attestation IS NULL OR octet_length(p_attestation) = 0 THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'identity change has no attestation; nothing was changed. Run it through crosscodexd admin, which signs every change';
    END IF;
    IF octet_length(p_request) > 4096 THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'identity request is larger than 4096 bytes; nothing was changed. Run it through crosscodexd admin, which builds a request of bounded size';
    END IF;
    IF octet_length(p_attestation) > 65536 THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'identity attestation is larger than 65536 bytes; nothing was changed. Run it through crosscodexd admin, which signs a single link';
    END IF;
    BEGIN
        v_req := convert_from(p_request, 'UTF8')::jsonb;
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'identity request is not valid UTF-8 JSON; nothing was changed. Run it through crosscodexd admin';
    END;
    v_expected := jsonb_build_object('action', p_action, 'actor', p_actor, 'reason', p_reason, 'identity_id', p_subject);
    IF p_action <> 'trace' THEN
        v_expected := v_expected || jsonb_build_object('kind', p_kind, 'tenant_id', p_tenant);
    END IF;
    SELECT string_agg(e.key, ', ' ORDER BY e.key) INTO v_differs
    FROM jsonb_each_text(v_expected) e
    WHERE v_req->>e.key IS DISTINCT FROM e.value;
    IF v_differs IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'signed identity request does not match the change: its %s %s; nothing was changed. Sign a new request for this change',
            v_differs, CASE WHEN strpos(v_differs, ',') > 0 THEN 'differ' ELSE 'differs' END);
    END IF;
    INSERT INTO public.identity_events
        (action, kind, tenant_id, identity_id, actor, reason, request, attestation)
    VALUES (p_action, p_kind, p_tenant, p_identity, p_actor, p_reason, p_request, p_attestation);
END
$$;
REVOKE ALL ON FUNCTION public.record_identity_event(text, text, text, text, text, text, text, bytea, bytea) FROM PUBLIC;

-- delete_tenant soft-deletes a tenant: its row, data and judgements stay,
-- and enter_tenant refuses it on every app path.
CREATE FUNCTION public.delete_tenant(p_id text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
BEGIN
    SELECT status INTO v_status FROM public.tenants WHERE tenant_id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'tenant %L does not exist; nothing was changed. Check the ID with ''crosscodexd admin tenant list''', left(p_id, 64));
    END IF;
    IF v_status = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is already deleted; nothing was changed. To bring it back run ''crosscodexd admin tenant restore''', left(p_id, 64));
    END IF;
    UPDATE public.tenants SET status = 'deleted', deleted_at = now(), deleted_by = p_actor WHERE tenant_id = p_id;
    PERFORM public.record_identity_event('delete', 'tenant', p_id, p_id, p_id, p_actor, p_reason, p_request, p_attestation);
END
$$;

-- restore_tenant brings a deleted, unhidden tenant back as suspended, so an
-- operator re-activates it deliberately with set_tenant_status.
CREATE FUNCTION public.restore_tenant(p_id text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
    v_hidden_at timestamptz;
BEGIN
    SELECT status, hidden_at INTO v_status, v_hidden_at FROM public.tenants WHERE tenant_id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'tenant %L does not exist; nothing was changed. Check the ID with ''crosscodexd admin tenant list''', left(p_id, 64));
    END IF;
    IF v_status <> 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is not deleted (status %L); nothing was changed', left(p_id, 64), v_status);
    END IF;
    IF v_hidden_at IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is hidden; nothing was changed. Run ''crosscodexd admin tenant unhide'' first', left(p_id, 64));
    END IF;
    UPDATE public.tenants SET status = 'suspended', deleted_at = NULL, deleted_by = NULL WHERE tenant_id = p_id;
    PERFORM public.record_identity_event('restore', 'tenant', p_id, p_id, p_id, p_actor, p_reason, p_request, p_attestation);
END
$$;

-- hide_tenant shows a deleted tenant only by its pseudonym and returns it.
-- The pseudonym is 64 bits of a hash of a random UUID (sha256, not md5, so
-- it works under FIPS), is kept on unhide, and is reused on the next hide.
CREATE FUNCTION public.hide_tenant(p_id text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
    v_hidden_at timestamptz;
    v_pseudonym text;
BEGIN
    SELECT status, hidden_at INTO v_status, v_hidden_at FROM public.tenants WHERE tenant_id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'tenant %L does not exist; nothing was changed. Check the ID with ''crosscodexd admin tenant list''', left(p_id, 64));
    END IF;
    IF v_status <> 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'only a deleted tenant can be hidden; tenant %L is %L. Delete it first with ''crosscodexd admin tenant delete''',
            left(p_id, 64), v_status);
    END IF;
    IF v_hidden_at IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is already hidden; nothing was changed', left(p_id, 64));
    END IF;
    UPDATE public.tenants
    SET pseudonym = COALESCE(pseudonym, 'hidden-tenant-' || left(encode(sha256(uuid_send(gen_random_uuid())), 'hex'), 16)),
        hidden_at = now()
    WHERE tenant_id = p_id
    RETURNING pseudonym INTO v_pseudonym;
    PERFORM public.record_identity_event('hide', 'tenant', p_id, p_id, p_id, p_actor, p_reason, p_request, p_attestation);
    RETURN v_pseudonym;
END
$$;

-- unhide_tenant shows a hidden tenant by its ID again; it stays deleted.
CREATE FUNCTION public.unhide_tenant(p_id text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_hidden_at timestamptz;
BEGIN
    SELECT hidden_at INTO v_hidden_at FROM public.tenants WHERE tenant_id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'tenant %L does not exist; nothing was changed. Check the ID with ''crosscodexd admin tenant list''', left(p_id, 64));
    END IF;
    IF v_hidden_at IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is not hidden; nothing was changed', left(p_id, 64));
    END IF;
    UPDATE public.tenants SET hidden_at = NULL WHERE tenant_id = p_id;
    PERFORM public.record_identity_event('unhide', 'tenant', p_id, p_id, p_id, p_actor, p_reason, p_request, p_attestation);
END
$$;

REVOKE ALL ON FUNCTION public.delete_tenant(text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.restore_tenant(text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.hide_tenant(text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.unhide_tenant(text, text, text, bytea, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.delete_tenant(text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.restore_tenant(text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.hide_tenant(text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.unhide_tenant(text, text, text, bytea, bytea) TO tenant_admin;

-- add_reviewer registers a reviewer and returns true, or returns false when
-- the reviewer is already active (upsert over reject); both are recorded.
-- A deleted reviewer is refused: restore_reviewer brings it back.
--
-- On a deleted tenant, add_reviewer and restore_reviewer are refused, since
-- they would make a reviewer usable again; delete, hide and unhide stay
-- allowed so a deleted tenant's reviewers can still be retired and concealed.
CREATE FUNCTION public.add_reviewer(p_tenant text, p_reviewer text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tenant_status text;
    v_status text;
    v_inserted boolean;
BEGIN
    -- FOR SHARE keeps delete_tenant from running until this commits.
    SELECT status INTO v_tenant_status FROM public.tenants WHERE tenant_id = p_tenant FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'tenant %L does not exist; nothing was changed. Check the ID with ''crosscodexd admin tenant list''', left(p_tenant, 64));
    END IF;
    IF v_tenant_status = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is deleted; nothing was changed. To add reviewers, restore the tenant first with ''crosscodexd admin tenant restore''',
            left(p_tenant, 64));
    END IF;
    IF p_reviewer LIKE 'hidden-%' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'reviewer ID %L is invalid: reviewer IDs starting with ''hidden-'' are reserved for pseudonyms of hidden reviewers; nothing was changed. Choose another ID',
            left(p_reviewer, 64));
    END IF;
    SELECT status INTO v_status FROM public.reviewers
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer FOR UPDATE;
    IF v_status = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'reviewer %L of tenant %L is deleted; nothing was changed. Run ''crosscodexd admin reviewer restore'' instead',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    BEGIN
        INSERT INTO public.reviewers (tenant_id, reviewer_id, created_by)
        VALUES (p_tenant, p_reviewer, p_actor)
        ON CONFLICT (tenant_id, reviewer_id) DO NOTHING;
        v_inserted := FOUND;
    EXCEPTION WHEN check_violation OR not_null_violation THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'reviewer ID must be 1-256 bytes of printable text without leading or trailing spaces; nothing was changed';
    END;
    PERFORM public.record_identity_event('add', 'reviewer', p_tenant, p_reviewer, p_reviewer, p_actor, p_reason, p_request, p_attestation);
    RETURN v_inserted;
END
$$;

-- delete_reviewer soft-deletes a reviewer: their verdicts stay, and
-- require_active_reviewer refuses them new ones.
CREATE FUNCTION public.delete_reviewer(p_tenant text, p_reviewer text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
BEGIN
    SELECT status INTO v_status FROM public.reviewers
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'reviewer %L is not registered for tenant %L; nothing was changed. Register them with ''crosscodexd admin reviewer add''',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    IF v_status = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'reviewer %L of tenant %L is already deleted; nothing was changed. To bring them back run ''crosscodexd admin reviewer restore''',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    UPDATE public.reviewers SET status = 'deleted', deleted_at = now(), deleted_by = p_actor
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer;
    PERFORM public.record_identity_event('delete', 'reviewer', p_tenant, p_reviewer, p_reviewer, p_actor, p_reason, p_request, p_attestation);
END
$$;

-- restore_reviewer returns a deleted, unhidden reviewer to active (a
-- reviewer has no suspended state).
CREATE FUNCTION public.restore_reviewer(p_tenant text, p_reviewer text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
    v_hidden_at timestamptz;
    v_tenant_status text;
BEGIN
    SELECT status, hidden_at INTO v_status, v_hidden_at FROM public.reviewers
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'reviewer %L is not registered for tenant %L; nothing was changed. Register them with ''crosscodexd admin reviewer add''',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    -- FOR SHARE keeps delete_tenant from running until this commits.
    SELECT status INTO v_tenant_status FROM public.tenants WHERE tenant_id = p_tenant FOR SHARE;
    IF v_tenant_status = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is deleted; nothing was changed. To restore its reviewers, restore the tenant first with ''crosscodexd admin tenant restore''',
            left(p_tenant, 64));
    END IF;
    IF v_status <> 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'reviewer %L of tenant %L is not deleted (status %L); nothing was changed',
            left(p_reviewer, 64), left(p_tenant, 64), v_status);
    END IF;
    IF v_hidden_at IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'reviewer %L of tenant %L is hidden; nothing was changed. Run ''crosscodexd admin reviewer unhide'' first',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    UPDATE public.reviewers SET status = 'active', deleted_at = NULL, deleted_by = NULL
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer;
    PERFORM public.record_identity_event('restore', 'reviewer', p_tenant, p_reviewer, p_reviewer, p_actor, p_reason, p_request, p_attestation);
END
$$;

-- hide_reviewer shows a deleted reviewer only by its pseudonym and returns
-- it. The pseudonym is generated as for tenants, kept on unhide, and reused
-- on the next hide.
CREATE FUNCTION public.hide_reviewer(p_tenant text, p_reviewer text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
    v_hidden_at timestamptz;
    v_pseudonym text;
BEGIN
    SELECT status, hidden_at INTO v_status, v_hidden_at FROM public.reviewers
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'reviewer %L is not registered for tenant %L; nothing was changed. Register them with ''crosscodexd admin reviewer add''',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    IF v_status <> 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'only a deleted reviewer can be hidden; reviewer %L of tenant %L is %L. Delete them first with ''crosscodexd admin reviewer delete''',
            left(p_reviewer, 64), left(p_tenant, 64), v_status);
    END IF;
    IF v_hidden_at IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'reviewer %L of tenant %L is already hidden; nothing was changed', left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    UPDATE public.reviewers
    SET pseudonym = COALESCE(pseudonym, 'hidden-reviewer-' || left(encode(sha256(uuid_send(gen_random_uuid())), 'hex'), 16)),
        hidden_at = now()
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer
    RETURNING pseudonym INTO v_pseudonym;
    PERFORM public.record_identity_event('hide', 'reviewer', p_tenant, p_reviewer, p_reviewer, p_actor, p_reason, p_request, p_attestation);
    RETURN v_pseudonym;
END
$$;

-- unhide_reviewer shows a hidden reviewer by their ID again; they stay deleted.
CREATE FUNCTION public.unhide_reviewer(p_tenant text, p_reviewer text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_hidden_at timestamptz;
BEGIN
    SELECT hidden_at INTO v_hidden_at FROM public.reviewers
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'reviewer %L is not registered for tenant %L; nothing was changed. Register them with ''crosscodexd admin reviewer add''',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    IF v_hidden_at IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'reviewer %L of tenant %L is not hidden; nothing was changed', left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    UPDATE public.reviewers SET hidden_at = NULL WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer;
    PERFORM public.record_identity_event('unhide', 'reviewer', p_tenant, p_reviewer, p_reviewer, p_actor, p_reason, p_request, p_attestation);
END
$$;

-- list_reviewers lists a tenant's reviewers; a hidden reviewer is shown
-- only by its pseudonym.
CREATE FUNCTION public.list_reviewers(p_tenant text)
RETURNS TABLE (reviewer_id text, status text, created_at timestamptz, created_by text,
               deleted_at timestamptz, hidden boolean)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT CASE WHEN r.hidden_at IS NULL THEN r.reviewer_id ELSE r.pseudonym END,
           r.status, r.created_at, r.created_by, r.deleted_at, r.hidden_at IS NOT NULL
    FROM public.reviewers r
    WHERE r.tenant_id = p_tenant
    ORDER BY 1
$$;

-- trace_pseudonym maps a tenant or reviewer pseudonym back to the identity
-- and records the lookup, because revealing a hidden identity is itself an
-- audited action. An unknown pseudonym records nothing.
CREATE FUNCTION public.trace_pseudonym(p_pseudonym text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS TABLE (kind text, tenant_id text, identity_id text, status text, hidden boolean)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_kind text;
    v_tenant text;
    v_identity text;
    v_status text;
    v_hidden boolean;
BEGIN
    -- Pseudonyms are unique per table and the prefix names the table, so at
    -- most one row matches.
    SELECT m.kind, m.tenant_id, m.identity_id, m.status, m.hidden
    INTO v_kind, v_tenant, v_identity, v_status, v_hidden
    FROM (SELECT 'tenant'::text AS kind, t.tenant_id, t.tenant_id AS identity_id, t.status,
                 t.hidden_at IS NOT NULL AS hidden
          FROM public.tenants t WHERE t.pseudonym = p_pseudonym
          UNION ALL
          SELECT 'reviewer'::text, r.tenant_id, r.reviewer_id, r.status, r.hidden_at IS NOT NULL
          FROM public.reviewers r WHERE r.pseudonym = p_pseudonym) m;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'no tenant or reviewer has pseudonym %L; nothing was recorded. Check the pseudonym in the output that showed it',
            left(p_pseudonym, 64));
    END IF;
    -- p_subject is the pseudonym: a trace request names it, not the identity.
    PERFORM public.record_identity_event('trace', v_kind, v_tenant, v_identity, p_pseudonym, p_actor, p_reason, p_request, p_attestation);
    RETURN QUERY SELECT v_kind, v_tenant, v_identity, v_status, v_hidden;
END
$$;

-- export_identity_events is the one read path for the audit log: no role
-- holds any privilege on identity_events, so 'crosscodexd admin identity
-- export' reads it here before a database is decommissioned. It returns
-- every column, real IDs of hidden tenants and reviewers included, so only
-- tenant_admin (the platform admin) may run it. Pages are keyset-paginated
-- on event_id and bounded, so no call materializes the whole log.
CREATE FUNCTION public.export_identity_events(p_after bigint, p_limit int)
RETURNS TABLE (event_id bigint, occurred_at timestamptz, action text, kind text, tenant_id text,
               identity_id text, actor text, db_user text, reason text, request bytea,
               attestation bytea, attestation_sha256 text)
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_after IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'the starting event_id is NULL; nothing was read. Pass 0 to start at the first event, or the last event_id already read';
    END IF;
    IF p_limit IS NULL OR p_limit < 1 OR p_limit > 10000 THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'page size %s is out of range; nothing was read. Pass a page size from 1 to 10000',
            coalesce(p_limit::text, 'NULL'));
    END IF;
    RETURN QUERY
    SELECT e.event_id, e.occurred_at, e.action, e.kind, e.tenant_id, e.identity_id, e.actor,
           e.db_user, e.reason, e.request, e.attestation, e.attestation_sha256
    FROM public.identity_events e
    WHERE e.event_id > p_after
    ORDER BY e.event_id
    LIMIT p_limit;
END
$$;

REVOKE ALL ON FUNCTION public.export_identity_events(bigint, int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.export_identity_events(bigint, int) TO tenant_admin;

REVOKE ALL ON FUNCTION public.add_reviewer(text, text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.delete_reviewer(text, text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.restore_reviewer(text, text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.hide_reviewer(text, text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.unhide_reviewer(text, text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.list_reviewers(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.trace_pseudonym(text, text, text, bytea, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.add_reviewer(text, text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.delete_reviewer(text, text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.restore_reviewer(text, text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.hide_reviewer(text, text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.unhide_reviewer(text, text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.list_reviewers(text) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.trace_pseudonym(text, text, text, bytea, bytea) TO tenant_admin;

-- list_retention_tenants lists every tenant for 'crosscodexd admin retention
-- scan --all-tenants': a deleted tenant's job data still follows retention.
-- tenant_id is real, to scope the scan; display_id is the pseudonym of a
-- hidden tenant, else the tenant_id, and is the only column to print. The
-- caller already holds tenant_admin, which can run delete_tenant and
-- trace_pseudonym, so exposing real IDs here widens nothing.
CREATE FUNCTION public.list_retention_tenants()
RETURNS TABLE (tenant_id text, display_id text, status text)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT t.tenant_id,
           CASE WHEN t.hidden_at IS NULL THEN t.tenant_id ELSE t.pseudonym END,
           t.status
    FROM public.tenants t
    ORDER BY 1
$$;
REVOKE ALL ON FUNCTION public.list_retention_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.list_retention_tenants() TO tenant_admin;

-- ============================================================================
-- Restricted-role access
-- ============================================================================
-- The daemon runs as app_user (under row-level security), not as the schema
-- owner. These are the definer functions and grants that requires.

-- The daemon checks at startup that the schema is at the migration version
-- it was built for (pkg/db.CheckSchemaVersion). Migrations themselves run as
-- the owner (crosscodexd migrate).
GRANT SELECT ON schema_migrations TO app_user;

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
REVOKE ALL ON FUNCTION public.tenant_is_active(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.tenant_is_active(text) TO app_user;

-- enter_tenant scopes the transaction to p_id for RLS, refusing a deleted
-- tenant (fail closed). Every app_user and graph_user path uses it instead
-- of set_config('app.current_tenant', ...); retention keeps set_config
-- because job data of a deleted tenant still follows retention. An unknown
-- tenant is entered: RLS then matches zero rows. set_config is transaction-
-- local and is not reset on return, because only search_path is a SET clause.
--
-- The check takes no lock, like suspension: a transaction that entered
-- before delete_tenant committed may finish; every later one is refused.
CREATE FUNCTION public.enter_tenant(p_id text)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.tenants WHERE tenant_id = p_id AND status = 'deleted') THEN
        RAISE EXCEPTION USING ERRCODE = '42501', MESSAGE = format(
            'tenant %L is deleted; its data is kept but cannot be used. To use it again an operator runs ''crosscodexd admin tenant restore --tenant %s --by <operator> --reason <text>''',
            left(p_id, 64), left(p_id, 64));
    END IF;
    PERFORM set_config('app.current_tenant', p_id, true);
END
$$;
REVOKE ALL ON FUNCTION public.enter_tenant(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.enter_tenant(text) TO app_user, graph_user;

-- require_active_reviewer locks the current tenant's reviewer row FOR SHARE
-- until the caller's transaction ends, so delete_reviewer cannot run while a
-- verdict by that reviewer is being recorded. It is a definer function
-- because FOR SHARE needs UPDATE privilege, which app_user must not have.
--
-- The tenant comes from app.current_tenant, which app_user can set directly
-- with set_config, so this function trusts it exactly as RLS does: it
-- reveals no more than app_user can already SELECT from reviewers. Residual
-- risk: app_user can take FOR SHARE on another tenant's reviewer row and
-- delay that tenant's delete_reviewer until its own transaction ends. It
-- re-checks that the tenant is not deleted, because a direct set_config
-- skips enter_tenant's check. The reviewer messages stay terse: pkg callers
-- map the SQLSTATE (P0002 unregistered, 55000 deleted) to actionable errors.
CREATE FUNCTION public.require_active_reviewer(p_reviewer text)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tenant text := current_setting('app.current_tenant', true);
    v_status text;
BEGIN
    IF v_tenant IS NULL OR v_tenant = '' THEN
        RAISE EXCEPTION USING ERRCODE = '42501', MESSAGE =
            'require_active_reviewer needs a tenant transaction: call enter_tenant first';
    END IF;
    IF EXISTS (SELECT 1 FROM public.tenants t WHERE t.tenant_id = v_tenant AND t.status = 'deleted') THEN
        RAISE EXCEPTION USING ERRCODE = '42501', MESSAGE = format(
            'tenant %L is deleted; its data is kept but cannot be used. To use it again an operator runs ''crosscodexd admin tenant restore --tenant %s --by <operator> --reason <text>''',
            left(v_tenant, 64), left(v_tenant, 64));
    END IF;
    SELECT r.status INTO v_status FROM public.reviewers r
    WHERE r.tenant_id = v_tenant AND r.reviewer_id = p_reviewer FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = 'reviewer not registered';
    END IF;
    IF v_status = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = 'reviewer deleted';
    END IF;
END
$$;
REVOKE ALL ON FUNCTION public.require_active_reviewer(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.require_active_reviewer(text) TO app_user;

-- resumable_jobs lists every live tenant's running jobs. It is the one
-- sanctioned cross-tenant read: at startup crosscodexd resumes running jobs
-- before any tenant is entered, and as app_user RLS on jobs matches zero rows
-- outside a tenant. It returns only running jobs, and none of a deleted
-- tenant (fail closed: enter_tenant would refuse to resume them anyway).
CREATE FUNCTION public.resumable_jobs()
RETURNS TABLE (
    job_id        text,
    tenant_id     text,
    status        text,
    config        jsonb,
    created_by    text,
    created_at    timestamptz,
    updated_at    timestamptz,
    error_message text
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT j.job_id, j.tenant_id, j.status, j.config, j.created_by,
           j.created_at, j.updated_at, j.error_message
    FROM public.jobs j
    JOIN public.tenants t ON t.tenant_id = j.tenant_id
    WHERE j.status = 'running' AND t.status <> 'deleted'
    ORDER BY j.created_at
$$;
REVOKE ALL ON FUNCTION public.resumable_jobs() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.resumable_jobs() TO app_user;

-- ============================================================================
-- Apache AGE graph lifecycle
-- ============================================================================
-- Each tenant gets a dedicated named graph (crosscodex_{tenant_id}).
-- This is the graph layer's equivalent of RLS — isolation by namespace
-- rather than by row policy, because AGE manages its own internal tables
-- that RLS cannot reach. Tenants are never hard-deleted, so graphs are
-- created with their tenant and never dropped.
--
-- This section comes last because SET LOCAL search_path below lasts until
-- the end of the migration's transaction.

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
--
-- schema_name is typed `name`, not TEXT, on purpose. PostgreSQL truncates
-- identifiers to 63 bytes (NAMEDATALEN-1); for a tenant_id where
-- 'crosscodex_' || tenant_id exceeds 63 bytes the schema and its AGE label
-- tables are created under the truncated name. Declaring schema_name as
-- `name` applies the same truncation to the variable, so create_graph(), the
-- ALTER SCHEMA, and both object loops all reference the identical actual
-- schema name. With TEXT the loops compared pg_tables.schemaname (truncated)
-- against the untruncated variable, never matched, and left _ag_label_vertex
-- / _ag_label_edge owned by postgres — graph_user then hit "permission
-- denied for table _ag_label_vertex" on the first cypher call.
CREATE OR REPLACE FUNCTION public.create_tenant_graph()
RETURNS TRIGGER AS $$
DECLARE
    schema_name name := 'crosscodex_' || NEW.tenant_id;
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

GRANT EXECUTE ON FUNCTION public.tenant_graph_name() TO graph_user;
GRANT EXECUTE ON FUNCTION public.assert_tenant_graph(TEXT) TO graph_user;

GRANT USAGE ON SCHEMA ag_catalog TO graph_user;
GRANT SELECT ON ALL TABLES IN SCHEMA ag_catalog TO graph_user;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA ag_catalog TO graph_user;
