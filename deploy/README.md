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

| Variable | Purpose | Example |
|----------|---------|---------|
| `POSTGRES_PASSWORD` | PostgreSQL superuser password for the `db` container | `a-strong-random-password` |
| `OPENAI_API_KEY` | Upstream model provider key (OpenAI, Azure, etc.) that LiteLLM uses to reach the real model API | `sk-proj-...` |
| `CROSSCODEX_LLM_API_KEY` | The key CrossCodex presents to LiteLLM (must match a virtual key in `deploy/litellm/config.yaml`) | `sk-crosscodex-to-litellm` |
| `CROSSCODEX_LLM_DEFAULT_MODEL` | Model alias the daemon requests for general inference (must exist in `deploy/litellm/config.yaml`) | `default` |
| `CROSSCODEX_LLM_EMBEDDING_MODEL` | Model alias the daemon requests for embeddings (must exist in `deploy/litellm/config.yaml`) | `embed` |
| `TAG` | Container image tag to run (optional) | `latest` |

**Security note**: Never commit real secrets. The `.env` file is gitignored by default.

### 2. Generate TLS certificates

The production stack enforces mutual TLS across all services (PostgreSQL, NATS, LiteLLM, and the CrossCodex gateway). Generate the PKI and load it into the `crosscodex-certs` volume:

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
- **LiteLLM gateway**: LiteLLM's native TLS mode is server-only, so a `litellm-tls` stunnel sidecar terminates mutual TLS on port 4443 and forwards to LiteLLM's plaintext backend on port 4000. This provides end-to-end mTLS for the entire stack.
- **CrossCodex gateway**: Serves mTLS at `:50051`, published as `localhost:50051` on the host

All services share the same PKI (loaded from the `crosscodex-certs` volume).

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

**Offline by default — no API key required.** Unlike `deploy:up` (which routes LiteLLM to real OpenAI), the smoke test backs the stack with a local Ollama provider so it runs self-contained. It builds a baked-model image (`llama3.2:1b` + `granite-embedding:30m`, reused from the integration stack), renders a smoke-only LiteLLM config that points the `default`/`embed` aliases at Ollama, and layers `deploy/compose.offline.yaml` onto the production `compose.yaml`. Production `deploy/compose.yaml` is untouched and stays OpenAI-backed. Your `deploy/.env` still needs `CROSSCODEX_LLM_DEFAULT_MODEL=default` and `CROSSCODEX_LLM_EMBEDDING_MODEL=embed`, but `OPENAI_API_KEY` / `CROSSCODEX_LLM_API_KEY` may be any non-empty placeholder (Ollama ignores them).

To point the smoke test at an existing Ollama instead of the baked container, pass `OLLAMA_HOST` (and optionally the model names):

```bash
task deploy:smoke OLLAMA_HOST=http://your-host:11434 \
  OLLAMA_CHAT_MODEL=llama3.2:1b OLLAMA_EMBED_MODEL=granite-embedding:30m
```

The host is probed and its models pulled before the stack starts, so an unreachable provider or missing model fails fast. `OLLAMA_HOST` may omit the scheme (`your-host:11434`); `http://` is assumed.

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

## What's next

The single-host compose stack is suitable for evaluation and small-scale production use. For multi-host deployments, distributed role assignment, or FIPS 140 container images, see issue [#17](https://github.com/complytime-labs/crosscodex/issues/17).

## Troubleshooting

### Gateway health check fails

If `task deploy:up` times out waiting for `/healthz`:

1. Check recent logs: `task deploy:logs -- crosscodexd`
2. Verify the `.env` file is populated (especially `POSTGRES_PASSWORD` and `CROSSCODEX_LLM_API_KEY`)
3. Ensure no other service is bound to port 50051 on the host: `ss -lntp | grep 50051`
4. Verify the certs volume exists and is populated: `podman volume inspect crosscodex-certs` (or `docker volume inspect`)

### CLI connection refused

If the CLI fails to connect from the host:

- Verify the gateway container is running: `podman ps | grep crosscodexd` (or `docker ps`)
- Check that the published port is bound: `ss -lntp | grep 50051`
- Confirm the CA and client certificate paths are correct (relative to your current directory)
- Use `--verbose` or `--debug` on the CLI command for detailed connection logs

### LiteLLM configuration

The LiteLLM model aliases (`default`, `embed`) and upstream provider keys are configured in `deploy/litellm/config.yaml`. If you need to change the backing model or add a new provider, edit that file and restart the stack (`task deploy:down && task deploy:up`).
