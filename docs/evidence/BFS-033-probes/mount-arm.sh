#!/usr/bin/env bash
# mount-arm.sh — one measured mount arm for BFS-030 (the write-side twin of BFS-025).
#
# Starts the repo's own client-facing surface (probes/davserve, the same handler
# bunkerd serves), mounts it with the bunker-fs client, runs a reader, and leaves
# the raw output plus the client's own records in the run directory.
#
# Conventions copied from BFS-012's mount-arm.sh, for the same reasons:
#   * a FRESH run directory per arm, so nothing is inherited;
#   * every process killed by explicit PID, never `pkill -f` (the pattern matches
#     the shell running it);
#   * every fusermount bounded with `timeout`.
set -uo pipefail

LABEL=""; TREE=""; BIN=""; DS=""; READER=""; RARGS=""; WORK=/tmp/bfs030
RTO=600; MTMO=900; EXTRA=""; TRACE=0; TRACE_PORT=18490
while [ $# -gt 0 ]; do
  case "$1" in
    --label) LABEL="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --reader) READER="$2"; shift 2;;
    --reader-args) RARGS="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --timeout-s) RTO="$2"; shift 2;;
    --mount-timeout-s) MTMO="$2"; shift 2;;
    --flag) EXTRA="$2"; shift 2;;
    --trace) TRACE=1; shift;;
    --trace-port) TRACE_PORT="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$LABEL" ] && [ -n "$TREE" ] && [ -n "$BIN" ] && [ -n "$DS" ] && [ -n "$READER" ] || { echo "missing arg" >&2; exit 2; }

OUT="$WORK/run-$LABEL"
[ -e "$OUT" ] && { echo "refusing to reuse $OUT (a fresh directory per arm)" >&2; exit 2; }
mkdir -p "$OUT"
export XDG_CACHE_HOME="$OUT/xdg"
MNT="$OUT/mnt"; mkdir -p "$MNT"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

cleanup() {
  if [ -n "${MNT_PID:-}" ]; then kill -TERM "$MNT_PID" 2>/dev/null; sleep 0.3; kill -KILL "$MNT_PID" 2>/dev/null; fi
  timeout 20 fusermount -u "$MNT" >/dev/null 2>&1
  if [ -n "${SRV_PID:-}" ]; then kill -TERM "$SRV_PID" 2>/dev/null; fi
  if [ -n "${PROXY_PID:-}" ]; then kill -TERM "$PROXY_PID" 2>/dev/null; fi
}
trap cleanup EXIT

echo "arm        = $LABEL"
echo "host       = $(uname -n) loadavg $(cat /proc/loadavg)"
echo "tree       = $TREE  ($(find "$TREE" -type f | wc -l) files, $(find "$TREE" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}') B)"
echo "binaries   = $BIN ; $DS"

"$DS" --root "$TREE" --addr 127.0.0.1:0 >"$OUT/davserve.out" 2>&1 &
SRV_PID=$!
export SRV_PID
URL=""
for _ in $(seq 1 80); do
  URL=$(sed -n 's/^URL=//p' "$OUT/davserve.out" | head -1)
  [ -n "$URL" ] && break
  sleep 0.25
done
[ -n "$URL" ] || { echo "davserve did not start"; cat "$OUT/davserve.out"; exit 1; }
echo "endpoint   = $URL (pid $SRV_PID)"

MOUNT_URL="$URL"
if [ "$TRACE" = 1 ]; then
  ORIGIN=$(printf '%s' "$URL" | sed -E 's#(https?://[^/]+).*#\1#')
  python3 "$HERE/countproxy.py" --upstream "$ORIGIN" --listen-port "$TRACE_PORT" --log "$OUT/requests.jsonl" >"$OUT/proxy.log" 2>&1 &
  PROXY_PID=$!
  for _ in $(seq 1 60); do
    grep -q "listening on" "$OUT/proxy.log" 2>/dev/null && break
    sleep 0.2
  done
  grep -q "listening on" "$OUT/proxy.log" || { echo "counting-proxy did not start"; cat "$OUT/proxy.log"; exit 1; }
  MOUNT_URL="http://127.0.0.1:$TRACE_PORT/dav"
  echo "trace      = $MOUNT_URL -> $URL (pid $PROXY_PID, per-request log $OUT/requests.jsonl)"
fi

echo "mount-args = --url $MOUNT_URL $EXTRA"
timeout "$MTMO" "$BIN" fs mount "$MNT" --url "$MOUNT_URL" $EXTRA >"$OUT/mount.log" 2>&1 &
MNT_PID=$!
for _ in $(seq 1 200); do
  grep -q " $MNT " /proc/mounts && break
  kill -0 "$MNT_PID" 2>/dev/null || { echo "MOUNT EXITED"; cat "$OUT/mount.log"; exit 1; }
  sleep 0.25
done
grep -q " $MNT " /proc/mounts || { echo "MOUNT DID NOT COME UP"; cat "$OUT/mount.log"; exit 1; }
CDIR=$(ls -td "$OUT/xdg/bunker/fs"/* 2>/dev/null | head -1)
echo "cache-dir  = $CDIR"
grep -E '^  (cache dir|concurrency|invalidation|snapshot|endpoint)' "$OUT/mount.log" || true

MNT="$MNT" CDIR="$CDIR" TREE="$TREE" OUT="$OUT" WORK="$WORK" URL="$URL" \
  timeout "$RTO" "$READER" $RARGS >"$OUT/reader.out" 2>&1
RRC=$?
echo "reader     = $READER $RARGS  (rc=$RRC)"
sed 's/^/  | /' "$OUT/reader.out"

sleep 1
cp "$CDIR/status.json" "$OUT/status.json" 2>/dev/null
cp "$CDIR/conflicts.jsonl" "$OUT/conflicts.jsonl" 2>/dev/null
du -sb "$CDIR" > "$OUT/du-total.txt" 2>/dev/null
echo "mount.log  :"; sed 's/^/  | /' "$OUT/mount.log" | tail -25
echo "owner-facing surface (bunker fs status, XDG_CACHE_HOME=$XDG_CACHE_HOME):"
"$BIN" fs status 2>&1 | sed 's/^/  | /' | head -40
echo "--- conflicts ---"
"$BIN" fs conflicts --limit 5 2>&1 | sed 's/^/  | /' | head -30
echo "run dir : $OUT"
exit $RRC
