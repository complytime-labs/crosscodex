# Design Principles

Rationale behind the rules condensed in the root `AGENTS.md`. Read this when changing an isolation boundary, a safety mechanism, the graph schema, or the relationship taxonomy.

## Graph Data Model

CrossCodex is a property-graph-first system. The compliance knowledge graph is the central data representation — every analyzer builds toward it, every query consumes from it, and every compliance report is derived from it. Understanding the graph model is a prerequisite for working on any service.

**Property graph semantics:**

CrossCodex uses a labeled property graph where nodes represent entities (controls, catalogs, artifacts, artifact types), edges represent relationships between them (REQUIRES, SEMANTIC_MATCH, DEMANDS, IS_TYPE, PARENT_OF), and key-value properties carry data about those entities and relationships (confidence scores, classification labels, temporal validity windows). Structure and data are distinct concerns:

- **Structure** is expressed by the graph topology itself — which nodes exist, which edges connect them, and in what direction. The graph engine (currently Apache AGE) manages this natively.
- **Data** is expressed by properties on nodes and edges — metadata that describes the entity or relationship, not the connection itself.

Never conflate the two. Edge endpoints are structure. Confidence scores are data. Tenant isolation is a partition key. See "Never Store Structural Topology as Data Properties" under Defensive Design Principles for the enforcement rules.

**What lives where:**

| Store | What | Why |
|-------|------|-----|
| **Property graph** (AGE) | Controls, relationships, artifacts, artifact types, compliance topology | Traversal queries, path finding, transitive closure, compliance mapping visualization |
| **Relational tables** (PostgreSQL) | Jobs, vote summaries, catalog metadata, tenant records, migrations | ACID transactions, aggregation, batch updates, operational state |
| **Object storage** (local FS / S3) | OSCAL documents, analysis results, attestations, prompt specs, CSV exports | Immutable artifacts, large blobs, reconstruction source |
| **Vector store** (pgvector) | Control embeddings, similarity matrices | Approximate nearest neighbor search, candidate pair selection |
| **Event stream** (NATS JetStream) | Audit events, work dispatch, stage notifications | Async communication, provenance, decoupled services |

The relational database and object storage are authoritative. The graph is a **materialized view** — it can be destroyed and rebuilt from the authoritative stores without data loss. Analyzers produce facts (stored as analysis results in object storage and vote summaries in relational tables), and materializers project those facts into the graph as nodes and edges. This means:

- Graph writes are idempotent. Re-materializing the same facts produces the same graph.
- Graph loss is recoverable. Re-running materializers against stored results reconstructs the graph.
- Graph queries are read-heavy. Services query the graph for compliance topology; they do not treat it as the source of truth for raw analysis data.

**Graph partitioning:**

Each tenant gets its own graph instance named `crosscodex_{tenant_id}`. Tenant isolation at the graph level is structural — queries execute within a single graph, and cross-tenant traversal is impossible by construction. This is enforced by `pkg/graphdb/agedriver`, which prepends the tenant-scoped graph name to every Cypher query.

**The relationship-type taxonomy is a starting point, not a closed standard:** `pkg/config/validate.go`'s `validRelationshipTypes` and `internal/analyzer/relationship/types.go` currently enumerate the 8 NIST IR 8477 relationship types (EQUIVALENT, SUPERSET_OF, SUBSET_OF, CONTRIBUTES_TO, COMPLEMENTS, PARTIAL, CONFLICTS_WITH, NO_RELATIONSHIP). These are CrossCodex's current set, not an immutable external constraint — expand the enum when a real relationship concept doesn't cleanly fit the existing 8 values. Weigh that against keeping the new concept outside the enum entirely (see `internal/synthesis.ConsensusRequires`, which treats the `requires` analyzer's consensus label as a distinct, always-actionable signal rather than folding it into this taxonomy) based on whether it's genuinely a peer of the existing relationship types or a structurally different kind of signal.

The long-term direction is broader than NIST IR 8477: CrossCodex is expected to eventually support the relationship and attribute surface of **ReqIF** (Requirements Interchange Format) and **SysML v2 Requirements** structures, not just compliance-control relationships. Don't design the relationship/attribute model as if the current 8-value enum or today's control-oriented properties are the final shape — keep node/edge property schemas (see the table above) and the relationship-type enum easy to extend rather than hard-coding assumptions that only fit compliance catalogs.

## Defensive Design Principles

These principles apply to all packages, not just database code. They reflect hard-won decisions about how CrossCodex handles failure, isolation, and human error.

### Fail Closed, Not Open

When a safety mechanism is absent or misconfigured, the system must deny access rather than grant it. Examples:
- If `app.current_tenant` is not set, RLS policies match zero rows (not all rows).
- If a graph name can't be derived, the query fails with an error (not a fallback graph).
- If an extension is missing at startup, the service refuses to start with a clear error.

Design every isolation boundary so that the default state — before any configuration — is "deny all."

### Errors Must Be Actionable

Every error a human can trigger must tell them: what happened, why it was blocked, and what to do instead. This applies to database triggers, API responses, CLI output, and log messages. A tired operator at 2am should be able to read the error and know their next step without consulting documentation.

Bad: `ERROR: permission denied`
Good: `ERROR: cannot modify job abc123: status is "completed". To retry, create a new job instead of resetting this one.`

### Verify the Negative Path

Every safety mechanism needs a test that proves it works by attempting the forbidden operation and asserting three things:
1. The operation returned an error (not silent success).
2. The error message is actionable (contains enough context to diagnose).
3. The data is unchanged after the failed operation (the error wasn't raised but swallowed).

Skipping step 3 is how silent data corruption ships.

### Threat Model: Tired Admin at 2am

Design guardrails assuming the adversary is not a malicious attacker but a well-intentioned operator making reasonable-seeming manual interventions under pressure. Test for:
- Bulk operations that hit a mix of protected and unprotected rows (must fail entirely, not partially).
- Attempts to disable safety mechanisms (triggers, RLS) via direct SQL.
- Privilege escalation through operations the application role shouldn't have (TRUNCATE, ALTER, COPY).
- Manual "fixes" like resetting a completed job's status or reassigning rows between tenants.

The system should make the wrong thing hard and the right thing obvious.

### Privilege Separation

Application code runs under a restricted database role, not the table owner. The table owner (superuser) bypasses RLS and triggers. This is PostgreSQL's design, not a bug — but it means:
- Tests must use the restricted role to verify isolation, not the superuser.
- GRANTs must be explicit and minimal (SELECT, INSERT, UPDATE, DELETE — not TRUNCATE, not ALTER, not TRIGGER).
- Comment in the codebase WHY the restricted role exists, because the next person will be tempted to simplify by using the superuser for everything.

### No Partial Success on Safety-Critical Operations

When a batch operation (UPDATE, DELETE) touches rows with mixed protection states (some completed, some not), the entire statement must fail — not silently succeed for the unprotected rows. PostgreSQL's per-row BEFORE triggers provide this guarantee, but tests must verify it explicitly because it's the kind of behavior that breaks silently if someone refactors triggers into rules or policies.

### Prefer Discovery Over Hard Coding

When practical, code should discover what to load and how to load it instead of hard coding it. If a value is likely to vary across deployments, document types, or user preferences, expose it as configuration with a sensible default — do not bury it in source code where only a developer can change it.

This applies to thresholds, keyword lists, regex patterns, format allowlists, chunk sizes, retry counts, and any other tuning parameter. The test: would a user deploying CrossCodex against a different compliance framework need to change this value? If yes, it must be configuration, not code.

### Configuration Surface Discipline

Every runtime-configurable knob in a package MUST have a corresponding config field in `pkg/config` OR a documented wiring contract explaining how it gets its value. No silent knobs — if a functional option exists but has no config field, the gap must be caught during review.

Rules:
- **No orphan options:** If a package exposes `WithX(val)`, there must be a config field that feeds it, or a doc comment on the option explaining the wiring contract (e.g., FIPS mode derived from `tls.fips.enabled`).
- **Naming fidelity:** Config field names must match the semantic meaning of the value they configure. If the package expects a public key PEM file, the config field is `public_key_path`, not `cert_path`.
- **Per-tenant completeness:** If a config section supports `tenant_overrides`, every field that could reasonably differ between tenants must be overridable. If a field is intentionally global-only, document why.
- **Deduplication:** Do not create a second config knob for a value that already has a canonical source. Example: FIPS mode is `tls.fips.enabled` — attestation, storage, and future packages derive it from there rather than adding their own `fips_mode` field.
- **Validation coverage:** Per-tenant overrides must undergo the same validation rules as their global counterparts (e.g., key path pairing, positive durations).
- **Test coverage:** Every config field must have tests covering: default value, valid override, and invalid value rejection with actionable error message.

### Upsert Over Reject When Data Is Sound

When receiving valid data that conflicts with an existing record by identity (same ID, same tenant), prefer upsert (insert-or-update) over rejection. Failing with "already exists" forces the caller to implement delete-then-insert or check-then-branch logic that is both fragile and race-prone. If the incoming data passes validation, the caller's intent is clear: this is the current truth, store it.

This applies to controls, catalogs, embeddings, graph nodes, configuration records, and any other entity where re-import or incremental update is a reasonable operation. Reserve "already exists" errors for cases where duplication genuinely indicates a bug (e.g., two controls with the same derived ID within a single import batch).

### Graph Backend Portability

The graph database (currently Apache AGE) is accessed exclusively through the `pkg/graphdb.GraphDB` interface. No package outside `pkg/graphdb` may import AGE-specific types, use AGE SQL functions directly, or assume the graph shares a PostgreSQL transaction with relational queries. The graph is a materialized view — reconstructible from authoritative stores (see "Graph Data Model" above). Design every graph consumer so that swapping AGE for Neo4j, Neptune, or any openCypher/Gremlin backend requires changes only inside `pkg/graphdb/agedriver` (or a new sibling sub-package).

### Never Store Structural Topology as Data Properties

In a property graph, edges ARE the structural connection between nodes. The endpoints of an edge (which nodes it connects) are topology, not data. Never duplicate this structural information as key-value properties on the edge.

**Anti-pattern (do not do this):**
```go
// WRONG: source/target stored as data properties on the edge
edge := graphdb.Edge{
    Label:      "REQUIRES",
    Properties: map[string]any{"source": "AC-1", "target": "AC-2"},
}
client.CreateEdge(ctx, tenant, edge)
```

**Correct pattern:**
```go
// RIGHT: source/target are parameters that define which nodes the edge connects
edge := graphdb.Edge{
    Label:      "REQUIRES",
    Properties: map[string]any{"confidence": 0.95},
}
client.CreateEdge(ctx, tenant, "AC-1", "AC-2", edge)
```

This principle extends to any value that is already expressed by the graph structure:
- **Edge endpoints** — encoded structurally by `(source)-[edge]->(target)`, never as edge properties
- **Tenant ID on edges** — already the graph partition key (graph name `crosscodex_{tenant_id}`), never redundant as an edge property
- **Node identity** — the `id` property on a vertex is its lookup key, not duplicated elsewhere

On the read side, edge endpoint identity comes from the `Relationship` struct's `Source` and `Target` `Node` fields (populated by the MATCH pattern's `s` and `t` columns), not from edge properties.
