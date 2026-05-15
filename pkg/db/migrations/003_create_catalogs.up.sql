CREATE TABLE IF NOT EXISTS catalogs (
    catalog_id  TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(tenant_id),
    name        TEXT NOT NULL,
    version     TEXT NOT NULL,
    source_type TEXT NOT NULL,
    object_path TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
