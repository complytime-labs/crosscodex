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
  local label="$1"; shift
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
    echo "Probing remote Ollama at ${OLLAMA_HOST} ..."
    if ! curl -sf --connect-timeout 5 --max-time 10 "${OLLAMA_HOST}/api/tags" >/dev/null 2>&1; then
      echo "ERROR: Cannot reach Ollama at ${OLLAMA_HOST} (timed out after 10s)"
      exit 1
    fi
    echo "ollama is ready."
  fi
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
    ;;
  *)
    echo "ERROR: Unknown profile '${PROFILE}'. Expected: db|storage|nats|authn|grpc|telemetry|llm|all"
    exit 1
    ;;
esac
