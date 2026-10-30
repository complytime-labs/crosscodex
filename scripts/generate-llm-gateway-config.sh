#!/usr/bin/env bash
# generate-llm-gateway-config.sh — Render an LLM gateway (LiteLLM or Praxis) config from template.
#
# Usage: generate-llm-gateway-config.sh <profile> <template> <dest> <ollama-host> <chat-model> <embed-model>
#   profile:     llm|llm-praxis|all (no-op for other profiles)
#   template:    Path to the gateway config template (LiteLLM or Praxis)
#   dest:        Output path for rendered config
#   ollama-host: Ollama base URL (e.g. http://localhost:11434); only LiteLLM
#                templates use it (as api_base)
#   chat-model:  Chat model name for sed substitution
#   embed-model: Embedding model name for sed substitution
set -euo pipefail

PROFILE="${1:?Usage: generate-llm-gateway-config.sh <profile> <template> <dest> <ollama-host> <chat-model> <embed-model>}"
TEMPLATE="${2:?Usage: generate-llm-gateway-config.sh <profile> <template> <dest> <ollama-host> <chat-model> <embed-model>}"
DEST="${3:?Usage: generate-llm-gateway-config.sh <profile> <template> <dest> <ollama-host> <chat-model> <embed-model>}"
OLLAMA_HOST="${4:?Usage: generate-llm-gateway-config.sh <profile> <template> <dest> <ollama-host> <chat-model> <embed-model>}"
CHAT_MODEL="${5:?Usage: generate-llm-gateway-config.sh <profile> <template> <dest> <ollama-host> <chat-model> <embed-model>}"
EMBED_MODEL="${6:?Usage: generate-llm-gateway-config.sh <profile> <template> <dest> <ollama-host> <chat-model> <embed-model>}"

# No-op for profiles that don't include an LLM gateway.
case "${PROFILE}" in
llm | llm-praxis | all) ;;
*) exit 0 ;;
esac

if [[ ! -f "${TEMPLATE}" ]]; then
	echo "ERROR: LLM gateway config template not found: ${TEMPLATE}"
	exit 1
fi

mkdir -p "$(dirname "${DEST}")"

# OLLAMA_API_BASE is a LiteLLM template placeholder; Praxis templates use
# the fixed compose endpoint ollama:11434 and leave it unused.
# Determine the api_base LiteLLM should use to reach Ollama.
# The openai/ provider prefix requires a /v1 suffix on the base URL
# (LiteLLM's openai client appends /chat/completions, /embeddings).
# When running the local compose stack, LiteLLM reaches Ollama via
# the compose network service name. When OLLAMA_HOST is overridden
# to a remote, we use that URL directly.
if [[ "${OLLAMA_HOST}" = "http://localhost:11434" ]]; then
	# Default: use the compose-internal service name so LiteLLM
	# (a container) can reach the Ollama container on the same network.
	api_base="http://ollama:11434/v1"
else
	# Non-default: LiteLLM runs with host networking (compose.host-network.yaml),
	# so it can reach both localhost and LAN/remote hosts directly.
	api_base="${OLLAMA_HOST%/}/v1"
fi

sed \
	-e "s|OLLAMA_API_BASE|${api_base}|g" \
	-e "s|OLLAMA_CHAT_MODEL|${CHAT_MODEL}|g" \
	-e "s|OLLAMA_EMBED_MODEL|${EMBED_MODEL}|g" \
	"${TEMPLATE}" >"${DEST}"

if grep -q OLLAMA_API_BASE "${TEMPLATE}"; then
	echo "LLM gateway config written to ${DEST}: api_base=${api_base}, chat=${CHAT_MODEL}, embed=${EMBED_MODEL}"
else
	echo "LLM gateway config written to ${DEST}: chat=${CHAT_MODEL}, embed=${EMBED_MODEL}"
fi
