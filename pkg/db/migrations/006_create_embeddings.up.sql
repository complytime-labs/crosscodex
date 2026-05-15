-- Dimension set to 2000 (pgvector ivfflat index maximum).
-- Covers common models: OpenAI ada-002 (1536), most open-source models.
-- If a future model requires >2000 dimensions, add a migration to alter
-- the column and switch to hnsw or a newer pgvector version.
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
