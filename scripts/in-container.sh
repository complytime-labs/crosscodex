#!/usr/bin/env bash
# in-container.sh — Exit 0 when running inside a container, 1 otherwise.
#
# Usage: in-container.sh [root]
#   root: Filesystem root to inspect (default: /). Tests pass a fake root.
#
# Markers, any one of which is enough:
#   * /run/.containerenv         — written by podman (it creates no /.dockerenv)
#   * the `container` variable   — set by podman, systemd-nspawn, and LXC
#   * /.dockerenv                — written by docker
#   * a docker|containerd|lxc|kubepods path in /proc/1/cgroup or /proc/self/cgroup
set -euo pipefail

ROOT="${1:-}"
ROOT="${ROOT%/}"

if [ -e "${ROOT}/run/.containerenv" ] || [ -n "${container:-}" ] || [ -e "${ROOT}/.dockerenv" ]; then
	exit 0
fi
for cgroup in "${ROOT}/proc/1/cgroup" "${ROOT}/proc/self/cgroup"; do
	if grep -qE 'docker|containerd|lxc|kubepods' "${cgroup}" 2>/dev/null; then
		exit 0
	fi
done
exit 1
