#!/bin/sh
# Downloads the pinned WAL-G PostgreSQL binary, verifies its sha256, and
# installs it as <install-dir>/wal-g. Shared by deploy/db/Containerfile and
# deploy/Dockerfile{,.local} so both images run the same WAL-G.
#
# The Ubuntu 22.04 build links glibc 2.35, which the Debian 12 based images
# (postgres/AGE, distroless/base-debian12) satisfy with glibc 2.36.
# To upgrade: change WALG_VERSION and both sums, taken from the release's
# .tar.gz.sha256 assets and recomputed with sha256sum after download.
#
# Usage: fetch-walg.sh <install-dir>
set -eu

WALG_VERSION=v3.0.9

[ $# -eq 1 ] || {
	echo "usage: fetch-walg.sh <install-dir>" >&2
	exit 2
}

arch=$(dpkg --print-architecture)
case "$arch" in
amd64)
	asset=wal-g-pg-22.04-amd64
	sum=4f03ee4679db7f660bfd1b2b8291ac5df247cd2ec48074b60c112f4897299f24
	;;
arm64)
	asset=wal-g-pg-22.04-aarch64
	sum=20191a37ad091a880c25f4cc907e6f0deb7f12bdd8a53fd62dbc7fcbf2eac712
	;;
*)
	echo "fetch-walg.sh: no pinned WAL-G build for architecture $arch" >&2
	exit 1
	;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/walg.tar.gz" "https://github.com/wal-g/wal-g/releases/download/${WALG_VERSION}/${asset}.tar.gz"
echo "$sum  $tmp/walg.tar.gz" | sha256sum -c -
tar -xzf "$tmp/walg.tar.gz" -C "$tmp"
install -m 0755 "$tmp/$asset" "$1/wal-g"
"$1/wal-g" --version
