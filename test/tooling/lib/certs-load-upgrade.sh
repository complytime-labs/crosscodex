#!/usr/bin/env bash
# Runs `task deploy:certs` against a stub podman whose named volumes are
# directories, optionally seeding the host PKI dir or a volume with a layout from
# an earlier release, and prints what each volume and the host dir hold
# afterwards. The final run must reload any volume whose listing differs from its
# allowlist (certs:load:volume's status check), and replace a host PKI in the
# shared-server-key layout (deploy/certgen/gen.go --verify fails on it).
#
# The stub runs certs:load:volume's real `sh -c` scripts on the host with the
# volume and source mounts rewritten to their directories; chown is a no-op
# (it needs root), everything else (rm, find, cp, chmod, ls) is real. The PKI
# is generated for real by deploy/certgen into a scratch dir.
#
# Usage: certs-load-upgrade.sh <repo-root> <layout>
#   layout: legacy       the host PKI dir holds the shared-server-key set of
#                        earlier releases (server.pem, server-key.pem), and one
#                        shared volume holds it; no per-service volumes exist
#           foreign-key  per-service volume db holds nats's key and the client
#                        volume also holds the attestation keypair
#           current      the layout the first run loaded, left as it is
# Prints: host=[...] (C-sorted `ls -A` of the PKI dir), <service>=[...] for each
#   service volume, client=[...], attestation=[...], db_key=own|foreign,
#   nats_key_in_db=yes|no, reloads=N (volumes the last run emptied and reloaded)
#   and, for legacy, ca_replaced=yes|no.
set -u
root="${1:?Usage: certs-load-upgrade.sh <repo-root> <legacy|foreign-key|current>}"
layout="${2:?Usage: certs-load-upgrade.sh <repo-root> <legacy|foreign-key|current>}"
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

ls_c() { # ls_c <dir>: C-sorted `ls -A`, the listing the status check compares
	# shellcheck disable=SC2012
	LC_ALL=C ls -A "$1" 2>&1 | tr '\n' ' '
}
seed() { # seed <volume> <files...>: replace the volume's content with files from CERTS_DIR
	local v="$STUB_VOLS/$1"
	shift
	rm -rf "$v" && mkdir -p "$v" && (cd "$CERTS_DIR" && cp "$@" .fingerprint "$v/")
}

if [ "$layout" = legacy ]; then
	go run ./internal/testcerts/gen.go "$CERTS_DIR" >/dev/null || exit 1
	old_ca=$(cat "$CERTS_DIR/ca.pem")
	seed srv ca.pem server.pem server-key.pem
	echo "seeded_host=[$(ls_c "$CERTS_DIR")]"
fi

if ! task deploy:certs >"$d/first.log" 2>&1; then
	echo "first run failed:"
	cat "$d/first.log"
	exit 1
fi
case "$layout" in
legacy)
	[ "$old_ca" != "$(cat "$CERTS_DIR/ca.pem")" ] && echo "ca_replaced=yes" || echo "ca_replaced=no"
	;;
foreign-key)
	# db's volume gets nats's key under db's file names; the client volume also
	# gets the attestation keypair.
	rm -rf "$STUB_VOLS/srv-db" && mkdir -p "$STUB_VOLS/srv-db" &&
		cp "$STUB_VOLS/srv-nats/ca.pem" "$STUB_VOLS/srv-nats/server.pem" "$STUB_VOLS/srv-nats/server-key.pem" "$STUB_VOLS/srv-nats/.fingerprint" "$STUB_VOLS/srv-db/" &&
		seed cli ca.pem client.pem client-key.pem attestation-key.pem attestation-pub.pem || exit 1
	;;
current) ;;
*)
	echo "unknown layout ${layout}"
	exit 1
	;;
esac

: >"$STUB_LOG"
if ! task deploy:certs >"$d/second.log" 2>&1; then
	echo "second run failed:"
	cat "$d/second.log"
	exit 1
fi
echo "host=[$(ls_c "$CERTS_DIR")]"
for svc in db nats litellm praxis crosscodexd; do
	echo "${svc}=[$(ls_c "$STUB_VOLS/srv-$svc")]"
done
echo "client=[$(ls_c "$STUB_VOLS/cli")]"
echo "attestation=[$(ls_c "$STUB_VOLS/att")]"
cmp -s "$STUB_VOLS/srv-db/server-key.pem" "$CERTS_DIR/db-key.pem" && echo "db_key=own" || echo "db_key=foreign"
cmp -s "$STUB_VOLS/srv-db/server-key.pem" "$CERTS_DIR/nats-key.pem" && echo "nats_key_in_db=yes" || echo "nats_key_in_db=no"
echo "reloads=$(wc -l <"$STUB_LOG")"
