# Service Runtime

This document covers how `crosscodexd` picks a service role at startup, what each role wires up, and what an operator must configure before running the roles that touch tenant data.

## Overview

`crosscodexd` is a single binary that can run as one process (`all`, the default) or split into independently deployable pieces (`gateway`, `worker`, `graph`). The role determines which services `cmd/crosscodexd/bootstrap.go` constructs and which resources (Postgres pools, NATS client, LLM client) it needs to hold open.

## Roles

`pkg/config/role.go` defines the canonical roles crosscodexd can actually start, plus a set of aliases that resolve to one of them:

| Role                              | What it runs                                                                 | Notes |
|------------------------------------|-------------------------------------------------------------------------------|-------|
| `all` (default)                    | Graph service + LLM worker + gateway (HTTP server + pipeline service)        | Everything in one process; see `attachGraph`, `attachWorker`, and `attachGateway` in `cmd/crosscodexd/bootstrap.go` |
| `gateway`                           | Gateway HTTP server (`internal/gateway.Server`) and a production `pipeline.Service`, dispatching analyzer work to workers over NATS | Enforces the fail-closed preconditions below |
| `worker`                            | LLM worker (`internal/worker.Worker`) consuming tasks from the NATS queue group | No Postgres connection is opened for this role — a worker replica scales horizontally without database credentials |
| `graph`                             | Graph-materialization service (`internal/graph.Service`), subscribing to NATS and writing to the AGE-backed graph database | |
| `pipeline`, `analysis`, `synthesis` | Alias for `gateway`                                                          | See [Aliases](#aliases-pipeline-analysis-synthesis) below |

`config.ResolveRole` validates a requested role name against `config.CanonicalRoles` (`all`, `gateway`, `worker`, `graph`) and the alias map, returning an error listing every accepted value if the input matches neither.

### Aliases: `pipeline`, `analysis`, `synthesis`

`pipeline`, `analysis`, and `synthesis` are deployment-facing names that all resolve to the `gateway` role today. Pipeline job creation only happens in the process that embeds `pipeline.Service` and receives the `CreateJob` RPC — currently that's always the gateway role, since `internal/gateway.PipelineBackend` is wired directly to `pipeline.Service` in-process. There is no standalone, network-addressable pipeline service yet. Analysis and synthesis execute synchronously inside `pipeline.Service`, so they resolve the same way.

Making `pipeline` (and by extension `analysis`/`synthesis`) an independently deployable, network-addressable role is tracked separately: see [complytime-labs/crosscodex#132](https://github.com/complytime-labs/crosscodex/issues/132). Until that lands, deploying with `--role pipeline` (or `analysis`/`synthesis`) is equivalent to `--role gateway` — it does not give you a separate scaling unit.

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
- **`graph`** — `attachGraph` also starts a dedicated listener on `health.addr`, this one backed by a real check: it calls the app Postgres pool's `Health(ctx)` and fails the probe if the pool reports not connected.
- **`gateway`** and **`all`** — do *not* use the dedicated listener. `internal/gateway.Server` already serves its own built-in `/healthz`. For `all`, `bootstrap` explicitly sets `rt.healthServer = nil` after wiring graph/worker/gateway, because the gateway's `/healthz` already covers process liveness for every co-located service in that process — binding `health.addr` twice (once per role's `attachX` call) would either fail outright or leave a redundant listener running.

## Fail-closed preconditions for `gateway` / `all`

`attachGateway` in `cmd/crosscodexd/bootstrap.go` refuses to start unless three things are configured, returning an error immediately rather than starting in a partially-usable state:

1. **Attestation key paths** — `attestation.private_key_path` and `attestation.public_key_path` (`pkg/config.AttestationConfig.PrivateKeyPath` / `PublicKeyPath`) must both be set. `pipeline.New` requires a real `attestation.Generator`, and `attestation.NewGenerator` is constructed from an `attestation.FileKeyProvider` pointed at these two paths — an operator must have a signing keypair on disk (or otherwise provisioned to those paths) before running `gateway` or `all`.
2. **Default tenant** — `tenants.default_tenant` (`pkg/config.TenantsConfig.DefaultTenant`) must be set. `storage.NewLocal` binds the storage provider to exactly one tenant at construction time (a known single-tenant storage limitation shared with `cmd/crosscodex`'s embedded mode), so the gateway can't start without knowing which tenant's storage namespace to use.
3. **Storage base path** — `storage.objects.base_path` (`pkg/config.ObjectStorageConfig.BasePath`) must be set. It defaults to `""` (`pkg/config/defaults.go`), and `storage.NewLocal` resolves an empty root via `filepath.Abs("")`, which silently resolves to the process's current working directory. Without this check, a `gateway`/`all` daemon started with default config would write tenant object storage under `<cwd>/<tenant>/` instead of failing fast.

Because `all` runs `attachGateway` as part of its startup sequence, all three preconditions apply to `all` as well as `gateway`.

## See also

- [Cryptographic Attestation Guide](attestation.md) for the attestation model referenced by the gateway precondition above.
- [Telemetry Guide](telemetry.md) and [Audit Streams Guide](audit-streams.md) for observability across roles.
