# Service Runtime

This document covers how `crosscodexd` picks a service role at startup, what each role wires up, and what an operator must configure before running the roles that touch tenant data.

## Overview

`crosscodexd` is a single binary that can run as one process (`all`, the default) or split into independently deployable pieces (`gateway`, `pipeline`, `worker`, `graph`). The role determines which services `cmd/crosscodexd/bootstrap.go` constructs and which resources (Postgres pools, NATS client, LLM client) it needs to hold open.

## Roles

> **Migrating from a pre-#132 deployment.** The role vocabulary changed. `--role pipeline` used to alias to `gateway`; it now starts a standalone process that requires its own `pipeline.addr` plus mutual TLS (`tls.mode: mutual` with a CA, or the equivalent `tls.targets.pipeline-server` override) — there is no unauthenticated fallback. `--role gateway` used to run the pipeline in-process; it now requires `pipeline.endpoint` pointed at a separately-running `pipeline` role, plus a mutual-TLS `pipeline-client` identity (client cert, key, and CA) to reach it — there is no cleartext fallback. Both fail loudly with an actionable error rather than silently misbehaving, so an operator upgrading from before this branch will see startup errors, not degraded behavior.

`pkg/config/role.go` defines the canonical roles crosscodexd can actually start, plus a set of aliases that resolve to one of them:

| Role                    | What it runs                                                                                                                                                | Notes                                                                                                                                                                                          |
|-------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `all` (default)         | Graph service + LLM worker + gateway (HTTP server + in-process pipeline service)                                                                            | Everything in one process, no network hop between gateway and pipeline; see `attachGraph`, `attachWorker`, `buildPipelineService`, and `attachGatewayServer` in `cmd/crosscodexd/bootstrap.go` |
| `gateway`               | Gateway HTTP server (`internal/gateway.Server`), whose pipeline backend is a Connect client reaching a separately-deployed `pipeline` role over the network | Requires `pipeline.endpoint`; enforces the fail-closed precondition below                                                                                                                      |
| `pipeline`              | A production `pipeline.Service` behind its own Connect RPC listener (`internal/pipeline.Server`), dispatching analyzer work to workers over NATS            | Requires `pipeline.addr`; enforces the fail-closed preconditions below                                                                                                                         |
| `worker`                | LLM worker (`internal/worker.Worker`) consuming tasks from the NATS queue group                                                                             | No Postgres connection is opened for this role — a worker replica scales horizontally without database credentials                                                                             |
| `graph`                 | Graph-materialization service (`internal/graph.Service`), subscribing to NATS and writing to the AGE-backed graph database                                  |                                                                                                                                                                                                |
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

## Health checks

The `health.addr` config field (`pkg/config.HealthConfig.Addr`, default `:9091` from `pkg/config/defaults.go`) configures a dedicated `GET /healthz` HTTP listener, built by `newHealthServer` in `cmd/crosscodexd/health.go`.

Which roles use it:

- **`worker`** — `attachWorker` starts a dedicated health listener on `health.addr`. It has no dependency to check (there's no NATS connectivity probe in `pkg/natsbus.Client` today), so it's a pure liveness probe — `healthCheckHandler` called with zero checks always reports `{"status": "healthy"}`.
- **`graph`** and **`pipeline`** — both start a dedicated listener on `health.addr`, backed by the shared `dbPingCheck` helper: it calls the app Postgres pool's `Health(ctx)` and fails the probe if the pool reports not connected.
- **`gateway`** and **`all`** — do *not* use the dedicated listener. `internal/gateway.Server` already serves its own built-in `/healthz`. For `all`, `bootstrap` explicitly sets `rt.healthServer = nil` after wiring graph/worker/gateway, because the gateway's `/healthz` already covers process liveness for every co-located service in that process. `internal/pipeline.Server`'s own RPC listener has no `/healthz` of its own — every RPC on it requires mTLS, so health checks go through the separate dedicated listener above instead of carving an unauthenticated path out of the RPC listener.

## Fail-closed preconditions for `pipeline` / `all`

`buildPipelineService` in `cmd/crosscodexd/bootstrap.go` refuses to construct a `pipeline.Service` unless three things are configured, returning an error immediately rather than starting in a partially-usable state. This applies to the `pipeline` role directly, and to `all` (which calls `buildPipelineService` as part of its startup sequence):

1. **Attestation key paths** — `attestation.private_key_path` and `attestation.public_key_path` (`pkg/config.AttestationConfig.PrivateKeyPath` / `PublicKeyPath`) must both be set. `pipeline.New` requires a real `attestation.Generator`, and `attestation.NewGenerator` is constructed from an `attestation.FileKeyProvider` pointed at these two paths — an operator must have a signing keypair on disk (or otherwise provisioned to those paths) before running `pipeline` or `all`.
2. **Default tenant** — `tenants.default_tenant` (`pkg/config.TenantsConfig.DefaultTenant`) must be set. `storage.NewLocal` binds the storage provider to exactly one tenant at construction time (a known single-tenant storage limitation shared with `cmd/crosscodex`'s embedded mode), so pipeline can't start without knowing which tenant's storage namespace to use.
3. **Storage base path** — `storage.objects.base_path` (`pkg/config.ObjectStorageConfig.BasePath`) must be set. It defaults to `""` (`pkg/config/defaults.go`), and `storage.NewLocal` resolves an empty root via `filepath.Abs("")`, which silently resolves to the process's current working directory. Without this check, a `pipeline`/`all` daemon started with default config would write tenant object storage under `<cwd>/<tenant>/` instead of failing fast.

## Fail-closed preconditions for `pipeline` and `gateway`, when run as separate roles

- **`pipeline`** additionally requires `pipeline.addr` (the bind address for its standalone Connect RPC listener) — `attachPipeline` fails closed if it's unset. It also requires mutual TLS: `attachPipeline` resolves the `pipeline-server` TLS target and fails closed unless it produces a `mutual`-mode config with a CA, because `pipeline.Server` enforces `RequireAndVerifyClientCert` on every RPC and `rpcserver.Listen` would otherwise fall back to plaintext when TLS is off.
- **`gateway`** requires `pipeline.endpoint` (where its Connect client dials to reach a separately-deployed `pipeline` role) — `attachGateway` fails closed if it's unset. It also requires mutual TLS on that client: `gateway.NewConnectPipelineBackend` resolves the `pipeline-client` TLS target and fails closed unless it yields a config that both presents a client certificate and carries a CA to verify the pipeline's server certificate. This mirrors the `pipeline` server-side precondition — because the pipeline listener enforces `RequireAndVerifyClientCert` and has no plaintext fallback, a client that couldn't present a certificate would only fail at the first RPC, so both halves of the split refuse to start without genuine mutual TLS rather than degrading to cleartext. A standalone `gateway` role needs *none* of the three `buildPipelineService` preconditions above: it no longer constructs a `pipeline.Service` itself, only a network client to one.
- **`all`** needs neither `pipeline.addr` nor `pipeline.endpoint` — it wires `pipeline.Service` directly into the gateway's `PipelineBackend` in-process, with no listener or client involved.

## Tenant propagation between gateway and a standalone pipeline role

`pipeline.Service`'s RPC methods resolve the caller's tenant via `tenant.FromContext(ctx)`. Inside a single process (`all`, or `cmd/crosscodex`'s embedded mode), that context value is populated once by `internal/gateway`'s mTLS auth interceptor and flows straight through an in-process Go call. Once gateway and pipeline are split into separate roles, that `ctx` value cannot cross the network on its own.

`pkg/tenant/connectutil` closes this gap: `gateway.NewConnectPipelineBackend`'s client attaches the ambient tenant (and user, if present) as an outgoing `x-tenant-id`/`x-user-id` header on every RPC; `internal/pipeline.Server` runs the matching server-side interceptor, which fails closed with `CodeUnauthenticated` if that header is missing or malformed. `pipeline`'s own listener additionally requires mTLS (`RequireAndVerifyClientCert`) — in practice, only the gateway's service identity can reach it at all. `pipeline` does not re-run end-user authentication; it trusts the network boundary (mTLS) plus the explicit, required tenant header asserted by whichever process already authenticated the caller.

## See also

- [Cryptographic Attestation Guide](attestation.md) for the attestation model referenced by the pipeline/all precondition above.
- [Telemetry Guide](telemetry.md) and [Audit Streams Guide](audit-streams.md) for observability across roles.
