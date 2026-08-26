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

# Offline LLM provider: back the smoke stack with a local Ollama instead of a
# real OpenAI key, mirroring the integration stack. The deploy:smoke:up task
# exports OLLAMA_*; the defaults below keep a direct `bash smoke_up.sh` offline
# too. Reuses the integration stack's rendered-config, baked-image, and
# model-pull helpers verbatim -- no Ollama plumbing is duplicated here.
: "${OLLAMA_HOST:=http://localhost:11434}"
: "${OLLAMA_CHAT_MODEL:=llama3.2:1b}"
: "${OLLAMA_EMBED_MODEL:=granite-embedding:30m}"

# Normalize the scheme early: the rendered LiteLLM api_base must be a valid URL.
# A scheme-less host (e.g. an override of "localhost:11435") renders as
# "localhost:11435/v1", which httpx parses as scheme=localhost -> InvalidURL, so
# every LLM call fails cryptically. Prepend http:// when no scheme is present.
case "${OLLAMA_HOST}" in
http://* | https://*) ;;
*) OLLAMA_HOST="http://${OLLAMA_HOST}" ;;
esac
export OLLAMA_HOST OLLAMA_CHAT_MODEL OLLAMA_EMBED_MODEL

if [ "${OLLAMA_HOST}" = "http://localhost:11434" ]; then
	# Local (default): the overlay runs a baked-model Ollama container that LiteLLM
	# depends on. Build that image first. This mirrors the internal
	# `test:integration:build-ollama-image` task (same image, Containerfile, and
	# image-inspect idempotency guard); that task is internal and so cannot be
	# invoked from the CLI here. The guard skips the multi-minute build when the
	# image already exists.
	export COMPOSE_OVERLAY="${ROOT_DIR}/deploy/compose.offline.yaml"
	: "${CONTAINER_ENGINE:=podman}"
	if ! "${CONTAINER_ENGINE}" image inspect crosscodex-test-ollama:latest >/dev/null 2>&1; then
		echo "Building baked-model Ollama image (crosscodex-test-ollama:latest) ..."
		"${CONTAINER_ENGINE}" build -t crosscodex-test-ollama:latest \
			-f "${ROOT_DIR}/test/ollama/Containerfile" "${ROOT_DIR}/test/ollama"
	fi
	# The generator maps this sentinel to the compose service name (ollama:11434).
	render_host="${OLLAMA_HOST}"
else
	# External Ollama (any non-default OLLAMA_HOST): use the overlay that only
	# repoints LiteLLM (no local ollama service, no depends_on) so LiteLLM starts
	# immediately. Probe and pull from the HOST against the address as given --
	# these run in the host netns, where a loopback override is reachable -- so a
	# down provider or missing model fails fast here, not at the 120s catalog poll.
	export COMPOSE_OVERLAY="${ROOT_DIR}/deploy/compose.offline-remote.yaml"
	echo "Probing Ollama at ${OLLAMA_HOST} ..."
	if ! curl -sf --connect-timeout 5 --max-time 10 "${OLLAMA_HOST}/api/tags" >/dev/null 2>&1; then
		echo "ERROR: cannot reach Ollama at ${OLLAMA_HOST} from the host" >&2
		exit 1
	fi
	bash "${ROOT_DIR}/scripts/ensure-ollama-models.sh" all \
		"${OLLAMA_HOST}" "${OLLAMA_CHAT_MODEL}" "${OLLAMA_EMBED_MODEL}"
	# LiteLLM runs in the compose bridge netns, where "localhost" is the container
	# itself -- not the host. Translate a loopback override to host.containers.internal
	# so LiteLLM reaches an Ollama on the host; compose.offline-remote.yaml maps that
	# name via `extra_hosts: host-gateway`. Non-loopback hosts pass through unchanged.
	render_host="$(printf '%s' "${OLLAMA_HOST}" | sed -E 's#://(localhost|127\.0\.0\.1)([:/]|$)#://host.containers.internal\2#')"
	if [ "${render_host}" != "${OLLAMA_HOST}" ]; then
		echo "Loopback override: LiteLLM will reach it as ${render_host} (host-gateway)."
	fi
fi

# Render the offline LiteLLM config the overlay mounts, using render_host -- the
# address LiteLLM (a container) uses to reach Ollama: the compose service name on
# the baked path, or the host-gateway-translated override for an external host.
# Written to OUT_DIR, which the overlay bind-mounts.
bash "${ROOT_DIR}/scripts/generate-litellm-config.sh" all \
	"${ROOT_DIR}/deploy/litellm/config.offline.yaml.tmpl" \
	"${OUT_DIR}/litellm_config.yaml" \
	"${render_host}" "${OLLAMA_CHAT_MODEL}" "${OLLAMA_EMBED_MODEL}"

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

# Assert the import took effect (not just that the CLI exited 0): the gateway
# must acknowledge the submission. --plain disables color so the "Job ID:" line
# parses cleanly below.
run_cli --plain catalog import "${FIXTURE}" | tee "${OUT_DIR}/import.log"
grep -q "Document submitted successfully" "${OUT_DIR}/import.log"

# Recover the async analysis job id the import enqueued. Gating on this job's
# terminal status is what makes the smoke test exercise the LLM path: the catalog
# row + controls persist during parse regardless of embeddings, so a `list` check
# alone passes against a broken LiteLLM/Ollama. The FULL_ANALYSIS job, by
# contrast, FAILs on insufficient embedding coverage (<80%), so only a working
# embed path drives it to COMPLETED.
JOB_ID="$(sed -n 's/^Job ID: //p' "${OUT_DIR}/import.log" | head -n1)"
if [ -z "${JOB_ID}" ]; then
	echo "Could not parse a Job ID from the import output:" >&2
	cat "${OUT_DIR}/import.log" >&2
	exit 1
fi

# Poll the job to a terminal state: pass on COMPLETED, fail FAST on FAILED
# (printing the daemon's error so a broken LLM path is diagnosed here, not left
# to a misleading title-timeout). 60 x 2s = 120s, ample for a 3-control catalog's
# embedding + candidate pass on a freshly started stack.
echo "Waiting for analysis job ${JOB_ID} to reach a terminal state ..."
for i in $(seq 1 60); do
	# Tolerate a transient CLI error mid-poll: an empty/garbled read yields no
	# Status line, so it is treated as not-ready and retried; the timeout below
	# governs give-up, and the last captured output is dumped for triage.
	run_cli --plain run status "${JOB_ID}" >"${OUT_DIR}/job.log" 2>&1 || true
	status="$(sed -n 's/^Status: //p' "${OUT_DIR}/job.log" | head -n1)"
	case "${status}" in
	COMPLETED)
		# Round-trip completeness: the analysis succeeded, so `catalog list` must
		# return the persisted catalog. The NAME column is intentionally blank --
		# the CLI import sends no catalog name and the server never derives one
		# from OSCAL metadata (see ISSUE.md) -- so match a data row beyond the
		# header rather than a title. This is a fresh, ephemeral DB, so exactly one
		# catalog row exists: header + one row = 2 non-empty lines.
		if ! run_cli --plain catalog list >"${OUT_DIR}/list.log" 2>&1; then
			echo "Job COMPLETED but 'catalog list' failed:" >&2
			cat "${OUT_DIR}/list.log" >&2
			exit 1
		fi
		if [ "$(grep -c . "${OUT_DIR}/list.log")" -lt 2 ]; then
			echo "Job COMPLETED but 'catalog list' shows no catalog row:" >&2
			cat "${OUT_DIR}/list.log" >&2
			exit 1
		fi
		cat "${OUT_DIR}/list.log"
		echo "SMOKE PASS"
		exit 0
		;;
	FAILED | CANCELLED)
		echo "Analysis job ${JOB_ID} ended ${status}; daemon report:" >&2
		cat "${OUT_DIR}/job.log" >&2
		exit 1
		;;
	esac
	# Progress so a slow job does not look hung (attempt N/60, ~2s apart).
	echo "  job ${status:-not-ready} (attempt ${i}/60); retrying in 2s ..."
	sleep 2
done
echo "Analysis job ${JOB_ID} did not reach a terminal state within 120s; last status:" >&2
cat "${OUT_DIR}/job.log" >&2
exit 1
