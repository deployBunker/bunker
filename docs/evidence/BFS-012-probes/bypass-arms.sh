#!/usr/bin/env bash
# bypass-arms.sh — BFS-012 deliverable 1, part 2: the two ways a read meets a full
# cache it may NOT evict, driven through a real mount.
#
# BFS-009 measured `bypass=0 oversize=0` in every arm it ran. Its bound arms used a
# SERIAL reader against a large tree, so every insert could evict an unpinned
# neighbour and neither bypass class was ever reached at the mount level (the unit
# suite reaches them with the Cache API). The owner's rule — "local storage MUST
# NOT grow with the tree" — is only satisfied by a client that BYPASSES when it
# cannot evict, so the live path has to be shown, not inferred from a unit test.
#
#   arm OVER  --cache-max-entry-bytes 64 KiB against 1 MiB files: every bulk read
#             is an entry the cap refuses; the read must still succeed and no bulk
#             blob may exist on disk afterwards.
#   arm PIN   --cache-max-size 4 MiB against 1 MiB files, three read handles held
#             OPEN: the three cached blobs are pinned, a pinned blob is never an
#             eviction candidate, so the fourth read finds a full cache with
#             nothing evictable and must BYPASS (bypass_events moves) rather than
#             fail or grow.
#
# usage: bypass-arms.sh --bin B --davserve D --tree T --work W [--probe-dir DIR]
set -uo pipefail

BIN=""; DS=""; TREE=""; WORK=/tmp/bfs012; PROBEDIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --probe-dir) PROBEDIR="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$BIN" ] && [ -n "$DS" ] && [ -n "$TREE" ] || { echo "missing arg" >&2; exit 2; }

rc=0

echo "########################################################################"
echo "# ARM OVER — a single entry above --cache-max-entry-bytes is never cached,"
echo "# and every read still returns the right bytes."
echo "########################################################################"
bash "$PROBEDIR/mount-arm.sh" --label C-oversize --tree "$TREE" --bin "$BIN" --davserve "$DS" \
  --work "$WORK" --flag "--cache-max-entry-bytes 65536" \
  --reader "$PROBEDIR/bypass_check.py" --reader-args "--hold 0 --dir src/bulk --settle 1" \
  --mount-timeout-s 300
rc=$(( rc + $? ))

echo
echo "########################################################################"
echo "# ARM PIN — every cached blob pinned: the cache is full and NOTHING is"
echo "# evictable, so the next read must BYPASS."
echo "########################################################################"
bash "$PROBEDIR/mount-arm.sh" --label D-pinned --tree "$TREE" --bin "$BIN" --davserve "$DS" \
  --work "$WORK" --flag "--cache-max-size 4194304" \
  --reader "$PROBEDIR/bypass_check.py" --reader-args "--hold 3 --dir src/bulk --settle 4" \
  --mount-timeout-s 300
rc=$(( rc + $? ))

echo
echo "bypass-arms aggregate rc=$rc  (0 = every arm's own assertions held)"
exit $rc
