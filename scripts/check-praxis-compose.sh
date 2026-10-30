#!/usr/bin/env bash
# check-praxis-compose.sh — Refuse the Praxis LLM gateway on a compose tool that
# ignores volume subpath.
#
# compose.gateway-praxis.yaml narrows the cert volume to three files with
# subpath. podman-compose 1.0.6 through 1.5.0 silently drop it and mount the
# whole volume, CA key included, into the third-party Praxis image. Only
# podman-compose 1.6.0 or later (a final release, not a pre-release) is
# verified to honour it, so anything else is refused.
#
# Called by `task deploy:up` only when LLM_GATEWAY is exactly "praxis"; the
# Taskfile gates the call so an unvalidated value never reaches a shell.
#
# Usage: check-praxis-compose.sh <compose-cmd>
#   compose-cmd: the compose command the Taskfile resolved (COMPOSE_CMD)
# Exit: 0 if allowed, 1 (message on stderr) if refused.
set -euo pipefail

compose="${1:?Usage: check-praxis-compose.sh <compose-cmd>}"
why="Praxis must receive only ca.pem, server.pem and server-key.pem; a compose tool that ignores volume subpath mounts the whole cert volume, exposing the CA private key (ca-key.pem) to the third-party Praxis container."

if [ "$compose" != podman-compose ]; then
	echo "Refusing LLM_GATEWAY=praxis: the compose tool is '$compose', but the Praxis gateway is verified only with podman-compose 1.6.0 or later. $why Install podman-compose 1.6.0+ (pip install --user --upgrade podman-compose) and use CONTAINER_ENGINE=podman, or deploy with LLM_GATEWAY=litellm." >&2
	exit 1
fi

# 1.0.6 through 1.6.0 print "podman-compose version X.Y.Z" on stdout, plus
# podman's own "podman version ..." line (sometimes first). A failing or
# missing command yields no version, which is refused below.
output="$(podman-compose version 2>/dev/null)" || output=""
ver="$(printf '%s\n' "$output" | sed -nE 's/^podman-compose version ([^[:space:]]+)[[:space:]]*$/\1/p' | head -n 1)"

# Only a plain X.Y.Z release qualifies: 1.6.0rc1 and the like are refused.
if [[ "$ver" =~ ^([0-9]+)\.([0-9]+)\.[0-9]+$ ]]; then
	major="${BASH_REMATCH[1]}"
	minor="${BASH_REMATCH[2]}"
	if [ "$major" -gt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -ge 6 ]; }; then
		exit 0
	fi
fi

echo "Refusing LLM_GATEWAY=praxis: podman-compose ${ver:-(version not detected)} is not 1.6.0 or later. $why Upgrade with: pip install --user --upgrade podman-compose. Or deploy with LLM_GATEWAY=litellm." >&2
exit 1
