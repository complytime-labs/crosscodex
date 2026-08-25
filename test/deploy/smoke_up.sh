#!/usr/bin/env bash
# Bring-up half of the container-driven smoke test for the production compose
# stack. Brings up a fully isolated, ephemeral copy of the stack, imports a
# reference catalog through the gateway over mTLS using a host-built crosscodex
# CLI, and asserts it lists. It deliberately does NOT tear the stack down: teardown is
# the `deploy:smoke:down` task, so `deploy:smoke:up` can be run on its own to
# leave a live stack for debugging. The `deploy:smoke` task pairs this with a
# deferred `deploy:smoke:down` that runs whether this passes or fails.
#
# Isolation (so the smoke stack can run alongside a real production stack):
#   * COMPOSE_PROJECT_NAME    — separate project → own pod/containers/network
#                               and own (auto-namespaced) data volumes.
#   * CROSSCODEX_CERTS_VOLUME — own PKI volume, generated fresh.
#   * CROSSCODEX_GATEWAY_PORT — own published host port (production keeps 50051).
#   * CERTS_DIR               — throwaway host dir for the smoke PKI.
# All four are consumed by deploy/compose.yaml and the deploy:* tasks; the
# defaults reproduce the production stack, so only the smoke test overrides them.
# The `deploy:smoke:*` tasks export these from the SMOKE_* taskfile vars; the
# defaults below only apply when this script is run directly.
set -euo pipefail

ROOT_DIR="${ROOT_DIR:-$(git rev-parse --show-toplevel)}"
OUT_DIR="${ROOT_DIR}/.test-output/deploy-smoke"
FIXTURE="${ROOT_DIR}/cmd/crosscodexd/testdata/oscal/e2e-minimal-catalog.json"
mkdir -p "${OUT_DIR}"

# Podman overlay workaround: point buildah temp to a workspace subdir to avoid
# "mounting an overlay over build context directory" errors. Detect container
# execution (/.dockerenv exists, or cgroup shows container runtime).
if [ -f /.dockerenv ] || grep -qE 'docker|containerd|lxc|kubepods' /proc/1/cgroup 2>/dev/null || grep -qE 'docker|containerd|lxc|kubepods' /proc/self/cgroup 2>/dev/null; then
  export TMPDIR="${TMPDIR:-${ROOT_DIR}/.test-output/buildtmp}"
  export STORAGE_DRIVER="${STORAGE_DRIVER:-vfs}"
  mkdir -p "${TMPDIR}"
fi

# Isolate this stack from any production stack sharing the same host. The values
# come from the environment (set by the deploy:smoke:* tasks); the defaults keep
# a direct `bash smoke_up.sh` invocation isolated too. CERTS_DIR is a fixed path
# under .test-output (not an mktemp) so deploy:smoke:down can find and remove the
# same PKI dir in a separate task invocation.
: "${COMPOSE_PROJECT_NAME:=crosscodex-smoke}"
: "${CROSSCODEX_CERTS_VOLUME:=crosscodex-smoke-certs}"
: "${CROSSCODEX_GATEWAY_PORT:=50151}"
: "${CERTS_DIR:=${OUT_DIR}/certs}"
export COMPOSE_PROJECT_NAME CROSSCODEX_CERTS_VOLUME CROSSCODEX_GATEWAY_PORT CERTS_DIR

task deploy:up

# Reach the daemon exactly as deploy:up's health check does: a host process
# dialing the published gateway port over mTLS. In this rootless, nested-podman
# environment a sibling `podman run` container cannot reach the daemon -- joining
# its netns (--pod / --network container:) does not confer reachability, and
# rootless port publishing binds the port only in the host netns, not inside a
# --network host container. deploy:up's /healthz loop proves the
# host->published-port path works, so the smoke CLI uses the same reach.
#
# Build the CLI on the host. The deploy tasks already require a Go toolchain
# (deploy:certs runs `go run` for the PKI generator), so this adds no new
# dependency. CGO_ENABLED=0 mirrors the release binary.
CLI_BIN="${OUT_DIR}/crosscodex"
CGO_ENABLED=0 go build -o "${CLI_BIN}" "${ROOT_DIR}/cmd/crosscodex"

# The server cert SAN covers localhost + 127.0.0.1; client.pem is the CLI's mTLS
# identity. Certs come from the host PKI dir that deploy:certs populated.
run_cli() {
  "${CLI_BIN}" \
    --endpoint "https://localhost:${CROSSCODEX_GATEWAY_PORT}" \
    --tls-ca "${CERTS_DIR}/ca.pem" \
    --tls-cert "${CERTS_DIR}/client.pem" \
    --tls-key "${CERTS_DIR}/client-key.pem" \
    "$@"
}

run_cli catalog import "${FIXTURE}" | tee "${OUT_DIR}/import.log"
run_cli catalog list | tee "${OUT_DIR}/list.log" | grep -q . && echo "SMOKE PASS"
