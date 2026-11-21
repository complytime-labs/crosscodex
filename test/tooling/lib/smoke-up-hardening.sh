#!/usr/bin/env bash
# Runs test/deploy/smoke_up.sh against a stub `task` and a stub container
# engine so its hardening and key-separation gates can be exercised without a
# stack. Nothing is started. The stub engine answers ps/inspect/exec/run from
# two tables in the environment; /proc/<pid>/status is faked by giving each
# container a pid of "../<tmpdir>/proc/<svc>" (smoke_up.sh reads
# /proc/${pid}/status, which then resolves to the fake file).
# Usage: smoke-up-hardening.sh <repo-root>
# Env:   TABLE   rows "<service> <CapBnd hex> <PidsLimit> <comma-separated mounted volumes or ->";
#                default is a compliant stack. The container id is the service name.
#        PRESENT lines "<id>:<path>" (exec) or "<volume>:<path>" (run) whose key
#                file exists; default none.
# Prints smoke_up.sh output, then "smoke_up exit=<code>".
set -u
root="${1:?Usage: smoke-up-hardening.sh <repo-root>}"
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
mkdir -p "$d/scripts" "$d/test/deploy" "$d/deploy/litellm" "$d/bin" "$d/proc"
cp "${root}/test/deploy/podman_env.sh" "$d/test/deploy/"
cp "${root}/scripts/in-container.sh" "${root}/scripts/generate-llm-gateway-config.sh" \
	"${root}/scripts/check-openai-compat-models.sh" "$d/scripts/"
cp "${root}"/deploy/litellm/*.tmpl "$d/deploy/litellm/"
printf '#!/bin/sh\nexit 0\n' >"$d/bin/task"

default_table='db 0000000000000000 256 -
nats 0000000000000000 256 -
litellm-tls 0000000000000000 256 -
migrate 0000000000000000 256 smoke-client-certs
crosscodexd 0000000000000000 256 smoke-client-certs,smoke-attestation-keys'
printf '%s\n' "${TABLE:-$default_table}" >"$d/table"
printf '%s\n' "${PRESENT:-}" >"$d/present"
while read -r svc capbnd _; do
	mkdir -p "$d/proc/$svc"
	printf 'CapBnd:\t%s\nNoNewPrivs:\t1\n' "$capbnd" >"$d/proc/$svc/status"
done <"$d/table"

cat >"$d/bin/engine" <<'STUB'
#!/usr/bin/env bash
d="$(dirname "$0")/.."
row() { awk -v s="$1" '$1 == s' "$d/table"; }
case "$1" in
image) exit 0 ;;
ps)
	if [[ "$*" =~ com.docker.compose.service=([^[:space:]]+) ]]; then
		[ -n "$(row "${BASH_REMATCH[1]}")" ] && echo "${BASH_REMATCH[1]}"
	else
		awk '{print $1}' "$d/table"
	fi
	;;
inspect)
	id="${*: -1}"
	read -r svc _ pids mounts <<<"$(row "$id")"
	if [[ "$*" == *State.Pid* ]]; then
		echo "$svc ../$d/proc/$svc 1073741824 $pids true"
	else
		[ "$mounts" = "-" ] && mounts=""
		echo "$svc ${mounts//,/ }"
	fi
	;;
exec) [ "$4" = "-r" ] && exit 0; grep -qxF "$2:${*: -1}" "$d/present" && exit 0 || exit 1 ;;
run)
	if [[ "$*" =~ -v[[:space:]]([^:]+):/v: ]]; then
		grep -qxF "${BASH_REMATCH[1]}:${*: -1}" "$d/present" && exit 0 || exit 1
	fi
	exit 1
	;;
esac
STUB
chmod +x "$d/bin/task" "$d/bin/engine"
env -u OPENAI_COMPAT_HOST OLLAMA_HOST=http://localhost:11434 \
	CROSSCODEX_CLIENT_CERTS_VOLUME=smoke-client-certs CROSSCODEX_ATTESTATION_KEYS_VOLUME=smoke-attestation-keys \
	PATH="$d/bin:$PATH" CONTAINER_ENGINE="$d/bin/engine" ROOT_DIR="$d" \
	bash "${root}/test/deploy/smoke_up.sh" 2>&1
echo "smoke_up exit=$?"
