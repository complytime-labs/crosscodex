-- Reset search_path: migration 009 sets it to ag_catalog for AGE operations.
-- golang-migrate reuses the same connection, so a non-LOCAL SET leaks here.
SET LOCAL search_path = "$user", public;

-- Controls table: stores decomposed compliance controls with full-text search.
-- Tenant isolation via RLS (consistent with migration 007 pattern).
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

-- Grant to app_user (created in migration 007).
GRANT SELECT, INSERT, UPDATE, DELETE ON controls TO app_user;

-- Provenance columns on existing catalogs table (created in migration 003).
ALTER TABLE catalogs
    ADD COLUMN IF NOT EXISTS source_uri        TEXT,
    ADD COLUMN IF NOT EXISTS content_hash      TEXT,
    ADD COLUMN IF NOT EXISTS content_size      BIGINT,
    ADD COLUMN IF NOT EXISTS format            TEXT,
    ADD COLUMN IF NOT EXISTS output_hash       TEXT,
    ADD COLUMN IF NOT EXISTS extractor_name    TEXT,
    ADD COLUMN IF NOT EXISTS extractor_version TEXT;
