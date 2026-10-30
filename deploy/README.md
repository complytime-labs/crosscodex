# CrossCodex Production Deployment (compose)

Single-host production stack running all CrossCodex services with full mutual TLS authentication.

## Prerequisites

- **Container engine**: podman (recommended) or docker with compose support
- **Go**: for generating TLS certificates via `task deploy:certs`
- **Task**: build orchestration (install via `go install github.com/go-task/task/v3/cmd/task@latest`)

## Setup

### 1. Configure environment variables

```bash
cp deploy/.env.example deploy/.env
```

Edit `deploy/.env` and fill in these required values:

| Variable                         | Purpose                                                                                            | Example                    |
|----------------------------------|----------------------------------------------------------------------------------------------------|----------------------------|
| `POSTGRES_PASSWORD`              | Password of the `crosscodex` owner (superuser); only `db` and the `migrate` service receive it (hex: `openssl rand -hex 24`) | `9b4d...` (48 hex chars)   |
| `APP_USER_PASSWORD`              | Password for `app_user`, the role crosscodexd uses for relational data (hex: `openssl rand -hex 24`) | `3f9c...` (48 hex chars)   |
| `GRAPH_USER_PASSWORD`            | Password for `graph_user`, the role crosscodexd uses for the AGE graph (hex: `openssl rand -hex 24`) | `a71e...` (48 hex chars)   |
| `OPENAI_API_KEY`                 | Upstream model provider key (OpenAI, Azure, etc.) that the LLM gateway uses to reach the real model API | `sk-proj-...`              |
| `CROSSCODEX_LLM_API_KEY`         | The key CrossCodex presents to the LLM gateway (checked by LiteLLM only; see [LLM gateway](#llm-gateway-llm_gateway)). Must match a virtual key in `deploy/litellm/config.yaml`  | `sk-crosscodex-to-litellm` |
| `CROSSCODEX_LLM_DEFAULT_MODEL`   | Model alias the daemon requests for general inference (must exist in the gateway config: `deploy/litellm/config.yaml` or `deploy/praxis/config.yaml`) | `default`                  |
| `CROSSCODEX_LLM_EMBEDDING_MODEL` | Model alias the daemon requests for embeddings (must exist in the gateway config: `deploy/litellm/config.yaml` or `deploy/praxis/config.yaml`)        | `embed`                    |
| `TAG`                            | Container image tag to run (optional)                                                              | `latest`                   |

**Security note**: Never commit real secrets. The `.env` file is gitignored by default.

`POSTGRES_PASSWORD` and the role passwords are embedded in `postgres://` URLs, so they must be URL-safe: use hex. `task deploy:up` refuses any of the three that contains a character other than `A-Z a-z 0-9 . _ ~ -`. The `migrate` service sets them on every start of the stack, so changing one in `.env` and running `task deploy:up` again rotates it.

### Database roles

crosscodexd runs as the restricted roles `app_user` (relational data, subject to row-level security) and `graph_user` (the AGE graph). At startup it refuses a superuser, a `BYPASSRLS` role or the schema owner, and a schema that is not at the latest migration version.

The one-shot `migrate` service runs `crosscodexd migrate` before the daemon starts, as the `crosscodex` owner. It applies the schema migrations, sets the password of each restricted role from the password in that role's DSN (`app_user`, `graph_user`, and `tenant_admin` and `backup_user` when their DSNs are set in `.env`), and provisions the default tenant. The DSNs are assembled in `deploy/compose.yaml`'s `environment:`; `deploy/config.yaml` holds none. Only `db` and `migrate` receive `POSTGRES_PASSWORD`.

podman-compose waits for `migrate` to exit but does not check its exit code. If `migrate` fails, crosscodexd still refuses to start (the schema is not at the latest version, its role password is not set, or the default tenant is missing), so when the daemon will not come up, check `task deploy:logs -- migrate` first.

### 2. Generate TLS certificates

The production stack enforces mutual TLS across all services (PostgreSQL, NATS, the LLM gateway, and the CrossCodex gateway). Generate the PKI and load it into the `crosscodex-certs` volume:

```bash
task deploy:certs
```

This creates:
- `deploy/certs/ca.pem` — Certificate Authority root
- `deploy/certs/client.pem` + `deploy/certs/client-key.pem` — client certificate and key
- `deploy/certs/server.pem` + `deploy/certs/server-key.pem` — server certificate and key
- `deploy/certs/attestation-key.pem` + `deploy/certs/attestation-pub.pem` — ECDSA P-256 keypair the pipeline uses to sign in-toto attestations

The server certificate's Subject Alternative Name (SAN) covers `localhost` and `127.0.0.1`, so host-to-localhost connections work.

Certificates are copied into the `crosscodex-certs` docker/podman volume with shared-group ownership (GID 10001) so every service can read the keys despite running as different users (postgres, nats=root, stunnel=nobody, crosscodexd=nonroot).

## Bring up the stack

```bash
task deploy:up
```

This command:
1. Builds container images (`task deploy:build` via dependency)
2. Starts all services with `compose up -d`
3. Polls `https://localhost:50051/healthz` until the gateway is healthy (up to 5 minutes)

Success output:

```
Waiting for gateway /healthz ...
Gateway healthy at https://localhost:50051
Next: crosscodex config set cli.endpoint https://localhost:50051
```

The gateway is now reachable at `https://localhost:50051` with mutual TLS required.

### Faster builds during development

By default the images are built hermetically: the daemon and CLI are compiled inside a `golang` builder stage (`deploy/Dockerfile`), which pins the Go version and regenerates protobuf code, so the result is reproducible and CI-identical.

For a faster inner loop, pass `LOCAL=1` to any `deploy:*` task:

```bash
task deploy:up LOCAL=1      # also works with deploy:build and deploy:smoke
```

This builds the binaries on your host (`task dev:build`, with `CGO_ENABLED=0` and `GOOS/GOARCH` pinned to the container target) and copies them into thin `distroless/static` images (`deploy/Dockerfile.local`), skipping the in-container Go build. It's safe here specifically because the production binaries are static — there is no libc or dynamic-linkage mismatch to worry about.

Trade-off: `LOCAL=1` uses your host Go toolchain and the protobuf code already committed to the tree, so it is not hermetic. Keep the default (hermetic) path for release images and CI; use `LOCAL=1` only for local iteration.

## First use: connect the CLI from the host

The `crosscodex` CLI running on your host machine needs the client certificate to dial the production gateway. Use the `--endpoint`, `--tls-ca`, `--tls-cert`, and `--tls-key` flags to authenticate via mutual TLS:

```bash
# Import a catalog
crosscodex \
  --endpoint https://localhost:50051 \
  --tls-ca deploy/certs/ca.pem \
  --tls-cert deploy/certs/client.pem \
  --tls-key deploy/certs/client-key.pem \
  catalog import catalogs/NIST_SP-800-53_rev5_catalog.json

# List imported catalogs
crosscodex \
  --endpoint https://localhost:50051 \
  --tls-ca deploy/certs/ca.pem \
  --tls-cert deploy/certs/client.pem \
  --tls-key deploy/certs/client-key.pem \
  catalog list
```

**Tip**: Set these flags once in your user config (`~/.config/crosscodex/config.yaml`) or via `crosscodex config set` to avoid repeating them:

```bash
crosscodex config set cli.endpoint https://localhost:50051
crosscodex config set tls.ca deploy/certs/ca.pem
crosscodex config set tls.cert deploy/certs/client.pem
crosscodex config set tls.key deploy/certs/client-key.pem
crosscodex config set tls.mode mutual
```

## Architecture: mutual TLS and LiteLLM via stunnel

The production stack runs all inter-service communication over mutual TLS:

- **PostgreSQL**: TLS-enabled with client-cert verification via `pg_hba.conf`
- **NATS**: TLS with `--tlsverify`
- **LiteLLM gateway** (`LLM_GATEWAY=litellm`, the default): LiteLLM's native TLS mode is server-only, so a `litellm-tls` stunnel sidecar terminates mutual TLS on port 4443 and forwards to LiteLLM's plaintext backend on port 4000. This provides end-to-end mTLS for the entire stack.
- **Praxis gateway** (`LLM_GATEWAY=praxis`): Praxis requires the client certificate on its own listener (`praxis:4443`), so it needs no sidecar.
- **CrossCodex gateway**: Serves mTLS at `:50051`, published as `localhost:50051` on the host

All services share the same PKI (loaded from the `crosscodex-certs` volume).

## LLM gateway (`LLM_GATEWAY`)

The daemon reaches the model provider through an OpenAI-compatible gateway. Pick it with `LLM_GATEWAY` on any `deploy:*` task; the default is `litellm`.

| `LLM_GATEWAY` | Compose fragment | mTLS | Daemon auth to gateway | Config |
|---|---|---|---|---|
| `litellm` (default) | `compose.gateway-litellm.yaml` | `litellm-tls` stunnel sidecar | client certificate **and** `CROSSCODEX_LLM_API_KEY` (LiteLLM `master_key`) | `litellm/config.yaml` |
| `praxis` (spike) | `compose.gateway-praxis.yaml` (plus `compose.gateway-praxis-models.yaml` with `PRAXIS_MODELS_ENDPOINT=true`) | Praxis itself | client certificate only | `praxis/config.yaml` (`praxis/config.models.yaml` with `PRAXIS_MODELS_ENDPOINT=true`) |

```bash
task deploy:up LLM_GATEWAY=praxis
task deploy:smoke LLM_GATEWAY=praxis
```

### Persisting the choice

Set `LLM_GATEWAY` and `PRAXIS_MODELS_ENDPOINT` in `deploy/.env` and `deploy:up` and `deploy:logs` use them on every later run (`deploy:down` removes both gateways whatever is set), so a plain `task deploy:up` keeps the gateway you deployed. Precedence: command-line variable, then environment, then `deploy/.env`, then the default. Write the values unquoted with no spaces around `=` (`LLM_GATEWAY=praxis`); an `export` prefix is allowed. `deploy:smoke` ignores `deploy/.env`: it uses only the command-line or environment value, and `litellm` if neither is set.

### Switching gateways

1. Change `deploy/.env`, or pass the variable on the command line.
2. `task deploy:down`. It removes both gateways' containers whichever one is selected, and prints "no container ... found" errors for the gateway that isn't running. Those are expected.
3. `task deploy:up`.

Switching to Praxis may also need regenerated certificates (see below).

### Praxis gateway

Praxis AI support is a spike: Praxis is alpha software (v0.8.0, configuration may change before v1.0). Known differences from LiteLLM:

- `CROSSCODEX_LLM_API_KEY` is sent but not checked (Praxis has no stable bearer-key filter); the client certificate is the only credential. The value is replaced with `OPENAI_API_KEY` before the provider hop.
- Compose requirement: `LLM_GATEWAY=praxis` needs podman-compose 1.6.0 or later. `deploy:up` refuses older versions and other compose tools, and says why. Praxis is a third-party image, so it mounts only `ca.pem`, `server.pem` and `server-key.pem` from the certs volume (compose `subpath` mounts), never the CA key or the daemon's client key. A compose tool that ignores `subpath` would expose the whole volume, which is why the check exists. Upgrade with `pip install --user --upgrade podman-compose`, or use `LLM_GATEWAY=litellm`.
- Only `POST` reaches the provider. Any other method gets 405, including `GET /v1/models` unless the opt-in below is on.
- Client headers `OpenAI-*`, `X-Forwarded-*`, `Forwarded`, `Cookie`, `X-Real-IP`, `x-api-key` and `api-key` are stripped before the provider hop. This is a denylist, so any other client header is forwarded.
- `GET /v1/models` is off by default. `PRAXIS_MODELS_ENDPOINT=true` turns it on: for example, `task deploy:up LLM_GATEWAY=praxis PRAXIS_MODELS_ENDPOINT=true` (or set both in `deploy/.env`, and pass the same values to `deploy:logs` if you don't). Praxis then starts on `praxis/config.models.yaml`. The endpoint drops query strings and refuses (HTTP 400) a request that carries a body. To serve `/v1/models` there, that config disables Praxis's startup check for conflicting cluster selectors (`insecure_options.skip_pipeline_checks.conflicting_cluster_selectors`), so a later config edit that introduces a real conflict won't be caught at startup. Keep `config.models.yaml` in sync with `config.yaml`. The flag has no effect with `litellm` (which always serves the endpoint) or with `deploy:smoke`.
- Retries and double billing: `retry_policy` retries only 429, 503 and connection failures before the request was sent, with jittered backoff (up to 1 s, then up to 2 s); it does not retry resets or other 5xx, and it ignores `Retry-After`. Praxis does still replay a request that fails on a stale pooled upstream connection, so a POST can be billed twice. `idle_timeout_ms: 1` in `praxis/config.yaml` narrows that window at the cost of about one TLS handshake per request, but under concurrent load a replay (and a second charge) remains possible. The daemon's analysis engine also re-dispatches failed tasks (`analysis.engine.max_retries`), which multiplies upstream attempts (tracked in issue #237). The comments in `praxis/config.yaml` are the source of truth for the proxy side.
- A request for a model not in the Praxis config fails with an empty HTTP 500 (LiteLLM returns a 4xx). See [Troubleshooting](#empty-http-500-from-praxis).
- The server certificate must name `praxis`. Certificates generated before Praxis support don't, and `deploy:up` keeps any existing PKI that still verifies, so force a regeneration: `task deploy:down`, then `rm deploy/certs/.fingerprint`, then `task deploy:up LLM_GATEWAY=praxis`. Data volumes are kept. The regeneration mints a new CA, new client certificates for the CLI and a new attestation keypair, so keep a copy of the old `deploy/certs/attestation-pub.pem` if you need to verify attestations signed before the change.
- The smoke test on Praxis supports only the local baked Ollama (no `OLLAMA_HOST` override). It also probes the Praxis listener directly, published on `127.0.0.1:${SMOKE_PRAXIS_PORT}` (default 54443): a trusted client certificate must be served, and no certificate, an untrusted-CA certificate and an unmapped model must all be refused. The unmapped-model probe sends a model that Ollama serves but Praxis doesn't map, so a pass-through regression would fail the smoke.
- The image is amd64 only.

### Startup check

Whichever gateway you use, crosscodexd sends one request to `llm.gateway_url` at startup and refuses to start if it can't connect (DNS, connect, TLS or mTLS failure, or a timeout). Any HTTP response counts as reachable, whatever its status. See [Troubleshooting](#crosscodexd-exits-with-cannot-reach-llm-gateway).

Running compose by hand always needs the gateway fragment, e.g. `podman-compose -f compose.yaml -f compose.gateway-litellm.yaml ps`.

## Operations

### View logs

Tail logs from all services:

```bash
task deploy:logs
```

Or tail a specific service (pass service names via `CLI_ARGS`):

```bash
task deploy:logs -- crosscodexd
```

### Run smoke test

Container-driven end-to-end smoke test (bring up the stack, catalog import round-trip over mTLS, tear down):

```bash
task deploy:smoke
```

**Offline by default — no API key required.** Unlike `deploy:up` (which routes the gateway to real OpenAI), the smoke test backs the stack with a local Ollama provider so it runs self-contained. It builds a baked-model image (`llama3.2:1b` + `granite-embedding:30m`, reused from the integration stack), renders a smoke-only gateway config that points the `default`/`embed` aliases at Ollama, and layers an offline overlay onto `compose.yaml` plus the gateway fragment (`deploy/compose.offline.yaml` for LiteLLM, `deploy/compose.offline-praxis.yaml` for Praxis). Production `deploy/compose.yaml` is untouched and stays OpenAI-backed. Your `deploy/.env` still needs `CROSSCODEX_LLM_DEFAULT_MODEL=default` and `CROSSCODEX_LLM_EMBEDDING_MODEL=embed`, but `OPENAI_API_KEY` / `CROSSCODEX_LLM_API_KEY` may be any non-empty placeholder (Ollama ignores them).

To point the smoke test at an existing Ollama instead of the baked container, pass `OLLAMA_HOST` (and optionally the model names):

```bash
task deploy:smoke OLLAMA_HOST=http://your-host:11434 \
  OLLAMA_CHAT_MODEL=llama3.2:1b OLLAMA_EMBED_MODEL=granite-embedding:30m
```

The `OLLAMA_HOST` override works with `LLM_GATEWAY=litellm` only. The host is probed and its models pulled before the stack starts, so an unreachable provider or missing model fails fast. `OLLAMA_HOST` may omit the scheme (`your-host:11434`); `http://` is assumed.

To use an Ollama running on your **host machine**, point at loopback — `OLLAMA_HOST=localhost:11435` (any port other than the baked default `11434`). LiteLLM runs in the compose bridge network, so `smoke_up.sh` rewrites a `localhost`/`127.0.0.1` host to `host.containers.internal` when rendering the LiteLLM config, and the `compose.offline-remote.yaml` overlay maps that name to the host gateway. This needs podman ≥ 4.1 (or a docker-compose that supports `host-gateway`). A `localhost` override is reachable from the host for the pre-flight probe but only reachable from the container via this rewrite — without it, `localhost` inside the container is the container itself.

The smoke test is fully isolated and ephemeral, so it can run alongside a live production stack:

- It runs under its own compose project (`crosscodex-smoke`), with its own data volumes, its own PKI volume (`crosscodex-smoke-certs`) generated from a throwaway host cert dir, and its own published gateway port (`50151`, leaving production's `50051` untouched).
- Everything — containers, data volumes, the cert volume, and the temporary PKI dir — is torn down on exit, whether the test passes, fails, or is interrupted. Nothing survives the run.

The isolation is driven entirely by environment variables (`COMPOSE_PROJECT_NAME`, `CROSSCODEX_CERTS_VOLUME`, `CROSSCODEX_GATEWAY_PORT`, `CERTS_DIR`) that the same `deploy:*` tasks and `compose.yaml` read; their defaults reproduce the production stack.

`deploy:smoke` is a thin wrapper over two tasks you can also run on their own to debug a failing run:

```bash
task deploy:smoke:up     # bring up the isolated stack + run the round-trip, then LEAVE it running
task deploy:smoke:down   # tear the isolated stack down: drop its volumes and throwaway PKI
```

`deploy:smoke` runs `smoke:up` and always runs `smoke:down` afterward (via a deferred command), so the stack is cleaned up whether the run passes, fails, or is interrupted. Run `smoke:up` alone to keep the stack up for inspection (`task deploy:logs`, exec into a container), then `smoke:down` when finished. Both act on the same isolated project (`crosscodex-smoke`), so `smoke:down` never touches a live production stack.

### Stop the stack

Stop all services but preserve data volumes:

```bash
task deploy:down
```

To stop **and** delete data volumes (PostgreSQL data, NATS JetStream, object storage):

```bash
task deploy:down VOLUMES=1
```

**Warning**: `VOLUMES=1` is destructive and deletes all catalogs, analysis results, and objects.

## Backups

CrossCodex backs up PostgreSQL (WAL-G base backups plus continuous WAL archiving, which enables point-in-time recovery), every tenant's object store, and the three JetStream audit streams into versioned backup points on a local or S3 destination. Backup requires an external NATS server (`nats.url` set); it refuses in embedded mode.

### Enable

1. Copy the WAL-G env file (used by the `db` service and the restore one-shot) and edit it for your destination:

   ```bash
   cp backup.env.example backup.env
   ```

2. Uncomment `CROSSCODEX_BACKUP_DSN` in `.env` (see `.env.example`) and replace `change-me` with a real hex password; it is a `postgres://` URL for `backup_user`.

   The `migrate` service sets `backup_user`'s password from that DSN when the stack starts; no manual `ALTER ROLE` is needed.

3. Bring the stack up with the compose files. Every backup and restore command below names the LiteLLM fragment; if you run `LLM_GATEWAY=praxis`, use `compose.gateway-praxis.yaml` in its place (add `-f compose.gateway-praxis-models.yaml` after it if you also use `PRAXIS_MODELS_ENDPOINT=true`):

   ```bash
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml up -d
   ```

`backup.config.yaml` (mounted into `crosscodexd`) and `backup.env` (WAL-G's settings on `db`) must name the **same destination** — keep both in step when switching between local and S3.

### Encryption requirement (security)

Backups hold a second copy of all tenant evidence; protect the destination like primary data:

- **S3**: enable SSE-KMS on the bucket and attach a bucket policy that denies unencrypted `PutObject` and non-TLS requests.
- **Local**: the volume layout is group 10001 only, mode `2770`; keep the host directory backing the volume at mode `0700`.
- Client-side encryption is **not implemented yet** — destination-side encryption (above) is the only protection today.

### Run and verify

```bash
docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml exec crosscodexd /crosscodexd admin backup run
docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml exec crosscodexd /crosscodexd admin backup list
docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml exec crosscodexd /crosscodexd admin backup verify [--point ID]
```

Exit codes: `0` ok, `1` failure or stale, `2` usage error. `verify` reports a store stale once its newest backup point is older than `backup.max_age.postgres` / `.objects` / `.nats` (default `26h` each).

There is no in-daemon scheduler yet (sub-project 2 of issue #36); run `backup run` from cron or a systemd timer until it lands.

`backup run` and a non-dry-run `retention scan` share one global advisory lock and exclude each other — the lock is global, so two non-dry-run retention scans (any tenants) also exclude each other. Whichever side loses the race exits `1` with `backup/retention in progress, retry later`; retry it once the winner finishes. A `retention scan --all-tenants` scans tenants one at a time, so it never contends with itself, but a tenant whose scan loses the race to a backup is reported as failed and the run exits `1` after the remaining tenants.

### Restore

1. Stop crosscodexd:

   ```bash
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml stop crosscodexd
   ```

2. Pick a backup point:

   ```bash
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml run --rm --no-deps crosscodexd admin backup list
   ```

   For point-in-time recovery, pick a target time `T` and use the newest point whose objects/NATS capture windows end at or before `T` (Postgres itself recovers to the exact time `T` via `--target-time` below).

3. Recreate an empty pgdata volume:

   ```bash
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml stop db
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml rm -f db
   docker volume rm <project>_crosscodex-pgdata
   ```

4. Restore Postgres from the point's `base_...` backup name (see it in the `list` output):

   ```bash
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml --profile restore run --rm db-restore <base_...> [--target-time T]
   ```

5. Start the database and wait for recovery to finish:

   ```bash
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml up -d db
   ```

   Poll `SELECT pg_is_in_recovery();` (as the `crosscodex` superuser) until it returns `f`.

6. Restore objects and audit streams:

   ```bash
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml run --rm --no-deps crosscodexd admin backup restore --point <id>
   ```

7. Start crosscodexd and confirm:

   ```bash
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml up -d crosscodexd
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml -f compose.backup.yaml exec crosscodexd /crosscodexd admin backup verify
   ```

### Consistency model

Postgres is captured first and is authoritative. The objects and JetStream steps that follow it may end up slightly ahead — never behind — because the backup/retention advisory lock blocks purges for the whole run, so nothing a completed Postgres capture already references can disappear before the later steps copy it. The AGE graph needs no separate backup: it is a materialized view rebuilt from Postgres.

### Troubleshooting

| Message                                                     | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                             |
|-------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `refusing to restore: ... is not empty`                     | Restore refuses to write over an existing target, so a refusal changes nothing: `crosscodex-db-restore` checks that PGDATA is empty; `admin backup restore` checks that each tenant's object store is empty (`already holds N object(s)`) and that no target JetStream stream already exists (`already exists on the NATS server`). Recreate the pgdata volume, empty the tenant's object prefix, or delete the stream, then retry. |
| `JetStream backup and restore need an external NATS server` | `nats.url` is empty (embedded mode). Point it at an external NATS server; backup and restore do not support the embedded server.                                                                                                                                                                                                                                                                                                    |
| `verify` reports `FAILURE` under WAL CHECK                  | An archived WAL segment is missing: point-in-time recovery across the gap is impossible. Take a new `backup run` to restart the WAL chain. A gap within the newest `WALG_UPLOAD_CONCURRENCY` segments (default 16) instead reports `WARNING` ("probably uploading") and does not fail verify.                                                                                                                                       |
| `backup is disabled`                                        | `backup.destination.backend` is empty. Set it to `local` or `s3` and set `backup.dsn` (or `CROSSCODEX_BACKUP_DSN`), then retry.                                                                                                                                                                                                                                                                                                     |

## Tenant administration

`crosscodexd admin tenant` creates, lists, suspends, resumes, deletes and restores tenants, and `crosscodexd admin reviewer` registers the people who record verdicts, as the `tenant_admin` role. That role holds no table privileges; it can only call the `SECURITY DEFINER` tenant administration functions of the initial schema.

1. Uncomment `CROSSCODEX_DATABASE_TENANT_ADMIN_DSN` in `.env` (see `.env.example`) and replace `change-me` with a real hex password, then run `task deploy:up` again. The `migrate` service sets `tenant_admin`'s password from that DSN, and the recreated `crosscodexd` container picks up the variable.

2. Run the commands inside the container, from `deploy/`. They name the LiteLLM fragment; for `LLM_GATEWAY=praxis` use the Praxis fragments as in [Backups](#enable):

   ```bash
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml exec crosscodexd /crosscodexd admin tenant list
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml exec crosscodexd /crosscodexd admin tenant create --tenant acme --display-name "Acme Corp"
   docker compose -f compose.yaml -f compose.gateway-litellm.yaml exec crosscodexd /crosscodexd admin tenant suspend --tenant acme
   ```

A suspended tenant's requests are refused with `PermissionDenied` until `admin tenant resume`.

Tenants and reviewers are never removed; they are soft-deleted, and a deleted one can be hidden behind a pseudonym. `adjudicate artifacts decide` accepts only registered reviewers. The identity commands (`admin tenant delete|restore|hide|unhide`, `admin reviewer add|list|delete|restore|hide|unhide`, `admin identity trace|export`) run the same way. All but `reviewer list` and `identity export` need `--by` and `--reason` and are signed with the attestation keypair in `deploy/certs/` (`attestation.private_key_path` and `attestation.public_key_path` in `deploy/config.yaml`), plus the same `CROSSCODEX_DATABASE_TENANT_ADMIN_DSN`. For example:

```bash
docker compose -f compose.yaml -f compose.gateway-litellm.yaml exec crosscodexd /crosscodexd admin reviewer add --tenant acme --reviewer jdoe --by alice --reason "joined review team"
docker compose -f compose.yaml -f compose.gateway-litellm.yaml exec crosscodexd /crosscodexd admin tenant delete --tenant acme --by alice --reason "contract ended"
docker compose -f compose.yaml -f compose.gateway-litellm.yaml exec crosscodexd /crosscodexd admin tenant restore --tenant acme --by alice --reason "contract renewed"
```

`admin identity export` writes the whole identity audit log as JSON Lines to a new file (mode `0600`; an existing file is never overwritten). The file holds the real IDs of hidden tenants and reviewers, so treat it as sensitive. Write it to the container's `/tmp` and copy it out; the container copy is discarded when the container is removed (for example by a compose `down`):

```bash
docker compose -f compose.yaml -f compose.gateway-litellm.yaml exec crosscodexd /crosscodexd admin identity export --out /tmp/identity-audit.jsonl
docker compose -f compose.yaml -f compose.gateway-litellm.yaml cp crosscodexd:/tmp/identity-audit.jsonl ./identity-audit.jsonl
chmod 600 ./identity-audit.jsonl
```

A deleted tenant is refused like a suspended one, and also by every service. `tenant restore` brings it back as `suspended`; run `admin tenant resume` to reactivate it. See [Tenant administration](../README.md#tenant-administration) for what each command does.

The gateway checks tenant status with one uncached database query per request or stream, on the same pool as application queries (`database.max_conns`, default 10). Suspension therefore takes effect on the next request. Under heavy concurrency, raise `database.max_conns`: an exhausted pool refuses requests with `Unavailable`.

## Decommissioning

The identity audit log (`identity_events`) is permanent: no command deletes it, and the schema teardown (`001_initial_schema.down.sql`) refuses while it holds any record. To retire a deployment:

1. Export the audit log as shown in [Tenant administration](#tenant-administration) and store the file securely; it holds the real IDs of hidden tenants and reviewers. Each line keeps the signed `request` and `attestation` bytes, so events stay verifiable with the attestation public key, which you should keep alongside it.
2. Take a final backup if you need the rest of the data (see [Backups](#backups)).
3. Remove the database with its volume: `docker compose -f compose.yaml -f compose.gateway-litellm.yaml down -v` (run from `deploy/`; use the Praxis fragments instead if you ran `LLM_GATEWAY=praxis`), or `DROP DATABASE` on an external server. Dropping the database never runs the schema teardown.

## What's next

The single-host compose stack is suitable for evaluation and small-scale production use. For multi-host deployments, distributed role assignment, or FIPS 140 container images, see issue [#17](https://github.com/complytime-labs/crosscodex/issues/17).

## Troubleshooting

### Gateway health check fails

If `task deploy:up` times out waiting for `/healthz`:

1. Check recent logs: `task deploy:logs -- crosscodexd`
   `/healthz` returns 503 until every dependency the gateway reports (currently the database) is healthy. In the JSON body `status` fields are numeric enum values; `services.database.message` carries the reason (`database unreachable`). To read it, run `curl -s --cacert deploy/certs/ca.pem https://localhost:50051/healthz`.
2. Verify the `.env` file is populated (especially `POSTGRES_PASSWORD`, `APP_USER_PASSWORD`, `GRAPH_USER_PASSWORD` and `CROSSCODEX_LLM_API_KEY`), and that `migrate` succeeded: `task deploy:logs -- migrate`
3. Ensure no other service is bound to port 50051 on the host: `ss -lntp | grep 50051`
4. Verify the certs volume exists and is populated: `podman volume inspect crosscodex-certs` (or `docker volume inspect`)

### CLI connection refused

If the CLI fails to connect from the host:

- Verify the gateway container is running: `podman ps | grep crosscodexd` (or `docker ps`)
- Check that the published port is bound: `ss -lntp | grep 50051`
- Confirm the CA and client certificate paths are correct (relative to your current directory)
- Use `--verbose` or `--debug` on the CLI command for detailed connection logs

### crosscodexd exits with `cannot reach LLM gateway`

The daemon could not connect to `llm.gateway_url` at startup. Likely causes:

- The gateway fragment wasn't loaded. This happens with hand-run compose commands that name only `compose.yaml`; add `-f compose.gateway-litellm.yaml` or `-f compose.gateway-praxis.yaml` (plus `-f compose.gateway-praxis-models.yaml` with `PRAXIS_MODELS_ENDPOINT=true`). `task deploy:*` loads the right fragment itself.
- The server certificate doesn't name the gateway (`praxis` or `litellm`). After switching to Praxis, regenerate the certificates as described under [Praxis gateway](#praxis-gateway).
- The gateway is down or failed to start: `task deploy:logs -- praxis` (or `litellm`, `litellm-tls`).

### Empty HTTP 500 from Praxis

Praxis answers an empty 500 when the requested model isn't in its config. Check that `CROSSCODEX_LLM_DEFAULT_MODEL` and `CROSSCODEX_LLM_EMBEDDING_MODEL` in `deploy/.env` name aliases listed under `model_to_provider` in `deploy/praxis/config.yaml` (`deploy/praxis/config.models.yaml` with `PRAXIS_MODELS_ENDPOINT=true`; the defaults are `default` and `embed`).

### LLM gateway configuration

The model aliases (`default`, `embed`) and upstream provider keys are configured in `deploy/litellm/config.yaml` or `deploy/praxis/config.yaml`, depending on `LLM_GATEWAY`. If you need to change the backing model or add a new provider, edit that file and restart the stack with the same `LLM_GATEWAY` (`task deploy:down LLM_GATEWAY=<gateway> && task deploy:up LLM_GATEWAY=<gateway>`).
