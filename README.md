# CrossCodex

A Go-first, multi-service compliance mapping platform that compares compliance standards, maps relationships between requirements, and stores the complete graph with full traceability.

CrossCodex delivers composable microservices, provider-agnostic LLM integration, and multi-tenant security with defense-in-depth.

______________________________________________________________________

> LLM WARNING: This project was written with LLM (AI) assistance.

______________________________________________________________________

## Status

CrossCodex is in early development. All foundational, domain, and service packages are implemented and tested. The CLI provides approximately 30 commands across project, catalog, run, results, prompt, version, and completion groups with daemon connectivity and embedded single-node mode. See [Development](#development) below to build from source and run tests.

## Quick Start

```sh
# Prerequisites: Go >= 1.26, Task (taskfile.dev), container engine (podman or docker)

# Build from source
task build

# Start the development database (PostgreSQL + AGE + pgvector)
task dev:up

# Configure the database connection (use the DSN printed by dev:up)
<!-- secretlint-disable-next-line @secretlint/secretlint-rule-database-connection-string -- documentation example with dev-only credentials (user: postgres, password: integration, host: localhost) -->
crosscodex config set database.dsn "postgres://postgres:integration@localhost:15432/crosscodex_test?sslmode=disable"

# Download official NIST OSCAL catalogs
task fetch:oscal-docs

# Initialize a project
crosscodex project init

# Import a compliance catalog
crosscodex catalog import catalogs/NIST_SP-800-53_rev5_catalog.json

# List imported catalogs
crosscodex catalog list

# Inspect a catalog
crosscodex catalog inspect <catalog-id>
```

Run `crosscodex --help` to see all available commands, or `crosscodex <command> --help` for command-specific usage.

## Architecture

The target architecture consists of seven core services that can run embedded in a single process or distributed across multiple hosts. Today the monorepo provides implemented infrastructure (`pkg/config`, `pkg/db`, `pkg/storage`, `pkg/natsbus`, `pkg/tlsconfig`, `pkg/authn`, `pkg/tenant`, `pkg/telemetry`, `pkg/llmclient`) and implemented domain packages (`pkg/analyzer`, `pkg/attestation`, `pkg/oscal`, `pkg/graphdb`, `pkg/vectordb`, `pkg/prompt`). Implemented services include `internal/worker` (LLM task execution), `internal/synthesis` (viability ranking and quality diagnostics), `internal/graph` (compliance relationship graph), `internal/analysis` (analysis engine), `internal/catalog` (OSCAL catalog management), `internal/gateway` (API gateway), `internal/pipeline` (job orchestration), and the analyzer plugins (`internal/analyzer/classify`, `internal/analyzer/embedding`, `internal/analyzer/relationship`).

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {
  'primaryColor': '#2f6dab',
  'primaryTextColor': '#1e1e1e',
  'primaryBorderColor': '#7c8ba1',
  'lineColor': '#7c8ba1',
  'edgeLabelBackground': '#eef2f8',
  'tertiaryColor': 'transparent',
  'tertiaryTextColor': '#7c8ba1',
  'tertiaryBorderColor': '#7c8ba1',
  'clusterBkg': 'transparent',
  'clusterBorder': '#7c8ba1',
  'titleColor': '#7c8ba1',
  'noteBkgColor': '#eef2f8',
  'noteTextColor': '#1e1e1e',
  'fontFamily': 'system-ui, sans-serif'
}, 'themeCSS': '.node .nodeLabel{color:#ffffff!important;fill:#ffffff!important;}'}}%%
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
    subgraph orchestration ["Orchestration"]
        synthesis["Synthesis<br/>Quality Ranking"]
        pipeline["Pipeline<br/>Go + NATS"]
    end
    subgraph datalayer ["Data Layer"]
        graphdb["Graph Store<br/>PostgreSQL + AGE"]
        vectordb["Vector Store<br/>pgvector"]
    end
    subgraph infra ["Infrastructure"]
        tenant["Tenant Isolation"]
        tls["TLS / mTLS"]
        authn["Authentication"]
        telemetry["Telemetry<br/>OpenTelemetry"]
        attestation["Attestation<br/>in-toto"]
    end
    docs --> ingestion --> catalog --> analysis --> llm
    llm --> synthesis --> graphdb
    pipeline --> analysis
    pipeline --> synthesis
    graphdb --> pipeline
    analysis --> vectordb
    infra -.-> processing
    infra -.-> orchestration
    infra -.-> datalayer

    classDef sysA fill:#2f6dab,color:#ffffff,stroke:#7c8ba1
    classDef sysB fill:#1d7848,color:#ffffff,stroke:#7c8ba1
    classDef sysC fill:#7457b8,color:#ffffff,stroke:#7c8ba1
    classDef sysD fill:#2d747e,color:#ffffff,stroke:#7c8ba1
    classDef sysF fill:#5c6a82,color:#ffffff,stroke:#7c8ba1
    class ingestion,catalog,analysis,llm sysA
    class synthesis,pipeline sysB
    class graphdb,vectordb sysC
    class tenant,tls,authn,telemetry,attestation sysD
    class docs sysF
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

- **Embedded** -- All services in one process with auto-bootstrapped mTLS. Requires PostgreSQL with AGE and pgvector extensions (start with `task dev:up`). Local filesystem for object storage. Catalog import, list, and inspect work for OSCAL JSON documents. Analysis pipeline execution requires additional service backends (see issue #31).
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

Additional XDG directories:
  $XDG_DATA_HOME/crosscodex/
    prompts/                     # User prompt template layers
  $XDG_STATE_HOME/crosscodex/
    pki/                         # Embedded daemon auto-generated TLS certificates
    daemon.pid                   # Embedded daemon PID and port
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

# Analysis
analysis:
  classification:
    enabled: true
    model: ""                     # Inherits from llm.default_model
    max_text_length: 2000
    temperature: 0.0
    max_tokens: 20
  embedding:
    enabled: true
    models: ["snowflake-arctic-embed2"]  # Embedding model names
    max_chars: 1500                      # Max runes before truncation
    batch_size: 50                       # Controls per batch call
  relationship:
    top_k: 20                            # Most-similar pairs to retain

synthesis:
  confidence_threshold: 0.5               # Minimum confidence for viable mappings
  max_mappings_per_control: 10            # Maximum mappings per source control
  viability:
    type_mismatch_factor: 0.8             # Penalty for different classification types
    skip_level_factor: 0.7                # Penalty for non-adjacent abstraction levels
    integral_to_factor: 1.1               # Boost for INTEGRAL_TO contribution type
  assessment:
    iqr_good: 20.0                        # IQR threshold for good embedding spread
    iqr_poor: 10.0                        # IQR threshold for poor embedding spread
    no_rel_high: 0.97                     # NO_RELATIONSHIP rate upper bound
    no_rel_low: 0.80                      # NO_RELATIONSHIP rate lower bound
    contested_warn: 0.20                  # Contested pairs warning threshold
    actionable_warn: 0.30                 # Actionable coverage warning threshold
```

### Environment Variables

The CLI recognizes these environment variables:

| Variable              | Purpose                                    | Default              |
|-----------------------|--------------------------------------------|----------------------|
| `CROSSCODEX_ENDPOINT` | daemon address                        | `localhost:50051`    |
| `CROSSCODEX_COLOR`    | Force color output (`1`) or disable (`0`)  | Auto-detect (isatty) |
| `CROSSCODEX_LOGLEVEL` | Log level (`debug`/`info`/`warn`/`error`)  | `warn`               |
| `NO_COLOR`            | Disable color output (standard convention) | —                    |

### Verbosity

Persistent flags override the configured log level for a single invocation:

- `-v`, `--verbose` — set log level to `info` (repeat `-vv` for `debug`)
- `--debug` — set log level to `debug` (implies `--verbose`; wins if combined)

Precedence (highest first): `--debug`/`-vv` → `-v` → `logging.level`
(config file or `CROSSCODEX_LOGLEVEL`) → default `warn`. The resolved level
applies to both the CLI and the auto-started embedded daemon.

## Development

### Repository Structure

CrossCodex uses a Go monorepo with separate repositories for Python ingestion and TypeScript UI:

```
crosscodex/                      # Main monorepo
  api/proto/                     # Protobuf definitions
  pkg/                           # Public SDK packages
  cmd/                           # CLI and daemon binaries
  internal/                      # Service implementations
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

| Test Type       | Framework               | Status                                  |
|-----------------|-------------------------|-----------------------------------------|
| **Unit**        | Ginkgo/Gomega (BDD)     | Available (`task test:unit`)            |
| **Integration** | Go testing + containers | Available (`task test:integration:all`) |
| **E2E**         | Venom                   | Planned                                 |

#### Stress Tests (#112)

Gateway stress tests verify concurrent upload handling, resource limits, data integrity, and throughput under load.

```bash
# Run in-process stress tests
task test:stress        # alias for test:stress:unit
task test:stress:unit

# Run throughput benchmark
task test:stress:bench

# Run full-stack round-trip integration suite
task test:stress:e2e
```

**Build tag:** Stress tests compile only under `-tags stress` and are excluded from the default `task test` / `go build ./...` path.

**E2E requirements:** `task test:stress:e2e` requires a podman runtime (rootless mode). The taskfile sets `TMPDIR=/workspace/.test-output/buildtmp` for build scratch. The container storage driver is taken from your podman configuration (`storage.conf`), matching the rest of the integration suite; if your environment has no overlay support, select vfs via `storage.conf` or `STORAGE_DRIVER=vfs` rather than relying on the taskfile.

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
| **Services**     | Request metadata validation                     | Context propagation   |
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

- **FIPS build**: BoringCrypto, approved cipher suites only (container image planned — Red Hat UBI base)
- **Standard build**: Go stdlib crypto (container image planned — distroless base)
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

## Uninstall

To fully remove CrossCodex, delete the binary and all data directories:

```sh
# Remove the binary (location depends on your install method)
rm $(which crosscodex)

# Remove configuration
rm -rf "${XDG_CONFIG_HOME:-$HOME/.config}/crosscodex"

# Remove data (prompt layers)
rm -rf "${XDG_DATA_HOME:-$HOME/.local/share}/crosscodex"

# Remove state (daemon PID, embedded TLS certificates)
rm -rf "${XDG_STATE_HOME:-$HOME/.local/state}/crosscodex"

# Remove project-level configuration (per project)
rm -rf .crosscodex/

# Remove shell completions (if installed)
rm -f ~/.bash_completion.d/crosscodex
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
