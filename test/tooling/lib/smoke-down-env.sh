#!/usr/bin/env bash
# Runs `task deploy:smoke:down` in a scratch copy of the repo with a stub
# podman-compose/podman that records TMPDIR and STORAGE_DRIVER, and prints what
# the container engine saw. Nothing real is torn down.
# Usage: smoke-down-env.sh <repo-root> <in-container: yes|no>
#   Container detection is forced by replacing scripts/in-container.sh in the
#   scratch copy. Pre-set TMPDIR/STORAGE_DRIVER in the caller's env are passed through.
set -u
root="${1:?Usage: smoke-down-env.sh <repo-root> <yes|no>}"
in_container="${2:?Usage: smoke-down-env.sh <repo-root> <yes|no>}"
d=$(TMPDIR=/tmp mktemp -d) # not caller TMPDIR: it may be the value under test
trap 'rm -rf "$d"' EXIT
mkdir -p "$d/scripts" "$d/test/deploy" "$d/deploy" "$d/bin"
cp "${root}/Taskfile.yml" "$d/"
cp -r "${root}/.taskfiles" "$d/"
cp "${root}"/deploy/*.yaml "${root}/deploy/.env.example" "$d/deploy/"
cp "${root}/deploy/.env.example" "$d/deploy/.env"
# Ship the shared env script when it exists, so this runs before and after it lands.
if [ -e "${root}/test/deploy/podman_env.sh" ]; then
	cp "${root}/test/deploy/podman_env.sh" "$d/test/deploy/"
fi
if [ "$in_container" = yes ]; then
	printf '#!/bin/sh\nexit 0\n' >"$d/scripts/in-container.sh"
else
	printf '#!/bin/sh\nexit 1\n' >"$d/scripts/in-container.sh"
fi
for stub in podman-compose podman; do
	# shellcheck disable=SC2016 # the $ must reach the stub unexpanded
	printf '#!/bin/sh\necho "engine=%s args=$*"\necho "TMPDIR=${TMPDIR-<unset>}"\necho "STORAGE_DRIVER=${STORAGE_DRIVER-<unset>}"\nexit 0\n' "$stub" >"$d/bin/$stub"
	chmod +x "$d/bin/$stub"
done
echo "scratch=$d"
(cd "$d" && PATH="$d/bin:$PATH" task deploy:smoke:down LLM_GATEWAY=litellm 2>&1)
echo "task_exit=$?"
