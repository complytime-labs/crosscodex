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
#
# Panel aliases: the relationship, requires and critique panels use the gateway
# aliases panel-1..3, which must resolve to three different model families. The
# models come from a comma-separated list of exactly three names:
#   OPENAI_COMPAT_PANEL_MODELS  for the openai-compat template (default
#                               qwen3.8-flash-next,gpt-oss-120b,qwen3.8-27b)
#   OLLAMA_PANEL_MODELS         for every other template (default: chat-model
#                               three times, which needs the daemon's
#                               allow_shared_panel_models; see
#                               deploy/compose.offline.yaml)
# An unset or empty variable selects the default.
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

# Choose the panel list for this template and require exactly three model
# tokens (letters, digits and . - _ : /). Names reach sed and YAML unquoted, so
# anything else (spaces, quotes, ; & |) is rejected rather than escaped.
case "${TEMPLATE}" in
*openai-compat*)
	panel_var="OPENAI_COMPAT_PANEL_MODELS"
	panel_default="qwen3.8-flash-next,gpt-oss-120b,qwen3.8-27b"
	panel_value="${OPENAI_COMPAT_PANEL_MODELS:-${panel_default}}"
	;;
*)
	panel_var="OLLAMA_PANEL_MODELS"
	panel_default="${CHAT_MODEL},${CHAT_MODEL},${CHAT_MODEL}"
	panel_value="${OLLAMA_PANEL_MODELS:-${panel_default}}"
	;;
esac
panel_token='^[A-Za-z0-9._:/-]+$'
panel_commas="${panel_value//[^,]/}"
IFS=',' read -r panel_1 panel_2 panel_3 <<<"${panel_value}"
if [[ "${#panel_commas}" -ne 2 || ! "${panel_1}" =~ ${panel_token} || ! "${panel_2}" =~ ${panel_token} || ! "${panel_3}" =~ ${panel_token} ]]; then
	echo "ERROR: ${panel_var}=${panel_value} must list exactly three model names, comma-separated (for panel-1, panel-2, panel-3)." >&2
	echo "Each name may contain only letters, digits and . - _ : / (no spaces or empty elements), e.g. ${panel_default}" >&2
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
	-e "s|OLLAMA_PANEL_1_MODEL|${panel_1}|g" \
	-e "s|OLLAMA_PANEL_2_MODEL|${panel_2}|g" \
	-e "s|OLLAMA_PANEL_3_MODEL|${panel_3}|g" \
	"${TEMPLATE}" >"${DEST}"

if grep -q OLLAMA_API_BASE "${TEMPLATE}"; then
	echo "LLM gateway config written to ${DEST}: api_base=${api_base}, chat=${CHAT_MODEL}, embed=${EMBED_MODEL}, panel=${panel_value}"
else
	echo "LLM gateway config written to ${DEST}: chat=${CHAT_MODEL}, embed=${EMBED_MODEL}, panel=${panel_value}"
fi
