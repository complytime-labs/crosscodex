#!/usr/bin/env bash
# check-openai-compat-models.sh — Fail fast unless an OpenAI-compatible server
# (llama.cpp, vLLM, LocalAI, ...) lists the chat and embedding models the smoke
# stack's gateway aliases will request.
#
# Usage: check-openai-compat-models.sh <base-url> <chat-model> <embed-model>
#   base-url:    Server base URL without /v1 (e.g. http://localhost:18080)
#   chat-model:  Model ID for the gateway's `default` alias
#   embed-model: Model ID for the gateway's `embed` alias
#
# Nothing is pulled: such servers load models from their own configuration
# (llama.cpp's router mode loads a listed model on first request).
set -euo pipefail

BASE_URL="${1:?Usage: check-openai-compat-models.sh <base-url> <chat-model> <embed-model>}"
CHAT_MODEL="${2:?Usage: check-openai-compat-models.sh <base-url> <chat-model> <embed-model>}"
EMBED_MODEL="${3:?Usage: check-openai-compat-models.sh <base-url> <chat-model> <embed-model>}"
BASE_URL="${BASE_URL%/}"

if ! command -v jq >/dev/null 2>&1; then
	echo "ERROR: jq is required to read the model list from ${BASE_URL}/v1/models" >&2
	exit 1
fi

if ! models_json="$(curl -sf --connect-timeout 5 --max-time 10 "${BASE_URL}/v1/models")"; then
	echo "ERROR: cannot reach an OpenAI-compatible server at ${BASE_URL} (GET /v1/models failed)" >&2
	exit 1
fi

if ! available="$(printf '%s' "${models_json}" | jq -r '.data[].id')"; then
	echo "ERROR: ${BASE_URL}/v1/models did not return an OpenAI model list ({\"data\": [{\"id\": ...}]})" >&2
	exit 1
fi

missing=0
for kind_model in "chat:${CHAT_MODEL}" "embedding:${EMBED_MODEL}"; do
	kind="${kind_model%%:*}"
	model="${kind_model#*:}"
	if ! grep -qxF -- "${model}" <<<"${available}"; then
		echo "ERROR: ${BASE_URL} does not serve the ${kind} model ${model}" >&2
		missing=1
	fi
done
if [[ "${missing}" -ne 0 ]]; then
	echo "Models it lists: $(tr '\n' ' ' <<<"${available}")" >&2
	exit 1
fi

echo "OpenAI-compatible server at ${BASE_URL} lists chat model ${CHAT_MODEL} and embedding model ${EMBED_MODEL}"
