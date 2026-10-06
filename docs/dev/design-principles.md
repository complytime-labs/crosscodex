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
- Graph loss is recoverable. Re-importing the tenant's catalogs rebuilds the graph: import writes the Control nodes and queues a full analysis, whose results the graph service writes back as edges and Artifact nodes (see the upgrade note below). Replaying stored results without re-running analysis is not wired yet: the analyzer `GraphMaterializer`s that read them back have no production caller (see "Graph writers" below).
- Graph queries are read-heavy. Services query the graph for compliance topology; they do not treat it as the source of truth for raw analysis data.

**Graph writers:**

`internal/graph.Service` is the live writer of analysis facts. Its NATS subscriber hands each analyzer result event to `materialize` in `internal/graph/subscriber_handlers.go`, which writes the nodes and edges for that analyzer (SEMANTIC_MATCH, REQUIRES, DEMANDS and IS_TYPE edges; Artifact and ArtifactType nodes). Catalog ingest (`internal/catalog/service.go`) writes Control nodes and PARENT_OF edges. The `GraphMaterializer` types in `internal/analyzer/artifacts` and `internal/analyzer/relationship` write a job's nodes and edges from its results in object storage, but nothing in production calls them yet. They use their own IDs (`<control>__art_<hash>` artifacts, plain lower-case ArtifactType IDs, and the `semantic-match-rebuild` edge kind), so if one is wired into a graph that `internal/graph` also writes, the two produce separate nodes and edges for the same fact instead of one silently keeping the other's payload. The artifacts `GraphMaterializer` never replaces a stored Artifact: its artifact IDs hash only the normalized name and type, so a later job whose artifact gets the same ID with any different content (wording, confidence) is rejected, and the graph has no per-node delete to remove the stored one; keeping that job's version instead means materializing it before the conflicting job into a rebuilt tenant graph (drop and recreate it, then re-import the catalogs so the Control nodes exist; see the upgrade note below), not just re-running this materializer. Unifying the two writers is open work.

**Graph partitioning:**

Each tenant gets its own graph instance named `crosscodex_{tenant_id}`. Tenant isolation at the graph level is structural — queries execute within a single graph, and cross-tenant traversal is impossible by construction. This is enforced by each driver: `pkg/graphdb/agedriver` prepends the tenant-scoped graph name to every Cypher query, and `pkg/graphdb/memdriver` keeps one in-memory graph per tenant.

**Driver contract and temporal encoding:**

Every `graphdb.GraphDB` driver honors the contract documented on the interface and enforced by `internal/testspecs.GraphDBContractBehavior` (run against `memdriver` in unit tests and `agedriver` in integration tests). Edges require both endpoints to exist exactly once, node IDs are unique across labels within a tenant's graph (a second label returns ErrNodeIDConflict), non-empty edge IDs are unique (`ErrEdgeExists`), every derived edge ID (REQUIRES, semantic-match, semantic-match-rebuild, demands, is-type, parent-of) is built with `graphdb.DerivedID` and a leading kind tag (for example `graphdb.DerivedID("requires", job, source, target)`), and `BulkCreateEdges` is all-or-nothing. Together these make re-materialization idempotent.

**How agedriver serializes writes.** AGE has no unique constraints on property values, so agedriver enforces node-ID uniqueness and idempotent graph creation with transaction-scoped PostgreSQL advisory locks (`pg_advisory_xact_lock`, which the restricted `graph_user` may call without a grant; PostgreSQL releases the lock at commit or rollback). `CreateNode` and `UpsertNode` lock `advisoryKey("node", graph, id)`, so writers for one ID wait for each other whatever their labels; `CreateGraph` locks `advisoryKey("graph", graph)`, a separate key domain. The locks work only because `beginTx` pins every transaction to READ COMMITTED: the existence check that runs after a wait takes a fresh snapshot and sees what the previous holder committed, which a REPEATABLE READ snapshot taken before the wait would miss. Edge writes take no lock, so concurrent writers can still create two edges with one non-empty ID. The contract clause is on `graphdb.GraphDB` (`pkg/graphdb/interface.go`); the mechanism is `lockNode`, `CreateGraph` and `beginTx` in `pkg/graphdb/agedriver/client.go`.

A derived node or edge ID that joins more than one variable part goes through `graphdb.DerivedID` unless every part but one is drawn from a fixed alphabet that cannot contain the separator; then the separator found from that side splits the ID back into its parts, so distinct inputs cannot collide. Current examples of that exception: artifact node IDs `<control>__art_<n>` (`internal/graph/subscriber_handlers.go`, decimal index) and `<control>__art_<hash>` (`internal/analyzer/artifacts/materializer.go`, fixed-width hex), and catalog Control node IDs `<catalogID>/<itemID>` (`internal/catalog/service.go`, where `catalogID` is 16 hex characters). `graphdb.DerivedID` percent-escapes `%` and `_` in each part and joins the parts with `_`. Control IDs come from tenant catalogs and can contain `_`, so plain joining would let distinct pairs share an ID and the second edge would be dropped as a duplicate. Escaping keeps `_` a pure separator, so distinct non-empty part lists always get distinct IDs (the empty list and `[""]` both give `""`, so callers always pass a kind tag first); parts without `_` or `%` read unchanged (`requires_job-9_ac-2_ac-1`).

agedriver's isolation depends on caller text never reaching SQL. One database role owns every tenant graph, so nothing below the driver stops a statement from reading another graph. Each Cypher query is embedded in SQL inside a `$cypher$ ... $cypher$` dollar-quote, and three rules keep caller text where it belongs. Labels and property keys are spliced into Cypher unquoted, so every driver rejects any that fail `graphdb.CheckIdentifier` (`[A-Za-z_][A-Za-z0-9_]*`). Values are escaped and have the tag stripped repeatedly, because one pass can rebuild it from its halves. `ExecuteQuery` rejects caller-written Cypher that contains the tag instead of rewriting it. The tenant is spliced into the SQL itself, outside the dollar-quote, as the graph name `'crosscodex_<tenant>'`, so every driver rejects a tenant that fails `graphdb.CheckTenant` (`tenant.ValidateTenantID`: 3-52 lowercase letters, digits and hyphens) instead of trusting callers to have validated it. The 52-character cap is 63 minus `len("crosscodex_")`: PostgreSQL truncates identifiers to 63 bytes, so without it two tenant IDs sharing a 52-character prefix would map to the same graph. Separately, written `Properties` may not use a key the driver writes itself from a `Node` or `Edge` field (`id`, `valid_from`, `valid_to`, ...; `confidence` on edges only, because nodes carry a caller-set `confidence`; see `graphdb.ReservedNodeKeys` and `graphdb.ReservedEdgeKeys`): such a key would override the field, for example replacing `Edge.ID` after the `ErrEdgeExists` check ran. Query property filters may still use reserved keys.

**How bulk writes work.** agedriver has no property indexes, so every lookup by `id` is a sequential scan of all label tables. `BulkCreateEdges` checks a whole batch with one or two queries: `n.id IN [...]` for the endpoints, plus `e.id IN [...]` when any edge has an ID. It then creates the batch with one `UNWIND` statement per edge label, matching endpoints with `WHERE s.id = r.s` so PostgreSQL hash-joins the rows against a single vertex scan. A batch costs a fixed number of scans instead of several per edge. The graph RPC caps batches at `graph.max_bulk_edges` because AGE evaluates a long literal list in worse than linear time.

**Why there are no indexes.** Indexes were tested on AGE 1.5 / PostgreSQL 16. An index on a label's own table is used: a GIN index on `properties` serves `{id: 'x'}`, and an expression index on `agtype_access_operator(VARIADIC ARRAY[properties, '"id"'::agtype])` serves `n.id = 'x'` and `n.id IN [...]`. An index on the parent `_ag_label_vertex` / `_ag_label_edge` does not cover the label tables that inherit from it. AGE creates label tables lazily inside a Cypher `CREATE`, and that does not fire `ddl_command_end` event triggers, so neither a migration nor the `tenant_graph_create` trigger can index labels that do not exist yet. `graph_user` owns its label tables and could create indexes without new grants, but that would put DDL on the write path: `CREATE INDEX` takes a SHARE lock that blocks writers, concurrent first writers race, and the expression form depends on how AGE translates property access internally. Adding indexes reliably needs explicit label creation (`ag_catalog.create_vlabel` / `create_elabel`) together with the index, behind a driver API that callers invoke before writing.

Temporal properties (`valid_from`, `valid_to`, `analyzed_at`) are stored with `graphdb.FormatTime`, a fixed-width UTC layout (`2006-01-02T15:04:05.000000000Z`) whose lexical order equals chronological order. AGE compares these strings in Cypher, so the width matters.

> **Upgrade note (#148):** graphs written before this encoding used RFC3339Nano, which trims trailing zeros. Old and new encodings do not compare correctly, older REQUIRES edges have no `id` or `valid_from`, and derived edge IDs changed format (see `graphdb.DerivedID`): SEMANTIC_MATCH, DEMANDS, IS_TYPE and PARENT_OF IDs always differ, and REQUIRES IDs differ when a job or control ID contains `_` or `%`. Older graphs can also hold one node ID under two labels; edges to such an ID fail as an "ambiguous node ID", and CreateNode or UpsertNode under the other label returns ErrNodeIDConflict. Re-materializing into an old graph adds a second copy of each such edge instead of deduplicating it. No automated rebuild command exists yet; rebuilding an affected tenant's graph from the authoritative stores is a manual process:
>
> 1. Drop the tenant's graph (e.g. `SELECT ag_catalog.drop_graph('crosscodex_{tenant_id}', true)` — the same call the `tenant_graph_drop` trigger in `pkg/db/migrations/001_initial_schema.up.sql` runs on tenant deletion).
> 2. Recreate it with `graphdb.GraphDB.CreateGraph` (idempotent — safe even if the graph already exists).
> 3. Re-import each of the tenant's catalogs (`crosscodex catalog import <file>` or `crosscodex run start <file>`) so control nodes and `PARENT_OF` edges exist again; catalog ingest (`internal/catalog/service.go`) is the only place that creates them, never the analyzer materializers.
> 4. Catalog import queues a full-analysis job automatically; let it run to completion so the graph service (`internal/graph/subscriber_handlers.go`) recreates the `SEMANTIC_MATCH`, `REQUIRES`, `DEMANDS`, and `IS_TYPE` edges from the new results.
>
> The graph is still a materialized view, so no data is lost — but step 4 re-runs the LLM-backed analyzers from scratch rather than replaying already-stored results, so expect it to take as long (and cost as much) as the original analysis.
>
> **Tenant IDs longer than 52 characters (#148):** `pkg/tenant` now rejects them, so every request and message carrying such an ID fails validation, and so does config load when one appears as `tenants.default_tenant` or as a key under `llm.tenant_overrides`, `attestation.tenant_overrides`, `prompt.tenant_overrides` or `synthesis.tenant_overrides`. Their graphs were also never isolated: `create_tenant_graph()` truncates `'crosscodex_' || tenant_id` to PostgreSQL's 63-byte identifier limit, so two such tenants sharing a 52-character prefix share one graph. Find affected tenants as the `postgres` superuser (the `tenants` RLS policy hides every row from `app_user`):
>
> ```sql
> SELECT tenant_id, length(tenant_id) FROM tenants WHERE length(tenant_id) > 52;
> ```
>
> A tenant ID cannot be renamed in place: nine tables reference `tenants(tenant_id)` without `ON UPDATE CASCADE`, and copying rows between tenants is exactly the manual fix the isolation rules forbid. Re-provision instead:
>
> 1. Choose a replacement ID that passes `tenant.ValidateTenantID` (3-52 characters) and does not equal the first 52 characters of any existing `tenant_id`. `crosscodex_<those 52 characters>` is already a graph name — the old tenant's truncated graph, created by `create_tenant_graph()`'s own truncation of `schema_name` — so inserting a tenant whose graph name collides makes `ag_catalog.create_graph` fail inside the `tenant_graph_create` trigger, and the insert (and the restart in step 2) fails with it.
> 2. Set `tenants.default_tenant` to it, and rename the tenant's keys under every `tenant_overrides` map (config load rejects an invalid `default_tenant` and the old keys anyway). Only role `all` provisions the tenant: `attachAllGateway` (`cmd/crosscodexd/bootstrap.go`) calls `db.EnsureTenant`, whose insert fires `tenant_graph_create` and creates the new tenant's graph. Restart `crosscodexd` with role `all` to pick up the change. Role `pipeline`'s `buildPipelineService` (`cmd/crosscodexd/bootstrap.go`) only checks that `default_tenant` is set — it does not provision anything — so a split-role deployment that never runs role `all` must provision the tenant by running `EnsureTenant`'s insert manually, as the `postgres` superuser/owner connection (never as `app_user`, which the `tenants` RLS policy blocks): `INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT (tenant_id) DO NOTHING`.
> 3. Re-import the tenant's catalogs under the new ID and let the queued analysis jobs finish, as in steps 3-4 above. Do not reuse the old graph: it may hold another tenant's data.
>
> The old tenant's rows stay in place. Nothing can read them, because every service rejects the old ID. No supported command deletes a tenant yet (`AdminService.DeleteTenant` is declared in `admin.proto` but not implemented), so removing them is a manual superuser operation under your own retention policy: delete the child-table rows that reference `tenants(tenant_id)` first — none of the nine foreign keys has `ON DELETE CASCADE`, so deleting the `tenants` row before its dependents fails with a foreign-key violation. Only then delete the `tenants` row itself. That delete fires the `tenant_graph_drop` trigger, which runs `ag_catalog.drop_graph('crosscodex_' || OLD.tenant_id, true)` under the same 63-byte truncation as `create_tenant_graph()`: for an over-long tenant ID this drops `crosscodex_<52-character-prefix>`, the one graph every tenant sharing that 52-character prefix was writing into. Confirm no other tenant still depends on that graph before running the delete — it destroys the shared graph outright.

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

An error whose detail would expose internals still has to be actionable, but without that detail. A graph RPC whose error maps to `CodeInternal` does not return driver or database text (SQL and Cypher fragments, SQLSTATE codes, internal graph names) to the client (CWE-209). The client gets a fixed message that says what to do: retry, and if the failure persists, give the operator the trace ID it names (or the time and method name when there is no trace). `rpcError` in `internal/graph/service.go` logs the full error server-side once, with the method and trace ID, so the operator can find the cause. Errors the client can fix (not found, invalid argument, reserved property key) keep their detailed message.

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

This applies to controls, catalogs, embeddings, configuration records, and any other entity where re-import or incremental update is a reasonable operation. Reserve "already exists" errors for cases where duplication genuinely indicates a bug (e.g., two controls with the same derived ID within a single import batch).

Graph nodes and edges are a deliberate exception. `CreateNode` rejects a duplicate label and ID with `ErrNodeExists` (and an ID already used under another label with `ErrNodeIDConflict`, which no writer treats as success), and `CreateEdge` rejects a duplicate non-empty edge ID with `ErrEdgeExists`; both keep the stored element, because the graph is a rebuildable materialized view. `internal/graph`, the live writer, treats `ErrNodeExists` and `ErrEdgeExists` as success. The analyzer `GraphMaterializer`s (no production caller yet; see "Graph writers") do too, except that the artifacts one first checks that a stored node matches and reports a mismatch. For edges whose ID includes the job (REQUIRES, SEMANTIC_MATCH) a duplicate from the same writer carries the same payload; the two SEMANTIC_MATCH writers use different kind tags, so they never collide. Edges whose ID has no job (DEMANDS, IS_TYPE, PARENT_OF) are shared across jobs: the stored edge keeps the first writer's `determined_by` and `confidence`, and later jobs do not change them. The live writer's Artifact nodes are shared the same way: `internal/graph` derives an Artifact ID from the control and the artifact's position in the result list (`<control>__art_<index>`), with no job and no content in it, and treats `ErrNodeExists` as success. A later analysis job that returns a different artifact at the same position for a control therefore leaves the first job's name, type, confidence and `job_id` on that node, and the DEMANDS edge to it is the existing one. Changed artifacts from a re-analysis appear only after the tenant's graph is rebuilt (see the upgrade note above), so "re-materializing the same facts produces the same graph" holds for the same facts, not for revised ones. Graph nodes written by catalog ingest use `UpsertNode` instead: a `Control` node projects a control row that ingest has just upserted, so re-importing a catalog refreshes the node's title, statement and other properties in place (its `PARENT_OF` edges stay attached) instead of leaving the first import's text, while the node keeps the `valid_from` of its first import, so `AsOf` traversals of earlier times still see it. `UpsertNode` replaces the caller-owned properties, so a property the new import no longer sets is removed, and it keeps the `SupersedeFact` state (`valid_to`, `superseded_by`) unless the caller sets `ValidTo`. A catalog ID is a hash of the document content and tenant, so an edited document becomes a new catalog with new nodes; only a re-import of the same bytes (for example after a parser change) reaches the existing nodes. A fact that changes is recorded with `SupersedeFact` and a new edge, never by updating an edge in place.

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
