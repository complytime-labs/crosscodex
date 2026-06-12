#!/usr/bin/env bash
# integration-health.sh — Host-side health probes for integration test containers.
#
# podman-compose has a multi-minute delay before running container healthchecks,
# so we probe the published ports from the host instead.
#
# Usage: integration-health.sh <profile> <container-engine> [ollama-host]
#   profile:          db|storage|nats|authn|grpc|telemetry|llm|all
#   container-engine: podman|docker
#   ollama-host:      Ollama base URL (default: http://localhost:11434)
set -euo pipefail

PROFILE="${1:?Usage: integration-health.sh <profile> <container-engine> [ollama-host]}"
ENGINE="${2:?Usage: integration-health.sh <profile> <container-engine> [ollama-host]}"
OLLAMA_HOST="${3:-http://localhost:11434}"

wait_for() {
	local label="$1"
	shift
	echo "Waiting for $label ..."
	for _ in $(seq 1 60); do
		"$@" >/dev/null 2>&1 && break
		sleep 2
	done
	if ! "$@" >/dev/null 2>&1; then
		echo "ERROR: $label did not become ready after 120s"
		return 1
	fi
	echo "$label is ready."
}

wait_for_ollama() {
	if [[ "${OLLAMA_HOST}" = "http://localhost:11434" ]]; then
		wait_for "ollama" bash -c "echo > /dev/tcp/localhost/11434"
	else
		echo "Probing remote Ollama at ${OLLAMA_HOST} (host) ..."
		if ! curl -sf --connect-timeout 5 --max-time 10 "${OLLAMA_HOST}/api/tags" >/dev/null 2>&1; then
			echo "ERROR: Cannot reach Ollama at ${OLLAMA_HOST} from the host (timed out after 10s)"
			exit 1
		fi
		echo "ollama reachable from host."
	fi
}

# Verify the LiteLLM container can reach the external Ollama host.
# Must be called AFTER litellm is healthy (need a running container to exec into).
verify_ollama_from_container() {
	if [[ "${OLLAMA_HOST}" = "http://localhost:11434" ]]; then
		return 0
	fi
	echo "Probing remote Ollama at ${OLLAMA_HOST} (from litellm container) ..."
	if ! "${ENGINE}" exec \
		-e OLLAMA_PROBE_URL="${OLLAMA_HOST}/api/tags" \
		crosscodex-test-litellm \
		python -c "import os, urllib.request; urllib.request.urlopen(os.environ['OLLAMA_PROBE_URL'], timeout=10)" \
		>/dev/null 2>&1; then
		echo ""
		echo "ERROR: LiteLLM container cannot reach Ollama at ${OLLAMA_HOST}"
		echo ""
		echo "  The host can reach it, but the container cannot. Common causes:"
		echo "    - Container network isolation (try --network=host on the compose service)"
		echo "    - DNS resolution differs inside the container"
		echo "    - Firewall rules blocking container egress"
		echo ""
		exit 1
	fi
	echo "ollama reachable from litellm container."
}

case "${PROFILE}" in
db)
	wait_for "postgres" "${ENGINE}" exec crosscodex-test-db pg_isready -U postgres
	;;
storage)
	wait_for "rustfs" curl -so /dev/null http://localhost:19000/
	;;
nats)
	wait_for "nats" "${ENGINE}" exec crosscodex-test-nats nats-server --help
	;;
authn)
	wait_for "authn-proxy" "${ENGINE}" exec crosscodex-test-authn nginx -t
	;;
grpc)
	wait_for "postgres" "${ENGINE}" exec crosscodex-test-db pg_isready -U postgres
	wait_for "nats" "${ENGINE}" exec crosscodex-test-nats nats-server --help
	wait_for "grpc" bash -c "echo > /dev/tcp/localhost/19090"
	;;
telemetry)
	wait_for "jaeger" wget -qO- http://localhost:16686/ 2>/dev/null
	;;
llm)
	wait_for_ollama
	wait_for "litellm" curl -sf http://localhost:14000/health
	verify_ollama_from_container
	wait_for "jaeger" wget -qO- http://localhost:16686/ 2>/dev/null
	;;
all)
	wait_for_ollama
	wait_for "postgres" "${ENGINE}" exec crosscodex-test-db pg_isready -U postgres
	wait_for "rustfs" curl -so /dev/null http://localhost:19000/
	wait_for "nats" "${ENGINE}" exec crosscodex-test-nats nats-server --help
	wait_for "authn-proxy" "${ENGINE}" exec crosscodex-test-authn nginx -t
	wait_for "grpc" bash -c "echo > /dev/tcp/localhost/19090"
	wait_for "jaeger" wget -qO- http://localhost:16686/ 2>/dev/null
	wait_for "litellm" curl -sf http://localhost:14000/health
	verify_ollama_from_container
	;;
*)
	echo "ERROR: Unknown profile '${PROFILE}'. Expected: db|storage|nats|authn|grpc|telemetry|llm|all"
	exit 1
	;;
esac
