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

## Status

CrossCodex is in early development. The Go monorepo provides package interfaces, protobuf service contracts, and a configuration system. The CLI binary builds but does not yet implement user-facing commands. See [Development](#development) below to build from source and run tests.

## Architecture

The target architecture consists of seven core services that can run embedded in a single process or distributed across multiple hosts. Today the monorepo provides package-level foundations (`pkg/oscal`, `pkg/analyzer`, `pkg/llmclient`, `pkg/graphdb`, `pkg/natsbus`); full service implementations are not yet built.

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
  internal/                      # Service implementations (planned)
  deploy/                        # Deployment manifests (planned)
  tests/                         # Integration and E2E tests (planned)
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

| Test Type | Framework | Status |
|-----------|-----------|--------|
| **Unit** | Go testing | Available (`task test:unit`) |
| **Integration** | Go testing + containers | Planned |
| **E2E** | Venom | Planned |

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
