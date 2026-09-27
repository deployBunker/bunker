#!/usr/bin/env bash
# mount-arm.sh — one measured mount arm: an endpoint, a bounded mount, an in-flight
# sampler, a reader, and the accounting of what actually landed on disk.
#
# BFS-012 deliverable 1 needs THREE instruments to agree, so this script runs all
# three and leaves their raw output in the run directory:
#   * `du -sb` of the cache directory (the filesystem), sampled WHILE the run is in
#     flight, not only after it;
#   * the client's own status.json (`cache.used_bytes`), sampled at the same
#     instants (sample.py), plus `bunker fs status` — the owner-facing surface;
#   * the counters: evictions_total, bypass_events, oversize_bypasses, pinned_blobs
#     — the difference between "the bound held" and "the bound held because
#     nothing was ever cached".
#
# usage: mount-arm.sh --label L --tree DIR --bin PATH --davserve PATH \
#          [--flag "…"] [--reader PATH] [--reader-args "…"] [--work /tmp/bfs012] \
#          [--timeout-s 3600] [--no-sample]
#
# NEVER `pkill -f`: the pattern matches the shell running it (four workers in this
# fleet have hung on that). Every process here is killed by explicit PID and every
# fusermount is bounded with `timeout`.
set -uo pipefail

LABEL=""; TREE=""; BIN=""; DS=""; EXTRA=""; READER=""; RARGS=""; WORK=/tmp/bfs012
RTO=3600; SAMPLE=1; CONC=25; MTMO=1800; TRACE=0; TRACE_PORT=18471
while [ $# -gt 0 ]; do
  case "$1" in
    --label) LABEL="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --flag) EXTRA="$2"; shift 2;;
    --reader) READER="$2"; shift 2;;
    --reader-args) RARGS="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --timeout-s) RTO="$2"; shift 2;;
    --concurrency) CONC="$2"; shift 2;;
    --mount-timeout-s) MTMO="$2"; shift 2;;
    --trace) TRACE=1; shift;;
    --trace-port) TRACE_PORT="$2"; shift 2;;
    --no-sample) SAMPLE=0; shift;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$LABEL" ] && [ -n "$TREE" ] && [ -n "$BIN" ] && [ -n "$DS" ] || { echo "missing arg" >&2; exit 2; }

OUT="$WORK/run-$LABEL"
[ -e "$OUT" ] && { echo "refusing to reuse $OUT (a fresh directory per arm, so nothing is inherited)" >&2; exit 2; }
mkdir -p "$OUT"
export XDG_CACHE_HOME="$OUT/xdg"
MNT="$OUT/mnt"; mkdir -p "$MNT"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

cleanup() {
  if [ -n "${MNT_PID:-}" ]; then kill -TERM "$MNT_PID" 2>/dev/null; sleep 0.3; kill -KILL "$MNT_PID" 2>/dev/null; fi
  timeout 20 fusermount -u "$MNT" >/dev/null 2>&1
  if [ -n "${SRV_PID:-}" ]; then kill -TERM "$SRV_PID" 2>/dev/null; fi
  if [ -n "${SMP_PID:-}" ]; then kill -TERM "$SMP_PID" 2>/dev/null; fi
  if [ -n "${RD_PID:-}" ]; then kill -TERM "$RD_PID" 2>/dev/null; fi
  if [ -n "${PROXY_PID:-}" ]; then kill -TERM "$PROXY_PID" 2>/dev/null; fi
}
trap cleanup EXIT

echo "arm        = $LABEL"
echo "host       = $(uname -n) loadavg $(cat /proc/loadavg)"
echo "tree       = $TREE  ($(find "$TREE" -type f | wc -l) files, $(find "$TREE" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}') B)"
echo "binaries   = $BIN ; $DS"

# 1. the endpoint: the repo's own client-facing surface (internal/server/webdav),
#    loopback, plain HTTP/1.1, no TLS and no h2c.
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

# 1b. OPTIONAL request trace: the same surface, behind a logging proxy. It adds no
#     behaviour; it makes the exact request sequence visible, which is the only way
#     to tell "the client retried" from "one request both refused and wrote".
MOUNT_URL="$URL"
if [ "$TRACE" = 1 ]; then
  python3 "$HERE/countproxy.py" --upstream "$URL" --listen-port "$TRACE_PORT" --log "$OUT/requests.jsonl" >"$OUT/proxy.log" 2>&1 &
  PROXY_PID=$!
  for _ in $(seq 1 60); do
    grep -q "listening on" "$OUT/proxy.log" 2>/dev/null && break
    sleep 0.2
  done
  grep -q "listening on" "$OUT/proxy.log" || { echo "counting-proxy did not start"; cat "$OUT/proxy.log"; exit 1; }
  MOUNT_URL="http://127.0.0.1:$TRACE_PORT/dav"
  echo "trace      = $MOUNT_URL -> $URL (pid $PROXY_PID, per-request log $OUT/requests.jsonl)"
fi

# 2. the mount, bounded by `timeout` so a wedged FUSE thread cannot hold the arm.
#    The bound must exceed the arm it wraps, or the mount dies mid-measurement and
#    the arm silently measures a shorter run (BFS-009's 90 s bound would have).
echo "mount-args = --url $MOUNT_URL --concurrency $CONC $EXTRA"
echo "mount-timeout = ${MTMO}s"
# shellcheck disable=SC2086
timeout "$MTMO" "$BIN" fs mount "$MNT" --url "$MOUNT_URL" --concurrency "$CONC" $EXTRA >"$OUT/mount.log" 2>&1 &
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

# 3. the in-flight sampler.
if [ "$SAMPLE" = 1 ]; then
  python3 "$HERE/sample.py" "$CDIR" "$OUT/sample.csv" 0.5 &
  SMP_PID=$!
fi

# 4. the reader — any program; it gets the mount and the cache dir in the env.
MNT="$MNT" CDIR="$CDIR" TREE="$TREE" OUT="$OUT" WORK="$WORK" URL="$URL" \
  timeout "$RTO" ${READER:-bash} $RARGS >"$OUT/reader.out" 2>&1 &
RD_PID=$!
wait "$RD_PID"; RRC=$?
echo "reader     = ${READER:-bash} $RARGS  (rc=$RRC)"
sed 's/^/  | /' "$OUT/reader.out" | head -60

# 5. settle, stop sampling, take the final state with all three instruments.
sleep 2.5
[ -n "${SMP_PID:-}" ] && { kill -TERM "$SMP_PID" 2>/dev/null; wait "$SMP_PID" 2>/dev/null; }
cp "$CDIR/status.json" "$OUT/status.json" 2>/dev/null
cp "$CDIR/index.json" "$OUT/index.json" 2>/dev/null
cp "$CDIR/conflicts.jsonl" "$OUT/conflicts.jsonl" 2>/dev/null
du -sb "$CDIR" > "$OUT/du-total.txt" 2>/dev/null
du -sb "$CDIR/blobs" > "$OUT/du-blobs.txt" 2>/dev/null
find "$CDIR/blobs" -type f -printf '%s\n' 2>/dev/null | awk '{s+=$1} END{print s+0}' > "$OUT/real-blobs-bytes.txt"
find "$CDIR/blobs" -type f 2>/dev/null | wc -l > "$OUT/blob-count.txt"
find "$CDIR" -maxdepth 1 -type f -printf '%s\n' 2>/dev/null | awk '{s+=$1} END{print s+0}' > "$OUT/other-files-bytes.txt"
ls -la "$CDIR" > "$OUT/cachedir-ls.txt" 2>/dev/null
echo "mount.log  :"; sed 's/^/  | /' "$OUT/mount.log" | tail -25
echo "owner-facing surface (bunker fs status / conflicts, XDG_CACHE_HOME=$XDG_CACHE_HOME):"
"$BIN" fs status 2>&1 | sed 's/^/  | /' | head -40
echo "--- conflicts ---"
"$BIN" fs conflicts --limit 5 2>&1 | sed 's/^/  | /' | head -30

echo
python3 "$HERE/accounting.py" "$OUT"
if [ "$TRACE" = 1 ] && [ -s "$OUT/requests.jsonl" ]; then
  echo
  echo "=== REQUEST TRACE (every HTTP request the mount made, via the logging proxy) ==="
  python3 "$HERE/trace_summary.py" "$OUT/requests.jsonl"
fi
exit $RRC
