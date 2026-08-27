# Embedding Storage

This document covers the model-agnostic embedding storage schema, the procedure for switching or adding embedding models, and the at-scale indexing strategy for production deployments.

## Model-Agnostic Column

The `embeddings.vector` column is dimensionless (no fixed width). Any embedding model's vectors are stored without schema modification. Rows are keyed by `(catalog_id, control_id, model)`, allowing multiple models to coexist in the same database.

Every similarity query is model-scoped via `WHERE model = $model`, so mixed vector widths never collide in query results:

- `FindSimilar` filters by model before computing cosine distance
- `SimilarityMatrix` joins on `a.model = b.model`

This design decouples schema evolution from model selection. See [ADR 0001](adr/0001-model-agnostic-embedding-storage.md) for the decision context and tradeoffs.

The initial schema (`001_initial_schema`) creates `embeddings.vector` as a dimensionless `vector` column with no ivfflat index, so a fresh install stores any model's vectors directly with no further migration. A fixed-width column such as `vector(2000)` was rejected precisely because it would reject vectors of any other dimension.

Migrations run automatically via `Up()` (embedded `*.sql` files are auto-discovered through `//go:embed`). See [Database Migrations](migrations.md) for migration authoring and runtime behavior.

## Switching or Adding an Embedding Model

To switch to a different embedding model or add a second model alongside the current one:

1. Add the new model name to the `analysis.embedding.models` list in your config file:

   ```yaml
   analysis:
     embedding:
       models:
         - "snowflake-arctic-embed2"
         - "granite-embedding:30m"  # Add new model here
   ```

2. Point `analysis.candidates.embed_model` at the model you want candidate generation to use (it must be a member of `analysis.embedding.models`):

   ```yaml
   candidates:
     embed_model: "granite-embedding:30m"
   ```

3. Re-run analysis. The embedding stage writes new rows with `model = "granite-embedding:30m"`. Old `snowflake-arctic-embed2` rows coexist in the same table.

No schema migration is required. The dimensionless column accepts any vector width. Note: the shared `"generic"` bucket (used by the graph Index path) holds one vector width per deployment, so switching a model that uses the generic bucket without renaming would mix widths under one model key.

## Removing an Old Model's Embeddings

To delete all embeddings for a specific model after switching:

```go
err := vectorStore.DeleteByModel(ctx, tenantID, catalogID, "snowflake-arctic-embed2")
```

This removes all rows matching the tenant, catalog, and model. See `pkg/vectordb/pgvector.go` for the implementation.

## Upgrade Cycle

Switching or adding an embedding model requires no schema migration: edit the config, re-run analysis, and old and new model rows coexist in the same table. The dimensionless `vector` column ships in the initial schema, so a fresh install accepts any model's vectors without further migration. The migration system is covered in [Database Migrations](migrations.md).

## At-Scale Indexing

The dimensionless column uses exact scans (no ANN index) because pgvector's ivfflat and hnsw indexes require a fixed dimension at index creation time.

For small datasets (scoped per tenant/catalog/model), exact scans are acceptable. At scale, create one partial expression index per model actually in use:

```sql
-- Example: per-model ivfflat index for granite-embedding:30m (384 dimensions)
CREATE INDEX idx_embeddings_granite384
    ON embeddings USING ivfflat ((vector::vector(384)) vector_cosine_ops)
    WITH (lists = 100)
    WHERE model = 'granite-embedding:30m';

-- Example: per-model ivfflat index for nomic-embed-text-v1.5 (768 dimensions)
CREATE INDEX idx_embeddings_nomic768
    ON embeddings USING ivfflat ((vector::vector(768)) vector_cosine_ops)
    WITH (lists = 100)
    WHERE model = 'nomic-embed-text-v1.5';
```

Each index covers only the rows for that model. The query planner uses the matching index when `WHERE model = 'granite-embedding:30m'` appears in the query.

Add these indexes via a new migration when deploying to production with a large embedding dataset. See [Database Migrations](migrations.md) for migration authoring.
