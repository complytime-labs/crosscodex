#!/usr/bin/env bash
# Bring up an offline copy of the production compose stack under a caller-chosen
# identity: select the local model server, render the LLM gateway config for it,
# and run `task deploy:up` with the matching offline overlay. Used by two callers:
#   * the deploy:local task runs it for a persistent stack to try CrossCodex on;
#   * test/deploy/smoke_up.sh sources it for the ephemeral smoke stack, then
#     runs its assertions against what it brought up.
#
# The caller sets the isolation identity; this script defaults none of it, so
# one caller's stack cannot land on another's project, volumes or PKI:
#   * COMPOSE_PROJECT_NAME (required) — own pod/containers/network and own
#                               (auto-namespaced) data volumes.
#   * CERTS_DIR (required)    — host dir for this stack's PKI.
#   * OUT_DIR (required)      — host dir the rendered gateway config is written
#                               to; the offline overlays mount it from there.
#   * CROSSCODEX_CERTS_VOLUME, CROSSCODEX_CLIENT_CERTS_VOLUME,
#     CROSSCODEX_ATTESTATION_KEYS_VOLUME, CROSSCODEX_GATEWAY_PORT — read by the
#                               child `task deploy:up` and compose.yaml; unset,
#                               they are the production stack's.
# Provider knobs: LLM_GATEWAY, PRAXIS_MODELS_ENDPOINT, OLLAMA_HOST,
# OLLAMA_{CHAT,EMBED,PANEL}_MODEL(S), OPENAI_COMPAT_HOST,
# OPENAI_COMPAT_{CHAT,EMBED}_MODEL, OPENAI_COMPAT_PANEL_MODELS, CONTAINER_ENGINE.
# LLM_GATEWAY=praxis also needs SMOKE_PRAXIS_PORT (compose.offline-praxis.yaml).
# INGESTION_OVERLAYS (optional): space-separated compose files appended after
# the offline overlay; the deploy:local task sets it from INGESTION.
set -euo pipefail

ROOT_DIR="${ROOT_DIR:-$(git rev-parse --show-toplevel)}"
: "${COMPOSE_PROJECT_NAME:?local_up.sh needs COMPOSE_PROJECT_NAME: the compose project that isolates this stack}"
: "${CERTS_DIR:?local_up.sh needs CERTS_DIR: the host dir for the PKI of this stack}"
: "${OUT_DIR:?local_up.sh needs OUT_DIR: the host dir for the rendered gateway config}"
export COMPOSE_PROJECT_NAME CERTS_DIR
mkdir -p "${OUT_DIR}"

# Shared with the deploy:*:down tasks so every step acts on the same podman
# runtime view.
# shellcheck source=test/deploy/podman_env.sh
source "${ROOT_DIR}/test/deploy/podman_env.sh"

# LLM gateway (litellm default, or praxis). Exported so the child
# `task deploy:up` loads the matching compose.gateway-<gw>.yaml fragment: the
# process env beats deploy/.env there, so an operator's persisted gateway
# choice cannot change this stack. PRAXIS_MODELS_ENDPOINT is pinned the
# same way.
: "${LLM_GATEWAY:=litellm}"
: "${PRAXIS_MODELS_ENDPOINT:=false}"
case "${LLM_GATEWAY}" in
litellm | praxis) ;;
*)
	echo "ERROR: LLM_GATEWAY=${LLM_GATEWAY} is not supported. Use litellm or praxis." >&2
	exit 1
	;;
esac
export LLM_GATEWAY PRAXIS_MODELS_ENDPOINT

# Offline LLM provider: back the stack with a local model server instead
# of a real OpenAI key. Two kinds are supported:
#   - Ollama (default): OLLAMA_HOST, OLLAMA_CHAT_MODEL, OLLAMA_EMBED_MODEL. With
#     no OLLAMA_HOST, a baked-model Ollama container runs inside the stack.
#   - Any other OpenAI-compatible server (llama.cpp, vLLM, ...): OPENAI_COMPAT_HOST,
#     OPENAI_COMPAT_CHAT_MODEL, OPENAI_COMPAT_EMBED_MODEL.
# The panel-1..3 aliases take OPENAI_COMPAT_PANEL_MODELS or OLLAMA_PANEL_MODELS
# (three comma-separated names). generate-llm-gateway-config.sh reads them from
# the environment, validates them and applies the defaults.
# The deploy:local and deploy:smoke:up tasks export these; the defaults below
# keep a direct run offline too. Reuses the integration stack's rendered-config,
# baked-image, and model-pull helpers -- no Ollama plumbing is duplicated here.
if [ -n "${OPENAI_COMPAT_HOST:-}" ]; then
	# The root Taskfile always exports OLLAMA_HOST, defaulting to the baked
	# container's http://localhost:11434, so only another value is a conflict.
	if [ -n "${OLLAMA_HOST:-}" ] && [ "${OLLAMA_HOST}" != "http://localhost:11434" ]; then
		echo "ERROR: both OPENAI_COMPAT_HOST=${OPENAI_COMPAT_HOST} and OLLAMA_HOST=${OLLAMA_HOST} are set; the stack uses one model server." >&2
		echo "Unset OLLAMA_HOST to use the OpenAI-compatible server, or unset OPENAI_COMPAT_HOST to use Ollama." >&2
		exit 1
	fi
	if [ "${LLM_GATEWAY}" = "praxis" ]; then
		echo "ERROR: LLM_GATEWAY=praxis supports only the local baked Ollama (OPENAI_COMPAT_HOST=${OPENAI_COMPAT_HOST} was given)." >&2
		echo "Use LLM_GATEWAY=litellm with OPENAI_COMPAT_HOST, or unset OPENAI_COMPAT_HOST for Praxis." >&2
		exit 1
	fi
	provider="openai-compat"
	provider_host="${OPENAI_COMPAT_HOST}"
	provider_chat_model="${OPENAI_COMPAT_CHAT_MODEL:-qwen3.8-flash-next}"
	provider_embed_model="${OPENAI_COMPAT_EMBED_MODEL:-qwen3-embedding-8b}"
	litellm_template="${ROOT_DIR}/deploy/litellm/config.openai-compat.yaml.tmpl"
else
	provider="ollama"
	: "${OLLAMA_HOST:=http://localhost:11434}"
	provider_host="${OLLAMA_HOST}"
	provider_chat_model="${OLLAMA_CHAT_MODEL:-llama3.2:1b}"
	provider_embed_model="${OLLAMA_EMBED_MODEL:-granite-embedding:30m}"
	litellm_template="${ROOT_DIR}/deploy/litellm/config.offline.yaml.tmpl"
fi

# Normalize the scheme early: the rendered LiteLLM api_base must be a valid URL.
# A scheme-less host (e.g. an override of "localhost:11435") renders as
# "localhost:11435/v1", which httpx parses as scheme=localhost -> InvalidURL, so
# every LLM call fails cryptically. Prepend http:// when no scheme is present.
case "${provider_host}" in
http://* | https://*) ;;
*) provider_host="http://${provider_host}" ;;
esac

if [ "${provider}" = "ollama" ]; then
	OLLAMA_HOST="${provider_host}"
	OLLAMA_CHAT_MODEL="${provider_chat_model}"
	OLLAMA_EMBED_MODEL="${provider_embed_model}"
	export OLLAMA_HOST OLLAMA_CHAT_MODEL OLLAMA_EMBED_MODEL
	# Without OLLAMA_PANEL_MODELS the three panel aliases are the one chat model,
	# so declare that to the daemon (read by compose.offline-remote.yaml; the
	# local overlays set it themselves). An explicit list stays checked, and the
	# openai-compat provider (three distinct models by default) never gets this.
	if [ -z "${OLLAMA_PANEL_MODELS:-}" ]; then
		export CROSSCODEX_ANALYSIS_RELATIONSHIP_ALLOW_SHARED_PANEL_MODELS=true
		export CROSSCODEX_ANALYSIS_REQUIRES_ALLOW_SHARED_PANEL_MODELS=true
		export CROSSCODEX_ANALYSIS_CRITIQUE_ALLOW_SHARED_PANEL_MODELS=true
	fi
fi

if [ "${provider}" = "ollama" ] && [ "${provider_host}" = "http://localhost:11434" ]; then
	# Local (default): the gateway's offline overlay runs a baked-model Ollama
	# container that the gateway depends on. Build that image first. This
	# mirrors the internal `test:integration:build-ollama-image` task (same
	# image, Containerfile, and image-inspect idempotency guard); that task is
	# internal and so cannot be invoked from the CLI here. The guard skips the
	# multi-minute build when the image already exists.
	if [ "${LLM_GATEWAY}" = "praxis" ]; then
		export COMPOSE_OVERLAY="${ROOT_DIR}/deploy/compose.offline-praxis.yaml"
	else
		export COMPOSE_OVERLAY="${ROOT_DIR}/deploy/compose.offline.yaml"
	fi
	: "${CONTAINER_ENGINE:=podman}"
	if ! "${CONTAINER_ENGINE}" image inspect crosscodex-test-ollama:latest >/dev/null 2>&1; then
		echo "Building baked-model Ollama image (crosscodex-test-ollama:latest) ..."
		"${CONTAINER_ENGINE}" build -t crosscodex-test-ollama:latest \
			-f "${ROOT_DIR}/test/ollama/Containerfile" "${ROOT_DIR}/test/ollama"
	fi
	# The compose service name ollama:11434 serves this path: the generator maps
	# this sentinel to it for LiteLLM, and the Praxis template hardcodes it.
	render_host="${provider_host}"
else
	# External server (an OpenAI-compatible host, or any non-default OLLAMA_HOST):
	# use the overlay that only repoints LiteLLM (no local ollama service, no
	# depends_on) so LiteLLM starts immediately. Probe (and for Ollama, pull) from
	# the HOST against the address as given -- these run in the host netns, where a
	# loopback override is reachable -- so a down provider or missing model fails
	# fast here, not at the 120s catalog poll.
	if [ "${LLM_GATEWAY}" = "praxis" ]; then
		echo "ERROR: LLM_GATEWAY=praxis supports only the local baked Ollama (OLLAMA_HOST=${provider_host} was given)." >&2
		echo "The Praxis spike has no remote-Ollama overlay. Unset OLLAMA_HOST, or use LLM_GATEWAY=litellm for a remote Ollama." >&2
		exit 1
	fi
	export COMPOSE_OVERLAY="${ROOT_DIR}/deploy/compose.offline-remote.yaml"
	if [ "${provider}" = "openai-compat" ]; then
		echo "Probing OpenAI-compatible server at ${provider_host} ..."
		bash "${ROOT_DIR}/scripts/check-openai-compat-models.sh" \
			"${provider_host}" "${provider_chat_model}" "${provider_embed_model}"
	else
		echo "Probing Ollama at ${provider_host} ..."
		if ! curl -sf --connect-timeout 5 --max-time 10 "${provider_host}/api/tags" >/dev/null 2>&1; then
			echo "ERROR: cannot reach Ollama at ${provider_host} from the host" >&2
			exit 1
		fi
		bash "${ROOT_DIR}/scripts/ensure-ollama-models.sh" all \
			"${provider_host}" "${provider_chat_model}" "${provider_embed_model}"
	fi
	# LiteLLM runs in the compose bridge netns, where "localhost" is the container
	# itself -- not the host. Translate a loopback override to host.containers.internal
	# so LiteLLM reaches a server on the host; compose.offline-remote.yaml maps that
	# name via `extra_hosts: host-gateway`. Non-loopback hosts pass through unchanged.
	render_host="$(printf '%s' "${provider_host}" | sed -E 's#://(localhost|127\.0\.0\.1)([:/]|$)#://host.containers.internal\2#')"
	if [ "${render_host}" != "${provider_host}" ]; then
		echo "Loopback override: LiteLLM will reach it as ${render_host} (host-gateway)."
	fi
fi

# Render the offline gateway config the overlay mounts, using render_host -- the
# address the gateway (a container) uses to reach the model server: the compose
# service name on the baked path, or the host-gateway-translated override for an
# external host (LiteLLM only). Written to OUT_DIR, which the overlay bind-mounts
# as CROSSCODEX_GATEWAY_CONFIG_DIR.
export CROSSCODEX_GATEWAY_CONFIG_DIR="${OUT_DIR}"
case "${LLM_GATEWAY}" in
praxis)
	gateway_template="${ROOT_DIR}/deploy/praxis/config.offline.yaml.tmpl"
	gateway_config="${OUT_DIR}/praxis_config.yaml"
	;;
litellm)
	gateway_template="${litellm_template}"
	gateway_config="${OUT_DIR}/litellm_config.yaml"
	;;
esac
bash "${ROOT_DIR}/scripts/generate-llm-gateway-config.sh" all \
	"${gateway_template}" "${gateway_config}" \
	"${render_host}" "${provider_chat_model}" "${provider_embed_model}"

if [ -n "${INGESTION_OVERLAYS:-}" ]; then
	COMPOSE_OVERLAY="${COMPOSE_OVERLAY} ${INGESTION_OVERLAYS}"
fi

task deploy:up
