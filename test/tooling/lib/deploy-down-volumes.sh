#!/usr/bin/env bash
# Runs `task deploy:down VOLUMES=1` in a scratch copy of the repo against a stub
# podman-compose and a stub podman whose volumes live in a list file. The stub
# resolves `volume rm` names the way podman 6.1 does: an exact name removes
# that volume, a name that is only a prefix of several volumes fails with
# "more than one result", a name matching nothing fails unless -f is given.
# Usage: deploy-down-volumes.sh <repo-root>
# Env:   EXTRA_VOLUMES    volume names, space-separated, that exist before teardown
#                         besides the local stack's per-service, client and
#                         attestation key volumes (which always exist)
#        STUCK            a volume name whose removal fails (it is in use)
#        LS_FAILS         non-empty: `volume ls` fails
# Prints each removal, then "task_exit=<code>" and "left=<remaining volumes>".
set -u
root="${1:?Usage: deploy-down-volumes.sh <repo-root>}"
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
mkdir -p "$d/deploy" "$d/bin"
cp "${root}/Taskfile.yml" "$d/"
cp -r "${root}/.taskfiles" "$d/"
cp "${root}"/deploy/*.yaml "${root}/deploy/.env.example" "$d/deploy/"
cp "${root}/deploy/.env.example" "$d/deploy/.env"
for v in db nats litellm praxis crosscodexd; do echo "crosscodex-local-certs-$v"; done >"$d/vols"
# shellcheck disable=SC2086 # EXTRA_VOLUMES splits into one name per word
printf '%s\n' crosscodex-local-client-certs crosscodex-local-attestation-keys ${EXTRA_VOLUMES:-} >>"$d/vols"
printf '%s\n' "${STUCK:-}" >"$d/stuck"
[ -n "${LS_FAILS:-}" ] && touch "$d/ls-fails"
printf '#!/bin/sh\nexit 0\n' >"$d/bin/podman-compose"
cat >"$d/bin/podman" <<'STUB'
#!/usr/bin/env bash
d="$(dirname "$0")/.."
[ "$1" = volume ] || exit 0
case "$2" in
ls)
	[ -e "$d/ls-fails" ] && { echo "Error: cannot list volumes" >&2; exit 125; }
	cat "$d/vols"
	;;
rm)
	shift 2
	force=""
	for a in "$@"; do [ "$a" = -f ] && force=1; done
	# Resolve every name before removing any, as podman does.
	names=()
	for a in "$@"; do
		[ "$a" = -f ] && continue
		if grep -qxF "$a" "$d/vols"; then
			names+=("$a")
			continue
		fi
		n=$(grep -c "^$a" "$d/vols")
		if [ "$n" -gt 1 ]; then
			echo "Error: more than one result for volume name $a: volume already exists" >&2
			exit 125
		elif [ "$n" -eq 1 ]; then
			names+=("$(grep "^$a" "$d/vols")")
		elif [ -z "$force" ]; then
			echo "Error: no volume with name \"$a\" found: no such volume" >&2
			exit 1
		fi
	done
	for name in "${names[@]}"; do
		if grep -qxF "$name" "$d/stuck"; then
			echo "Error: volume $name is being used" >&2
			exit 2
		fi
		grep -vxF "$name" "$d/vols" >"$d/vols.new"
		mv "$d/vols.new" "$d/vols"
		echo "removed=$name"
	done
	;;
esac
STUB
chmod +x "$d/bin/podman" "$d/bin/podman-compose"
(cd "$d" && PATH="$d/bin:$PATH" CONTAINER_ENGINE=podman \
	CROSSCODEX_CERTS_VOLUME=crosscodex-local-certs \
	CROSSCODEX_CLIENT_CERTS_VOLUME=crosscodex-local-client-certs \
	CROSSCODEX_ATTESTATION_KEYS_VOLUME=crosscodex-local-attestation-keys \
	task deploy:down VOLUMES=1 2>&1)
echo "task_exit=$?"
echo "left=[$(tr "\n" " " <"$d/vols")]"
