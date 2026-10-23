#!/usr/bin/env bash
# Build sshfs from the pinned upstream commit, optionally with the readdirplus
# patch, and print the resulting binary path.
#
# The sshfs source is NOT vendored into this repo: it is cloned at a pinned
# commit so the build is reproducible and the diff under test stays visible
# (see docs/supply-chain-pinning.md).
#
# Usage: build.sh <workdir> [patched|base]
#   build.sh /tmp/sshfs-base  base       -> <workdir>/sshfs        (unpatched)
#   build.sh /tmp/sshfs-plus  patched    -> <workdir>/sshfs        (patched)
set -eu

WORK=${1:?workdir}
MODE=${2:-patched}
PIN=a5d0c9b3b66ae00c1b97c95dbc00789f77b89dd0   # sshfs-3.7.6-12-ga5d0c9b
PATCH_DIR=$(cd "$(dirname "$0")" && pwd)

mkdir -p "$WORK"
SRC=$WORK/src
if [ ! -d "$SRC/.git" ]; then
	git clone --quiet https://github.com/libfuse/sshfs.git "$SRC"
fi
git -C "$SRC" fetch --quiet origin || true
git -C "$SRC" checkout --quiet "$PIN"
git -C "$SRC" clean -qfd
git -C "$SRC" rev-parse HEAD
git -C "$SRC" describe --tags

case $MODE in
patched)
	git -C "$SRC" apply "$PATCH_DIR/sshfs-readdirplus.patch"
	echo "applied $(basename "$PATCH_DIR/sshfs-readdirplus.patch")"
	;;
base) echo "unpatched build" ;;
*) echo "usage: build.sh <workdir> [patched|base]" >&2; exit 2 ;;
esac

( cd "$SRC" && meson setup "$WORK/build" --buildtype release > "$WORK/meson.log" 2>&1 &&
  ninja -C "$WORK/build" >> "$WORK/meson.log" 2>&1 )
cp "$WORK/build/sshfs" "$WORK/sshfs"
"$WORK/sshfs" --version
echo "binary: $WORK/sshfs"
