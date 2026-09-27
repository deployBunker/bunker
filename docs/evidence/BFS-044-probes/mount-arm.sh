#!/usr/bin/env bash
# mount-arm.sh — one measured mount arm for BFS-044 (the client config surface).
#
# Conventions copied from BFS-012/BFS-030's mount-arm.sh, for the same reasons:
#   * a FRESH run directory per arm, so nothing is inherited;
#   * every process killed by explicit PID, never `pkill -f` (the pattern matches
#     the shell running it);
#   * every fusermount bounded with `timeout`.
#
# usage: mount-arm.sh --label L --tree DIR --bin BUNKER --davserve DS \
#                     --reader SCRIPT [--reader-args "…"] [--work DIR] \
#                     [--flags "…"] [--mount-timeout-s N] [--reader-timeout-s N]
set -uo pipefail

LABEL=""; TREE=""; BIN=""; DS=""; READER=""; RARGS=""; WORK=/tmp/bfs044
FLAGS=""; MTMO=300; RTO=600
while [ $# -gt 0 ]; do
  case "$1" in
    --label) LABEL="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --reader) READER="$2"; shift 2;;
    --reader-args) RARGS="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --flags) FLAGS="$2"; shift 2;;
    --mount-timeout-s) MTMO="$2"; shift 2;;
    --reader-timeout-s) RTO="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$LABEL" ] && [ -n "$TREE" ] && [ -n "$BIN" ] && [ -n "$DS" ] && [ -n "$READER" ] \
  || { echo "missing arg (label/tree/bin/davserve/reader)" >&2; exit 2; }

OUT="$WORK/run-$LABEL"
[ -e "$OUT" ] && { echo "refusing to reuse $OUT (a fresh directory per arm)" >&2; exit 2; }
mkdir -p "$OUT"
export XDG_CACHE_HOME="$OUT/xdg"
MNT="$OUT/mnt"; mkdir -p "$MNT"

cleanup() {
  if [ -n "${MNT_PID:-}" ]; then kill -TERM "$MNT_PID" 2>/dev/null; sleep 0.3; kill -KILL "$MNT_PID" 2>/dev/null; fi
  timeout 20 fusermount3 -u "$MNT" >/dev/null 2>&1 || timeout 20 fusermount -u "$MNT" >/dev/null 2>&1
  if [ -n "${SRV_PID:-}" ]; then kill -TERM "$SRV_PID" 2>/dev/null; fi
}
trap cleanup EXIT

echo "arm        = $LABEL"
echo "tree       = $TREE  ($(find "$TREE" -type f | wc -l | tr -d ' ') files, $(find "$TREE" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}') B)"
echo "binaries   = $BIN ; $DS"

"$DS" --root "$TREE" --addr 127.0.0.1:0 >"$OUT/davserve.out" 2>&1 &
SRV_PID=$!
URL=""
for _ in $(seq 1 80); do
  URL=$(sed -n 's/^URL=//p' "$OUT/davserve.out" | head -1)
  [ -n "$URL" ] && break
  sleep 0.25
done
[ -n "$URL" ] || { echo "davserve did not start"; cat "$OUT/davserve.out"; exit 1; }
echo "endpoint   = $URL (pid $SRV_PID)"
echo "mount-args = --url $URL $FLAGS"

timeout "$MTMO" "$BIN" fs mount "$MNT" --url "$URL" $FLAGS >"$OUT/mount.log" 2>&1 &
MNT_PID=$!
for _ in $(seq 1 200); do
  grep -q " $MNT " /proc/mounts && break
  kill -0 "$MNT_PID" 2>/dev/null || { echo "MOUNT EXITED (rc from the mount itself)"; cat "$OUT/mount.log"; exit 1; }
  sleep 0.25
done
grep -q " $MNT " /proc/mounts || { echo "MOUNT DID NOT COME UP"; cat "$OUT/mount.log"; exit 1; }
CDIR=$(ls -td "$OUT/xdg/bunker/fs"/* 2>/dev/null | head -1)
echo "cache-dir  = $CDIR"
echo "--- the mount's own banner (the resolved bounds, BFS-044) ---"
sed 's/^/  | /' "$OUT/mount.log"

MNT="$MNT" CDIR="$CDIR" TREE="$TREE" OUT="$OUT" URL="$URL" \
  timeout "$RTO" "$READER" $RARGS
RRC=$?
echo "reader     = $READER $RARGS  (rc=$RRC)"

sleep 1
cp "$CDIR/status.json" "$OUT/status.json" 2>/dev/null
cp "$CDIR/conflicts.jsonl" "$OUT/conflicts.jsonl" 2>/dev/null
echo "--- the cache's own figures (the reported bound) ---"
grep -E '"(used_bytes|max_bytes|entries|max_entries|blobs|index_bytes|reserved_bytes|in_flight_bytes|max_inflight|bypass_events|hits|misses|oversize_bypasses)":' \
  "$OUT/status.json" 2>/dev/null | sed 's/^/  | /'
echo "--- the effective config block, read back from the status document ---"
"$BIN" fs status 2>/dev/null | sed -n '/config *:/,$p' | sed 's/^/  | /'
echo "run dir    : $OUT"
exit $RRC
