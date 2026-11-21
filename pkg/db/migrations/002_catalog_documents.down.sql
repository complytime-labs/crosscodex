-- Reverses 002_catalog_documents.up.sql. Version lineage is lost; the catalogs
-- rows, their controls and every other dependent are untouched.

DROP TABLE IF EXISTS catalog_documents;

ALTER TABLE catalogs
    DROP CONSTRAINT IF EXISTS catalogs_supersedes_tenant_fkey,
    DROP CONSTRAINT IF EXISTS catalogs_link_method_check,
    DROP COLUMN IF EXISTS link_method,
    DROP COLUMN IF EXISTS supersedes_catalog_id,
    DROP COLUMN IF EXISTS version_label,
    DROP COLUMN IF EXISTS document_slug;
