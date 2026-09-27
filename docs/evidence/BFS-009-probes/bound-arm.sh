#!/usr/bin/env bash
# bfs009-bound-arm.sh — DELIVERABLE 1 of BFS-009: read a source tree LARGER than
# the client's cache bound through a real FUSE mount, sampling the cache
# directory's REAL on-disk size during the run, and compare it with the figure
# the client itself reports (status.json: cache.used_bytes vs cache.max_bytes).
#
# usage: bfs009-bound-arm.sh --label L --tree DIR --bin PATH --davserve PATH \
#          --flag "--cache-max-size 8388608" --read-set bulk|all
#
# NEVER uses pkill -f: every process is killed by explicit PID. Every
# fusermount/mount step is bounded with `timeout`. Nothing is removed
# recursively; each run gets its own fresh directory.
set -uo pipefail

LABEL=""; TREE=""; BIN=""; DS=""; EXTRA=""; READSET="all"
while [ $# -gt 0 ]; do
  case "$1" in
    --label) LABEL="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --flag) EXTRA="$2"; shift 2;;
    --read-set) READSET="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$LABEL" ] && [ -n "$TREE" ] && [ -n "$BIN" ] && [ -n "$DS" ] || { echo "missing arg" >&2; exit 2; }

OUT="/tmp/bfs009/run-$LABEL"
[ -e "$OUT" ] && { echo "refusing to reuse $OUT (fresh directory per run)" >&2; exit 2; }
mkdir -p "$OUT"
export XDG_CACHE_HOME="$OUT/xdg"
MNT="$OUT/mnt"; mkdir -p "$MNT"
# The sampler lives next to this script, so a copy of the pair works from anywhere.
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SAMPLER="$HERE/sample.sh"
[ -x "$SAMPLER" ] || chmod +x "$SAMPLER" 2>/dev/null || true

cleanup() {
  if [ -n "${MNT_PID:-}" ]; then kill -TERM "$MNT_PID" 2>/dev/null; sleep 0.3; kill -KILL "$MNT_PID" 2>/dev/null; fi
  timeout 20 fusermount -u "$MNT" >/dev/null 2>&1
  if [ -n "${SRV_PID:-}" ]; then kill -TERM "$SRV_PID" 2>/dev/null; fi
  if [ -n "${SMP_PID:-}" ]; then kill -TERM "$SMP_PID" 2>/dev/null; fi
}
trap cleanup EXIT

# 1. endpoint: the repo's own client-facing WebDAV surface, loopback HTTP/1.1.
"$DS" --root "$TREE" --addr 127.0.0.1:0 >"$OUT/davserve.out" 2>&1 &
SRV_PID=$!
URL=""
for _ in $(seq 1 80); do
  URL=$(sed -n 's/^URL=//p' "$OUT/davserve.out" | head -1)
  [ -n "$URL" ] && break
  sleep 0.25
done
[ -n "$URL" ] || { echo "davserve did not start"; cat "$OUT/davserve.out"; exit 1; }
echo "endpoint  = $URL (pid $SRV_PID, HTTP/1.1, loopback)"

# 2. mount, bounded.
echo "mount-args= --url $URL --concurrency 25 $EXTRA"
# shellcheck disable=SC2086
timeout 90 "$BIN" fs mount "$MNT" --url "$URL" --concurrency 25 $EXTRA >"$OUT/mount.log" 2>&1 &
MNT_PID=$!
for _ in $(seq 1 160); do
  grep -q " $MNT " /proc/mounts && break
  kill -0 "$MNT_PID" 2>/dev/null || { echo "MOUNT EXITED"; cat "$OUT/mount.log"; exit 1; }
  sleep 0.25
done
grep -q " $MNT " /proc/mounts || { echo "MOUNT DID NOT COME UP"; cat "$OUT/mount.log"; exit 1; }
CDIR=$(ls -td "$OUT/xdg/bunker/fs"/* 2>/dev/null | head -1)
echo "cache-dir = $CDIR"
grep -E '^  (cache dir|concurrency|invalidation|snapshot)' "$OUT/mount.log" || true

# 3. sample the real on-disk size WHILE the run is in flight.
bash "$SAMPLER" "$CDIR" "$OUT/sample.csv" 0.5 &
SMP_PID=$!

# 4. read the whole tree through the mount.
case "$READSET" in
  bulk) FIND_ROOT="$MNT/src/bulk" ;;
  *)    FIND_ROOT="$MNT" ;;
esac
mapfile -t SET < <(find "$FIND_ROOT" -type f | sort)
echo "reading ${#SET[@]} files through the mount…"
START=$(( $(date +%s%N) / 1000000 ))
OK=0; FAILS=0
for f in "${SET[@]}"; do
  if timeout 45 cat "$f" >/dev/null 2>&1; then OK=$((OK+1)); else FAILS=$((FAILS+1)); fi
done
END=$(( $(date +%s%N) / 1000000 ))

# byte totals both sides: the tree on disk (truth) and the mount's own view.
SRC_BYTES=$(find "$TREE" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}')
MNT_BYTES=$(find "$FIND_ROOT" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}')
echo "read ok=$OK failures=$FAILS  wall_ms=$((END-START))"
echo "tree_bytes_on_disk=$SRC_BYTES  mount_bytes_seen=$MNT_BYTES"

# 5. let the status document catch up, take the final figures, then stop.
sleep 2.5
kill -TERM "$SMP_PID" 2>/dev/null; wait "$SMP_PID" 2>/dev/null
cp "$CDIR/status.json" "$OUT/status.json" 2>/dev/null
cp "$CDIR/index.json" "$OUT/index.json" 2>/dev/null
du -sb "$CDIR" > "$OUT/du-total.txt" 2>/dev/null
du -sb "$CDIR/blobs" > "$OUT/du-blobs.txt" 2>/dev/null
du -sb "$CDIR/index.json" > "$OUT/du-index.txt" 2>/dev/null
find "$CDIR/blobs" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}' > "$OUT/real-blobs-bytes.txt"
find "$CDIR/blobs" -type f | wc -l > "$OUT/blob-count.txt"
ls -la "$CDIR" > "$OUT/cachedir-ls.txt" 2>/dev/null
MAXDU=$(awk -F, 'NR>1 && $2>m {m=$2} END{print m+0}' "$OUT/sample.csv")
MAXDU_BLOBS=$(awk -F, 'NR>1 && $3>m {m=$3} END{print m+0}' "$OUT/sample.csv")

echo "=== FINAL (after the run) ==="
echo "du -sb cache-dir   : $(cat "$OUT/du-total.txt")"
echo "du -sb blobs       : $(cat "$OUT/du-blobs.txt")"
echo "du -sb index.json  : $(cat "$OUT/du-index.txt")"
echo "real blobs bytes   : $(cat "$OUT/real-blobs-bytes.txt")"
echo "blob files         : $(cat "$OUT/blob-count.txt")"
echo "MAX du cache-dir  during run : $MAXDU"
echo "MAX du blobs      during run : $MAXDU_BLOBS"
python3 - "$OUT/status.json" <<'PY'
import json,sys
st=json.load(open(sys.argv[1]))
c=st["cache"]
print("reported max_bytes     = %d" % c["max_bytes"])
print("reported used_bytes    = %d" % c["used_bytes"])
print("reported blobs/index   = %d / %d" % (c["blobs_bytes"], c["index_bytes"]))
print("reported entries/blobs = %d / %d" % (c["entries"], c["blobs"]))
print("reported evictions     = %d  bypass=%d oversize=%d pins=%d hits=%d misses=%d"
      % (c["evictions_total"], c["bypass_events"], c["oversize_bypasses"], c["pinned_blobs"], c["hits"], c["misses"]))
print("invalidation=%s" % json.dumps(st["invalidation"]))
print("snapshot=%s" % json.dumps(st["snapshot"]))
print("status_updated_ms=%d" % st["updated_ms"])
PY
