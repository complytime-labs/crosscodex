#!/usr/bin/env bash
# Runs one deploy task in a scratch copy of the repo whose bring-up scripts and
# nested `task` are stubs, and prints the identity each stub received. Nothing
# is built or started: deploy/local_up.sh and test/deploy/smoke_up.sh print
# "ran=<script>" and the identity, then exit 3; a nested `task` (deploy:local:down
# runs `task deploy:down`) prints "nested_task=<args>" and the identity, then
# exits with $STUB_TASK_EXIT (default 0). Before the run, .local/crosscodex holds
# a marker; after it, "pki=present|removed" says whether the task deleted it.
# Paths under the scratch copy are printed as ROOT.
# Usage: deploy-local.sh <repo-root> <with deploy/.env: yes|no> <task args...>
# Prints the stubs' output, then "task_exit=<code>".
set -u
root="${1:?Usage: deploy-local.sh <repo-root> <yes|no> <task args...>}"
with_env="${2:?Usage: deploy-local.sh <repo-root> <yes|no> <task args...>}"
shift 2
task_bin="$(command -v task)"
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
mkdir -p "$d/scripts" "$d/test/deploy" "$d/deploy" "$d/bin"
cp "${root}/Taskfile.yml" "$d/"
cp -r "${root}/.taskfiles" "$d/"
cp "${root}"/deploy/*.yaml "${root}/deploy/.env.example" "$d/deploy/"
cp "${root}/test/deploy/podman_env.sh" "$d/test/deploy/"
cp "${root}/scripts/in-container.sh" "$d/scripts/"
if [ "$with_env" = yes ]; then
	cp "${root}/deploy/.env.example" "$d/deploy/.env"
fi
cat >"$d/bin/identity" <<'STUB'
#!/usr/bin/env bash
for k in COMPOSE_PROJECT_NAME CROSSCODEX_CERTS_VOLUME CROSSCODEX_CLIENT_CERTS_VOLUME \
	CROSSCODEX_ATTESTATION_KEYS_VOLUME CROSSCODEX_GATEWAY_PORT CERTS_DIR OUT_DIR \
	LLM_GATEWAY COMPOSE_OVERLAY CROSSCODEX_GATEWAY_CONFIG_DIR; do
	echo "$k=${!k-<unset>}" | sed "s#${SCRATCH}#ROOT#g"
done
STUB
printf '#!/bin/sh\necho "ran=local_up.sh"\nidentity\nexit 3\n' >"$d/deploy/local_up.sh"
printf '#!/bin/sh\necho "ran=smoke_up.sh"\nidentity\nexit 3\n' >"$d/test/deploy/smoke_up.sh"
# shellcheck disable=SC2016 # the $ must reach the stub unexpanded
printf '#!/bin/sh\necho "nested_task=$*"\nidentity\nexit "${STUB_TASK_EXIT:-0}"\n' >"$d/bin/task"
chmod +x "$d/bin/identity" "$d/bin/task"
mkdir -p "$d/.local/crosscodex"
(cd "$d" && SCRATCH="$d" PATH="$d/bin:$PATH" "$task_bin" "$@" 2>&1)
echo "task_exit=$?"
if [ -d "$d/.local/crosscodex" ]; then echo "pki=present"; else echo "pki=removed"; fi
