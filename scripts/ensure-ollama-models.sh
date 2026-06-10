#!/usr/bin/env bash
# ensure-ollama-models.sh — Pull required Ollama models if not already present.
#
# Usage: ensure-ollama-models.sh <profile> <ollama-host> <chat-model> <embed-model>
#   profile:     llm|all (no-op for other profiles)
#   ollama-host: Ollama base URL (e.g. http://localhost:11434)
#   chat-model:  Chat model name (e.g. llama3.2:1b)
#   embed-model: Embedding model name (e.g. nomic-embed-text)
set -euo pipefail

PROFILE="${1:?Usage: ensure-ollama-models.sh <profile> <ollama-host> <chat-model> <embed-model>}"
OLLAMA_HOST="${2:?Usage: ensure-ollama-models.sh <profile> <ollama-host> <chat-model> <embed-model>}"
CHAT_MODEL="${3:?Usage: ensure-ollama-models.sh <profile> <ollama-host> <chat-model> <embed-model>}"
EMBED_MODEL="${4:?Usage: ensure-ollama-models.sh <profile> <ollama-host> <chat-model> <embed-model>}"

# No-op for profiles that don't include Ollama.
case "${PROFILE}" in
  llm|all) ;;
  *) exit 0 ;;
esac

# Fail fast if Ollama is unreachable.
if ! curl -sf --connect-timeout 5 --max-time 10 "${OLLAMA_HOST}/api/tags" >/dev/null 2>&1; then
  echo ""
  echo "ERROR: Cannot reach Ollama at ${OLLAMA_HOST}"
  echo ""
  echo "  Is Ollama running? If using the managed stack, try:"
  echo "    task test:integration:compose-up PROFILE=llm"
  echo ""
  echo "  If using an external Ollama, check OLLAMA_HOST:"
  echo "    task test:integration:llm OLLAMA_HOST=http://your-host:11434"
  echo ""
  exit 1
fi

pull_model() {
  local model="$1"
  echo "Pulling Ollama model: ${model}"
  echo "  (large models can take several minutes — progress dots appear below)"
  local last_status tmpfile
  tmpfile=$(mktemp)
  # Print one dot to stderr per JSON status line (visible progress).
  # Capture the final line to a tmpfile for error detection.
  # Temporarily disable set -e so we can capture the exit code manually.
  set +e
  curl -sf -X POST "${OLLAMA_HOST}/api/pull" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"${model}\",\"stream\":true}" \
    | while IFS= read -r line; do
        printf '.' >&2
        echo "${line}"
      done \
    | tail -n1 > "${tmpfile}"
  local rc=$?
  set -e
  echo >&2  # newline after progress dots
  last_status=$(cat "${tmpfile}")
  rm -f "${tmpfile}"

  if [[ "$rc" -ne 0 ]] || [[ -z "${last_status}" ]]; then
    echo ""
    echo "ERROR: Pull of '${model}' failed (curl exit ${rc})."
    echo ""
    echo "  Possible causes:"
    echo "    - Ollama container has no internet access (check proxy/firewall)"
    echo "    - Model name is incorrect (check 'ollama.com/library')"
    echo "    - Disk space exhausted on the Ollama host"
    echo ""
    echo "  To pull manually inside the container:"
    echo "    podman exec crosscodex-test-ollama ollama pull ${model}"
    echo ""
    return 1
  fi
  if echo "${last_status}" | grep -q '"error"'; then
    local errmsg
    errmsg=$(echo "${last_status}" | grep -o '"error":"[^"]*"' | sed 's/"error":"//;s/"//')
    echo ""
    echo "ERROR: Ollama rejected pull of '${model}': ${errmsg}"
    echo ""
    echo "  To pull manually inside the container:"
    echo "    podman exec crosscodex-test-ollama ollama pull ${model}"
    echo ""
    return 1
  fi
  echo "  Model ready: ${model}"
}

# GET /api/tags returns {"models":[{"name":"llama3.2:1b",...},...]}
# grep -o returns exit 1 when no matches — tolerate that under set -e.
existing=$(curl -sf --max-time 10 "${OLLAMA_HOST}/api/tags" \
  | grep -o '"name":"[^"]*"' \
  | sed 's/"name":"//;s/"//') || true

for model in "${CHAT_MODEL}" "${EMBED_MODEL}"; do
  if echo "${existing}" | grep -qF "${model}"; then
    echo "Ollama model already present: ${model}"
  else
    pull_model "${model}"
  fi
done
