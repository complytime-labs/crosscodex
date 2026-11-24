#!/usr/bin/env bash
# Runs `<compose> -f compose.yaml -f compose.ingestion.yaml -f compose.rustfs.yaml
# config` in a scratch copy of deploy/ (its .env copied from .env.example) with
# the RustFS credentials set, unset or empty as asked, under a throwaway
# compose project. `config` only parses; nothing is pulled, built or started.
# Usage: compose-rustfs-config.sh <repo-root> <compose command> <access> <secret>
#        <access>, <secret>: set | unset | empty
# Prints "config_exit=<code>", the tool's stderr, "parsed=<service>" for each
# of rustfs, rustfs-init and ingestion in the resolved config, and
# "state=containers:<n> volumes:<n> networks:<n>": what the container engine
# holds for the throwaway project afterwards.
set -u
root="${1:?Usage: compose-rustfs-config.sh <repo-root> <compose command> <access> <secret>}"
compose="${2:?compose command}"
access="${3:?access: set, unset or empty}"
secret="${4:?secret: set, unset or empty}"
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
cp "${root}"/deploy/*.yaml "${root}/deploy/.env.example" "$d/"
cp "${root}/deploy/.env.example" "$d/.env"
project="crosscodex-rustfs-config-check-$$"
case "${compose}" in
docker*) engine=docker ;;
*) engine=podman ;;
esac
# Both keys are removed from the environment first (env -u), so "unset" adds
# nothing back.
cred_env() { # cred_env <name> <set|unset|empty> <value>
	case "$2" in
	set) echo "$1=$3" ;;
	empty) echo "$1=" ;;
	unset) ;;
	esac
}
# shellcheck disable=SC2046,SC2086 # cred_env yields separate env(1) arguments, and the compose command may be two words ("podman compose")
(cd "$d" && env -u RUSTFS_ACCESS_KEY -u RUSTFS_SECRET_KEY \
	$(cred_env RUSTFS_ACCESS_KEY "${access}" a1b2c3d4e5f6) \
	$(cred_env RUSTFS_SECRET_KEY "${secret}" 0a1b2c3d4e5f6a7b) \
	COMPOSE_PROJECT_NAME="${project}" \
	${compose} -f compose.yaml -f compose.ingestion.yaml -f compose.rustfs.yaml config >"$d/out" 2>"$d/err")
echo "config_exit=$?"
cat "$d/err"
for svc in rustfs rustfs-init ingestion; do
	grep -qx "  ${svc}:" "$d/out" && echo "parsed=${svc}"
done
containers=$("${engine}" ps -a -q --filter "label=com.docker.compose.project=${project}" | wc -l)
volumes=$("${engine}" volume ls -q --filter "name=${project}" | wc -l)
networks=$("${engine}" network ls -q --filter "name=${project}" | wc -l)
echo "state=containers:${containers} volumes:${volumes} networks:${networks}"
