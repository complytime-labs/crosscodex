# CrossCodex Agent Guide

Rules for coding agents in this Go monorepo. Rationale for the design rules below lives in `docs/dev/design-principles.md`; read it before changing an isolation boundary, a safety mechanism, the graph schema, or the relationship taxonomy.

## Commands

Use `task` for everything; never run raw `go build` / `go test`.

- `task dev:deps`, `task generate` (buf; output in gitignored `api/gen/`), `task build`, `task lint`, `task check` (lint, then unit tests, then `test:race`, sequentially)
- `task test:unit`, `task test:property`, `task test:race` (`pkg/graphdb` with the race detector), `task test:fuzz` (`FUZZ_TIME=30s` in CI), `task test:integration:<name>` (see `task --list`)
- `FIPS=1` on any build/test task enables BoringCrypto FIPS mode.
- Proto changes: from `api/proto`, `go tool buf lint` and `go tool buf breaking --against '../../.git#branch=main,subdir=api/proto'` must introduce no new failures. No task or CI job runs these (`task dev:lint:proto` is protolint), and `buf lint` already has violations in `gateway.proto`.

## Imports

- `pkg/` never imports `internal/`.
- `internal/` packages do not import each other, except: `internal/pipeline` is the composition root (imports `analysis`, `synthesis`, `analyzer/*`, `gateway`, `rpcserver`); services implement backend interfaces defined in `internal/gateway` (e.g. `graph` → `gateway.GraphBackend`); `gateway` imports `rpcserver`. Analyzers may import the shared `internal/analyzer` helper package but never each other. New cross-service dependencies go through `pkg/` interfaces, not new `internal/` → `internal/` imports. Test helpers (`testspecs`, `testcerts`) and `version` are shared.
- `pkg/graphdb/agedriver` is imported only by `cmd/crosscodexd/resources.go` (and integration tests). Everything else uses the `pkg/graphdb.GraphDB` interface; no AGE types or AGE SQL outside `agedriver`, and never assume the graph shares a PostgreSQL transaction with relational queries.

## Graph model

- Relational tables and object storage are authoritative; the graph is a materialized view rebuilt by materializers. Graph writes must be idempotent.
- Edge endpoints are structure: pass them to `CreateEdge(ctx, tenant, sourceID, targetID, edge)`, never as edge properties. Never put `tenant_id` on edges (the graph name is the partition). Read endpoints from `Relationship.Source` / `.Target`.
- The relationship enum (`pkg/config/validate.go` `validRelationshipTypes`, `internal/analyzer/relationship/types.go`, currently the 8 NIST IR 8477 types) is extensible. Extend it when a real relationship doesn't fit; keep structurally different signals outside it (see `internal/synthesis.ConsensusRequires`). The long-term target includes ReqIF and SysML v2 Requirements, so keep node/edge property schemas and the enum easy to extend rather than compliance-catalog-specific.

## Protobuf

- Every tenant-scoped request carries `TenantContext tenant_context` (`common.proto`).
- Exception: AdminService system-level RPCs (health/service status, certificate issue/revoke/list, tenant create/get/update/list/delete) take no TenantContext; they are platform-admin only via gateway RBAC. Retention and hold RPCs are tenant-scoped and do take TenantContext.
- Graph node/edge messages carry `TemporalAttributes temporal`, `AuditMetadata audit` and `ProvenanceMetadata provenance`.

## Testing

- Ginkgo v2 + Gomega only. Stdlib `testing` is allowed only in suite bootstraps (`RegisterFailHandler(Fail); RunSpecs(...)`), `export_test.go` bridge files, and `*_fuzz_test.go`.
- File suffixes: `*_bdd_test.go` (specs), `*_property_test.go`, `*_fuzz_test.go`, `export_test.go`. Parameterize with `DescribeTable`/`Entry`, not table-driven loops.
- Shared cross-package specs live in `internal/testspecs` (`TenantIsolationBehavior`, `ConfigurationComplianceBehavior`, `SecurityBoundaryBehavior`, `ErrorHandlingBehavior`, `GraphDBContractBehavior`). A new `graphdb.GraphDB` driver must run `GraphDBContractBehavior`; a contract change updates `graphdb.GraphDB`'s doc comment and the shared specs together.
- Unit tests that need a graph use `pkg/graphdb/memdriver` (call `CreateGraph` first). To inject a driver error or an output memdriver can't produce, embed `graphdb.GraphDB` over a memdriver and override only those methods (see `countingGraph` in `internal/catalog/graph_write_bdd_test.go`); don't write a new full `GraphDB` fake. `internal/graph`'s `mockGraphDB` predates this rule; specs in that package may keep using it, including where every method must fail, but prefer the memdriver embed when only some methods are intercepted.
- Every `pkg/` package needs unit tests with mocked externals and a `<pkg>_property_test.go`:
  - use `rapid.Check(GinkgoT(), ...)` inside `It`, never `rapid.MakeCheck`;
  - wrap specs in `Describe("Property Specifications", Ordered, ...)`;
  - no extra `RunSpecs`.
- Packages that parse untrusted input, handle credentials, validate at system boundaries, or build queries need a `<pkg>_fuzz_test.go`: 3–10 seeds (valid, invalid, boundary, attack), invariants asserted only on accepted input, crash inputs committed to `testdata/fuzz/<FuncName>/`.
- Integration tests are Ginkgo, colocated with their package, use Podman/Docker Compose, and get a `task test:integration:<name>` entry.
- Every safety mechanism needs a negative test asserting: (1) an error is returned, (2) the message is actionable, (3) the data is unchanged afterward.
- Packages producing provenance data must test: deterministic content hashes; trace context surviving publish/subscribe; all mandatory provenance headers present; metadata hash matches recomputed payload hash; missing/corrupt headers produce actionable errors.
- DB isolation tests run as the restricted application role, never the superuser.

## Security

- Get the tenant from `tenant.FromContext(ctx)` (returns `(string, error)`), validate it before data access, never hardcode tenant IDs or bypass the check.
- NATS tenant isolation is subject-level only (via `pkg/tenant.ValidateTenantID`). Per-tenant NATS accounts are not implemented; do not assume server-level isolation.
- Never log or commit credentials. Load them from env or secret managers.
- Production network traffic uses TLS via `pkg/tlsconfig`; TLS mode `off` is for dev and tests only.
- Fail closed: an absent or misconfigured safety mechanism denies (RLS matches zero rows without `app.current_tenant`; no fallback graph; missing extension → refuse to start).
- Batch operations over rows with mixed protection states must fail entirely, not partially.
- The app runs as a restricted DB role. GRANTs are explicit and minimal (SELECT, INSERT, UPDATE, DELETE — never TRUNCATE, ALTER, TRIGGER). Comment in code why the restricted role exists.
- Guard against the tired admin: test bulk ops over mixed rows, attempts to disable triggers/RLS, privilege escalation (TRUNCATE/ALTER/COPY), and manual "fixes" like resetting a completed job or reassigning rows between tenants.

## Errors and configuration

- Return wrapped errors (`fmt.Errorf("...: %w", err)`); don't log and return. Exception: where detail is withheld from the caller (CWE-209, e.g. `internal/graph` `rpcError` for `CodeInternal`), log the full error once at that boundary.
- Human-triggerable errors state what happened, why it was blocked, and what to do instead — e.g. `cannot modify job abc123: status is "completed". To retry, create a new job instead of resetting this one.`
- Tuning values a deployment might change (thresholds, patterns, allowlists, chunk sizes, retry counts) are config in `pkg/config`, not code.
- Every functional option `WithX` has a `pkg/config` field feeding it, or a doc comment stating its wiring contract. Field names match meaning (e.g. `public_key_path`, not `cert_path`). With `tenant_overrides`, every tenant-variable field is overridable (document global-only fields), and overrides get the same validation as globals. Never add a second knob for a value with a canonical source (FIPS is always `tls.fips.enabled`). Each field is tested for default, valid override, and rejected invalid value with an actionable message.
- On an identity conflict with valid data, upsert rather than reject. Reserve "already exists" for real bugs (e.g. duplicate derived IDs within one import batch). Exception: graph `CreateNode`/`CreateEdge` reject duplicates (`ErrNodeExists`/`ErrEdgeExists`); `internal/graph`'s subscriber writers treat that as success; the artifacts materializer accepts an existing Artifact node only when its content matches (verify-then-accept). Catalog ingest upserts `Control` nodes with `UpsertNode`, so re-import refreshes them. See "Upsert Over Reject" in `docs/dev/design-principles.md`.

## Observability and attestation

- Every package doing I/O, business logic, or cross-service calls is instrumented with `pkg/telemetry`: spans on public methods with tenant/operation attributes; an operations counter, a duration histogram, and a gauge for resource usage via `telemetry.NewCounter` / `NewHistogram` / `NewGauge` / `NewIntCounter`; slog logging; `context.Context` propagated everywhere. Test with `telemetrytest.NewTestProvider()`.
- Service mains call `telemetry.Init()` at startup (no binary does yet — a known wiring gap), and trace context propagates through NATS message headers and Connect request headers.
- Populate `AuditMetadata.correlation_id` from the active span's trace ID.
- Catalog ingestion, control mappings, risk assessments, and policy enforcement actions produce in-toto attestations (`pkg/attestation`) embedding `telemetry.TraceIDFromContext(ctx)`.

## Git

- Branches: `feature/<issue-number>-<short-description>`. Worktrees go in `.worktrees/` (gitignored).
