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
#        PRESENT lines "<id>:<path>" whose file exists in that container (exec
#                test -e); default none.
#        VOLUME_FILES lines "<volume>/<file>[=<content>]" added to (or, with
#                content, replacing the content of) the default volume contents:
#                each service volume holds ca.pem, server.pem and server-key.pem
#                (the key's content is the service name), the client and
#                attestation volumes their own files; default none.
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

default_table='db 0000000000000000 256 crosscodex-smoke-certs-db
nats 0000000000000000 256 crosscodex-smoke-certs-nats
litellm-tls 0000000000000000 256 crosscodex-smoke-certs-litellm
migrate 0000000000000000 256 smoke-client-certs
crosscodexd 0000000000000000 256 crosscodex-smoke-certs-crosscodexd,smoke-client-certs,smoke-attestation-keys'
printf '%s\n' "${TABLE:-$default_table}" >"$d/table"
printf '%s\n' "${PRESENT:-}" >"$d/present"

# Host PKI dir (smoke_up.sh's default CERTS_DIR) and the volumes loaded from it:
# a key's content is its name, so a volume holding another service's key is
# told apart by content.
host="$d/.test-output/deploy-smoke/certs"
mkdir -p "$host"
for k in ca client attestation db nats litellm praxis crosscodexd; do
	printf '%s\n' "$k" >"$host/$k-key.pem"
done
mkvol() { # mkvol <volume> <file>=<content>...
	local v="$d/vols/$1"
	shift
	mkdir -p "$v"
	local f
	for f in "$@"; do printf '%s\n' "${f#*=}" >"$v/${f%%=*}"; done
	printf 'fingerprint\n' >"$v/.fingerprint"
}
for svc in db nats litellm praxis crosscodexd; do
	mkvol "crosscodex-smoke-certs-$svc" ca.pem=ca server.pem="$svc" server-key.pem="$svc"
done
mkvol smoke-client-certs ca.pem=ca client.pem=client client-key.pem=client
mkvol smoke-attestation-keys attestation-key.pem=attestation attestation-pub.pem=attestation
while IFS= read -r line; do
	[ -n "$line" ] || continue
	vf="${line%%=*}"
	mkdir -p "$d/vols/$(dirname "$vf")"
	if [[ "$line" == *=* ]]; then printf '%s\n' "${line#*=}" >"$d/vols/$vf"; else printf 'extra\n' >"$d/vols/$vf"; fi
done <<<"${VOLUME_FILES:-}"
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
	# Volume helpers: `--entrypoint sh ... -c <ls>` lists the volume,
	# `--entrypoint sha256sum ... /v/<file>` hashes one of its files.
	if [[ "$*" =~ -v[[:space:]]([^:]+):/v: ]]; then
		vol="$d/vols/${BASH_REMATCH[1]}"
		if [[ "$*" == *"--entrypoint sh "* ]]; then
			LC_ALL=C ls -A "$vol" | tr '\n' ' '
			exit 0
		fi
		if [[ "$*" == *"--entrypoint sha256sum"* ]]; then
			f="${*: -1}"
			sha256sum "$vol/${f#/v/}" || exit 1
			exit 0
		fi
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
