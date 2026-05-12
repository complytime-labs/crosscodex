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

| Package | Purpose | Key Dependencies |
|---------|---------|------------------|
| **pkg/config** | Configuration loading, XDG compliance, precedence resolution | None (foundational) |
| **pkg/db** | PostgreSQL connection pooling, transaction management | pkg/config |
| **pkg/graphdb** | Apache AGE openCypher queries, relationship traversal | pkg/db |
| **pkg/vectordb** | pgvector similarity search for embeddings | pkg/db |
| **pkg/natsbus** | NATS JetStream publish/subscribe, stream management | pkg/config |
| **pkg/storage** | Object storage abstraction (local FS / S3) | pkg/config |
| **pkg/tlsconfig** | TLS setup, FIPS validation, certificate loading | pkg/config |
| **pkg/authn** | mTLS, Kerberos, SAML authentication | pkg/tlsconfig, pkg/tenant |
| **pkg/tenant** | Multi-tenant context propagation, isolation enforcement | None (foundational) |
| **pkg/telemetry** | OpenTelemetry traces, metrics, logs | pkg/config |
| **pkg/llmclient** | LLM gateway client, completion & embedding requests | pkg/config, pkg/telemetry |
| **pkg/oscal** | OSCAL catalog parsing, validation | None (domain logic) |
| **pkg/attestation** | in-toto layout, link generation, signing | pkg/tlsconfig |
| **pkg/analyzer** | Plugin interface for analysis capabilities | None (interface only) |

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

## Build & Task Automation

Use `task` (https://taskfile.dev) for all build operations:

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

## Next Steps

After scaffolding is complete, the next implementation phases are:

1. **Phase 1: Implement foundational packages** (`config`, `tenant`)
2. **Phase 2: Implement infrastructure packages** (`db`, `storage`, `natsbus`)
3. **Phase 3: Implement extended packages** (`graphdb`, `vectordb`, `tlsconfig`, `telemetry`)
4. **Phase 4: Implement security packages** (`authn`)
5. **Phase 5: Implement domain packages** (`oscal`, `attestation`, `analyzer`, `llmclient`)
6. **Phase 6: Implement first service** (Ingestion Service in `internal/ingestion`)

Refer to the issue tracker for detailed implementation plans for each phase.
