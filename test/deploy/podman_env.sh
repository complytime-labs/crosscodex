#!/usr/bin/env bash
# podman_env.sh — Source this (do not execute it) before any podman/compose call
# against the smoke stack, so every step sees the same podman runtime view.
# Sourced by test/deploy/smoke_up.sh and the deploy:smoke:down task.
#
# Podman overlay workaround: point buildah temp to a workspace subdir to avoid
# "mounting an overlay over build context directory" errors. Applies only inside
# a container (docker or podman); see scripts/in-container.sh for the markers
# checked. Values already set in the environment win. See
# docs/dev/podman-dev-container.md.
#
# Requires ROOT_DIR (repository root) in the environment.

if bash "${ROOT_DIR:?podman_env.sh needs ROOT_DIR set}/scripts/in-container.sh"; then
	export TMPDIR="${TMPDIR:-${ROOT_DIR}/.test-output/buildtmp}"
	export STORAGE_DRIVER="${STORAGE_DRIVER:-vfs}"
	mkdir -p "${TMPDIR}"
fi
