#!/usr/bin/env bash
# bfs009-uncacheable-probe.sh — P3, made rigorous: the ground truth is the cache
# DIRECTORY (blob files + index.json), not the status document, so a stale or
# lagging status sample cannot decide the result.
#
# The question: when the client fetches bytes whose hash is not the hash the
# surface advertised for them, Cache.Insert refuses (internal/fsclient/cache.go,
# the "refusing to store" branch — the client must never lie about its own
# content address). §3.4 requires EVERY read the client did not cache to be
# REPORTED, so that "the bound is respected by caching nothing" is visible.
# Is this refusal reported?
#
# A CONTROL ARM proves the instrument can see an insert at all: a plain
# never-read file, read the same way, must appear in the cache directory and in
# the counters. Without the control, "nothing moved" proves nothing.
#
# usage: bfs009-uncacheable-probe.sh --bin PATH --davserve PATH --work DIR
set -uo pipefail
BIN=""; DS=""; WORK=""; LABEL="${LABEL:-1}"
while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --label) LABEL="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
OUT="$WORK/run-uncacheable-$LABEL"
[ -e "$OUT" ] && { echo "refusing to reuse $OUT" >&2; exit 2; }
mkdir -p "$OUT"
TREE="$WORK/uctree-$LABEL"; mkdir -p "$TREE"
export XDG_CACHE_HOME="$OUT/xdg"
MNT="$OUT/mnt"; mkdir -p "$MNT"

printf 'stale-identity-target\n' > "$TREE/target.txt"   # the mangled one
printf 'plain-control\n'        > "$TREE/control.txt"  # the control

cleanup() {
  if [ -n "${MNT_PID:-}" ]; then kill -TERM "$MNT_PID" 2>/dev/null; sleep 0.3; kill -KILL "$MNT_PID" 2>/dev/null; fi
  timeout 20 fusermount -u "$MNT" >/dev/null 2>&1
  if [ -n "${SRV_PID:-}" ]; then kill -TERM "$SRV_PID" 2>/dev/null; fi
}
trap cleanup EXIT
"$DS" --root "$TREE" --addr 127.0.0.1:0 > "$OUT/davserve.out" 2>&1 &
SRV_PID=$!
URL=""
for _ in $(seq 1 80); do URL=$(sed -n 's/^URL=//p' "$OUT/davserve.out" | head -1); [ -n "$URL" ] && break; sleep 0.25; done
[ -n "$URL" ] || { echo "davserve did not start"; exit 1; }
timeout 90 "$BIN" fs mount "$MNT" --url "$URL" --concurrency 4 > "$OUT/mount.log" 2>&1 &
MNT_PID=$!
for _ in $(seq 1 160); do grep -q " $MNT " /proc/mounts && break; sleep 0.25; done
grep -q " $MNT " /proc/mounts || { echo "MOUNT DID NOT COME UP"; cat "$OUT/mount.log"; exit 1; }
CDIR=$(ls -td "$OUT/xdg/bunker/fs"/* 2>/dev/null | head -1)
STATUS="$CDIR/status.json"
echo "endpoint: $URL"; echo "cache-dir: $CDIR"

ground() {
  local blobs idx
  blobs=$(find "$CDIR/blobs" -type f 2>/dev/null | wc -l)
  idx=$(python3 - "$CDIR/index.json" "$1" <<'PY'
import json,sys
try:
    d=json.load(open(sys.argv[1]))
except Exception:
    print("index-unreadable"); raise SystemExit
paths=[e["path"] for e in d.get("entries",[])]
print("present" if sys.argv[2] in paths else "absent")
PY
)
  echo "blobs_on_disk=$blobs  $1_in_index=$idx"
}
counters() { python3 -c 'import json,sys; c=json.load(open(sys.argv[1]))["cache"]; print("entries=%-3d blobs=%-3d used=%-6d blobs_bytes=%-6d evictions=%-3d bypass=%-3d oversize=%-3d hits=%-3d misses=%-3d"%(c["entries"],c["blobs"],c["used_bytes"],c["blobs_bytes"],c["evictions_total"],c["bypass_events"],c["oversize_bypasses"],c["hits"],c["misses"]))' "$STATUS"; }

echo
echo "=== ARM 1 (control): a plain never-read file, read through the mount ==="
echo "before : $(ground control.txt)"; echo "         $(counters)"
timeout 30 cat "$MNT/control.txt" >/dev/null
sleep 2
echo "after  : $(ground control.txt)"; echo "         $(counters)"

echo
echo "=== ARM 2: the un-cacheable read ==="
# Warm the SURFACE's identity cache with curl, NOT through the mount, so the
# mount has never seen this path (a local copy cannot hide the answer).
curl -sS -o /dev/null "$URL/target.txt"
cp -p "$TREE/target.txt" "$OUT/target.orig"
printf 'STALE-IDENTITY-TARGET\n' > "$TREE/target.txt"     # same byte length
touch -r "$OUT/target.orig" "$TREE/target.txt"
echo "agent bytes   : '$(cat "$TREE/target.txt")'  size=$(stat -c %s "$TREE/target.txt")  mtime=$(stat -c %Y "$TREE/target.txt") (restored)"
echo "surface says  : $(curl -sS -I "$URL/target.txt" | sed -n 's/^[Xx]-[Bb]unker-[Hh]ash: *//p' | tr -d '\r')"
echo "body hashes to: sha256:$(curl -sS "$URL/target.txt" | sha256sum | awk '{print $1}')"
echo "before : $(ground target.txt)"; echo "         $(counters)"
GOT=$(timeout 30 cat "$MNT/target.txt")
echo "mount served  : '$GOT'  (${#GOT} chars — the NEW bytes, so the read was a real cache MISS)"
sleep 3
echo "after  : $(ground target.txt)"; echo "         $(counters)"
echo
echo "READ THIS AS: the control arm moved both the blob count and the index; arm 2"
echo "moved neither and no counter moved, so the refusal is not reported anywhere."
cp "$STATUS" "$OUT/status-final.json"
