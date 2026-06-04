# CrossCodex

A Go-first, multi-service compliance mapping platform that compares compliance standards, maps relationships between requirements, and stores the complete graph with full traceability.

CrossCodex delivers composable microservices, provider-agnostic LLM integration, and multi-tenant security with defense-in-depth.

______________________________________________________________________

> LLM WARNING: This project was written with LLM (AI) assistance.

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
| **pkg/authn**     | Implemented | X.509 mTLS authentication, registry dispatch, audit emission; Kerberos/SAML stubbed          |
| **pkg/tenant**    | Partial     | Tenant ID validation implemented; context propagation interface scaffolded                   |
| All others        | Scaffolded  | Interfaces and types defined; implementation pending                                         |

Unit tests cover the implemented packages. Integration tests for `pkg/db`, `pkg/storage`, `pkg/natsbus`, and `pkg/authn` run against containerized services (`task test:integration:all`).

## Architecture

The target architecture consists of seven core services that can run embedded in a single process or distributed across multiple hosts. Today the monorepo provides implemented infrastructure (`pkg/config`, `pkg/db`, `pkg/storage`, `pkg/natsbus`, `pkg/tlsconfig`, `pkg/authn`) and scaffolded domain packages (`pkg/oscal`, `pkg/analyzer`, `pkg/llmclient`, `pkg/graphdb`); full service implementations are not yet built.

```mermaid
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
    docs --> ingestion --> catalog --> analysis --> llm
    llm --> synthesis --> graphdb
    pipeline --> analysis & synthesis
    graphdb --> pipeline
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

### Deployment Modes

- **Embedded** -- All services in one process, PostgreSQL in container, local filesystem storage. Zero external dependencies beyond an LLM endpoint.
- **Quadlet** -- Systemd-managed containers with shared PostgreSQL, NATS, and MinIO. Deployment manifests planned under `deploy/`.
- **Distributed** -- Services scale independently with external PostgreSQL cluster (AGE + pgvector), NATS cluster with JetStream, and S3-compatible object storage.

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

Values merge in ascending priority (last wins):

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

```yaml
# LLM Gateway
llm:
  gateway_url: "http://localhost:4000"
  default_model: "qwen3:8b"
  timeout: 30

# Storage
storage:
  objects:
    backend: local                # local | s3

# Database
database:
  dsn: "${DATABASE_DSN}"          # e.g. postgres://user:password@localhost:5432/crosscodex
  extensions: [age, vector]

# TLS (Global Default)
tls:
  mode: "mutual"                  # off | server-only | mutual
  ca: /etc/crosscodex/tls/ca.crt
  cert: /etc/crosscodex/tls/server.crt
  key: /etc/crosscodex/tls/server.key

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

### Prerequisites

- **Go >= 1.26** -- see `go.mod` for exact version
- **Task** ([taskfile.dev](https://taskfile.dev)) -- install via `go install github.com/go-task/task/v3/cmd/task@latest`
- **Buf** ([buf.build](https://buf.build)) -- for protobuf code generation
- **Container engine** (podman or docker) -- for integration tests only

### Build Commands

```bash
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

Run `task --list` for all available commands including integration tests and development utilities.

### Testing Strategy

| Test Type       | Framework               | Status                                                                                      |
|-----------------|-------------------------|---------------------------------------------------------------------------------------------|
| **Unit**        | Ginkgo/Gomega (BDD)     | Available (`task test:unit`)                                                                |
| **Integration** | Go testing + containers | Available for pkg/db, pkg/storage, pkg/natsbus, and pkg/authn (`task test:integration:all`) |
| **E2E**         | Venom                   | Planned                                                                                     |

### Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for development workflow, PR process, and coding standards.

## CI Security

All GitHub Actions workflows follow least-privilege principles:

- Default to no permissions (`permissions: {}`) with explicit per-job grants
- MegaLinter runs actionlint and other YAML-aware linters for workflow validation

## Security & Compliance

### Multi-tenant Isolation (Defense-in-Depth)

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
- **Verification**: Independent validation via in-toto CLI (CrossCodex verify command planned)

See [Cryptographic Attestation Guide](docs/dev/attestation.md) for the attestation model, trace correlation, and implementation roadmap.

## Storage Architecture

PostgreSQL with extensions handles all data:

| Store          | Extension  | Purpose                                                     |
|----------------|------------|-------------------------------------------------------------|
| **Relational** | PostgreSQL | Job metadata, catalogs, classifications, tenant config      |
| **Graph**      | Apache AGE | Relationship graph, openCypher queries, temporal attributes |
| **Vector**     | pgvector   | Embedding similarity search                                 |

Additional storage:

| Store            | Technology     | Purpose                                                |
|------------------|----------------|--------------------------------------------------------|
| **Object Store** | Local FS / S3  | Documents, embeddings, attestation bundles             |
| **Message Bus**  | NATS JetStream | Audit trails, work distribution, service communication |

## Observability

### OpenTelemetry Integration

Built-in observability with OTLP export:

- **Traces**: Span per stage, span per LLM call, cross-service correlation
- **Metrics**: Job duration, LLM latency, worker utilization, queue depth
- **Logs**: Structured logging correlated to trace IDs

See [Telemetry Guide](docs/dev/telemetry.md) for configuration, Jaeger setup, metrics reference, and instrumentation status.

### Audit Trails

JetStream provides persistent audit streams:

| Stream        | Retention  | Content                                 |
|---------------|------------|-----------------------------------------|
| **Decisions** | Indefinite | Final compliance determinations         |
| **LLM Calls** | 90 days    | Full prompts, responses, model versions |
| **Events**    | 30 days    | Pipeline lifecycle, debugging           |

See [Audit Streams Guide](docs/dev/audit-streams.md) for provenance headers, message inspection, and trace correlation.

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
