#!/usr/bin/env bash
# Runs `task deploy:certs` twice against a stub podman whose named volumes are
# directories, seeding a key-volume layout from an older release in between,
# and prints what each volume holds afterwards. The second run must reload any
# volume whose listing differs from its allowlist (certs:load:volume's status
# check), so a seeded old layout ends up stripped.
#
# The stub runs certs:load:volume's real `sh -c` scripts on the host with the
# volume and source mounts rewritten to their directories; chown is a no-op
# (it needs root), everything else (rm, find, cp, chmod, ls) is real. The PKI
# is generated for real by deploy/certgen into a scratch dir.
#
# Usage: certs-load-upgrade.sh <repo-root> <layout>
#   layout: original  one shared volume with every *.pem, ca-key.pem included
#                     (releases before the key split), no other key volume
#           split2    server volume correct, client volume also holding the
#                     attestation keypair (472c6eb1), no attestation volume
#           current   the layout the first run loaded, left as it is
# Prints: server=[...] client=[...] attestation=[...] (C-sorted `ls -A`), and
#   reloads=N, the number of volumes the second run emptied and reloaded.
set -u
root="${1:?Usage: certs-load-upgrade.sh <repo-root> <original|split2|current>}"
layout="${2:?Usage: certs-load-upgrade.sh <repo-root> <original|split2|current>}"
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
mkdir -p "$d/bin" "$d/shim" "$d/vols"
printf '#!/bin/sh\nexit 0\n' >"$d/shim/chown"
chmod +x "$d/shim/chown"
cat >"$d/bin/podman" <<'STUB'
#!/usr/bin/env bash
# Stub podman: volumes are directories under $STUB_VOLS; `run` executes an
# `--entrypoint sh ... -c <script>` on the host with mounts rewritten.
set -u
case "$1" in
build) exit 0 ;;
volume)
	[ "$2" = create ] || { echo "stub podman: unsupported: $*" >&2; exit 2; }
	mkdir -p "$STUB_VOLS/$3"
	exit 0
	;;
run) shift ;;
*) echo "stub podman: unsupported: $*" >&2; exit 2 ;;
esac
mounts=() entrypoint=""
while [ $# -gt 0 ]; do
	case "$1" in
	--rm) shift ;;
	--user | --cap-drop | --cap-add | --security-opt) shift 2 ;;
	-v) mounts+=("$2"); shift 2 ;;
	--entrypoint) entrypoint="$2"; shift 2 ;;
	-*) echo "stub podman: unsupported run option $1" >&2; exit 2 ;;
	*) shift; break ;; # the image
	esac
done
if [ "$entrypoint" != sh ] || [ "${1:-}" != -c ]; then
	echo "stub podman: only '--entrypoint sh IMAGE -c SCRIPT' is supported" >&2
	exit 2
fi
script="$2"
for m in "${mounts[@]}"; do
	IFS=: read -r src dst _ <<<"$m"
	case "$src" in
	/*) host="$src" ;;
	*) host="$STUB_VOLS/$src"; mkdir -p "$host" ;; # podman run creates a missing named volume
	esac
	script="${script//$dst/$host}"
done
case "$script" in *"-mindepth 1 -delete"*) echo reload >>"$STUB_LOG" ;; esac
PATH="$STUB_SHIM:$PATH" exec sh -c "$script"
STUB
chmod +x "$d/bin/podman"

export STUB_VOLS="$d/vols" STUB_SHIM="$d/shim" STUB_LOG="$d/reloads.log"
export CERTS_DIR="$d/certs" CONTAINER_ENGINE=podman
export CROSSCODEX_CERTS_VOLUME=srv CROSSCODEX_CLIENT_CERTS_VOLUME=cli CROSSCODEX_ATTESTATION_KEYS_VOLUME=att
export PATH="$d/bin:$PATH"
cd "$root" || exit 1

if ! task deploy:certs >"$d/first.log" 2>&1; then
	echo "first run failed:"
	cat "$d/first.log"
	exit 1
fi
seed() { # seed <volume> <files...>: replace the volume's content
	local v="$STUB_VOLS/$1"
	shift
	rm -rf "$v" && mkdir -p "$v" && (cd "$CERTS_DIR" && cp "$@" .fingerprint "$v/")
}
case "$layout" in
original)
	(cd "$CERTS_DIR" && seed srv ./*.pem) && rm -rf "$STUB_VOLS/cli" "$STUB_VOLS/att"
	;;
split2)
	seed srv ca.pem server.pem server-key.pem &&
		seed cli ca.pem client.pem client-key.pem attestation-key.pem attestation-pub.pem &&
		rm -rf "$STUB_VOLS/att"
	;;
current) ;;
*)
	echo "unknown layout ${layout}"
	exit 1
	;;
esac || exit 1
# shellcheck disable=SC2012 # ls -A, C-sorted, is the listing the status check compares
echo "seeded_server=[$(LC_ALL=C ls -A "$STUB_VOLS/srv" | tr '\n' ' ')]"

: >"$STUB_LOG"
if ! task deploy:certs >"$d/second.log" 2>&1; then
	echo "second run failed:"
	cat "$d/second.log"
	exit 1
fi
for v in server:srv client:cli attestation:att; do
	# shellcheck disable=SC2012 # same listing as the status check
	echo "${v%%:*}=[$(LC_ALL=C ls -A "$STUB_VOLS/${v#*:}" 2>&1 | tr '\n' ' ')]"
done
echo "reloads=$(wc -l <"$STUB_LOG")"
