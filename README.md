# CrossCodex

A Go-first, multi-service compliance mapping platform that compares compliance standards, maps relationships between requirements, and stores the complete graph with full traceability.

CrossCodex delivers composable microservices, provider-agnostic LLM integration, and multi-tenant security with defense-in-depth.

______________________________________________________________________

> 🤖 LLM WARNING 🤖
>
> This project was written with LLM (AI) assistance.
>
> 🤖 LLM WARNING 🤖

______________________________________________________________________

## Status

CrossCodex is in early development. Six foundational packages are implemented and tested, protobuf service contracts define the inter-service API, and the CLI binary builds but does not yet implement user-facing commands. See [Development](#development) below to build from source and run tests.

| Package           | Status      | Summary                                                                                      |
|-------------------|-------------|----------------------------------------------------------------------------------------------|
| **pkg/config**    | Implemented | XDG 9-layer configuration merge, YAML loading, validation with source tracking               |
| **pkg/storage**   | Implemented | Local filesystem and S3 object storage with tenant isolation, atomic writes                  |
| **pkg/db**        | Implemented | PostgreSQL connection pool with tenant RLS, schema migrations, extension verification        |
| **pkg/natsbus**   | Implemented | Dual-mode NATS client (embedded + external), tenant-scoped subjects, JetStream audit streams |
| **pkg/tlsconfig** | Implemented | Shared TLS config builder with FIPS enforcement, config merging, cert reload, dev PKI        |
| **pkg/authn**     | Implemented | X.509 mTLS authentication, registry dispatch, audit emission; Kerberos/SAML stubbed         |
| **pkg/tenant**    | Partial     | Tenant ID validation implemented; context propagation interface scaffolded                   |
| All others        | Scaffolded  | Interfaces and types defined; implementation pending                                         |

Unit tests cover the implemented packages. Integration tests for `pkg/db`, `pkg/storage`, `pkg/natsbus`, and `pkg/authn` run against containerized services (`task test:integration:all`).

## Implemented Packages

### pkg/config

Configuration loading with XDG Base Directory compliance. Merges values from nine layers in defined precedence order:
compiled defaults, system config, system drop-ins, user config, user drop-ins, profile selection, project config,
environment variables, and CLI flags. Each resolved value carries provenance metadata indicating which layer set it.
Validation runs after merge and reports all errors with source locations.

See [Configuration](#configuration) below for the full resolution order and examples.

### pkg/storage

Object storage abstraction supporting local filesystem and S3-compatible backends. Both backends enforce tenant isolation through path prefixing and validate paths to prevent directory traversal and symlink attacks. Writes are atomic (write-to-temp then rename) to prevent partial artifacts. The S3 backend supports configurable endpoints for MinIO or other S3-compatible services.

### pkg/db

PostgreSQL connection pool with tenant-scoped Row-Level Security. The package provides three main components:

- **Pool** — Connection pool built on `database/sql` with health checks and PostgreSQL extension verification (AGE, pgvector).
- **TenantPool** — Wraps a Pool to automatically set `app.current_tenant` (and optionally `app.current_user`) via `SET LOCAL` on every transaction. Direct queries outside a transaction are rejected.
- **Migrator** — Schema migrations using `golang-migrate/migrate/v4` with SQL files embedded via `//go:embed`. Migrations create tables, RLS policies, immutability triggers, and tenant-scoped graph lifecycle functions.

Application connections use a restricted `app_user` role with no DDL privileges. The migration role (superuser) is used only for schema changes. See `docs/dev/migrations.md` for operational details.

### pkg/authn

Multi-method authentication with registry-based dispatch. The Registry holds an ordered list of Authenticator implementations and tries each in sequence -- `ErrUnsupportedMethod` means "try next," any other error stops iteration.

The X.509 mTLS authenticator maps client certificate fields (CN, Organization, OrgUnit, SAN Email/DNS/URI) to tenant identities using glob patterns. In single-tenant mode, any valid client certificate receives the default tenant and admin role. In multi-tenant mode, ordered mapping rules determine tenant and role assignment (first match wins).

Authentication events are emitted via the `AuditEmitter` interface for audit logging. The package never imports `pkg/natsbus` directly -- the gateway provides the natsbus-backed implementation.

## Architecture

The target architecture consists of seven core services that can run embedded in a single process or distributed across multiple hosts. Today the monorepo provides implemented infrastructure (`pkg/config`, `pkg/db`, `pkg/storage`, `pkg/natsbus`, `pkg/tlsconfig`, `pkg/authn`) and scaffolded domain packages (`pkg/oscal`, `pkg/analyzer`, `pkg/llmclient`, `pkg/graphdb`); full service implementations are not yet built.

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {
  'primaryColor': '#3e6fa0',
  'primaryTextColor': '#333333',
  'primaryBorderColor': '#7c8ba1',
  'lineColor': '#7c8ba1',
  'edgeLabelBackground': '#f5f5f5',
  'clusterBkg': '#e8e8e8',
  'clusterBorder': '#7c8ba1',
  'transitionColor': '#333333',
  'transitionLabelColor': '#333333',
  'stateLabelColor': '#333333',
  'fontFamily': 'system-ui, sans-serif'
}}}%%
flowchart TD
    subgraph external ["External Sources"]
        docs["Documents<br/>PDF, DOCX, HTML"]
    end

    subgraph processing ["Processing Pipeline"]
        ingestion["Ingestion Service<br/>Python + Docling"]
        catalog["Catalog Service<br/>Go + OSCAL"]
        analysis["Analysis Engine<br/>Go + Plugins"]
        llm["LLM Workers<br/>Go + AI Models"]
    end

    subgraph datalayer ["Data Layer"]
        synthesis["Synthesis<br/>Quality Ranking"]
        graphdb["Graph Store<br/>PostgreSQL + AGE"]
        pipeline["Pipeline<br/>Go + NATS"]
    end

    docs --> ingestion
    ingestion --> catalog
    catalog --> analysis
    analysis --> llm
    llm --> synthesis
    synthesis --> graphdb
    pipeline --> analysis
    pipeline --> synthesis
    graphdb --> pipeline

    classDef sysA fill:#3e6fa0,color:#ffffff,stroke:#7c8ba1
    classDef sysB fill:#3a8054,color:#ffffff,stroke:#7c8ba1
    classDef sysC fill:#2d747e,color:#ffffff,stroke:#7c8ba1
    class docs sysA
    class ingestion,catalog,analysis,llm sysB
    class synthesis,graphdb,pipeline sysC

```

### Service Responsibilities

| Service             | Purpose                                         | Technology          |
|---------------------|-------------------------------------------------|---------------------|
| **Ingestion**       | Multi-format document conversion via Docling    | Python gRPC service |
| **Catalog**         | OSCAL parsing, document structuring, validation | Go                  |
| **Analysis Engine** | Host for analyzer plugins, DAG execution        | Go                  |
| **LLM Workers**     | Horizontally scalable LLM task execution        | Go                  |
| **Synthesis**       | Ranking, viability weighting, quality metrics   | Go                  |
| **Graph**           | openCypher queries via Apache AGE on PostgreSQL | Go                  |
| **Pipeline**        | Job orchestration, state tracking, retry logic  | Go                  |

### Analyzer Plugin Architecture

Analysis capabilities will be implemented as independent analyzers that register with the Analysis Engine. The `Analyzer` interface is defined in `pkg/analyzer/`; planned analyzers include:

- `classify` - Control type and level classification
- `embedding` - Vector embedding generation and similarity
- `relationship` - LLM panel voting on relationship types
- `requires` - Multi-pass prerequisite detection
- `artifacts` - Observable artifact extraction with deduplication

Adding new analysis capabilities requires only implementing the Analyzer interface — no modifications to existing services.

## Deployment Modes

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {
  'primaryColor': '#3e6fa0',
  'primaryTextColor': '#333333',
  'primaryBorderColor': '#7c8ba1',
  'lineColor': '#7c8ba1',
  'edgeLabelBackground': '#f5f5f5',
  'clusterBkg': '#e8e8e8',
  'clusterBorder': '#7c8ba1',
  'transitionColor': '#333333',
  'transitionLabelColor': '#333333',
  'stateLabelColor': '#333333',
  'fontFamily': 'system-ui, sans-serif'
}}}%%
stateDiagram-v2
    [*] --> Embedded
    Embedded --> Quadlet : Team grows
    Quadlet --> Distributed : Production scale

    Embedded : All services in one process
    Embedded : PostgreSQL in container
    Embedded : Local filesystem storage

    Quadlet : Systemd-managed containers
    Quadlet : Shared PostgreSQL / NATS / MinIO

    Distributed : Services scale independently
    Distributed : External PostgreSQL + NATS cluster
    Distributed : S3-compatible object storage
```

### Embedded (Laptop, CI)

- All services in one process
- PostgreSQL in container (auto-managed)
- Local filesystem storage
- Zero external dependencies beyond LLM endpoint

### Quadlet (Small Team, Single Host)

- Systemd-managed containers via quadlet
- Shared PostgreSQL, NATS, MinIO
- Deployment manifests planned under `deploy/`

### Distributed (Production, Multi-tenant)

- Services scale independently
- External PostgreSQL cluster with AGE + pgvector
- NATS cluster with JetStream
- S3-compatible object storage

## Configuration

CrossCodex follows XDG Base Directory conventions:

```
$XDG_CONFIG_HOME/crosscodex/
  config.yaml                    # User-level defaults
  profiles/
    local.yaml                   # Single-node overrides  
    distributed.yaml             # Cluster overrides
  credentials/                   # API keys, certificates (mode 0600)
  tenants/                       # Per-tenant configuration

Project directory:
  .crosscodex/
    config.yaml                  # Project-specific overrides
    prompts/                     # Custom prompt templates
```

### Configuration Resolution Order

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {
  'primaryColor': '#3e6fa0',
  'primaryTextColor': '#333333',
  'primaryBorderColor': '#7c8ba1',
  'lineColor': '#7c8ba1',
  'edgeLabelBackground': '#f5f5f5',
  'clusterBkg': '#e8e8e8',
  'clusterBorder': '#7c8ba1',
  'transitionColor': '#333333',
  'transitionLabelColor': '#333333',
  'stateLabelColor': '#333333',
  'fontFamily': 'system-ui, sans-serif'
}}}%%
flowchart TD
    subgraph system ["System-wide"]
        defaults["Compiled defaults"]
        syscfg["/etc/crosscodex/config.yaml"]
        sysdrop["/etc/crosscodex/conf.d/*.yaml"]
    end
    subgraph user ["User-level"]
        usercfg["$XDG_CONFIG_HOME/.../config.yaml"]
        userdrop["$XDG_CONFIG_HOME/.../conf.d/*.yaml"]
        profile["--profile selection"]
    end
    subgraph runtime ["Runtime"]
        projcfg[".crosscodex/config.yaml"]
        envvars["CROSSCODEX_* env vars"]
        cliflags["CLI flags"]
    end

    defaults --> syscfg
    syscfg --> sysdrop
    sysdrop --> usercfg
    usercfg --> userdrop
    userdrop --> profile
    profile --> projcfg
    projcfg --> envvars
    envvars --> cliflags

    classDef sysA fill:#3e6fa0,color:#ffffff,stroke:#7c8ba1
    classDef sysB fill:#3a8054,color:#ffffff,stroke:#7c8ba1
    classDef sysC fill:#a55726,color:#ffffff,stroke:#7c8ba1
    class defaults,syscfg,sysdrop sysA
    class usercfg,userdrop,profile sysB
    class projcfg,envvars,cliflags sysC
```

1. Compiled defaults
1. System config (`/etc/crosscodex/config.yaml`)
1. System drop-ins (`/etc/crosscodex/conf.d/*.yaml`)
1. User config (`$XDG_CONFIG_HOME/crosscodex/config.yaml`)
1. User drop-ins (`$XDG_CONFIG_HOME/crosscodex/conf.d/*.yaml`)
1. Profile (`--profile local`)
1. Project config (`.crosscodex/config.yaml`)
1. Environment variables (`CROSSCODEX_*`)
1. CLI flags (highest priority)

### Key Configuration Examples

#### LLM Gateway

```yaml
llm:
  gateway_url: "http://localhost:4000"
  default_model: "qwen3:8b"
  embedding_model: "qwen3-embedding"
  timeout: 30
```

#### Storage

```yaml
storage:
  objects:
    backend: local                # local | s3
```

#### Database

```yaml
database:
  dsn: "${DATABASE_DSN}"          # e.g. postgres://user:password@localhost:5432/crosscodex
  extensions: [age, vector]
```

#### TLS (Global Default)

```yaml
tls:
  mode: "mutual"                  # off | server-only | mutual
  ca: /etc/crosscodex/tls/ca.crt
  cert: /etc/crosscodex/tls/server.crt
  key: /etc/crosscodex/tls/server.key
```

#### Authentication

```yaml
# Multi-tenant X.509 certificate-to-tenant mapping
tenants:
  enabled: true
auth:
  x509_mappings:
    - match:
        organization: "Acme*"
        org_unit: "Engineering"
      tenant: acme-engineering
      roles: [admin, writer]
    - match:
        san_email: "*@partner.com"
      tenant: partner-org
      roles: [reader]
```

## Development

### Repository Structure

CrossCodex uses a Go monorepo with separate repositories for Python ingestion and TypeScript UI:

```
crosscodex/                      # Main monorepo
  api/proto/                     # Protobuf definitions
  pkg/                           # Public SDK packages
  cmd/                           # CLI and daemon binaries
  internal/                      # Service implementations (planned)
  deploy/                        # Deployment manifests (planned)
```

### Build Commands

```bash
# Install Taskfile if not present
curl -sL https://taskfile.dev/install.sh | sh

# Build all binaries
task build

# Run all tests
task test

# Run unit tests only
task test:unit

# Lint
task lint

# Generate protobuf code
task generate
```

### Testing Strategy

| Test Type       | Framework               | Status                                                                           |
|-----------------|-------------------------|----------------------------------------------------------------------------------|
| **Unit**        | Ginkgo/Gomega (BDD)     | Available (`task test:unit`)                                                     |
| **Integration** | Go testing + containers | Available for pkg/db, pkg/storage, pkg/natsbus, and pkg/authn (`task test:integration:all`) |
| **E2E**         | Venom                   | Planned                                                                          |

### Contributing

1. **Fork and clone** the repository
1. **Create feature branch** from main
1. **Write tests** for new functionality (TDD approach)
1. **Implement** following existing patterns
1. **Run full test suite** before submitting
1. **Submit PR** with clear description

For large features, open an issue first to discuss the approach.

## CI Security

All GitHub Actions workflows follow least-privilege principles:

- Default to no permissions (`permissions: {}`) with explicit per-job grants
- MegaLinter runs actionlint and other YAML-aware linters for workflow validation

## Security & Compliance

### Multi-tenant Isolation (Defense-in-Depth)

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {
  'primaryColor': '#3e6fa0',
  'primaryTextColor': '#333333',
  'primaryBorderColor': '#7c8ba1',
  'lineColor': '#7c8ba1',
  'edgeLabelBackground': '#f5f5f5',
  'clusterBkg': '#e8e8e8',
  'clusterBorder': '#7c8ba1',
  'transitionColor': '#333333',
  'transitionLabelColor': '#333333',
  'stateLabelColor': '#333333',
  'fontFamily': 'system-ui, sans-serif'
}}}%%
flowchart TD
    req["Tenant Request"]
    gw["Gateway<br/>mTLS, JWT, RBAC"]
    svc["Services<br/>gRPC metadata validation"]
    nats["NATS<br/>Tenant-scoped subjects + ACLs"]
    pg["PostgreSQL<br/>Row-Level Security"]
    obj["Object Store<br/>Tenant-prefixed paths"]
    age["Graph / AGE<br/>Separate graph per tenant"]

    req --> gw
    gw -->|identity verified| svc
    svc --> nats
    svc --> pg
    svc --> obj
    svc --> age

    classDef sysA fill:#3e6fa0,color:#ffffff,stroke:#7c8ba1
    classDef sysB fill:#3a8054,color:#ffffff,stroke:#7c8ba1
    classDef sysC fill:#7457b8,color:#ffffff,stroke:#7c8ba1
    class req sysA
    class gw,svc sysB
    class nats,pg,obj,age sysC
```

Every layer enforces tenant isolation independently:

| Layer            | Mechanism                                    | Purpose               |
|------------------|----------------------------------------------|-----------------------|
| **Gateway**      | mTLS client certificates, JWT sessions, RBAC | Identity verification |
| **Services**     | gRPC metadata validation                     | Context propagation   |
| **NATS**         | Tenant-scoped subjects and ACLs              | Message isolation     |
| **PostgreSQL**   | Row-Level Security policies                  | Data isolation        |
| **Object Store** | Tenant-prefixed paths, bucket policies       | Artifact isolation    |
| **Graph (AGE)**  | Separate graph per tenant                    | Traversal isolation   |

### Authentication Methods

| Method                | Use Case                            | How It Works                            |
|-----------------------|-------------------------------------|-----------------------------------------|
| **X.509 (mTLS)**      | CLI, service-to-service, automation | Client certificate during TLS handshake |
| **GSSAPI (Kerberos)** | Enterprise SSO, Active Directory    | Kerberos ticket via SPNEGO              |
| **SAML**              | Web UI, browser SSO                 | SAML assertion from IdP                 |

### FIPS 140 Support

CrossCodex supports dual builds (standard and FIPS) from the same source:

- **FIPS build**: Red Hat UBI base images, BoringCrypto, approved cipher suites only
- **Standard build**: Distroless images, Go stdlib crypto
- **Runtime enforcement**: `tls.fips.enabled: true` validates FIPS compliance

### Cryptographic Attestation

Pipeline outputs include in-toto attestation for audit trails:

- **Layout**: Signed by Pipeline service declaring authorized stages and functionaries
- **Links**: Per-stage attestations with input/output hashes, model versions, environment
- **Verification**: Independent validation via `crosscodex results verify` or in-toto CLI

## Storage Architecture

### Unified Database Strategy

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {
  'primaryColor': '#3e6fa0',
  'primaryTextColor': '#333333',
  'primaryBorderColor': '#7c8ba1',
  'lineColor': '#7c8ba1',
  'edgeLabelBackground': '#f5f5f5',
  'clusterBkg': '#e8e8e8',
  'clusterBorder': '#7c8ba1',
  'transitionColor': '#333333',
  'transitionLabelColor': '#333333',
  'stateLabelColor': '#333333',
  'fontFamily': 'system-ui, sans-serif'
}}}%%
erDiagram
    PostgreSQL ||--|| RelationalStore : "core engine"
    PostgreSQL ||--|| GraphStore : "Apache AGE"
    PostgreSQL ||--|| VectorStore : "pgvector"
    RelationalStore ||--o{ Job : tracks
    RelationalStore ||--o{ Catalog : stores
    RelationalStore ||--o{ Classification : records
    RelationalStore ||--o{ TenantConfig : isolates
    GraphStore ||--o{ Relationship : maps
    VectorStore ||--o{ Embedding : indexes
    Catalog ||--o{ Relationship : links
    Classification ||--o{ Embedding : generates
```

PostgreSQL with extensions handles all data:

| Store          | Extension  | Purpose                                                     |
|----------------|------------|-------------------------------------------------------------|
| **Relational** | PostgreSQL | Job metadata, catalogs, classifications, tenant config      |
| **Graph**      | Apache AGE | Relationship graph, openCypher queries, temporal attributes |
| **Vector**     | pgvector   | Embedding similarity search                                 |

### Additional Storage

| Store            | Technology     | Purpose                                                |
|------------------|----------------|--------------------------------------------------------|
| **Object Store** | Local FS / S3  | Documents, embeddings, attestation bundles             |
| **Message Bus**  | NATS JetStream | Audit trails, work distribution, service communication |

### Why PostgreSQL Everywhere

- Single database engine reduces operational complexity
- Row-Level Security enforces tenant isolation
- Shared connection pools and transactions
- Standard tooling (pg_dump, pgAdmin, managed services)
- AGE provides openCypher compatibility for graph queries

## Observability

### OpenTelemetry Integration

Built-in observability with OTLP export:

- **Traces**: Span per stage, span per LLM call, cross-service correlation
- **Metrics**: Job duration, LLM latency, worker utilization, queue depth
- **Logs**: Structured logging correlated to trace IDs

### Audit Trails

JetStream provides persistent audit streams:

| Stream        | Retention  | Content                                 |
|---------------|------------|-----------------------------------------|
| **Decisions** | Indefinite | Final compliance determinations         |
| **LLM Calls** | 90 days    | Full prompts, responses, model versions |
| **Events**    | 30 days    | Pipeline lifecycle, debugging           |

### Monitoring (Planned)

Once the CLI is implemented, these commands will be available:

```bash
# Health check all services (planned)
crosscodexd admin health

# View job status (planned)
crosscodex run status <job-id>

# Export traces (works with any OTLP-compatible backend)
export OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4317
```

- **Issues**: [github.com/complytime-labs/crosscodex/issues](https://github.com/complytime-labs/crosscodex/issues)
- **Discussions**: [github.com/complytime-labs/crosscodex/discussions](https://github.com/complytime-labs/crosscodex/discussions)
- **License**: [Apache 2.0](./LICENSE)

### Related Projects

- [CrossCodex Ingestion](https://github.com/complytime-labs/crosscodex-ingestion) - Python document conversion service
- [CrossCodex UI](https://github.com/complytime-labs/crosscodex-ui) - React web interface
- [Docling](https://github.com/DS4SD/docling) - Document extraction library
- [Apache AGE](https://github.com/apache/age) - Graph extension for PostgreSQL
- [NATS](https://nats.io/) - Cloud native messaging system
- [in-toto](https://in-toto.io/) - Supply chain attestation framework
