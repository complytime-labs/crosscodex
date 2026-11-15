# Embedding Storage

This document covers the model-agnostic embedding storage schema, the procedure for switching or adding embedding models, and the at-scale indexing strategy for production deployments.

## Model-Agnostic Column

The `embeddings.vector` column is dimensionless (no fixed width). Any embedding model's vectors are stored without schema modification. Rows are keyed by `(tenant_id, catalog_id, control_id, model)`, allowing multiple models to coexist in the same database and each tenant to own its embedding of the same control. An embedding may reference only a catalog of its own tenant: the foreign key to `catalogs` is `(catalog_id, tenant_id)`, because foreign keys ignore RLS and catalog IDs are global. `StoreEmbedding` and `StoreBatch` report a violation as `catalog "<id>" is not a catalog of tenant "<tenant>"`.

Every similarity query is model-scoped via `WHERE model = $model`, so mixed vector widths never collide in query results:

- `FindSimilar` filters by model before computing cosine distance
- `SimilarityMatrix` joins on `a.model = b.model`

This design decouples schema evolution from model selection. See [ADR 0001](adr/0001-model-agnostic-embedding-storage.md) for the decision context and tradeoffs.

The initial schema (`001_initial_schema`) creates `embeddings.vector` as a dimensionless `vector` column with no ivfflat index, so a fresh install stores any model's vectors directly with no further migration. A fixed-width column such as `vector(2000)` was rejected precisely because it would reject vectors of any other dimension.

Migrations are applied by `crosscodexd migrate`, never by the daemon (embedded `*.sql` files are auto-discovered through `//go:embed`). See [Database Migrations](migrations.md) for migration authoring and runtime behavior.

## Similarity Search

`GraphService.SimilaritySearch` (and the gateway's `FindSimilar`, which forwards `control_id`, `limit` and `catalog_ids`) reads rows of the `embeddings` table of the calling tenant. Each search runs under exactly one model, over the catalogs it names. There is no default catalog or model: nothing is guessed, and a search that cannot be resolved fails.

Two query forms (`internal/graph/read.go`):

| | `control_id` | `query_embedding` |
| --- | --- | --- |
| Query vector | the control's stored embedding, found through its `controls` row (`control_id` is the graph node ID, `<catalog_id>/<identifier>`) | the vector in the request |
| `catalog_ids` | optional; empty means the control's own catalog | required |
| `model` | optional; empty means the control's only stored model | required |
| Rows fetched per catalog | `limit + 1`, so dropping the control itself still leaves `limit` | `limit` |
| The control itself | left out of the results (matched on catalog and control ID) | not applicable |

`limit` defaults to 10 when it is 0 or negative. Matches from all catalogs are merged by descending similarity (ties by catalog, then control ID) and cut to `limit`. Each match's node ID is the stored `control_id`, and its properties carry `catalog_id`.

A request fails as a whole; it never returns a partial result.

- `catalog_ids` entries must be non-empty. Duplicates are dropped. More distinct catalogs than `graph.max_similarity_catalogs` (default 100, range 1-10000, global only) is `InvalidArgument`, because a search costs one vector query per catalog.
- `limit` (default 10) above `graph.max_similarity_limit` (default 1000, range 1-10000, global only) is `InvalidArgument` naming the maximum, because each catalog's rows are held in memory. The limit applies per catalog.
- `query_embedding` must be non-empty, finite and not all zeros (cosine similarity is undefined), and needs `catalog_ids` and `model`.
- A control that does not exist, or has no stored embedding, is `NotFound`. A control with embeddings under several models and no `model` is `InvalidArgument` listing them; `FindSimilar` cannot name a model, so such a control can only be searched through `SimilaritySearch`. A `model` the control has no embedding under is `NotFound` listing the stored ones.
- A catalog with no embeddings under the model is `NotFound` (`catalog "<c>" has no embeddings under model "<m>"`). The message is the same for a missing catalog and another tenant's, so it reveals nothing across tenants.

### Width check

Because the column is dimensionless, `PgVectorStore.FindSimilar` compares the query's width with the stored vectors before ranking them, instead of letting pgvector fail with "different vector dimensions":

- query width differs from the stored width: `InvalidArgument`, `query has N dimensions; catalog "<c>" stores M under model "<m>"`. Re-embed the query with the model named in the request.
- the catalog holds vectors of different widths under that model (a model re-served at a new width): an internal error naming the widths. The data is inconsistent, not the request; re-embed the catalog.

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
