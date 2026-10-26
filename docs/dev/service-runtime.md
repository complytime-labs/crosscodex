# Service Runtime

This document covers how `crosscodexd` picks a service role at startup, what each role wires up, and what an operator must configure before running the roles that touch tenant data.

## Overview

`crosscodexd` is a single binary that can run as one process (`all`, the default) or split into independently deployable pieces (`gateway`, `pipeline`, `worker`, `graph`). The role determines which services `cmd/crosscodexd/bootstrap.go` constructs and which resources (Postgres pools, NATS client, LLM client) it needs to hold open.

## Roles

> **Migrating from a pre-#132 deployment.** The role vocabulary changed. `--role pipeline` used to alias to `gateway`; it now starts a standalone process that requires its own `pipeline.addr` plus mutual TLS (`tls.mode: mutual` with a CA, or the equivalent `tls.targets.pipeline-server` override) — there is no unauthenticated fallback. `--role gateway` used to run the pipeline in-process; it now requires `pipeline.endpoint` pointed at a separately-running `pipeline` role, plus a mutual-TLS `pipeline-client` identity (client cert, key, and CA) to reach it — there is no cleartext fallback. Both fail loudly with an actionable error rather than silently misbehaving, so an operator upgrading from before this branch will see startup errors, not degraded behavior.

`pkg/config/role.go` defines the canonical roles crosscodexd can actually start, plus a set of aliases that resolve to one of them:

| Role                    | What it runs                                                                                                                                                | Notes                                                                                                                                                                                          |
|-------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `all` (default)         | Graph service + LLM worker + gateway (HTTP server + in-process pipeline service)                                                                            | Everything in one process, no network hop between gateway and pipeline; see `attachGraph`, `attachWorker`, `buildPipelineService`, and `attachAllGateway` in `cmd/crosscodexd/bootstrap.go` |
| `gateway`               | Gateway HTTP server (`internal/gateway.Server`). Serves catalog import itself (X.509 authn, ingestion, catalog service) and forwards jobs to a separately-deployed `pipeline` role over a Connect client | Requires `pipeline.endpoint`; enforces the fail-closed preconditions below; see [Catalog import in a split deployment](#catalog-import-in-a-split-deployment)                                                                                                                      |
| `pipeline`              | A production `pipeline.Service` behind its own Connect RPC listener (`internal/pipeline.Server`), dispatching analyzer work to workers over NATS            | Requires `pipeline.addr`; enforces the fail-closed preconditions below                                                                                                                         |
| `worker`                | LLM worker (`internal/worker.Worker`) consuming tasks from the NATS queue group                                                                             | No Postgres connection is opened for this role — a worker replica scales horizontally without database credentials                                                                             |
| `graph`                 | Graph-materialization service (`internal/graph.Service`), subscribing to NATS and writing to the AGE-backed graph database. Also projects Control nodes and PARENT_OF edges from catalog parsed events |                                                                                                                                                                                                |
| `analysis`, `synthesis` | Alias for `pipeline`                                                                                                                                        | See [Aliases](#aliases-analysis-synthesis) below                                                                                                                                               |

`config.ResolveRole` validates a requested role name against `config.CanonicalRoles` (`all`, `gateway`, `pipeline`, `worker`, `graph`) and the alias map, returning an error listing every accepted value if the input matches neither.

### Aliases: `analysis`, `synthesis`

`analysis` and `synthesis` are deployment-facing names that resolve to the `pipeline` role: both execute synchronously inside `pipeline.Service`, so whichever process actually runs `pipeline.Service` (the standalone `pipeline` role, or `all`) is what executes them. There is no plan to split them into their own NATS-decoupled services — see [complytime-labs/crosscodex#132](https://github.com/complytime-labs/crosscodex/issues/132)'s "out of scope" section.

## Selecting a role

Three ways to set the role, in precedence order (highest first):

1. **`--role` CLI flag** — `crosscodexd --role worker`. Takes priority over everything else.
2. **`CROSSCODEX_ROLE` environment variable / `role` config field** — the `role` key in `pkg/config`'s layered config (file, then env overlay under the `CROSSCODEX` prefix, so `CROSSCODEX_ROLE=worker`).
3. **Default** — `all`, set in the embedded config defaults (`pkg/config/defaults.go`: `role: all`).

`cmd/crosscodexd/main.go`'s `effectiveRole(flagRole, cfgRole string) string` implements this: it returns `flagRole` if non-empty, otherwise `cfgRole` (which has already had the config-file/env/default layers merged and validated by the time `main` sees it). If the flag differs from the already-resolved config role, `main` re-resolves it through `config.ResolveRole` so a flag-supplied alias (e.g. `--role pipeline`) is canonicalized the same way the config loader would.

## Gateway backends by role

`gateway.NewService` serves only the backends it is given; an RPC whose backend is absent returns `Unavailable`.

| Role | Mappings backend (`ListJobMappings`, `GetControlMappings`) | Graph backend (`QueryGraph`, `FindSimilar`) |
|------|------|------|
| `all` | `results.Service` via `newMappingsBackend`, in `buildIngestGateway` | `rt.graphService`, which `attachGraph` builds first; `attachAllGateway` passes it to `buildIngestGateway` |
| `gateway` | `results.Service` via `newMappingsBackend`, in `buildIngestGateway` | not attached |
| embedded CLI daemon (`cmd/crosscodex/embedded.go`) | `results.Service` over its tenant connection | not attached |

The feedback backend (`SubmitVote`, `GetReviewQueue`) is not implemented in any role; both RPCs return `Unavailable`.

### Mappings read path

Mappings come from PostgreSQL, not the graph. `internal/results.PGStore` reads `vote_summaries` (rows with analyzer name `relationship`, for the `synthesized` stage) and `analysis_results` (analyzer `relationship`, for the `analyzer` stage) in one tenant transaction on the restricted application role, so RLS confines the read to the caller's tenant. `vote_summaries` holds one row per analyzer contribution (primary key `job_id, source_id, target_id, analyzer_name`), so a pair with both `requires` and `relationship` results does not collide.

`results.Service` implements `gateway.MappingsBackend`; the assertion lives in `cmd/crosscodexd/bootstrap.go` because `internal/results` must not import `internal/gateway`. Storage sits behind the `results.Reader` interface (`ListMappings(ctx, Filter) (Page, error)`). `PGStore` is the only implementation; a different store replaces only the `Reader` passed to `results.NewService`. An implementation must confine reads to the tenant in `ctx` and return `ErrJobNotFound`, `*JobNotCompletedError` and `ErrInvalidPageToken` for the corresponding cases, which `Service` maps to Connect errors. `GetControlMappings` uses the same reader at the synthesized stage across the tenant's completed jobs; it rejects `limit` outside 0..1000 and returns no `next_page_token` (the request has no `page_token`), so callers wanting more page through `ListJobMappings`.

## Health checks

The `health.addr` config field (`pkg/config.HealthConfig.Addr`, default `:9091` from `pkg/config/defaults.go`) configures a dedicated `GET /healthz` HTTP listener, built by `newHealthServer` in `cmd/crosscodexd/health.go`.

Which roles use it:

- **`worker`** — `attachWorker` starts a dedicated health listener on `health.addr`. It has no dependency to check (there's no NATS connectivity probe in `pkg/natsbus.Client` today), so it's a pure liveness probe — `healthCheckHandler` called with zero checks always reports `{"status": "healthy"}`.
- **`graph`** and **`pipeline`** — both start a dedicated listener on `health.addr`, backed by a `dbPingCheck` closure (defined locally in `bootstrap.go`): it calls the app Postgres pool's `Health(ctx)` and fails the probe if the pool reports not connected.
- **`gateway`** and **`all`** — do *not* use the dedicated listener. `internal/gateway.Server` already serves its own built-in `/healthz`. For `all`, `bootstrap` explicitly sets `rt.healthServer = nil` after wiring graph/worker/gateway, because the gateway's `/healthz` already probes that process and its database dependency. `internal/pipeline.Server`'s own RPC listener has no `/healthz` of its own — every RPC on it requires mTLS, so health checks go through the separate dedicated listener above instead of carving an unauthenticated path out of the RPC listener.

The gateway's `/healthz` and its `Health` RPC share one source: both call `Service.Health`, which delegates to the `AdminBackend`. `gateway.PoolAdminBackend` (used by the embedded daemon and the `all` and `gateway` roles) pings the app pool and reports it as the `database` entry in `HealthCheckResponse.services`, with a fixed `database reachable` / `database unreachable` message. The driver error is logged by the daemon and never returned (CWE-209). The CLI's connection gate (`healthCheck` in `cmd/crosscodex/connect.go`) fails closed: any overall status other than `HEALTHY` is an error that names each non-healthy dependency, so a daemon that answers but can't reach its database is never treated as usable. `/healthz` (`healthzHandler` in `internal/gateway/server.go`) applies the same rule: it returns 503 unless the overall status is `HEALTHY`, with the `HealthCheckResponse` JSON as the body so an operator can see which dependency failed. If the health call itself errors, it returns 503 with a fixed `{"status":"unhealthy"}` body and logs the error, since `/healthz` is unauthenticated (CWE-209). It is a readiness signal, not bare process liveness, so `crosscodexd healthcheck` (the compose healthcheck, which checks only the status code) reports the container unhealthy while the database is unreachable.

## Database startup checks

Roles that open Postgres never migrate. Run `crosscodexd migrate` first (see [Database Migrations](migrations.md#crosscodexd-migrate)). At startup the daemon refuses to run when:

- `database.dsn`, `database.graph_dsn` or, for role `all` and `admin retention scan` when it is set, `database.purge_dsn` connects as a superuser, a `BYPASSRLS` role, a table owner, or a member of one (`pkg/db.CheckRestrictedRole`).
- the schema is not at the latest embedded migration version, or the last migration failed (`pkg/db.CheckSchemaVersion`, on the `database.dsn` connection).
- role `all` or `gateway` finds no `tenants` row for `tenants.default_tenant`, or finds it deleted. `crosscodexd migrate` provisions it.

## Fail-closed preconditions for `pipeline` / `all`

`buildPipelineService` in `cmd/crosscodexd/bootstrap.go` refuses to construct a `pipeline.Service` unless three things are configured, returning an error immediately rather than starting in a partially-usable state. This applies to the `pipeline` role directly, and to `all` (which calls `buildPipelineService` as part of its startup sequence):

1. **Attestation key paths** — `attestation.private_key_path` and `attestation.public_key_path` (`pkg/config.AttestationConfig.PrivateKeyPath` / `PublicKeyPath`) must both be set. `pipeline.New` requires a real `attestation.Generator`, and `attestation.NewGenerator` is constructed from an `attestation.FileKeyProvider` pointed at these two paths — an operator must have a signing keypair on disk (or otherwise provisioned to those paths) before running `pipeline` or `all`.
2. **Default tenant** — `tenants.default_tenant` (`pkg/config.TenantsConfig.DefaultTenant`) must be set. `storage.NewLocal` binds the storage provider to exactly one tenant at construction time (a known single-tenant storage limitation shared with `cmd/crosscodex`'s embedded mode), so pipeline can't start without knowing which tenant's storage namespace to use.
3. **Storage base path** — `storage.objects.base_path` (`pkg/config.ObjectStorageConfig.BasePath`) must be set. It defaults to `""` (`pkg/config/defaults.go`), and `storage.NewLocal` resolves an empty root via `filepath.Abs("")`, which silently resolves to the process's current working directory. Without this check, a `pipeline`/`all` daemon started with default config would write tenant object storage under `<cwd>/<tenant>/` instead of failing fast.

## Fail-closed preconditions for `pipeline` and `gateway`, when run as separate roles

- **`pipeline`** additionally requires `pipeline.addr` (the bind address for its standalone Connect RPC listener) — `attachPipeline` fails closed if it's unset. It also requires mutual TLS: `attachPipeline` resolves the `pipeline-server` TLS target and fails closed unless it produces a `mutual`-mode config with a CA, because `pipeline.Server` enforces `RequireAndVerifyClientCert` on every RPC and `rpcserver.Listen` would otherwise fall back to plaintext when TLS is off.
- **`gateway`** has the following requirements:
  - **Pipeline endpoint** — `pipeline.endpoint` (where its Connect client dials to reach a separately-deployed `pipeline` role) must be set. `attachGateway` fails closed if unset.
  - **Mutual TLS for client** — `gateway.NewConnectPipelineBackend` resolves the `pipeline-client` TLS target and fails closed unless it yields a config that both presents a client certificate and carries a CA to verify the pipeline's server certificate.
  - **Why mutual TLS is required** — This mirrors the `pipeline` server-side precondition. Because the pipeline listener enforces `RequireAndVerifyClientCert` and has no plaintext fallback, a client that couldn't present a certificate would only fail at the first RPC. Both halves of the split refuse to start without genuine mutual TLS rather than degrading to cleartext.
  - **Catalog import preconditions** — the gateway serves catalog import itself, so `buildIngestGateway` (shared with `all`) refuses to start unless:
    - `tenants.default_tenant` is set (`gateway role: tenants.default_tenant must be set`);
    - `storage.objects.base_path` is set (`gateway role: storage.objects.base_path must be set`). Uploaded documents are stored here, on the gateway's own filesystem;
    - the `gateway-server` TLS target resolves to `mutual` mode with a CA (`gateway role: catalog import authenticates clients by X.509 certificate, so tls.mode (or tls.targets.gateway-server.mode) must be "mutual" with a CA configured`), because the listener otherwise never verifies a client certificate;
    - the default tenant exists (`gateway role: default tenant "<id>" does not exist. Run crosscodexd migrate ...`) and is not deleted (`gateway role: default tenant "<id>" is deleted. Restore it with crosscodexd admin tenant restore ...`).
  - **Resources** — Postgres (`database.dsn`) and NATS. The gateway holds no graph or vector credentials.
  - **No pipeline service preconditions** — it does not need the attestation key paths from `buildPipelineService`; it constructs no `pipeline.Service`, only a network client to one.
- **`all`** needs neither `pipeline.addr` nor `pipeline.endpoint` — it wires `pipeline.Service` directly into the gateway's `PipelineBackend` in-process, with no listener or client involved.

## Catalog import in a split deployment

With `gateway`, `pipeline`, `worker` and `graph` running as separate processes, a catalog import flows through two roles:

1. A client uploads the document to the `gateway` role. The gateway owns ingestion: it authenticates the client certificate, stores the uploaded blob under its own `storage.objects.base_path`, parses the document and upserts the `controls` rows in Postgres. It never writes the graph.
2. After upserting a non-empty control set, the catalog service publishes `crosscodex.catalog.<tenant_id>.<catalog_id>.parsed` (`natsbus.CatalogParsedSubject`). If publishing fails, the import fails with `CodeUnavailable` and a message to re-import the same document; the rows and blob stay.
3. The `graph` role subscribes (queue group `graph-materializer`, `internal/graph/catalog_projection.go`), reads the tenant's `controls` rows under RLS and upserts the Control nodes (`UpsertNode`) and PARENT_OF edges. The event payload is ignored; the subject supplies the tenant and catalog. A malformed subject is dropped. A PARENT_OF edge whose parent node is missing is skipped and counted (`graph.materialize.edges_skipped.total`). Zero stored controls, a reader error or a write error is logged and counted as `graph.events.total` `status=error`.
4. Job starts are forwarded to the `pipeline` role as described in [Tenant propagation](#tenant-propagation-between-gateway-and-a-standalone-pipeline-role).

Operational consequences:

- **Eventual consistency.** Control nodes appear after the import returns. Analysis edges that arrive before their Control nodes exist are skipped and counted, not retried. Re-importing the catalog repairs both.
- **Start the graph role before importing.** The subscription is core NATS: an event published while no graph role is subscribed is lost. Deploy at least one graph role and start it first. Re-import to recover a lost event.
- **Storage.** Because the gateway writes uploaded blobs under its own `storage.objects.base_path`, that path is separate from the pipeline role's unless the two roles share a filesystem.
- Role `all` runs the same ingestion path and additionally confirms the tenant graph exists at startup (`CreateGraph`).

### Client certificate trust (known limitation)

Catalog import runs in single-tenant mode: any client certificate signed by the `gateway-server` CA authenticates as `admin` of `tenants.default_tenant`. The `auth.x509_mappings` config field exists in `pkg/config` but the daemon does not read it, so there is no way to map certificates to other tenants or roles. This is a limitation of the current implementation, not a configured policy.

`pkg/tlsconfig` uses the target's CA as both the root pool and the client CA pool (`ClientCAs`) for a `mutual` server target, and `tls.targets.<target>.ca` overrides the global CA for that target. In a split deployment the inter-service CA also signs service identities such as `pipeline-client`, so by default those certificates would authenticate as admin at the gateway. Give `gateway-server` a dedicated client CA through `tls.targets.gateway-server.ca`, separate from the inter-service CA.

## Tenant propagation between gateway and a standalone pipeline role

`pipeline.Service`'s RPC methods resolve the caller's tenant via `tenant.FromContext(ctx)`. Inside a single process (`all`, or `cmd/crosscodex`'s embedded mode), that context value is populated once by `internal/gateway`'s mTLS auth interceptor and flows straight through an in-process Go call. Once gateway and pipeline are split into separate roles, that `ctx` value cannot cross the network on its own.

`pkg/tenant/connectutil` closes this gap: `gateway.NewConnectPipelineBackend`'s client attaches the ambient tenant (and user, if present) as an outgoing `x-tenant-id`/`x-user-id` header on every RPC; `internal/pipeline.Server` runs the matching server-side interceptor, which fails closed with `CodeUnauthenticated` if that header is missing or malformed. `pipeline`'s own listener additionally requires mTLS (`RequireAndVerifyClientCert`) — in practice, only the gateway's service identity can reach it at all. `pipeline` does not re-run end-user authentication; it trusts the network boundary (mTLS) plus the explicit, required tenant header asserted by whichever process already authenticated the caller.

## See also

- [Cryptographic Attestation Guide](attestation.md) for the attestation model referenced by the pipeline/all precondition above.
- [Telemetry Guide](telemetry.md) and [Audit Streams Guide](audit-streams.md) for observability across roles.
