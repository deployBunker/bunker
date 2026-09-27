#!/usr/bin/env bash
# arm.sh — one measured mount arm for BFS-020 (a created name is not visible to a
# following operation until it is published).
#
# Starts the repo's own client-facing surface (probes/davserve, the same handler
# bunkerd serves), mounts it with the bunker-fs client, runs a reader, and leaves
# the raw output plus the client's own records in the run directory.
#
# Conventions (the same ones BFS-012/BFS-021's arms script states, for the same
# reasons):
#   * a FRESH run directory per arm, so nothing is inherited;
#   * every process killed by explicit PID, never `pkill -f` (the pattern matches
#     the shell running it);
#   * every mount and every fusermount bounded with `timeout`.
#
# usage: arm.sh --label L --tree DIR --bin BUNKER --davserve DS --reader CMD \
#               [--reader-args "ARGS"] [--work DIR] [--mount-args "FLAGS"] \
#               [--timeout-s N] [--keep]
set -uo pipefail

LABEL=""; TREE=""; BIN=""; DS=""; READER=""; RARGS=""; WORK=/tmp/bfs020
MFLAGS=""; RTO=600; MTMO=900; KEEP=0
while [ $# -gt 0 ]; do
  case "$1" in
    --label) LABEL="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --reader) READER="$2"; shift 2;;
    --reader-args) RARGS="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --mount-args) MFLAGS="$2"; shift 2;;
    --timeout-s) RTO="$2"; shift 2;;
    --mount-timeout-s) MTMO="$2"; shift 2;;
    --keep) KEEP=1; shift;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$LABEL" ] && [ -n "$TREE" ] && [ -n "$BIN" ] && [ -n "$DS" ] && [ -n "$READER" ] || {
  echo "missing arg (need --label --tree --bin --davserve --reader)" >&2; exit 2; }

OUT="$WORK/run-$LABEL"
[ -e "$OUT" ] && { echo "refusing to reuse $OUT (a fresh directory per arm)" >&2; exit 2; }
mkdir -p "$OUT"
export XDG_CACHE_HOME="$OUT/xdg"
MNT="$OUT/mnt"; mkdir -p "$MNT"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

cleanup() {
  RC=$?
  if [ -n "${MNT_PID:-}" ]; then kill -TERM "$MNT_PID" 2>/dev/null; sleep 0.3; kill -KILL "$MNT_PID" 2>/dev/null; fi
  timeout 20 fusermount -u "$MNT" >/dev/null 2>&1
  if [ -n "${SRV_PID:-}" ]; then kill -TERM "$SRV_PID" 2>/dev/null; sleep 0.2; kill -KILL "$SRV_PID" 2>/dev/null; fi
  [ "$KEEP" = 1 ] && echo "run dir kept: $OUT"
  exit $RC
}
trap cleanup EXIT

echo "arm        = $LABEL"
echo "host       = $(uname -n) loadavg $(cat /proc/loadavg)"
echo "tree       = $TREE  ($(find "$TREE" -type f | wc -l) files, $(find "$TREE" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}') B)"
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

echo "mount-args = --url $URL $MFLAGS"
# shellcheck disable=SC2086
timeout "$MTMO" "$BIN" fs mount "$MNT" --url "$URL" $MFLAGS >"$OUT/mount.log" 2>&1 &
MNT_PID=$!
for _ in $(seq 1 200); do
  grep -q " $MNT " /proc/mounts && break
  kill -0 "$MNT_PID" 2>/dev/null || { echo "MOUNT EXITED"; cat "$OUT/mount.log"; exit 1; }
  sleep 0.25
done
grep -q " $MNT " /proc/mounts || { echo "MOUNT DID NOT COME UP"; cat "$OUT/mount.log"; exit 1; }
CDIR=$(ls -td "$OUT/xdg/bunker/fs"/* 2>/dev/null | head -1)
echo "cache-dir  = $CDIR"
echo "mount      = up"

MNT="$MNT" CDIR="$CDIR" TREE="$TREE" OUT="$OUT" WORK="$WORK" URL="$URL" \
  timeout "$RTO" "$READER" $RARGS >"$OUT/reader.out" 2>&1
RRC=$?
echo "reader     = $READER $RARGS  (rc=$RRC)"
sed 's/^/  | /' "$OUT/reader.out"

sleep 1
cp "$CDIR/status.json" "$OUT/status.json" 2>/dev/null
cp "$CDIR/conflicts.jsonl" "$OUT/conflicts.jsonl" 2>/dev/null
du -sb "$CDIR" > "$OUT/du-total.txt" 2>/dev/null
echo "mount.log  :"; sed 's/^/  | /' "$OUT/mount.log" | tail -15
echo "run dir : $OUT"
exit $RRC
