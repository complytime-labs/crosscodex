#!/usr/bin/env bash
# Runs test/deploy/smoke_up.sh against a stub `task` and container engine and
# prints the ALLOW_SHARED_PANEL_MODELS variables the child `task deploy:up`
# would receive. Nothing is started; the stub task exits 3 after printing.
# Usage: smoke-up-allow-shared.sh <repo-root>   (provider settings come from the env)
set -u
root="${1:?Usage: smoke-up-allow-shared.sh <repo-root>}"
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
mkdir -p "$d/scripts" "$d/deploy/litellm" "$d/bin"
cp "${root}/scripts/in-container.sh" "${root}/scripts/generate-llm-gateway-config.sh" \
	"${root}/scripts/check-openai-compat-models.sh" "$d/scripts/"
cp "${root}"/deploy/litellm/*.tmpl "$d/deploy/litellm/"
printf '#!/bin/sh\nenv | grep ALLOW_SHARED_PANEL_MODELS | sort\nexit 3\n' >"$d/bin/task"
printf '#!/bin/sh\nexit 0\n' >"$d/bin/engine"
chmod +x "$d/bin/task" "$d/bin/engine"
PATH="$d/bin:$PATH" CONTAINER_ENGINE="$d/bin/engine" ROOT_DIR="$d" \
	bash "${root}/test/deploy/smoke_up.sh" 2>&1
echo "smoke_up exit=$?"
