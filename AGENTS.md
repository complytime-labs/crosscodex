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

| Package | Status | Purpose | Key Dependencies |
| ------- | ------ | ------- | ---------------- |
| **pkg/config** | `[implemented]` | Configuration loading, XDG compliance, precedence resolution | None (foundational) |
| **pkg/db** | `[implemented]` | PostgreSQL connection pooling, tenant RLS isolation, migrations | pkg/config |
| **pkg/graphdb** | `[scaffold]` | Apache AGE openCypher queries, relationship traversal | pkg/db |
| **pkg/vectordb** | `[scaffold]` | pgvector similarity search for embeddings | pkg/db, pkg/tenant, pkg/telemetry |
| **pkg/natsbus** | `[implemented]` | NATS JetStream publish/subscribe, stream management, embedded/external dual mode, provenance headers | pkg/config, pkg/tenant |
| **pkg/storage** | `[implemented]` | Object storage abstraction (local FS / S3) | pkg/config |
| **pkg/tlsconfig** | `[scaffold]` | TLS setup, FIPS validation, certificate loading | pkg/config |
| **pkg/authn** | `[scaffold]` | mTLS, Kerberos, SAML authentication | pkg/tlsconfig, pkg/tenant |
| **pkg/tenant** | `[scaffold]` | Multi-tenant context propagation, isolation enforcement | None (foundational) |
| **pkg/telemetry** | `[scaffold]` | OpenTelemetry traces, metrics, logs | pkg/config |
| **pkg/llmclient** | `[scaffold]` | LLM gateway client, completion & embedding requests | pkg/config, pkg/telemetry |
| **pkg/oscal** | `[scaffold]` | OSCAL catalog parsing, validation | None (domain logic) |
| **pkg/attestation** | `[scaffold]` | in-toto layout, link generation, signing | pkg/tlsconfig |
| **pkg/analyzer** | `[scaffold]` | Plugin interface for analysis capabilities | None (interface only) |

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
Domain Logic: oscal, attestation, analyzer, llmclient
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
   task test:unit
   ```

4. **Commit with conventional commits:**
   ```bash
   git commit -m "feat(analyzer): add terraform analyzer plugin"
   ```

5. **Push and create PR:**
   ```bash
   git push -u origin feature/123-analyzer-plugins
   gh pr create
   ```

6. **Cleanup after merge:**
   ```bash
   git worktree remove .worktrees/feature-123
   ```

## Testing Requirements

**Unit Tests:**
- Required for all `pkg/` packages
- Use table-driven tests for multiple cases
- Mock external dependencies (database, NATS, HTTP clients)

**Integration Tests:**
- Located in `tests/` directory
- Test service interactions (e.g., NATS + database + storage)
- Use Docker Compose or Podman Compose for dependencies

**E2E Tests:**
- Use Venom (YAML-based test suites) for full pipeline validation
- Located in `tests/e2e/`

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
- `pkg/tlsconfig.Builder.ValidateFIPS()` must pass
- Go binaries must be built using GOEXPERIMENT=boringcrypto
- Use the global `FIPS=1` task variable to enable FIPS mode across all build and test tasks (e.g., `task build FIPS=1`, `task test FIPS=1`, `task test:integration:nats FIPS=1`)

## Observability & Attestation

**OpenTelemetry Integration (pkg/telemetry):**

All packages that perform business operations MUST integrate OpenTelemetry:
- ✅ **Traces:** Instrument all public method calls with spans, including tenant and operation metadata
- ✅ **Metrics:** Counter for operations, histogram for duration, gauge for resource usage
- ✅ **Logs:** Structured logging with tenant context and correlation IDs
- ✅ **Context:** Propagate trace context through all internal calls

**Required telemetry for all packages:**
```go
// Example instrumentation pattern
func (s *Service) PublicMethod(ctx context.Context, req Request) error {
    ctx, span := telemetry.StartSpan(ctx, "service.public_method")
    defer span.End()
    
    // Add tenant and operation attributes
    span.SetAttributes(
        attribute.String("tenant.id", tenant.FromContext(ctx)),
        attribute.String("operation.type", req.Type),
    )
    
    telemetry.Counter("service.operations").Add(ctx, 1)
    start := time.Now()
    defer func() {
        telemetry.Histogram("service.duration").Record(ctx, time.Since(start).Milliseconds())
    }()
    
    // ... business logic
}
```

**in-toto Attestation (pkg/attestation):**

Compliance-critical operations MUST generate cryptographic attestations:
- ✅ **Catalog ingestion:** Attest to OSCAL/Gemara document authenticity and validation
- ✅ **Control mappings:** Attest to AI-generated compliance mapping accuracy and model used
- ✅ **Risk assessments:** Attest to evaluation results and evidence collection
- ✅ **Policy violations:** Attest to enforcement actions taken and audit trails

**Required attestation for compliance operations:**
```go
// Example attestation pattern
func (s *ComplianceService) GenerateMapping(ctx context.Context, req MappingRequest) error {
    // ... perform mapping operation
    
    // Generate attestation for compliance audit
    predicate := &attestation.CompliancePredicate{
        Operation: "control.mapping",
        Subject:   req.ControlID,
        Evidence:  result.Evidence,
        Model:     req.LLMModel,
        Timestamp: time.Now(),
    }
    
    return s.attestor.Sign(ctx, predicate)
}
```

**Integration Requirements:**
- **pkg/vectordb** MUST emit telemetry for similarity searches, model validation, tenant access
- **pkg/graphdb** MUST emit telemetry for relationship queries and graph traversals
- **pkg/llmclient** MUST generate attestations for AI-generated compliance content
- **All services** MUST propagate trace context via NATS message headers

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

## Recent Changes

- **pkg/db implementation** — Added PostgreSQL connection pool with tenant-scoped Row-Level Security, schema migrations via golang-migrate, extension verification, immutability triggers, and comprehensive integration tests including tenant isolation, immutability, and tired-admin threat model scenarios.
- **Build consolidation** — Consolidated all task definitions (build, test, lint, integration) into `.taskfiles/dev.yml`, replacing scattered top-level task definitions.
- **pkg/tenant package** — Added the `pkg/tenant` public API package with `ValidateTenantID` (regex-enforced format: lowercase alphanumeric with hyphens, 3-64 characters), error sentinels (`ErrNoTenant`, `ErrInvalidTenant`, `ErrTenantMismatch`), and the `Context` interface for tenant propagation. The `Context` interface remains unimplemented; `pkg/db` now delegates tenant validation to `pkg/tenant`.
- **pkg/natsbus implementation** — Added dual-mode NATS client (embedded + external) with tenant-scoped subjects, provenance headers (X-Trace-Id, X-Span-Id, X-Tenant-Id, X-Timestamp, X-Content-SHA256), three JetStream audit streams (AUDIT_LLM 90d, AUDIT_DECISIONS indefinite, AUDIT_EVENTS 30d), queue group work distribution, XDG_STATE_HOME-compliant embedded storage, and comprehensive integration tests with TLS.

**TODO:** NATS account-level tenant authorization is deferred until `pkg/authn` is implemented. Currently, tenant isolation is enforced at the subject level via `pkg/tenant.ValidateTenantID()`. When `pkg/authn` adds NATS account support, add per-tenant NATS accounts for server-level isolation.

**Attestation Testing Requirement:** Every package that produces provenance data (natsbus, llmclient, future services) must test: (1) content hashes are computed correctly and deterministically, (2) trace context propagates through publish/subscribe round-trips, (3) all mandatory provenance headers are present on every published message, (4) content hash in metadata matches recomputed hash of received payload, (5) missing or corrupt provenance headers produce actionable errors.

## Next Steps

After scaffolding is complete, the next implementation phases are:

1. **Phase 1: Implement foundational packages** (`config`, `tenant`)
2. **Phase 2: Implement infrastructure packages** (`db`, `storage`, `natsbus`)
3. **Phase 3: Implement extended packages** (`graphdb`, `vectordb`, `tlsconfig`, `telemetry`)
4. **Phase 4: Implement security packages** (`authn`)
5. **Phase 5: Implement domain packages** (`oscal`, `attestation`, `analyzer`, `llmclient`)
6. **Phase 6: Implement first service** (Ingestion Service in `internal/ingestion`)

Refer to the issue tracker for detailed implementation plans for each phase.
