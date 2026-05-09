# CrossCodex

A Go-first, multi-service compliance mapping platform that compares compliance standards, maps relationships between requirements, and stores the complete graph with full traceability.

CrossCodex delivers composable microservices, provider-agnostic LLM integration, and multi-tenant security with defense-in-depth.

----

> 🤖 LLM WARNING 🤖
>
> This project was written with LLM (AI) assistance.
>
> 🤖 LLM WARNING 🤖

----

## Quick Start

### Embedded Mode (Single User)

```bash
# Install CrossCodex CLI
curl -sSL https://github.com/complytime-labs/crosscodex/releases/latest/download/install.sh | sh

# Initialize a project
crosscodex project init my-compliance-analysis
cd my-compliance-analysis

# Import compliance catalogs
crosscodex catalog import --url https://example.com/nist-800-53.json --name "NIST 800-53"
crosscodex catalog import --url https://example.com/iso27001.json --name "ISO 27001"

# Run semantic analysis
crosscodex run start --source "NIST 800-53" --target "ISO 27001"

# View results
crosscodex results summary
```

The CLI automatically starts an embedded daemon on first use - no additional setup required.

## Architecture

CrossCodex consists of seven core services that can run embedded in a single process or distributed across multiple hosts:

```mermaid
flowchart TB
    subgraph external ["External Sources"]
        docs["Documents<br/>PDF, DOCX, HTML"]
    end

    subgraph processing ["Processing Pipeline"]
        direction TB
        ingestion["Ingestion Service<br/>Python + Docling"]
        catalog["Catalog Service<br/>Go + OSCAL"]
        analysis["Analysis Engine<br/>Go + Plugins"]
        llm["LLM Workers<br/>Go + AI Models"]
    end

    subgraph datalayer ["Data Layer"]
        direction LR
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

```

### Service Responsibilities

| Service | Purpose | Technology |
|---------|---------|------------|
| **Ingestion** | Multi-format document conversion via Docling | Python gRPC service |
| **Catalog** | OSCAL parsing, document structuring, validation | Go |
| **Analysis Engine** | Host for analyzer plugins, DAG execution | Go |
| **LLM Workers** | Horizontally scalable LLM task execution | Go |
| **Synthesis** | Ranking, viability weighting, quality metrics | Go |
| **Graph** | openCypher queries via Apache AGE on PostgreSQL | Go |
| **Pipeline** | Job orchestration, state tracking, retry logic | Go |

### Analyzer Plugin Architecture

Analysis capabilities are implemented as independent analyzers that register with the Analysis Engine:

- `classify` - Control type and level classification
- `embedding` - Vector embedding generation and similarity
- `relationship` - LLM panel voting on NIST IR 8477 relationship types
- `requires` - Multi-pass prerequisite detection  
- `artifacts` - Observable artifact extraction with deduplication

Adding new analysis capabilities requires only implementing the Analyzer interface - no modifications to existing services.

## Deployment Modes

### Embedded (Laptop, CI)
- All services in one process
- PostgreSQL in container (auto-managed)
- Local filesystem storage
- Zero external dependencies beyond LLM endpoint

### Quadlet (Small Team, Single Host)
- Systemd-managed containers via quadlet
- Shared PostgreSQL, NATS, MinIO
- `deploy/quadlet/` unit files + `deploy/kustomize/overlays/`

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

1. Compiled defaults
2. System config (`/etc/crosscodex/config.yaml`)
3. System drop-ins (`/etc/crosscodex/conf.d/*.yaml`)
4. User config (`$XDG_CONFIG_HOME/crosscodex/config.yaml`)
5. User drop-ins (`$XDG_CONFIG_HOME/crosscodex/conf.d/*.yaml`)
6. Profile (`--profile local`)
7. Project config (`.crosscodex/config.yaml`)
8. Environment variables (`CROSSCODEX_*`)
9. CLI flags (highest priority)

### Key Configuration Examples

#### LLM Gateway
```yaml
llm:
  gateway_url: "http://localhost:4000"
  default_model: "qwen3:8b"
  embedding_model: "qwen3-embedding"
  timeout: 30s
```

#### Storage
```yaml
storage:
  objects:
    backend: local                # local | s3
  database:
    postgres:
      dsn: "postgres://crosscodex:crosscodex@localhost:5432/crosscodex"
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

## Development

### Repository Structure

CrossCodex uses a Go monorepo with separate repositories for Python ingestion and TypeScript UI:

```
crosscodex/                      # Main monorepo
  api/proto/                     # Protobuf definitions
  pkg/                           # Public SDK packages
  cmd/                           # CLI and daemon binaries
  internal/                      # Service implementations
  deploy/                        # Quadlet units, kustomize manifests, skaffold config
  tests/                         # Integration and E2E tests
```

### Build Commands

```bash
# Install Taskfile if not present
curl -sL https://taskfile.dev/install.sh | sh

# Build all binaries
task build

# Run unit tests
task test:unit

# Run integration tests (requires containers)
task test:integration:embedded
task test:integration:distributed

# Build container images
task build:images

# Format and lint
task fmt
task lint
```

### Testing Strategy

| Test Type | Framework | Scope |
|-----------|-----------|-------|
| **Unit** | Go testing | Individual packages, mocked dependencies |
| **Integration** | Go testing + containers | Service interactions, real database |
| **E2E** | Venom | Full pipeline, user scenarios |

Integration tests use quadlet-managed containers (via skaffold) to spin up real infrastructure (PostgreSQL, NATS, MinIO) and test against actual service endpoints.

### Contributing

1. **Fork and clone** the repository
2. **Create feature branch** from main
3. **Write tests** for new functionality (TDD approach)
4. **Implement** following existing patterns
5. **Run full test suite** before submitting
6. **Submit PR** with clear description

For large features, open an issue first to discuss the approach.

## Security & Compliance

### Multi-tenant Isolation (Defense-in-Depth)

Every layer enforces tenant isolation independently:

| Layer | Mechanism | Purpose |
|-------|-----------|---------|
| **Gateway** | mTLS client certificates, JWT sessions, RBAC | Identity verification |
| **Services** | gRPC metadata validation | Context propagation |
| **NATS** | Tenant-scoped subjects and ACLs | Message isolation |  
| **PostgreSQL** | Row-Level Security policies | Data isolation |
| **Object Store** | Tenant-prefixed paths, bucket policies | Artifact isolation |
| **Graph (AGE)** | Separate graph per tenant | Traversal isolation |

### Authentication Methods

| Method | Use Case | How It Works |
|--------|----------|-------------|
| **X.509 (mTLS)** | CLI, service-to-service, automation | Client certificate during TLS handshake |
| **GSSAPI (Kerberos)** | Enterprise SSO, Active Directory | Kerberos ticket via SPNEGO |
| **SAML** | Web UI, browser SSO | SAML assertion from IdP |

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

PostgreSQL with extensions handles all data:

| Store          | Extension  | Purpose                                                     |
|----------------|------------|-------------------------------------------------------------|
| **Relational** | PostgreSQL | Job metadata, catalogs, classifications, tenant config      |
| **Graph**      | Apache AGE | Relationship graph, openCypher queries, temporal attributes |
| **Vector**     | pgvector   | Embedding similarity search                                 |

### Additional Storage

| Store | Technology | Purpose |
|-------|------------|---------|
| **Object Store** | Local FS / S3 | Documents, embeddings, attestation bundles |
| **Message Bus** | NATS JetStream | Audit trails, work distribution, service communication |

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

### Monitoring

```bash
# Health check all services
crosscodexd admin health

# View job status
crosscodex run status <job-id>

# Export traces
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
