# CrossCodex

A Go-first, multi-service compliance mapping platform that compares compliance standards, maps relationships between requirements, and stores the complete graph with full traceability.

CrossCodex delivers composable microservices, provider-agnostic LLM integration, and multi-tenant security with defense-in-depth.

______________________________________________________________________

> LLM WARNING: This project was written with LLM (AI) assistance.

______________________________________________________________________

## Status

CrossCodex is in early development. All foundational, domain, and service packages are implemented and tested. The CLI provides approximately 22 commands across project, catalog, run, results, prompt, version, and completion groups with daemon connectivity and embedded single-node mode. See [Development](#development) below to build from source and run tests.

## Upgrading

The graph storage hardening in #148 makes these breaking changes:

- Tenant IDs are limited to 52 characters (3-52 lowercase letters, digits and hyphens). Existing longer tenants stop working and must be re-provisioned under a new ID; see the "Tenant IDs longer than 52 characters" note linked below for the detection query and steps.
- Derived edge ID formats changed, so existing graphs do not deduplicate against newly written edges.
- Temporal graph properties (`valid_from`, `valid_to`, `analyzed_at`) use a new fixed-width encoding.
- Node and edge properties that use a reserved key (such as `id` or `valid_from`) are rejected.
- `BulkCreateEdges` rejects requests with more than `graph.max_bulk_edges` edges (default 1000).
- `CreateEdge` and `BulkCreateEdges` reject a `temporal.confidence` outside 0 to 1.
- The never-populated `BulkCreateEdgesResponse.errors` field (field 3) is removed and reserved; `BulkCreateEdges` reports failures only as the RPC status.
- The unused `tenants.allowed_tenants` config key is removed. A config file or `CROSSCODEX_TENANTS_ALLOWED_TENANTS` that still sets it is ignored; delete it.

Graphs written before this change must be rebuilt. Follow the "Upgrade note (#148)" in [docs/dev/design-principles.md](docs/dev/design-principles.md#graph-data-model).

The pre-release schema migrations are collapsed into one baseline, `001_initial_schema`. A database migrated by an earlier build cannot be upgraded; `crosscodexd` refuses it as newer than itself. Drop it (for the compose deployment, `task deploy:down VOLUMES=1`) and recreate it with `crosscodexd migrate`.

Tenant administration (#31):

- The gateway refuses every non-health request, with `PermissionDenied`, from a tenant whose `tenants` row is missing, suspended or deleted. The initial schema creates the `tenant_admin` role that `crosscodexd admin tenant` connects as, so tenants can only be provisioned after `crosscodexd migrate` runs. Once crosscodexd has migrated, set the role's password and `CROSSCODEX_DATABASE_TENANT_ADMIN_DSN`, then run `crosscodexd admin tenant create` for each authenticated tenant before sending it traffic; requests from that tenant are refused until then. Roles `all` and `gateway` refuse to start until `crosscodexd migrate` has provisioned `tenants.default_tenant`. See [Tenant administration](deploy/README.md#tenant-administration).

Identity lifecycle (#195):

- Tenants and SAME_AS verdicts can no longer be deleted by any role short of the database owner or a superuser deliberately disabling triggers (see [Identity Lifecycle](docs/dev/design-principles.md#identity-lifecycle)). Retire a tenant or reviewer with the soft-delete commands under [Tenant administration](#tenant-administration).
- `adjudicate artifacts decide` requires a registered reviewer; register reviewers with `crosscodexd admin reviewer add`.

## Quick Start

**Prerequisites**: Go >= 1.23, Task (taskfile.dev), container engine (podman or docker)

### 1. Build from source

```sh
task build
```

### 2. Start the development database

```sh
task dev:up
```

This starts PostgreSQL with AGE graph extension and pgvector.

### 3. Configure the database connection

The dev database requires mutual TLS. Copy the full DSN that `task dev:up` prints:

```sh
<!-- secretlint-disable-next-line @secretlint/secretlint-rule-database-connection-string -- documentation example with dev-only credentials (user: postgres, password: integration, host: localhost) -->
crosscodex config set database.dsn "postgres://postgres:integration@localhost:15432/crosscodex_test?sslmode=verify-full&sslrootcert=.test-output/certs/ca.pem&sslcert=.test-output/certs/client.pem&sslkey=.test-output/certs/client-key.pem"
```

### 4. Download OSCAL catalogs

```sh
task fetch:oscal-docs
```

This downloads official NIST OSCAL catalogs to the `catalogs/` directory.

### 5. Initialize a project

```sh
crosscodex project init
```

### 6. Import a compliance catalog

```sh
crosscodex catalog import catalogs/NIST_SP-800-53_rev5_catalog.json
```

### 7. List and inspect catalogs

```sh
# List all imported catalogs
crosscodex catalog list

# Inspect a specific catalog (use the ID from the list output)
crosscodex catalog inspect <catalog-id>
```

### 8. Read a job's mappings

Once a job has completed, read its control mappings. Both commands take the job ID and the same filters:

```sh
# Count mappings by relationship type
crosscodex results summary <job-id>

# Export viable synthesized mappings as proto JSON (enum names, all fields)
crosscodex results export <job-id> --output mappings.json

# Export one control's high-confidence EQUIVALENT mappings as CSV
crosscodex results export <job-id> --format csv --control <control-id> --type equivalent --min-confidence 0.8
```

| Flag | Applies to | Meaning |
|------|------------|---------|
| `--stage synthesized\|analyzer` | both | `synthesized` (default) is the output after synthesis's confidence threshold and per-control cap, with a viability score per mapping. `analyzer` is the raw relationship analyzer consensus, with no viability. |
| `--min-confidence <0-1>` | both | Only mappings at or above this confidence. |
| `--type <type>` | both | Only this relationship type; repeat for several. Case-insensitive. `NO_RELATIONSHIP` is excluded unless named here. |
| `--control <id>` | both | Only mappings whose source or target is this control. |
| `--include-non-viable` | both | Also return pairs synthesis filtered out (`synthesized` stage only). By default only viable mappings are returned. |
| `--format json\|csv` | `export` | Output format (default `json`). |
| `--output <file>` | `export` | Write to a file instead of stdout. |
| `--json` | `summary` | Structured output. |

A job that is still pending or running returns an error saying mappings are available once it completes. A failed or cancelled job never produces mappings; start a new job. An unknown job ID returns not found. `mapping_id` equals the ID of the mapping's `SEMANTIC_MATCH` graph edge. `REQUIRES` is not a mapping and is not returned.

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

- **Embedded** -- All services in one process with auto-bootstrapped mTLS. Requires PostgreSQL with AGE and pgvector extensions (start with `task dev:up`). Local filesystem for object storage. Catalog import, list, and inspect work for OSCAL JSON documents. Run `crosscodexd migrate` before starting `crosscodexd` against a database. `crosscodexd` (default `--role=all`) bootstraps a DB pool and NATS client and runs the graph, worker, and gateway/pipeline roles together in one process; analysis result persistence is durable end-to-end at the library layer (`internal/pipeline`, `pkg/analyzer`).
- **Production (compose)** -- Single-host production stack with full mutual TLS across all services (PostgreSQL, NATS, LiteLLM, CrossCodex gateway). One-command bring-up via `task deploy:up` after populating `deploy/.env` and generating certificates with `task deploy:certs`. The LiteLLM gateway runs behind a stunnel sidecar to provide end-to-end mTLS. See [deploy/README.md](deploy/README.md) for setup, first-use CLI commands, and operations.
- **Distributed** -- Services scale independently with external PostgreSQL cluster (AGE + pgvector), NATS cluster with JetStream, and S3-compatible object storage. Multi-host deployment and FIPS 140 container images tracked in issue [#17](https://github.com/complytime-labs/crosscodex/issues/17).

See [Service Runtime Guide](docs/dev/service-runtime.md) for the `--role` flag, role precedence, health-check wiring, and gateway startup preconditions.

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
  dsn: "${DATABASE_DSN}"          # app_user, e.g. postgres://app_user:example-password@localhost:5432/crosscodex
  migration_dsn: "${MIGRATION_DSN}"  # schema owner, used only by `crosscodexd migrate`, e.g. postgres://owner:example-password@localhost:5432/crosscodex
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
    models: ["snowflake-arctic-embed2"]  # Embedding model names; storage is model-agnostic (see docs/dev/embeddings.md), switching models needs no schema migration
    max_chars: 1500                      # Max runes before truncation
    batch_size: 50                       # Controls per batch call
  relationship:
    top_k: 20                            # Most-similar pairs to retain
  candidates:
    min_embedding_coverage: 0.8          # Minimum fraction of a job's controls that must have an embedding before candidate generation runs; below this, the candidate_generation stage fails the job (fail-closed)
    embed_model: "snowflake-arctic-embed2"  # Embedding model candidate generation reads; must be one of analysis.embedding.models
  artifact_adjudication:                 # `crosscodexd admin adjudicate artifacts`
    enabled: false                       # The command refuses to run until true
    models: []                           # Panel models; required when enabled
    samples_per_model: 3                 # Votes per model per pair; odd unless allow_even_samples
    allow_even_samples: false            # Allow an even samples_per_model
    sampling_temperature: 0.3            # [0, 2]; 0 is used with one sample per model
    max_tokens: 200                      # Max tokens per panel reply
    consensus_threshold: 0.67            # [0.5, 1]; below it (or on a split) the pair is undecided
    max_error_rate: 0.34                 # [0, 1]; more failed votes than this fails the panel (0 disables)
    max_attempts: 5                      # Failed panels before a pair is abandoned
    lease_duration: 15m                  # How long a run holds each leased pair, and each verdict it or `decide` projects; must be positive even when enabled is false
    lease_batch: 10                      # Pairs leased per round trip
    panel_concurrency: 4                 # Completions in flight per pair
    retry_base_delay: 1m                 # Wait after a pair's first failed panel, doubling per failure
    retry_max_delay: 24h                 # Cap on that wait; at least retry_base_delay

graph:
  max_bulk_edges: 1000                    # Most edges per BulkCreateEdges request, range [1, 10000]; larger requests are rejected (global-only)

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
| `CROSSCODEX_ENDPOINT` | daemon address                             | `localhost:50051`    |
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
  deploy/                        # Deployment manifests (compose stack, see deploy/README.md)
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
| **E2E**         | Venom                   | Available (`task test:e2e`)             |
| **Fuzz**        | Go native fuzzing       | Available (`task test:fuzz`)            |

#### Fuzz Tests

`task test:fuzz` fuzzes every `Fuzz*` target for `FUZZ_TIME` each (default `10s`). It runs one worker per CPU core, so expect it to occupy the machine. Narrow it with `PKGS`, for example `task test:fuzz PKGS=./pkg/backup FUZZ_TIME=1m`.

`task test` already replays every target's seed corpus and committed `testdata/fuzz/` inputs, so known crashers are regression-tested on every run. In CI, pull requests fuzz only the packages whose Go files they change (30s per target). The nightly `Fuzz (nightly)` workflow fuzzes every target for 2m each, uploads any new crash inputs as the `fuzz-crashers` artifact, and (scheduled runs only) opens or comments on a "Nightly fuzz run failed" issue, which is closed by hand once a run passes. GitHub disables scheduled workflows in public repositories after 60 days without repository activity; re-enable it from the Actions tab.

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

#### Embedded E2E Tests (#114)

Embedded daemon end-to-end tests exercise startup (PKI, migrations, tenant provisioning, mTLS server), a full catalog round-trip over the mTLS gateway, graceful shutdown, and error paths.

```bash
# Run the embedded daemon e2e suite (starts a PostgreSQL container)
task test:integration:embedded
```

**Build tag:** Embedded e2e tests compile only under `-tags integration` and require `TEST_DATABASE_DSN`; the suite skips cleanly when it is unset. The podman `TMPDIR` / storage-driver notes under **Stress Tests** apply here too (environment-only, not baked into the Taskfile).

#### Venom E2E Tests (#18)

Venom E2E suites exercise the `crosscodexd` daemon as a black box: they start the daemon and its PostgreSQL container, drive inline OSCAL through the mTLS gateway, and assert the resulting AGE graph.

```bash
# Run the hermetic E2E suite (Tier A, no LLM required)
task test:e2e            # alias for test:e2e:venom
task test:e2e:venom

# Run the opt-in suite that adds real LLM analysis (Tier B)
task test:e2e:venom:llm
```

**Tiers:** `test:e2e:venom` (Tier A) is hermetic and asserts the OSCAL-to-AGE structural surface without an LLM. `test:e2e:venom:llm` (Tier B) is opt-in and additionally spins up Ollama and LiteLLM to exercise real LLM analysis.

**Requirements:** both suites need `venom`, `psql`, and `pg_isready` on the host plus a podman (or docker) runtime; the LLM tier also needs `jq`. The podman `TMPDIR` / storage-driver notes under **Stress Tests** apply here too.

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
| **Services**     | Request metadata validation                  | Context propagation   |
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
- **Logs**: Structured logging correlated to trace IDs. `crosscodexd` writes logs to stderr at `logging.level` (default `warn`) in `logging.format` (`text` or `json`, default `text`)

See [Telemetry Guide](docs/dev/telemetry.md) for configuration, Jaeger setup, metrics reference, and instrumentation status.

### Audit Trails

JetStream provides persistent audit streams:

| Stream        | Retention  | Content                                 |
|---------------|------------|-----------------------------------------|
| **Decisions** | Indefinite | Final compliance determinations         |
| **LLM Calls** | 90 days    | Full prompts, responses, model versions |
| **Events**    | 30 days    | Pipeline lifecycle, debugging           |

See [Audit Streams Guide](docs/dev/audit-streams.md) for provenance headers, message inspection, and trace correlation.

## Data Retention & Legal Holds

CrossCodex enforces per-data-class retention policy: expired data is archived to
a secondary store and then purged from the primary store, unless a **legal hold**
covers it. Every operation is scoped to a single tenant. Completed jobs and
catalogs are purged as aggregates with their dependents: an expired catalog is
archived as `{catalog, classifications, controls}` and then purged together with
its `classifications`, `controls`, and `embeddings`. The archive deliberately
omits `embeddings` (derived/regenerable, so purge-only); their lifetime is bound
to the parent catalog's retention rather than an independent
`retention.tiers.embeddings` tier. For how the engine works internally, see the
[Retention Lifecycle](docs/dev/retention.md) guide.

### Client CLI (`crosscodex admin`)

All admin commands take `--tenant`, which must match the tenant the caller is
authenticated for; the server authorizes every request against the caller's
tenant.

```sh
# Scan and enforce the retention lifecycle for a tenant.
crosscodex admin retention scan --tenant acme

# Preview without mutating any data.
crosscodex admin retention scan --tenant acme --dry-run

# Show retention-eligible counts and the active-hold total.
crosscodex admin retention stats --tenant acme
```

### Legal Hold Lifecycle

A legal hold prevents matching data from being purged for as long as it is
active. An empty scope field is a wildcard (matches everything), so a hold with
no `--job`/`--created-before` covers the whole tenant.

```sh
# Create a hold. --name is required. The server forces the hold's tenant to the
# caller's tenant, so a hold can never be planted for another tenant.
crosscodex admin hold create --tenant acme --name "litigation-2026-014"

# Narrow a hold to one job and/or only data created before a cutoff (RFC3339),
# and optionally auto-release it at a future timestamp.
crosscodex admin hold create --tenant acme --name "audit-hold" \
  --job job-abc123 \
  --created-before 2026-01-01T00:00:00Z \
  --expires-at 2026-12-31T00:00:00Z

# List active holds (alias: ls).
crosscodex admin hold list --tenant acme

# Release a hold by name. Released holds remain visible in `list` as an audit
# trail but no longer block purges.
crosscodex admin hold release --tenant acme --name "litigation-2026-014"
```

### Database Migration (`crosscodexd migrate`)

`crosscodexd` never migrates. Before the first start, and after every upgrade, run:

```sh
crosscodexd migrate
```

It connects as the schema owner (`database.migration_dsn`, env `CROSSCODEX_DATABASE_MIGRATION_DSN`), applies pending migrations, sets the passwords of `app_user`, `graph_user`, `purge_user`, `tenant_admin` and `backup_user` from the passwords in `database.dsn`, `database.graph_dsn`, `database.purge_dsn`, `database.tenant_admin_dsn` and `backup.dsn`, and provisions `tenants.default_tenant`. It is idempotent. The daemon and the `crosscodexd admin` commands that open the application database (`retention scan`, `reconcile artifacts`, and `adjudicate artifacts` including `review`, `show` and `decide`) refuse a superuser, `BYPASSRLS` or table-owner connection, and a schema that is not at the latest migration. `admin tenant`, `admin identity` (`database.tenant_admin_dsn`) and `admin backup` (`backup.dsn`) do not run these checks. See [Database Migrations](docs/dev/migrations.md). The embedded `crosscodex` CLI is unchanged: it migrates and provisions with the DSN it is given.

### Daemon One-Shot (`crosscodexd admin`)

For on-host operation (e.g. an external scheduler running next to the daemon),
`crosscodexd` exposes in-process one-shot commands. Like `healthcheck` and
`version`, they are **unauthenticated by design**. They run locally against the
daemon's configured resources and select their tenant with `--tenant`, or run
for every active tenant with `--all-tenants` (`retention scan --all-tenants`
covers every tenant, deleted and suspended included, because a deleted tenant's
job data still follows retention):

```sh
crosscodexd admin retention scan --tenant acme
crosscodexd admin retention scan --tenant acme --dry-run
crosscodexd admin retention scan --all-tenants

crosscodexd admin reconcile artifacts --tenant acme
crosscodexd admin reconcile artifacts --tenant acme --dry-run --max-edges 10000
crosscodexd admin reconcile artifacts --all-tenants --dry-run

crosscodexd admin adjudicate artifacts --tenant acme
crosscodexd admin adjudicate artifacts --tenant acme --dry-run
crosscodexd admin adjudicate artifacts --tenant acme --reproject
crosscodexd admin adjudicate artifacts --all-tenants --max-pairs 500

crosscodexd admin adjudicate artifacts review --tenant acme
crosscodexd admin adjudicate artifacts show --tenant acme --pair 3f9c0a1b2d4e5f60
crosscodexd admin adjudicate artifacts decide --tenant acme --pair 3f9c0a1b2d4e5f60 --verdict rejected --reviewer jdoe --expect <token>
```

`retention scan` and an `adjudicate artifacts` run without `--dry-run` or
`--reproject` publish audit events, so they attach to the daemon's NATS server. They refuse to start
when `nats.url` is empty, because embedded mode would start a second JetStream
server on the daemon's store directory. They also refuse a server whose audit
streams do not exist yet, and never create or update a stream, so a one-shot
cannot change the daemon's stream retention. Start the daemon first.

`--all-tenants` and `--tenant` are mutually exclusive, and `adjudicate
artifacts review` and `decide` take `--tenant` only. With `--all-tenants`:

- **Tenant list:** the command lists tenants through
  [`database.tenant_admin_dsn`](#tenant-administration) and skips suspended ones.
  It refuses to run when that DSN is unset.
- **Order and output:** tenants run one at a time in ID order, on one set of
  connections opened for the whole run. Each report on stdout is preceded by a
  `tenant: <id>` line.
- **Failures:** a failing tenant does not stop the rest. Its error goes to
  stderr, and once every tenant has run the command prints
  `<n> of <m> tenants failed: <ids>` and exits `1`. If the command's
  connections or configuration cannot be opened at all (for example the
  database, graph or NATS is unreachable, or the adjudicator's prompts or LLM
  client cannot be set up), the command prints that one error and exits `1`
  without running any tenant.
- **No active tenants** (`retention scan`: no tenants): the command says so on stderr and exits `0`.

`reconcile artifacts` links equivalent artifacts across controls and catalogs:

- **Exact duplicates:** artifacts with the same type and normalized name join one
  `ArtifactGroup` node through `MEMBER_OF` edges.
- **Overlapping names:** groups of one type whose names overlap (token-set overlap of
  0.6 or more) get a scored `SAME_AS` edge.

It never merges or deletes nodes. Every run is a full pass, and re-running it over
an unchanged graph writes no graph elements (it re-queues open pairs for
adjudication, which leaves already-queued pairs unchanged), so it is safe to
schedule (cron, Kubernetes CronJob) once per tenant or once with `--all-tenants`.

- **Overlapping runs:** do not run two reconciles for the same tenant at the same
  time.
- **`--max-edges`:** defaults to 50000. A run that would add more `SAME_AS` edges
  fails before writing anything; this usually means an over-broad artifact name.
- **Exit codes:** 0 on success, 1 on failure, 2 on usage errors.
- **`--dry-run`:** the `new_*` counts report what the run would have written.

`reconcile artifacts` also queues every overlapping-name pair for
adjudication in PostgreSQL (`artifact_pair_verdicts`). It skips pairs that are
closed, meaning confirmed or rejected, and reports them in its `closed` row. It
therefore needs the database as well as the graph.

`adjudicate artifacts` asks an LLM panel whether each queued pair really
denotes one artifact. Token overlap over-matches containment: "Policy" scores
1.0 against "Password Policy".

- **Confirmed:** the panel says yes. The scored edge is superseded by a
  `SAME_AS` edge with `determination_type` `llm_panel`.
- **Rejected:** the panel says no. The scored edge is superseded and nothing
  replaces it.
- **Undecided:** the panel split or fell below
  `analysis.artifact_adjudication.consensus_threshold`. Automation does not
  retry the pair; only a [human verdict](#human-verdicts) can change it.
- **Failed:** too many calls failed or replies did not parse. The pair is
  retried after a backoff (`retry_base_delay`, doubling per failure up to
  `retry_max_delay`; 1 minute and 24 hours by default) and is abandoned after
  `max_attempts` failures.
- **LLM unavailable:** if every call for a pair fails because the LLM gateway
  is unavailable or not configured, the rate limit is hit, the credentials
  cannot be resolved or are rejected, or the model is not allowed, the
  tenant's run stops, the command exits `1`, and the pair is
  not charged a failure. An outage or a wrong key therefore cannot abandon the
  queue; fix the cause and run the command again. With `--all-tenants`, an
  outage stops each tenant's run in turn: every tenant fails fast on its first
  pair, no attempts are charged, and the command exits `1` naming them.
- **Human verdicts win:** automation never overwrites a pair a human decided
  with `decide` (see [Human verdicts](#human-verdicts)). If one lands while
  the panel is running, the panel's opinion is stored as dissent and the
  graph keeps the human verdict.

Other behavior:

- **Scheduling:** run `reconcile artifacts` first, then `adjudicate
  artifacts`, each on its own schedule. Concurrent adjudicators are safe:
  each pair is leased to one run at a time, and a pair whose lease expired
  before its panel started is skipped and retried by a later run.
- **`--max-pairs`:** the most pairs sent to the panel per tenant per run
  (default 200), which bounds LLM spend. Every pair costs
  `len(models) × samples_per_model` completions.
- **Report:** `reset` (with `--reproject`: verdicts marked for
  re-projection), `recovered` (verdicts from earlier runs written to the graph
  now), `orphaned` (confirmations whose groups are gone from the graph),
  `leased`, `confirmed`, `rejected`, `undecided`, `failed`, `abandoned`,
  `dissent` (a human decided the pair meanwhile), `stale` (another run decided
  it meanwhile), `expired` (lease expired before the panel started), `fenced`
  (a newer verdict replaced the one being written to the graph, or another
  run took over the write; the newer verdict is written next) and `busy`
  (another run is writing the pair's verdict to the graph and finishes it).
- **Audit:** every panel call publishes an LLM audit event to the
  `AUDIT_LLM` stream (see [Audit Trails](#audit-trails)), so a real run also
  attaches to the daemon's NATS server (see above). A tenant run's events
  share the job ID
  `artifact-adjudication-<uuid>`, logged as `run.id`.
- **`--dry-run`:** reports `pending`, `due` and `unprojected` counts without
  leasing, calling the LLM or writing verdicts or graph elements. It opens no
  LLM client and no NATS connection, so it runs on a host without an LLM
  gateway or NATS (the configuration check below still applies). Like every
  command that opens the database, it does not migrate: it refuses an
  outdated schema and points at `crosscodexd migrate`.
- **`--reproject`:** with `--tenant` only. Marks the tenant's projected
  confirmed and rejected verdicts, LLM-panel and human alike, unprojected and
  writes them back to the graph (`reset` counts them), then stops: it leases
  no pairs and makes no LLM calls, so like `--dry-run` it needs no LLM
  gateway or NATS. Use it after rebuilding the tenant's graph, then run
  without `--reproject` to adjudicate due pairs. It cannot be combined with
  `--dry-run`, `--max-pairs` or `--all-tenants`.
- **Configuration:** the command refuses to run until
  `analysis.artifact_adjudication.enabled` is `true` and `models` is set (see
  [Key Configuration Examples](#key-configuration-examples)). `review` and
  `decide` do not.
- **Prompt:** the panel uses the `artifact_same_as` prompt. Like any other
  prompt, a YAML file with that `name` in `$XDG_DATA_HOME/crosscodex/prompts/`
  or in a directory listed in `prompt.layer_paths` overrides it. Artifact
  names and samples come from analyzed catalogs, so they are flattened to one
  line, stripped of invisible characters, capped and escaped before they reach
  the prompt.
- **Exit codes:** 0 on success, 1 on failure, 2 on usage errors.
- **Clocks:** leases, and the claims that reserve a verdict while it is
  written to the graph, are timed by the clock of the host running the
  command (`adjudicate` or `decide`). Keep every such host in sync (NTP);
  otherwise a skewed host can take a pair whose lease or claim has not yet
  expired. A host whose clock runs far ahead holds its claims that far into
  the future: until then every run skips the pair (`--dry-run` still counts
  it as `unprojected`) and its verdict does not reach the graph.
  `--reproject` does not clear a claim; clearing one early takes a manual
  database update of that pair's row.

After rebuilding a tenant's graph, re-run the reconciler, then run
`crosscodexd admin adjudicate artifacts --tenant <tenant> --reproject` to write
the tenant's verdicts back to the graph, human verdicts included.

`crosscodexd admin backup` captures, lists, verifies and restores PostgreSQL (WAL-G base backups plus WAL archiving for PITR), tenant object stores, and the JetStream audit streams:

```sh
crosscodexd admin backup run
crosscodexd admin backup list
crosscodexd admin backup verify [--point ID]
crosscodexd admin backup restore --point ID
```

- `backup run` — captures Postgres, every tenant's objects and the audit streams into a new backup point.
- `backup list` — lists complete backup points (and incomplete ones separately).
- `backup verify` — checks blob/snapshot checksums, WAL-G base backup presence, WAL continuity and staleness.
- `backup restore` — restores objects and audit streams from a backup point; Postgres is restored separately with `crosscodex-db-restore`.

Exit codes: 0 ok, 1 failure or stale, 2 usage error. See [deploy/README.md, Backups](deploy/README.md#backups) for setup, the run/verify/restore procedures, and PITR.

#### Human verdicts

`adjudicate artifacts review`, `show` and `decide` let a person settle the
`SAME_AS` pairs the panel could not, or overrule a panel verdict. The workflow
has three steps: list, read, decide.

```sh
crosscodexd admin adjudicate artifacts review --tenant acme --limit 20
crosscodexd admin adjudicate artifacts show --tenant acme --pair 3f9c0a1b2d4e5f60
crosscodexd admin adjudicate artifacts decide --tenant acme --pair 3f9c0a1b2d4e5f60 --verdict confirmed --reviewer jdoe --expect <token>
```

**1. `review`** lists up to `--limit` pairs (default 100), oldest first, that
need a person:

- `undecided` and `abandoned` pairs;
- human verdicts that a later panel run disagreed with (its opinion is stored
  as dissent).

Its columns are `PAIR` (the pair ref), `STATUS`, `BY` (who decided it:
`llm_panel`, `human`, or `token_overlap` for an abandoned pair), `VOTES`,
`TYPE`, `LOW` and `HIGH` (the two artifact groups' names), `SCORE` (token
overlap) and `DISSENT` (the dissenting verdict, or `-`). `VOTES` is the
current panel verdict's split:

- `2/3 same`: two of three valid votes said the groups are the same;
- `2/3 same (1 failed)`: the same, with one more vote that failed and did not
  count;
- `unreadable`: the stored votes cannot be decoded; `show` prints the error;
- `-`: no panel verdict (an abandoned or human-decided pair).

Names come from analyzed catalogs, so they are flattened to one line,
stripped of control and invisible characters and capped before they are
printed. With nothing to review it prints `no pairs need review`.

**2. `show`** prints everything a reviewer needs to decide one pair, so the
decision rests on the panel's reasoning and not only its vote count:

```text
PAIR        3f9c0a1b2d4e5f60
TYPE        policy
LOW         Access Control Policy
HIGH        Access Policy
SCORE       0.71
STATUS      undecided
BY          llm_panel / artifact-adjudicator
CONFIDENCE  0.50
DECIDED     2026-10-13T09:14:02.51Z
PROMPT      artifact_same_as v1.1.0

VOTES (1 same, 1 distinct, 1 failed; 2 valid of 3)
  voter-a  same  high
    Both titles name the organization's access control policy ...
  voter-b  distinct  medium
    The second document scopes only remote access ...
  voter-c  failed  -

PANEL DISSENT
  none

RELATED PAIRS (showing 1 of 1 that share a group)
  7d21e0c4b9a85f36  confirmed  llm_panel  Access Policy | Access Control Standard

EXPECT    9be41c07d3a2f685
To decide: crosscodexd admin adjudicate artifacts decide --tenant acme --pair 3f9c0a1b2d4e5f60 --verdict <confirmed|rejected> --reviewer <id> --expect 9be41c07d3a2f685
```

The example is illustrative; values vary. Its sections:

- **Header:** the pair, its artifact groups, the token-overlap `SCORE`, the
  current `STATUS`, who decided it (`BY` is determination type / decider),
  `CONFIDENCE`, `DECIDED` time and the prompt that produced the panel verdict.
- **`VOTES`:** the split, then each vote's voter, decision (`same`,
  `distinct` or `failed`), confidence and justification.
- **`PANEL DISSENT`:** on a human verdict, the panel's opinion, labelled
  `overridden by the human verdict` (the panel verdict the person replaced),
  `given after the human verdict (disputed)` (a later panel run disagreed;
  the pair is in `review`) or `given after the human verdict (agrees)`. A
  dissent that cannot be decoded is labelled `unreadable`. Otherwise `none`.
- **`LAST ERROR`:** shown only when the last panel attempt failed.
- **`RELATED PAIRS`:** other pairs that share a group with this one, with
  their status and decider. `--related <n>` sets how many to list (default
  20, must be positive); the heading says how many exist in total.
- **`EXPECT`:** the token `decide` needs, and a ready-to-edit `decide`
  command.

Everything printed from stored data is cleaned the same way as `review`.
`show` changes nothing. If the stored votes cannot be decoded, it prints the
pair with an `unreadable` `VOTES` section and the `EXPECT` token anyway, then
exits 1 with the error; the pair can still be decided.

**3. `decide`** records the verdict, using the `EXPECT` token from `show`
(see the `--expect` item below).

To change a verdict the panel already reached, find the pair with `--name`:

```sh
crosscodexd admin adjudicate artifacts review --tenant acme --name "access policy"
crosscodexd admin adjudicate artifacts decide --tenant acme --pair 7d21e0c4b9a85f36 --verdict rejected --reviewer jdoe --expect <token>
```

`review --name <text>` lists, instead of the review queue, up to `--limit`
pairs in any status (pending, confirmed, rejected, undecided, abandoned),
whoever decided them, whose `LOW` or `HIGH` name contains the text. Matching
ignores case, and `%`, `_` and `\` match only themselves. The text must be
1–200 bytes of plain text (no control or invisible characters). Same columns,
oldest first; on a pair a person decided, `DISSENT` shows the panel's
opinion, such as the verdict the person overrode. With no match it prints `no pairs match "<text>"`.

`decide` records `--verdict confirmed` or `--verdict rejected` on one pair:

- **Pair ref:** `--pair` is the `PAIR` column of `review`, 16 lowercase hex
  characters derived from the pair's internal key. It is opaque and safe to
  paste; the key itself embeds artifact names and is never printed. A ref
  that matches no pair of the tenant records nothing. `review` lists only
  pairs that need a person; use `review --name` to find the ref of any
  other pair, such as one the panel confirmed or rejected.
- **`--expect`:** required. The `EXPECT` value `show` printed for this pair.
  It identifies the state you read: the verdict, who decided it, its
  confidence, decision time, score, votes and dissent. If another person or
  a panel run changed the pair since, `decide` records nothing and says `pair
  <ref> changed since you viewed it: it is now ...`. Run `show` again, read
  the new reasoning, and decide with the new token. Queue bookkeeping
  (retries, leases, projection) does not change the token. Repeating a
  verdict the pair already holds from a person is accepted with any valid
  token. Without `--expect`, `decide` exits 2 and names the `show` command
  to run.
- **`--reviewer`:** required. The ID of the person recording the verdict,
  stored as `determined_by` on the verdict and on a confirmation's graph
  edge (at most 256 bytes, no control or invisible characters). It must be
  a registered, non-deleted reviewer of the tenant (`crosscodexd admin
  reviewer add`, see [Tenant administration](#tenant-administration));
  otherwise `decide` records nothing and names the command to run. It is not
  authenticated: like every `crosscodexd admin` command, `decide` trusts
  whoever can run it on the host.
- **Authority:** a human verdict outranks the panel, and automation never
  changes it. Any pair can be decided, including one the panel decided or
  another person decided; a panel verdict it overrides is kept as dissent.
  Repeating a pair's current human verdict changes nothing and prints
  `unchanged` (the original `determined_by` is kept), unless a later panel
  run disagreed with it (the pair is in `review`). Then `decide` with the same
  verdict re-affirms it: it records `--reviewer` and the new decision time,
  writes a fresh graph edge for a confirmation (the earlier human edge ends
  at the new decision time), prints `re-affirmed over the panel's later
  dissent`, and the pair leaves `review`. The panel's opinion stays as
  dissent.
- **Graph:** `decide` writes the verdict to the graph at once. A confirmation
  adds a `SAME_AS` edge with `determination_type` `human` and confidence 1;
  either verdict supersedes every other current `SAME_AS` edge between the
  two groups (the token-overlap candidate, a panel edge, an earlier human
  edge). It then prints the projection report: `recovered` (verdicts written
  to the graph), `orphaned` (confirmations whose groups are gone from the
  graph), `fenced` and `busy` (as for `adjudicate artifacts`).
- **Delays:** if an `adjudicate` run is writing the pair's previous verdict
  to the graph at that moment, `decide` records the verdict, prints that it
  reaches the graph when that run finishes or by re-running `decide` after a
  printed time, and leaves the pair to that run. If its own graph write
  fails, it says the verdict is recorded and when to re-run (up to
  `lease_duration` later). Re-running `decide` with the same arguments never
  records the verdict twice; it finishes the graph write.
- **Requirements:** neither command needs
  `analysis.artifact_adjudication.enabled`, an LLM gateway or NATS; they open
  only the database and the graph. `lease_duration` bounds how long one
  write may hold a pair, so it is validated (must be positive) even when
  adjudication is disabled, and a non-positive value fails config load.
- **Rebuilds:** `adjudicate artifacts --reproject` writes human verdicts back
  to a rebuilt graph along with the panel's.
- **Exit codes:** 0 on success, 1 on failure, 2 on usage errors.

#### Tenant administration

```sh
crosscodexd admin tenant create  --tenant acme --display-name "Acme Corp"
crosscodexd admin tenant list
crosscodexd admin tenant inspect --tenant acme
crosscodexd admin tenant suspend --tenant acme
crosscodexd admin tenant resume  --tenant acme
crosscodexd admin tenant import  --file tenants.yaml
```

These commands connect as the least-privilege `tenant_admin` role, never the
app or owner credential. Set `database.tenant_admin_dsn` (or
`CROSSCODEX_DATABASE_TENANT_ADMIN_DSN`) to a DSN for that role.
`crosscodexd migrate` creates the role; set its password yourself. The commands
refuse to run while the DSN is empty.

- **`create`:** creates the tenant and its graph, or renames an existing one
  without changing its status. Prints `created` or `updated`. Display names
  must not be blank or contain control characters.
- **`suspend` / `resume`:** while suspended, the gateway refuses every request
  from the tenant except health checks, until `resume`. Queued pipeline work
  still drains. Prints `suspended` or `already suspended`, and `resumed` or
  `already active`. Both refuse a deleted tenant; run `tenant restore` first.
- **`import`:** applies a YAML file in one transaction, so either every tenant
  is created or renamed, or none is. Renaming a deleted tenant is refused,
  as with `create`:

  ```yaml
  tenants:
    - tenant_id: acme
      display_name: Acme Corp
  ```

Tenants and reviewers are never removed, because their judgements are
permanent. They are soft-deleted, and a deleted one can be hidden behind a
pseudonym:

```sh
crosscodexd admin tenant delete   --tenant acme --by alice --reason "contract ended"
crosscodexd admin tenant restore  --tenant acme --by alice --reason "contract renewed"
crosscodexd admin tenant hide     --tenant acme --by alice --reason "customer requested"
crosscodexd admin tenant unhide   --tenant acme --by alice --reason "audit follow-up"
crosscodexd admin reviewer add     --tenant acme --reviewer jdoe --by alice --reason "joined review team"
crosscodexd admin reviewer list    --tenant acme
crosscodexd admin reviewer delete  --tenant acme --reviewer jdoe --by alice --reason "left the team"
crosscodexd admin reviewer restore --tenant acme --reviewer jdoe --by alice --reason "rejoined"
crosscodexd admin reviewer hide    --tenant acme --reviewer jdoe --by alice --reason "privacy request"
crosscodexd admin reviewer unhide  --tenant acme --reviewer jdoe --by alice --reason "audit follow-up"
crosscodexd admin identity trace   --pseudonym hidden-reviewer-0123456789abcdef --by alice --reason "audit query"
crosscodexd admin identity export  --out /secure/crosscodex-audit-2026-10-22.jsonl
```

These also connect as `tenant_admin`. Every command except `reviewer list`
and `identity export` changes or reveals an identity, so it requires `--by` (who is acting, 1-256
bytes) and `--reason` (1-500 bytes), is signed with the attestation keys
(`attestation.private_key_path` and `attestation.public_key_path`) and is
recorded in the database's audit log. Without the keys or
`database.tenant_admin_dsn` the commands refuse to run and change nothing.

- **`tenant delete`:** the tenant's data and verdicts are kept, but the
  gateway and every service refuse it. Retention still applies to its job
  data. `create` cannot rename a deleted tenant.
- **`tenant restore`:** brings a deleted tenant back as `suspended`; run
  `crosscodexd admin tenant resume` to reactivate it. A hidden tenant must be
  unhidden first.
- **`reviewer add`:** registers a reviewer so `adjudicate artifacts decide`
  accepts them. Prints `added`, or `already registered`. A deleted
  reviewer is restored with `reviewer restore`, not added again.
- **`reviewer delete` / `restore`:** a deleted reviewer's verdicts stay, but
  `decide` refuses new ones from them. Restoring makes them active again.
  On a deleted tenant, `reviewer add` and `restore` are refused; `delete`,
  `hide` and `unhide` still work.
- **`hide` / `unhide`:** only a deleted tenant or reviewer can be hidden.
  `hide` prints the pseudonym (`hidden-tenant-…` or `hidden-reviewer-…`),
  which then replaces the ID in `tenant list`, `reviewer list`, `retention
  scan --all-tenants` and `adjudicate artifacts show`. The pseudonym stays
  the same across unhide and hide again. Hiding is not erasure: the real ID
  remains in stored verdicts, graph edges and audit records. Tenant and
  reviewer IDs starting with `hidden-` are reserved.
- **`identity trace`:** prints the kind, tenant, identity, status and hidden
  flag behind a pseudonym. The lookup itself is recorded.
- **`identity export`:** writes the whole audit log to `--out` as JSON Lines,
  one event per line in `event_id` order, and prints `exported <n> identity
  events to <path>`. Each line has `event_id`, `occurred_at` (RFC 3339, UTC),
  `action`, `kind`, `tenant_id`, `identity_id`, `actor`, `db_user`, `reason`,
  `request` and `attestation` (base64 of the stored bytes, so their SHA-256
  can be recomputed) and `attestation_sha256`. The file is sensitive: it
  holds the real IDs of hidden tenants and reviewers, so store it securely.
  It is created with mode `0600` and never overwrites an existing file. It is
  written as `<path>.partial` first and moved into place only when complete;
  a failed export removes the partial file. It reads only, so it needs no
  `--by`, `--reason` or attestation keys. Run it before decommissioning a
  database (see [Decommissioning](deploy/README.md#decommissioning)).
- **Exit codes:** 0 on success, 1 on failure, 2 on usage errors.

### Configuration Reference

```yaml
retention:
  # Default retention per data class. Each value is "indefinite", "<n>y",
  # "<n>d", or any Go duration (e.g. "720h"). An empty value means "keep".
  defaults:
    attestation: indefinite
    job_results: 90d
    catalogs: 2y
    embeddings: 90d
    # Audit-class tiers are accepted for config completeness but do NOT drive
    # engine purges — the retention engine never purges audit data. Audit
    # retention is enforced by JetStream stream MaxAge via
    # nats.streams.audit_llm_retention / audit_events_retention
    # (see docs/dev/audit-streams.md).
    audit_decisions: indefinite
    audit_llm: 90d
    audit_events: 30d

  # Optional cron expression for the in-daemon scheduler. Empty = disabled
  # (drive scans externally via the one-shot instead; see below).
  scan_schedule: ""

  # Archive backend for data moved out of the primary store before purge.
  archive:
    backend: local          # local | s3 | "" (disabled)
    local:
      path: /var/lib/crosscodex/archive
    s3:
      bucket: crosscodex-archive
      storage_class: GLACIER

  # Per-tenant overrides. Non-empty fields replace the matching default;
  # empty fields fall back to the default.
  tenants:
    acme:
      job_results: 1y

database:
  # Purge runs under a dedicated, RLS-scoped purge_user role that the
  # delete-path immutability triggers accept. When purge_dsn is unset, scans
  # still run but purges fail closed rather than deleting under the wrong role.
  purge_dsn: "${PURGE_DATABASE_DSN}"   # e.g. postgres://purge_user:...@localhost:5432/crosscodex

backup:
  # postgres:// URL for backup_user (LOGIN REPLICATION).
  # Required when destination.backend is set. Never logged.
  # CROSSCODEX_BACKUP_DSN sets this field from the environment.
  dsn: ""
  destination:
    backend: ""           # local | s3 | "" (backup disabled; fails closed)
    local:
      path: ""              # absolute; must not overlap storage.objects.base_path
    s3:
      bucket: ""
      region: ""
      endpoint: ""           # S3-compatible stores; enables path-style addressing
      storage_class: ""
  max_age:
    # `admin backup verify` reports a store stale once its newest backup
    # point is older than this. Each value must be greater than zero.
    postgres: 26h
    objects: 26h
    nats: 26h
```

### External Cron Example

When `retention.scan_schedule` is left empty, drive scans from an external
scheduler using the daemon one-shot. Example crontab entry running a nightly
scan of every tenant at 02:00:

```cron
0 2 * * *  crosscodexd admin retention scan --all-tenants >> /var/log/crosscodex/retention.log 2>&1
```

To scan only one tenant, use `--tenant acme` in place of `--all-tenants`. Do not
schedule both at the same time: non-dry-run scans share one global lock, so one
of them would fail every night (see [Backups](deploy/README.md#backups)).

Artifact deduplication runs the same way. Reconcile first, then adjudicate:

```cron
0 3 * * *  crosscodexd admin reconcile artifacts --all-tenants >> /var/log/crosscodex/reconcile.log 2>&1
30 3 * * * crosscodexd admin adjudicate artifacts --all-tenants >> /var/log/crosscodex/adjudicate.log 2>&1
```

## Uninstall

To fully remove CrossCodex:

1. **Remove the binary** (location depends on your install method):
   ```sh
   rm $(which crosscodex)
   ```

2. **Remove configuration**:
   ```sh
   rm -rf "${XDG_CONFIG_HOME:-$HOME/.config}/crosscodex"
   ```

3. **Remove data** (prompt layers):
   ```sh
   rm -rf "${XDG_DATA_HOME:-$HOME/.local/share}/crosscodex"
   ```

4. **Remove state** (daemon PID, embedded TLS certificates):
   ```sh
   rm -rf "${XDG_STATE_HOME:-$HOME/.local/state}/crosscodex"
   ```

5. **Remove project-level configuration** (run in each project directory):
   ```sh
   rm -rf .crosscodex/
   ```

6. **Remove shell completions** (if installed):
   ```sh
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
