-- Catalog document versions (#204).
--
-- A catalog_documents row names a document (the slug) a tenant tracks across
-- versions and points at its current version. Each version is an ordinary
-- catalogs row; the new catalogs columns record which document it belongs to,
-- its label, the version it supersedes and how that link was established.
-- Every new catalogs column is nullable: existing catalogs stay valid as
-- "unversioned" and nothing is backfilled.
--
-- Every reference is composite on (catalog_id, tenant_id). Foreign-key checks
-- ignore row-level security and catalog_id is unique across all tenants, so a
-- single-column key would let one tenant link to another tenant's catalog (see
-- the catalogs comment in 001). ON DELETE is NO ACTION: retiring a version
-- re-points its successor and the document before deleting the catalog.

ALTER TABLE catalogs
    ADD COLUMN document_slug         TEXT,
    ADD COLUMN version_label         TEXT,
    ADD COLUMN supersedes_catalog_id TEXT,
    ADD COLUMN link_method           TEXT,
    ADD CONSTRAINT catalogs_link_method_check
        CHECK (link_method IN ('declared', 'metadata', 'name')),
    ADD CONSTRAINT catalogs_supersedes_tenant_fkey
        FOREIGN KEY (supersedes_catalog_id, tenant_id) REFERENCES public.catalogs (catalog_id, tenant_id);

CREATE TABLE IF NOT EXISTS catalog_documents (
    tenant_id          TEXT NOT NULL REFERENCES tenants(tenant_id),
    document_slug      TEXT NOT NULL,
    current_catalog_id TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, document_slug),
    -- Mirrors the slug regex the import path validates. Postgres ARE's $
    -- anchors at the true end of the string, so a trailing newline fails.
    CONSTRAINT catalog_documents_slug_check CHECK (document_slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT catalog_documents_current_tenant_fkey
        FOREIGN KEY (current_catalog_id, tenant_id) REFERENCES public.catalogs (catalog_id, tenant_id)
);

ALTER TABLE catalog_documents ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON catalog_documents
    USING (tenant_id = current_setting('app.current_tenant', true));

-- The application runs as the restricted app_user so that RLS binds it: it
-- owns no table, so it cannot disable RLS, truncate, alter or add triggers.
-- GRANTs stay at the four DML privileges, never TRUNCATE, ALTER or TRIGGER.
GRANT SELECT, INSERT, UPDATE, DELETE ON catalog_documents TO app_user;

-- purge_user (retention purge and catalog retire) takes a catalog out of its
-- document's version chain before deleting it: it reads catalog_documents to
-- refuse a document's current version, and re-points the successor's
-- supersedes_catalog_id to the deleted version's predecessor (see
-- unlinkCatalogVersion in pkg/retention/purge.go). It gets exactly that: no
-- write on catalog_documents, and UPDATE on that one column of catalogs.
GRANT SELECT ON catalog_documents TO purge_user;
GRANT UPDATE (supersedes_catalog_id) ON catalogs TO purge_user;
