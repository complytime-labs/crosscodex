# Running the smoke stack in a systemd-less dev container

Maintainer notes for `task deploy:smoke`, `deploy:smoke:up` and
`deploy:smoke:down` when the Docker or podman host is itself a container with
no systemd (the usual agent and CI sandbox). Each section gives the symptom, how
to detect it, and the fix. The user-facing smoke instructions are in
`deploy/README.md` ("Run smoke test").

## 1. Healthchecks never run, so `--condition=healthy` waits forever

- **Symptom:** a compose service gated with `condition: service_healthy`
  (see `deploy/compose.gateway-litellm.yaml`, `compose.gateway-praxis.yaml`,
  `compose.offline-praxis.yaml`) never starts, or `deploy:up` waits the full
  300 s and prints "Gateway did not become healthy in time".
- **Cause:** podman schedules healthchecks with systemd timers. Without systemd
  nothing fires them, so a container stays `starting` indefinitely.
- **Detect:** `podman ps --format '{{.Names}} {{.Status}}'` shows `(starting)`
  forever; `podman healthcheck run <container>` run by hand exits 0 once the
  service is up.
- **What the repo does today:** only the ingestion end-to-end suite
  (`test/ingestion`, `task test:integration:ingestion`) runs
  `podman healthcheck run` itself, every 5 s on its own project's containers
  while it brings a stack up. Everything else relies on the engine:
  `deploy:up` polls the gateway's `/healthz` from the host with `curl`
  (`.taskfiles/deploy.yml`), and `scripts/integration-health.sh` probes
  published ports from the host for the integration stacks, for the same
  reason. Neither marks a container healthy, so a service gated on
  `service_healthy` stays blocked.
- **Fix:** run the healthcheck yourself in a second shell while the stack comes
  up. The loop must use the same `TMPDIR` and `STORAGE_DRIVER` as the smoke
  stack (see section 2): podman's runroot follows `TMPDIR`, so a loop with a
  different value cannot see the containers and every `podman healthcheck run`
  fails silently. `test/deploy/podman_env.sh` sets both and needs `ROOT_DIR`
  (the repository root) in the environment:

  ```bash
  ROOT_DIR="$(git rev-parse --show-toplevel)"; export ROOT_DIR
  source "${ROOT_DIR}/test/deploy/podman_env.sh"
  while sleep 5; do for c in $(podman ps -q); do podman healthcheck run "$c" >/dev/null 2>&1; done; done
  ```

  Stop the loop when the stack is up. This is a workaround for the environment,
  not part of the tasks.

## 2. `TMPDIR` / `STORAGE_DRIVER` (build and teardown must agree)

- **Symptom:** image builds fail with "mounting an overlay over build context
  directory", or `deploy:smoke:down` leaves containers or volumes behind because
  it sees a different podman storage than the one `deploy:smoke:up` built in.
- **Cause:** inside a container, podman needs a buildah temp dir off the
  overlay mount and the `vfs` storage driver. `deploy/local_up.sh` (sourced by
  `smoke_up.sh`) exports
  `TMPDIR=<repo>/.test-output/buildtmp` and `STORAGE_DRIVER=vfs`; a task that
  talks to podman without them uses another runtime view.
- **Detect:** `bash scripts/in-container.sh && echo in-container` (exit 0 means
  a container marker was found). Compare `podman info --format '{{.Store.GraphDriverName}}'`
  in the environment used by each task.
- **Fix:** both `deploy/local_up.sh` (which `test/deploy/smoke_up.sh` sources)
  and the `deploy:smoke:down` and `deploy:local:down` tasks source
  `test/deploy/podman_env.sh`, the single definition. It acts only when
  `scripts/in-container.sh` reports a container, keeps any `TMPDIR` or
  `STORAGE_DRIVER` you already set, and creates the `TMPDIR` directory. Source
  it in any new task that runs podman against the smoke stack:
  `export ROOT_DIR=... && . test/deploy/podman_env.sh`. The Venom suite
  `test/tooling/smoke-down-env.venom.yml` checks that `deploy:smoke:down` gets the
  same values.

## 3. Shared rootless network: another compose stack breaks LLM calls

- **Symptom:** a long eval against the smoke stack suddenly hangs and LLM calls
  time out, after another compose stack (for example `task test:integration:*`)
  was started.
- **Cause:** rootless podman networking shares one `aardvark-dns` per user.
  Bringing up another stack can restart it, and the running smoke containers
  lose their route to a model server on the host.
- **Detect:** from the crosscodexd or gateway container, the host model server
  is unreachable (`podman exec <container> curl -m 5 <model-server-url>` times
  out) while the same URL works from the dev container itself; `pgrep -a aardvark-dns`
  shows a recently started process.
- **Fix:** stop only the smoke stack's containers, selected by their compose
  project label:
  `podman stop $(podman ps -q --filter label=com.docker.compose.project=crosscodex-smoke)`.
  `crosscodex-smoke` is the default smoke project (`SMOKE_PROJECT` overrides
  it); `podman ps --format '{{.Label "com.docker.compose.project"}} {{.Names}}'`
  lists the project of every running container. Do not use a bare
  `podman stop $(podman ps -q)` (it stops every container, including other
  stacks) or `task deploy:down` (without the smoke variables it targets the
  production project). Then kill `aardvark-dns`
  (`pkill aardvark-dns`), remove its state directory (rootless default
  `$XDG_RUNTIME_DIR/containers/networks/aardvark-dns`; confirm the path on your
  host, it was not present where this page was written because `aardvark-dns`
  was not installed), then start the containers again.
- **Avoid:** do not run integration suites while the smoke stack is serving a
  long eval.

## 4. Cold model exceeds the probe timeout

- **Symptom:** the first job after the stack starts fails with
  `llm_unavailable`; a retry succeeds.
- **Cause:** each job first probes the model. A server that loads models on
  demand can take longer to load a cold model than the probe allows.
- **Detect:** the failure appears once per cold model, and the model server's log
  shows the model loading at the time of the failed probe.
- **Fix:** retry once the model is loaded. To avoid it, raise `llm.probe_timeout`
  (seconds, global only; 0 uses `llm.timeout`; defined in `pkg/config/types.go`,
  documented in `deploy/README.md` and the top-level `README.md`), or keep the
  model resident on the server.

## 5. Memory and pids limits are configured but not enforced

- **Symptom:** the smoke stack's hardening check passes, yet a container uses
  more memory than its `mem_limit`.
- **Cause:** in a nested container podman runs with the `cgroupfs` manager
  and no delegated cgroup of its own, so each container stays in the dev
  container's cgroup: `--memory` and `--pids-limit` are recorded in the
  container config but no cgroup applies them.
- **Detect:** `podman inspect <container> --format '{{.HostConfig.Memory}}'`
  shows the limit, while `podman exec <container> cat /sys/fs/cgroup/memory.max`
  prints `max` (and `pids.max` the outer container's value).
- **What the repo does:** `test/deploy/smoke_up.sh` checks the configured
  limits through `inspect`, which is what compose controls; capabilities and
  no-new-privileges it reads from the kernel (`/proc/<pid>/status`), so those
  are proven here. Prove enforcement of the limits on a host with cgroup v2
  and delegated `memory` and `pids` controllers.
