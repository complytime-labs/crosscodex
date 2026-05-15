CREATE TABLE IF NOT EXISTS classifications (
    catalog_id TEXT NOT NULL REFERENCES catalogs(catalog_id),
    control_id TEXT NOT NULL,
    type       TEXT NOT NULL,
    level      TEXT NOT NULL,
    tenant_id  TEXT NOT NULL REFERENCES tenants(tenant_id),
    PRIMARY KEY (catalog_id, control_id, type)
);
