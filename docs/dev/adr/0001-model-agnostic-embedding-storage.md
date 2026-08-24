---
status: accepted
date: 2026-08-23
deciders: crosscodex team
---

# Model-agnostic embedding storage

## Context and Problem Statement

The `embeddings.vector` column was initially created as `vector(2000)` — a fixed width that matched no real embedding model in use. Real models produce embeddings of varying dimensions: granite-embedding:30m (384), nomic-embed-text-v1.5 (768), mxbai-embed-large-v1 (1024), and OpenAI's text-embedding-3-small/large (1536/3072). Real ingestion failed with dimension mismatch errors (`expected 2000 dimensions, not 384`), exposing a gap: no test crossed the real-model ingestion path to the real storage layer.

The fixed-width schema also creates brittleness: switching to a different embedding model would require a schema migration and re-embedding all existing data, blocking multi-model coexistence and tying schema evolution to model selection.

## Decision Drivers

- **Multi-model support** — multiple embedding models must coexist in the same database
- **Schema stability** — switching models should not require schema migrations
- **Real-world correctness** — actual ingestion with real models must work without dimension mismatches
- **Performance at scale** — similarity search performance must remain acceptable as data grows

## Considered Options

- **Option A: Keep a fixed width (any constant)**
- **Option B: Configured per-deployment width, re-embed on model switch**
- **Option C: Dimensionless `vector` column**

## Decision Outcome

Chosen option: **Option C (dimensionless `vector` column)**, because it enables model switching without schema changes, allows multiple models to coexist via the `(catalog_id, control_id, model)` primary key, and eliminates dimension mismatch failures in the real ingestion path.

### Consequences

- **Positive:**
  - Model switching requires no schema migration
  - Multiple embedding models coexist in the same database
  - Real ingestion with actual models works correctly
  - Schema is decoupled from model selection

- **Negative:**
  - No ANN index support for dimensionless columns, so `FindSimilar` and `SearchControls` perform exact scans using the `<=>` operator
  - Acceptable now (queries are scoped per tenant/catalog/model, yielding small result sets), but at scale may require per-model partial expression indexes:
    ```sql
    CREATE INDEX idx_embeddings_granite384
        ON embeddings USING ivfflat ((vector::vector(384)) vector_cosine_ops)
        WITH (lists = 100)
        WHERE model = 'granite-embedding:30m';
    ```
  - Migration `003` rollback (`DOWN`) restores `vector(2000)` with ivfflat and will fail unless all rows are exactly 2000 dimensions

- **Correctness note:** Every similarity query is model-scoped (`FindSimilar` uses `WHERE model = $4`; `SimilarityMatrix` uses `a.model = b.model`), so mixed vector widths never collide in query results.

## Pros and Cons of the Options

### Option A: Keep a fixed width (any constant)

- Good, because ANN indexes (ivfflat, hnsw) work out of the box
- Good, because query planning is simpler with a known dimension
- Bad, because it re-breaks on every model change (dimension mismatch errors)
- Bad, because it blocks multi-model coexistence
- Bad, because schema migrations are required to switch models

### Option B: Configured per-deployment width, re-embed on model switch

- Good, because ANN indexes work for the configured width
- Good, because it's flexible across deployments
- Bad, because it reintroduces brittleness one layer up (config instead of schema)
- Bad, because switching models still requires re-embedding all data
- Bad, because it blocks multi-model coexistence within a deployment

### Option C: Dimensionless `vector` column

- Good, because any embedding dimension is supported without schema changes
- Good, because multiple models coexist via the composite primary key
- Good, because real ingestion with any model works without configuration
- Good, because schema is decoupled from model selection
- Bad, because exact scans are required (no ANN index on dimensionless columns)
- Bad, because at-scale deployments need per-model expression indexes

## More Information

- Implementation: migration `003` removes the dimension constraint and drops the ivfflat index
- Documentation: [../embeddings.md](../embeddings.md) documents the per-model indexing strategy for production deployments
- Related: issue #18 tracks the embedding storage defect and multi-model support
