-- Rollback restores the fixed-width contract and the ivfflat index.
-- This FAILS if any stored embedding is not exactly 2000 dimensions —
-- inherent to narrowing the type. Operators must purge non-2000-dim rows
-- (e.g. PgVectorStore.DeleteByModel per non-2000-dim model) before rolling back.
ALTER TABLE embeddings
    ALTER COLUMN vector TYPE vector(2000) USING vector::vector(2000);

CREATE INDEX IF NOT EXISTS idx_embeddings_vector
    ON embeddings USING ivfflat (vector vector_cosine_ops) WITH (lists = 100);
