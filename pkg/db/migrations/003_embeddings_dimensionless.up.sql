-- Make the embeddings vector column model-agnostic (dimensionless).
--
-- The ivfflat index requires a fixed column dimension; a dimensionless column
-- cannot carry one, so it is dropped first. Similarity queries fall back to an
-- exact scan, which is correct at current scale. Every <=> query in the
-- codebase is model-scoped (single width per query), so dropping the fixed
-- typmod cannot cause a cross-dimension comparison. When scale demands ANN,
-- add per-model partial expression indexes per deployment — see
-- docs/dev/embeddings.md and docs/dev/adr/0001-model-agnostic-embedding-storage.md.
DROP INDEX IF EXISTS idx_embeddings_vector;

ALTER TABLE embeddings
    ALTER COLUMN vector TYPE vector USING vector::vector;
