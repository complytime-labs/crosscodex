#!/usr/bin/env bash
# Runs one deploy task in a scratch copy of the repo with the real
# deploy/local_up.sh and the real deploy:up and deploy:down tasks, against stub
# podman, podman-compose, go and curl. Nothing is built or started: the stub
# podman and go exit 0, the stub curl answers the gateway health probe, and
# the stub podman-compose prints "compose: <args>" for every call. Paths under
# the scratch copy are printed as ROOT.
# Usage: deploy-ingestion.sh <repo-root> <task args...>
# Prints the stubs' output, then "task_exit=<code>".
set -u
root="${1:?Usage: deploy-ingestion.sh <repo-root> <task args...>}"
shift
task_bin="$(command -v task)"
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
mkdir -p "$d/scripts" "$d/test/deploy" "$d/deploy" "$d/bin"
cp "${root}/Taskfile.yml" "$d/"
cp -r "${root}/.taskfiles" "$d/"
cp "${root}"/deploy/*.yaml "${root}/deploy/.env.example" "${root}/deploy/local_up.sh" "$d/deploy/"
cp -r "${root}/deploy/litellm" "$d/deploy/"
cp "${root}/deploy/.env.example" "$d/deploy/.env"
cp "${root}/test/deploy/podman_env.sh" "$d/test/deploy/"
cp "${root}/scripts/in-container.sh" "${root}/scripts/generate-llm-gateway-config.sh" "$d/scripts/"
printf '#!/bin/sh\nexit 0\n' >"$d/bin/podman"
printf '#!/bin/sh\nexit 0\n' >"$d/bin/go"
printf '#!/bin/sh\nexit 0\n' >"$d/bin/curl"
# shellcheck disable=SC2016 # the $ must reach the stub unexpanded
printf '#!/bin/sh\necho "compose: $*" | sed "s#${SCRATCH}#ROOT#g"\n' >"$d/bin/podman-compose"
chmod +x "$d/bin/podman" "$d/bin/go" "$d/bin/curl" "$d/bin/podman-compose"
(cd "$d" && SCRATCH="$d" PATH="$d/bin:$PATH" "$task_bin" "$@" 2>&1)
echo "task_exit=$?"
