# CrossCodex Development Guide

This guide provides LLM-specific development guidance for working with the CrossCodex codebase. It is compatible with Claude Code, GitHub Copilot, Cursor, and other AI development tools.

## Architecture Overview

**Multi-Service Platform:** CrossCodex is a compliance mapping platform with 7 core services:

1. **Ingestion Service** - Ingest compliance frameworks (OSCAL catalogs)
2. **Catalog Service** - Manage framework catalogs and controls
3. **Analysis Service** - Coordinate artifact analysis via plugins
4. **LLM Workers** - Execute AI-powered compliance mapping
5. **Synthesis Service** - Generate compliance mappings and evidence
6. **Graph Service** - Manage compliance relationship graphs
7. **Pipeline Service** - Orchestrate end-to-end workflows

**Repository Structure:**

- **Monorepo for Go services** (this repository)
- **Separate repositories** for Python ingestion and TypeScript UI
- **Unified PostgreSQL storage** with AGE (graph) and pgvector (embeddings) extensions
- **NATS JetStream** for inter-service messaging

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

Each tenant gets its own graph instance named `crosscodex_{tenant_id}`. Tenant isolation at the graph level is structural — queries execute within a single graph, and cross-tenant traversal is impossible by construction. This is enforced by `pkg/graphdb`, which prepends the tenant-scoped graph name to every Cypher query.

**Node and edge types currently in use:**

| Type | Kind | Created by | Properties |
|------|------|------------|------------|
| `control` | Node | Catalog materializer | `id`, `title`, `text`, `catalog_id` |
| `artifact_type` | Node | Artifacts materializer | `id`, `name` |
| `artifact` | Node | Artifacts materializer | `id`, `name`, `description` |
| `REQUIRES` | Edge | Relationship materializer | `relationship_type`, `contribution_type`, `confidence`, `determined_by`, `determination_type`, temporal fields |
| `SEMANTIC_MATCH` | Edge | Relationship materializer | `relationship_type`, `contribution_type`, `confidence`, temporal fields |
| `DEMANDS` | Edge | Artifacts materializer | (data properties as needed) |
| `IS_TYPE` | Edge | Artifacts materializer | (data properties as needed) |
| `PARENT_OF` | Edge | Catalog service | (structural only) |

This table will grow as new analyzers and materializers are added. When adding a new node or edge type, update this table.

## Import Patterns

Follow these import patterns strictly:

**Public API (importable by external projects):**
```go
import "github.com/complytime-labs/crosscodex/pkg/config"
import "github.com/complytime-labs/crosscodex/pkg/storage"
```

**Internal implementations (never imported externally):**
```go
import "github.com/complytime-labs/crosscodex/internal/ingestion"
import "github.com/complytime-labs/crosscodex/internal/catalog"
```

**Generated protobuf code:**
```go
import pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
```

**Rules:**
- ❌ Never import `internal/` from `pkg/`
- ❌ Never import sibling `internal/` packages (e.g., `internal/catalog` from `internal/ingestion`)
- ✅ Always import through `pkg/` interfaces for cross-service dependencies

## Protobuf Definitions

**Location:** `api/proto/crosscodex/v1/`

All protobuf definitions are organized under a single versioned package:

```
api/proto/crosscodex/v1/
├── common.proto           # Shared types (timestamps, metadata, errors)
├── tenant.proto           # Multi-tenant isolation primitives
├── catalog.proto          # Catalog service messages
├── ingestion.proto        # Ingestion service messages
├── analysis.proto         # Analysis service messages
├── synthesis.proto        # Synthesis service messages
├── graph.proto            # Graph service messages
├── llm_worker.proto       # LLM worker messages
└── pipeline.proto         # Pipeline service messages
```

**Import Pattern:**

Always import the specific proto files you need rather than a monolithic definition:

```protobuf
syntax = "proto3";
package crosscodex.v1;

import "crosscodex/v1/common.proto";
import "crosscodex/v1/tenant.proto";

// Your service definition
```

**Multi-Tenant Isolation:**

Every message that represents tenant-scoped data MUST include `tenant_id`:

```protobuf
message CatalogRecord {
  string tenant_id = 1;  // Required for all tenant-scoped messages
  string catalog_id = 2;
  // ...
}
```

**Admin Service Tenant Isolation:**

The AdminService intentionally does NOT require TenantContext for system-level operations:

- **HealthCheck, GetServiceStatus** - Monitor cluster-wide health, not tenant-specific
- **IssueCertificate, RevokeCertificate, ListCertificates** - PKI operations span tenants (certificates identify tenants)
- **CreateTenant, ListTenants, DeleteTenant** - Tenant lifecycle management by platform operators

These operations are **platform-administrator only**, enforced by gateway RBAC. Tenant-scoped admin operations (ApplyRetentionPolicy, GetRetentionStats) DO include TenantContext and are filtered accordingly.

This follows standard practice for multi-tenant systems (Kubernetes system:masters, Consul acl:write, AWS root account).

**Temporal Graph Attributes:**

Messages representing graph nodes or edges include temporal attributes for versioning and audit trails:

```protobuf
message ControlNode {
  string tenant_id = 1;
  string control_id = 2;
  
  // Temporal attributes for graph versioning
  google.protobuf.Timestamp created_at = 10;
  google.protobuf.Timestamp updated_at = 11;
  google.protobuf.Timestamp valid_from = 12;
  google.protobuf.Timestamp valid_to = 13;
  
  // Audit metadata
  Metadata metadata = 100;
}
```

**Code Generation:**

Generated Go code is placed in `api/gen/go/crosscodex/v1/` and is gitignored. Regenerate after any proto changes:

```bash
task generate  # Runs buf generate
```

**Validation:**

All proto changes must pass linting and breaking change detection:

```bash
cd api/proto
buf build  # Validate syntax
buf lint   # Check style
buf breaking --against '.git#branch=main'  # Detect breaking changes
```

## Package Ownership & Responsibility

| Package             | Status          | Purpose                                                                                                                                                                            | Key Dependencies                  |
| -------             | ------          | -------                                                                                                                                                                            | ----------------                  |
| **pkg/config**      | `[implemented]` | Configuration loading, XDG compliance, precedence resolution                                                                                                                       | None (foundational)               |
| **pkg/db**          | `[implemented]` | PostgreSQL connection pooling, tenant RLS isolation, migrations                                                                                                                    | pkg/config                        |
| **pkg/graphdb**     | `[implemented]` | Apache AGE openCypher queries, entity retrieval, relationship traversal, bulk operations, Cypher query execution, temporal supersession, tenant-scoped graphs                      | pkg/db                            |
| **pkg/vectordb**    | `[implemented]` | pgvector similarity search for embeddings, batch upsert, tenant isolation, model tracking                                                                                          | pkg/db, pkg/tenant, pkg/telemetry |
| **pkg/natsbus**     | `[implemented]` | NATS JetStream publish/subscribe, stream management, embedded/external dual mode, provenance headers                                                                               | pkg/config, pkg/tenant            |
| **pkg/storage**     | `[implemented]` | Object storage abstraction (local FS / S3)                                                                                                                                         | pkg/config                        |
| **pkg/tlsconfig**   | `[implemented]` | Shared TLS config builder with FIPS enforcement, config merging, cert reload, dev PKI generation                                                                                   | pkg/config                        |
| **pkg/authn**       | `[implemented]` | X.509 mTLS authentication, registry dispatch, audit emission, identity context propagation; Kerberos/SAML stubbed                                                                  | pkg/tlsconfig, pkg/tenant         |
| **pkg/tenant**      | `[implemented]` | Multi-tenant context propagation, isolation enforcement                                                                                                                            | None (foundational)               |
| **pkg/telemetry**   | `[implemented]` | OpenTelemetry traces, metrics, structured logging with trace correlation                                                                                                           | pkg/config                        |
| **pkg/llmclient**   | `[implemented]` | OpenAI-compatible LLM gateway client with credential resolution, retry, telemetry, and audit emission, gateway mode (skip client-side retry when upstream gateway handles retries) | pkg/config, pkg/telemetry         |
| **pkg/oscal**       | `[implemented]` | OSCAL catalog parsing, validation                                                                                                                                                  | None (domain logic)               |
| **pkg/attestation** | `[implemented]` | in-toto layout/link generation and verification, hash chain verification, FIPS enforcement, enriched byproducts, manifest generation, ephemeral key provider, trace correlation    | pkg/telemetry, in-toto-golang     |
| **pkg/analyzer**    | `[implemented]` | Generic analyzer plugin interface, type-safe registry, DAG builder with Kahn's algorithm for level-based parallel execution                                                        | pkg/telemetry (optional)          |
| **pkg/prompt**      | `[implemented]` | Prompt versioning and management: YAML prompt specs, 4-layer resolution (embedded/user/project/CLI), deep merge via mergo, placeholder substitution, few-shot assembly, SHA-256 content hashing, slog.LogValuer debug output | pkg/config, pkg/storage           |
| **internal/analyzer/classify** | `[implemented]` | Classification analyzer: type/level dimensions via LLM, section auto-classification, lenient fail-closed parsing, OTel tracing and metrics | pkg/analyzer, pkg/llmclient, pkg/prompt, pkg/config, pkg/tenant, pkg/telemetry |
| **internal/analyzer/embedding** | `[implemented]` | Embedding analyzer: vector embedding generation via LLM, cosine similarity matrices via gonum, top-K pair filtering, CSV export, text preparation with OSCAL cleaning, OTel tracing and metrics | pkg/analyzer, pkg/llmclient, pkg/vectordb, pkg/storage, pkg/config, pkg/tenant, pkg/telemetry, gonum |
| **internal/analyzer/relationship** | `[implemented]` | Relationship analyzer: NIST IR 8477 multi-sample LLM panel voting, regex parser, plurality consensus with priority tiebreak, GraphMaterializer for edge creation, OTel tracing and metrics | pkg/analyzer, pkg/llmclient, pkg/prompt, pkg/storage, pkg/graphdb, pkg/natsbus, pkg/config, pkg/tenant, pkg/telemetry |
| **internal/analysis** | `[implemented]` | Analysis Engine: DAG-based analyzer orchestration, NATS work dispatch, result collection with retry | pkg/analyzer, pkg/natsbus, pkg/config, pkg/tenant, pkg/telemetry |
| **internal/worker** | `[implemented]` | LLM Worker service: NATS message handler for completion/embedding tasks, tenant-scoped config resolution, fail-closed validation, OTel tracing and metrics, audit emission | pkg/llmclient, pkg/natsbus, pkg/config, pkg/tenant, pkg/telemetry |
| **internal/synthesis** | `[implemented]` | Synthesis Service: viability ranking with Python-parity two-round rounding, quality assessment with 4 diagnostic categories (IQR, NO_RELATIONSHIP rate, contested pairs, actionable coverage), DB persistence via single-transaction UNNEST batch UPDATE (O(1) round-trips), OTel tracing and metrics | pkg/db, pkg/tenant, pkg/storage, pkg/config, pkg/telemetry |
| **internal/graph** | `[implemented]` | Graph Service: gRPC read/write RPCs, NATS subscriber for event-driven materialization, resource resolver abstraction, proto conversion, tenant-scoped queries and traversals | pkg/graphdb, pkg/vectordb, pkg/natsbus, pkg/authn, pkg/tenant, pkg/telemetry |
| **internal/pipeline** | `[implemented]` | Pipeline Service: job lifecycle orchestration, DAG-based stage execution, retry with reset-from-failure, in-toto attestation chain, NATS state events, OTel tracing and metrics | pkg/analyzer, pkg/attestation, pkg/natsbus, pkg/storage, pkg/config, pkg/tenant, pkg/telemetry, internal/analysis, internal/synthesis |

**Dependency Flow:**

```
Foundational: config, tenant
    ↓
Infrastructure: db, storage, natsbus
    ↓
Extended: graphdb, vectordb, tlsconfig, telemetry
    ↓
Security: authn (depends on tlsconfig + tenant)
    ↓
Domain Logic: oscal, attestation, analyzer, llmclient, prompt
```

## Worktree Strategy

Use `git worktree` for parallel feature development to avoid branch-switching overhead.

**Branch Naming Convention:**
```
feature/<issue-number>-<short-description>
```

**Worktree Location:**
`.worktrees/<branch-name>` (gitignored at root)

**Development Workflow:**

1. **Create worktree:**
   ```bash
   git worktree add .worktrees/feature-123 -b feature/123-analyzer-plugins
   ```

2. **Work in worktree:**
   ```bash
   cd .worktrees/feature-123
   # Make changes, commit, test
   ```

3. **Run tests:**
   ```bash
   task lint
   task test:unit
   ```

4. **Cleanup:**
   ```bash
   git worktree remove .worktrees/feature-123
   ```

## Testing Requirements

**Framework: Ginkgo BDD + Gomega**

All tests MUST use [Ginkgo v2](https://onsi.github.io/ginkgo/) with [Gomega](https://onsi.github.io/gomega/) matchers.

NEVER write stdlib `testing.T` tests (except suite bootstraps, `export_test.go` bridge files, and `*_fuzz_test.go` files).

**File naming:**
- `*_bdd_test.go` — Ginkgo specs (Describe/Context/It blocks with Gomega Expect assertions)
- `*_property_test.go` — Property-based tests using `pgregory.net/rapid` inside Ginkgo `It` blocks
- `*_fuzz_test.go` — Go native fuzz tests using stdlib `testing.F` (the one stdlib exception)
- `*_suite_test.go` or suite bootstrap in `*_bdd_test.go` — `func TestSuite(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "...") }`
- `export_test.go` — Go test-package bridge files exposing unexported symbols (no test functions, stays as stdlib)

**Structure:**
```go
var _ = Describe("ComponentName", func() {
    Context("when condition", func() {
        It("should behave", func() {
            Expect(result).To(Equal(expected))
        })
    })
})
```

**Shared behaviors:** Use `internal/testspecs` for cross-package behavioral specs (e.g., `TenantIsolationBehavior`, `ProviderContractBehavior`).

**Unit Tests:**
- Required for all `pkg/` packages
- Use Ginkgo `DescribeTable`/`Entry` for parameterized cases (replaces stdlib table-driven tests)
- Mock external dependencies (database, NATS, HTTP clients)

**Property Tests (required for all `pkg/` packages):**

Every package MUST have a `<pkg>_property_test.go` file with property-based tests using [`pgregory.net/rapid`](https://github.com/flyingmutant/rapid). Property tests verify invariants that must hold for all inputs, not just hand-picked examples.

- Use `rapid.Check(GinkgoT(), func(t *rapid.T) {...})` inside Ginkgo `It` blocks — NOT `rapid.MakeCheck`
- Do NOT add a duplicate `RunSpecs` — the existing suite bootstrap in `*_bdd_test.go` collects property specs automatically
- Wrap all property specs under `Describe("Property Specifications", Ordered, func() { ... })` for `--focus` filtering
- Property tests run as part of `task test:unit` and can be isolated via `task test:property`
- Use `rapid.StringMatching(regex)` for regex-constrained inputs, `rapid.SampledFrom()` for enums
- Invariant categories to cover: roundtrip (encode/decode), idempotency, determinism, security (fail-closed), format compliance

```go
// Example: property test inside Ginkgo
var _ = Describe("Property Specifications", Ordered, func() {
    Context("validateKey — path traversal prevention", func() {
        It("rejects all keys containing path traversal sequences", func() {
            rapid.Check(GinkgoT(), func(t *rapid.T) {
                prefix := rapid.String().Draw(t, "prefix")
                suffix := rapid.String().Draw(t, "suffix")
                key := prefix + ".." + suffix
                err := storage.ExportValidateKey(key)
                if err == nil {
                    t.Fatalf("validateKey accepted traversal key: %q", key)
                }
            })
        })
    })
})
```

**When to write property tests:**
- Validation functions (tenant IDs, names, keys, tokens) — invariant: regex match implies acceptance
- Roundtrip conversions (serialize/deserialize, encode/decode) — invariant: roundtrip preserves data
- Security boundaries (path traversal, injection, credential leakage) — invariant: fail-closed
- Merge/combine operations (config merge, header merge) — invariant: associativity, overlay wins
- Sorting/ordering algorithms — invariant: topological correctness, determinism

**Fuzz Tests (required for security-sensitive packages):**

Packages that accept untrusted input or perform security-critical parsing MUST have a `<pkg>_fuzz_test.go` file with Go native fuzz tests (`testing.F`). Fuzz tests find crashes, panics, and hangs that property tests miss.

- Use stdlib `testing.F` — this is the one permitted exception to the Ginkgo-only rule
- Provide 3-10 seed corpus entries per target (valid, invalid, boundary, attack vectors)
- Assert invariants on accepted inputs (no assertions on error paths — fuzz tests find crashes, not logic bugs)
- Fuzz tests run via `task test:fuzz` (default 10s per target, CI uses 30s)
- Crash inputs are committed to `testdata/fuzz/<FuncName>/` as regression tests

```go
// Example: fuzz test for security-critical parser
func FuzzParseAGVertex(f *testing.F) {
    f.Add(`{"id": 1, "label": "control", "properties": {}}::vertex`)
    f.Add("")
    f.Add("not a vertex")
    f.Add(`{"id": -1}::vertex`)

    f.Fuzz(func(t *testing.T, raw string) {
        _, _ = graphdb.ParseAGVertex(raw)
    })
}
```

**When to write fuzz tests:**
- Any function that parses untrusted strings/bytes (YAML, JSON, Cypher, AGType, DSSE, vectors)
- Credential handling (URI resolution, DSN redaction)
- Validation functions at system boundaries (tenant IDs, storage keys, NATS tokens)
- Functions that construct queries from user input (Cypher escaping, subject building)

**Integration Tests:**
- Colocated with the packages they test (e.g., `pkg/db/integration_test.go`)
- Use Docker Compose or Podman Compose (default) for dependencies
- Run via `task test:integration:<name>` tasks
- Integration tests use the same Ginkgo framework — no stdlib exceptions

**All tests must pass before merge.**

## Security Considerations

**Multi-Tenant Isolation:**

Tenant isolation is enforced at **EVERY** layer:

1. **API Gateway** - Extracts tenant ID from authentication
2. **Services** - Propagates tenant context via `pkg/tenant`
3. **NATS** - Scopes subjects by tenant: `tenant.<tenant-id>.<subject>`
4. **PostgreSQL** - Row-level security (RLS) policies
5. **Object Storage** - Prefixes keys by tenant: `<tenant-id>/<path>`
6. **Graph Database** - Separate graph partitions per tenant

**Rules:**
- ❌ Never bypass tenant context validation
- ❌ Never hardcode tenant IDs
- ✅ Always extract tenant ID from `context.Context` via `pkg/tenant.FromContext()`
- ✅ Always validate tenant ID before data access

**Credentials:**
- ❌ Never commit credentials to version control
- ❌ Never log credentials (API keys, passwords, tokens)
- ✅ Load credentials from environment variables or secret managers
- ✅ Use TLS for all network communication in production

**FIPS Mode:**

When `tls.fips.enabled: true` in configuration:
- Only FIPS-approved cipher suites are used
- TLS 1.2+ required
- `pkg/tlsconfig.VerifyFIPSBuild()` must pass
- Go binaries must be built using GOEXPERIMENT=boringcrypto
- Use the global `FIPS=1` task variable to enable FIPS mode across all build and test tasks (e.g., `task build FIPS=1`, `task test FIPS=1`, `task test:integration:nats FIPS=1`)

## Observability & Attestation

### Goal: Full Provenance Tracing for Auditors

CrossCodex is a compliance platform. Auditors must be able to cryptographically verify every step of compliance processing — from framework ingestion through AI-generated mappings to final compliance reports. This requires two complementary systems:

1. **OpenTelemetry (pkg/telemetry):** Distributed tracing and metrics across all services. Every operation that touches compliance data produces spans with tenant, operation, and data lineage attributes. Trace IDs flow through NATS messages, database operations, and gRPC calls so that an auditor can reconstruct the full processing chain for any compliance artifact.

2. **in-toto Attestation (pkg/attestation):** Cryptographic attestations for compliance-critical operations. Each attestation embeds the OTel trace ID from the active span (via `telemetry.TraceIDFromContext(ctx)`), creating a bidirectional link between the distributed trace and the cryptographic proof. An auditor can start from either the trace or the attestation and reach the other.

The proto field `AuditMetadata.correlation_id` is documented as "OpenTelemetry trace ID" — every service that populates `AuditMetadata` must set this field from the active span context.

### OpenTelemetry Integration (pkg/telemetry)

**Every package that performs I/O, business logic, or cross-service communication MUST integrate `pkg/telemetry`.** This is not optional. A package cannot be marked `[implemented]` without telemetry instrumentation.

Requirements:
- **Traces:** Instrument all public method calls with spans, including tenant and operation metadata
- **Metrics:** Counter for operations, histogram for duration, gauge for resource usage
- **Logs:** Structured logging via slog (trace IDs are injected automatically by `telemetry.Init`)
- **Context:** Propagate `context.Context` through all internal calls; never drop it

**Initialization (service mains):**
```go
// In cmd/daemon/main.go or equivalent service entry point
shutdown, err := telemetry.Init(ctx, cfg.Observability,
    telemetry.WithServiceName("crosscodex-catalog"),
    telemetry.WithServiceVersion(version.Version),
)
if err != nil {
    log.Fatalf("telemetry init: %v", err)
}
defer shutdown(ctx)
// After Init: global TracerProvider, MeterProvider, and slog trace injection are active.
```

**Instrumentation (per-package):**
```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/codes"
    "github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// Package-level tracer — call otel.GetTracerProvider() after Init has run.
var tracer = otel.GetTracerProvider().Tracer("crosscodex/pkg/mypackage")

func (s *Service) PublicMethod(ctx context.Context, req Request) error {
    ctx, span := tracer.Start(ctx, "mypackage.PublicMethod")
    defer span.End()

    span.SetAttributes(
        attribute.String("tenant.id", tenant.FromContext(ctx)),
        attribute.String("operation.type", req.Type),
    )

    // ... business logic

    if err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, err.Error())
        return err
    }
    span.SetStatus(codes.Ok, "")
    return nil
}
```

**Metrics (instrument factories enforce `crosscodex` namespace):**
```go
counter, _ := telemetry.NewCounter("mypackage.operations.total")
histogram, _ := telemetry.NewHistogram("mypackage.duration_ms")
gauge, _ := telemetry.NewGauge("mypackage.pool.utilization")
intCounter, _ := telemetry.NewIntCounter("mypackage.errors.total")
```

**Correlation helpers (for attestation bridge):**
```go
traceID := telemetry.TraceIDFromContext(ctx) // hex string or ""
spanID := telemetry.SpanIDFromContext(ctx)   // hex string or ""
```

**Testing (in-memory assertions, no network):**
```go
import "github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"

tp := telemetrytest.NewTestProvider()
// Use tp.TracerProvider() / tp.MeterProvider() in tests
spans := tp.GetSpans()
metrics := tp.GetMetrics()
tp.Reset()
```

**Config shape (in pkg/config):**
```yaml
observability:
  endpoint: ""              # shared OTLP endpoint; empty = all disabled
  protocol: grpc            # grpc | http
  tracing:
    endpoint: ""            # per-signal override
    protocol: ""            # per-signal override
    sample_rate: 1.0
  metrics:
    endpoint: ""            # per-signal override
    protocol: ""            # per-signal override
    interval: 30s
```

### in-toto Attestation (pkg/attestation)

Compliance-critical operations MUST generate cryptographic attestations:
- **Catalog ingestion:** Attest to OSCAL/Gemara document authenticity and validation
- **Control mappings:** Attest to AI-generated compliance mapping accuracy and model used
- **Risk assessments:** Attest to evaluation results and evidence collection
- **Policy violations:** Attest to enforcement actions taken and audit trails

Every attestation MUST embed the OTel trace ID from the active span:
```go
func (s *ComplianceService) GenerateMapping(ctx context.Context, req MappingRequest) error {
    ctx, span := tracer.Start(ctx, "compliance.GenerateMapping")
    defer span.End()

    // ... perform mapping operation

    predicate := &attestation.CompliancePredicate{
        Operation:   "control.mapping",
        Subject:     req.ControlID,
        Evidence:    result.Evidence,
        Model:       req.LLMModel,
        Timestamp:   time.Now(),
        TraceID:     telemetry.TraceIDFromContext(ctx), // Links attestation to trace
    }

    return s.attestor.Sign(ctx, predicate)
}
```

### Telemetry Integration Requirements

**Already instrumented:**
- **pkg/vectordb** — spans and metrics on all VectorDB operations (StoreEmbedding, FindSimilar, etc.)
- **pkg/llmclient** — spans on Complete/Embed operations with tenant/model/token attributes, Int64Counter for completions/embeddings/errors, Int64Histogram for completion/embedding latency, trace ID correlation in audit events via telemetry.TraceIDFromContext
- **pkg/analyzer** — span on BuildDAG with analyzer.count and dag.levels attributes, error status on cycle/missing dependency failures, Int64Counter for registrations, Float64Histogram for BuildDAG duration, Int64Gauge for registered analyzer count

**Must be retrofitted (telemetry was built after these packages):**
- **pkg/db** — spans on connection acquisition, query execution, migration, health checks, tenant context setup
- **pkg/natsbus** — subscriber consumer span, content hash verification, subscriber metrics still need integration testing against external NATS; W3C traceparent propagation migration pending
- **pkg/authn** — spans on authentication flow, metrics for auth latency/failure rates, OTel trace ID in AuditEvent.SessionID
- **pkg/graphdb** — spans on graph queries, relationship traversal, graph lifecycle
- **pkg/storage** — spans on object get/put/delete/list, metrics for operation latency

**Future packages (must include telemetry from the start):**
- **pkg/oscal** — spans on catalog parsing and validation
- **All internal/ services** — must call `telemetry.Init()` at startup and propagate trace context via NATS message headers and gRPC metadata

## Build & Task Automation

Use `task` (<https://taskfile.dev>) for all build operations:

```bash
task --list         # Show available tasks
task deps           # Download Go dependencies
task generate       # Generate protobuf code
task build          # Build binaries (depends on generate)
task test:unit      # Run unit tests
task lint           # Run linters
task clean          # Remove build artifacts
```

**Never bypass Taskfile** by running raw `go build` or `go test` commands directly. This ensures consistent build behavior across environments.

## Configuration Files

**Taskfile.yml** - Build automation (see `task --list`)
**buf.yaml** - Protobuf linting and breaking change detection
**buf.gen.yaml** - Protobuf code generation (Go + gRPC)
**.gitignore** - Excludes `.build/`, `.test-output/`, `api/gen/`, credentials
**.github/workflows/ci.yml** - CI pipeline (runs `task build`, `task lint`, `task test:unit`)

## Common Patterns

**Error Handling:**

```go
// Return errors, don't log and return
if err != nil {
    return fmt.Errorf("failed to load config: %w", err)
}
```

**Context Propagation:**

```go
// Always pass context.Context as first parameter
func LoadConfig(ctx context.Context, path string) (*Config, error) {
    // Extract tenant ID if needed
    tenantID, err := tenant.FromContext(ctx)
    if err != nil {
        return nil, err
    }
    // ...
}
```

**Functional Options:**

```go
// Use functional options for optional parameters
type Option func(*options)

func WithTimeout(d time.Duration) Option {
    return func(o *options) {
        o.timeout = d
    }
}

func NewClient(endpoint string, opts ...Option) (*Client, error) {
    // Apply options
}
```

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

The graph database (currently Apache AGE) is accessed exclusively through the `pkg/graphdb.GraphDB` interface. No package outside `pkg/graphdb` may import AGE-specific types, use AGE SQL functions directly, or assume the graph shares a PostgreSQL transaction with relational queries. The graph is a materialized view — reconstructible from authoritative stores (see "Graph Data Model" above). Design every graph consumer so that swapping AGE for Neo4j, Neptune, or any openCypher/Gremlin backend requires changes only inside `pkg/graphdb`.

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

## Recent Changes

- **pkg/db implementation** — Added PostgreSQL connection pool with tenant-scoped Row-Level Security, schema migrations via golang-migrate, extension verification, immutability triggers, and comprehensive integration tests including tenant isolation, immutability, and tired-admin threat model scenarios.
- **Build consolidation** — Consolidated all task definitions (build, test, lint, integration) into `.taskfiles/dev.yml`, replacing scattered top-level task definitions.
- **pkg/tenant package** — Added the `pkg/tenant` public API package with `ValidateTenantID` (regex-enforced format: lowercase alphanumeric with hyphens, 3-64 characters), error sentinels (`ErrNoTenant`, `ErrInvalidTenant`, `ErrTenantMismatch`), and the `Context` interface for tenant propagation. The `Context` interface remains unimplemented; `pkg/db` now delegates tenant validation to `pkg/tenant`.
- **pkg/natsbus implementation** — Added dual-mode NATS client (embedded + external) with tenant-scoped subjects, provenance headers (X-Trace-Id, X-Span-Id, X-Tenant-Id, X-Timestamp, X-Content-SHA256), three JetStream audit streams (AUDIT_LLM 90d, AUDIT_DECISIONS indefinite, AUDIT_EVENTS 30d), queue group work distribution, XDG_STATE_HOME-compliant embedded storage, and comprehensive integration tests with TLS.
- **pkg/tlsconfig implementation** — Added shared TLS configuration builder with global + per-target config merging (deep-merge overrides), three TLS modes (off, server-only, mutual), FIPS cipher enforcement via BoringCrypto with auto-discovered GCM-only filtering, general cipher allow/deny lists, certificate reload callbacks for zero-downtime rotation, and a `pki` sub-package for ECDSA P-256 dev certificate generation. Refactored `internal/testcerts` to delegate crypto generation to `pkg/tlsconfig/pki`.
- **pkg/authn implementation** — Added multi-method authentication with registry-based dispatch. X.509 mTLS authenticator maps client certificates to tenant identities via glob-pattern matching on CN, Organization, OrgUnit, SAN Email, SAN DNS, and SAN URI fields. Registry dispatches to ordered authenticators (`ErrUnsupportedMethod` = try next, any other error = stop). Audit emission via `AuditEmitter` interface (natsbus-backed in production). GSSAPI (Kerberos) and SAML authenticators stubbed with `ErrUnsupportedMethod`. New `auth` config section added to `pkg/config`. Extended `pkg/tlsconfig/pki` with `WithOrgUnit`, `WithEmailAddresses`, and `WithURIs` options. Container integration tests validate cross-stack mTLS interop with nginx/OpenSSL.
- **pkg/telemetry implementation** — Added OpenTelemetry package with `Init(ctx, cfg, ...Option)` returning shutdown function, OTLP exporters (gRPC and HTTP), TracerProvider and MeterProvider with global registration, slog handler wrapping for automatic `trace_id`/`span_id` injection in structured logs, `TraceIDFromContext()`/`SpanIDFromContext()` correlation helpers for attestation bridge, thin instrument factories (`NewCounter`, `NewHistogram`, `NewGauge`, `NewIntCounter`) enforcing `crosscodex` meter namespace, `telemetrytest.NewTestProvider()` subpackage with in-memory exporters for unit test assertions, `ObservabilityConfig` added to `pkg/config` with shared endpoint + per-signal override pattern matching `pkg/tlsconfig`. Empty endpoint = disabled (no-op, no error). Integration test infrastructure added with Jaeger all-in-one container in `test/compose.yaml` under `telemetry` profile.
- **pkg/llmclient implementation** — Added OpenAI-compatible chat completion and embedding client with credential resolution via URI schemes (env:/file:/vault:), exponential backoff retry with jitter on 429/5xx with Retry-After header support, tenant ID validation via `pkg/tenant`, model allow-list enforcement, OTel tracing and metrics via `WithTelemetry` option, audit event emission via `AuditEmitter` interface. `LLMConfig` added to `pkg/config` with `APIKeyRef` (credential URI reference), `AllowedModels`, `MaxRetries`, defaults and validation. Integration tests against Ollama with `integration_llm` build tag.
- **Gateway-agnostic LLM architecture** — Added `GatewayMode` boolean to `LLMConfig`. When true, `pkg/llmclient` makes a single attempt per request, deferring retry/failover to an upstream proxy (LiteLLM, Kong, Portkey, etc.). All other client behavior (tenant validation, audit emission, OTel, model allow-list) is unchanged. Full-stack integration tests added with LiteLLM Proxy + Ollama + Jaeger compose services under `llm` profile.
- **pkg/analyzer implementation** — Replaced scaffold with generic `Analyzer[T]` interface parameterized on `proto.Message`, type-erased `RegisteredAnalyzer` wrapper via `registeredWrapper[T]`, concurrency-safe `Registry` with `Register[T]` generic function, `DAG` builder using Kahn's algorithm with level-based parallel execution grouping, `Subset` for partial DAG construction with transitive dependency closure, cycle detection with formatted cycle path reporting, and OTel tracing on `BuildDAG` via `WithTelemetry(trace.TracerProvider)` option. Callers are responsible for propagating tenant context via `context.Context`; the package does not perform I/O or enforce tenant isolation directly. Comprehensive BDD test suite covers behavioral specs, edge cases (cycles, missing deps, type mismatches, empty registry), copy safety, telemetry integration, and concurrent registry access.
- **pkg/attestation completion** — Added EphemeralKeyProvider (in-memory ECDSA P-256), VerifyLayout with expiry checking (ErrExpired), VerifyChain for hash chain verification between consecutive pipeline steps (ErrChainBroken), FIPS algorithm enforcement via WithFIPSMode (ErrNonFIPSAlgorithm), enriched ByProducts (span_id, timestamp, hostname) via WithIncludeByProducts, GenerateManifest for GNU coreutils format SHA-256 manifests, LinkOption with WithByProducts for per-call byproduct injection. AttestationConfig added to pkg/config with per-tenant overrides via ForTenant(). JobAttestationKey added to pkg/storage for job-structured attestation paths. DAG-to-Layout bridge adapter added in internal/pipeline/attestation.
- **Attestation config surface fix** — Renamed `KeyPath`/`CertPath` to `PrivateKeyPath`/`PublicKeyPath` for naming fidelity (FileKeyProvider expects SPKI PEM, not X.509 certificate). Expanded `AttestationOverride` to cover all five fields (`Enabled`, `PrivateKeyPath`, `PublicKeyPath`, `ExpiryDuration`, `IncludeByProducts`). `ForTenant()` now returns `AttestationTenantConfig` struct instead of two bools. Added per-tenant validation (key path pairing, positive expiry). FIPS mode documented as derived from `tls.fips.enabled` (no duplicate knob). Added "Configuration Surface Discipline" to Defensive Design Principles.
- **pkg/prompt implementation** — Added prompt versioning and management system. YAML prompt specs with name, version, templates (system/user), metadata, and few-shot examples. 4-layer resolution: embedded defaults (baked into binary via `embed.FS`), user-level (`$XDG_DATA_HOME/crosscodex/prompts/`), project-level (`.crosscodex/prompts/`), CLI overrides. Deep merge via `dario.cat/mergo` with configurable slice strategies (replace/append/deep_copy). `${placeholder}` substitution, few-shot as user/assistant message pairs, SHA-256 content hashing via `storage.ContentHash`, `slog.LogValuer` for debug output. `PromptConfig` added to `pkg/config` with `capture_content`, `allow_commands`, `layer_paths`, per-layer enable/order/merge_mode, and per-tenant overrides. `PromptName`/`PromptVersion` fields added to `llmclient.AuditEvent`. Refactored `pkg/oscal` to consume `prompt.Registry` replacing the old `PromptLoader` interface. Existing prompt YAML files moved from `pkg/oscal/prompts/` to `pkg/prompt/defaults/` and converted to PromptSpec format. Comprehensive test suite: BDD unit tests (31 specs), property tests (placeholder roundtrip, hash determinism, merge idempotency), fuzz tests (YAML parsing, placeholder substitution).
- **internal/analyzer/classify implementation** — First concrete `Analyzer[T]` plugin: classifies compliance requirements on type (Technical/Procedural/Both/None) and level (Strategic/Tactical/Operational/None) dimensions. Implements `pkg/analyzer.Analyzer[*pb.Control]` with LLM-backed classification via `pkg/prompt` and `pkg/llmclient`, section auto-classification (None|None without LLM call), lenient response parsing with fail-closed defaults (unknown input → None|None), rune-safe text truncation, OTel tracing and metrics via `WithTelemetry` option. `AnalysisConfig` and `ClassificationConfig` added to `pkg/config` with defaults and validation (temperature 0.0-2.0, positive max_text_length/max_tokens). `classify.yaml` prompt spec added to `pkg/prompt/defaults/` with 8 few-shot examples. `pkg/prompt.SubstitutePlaceholders` exported for cross-package placeholder substitution. Comprehensive test suite: BDD unit tests, property tests (parse invariants, sanitization, hash determinism), fuzz tests (parser).
- **internal/analyzer/embedding implementation** — Second `Analyzer[T]` plugin: generates vector embeddings for compliance controls and builds cosine similarity matrices. Implements `pkg/analyzer.Analyzer[*pb.Control]` with `DependsOn: ["classify"]` for DAG ordering. Text preparation ported from Python OllamaCrosswalker: ancestor context prepend (`[Root Title] text`), OSCAL template/PDF artifact cleaning via compiled regexes, rune-safe truncation to configurable `max_chars`. Cosine similarity via `gonum/floats` (float32→float64 at boundary for precision), symmetric matrix builder with [0,100] scaling, top-K pair filtering (upper-triangle, sorted descending), Python-compatible CSV export. `EmbeddingConfig` (enabled, models, max_chars, batch_size) and `RelationshipConfig` (top_k) added to `pkg/config` with defaults and validation. Multi-model support: one task per control per model. Dependencies on `pkg/vectordb.VectorDB` and `pkg/storage.Provider` are injected for use during task execution by the pipeline orchestrator; the package itself produces work tasks and aggregates results. OTel tracing and metrics via `WithTelemetry` option. Comprehensive test suite: BDD unit tests, property tests (cosine symmetry/self-similarity/range/zero-safety, cleaning idempotency, truncation bound, matrix symmetry), fuzz tests (text cleaning, similarity computation).
- **internal/analyzer/relationship implementation** — Third `Analyzer[T]` plugin: classifies NIST IR 8477 relationships between control pairs using multi-sample LLM panel voting. Implements `pkg/analyzer.Analyzer[*pb.Control]` with `DependsOn: ["embedding"]` for DAG ordering. Regex-based response parser ports Python `_parse_response()` with fail-closed defaults. Deterministic consensus algorithm ports Python `_compute_consensus()` with one documented divergence: contribution type tiebreaks favor INTEGRAL_TO (Python's `max()` is non-deterministic on ties). CandidateProvider interface decouples from embedding similarity. GraphMaterializer creates SEMANTIC_MATCH edges from stored pair results (graph as materialized view). Proto expanded: 8 NIST IR 8477 relationship types replacing 4 coarse types, ContributionType and ConfidenceLevel enums added. RelationshipConfig expanded from 1 field (TopK) to 9 fields. `relationship.yaml` prompt spec with 8 few-shot examples ported from OllamaCrosswalker. OTel tracing (5 spans) and metrics (4 instruments: vote counter, consensus latency histogram, pair counter, edge materialization counter). Comprehensive test suite: BDD unit tests (parser + consensus Python parity vectors + analyzer struct + materializer), 8 rapid property tests, 2 fuzz tests.
- **internal/analysis implementation** — Analysis Engine service: DAG-based orchestration of registered analyzers via Dispatcher/Collector pattern. Engine builds execution DAGs from analyzer registry, executes levels in parallel (errgroup collect-all semantics), dispatches tasks to NATS work subjects via Dispatcher, collects results with per-task retry (exponential backoff + jitter) via Collector, aggregates partial results, and reports stage events to Pipeline via StageReporter interface. EngineConfig added to pkg/config with task_timeout, max_retries, retry_backoff fields and bounded validation. Error sanitization prevents information disclosure in NATS-published error strings. Context cancellation returns partial ExecutionResult. NATSStageReporter publishes stage events to pipeline subjects. Comprehensive test suite: BDD unit tests (dispatcher, collector, engine), property tests (backoff bounds), fuzz tests (proto serialization).
- **internal/worker implementation** — LLM Worker service: NATS message handler dispatching completion and embedding tasks to `pkg/llmclient`, tenant-scoped config resolution via `LLMConfig.ForTenant()`, fail-closed tenant validation (empty and malformed IDs rejected at handler entry), default model fallback from tenant config, error categorization with sanitized span attributes, nil-return NATS handler contract (prevents redelivery loops), OTel tracing and metrics via `WithTelemetry` option (tracer, 4 meter instruments with WARN-logged creation errors), audit emission via `NATSAuditEmitter`. `WorkerConfig` added to `pkg/config` with `QueueGroup` field (whitespace rejection, default `"llm-workers"` via `defaultQueueGroup` constant in `types.go`) and validation via `Validate()`. `NATSAuditEmitter` constructed via `NewNATSAuditEmitterWithMetrics` (registers `crosscodex.worker.audit.failures.total` Int64Counter; falls back to `NewNATSAuditEmitter` when metrics are unavailable). `LLMConfig` extended with `TenantOverrides map[string]LLMOverride` field (keys must satisfy `pkg/tenant.ValidateTenantID`); `LLMOverride` holds pointer-typed per-tenant overrides; `LLMTenantConfig` is the fully resolved view returned by `LLMConfig.ForTenant(tenantID)`. `GatewayMode` in `LLMTenantConfig` is always inherited from the global config and cannot be overridden per-tenant. Comprehensive test suite: BDD unit tests (completion/embedding routing, model resolution, error categories, tenant validation, telemetry integration, queue group distribution, default queue group fallback), property tests (payload roundtrip, embedding result float32 roundtrip precision, completion field preservation, error wrapping), fuzz tests via payload fuzzing.
- **internal/synthesis implementation** — Synthesis Service: viability ranking with Python-parity two-round rounding, quality assessment with 4 diagnostic categories (IQR, NO_RELATIONSHIP rate, contested pairs, actionable coverage), DB persistence via single-transaction UNNEST batch UPDATE (O(1) round-trips), OTel tracing and metrics. Ranker transforms `[]SynthesisInput` + classifications into `[]SynthesisRow` with viability weights. Assessor evaluates `[]SynthesisRow` into `*QualityReport` with diagnostics (embedding spread IQR via linear interpolation, NO_RELATIONSHIP rate, contested pairs fraction, actionable coverage). Service orchestrates Ranker, Assessor, DB persistence (single transaction, `UPDATE vote_summaries AS vs SET viability = u.viability FROM UNNEST($1::float8[], $2::text[], $3::text[]) AS u(viability, source_id, target_id) WHERE ...` with count-mismatch detection), and content hash via proto deterministic marshaling + `storage.ContentHash()`. `SynthesisConfig` added to `pkg/config` with `Viability` (TypeMismatchFactor=0.8, SkipLevelFactor=0.7, IntegralToFactor=1.1), `Assessment` (IQRGood=20, IQRPoor=10, NoRelHigh=0.97, NoRelLow=0.80, ContestedWarn=0.20, ActionableWarn=0.30), `ConfidenceThreshold=0.5`, `MaxMappingsPerControl=10`, per-tenant overrides via `ForTenant()`. `DiagnosticSeverity` enum and `DiagnosticEntry` message added to synthesis.proto. Comprehensive test suite: 66 BDD unit tests, 10 rapid property tests, 4 fuzz tests, 25 config validation tests.
- **Graph topology anti-pattern fix** — Removed `Source`/`Target` fields from `pkg/graphdb.Edge` struct. Edge endpoints are now passed as explicit `sourceID, targetID string` parameters to `CreateEdge(ctx, tenant, sourceID, targetID, edge)` instead of being stored as Cypher properties. Removed redundant `tenant_id` edge property from `CreateRequiresEdge` (already the graph partition key via graph name). Changed `extractString`/`extractFloat` in agtype.go to copy-not-delete (properties remain in Properties map; typed fields are convenience accessors). Removed `start_id`/`end_id` numeric fallback from `ageEdgeToEdge` — edge endpoint identity now comes exclusively from `Relationship.Source`/`Relationship.Target` Node fields populated by MATCH pattern columns. Updated materializers (relationship, artifacts, catalog) and all tests. Added "Never Store Structural Topology as Data Properties" to Defensive Design Principles.
- **internal/graph implementation** — Graph Service implementing gRPC read/write RPCs (GetNode, GetEdge, Traverse, Query, SimilaritySearch, TemporalQuery, CreateNode, CreateEdge, BulkCreateEdges, SupersedeFact) with CQRS architecture: event-driven writes via NATS subscriber consume pipeline stage completion events and materialize nodes/edges into per-tenant Apache AGE graphs, synchronous reads via gRPC. ResourceResolver abstraction with PGResolver for fetching analysis results. Proto conversion layer. OTel tracing and metrics on every handler. Comprehensive test suite: BDD unit tests, property-based tests with pgregory.net/rapid, integration tests with tenant isolation enforcement.
- **internal/pipeline implementation** — Pipeline Service: job lifecycle orchestration with DAG-based stage execution via analysis engine, synthesis executor, and async graph materialization. gRPC RPCs (CreateJob, GetJob, ListJobs, CancelJob, RetryJob, Start, Stop) with tenant isolation via RLS-scoped PGStore. Retry supports reset-from-failure (partial re-execution from first failed stage) and full re-creation. In-toto attestation chain: layout creation from DAG, per-stage link generation with material/product digests, chain verification, bundle storage. NATS event publishing for job state and stage transitions. OTel tracing and metrics (job counter, duration histogram). DBStageReporter bridges analysis engine stage events to both database persistence and NATS publishing. Graceful shutdown via Stop() with cancel-all and WaitGroup drain with timeout.

**TODO:** NATS account-level tenant authorization is not yet integrated with `pkg/authn`. While `pkg/authn` is now implemented (X.509 mTLS), NATS account-level isolation requires a separate integration layer. Currently, tenant isolation is enforced at the subject level via `pkg/tenant.ValidateTenantID()`. When the NATS-authn integration is built, add per-tenant NATS accounts for server-level isolation.

**Attestation Testing Requirement:** Every package that produces provenance data (natsbus, llmclient, future services) must test: (1) content hashes are computed correctly and deterministically, (2) trace context propagates through publish/subscribe round-trips, (3) all mandatory provenance headers are present on every published message, (4) content hash in metadata matches recomputed hash of received payload, (5) missing or corrupt provenance headers produce actionable errors.

## Next Steps - Keep This Up to Date

After scaffolding is complete, the next implementation phases are:

1. **Phase 1: Implement foundational packages** (`config`, `tenant`) ✓ Done
2. **Phase 2: Implement infrastructure packages** (`db`, `storage`, `natsbus`) ✓ Done
3. **Phase 3: Implement extended packages** (`graphdb`, `vectordb`, `tlsconfig`, `telemetry`) ✓ Done
4. **Phase 4: Implement security packages** (`authn`) ✓ Done
5. **Phase 5: Implement domain packages** (`oscal`, `attestation`, `analyzer`, `llmclient`) ✓ Done
6. **Phase 6: Implement services** (`internal/worker` ✓ Done; `internal/synthesis` ✓ Done; `internal/graph` ✓ Done; `internal/pipeline` ✓ Done; `internal/ingestion` — next)

Refer to the issue tracker for detailed implementation plans for each phase.
