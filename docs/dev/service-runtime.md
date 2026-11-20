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
| `gateway`               | Gateway HTTP server (`internal/gateway.Server`). Serves catalog import itself (X.509 authn, ingestion, catalog service), forwards jobs to a separately-deployed `pipeline` role and graph, crosswalk and edge-cleaning RPCs to a `graph` role, each over a Connect client | Requires `pipeline.endpoint` and `graph.endpoint`; enforces the fail-closed preconditions below; see [Catalog import in a split deployment](#catalog-import-in-a-split-deployment)                                                                                                                      |
| `pipeline`              | A production `pipeline.Service` behind its own Connect RPC listener (`internal/pipeline.Server`), dispatching analyzer work to workers over NATS            | Requires `pipeline.addr`; enforces the fail-closed preconditions below                                                                                                                         |
| `worker`                | LLM worker (`internal/worker.Worker`) consuming tasks from the NATS queue group                                                                             | No Postgres connection is opened for this role — a worker replica scales horizontally without database credentials                                                                             |
| `graph`                 | Graph-materialization service (`internal/graph.Service`), subscribing to NATS and writing to the AGE-backed graph database. Also projects Control nodes and PARENT_OF edges from catalog parsed events, and serves the gateway's read-only graph, crosswalk and edge-cleaning RPCs on `graph.addr` | Requires `graph.addr`; enforces the fail-closed preconditions in [Graph role listener](#graph-role-listener) |
| `analysis`, `synthesis` | Alias for `pipeline`                                                                                                                                        | See [Aliases](#aliases-analysis-synthesis) below                                                                                                                                               |

`config.ResolveRole` validates a requested role name against `config.CanonicalRoles` (`all`, `gateway`, `pipeline`, `worker`, `graph`) and the alias map, returning an error listing every accepted value if the input matches neither.

Only `all` mounts `AdminService` on its gateway server (`attachAllGateway` sets `gateway.ServerConfig.Admin`; `attachGateway` and the embedded daemon do not), so a client calling it on the `gateway` role or the embedded daemon gets `Unimplemented`. `EdgeCleaningService` is mounted by `all` (`attachAllGateway` sets `gateway.ServerConfig.Cleaning` to the in-process engine) and by `gateway` (`attachGateway` sets it to `graphBackend.Cleaning()`, which forwards to the graph role). The embedded daemon sets none, so `gateway.NewServer` mounts a stub that answers `Unavailable` with the fixed text `edge cleaning needs the crosscodexd daemon (role all, or gateway with a graph role)`. `EdgeCleaningService` gets the same interceptors and `publicErrorInterceptor` as `AdminService`, because `internal/cleaning` returns raw store and graph errors. The process that runs the engine (`attachAllGateway` for `all`, `attachGraphListener` for `graph`; both through `newCleaningEngine`) builds the cleaning signer from `attestation.private_key_path` and `attestation.public_key_path`, independent of `attestation.enabled`: with either path empty the engine runs without a signer and refuses apply and decide; with both set, keys that do not load stop startup. See [Edge Cleaning](cleaning.md).

### Aliases: `analysis`, `synthesis`

`analysis` and `synthesis` are deployment-facing names that resolve to the `pipeline` role: both execute synchronously inside `pipeline.Service`, so whichever process actually runs `pipeline.Service` (the standalone `pipeline` role, or `all`) is what executes them. There is no plan to split them into their own NATS-decoupled services — see [complytime-labs/crosscodex#132](https://github.com/complytime-labs/crosscodex/issues/132)'s "out of scope" section.

## Selecting a role

Three ways to set the role, in precedence order (highest first):

1. **`--role` CLI flag** — `crosscodexd --role worker`. Takes priority over everything else.
2. **`CROSSCODEX_ROLE` environment variable / `role` config field** — the `role` key in `pkg/config`'s layered config (file, then env overlay under the `CROSSCODEX` prefix, so `CROSSCODEX_ROLE=worker`).
3. **Default** — `all`, set in the embedded config defaults (`pkg/config/defaults.go`: `role: all`).

`cmd/crosscodexd/main.go`'s `effectiveRole(flagRole, cfgRole string) string` implements this: it returns `flagRole` if non-empty, otherwise `cfgRole` (which has already had the config-file/env/default layers merged and validated by the time `main` sees it). If the flag differs from the already-resolved config role, `main` re-resolves it through `config.ResolveRole` so a flag-supplied alias (e.g. `--role pipeline`) is canonicalized the same way the config loader would. `main` then applies the shared-NATS check below to the resulting effective role.

## Shared NATS server for split roles

Split roles (`gateway`, `pipeline`, `worker`, `graph`, and the `analysis`/`synthesis` aliases) reach each other only over NATS, so every one of them must set the same `nats.url` (`CROSSCODEX_NATS_URL`). An empty `nats.url`, the default, makes `pkg/natsbus` start a private embedded NATS server inside the process: each split role would boot and report healthy while no import, job or task crossed to another role. `crosscodexd`'s `main` refuses that by calling `config.RequireSharedNATS` once, on the effective role (the `--role` flag, else the configured role), before `bootstrap` opens any resource. So `CROSSCODEX_ROLE=gateway crosscodexd --role all` still starts as `all`. Config loading does not apply the check: the `crosscodex` CLI, `crosscodexd healthcheck` and the `admin` subcommands load the same configuration without starting a role. The error names the role, says why the embedded server cannot work, and points at `nats.url` / `CROSSCODEX_NATS_URL` or setting the role to `all` (`--role all` or `CROSSCODEX_ROLE=all`). Only role `all`, a single process, may run with an empty `nats.url`.

## Gateway backends by role

`gateway.NewService` serves only the backends it is given; an RPC whose backend is absent returns `Unavailable`. For graph queries, crosswalks and edge cleaning the message is fixed: `<graph queries | crosswalks | edge cleaning> needs the crosscodexd daemon (role all, or gateway with a graph role)`.

| Role | Mappings and candidates backend (`ListJobMappings`, `GetControlMappings`, `ListJobCandidates`) | Graph backend (`QueryGraph`, `FindSimilar`) and crosswalk backend (`GetCrosswalk`) |
|------|------|------|
| `all` | `results.Service` via `newMappingsBackend`, in `buildIngestGateway` | `rt.graphService`, which `attachGraph` builds first; `attachAllGateway` passes it to `buildIngestGateway` |
| `gateway` | `results.Service` via `newMappingsBackend`, in `buildIngestGateway` | `gateway.NewConnectGraphBackend`, a Connect client to the graph role at `graph.endpoint`, passed through `gateway.WithGraphBackend` and `gateway.WithCrosswalkBackend` |
| embedded CLI daemon (`cmd/crosscodex/embedded.go`) | `results.Service` over its tenant connection | not attached |

The feedback backend (`SubmitVote`, `GetReviewQueue`) is not implemented in any role; both RPCs return `Unavailable`.

### Mappings read path

Mappings come from PostgreSQL, not the graph. `internal/results.PGStore` reads `vote_summaries` (rows with analyzer name `relationship`, for the `synthesized` stage) and `analysis_results` (analyzer `relationship`, for the `analyzer` stage) in one tenant transaction on the restricted application role, so RLS confines the read to the caller's tenant. `vote_summaries` holds one row per analyzer contribution (primary key `job_id, source_id, target_id, analyzer_name`), so a pair with both `requires` and `relationship` results does not collide.

`results.Service` implements `gateway.MappingsBackend` and `gateway.CandidatesBackend` (the same instance serves both); the assertions live in `cmd/crosscodexd/bootstrap.go` because `internal/results` must not import `internal/gateway`. Storage sits behind the `results.Reader` interface (`ListMappings(ctx, Filter) (Page, error)` and `ListCandidates(ctx, CandidateFilter) (CandidatePage, error)`). `PGStore` is the only implementation; a different store replaces only the `Reader` passed to `results.NewService`. An implementation must confine reads to the tenant in `ctx` and return `ErrJobNotFound`, `*JobNotCompletedError` and `ErrInvalidPageToken` for the corresponding cases, which `Service` maps to Connect errors. `GetControlMappings` uses the same reader at the synthesized stage across the tenant's completed jobs; it rejects `limit` outside 0..1000 and returns no `next_page_token` (the request has no `page_token`), so callers wanting more page through `ListJobMappings`.

## Health checks

The `health.addr` config field (`pkg/config.HealthConfig.Addr`, default `:9091` from `pkg/config/defaults.go`) configures a dedicated `GET /healthz` HTTP listener, built by `newHealthServer` in `cmd/crosscodexd/health.go`.

Which roles use it:

- **`worker`**, **`pipeline`** and **`graph`** — `attachWorker`, `attachPipeline` and `attachGraph` start a dedicated plain-HTTP listener on `health.addr`, serving `healthCheckHandler` with the checks `roleHealthChecks` (`cmd/crosscodexd/health.go`) returns for the role.
- **`gateway`** and **`all`** — do *not* use the dedicated listener. `internal/gateway.Server` serves its own `/healthz` on `server.addr`, and `buildIngestGateway` passes the same `roleHealthChecks` to `gateway.NewPoolAdminBackend` as extra `DependencyCheck`s. For `all`, `bootstrap` explicitly sets `rt.healthServer = nil` after wiring graph/worker/gateway, so `health.addr` is inert there. `internal/pipeline.Server`'s own RPC listener has no `/healthz` of its own — every RPC on it requires mTLS, so health checks go through the separate dedicated listener above instead of carving an unauthenticated path out of the RPC listener.

Each role checks only the dependencies it uses (see `resourcesForRole`), so a failing check means the role cannot do its job:

| Role | `database` | `nats` | `graph_database` |
|------|------------|--------|------------------|
| `worker` | | yes | |
| `pipeline` | yes | yes | |
| `graph` | yes | yes | yes |
| `gateway` | yes (`PoolAdminBackend`) | yes | |
| `all` | yes (`PoolAdminBackend`) | yes | yes |

`database` is the app pool's `Health` (`dbPingCheck`); `nats` is `natsbus.Client.Connected`, which reads local connection state (false while the client reconnects, see `nats.reconnect_wait` / `nats.max_reconnects`, and after `Close`); `graph_database` is `PingContext` on the graph `*sql.DB`. The LLM is deliberately **not** a health dependency: restarting a process does not fix a down LLM, the per-job LLM probe already reports `llm_unavailable` on the job, and Praxis does not serve `/v1/models`, so a generic probe would mark a working deployment unhealthy.

The dedicated handler stops at the first failing check and returns 503 with `{"status":"unhealthy","error":"<name> unreachable"}`. The check's error is logged, never written to the body (CWE-209): `/healthz` is unauthenticated and a driver error can carry hosts, ports and roles.

`runtime.start` binds `health.addr` with `net.Listen` before it returns and serves the bound listener in a goroutine, so a bind failure (every split role defaults to `:9091`, so two roles on one host collide) fails startup with an error naming `health.addr` instead of leaving the role running with no health endpoint.

`crosscodexd healthcheck` (`cmd/crosscodexd/healthcheck.go`) resolves the role like the daemon does (`--role`, else the `role` config value) and probes `http://127.0.0.1:<health.addr port>/healthz` for `graph`, `pipeline` and `worker`, or `https://127.0.0.1:<server.addr port>/healthz` with the configured TLS trust for `gateway` and `all`. Only the port of either bind address is used.

The gateway's `/healthz` and its `Health` RPC share one source: both call `Service.Health`, which delegates to the `AdminBackend`. `gateway.PoolAdminBackend` (used by the embedded daemon and the `all` and `gateway` roles) pings the app pool and reports it as the `database` entry in `HealthCheckResponse.services`, plus one entry per extra `DependencyCheck` (`nats`, and `graph_database` for `all`; the embedded daemon passes none), each with a fixed `<name> reachable` / `<name> unreachable` message; any failing entry makes the overall status `UNHEALTHY`. The dependency error is logged by the daemon and never returned (CWE-209). The CLI's connection gate (`healthCheck` in `cmd/crosscodex/connect.go`) fails closed: any overall status other than `HEALTHY` is an error that names each non-healthy dependency, so a daemon that answers but can't reach its database is never treated as usable. `/healthz` (`healthzHandler` in `internal/gateway/server.go`) applies the same rule: it returns 503 unless the overall status is `HEALTHY`, with the `HealthCheckResponse` JSON as the body so an operator can see which dependency failed. If the health call itself errors, it returns 503 with a fixed `{"status":"unhealthy"}` body and logs the error, since `/healthz` is unauthenticated (CWE-209). It is a readiness signal, not bare process liveness, so `crosscodexd healthcheck` (the compose healthcheck, which checks only the status code) reports the container unhealthy while the database is unreachable.

## Database startup checks

Roles that open Postgres never migrate. Run `crosscodexd migrate` first (see [Database Migrations](migrations.md#crosscodexd-migrate)). At startup the daemon refuses to run when:

- `database.dsn`, `database.graph_dsn` or, for role `all` and `admin retention scan` when it is set, `database.purge_dsn` connects as a superuser, a `BYPASSRLS` role, a table owner, or a member of one (`pkg/db.CheckRestrictedRole`).
- the schema is not at the latest embedded migration version, or the last migration failed (`pkg/db.CheckSchemaVersion`, on the `database.dsn` connection).
- role `all` or `gateway` finds no `tenants` row for `tenants.default_tenant`, or finds it deleted. `crosscodexd migrate` provisions it.

## Fail-closed preconditions for `pipeline` / `all`

`buildPipelineService` in `cmd/crosscodexd/bootstrap.go` refuses to construct a `pipeline.Service` unless three things are configured, returning an error immediately rather than starting in a partially-usable state. This applies to the `pipeline` role directly, and to `all` (which calls `buildPipelineService` as part of its startup sequence):

1. **Attestation keys** — for every scope that enables attestation (`attestation.enabled`, and each `attestation.tenant_overrides` entry, resolved with `ForTenant`), the private and public key paths must both be set, load, and be one key pair, on a FIPS-approved curve when `tls.fips.enabled` is set. `pipeline.NewAttestors` checks this (`AttestationConfig.RequireKeys`, then `attestation.LoadFileKeys`, which reads each pair once) before anything else. A scope with attestation disabled needs no keys and reads none; the daemon logs at startup which scopes are unattested. See [Pipeline Attestation](attestation.md#pipeline-attestation-fail-closed).
2. **Default tenant** — `tenants.default_tenant` (`pkg/config.TenantsConfig.DefaultTenant`) must be set. `storage.NewLocal` binds the storage provider to exactly one tenant at construction time (a known single-tenant storage limitation shared with `cmd/crosscodex`'s embedded mode), so pipeline can't start without knowing which tenant's storage namespace to use.
3. **Storage base path** — `storage.objects.base_path` (`pkg/config.ObjectStorageConfig.BasePath`) must be set. It defaults to `""` (`pkg/config/defaults.go`), and `storage.NewLocal` resolves an empty root via `filepath.Abs("")`, which silently resolves to the process's current working directory. Without this check, a `pipeline`/`all` daemon started with default config would write tenant object storage under `<cwd>/<tenant>/` instead of failing fast.

## Fail-closed preconditions for `pipeline` and `gateway`, when run as separate roles

- **`pipeline`** additionally requires `pipeline.addr` (the bind address for its standalone Connect RPC listener) — `attachPipeline` fails closed if it's unset. It also requires mutual TLS: `attachPipeline` resolves the `pipeline-server` TLS target and fails closed unless it produces a `mutual`-mode config with a CA, because `pipeline.Server` enforces `RequireAndVerifyClientCert` on every RPC and `rpcserver.Listen` would otherwise fall back to plaintext when TLS is off.
- **`gateway`** has the following requirements:
  - **Pipeline endpoint** — `pipeline.endpoint` (where its Connect client dials to reach a separately-deployed `pipeline` role) must be set. `attachGateway` fails closed if unset.
  - **Graph endpoint** — `graph.endpoint` (`host:port` of a separately-deployed `graph` role's listener) must be set. `attachGateway` fails closed if unset, with an error that names the RPCs the graph role serves. `gateway.NewConnectGraphBackend` resolves the `graph-client` TLS target and, like the pipeline client, fails closed unless it presents a client certificate and carries a CA.
  - **Mutual TLS for client** — `gateway.NewConnectPipelineBackend` resolves the `pipeline-client` TLS target and fails closed unless it yields a config that both presents a client certificate and carries a CA to verify the pipeline's server certificate.
  - **Why mutual TLS is required** — This mirrors the `pipeline` server-side precondition. Because the pipeline listener enforces `RequireAndVerifyClientCert` and has no plaintext fallback, a client that couldn't present a certificate would only fail at the first RPC. Both halves of the split refuse to start without genuine mutual TLS rather than degrading to cleartext.
  - **Catalog import preconditions** — the gateway serves catalog import itself, so `buildIngestGateway` (shared with `all`) refuses to start unless:
    - `tenants.default_tenant` is set (`gateway role: tenants.default_tenant must be set`);
    - `storage.objects.base_path` is set (`gateway role: storage.objects.base_path must be set`). Uploaded documents are stored here, on the gateway's own filesystem;
    - the `gateway-server` TLS target resolves to `mutual` mode with a CA (`gateway role: catalog import authenticates clients by X.509 certificate, so tls.mode (or tls.targets.gateway-server.mode) must be "mutual" with a CA configured`), because the listener otherwise never verifies a client certificate;
    - the default tenant exists (`gateway role: default tenant "<id>" does not exist. Run crosscodexd migrate ...`) and is not deleted (`gateway role: default tenant "<id>" is deleted. Restore it with crosscodexd admin tenant restore ...`).
  - **Resources** — Postgres (`database.dsn`) and NATS. The gateway holds no graph or vector credentials; it reaches the graph through the graph role.
  - **No pipeline service preconditions** — it does not need the attestation keys from `buildPipelineService`; it constructs no `pipeline.Service`, only a network client to one.
- **`all`** needs neither `pipeline.addr` nor `pipeline.endpoint` — it wires `pipeline.Service` directly into the gateway's `PipelineBackend` in-process, with no listener or client involved.

## Catalog import in a split deployment

With `gateway`, `pipeline`, `worker` and `graph` running as separate processes, a catalog import flows through two roles. Both must point `nats.url` at the same NATS server; startup refuses a split role with an empty `nats.url` (see [Shared NATS server for split roles](#shared-nats-server-for-split-roles)):

1. A client uploads the document to the `gateway` role. The gateway owns ingestion: it authenticates the client certificate, stores the uploaded blob under its own `storage.objects.base_path`, parses the document and upserts the `controls` rows in Postgres. It never writes the graph.
2. After upserting a non-empty control set, the catalog service publishes `crosscodex.catalog.<tenant_id>.<catalog_id>.parsed` (`natsbus.CatalogParsedSubject`). If publishing fails, the import fails with `CodeUnavailable` and a message to re-import the same document; the rows and blob stay.
3. The event is stored in the `MATERIALIZE` work-queue stream. The `graph` role consumes it with the durable consumer `graph-materializer` (`internal/graph/catalog_projection.go`), reads the tenant's `controls` rows under RLS and upserts the Control nodes (`UpsertNode`) and PARENT_OF edges. The event payload is ignored; the subject supplies the tenant and catalog, and must name the tenant the message's provenance headers name. A malformed subject is terminated and counted as `status=skipped`, a tenant mismatch as `status=rejected`. A PARENT_OF edge whose parent node is missing is skipped and counted (`graph.materialize.edges_skipped.total`). Zero stored controls, a reader error or a write error is counted as `graph.events.total` `status=error` and redelivered up to `nats.materialize.max_deliver` times.
4. Job starts are forwarded to the `pipeline` role as described in [Tenant propagation](#tenant-propagation-between-gateway-and-a-standalone-pipeline-role).

Operational consequences:

- **Eventual consistency.** Control nodes appear after the import returns. An analysis event with an edge whose Control node does not exist yet is retried, every `nats.materialize.redeliver_delay`, until its last delivery (`nats.materialize.max_deliver`), and only then is the edge skipped and counted. Re-importing the catalog restores the Control nodes; starting a new analysis of it (`crosscodex run start --source <catalog-id> --target <catalog-id>`) rewrites the skipped edges.
- **Events wait for the graph role.** `MATERIALIZE` keeps an event until a graph role handles it, for up to `nats.streams.materialize_retention`, so an event published while no graph role runs is materialized when one starts.
- **`ack_wait` must exceed one event's materialization time.** The graph role sends no progress heartbeat: an event still being written after `nats.materialize.ack_wait` is redelivered and handled again concurrently. Writes are idempotent, so this adds nothing to the graph, but it spends a delivery.
- **Storage.** Because the gateway writes uploaded blobs under its own `storage.objects.base_path`, that path is separate from the pipeline role's unless the two roles share a filesystem.
- Role `all` runs the same ingestion path and additionally confirms the tenant graph exists at startup (`CreateGraph`).

### Client certificate trust

Trust is two checks. The TLS handshake admits only a certificate the `gateway-server` CA signed; `pkg/tlsconfig` uses the target's CA as both the root pool and the client CA pool (`ClientCAs`) for a `mutual` server target, and `tls.targets.<target>.ca` overrides the global CA for that target. Then `pkg/authn.X509Authenticator`, built from `auth.x509_mappings` by `allTenantAuthRegistry` (`cmd/crosscodexd/bootstrap.go`), gives the certificate the roles of the first mapping whose match it satisfies (every non-empty field, as a `filepath.Match` glob). A certificate that matches no mapping gets no identity and the call fails `Unauthenticated`; there is no implicit role. Roles are `admin` and `reviewer` (`authn.KnownRoles`), and `admin` does not include `reviewer`, so the cleaning two-person rule holds only while no one certificate is mapped to both.

`tenants.enabled` picks the tenant: `false` puts every identity in `tenants.default_tenant` (a mapping's `tenant` must be empty or equal it), `true` takes the matching mapping's `tenant`. Roles `all` and `gateway` refuse to start with no mappings, or with a mapping `NewX509Authenticator` rejects, before the default-tenant check.

The embedded CLI daemon (`cmd/crosscodex/embedded.go`) builds its registry the same way from one mapping: the exact common name and organization of the client certificate in its PKI directory, glob characters escaped, with role `admin`. The daemon's own server certificate and any other certificate its CA signs match nothing.

Mappings are globs, so a broad one (`cn: "*"`) admits every certificate the CA signs. In a split deployment the inter-service CA also signs service identities such as `pipeline-client`; map exact subjects, and give `gateway-server` a dedicated client CA through `tls.targets.gateway-server.ca`, separate from the inter-service CA, so a service identity cannot reach the gateway at all.

## Graph role listener

Role `graph` runs a Connect RPC listener (`internal/graph.Server`, built by `attachGraphListener` in `cmd/crosscodexd/bootstrap.go` after `attachGraph`) so a `gateway` role can serve graph, crosswalk and edge-cleaning RPCs without graph credentials. Role `all` serves the same RPCs in process and runs no listener. The listener mounts:

- `GraphService`, read-only: `Traverse`, `Query` and `SimilaritySearch`. Every other `GraphService` RPC returns `Unimplemented` with the fixed text `graph writes go through materialization; this listener serves the gateway's read RPCs only`, because only the NATS materializers write the graph.
- `CrosswalkService.GetCrosswalk` (`crosscodex/v1/crosswalk.proto`). It is a separate service because `gateway.proto` declares the `Crosswalk*` messages and imports `graph.proto`, so `GraphService` cannot carry the RPC. The gateway resolves the request (the tenant's completed jobs and the catalogs it names) and the graph role only reads the graph.
- `EdgeCleaningService`, backed by `pkg/cleaning.Engine` and `internal/cleaning.Service`, so the role checks and the tenant-match rule in [Edge Cleaning](cleaning.md#handler) apply unchanged.

`attachGraphListener` refuses to start, naming the key to fix, unless all of these hold. The configuration checks run before the database is touched:

- `graph.addr` is set (`host:port`; `pkg/config` rejects a value `net.SplitHostPort` cannot split, with a non-empty port).
- The `graph-server` TLS target resolves to `mutual` mode with a CA (`graph role: tls.mode (or tls.targets.graph-server.mode) must be "mutual" with a CA configured ...`). `rpcserver.Listen` would otherwise serve plaintext when TLS is off.
- `tls.service_identities` is non-empty (`errNoServiceIdentities("graph", "gateway")`): list only the gateway's outbound client certificate; the daemon cannot check how broad the list is.
- `tenants.default_tenant` is set, and exists and is not deleted (`checkDefaultTenant`).
- `pipeline.endpoint` is set. Edge cleaning compares each stored stage record with the one the stage would get now, and only the pipeline role knows each stage's current prompt and model revisions.

The graph role asks for them with `PipelineService.GetCurrentStageRecord` (`pipeline.proto`; served by `internal/pipeline/stage_record_rpc.go`, called through `remoteStageRecord` in `cmd/crosscodexd/stage_record_client.go`). It is service-to-service only: the gateway does not expose it, and the request's `tenant_context.tenant_id` must equal the authenticated connection's tenant (`PermissionDenied` otherwise). The pipeline role must therefore list the graph role's client certificate in its own `tls.service_identities`. The dial uses `gateway.NewPipelineClient`, so TLS target `pipeline-client` must resolve to mutual TLS with a client certificate, key and CA (`mtlsHTTPClient` refuses otherwise). When the call fails, the graph role logs the cause and returns a fresh `Unavailable` error that names `pipeline.endpoint` (visible only to a caller dialing the graph listener directly; through the gateway `publicError` replaces it with an error reference, and the gateway log keeps the text) and never wraps the pipeline's error, so a pipeline-side `PermissionDenied` or `NotFound` does not reach the API caller as its own code (connect-go sends the first `connect.Error` in the chain).

Health checks use the separate listener on `health.addr` (see [Health checks](#health-checks)), not this one.

### Identity assertion between gateway and a split role

The pipeline and graph listeners trust the gateway to have authenticated the end user. The gateway's Connect clients carry that identity in request headers, built by `pkg/tenant/connectutil`:

| Header | Meaning |
|--------|---------|
| `x-tenant-id` | Tenant of the request (always sent) |
| `x-user-id` | The end user's subject (`authn.Identity.Subject`) |
| `x-user-roles` | The user's roles, comma-separated (`authn.Identity.Roles`) |

`connectutil.UnaryServerInterceptor(identities)` honors the headers only from a client certificate matching one of `tls.service_identities` (see [Tenant propagation](#tenant-propagation-between-gateway-and-a-standalone-pipeline-role)). On top of that:

- User and roles travel together or not at all. Both present: the interceptor puts an `authn.Identity` and the user into the context, so the receiving service's role checks (for cleaning: admin, reviewer) apply to the asserted user, not to the gateway's certificate. Neither present: tenant only. Exactly one, a repeated or empty `x-user-id`, a repeated `x-user-roles`, an empty role list, an empty list element or a role outside `authn.KnownRoles` is `InvalidArgument`.
- The client side (`connectutil.UnaryClientInterceptor`) fails closed with `FailedPrecondition`, without sending the request, when the context has no tenant, has a user but no `authn.Identity`, has a user that differs from the identity's subject, or has an identity with an empty subject. Dropping the user instead would widen the per-user row scope (`pkg/db`) on the far side. A client certificate mapped by organization or organizational unit alone has no subject, so such a certificate fails gateway to pipeline and gateway to graph calls until it carries a CN.
- Streaming RPCs are rejected (`Unimplemented`) on these services.

A service identity can still assert any user and role it likes. The trust boundary is the certificate: list only the gateway's client certificate, and with `tenants.enabled: true` give the gateway a dedicated one.

## Job completion needs a graph role

A pipeline job completes only once its graph is written: the graph stage stores a barrier behind the job's materialization events and waits for a `graph` role (or `all`) to answer it (see [Graph Materialization Outcomes](audit-streams.md#graph-materialization-outcomes)). A split deployment therefore needs a running `graph` role on the same NATS server as the `pipeline` role. Without one, every job fails at its graph stage after `pipeline.graph_materialize_timeout` (default 30m, at most `pipeline.stage_timeout`) with a message naming the timeout and the missing graph role. Its events stay in `MATERIALIZE`, so a graph role started later writes them; re-run the job to complete it. A barrier's `not_before` is the run's start by the pipeline host's clock, compared with the timestamps the NATS server gives stored outcomes, so keep pipeline and NATS hosts NTP-synced: skew can make a barrier count an earlier run's `failed` outcome or ignore this run's.

## Tenant propagation between gateway and a standalone pipeline role

`pipeline.Service`'s RPC methods resolve the caller's tenant via `tenant.FromContext(ctx)`. Inside a single process (`all`, or `cmd/crosscodex`'s embedded mode), that context value is populated once by `internal/gateway`'s mTLS auth interceptor and flows straight through an in-process Go call. Once gateway and pipeline are split into separate roles, that `ctx` value cannot cross the network on its own.

`pkg/tenant/connectutil` closes this gap: `gateway.NewConnectPipelineBackend`'s client attaches the ambient tenant (and user, if present) as an outgoing `x-tenant-id`/`x-user-id` header on every RPC. `internal/pipeline.Server` runs the matching server-side interceptor, which fails closed with `CodeUnauthenticated` if that header is missing or malformed.

Those headers assert who the caller is, so a certificate the CA signed is not enough to set them: the CA also signs worker, graph and operator certificates, and any of them could otherwise name any tenant. The interceptor honors the headers only when the peer's client certificate (from `authn.TLSStateMiddleware`, which wraps the pipeline mux) matches an entry of `tls.service_identities`. A certificate matching none gets `CodePermissionDenied` naming its CN, no TLS state or peer certificate gets `CodeUnauthenticated`, and the handler is never reached. Entries use the same matcher fields and globs as `auth.x509_mappings`; list the gateway's outbound client certificate (`{cn: client, organization: CrossCodex}` in `deploy/config.yaml`) and nothing broader. The shipped deployment uses one `client.pem` for every role, for the operator, and as the gateway's service identity, so there service identities separate nothing: anyone holding that certificate can assert any tenant. With `tenants.enabled: true`, give the gateway a dedicated client certificate, issued to no user and no other role, and list it alone in `tls.service_identities`. The pipeline role refuses to start with an empty list, and with a `tenants.default_tenant` that is missing or deleted. Streaming RPCs are rejected (`CodeUnimplemented`) because the pipeline service defines none. `pipeline` does not re-run end-user authentication; it trusts the gateway, identified by its certificate, to have authenticated the caller.

## See also

- [Cryptographic Attestation Guide](attestation.md) for the attestation model referenced by the pipeline/all precondition above.
- [Telemetry Guide](telemetry.md) and [Audit Streams Guide](audit-streams.md) for observability across roles.
