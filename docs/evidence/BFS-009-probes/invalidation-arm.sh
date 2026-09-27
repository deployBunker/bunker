#!/usr/bin/env bash
# bfs009-invalidation-arm.sh — DELIVERABLE 2 of BFS-009: does an edit made on
# the server become visible through the mount, and WHICH mechanism delivered it?
#
# It measures, in order:
#   A. the channel's DECLARED state on a real mount (mode / mechanism /
#      channel_available / reason), read from status.json the mount keeps;
#   B. whether the channel is even TRYING: requests_total growth over a quiet
#      window with no filesystem I/O of ours;
#   C. a never-read path edited on the server, then read through the mount;
#   D. a path ALREADY READ (therefore cached) edited on the server, then read
#      again after > the declared poll interval;
#   E. `stat` through the mount on a snapshotted path, before/after the edit.
#
# usage: bfs009-invalidation-arm.sh --label L --tree DIR --bin PATH --davserve PATH \
#          [--flag "--invalidation poll"] [--edit-wait S] [--quiet-window S]
#
# NEVER uses pkill -f; explicit PIDs only; every mount step is `timeout`-bounded.
set -uo pipefail

LABEL=""; TREE=""; BIN=""; DS=""; EXTRA=""; EDIT_WAIT=6; QUIET=8
while [ $# -gt 0 ]; do
  case "$1" in
    --label) LABEL="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --flag) EXTRA="$2"; shift 2;;
    --edit-wait) EDIT_WAIT="$2"; shift 2;;
    --quiet-window) QUIET="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$LABEL" ] && [ -n "$TREE" ] && [ -n "$BIN" ] && [ -n "$DS" ] || { echo "missing arg" >&2; exit 2; }

OUT="/tmp/bfs009/run-$LABEL"
[ -e "$OUT" ] && { echo "refusing to reuse $OUT" >&2; exit 2; }
mkdir -p "$OUT"
export XDG_CACHE_HOME="$OUT/xdg"
MNT="$OUT/mnt"; mkdir -p "$MNT"

cleanup() {
  if [ -n "${MNT_PID:-}" ]; then kill -TERM "$MNT_PID" 2>/dev/null; sleep 0.3; kill -KILL "$MNT_PID" 2>/dev/null; fi
  timeout 20 fusermount -u "$MNT" >/dev/null 2>&1
  if [ -n "${SRV_PID:-}" ]; then kill -TERM "$SRV_PID" 2>/dev/null; fi
}
trap cleanup EXIT

"$DS" --root "$TREE" --addr 127.0.0.1:0 >"$OUT/davserve.out" 2>&1 &
SRV_PID=$!
URL=""
for _ in $(seq 1 80); do
  URL=$(sed -n 's/^URL=//p' "$OUT/davserve.out" | head -1)
  [ -n "$URL" ] && break
  sleep 0.25
done
[ -n "$URL" ] || { echo "davserve did not start"; exit 1; }
echo "endpoint  = $URL"

# shellcheck disable=SC2086
timeout 90 "$BIN" fs mount "$MNT" --url "$URL" --concurrency 8 --verbose $EXTRA >"$OUT/mount.log" 2>&1 &
MNT_PID=$!
for _ in $(seq 1 160); do
  grep -q " $MNT " /proc/mounts && break
  kill -0 "$MNT_PID" 2>/dev/null || { echo "MOUNT EXITED"; cat "$OUT/mount.log"; exit 1; }
  sleep 0.25
done
grep -q " $MNT " /proc/mounts || { echo "MOUNT DID NOT COME UP"; cat "$OUT/mount.log"; exit 1; }
CDIR=$(ls -td "$OUT/xdg/bunker/fs"/* 2>/dev/null | head -1)
STATUS="$CDIR/status.json"
echo "cache-dir = $CDIR"

st() { python3 - "$STATUS" "$1" <<'PY'
import json,sys
try:
    d=json.load(open(sys.argv[1]))
except Exception as e:
    print("UNREADABLE: %s" % e); raise SystemExit
print(json.dumps(d.get(sys.argv[2], {}), sort_keys=True))
PY
}
reqs() { sed -n 's/.*"requests_total": *\([0-9]*\).*/\1/p' "$STATUS" | head -1; }
sha() { sha256sum "$1" | awk '{print $1}'; }

echo
echo "=== A. the channel's declared state (as the mount reports it) ==="
sleep 3
echo "status.invalidation = $(st invalidation)"
echo "status.mode         = $(sed -n 's/.*"mode": *"\([a-z]*\)".*/\1/p' "$STATUS" | head -1)"
echo "mount.log degradation lines:"
grep -iE 'degradation|invalidation channel|bound to' "$OUT/mount.log" | sed 's/^/  /' || true

echo
echo "=== B. is the channel trying at all? (quiet window, no I/O of ours) ==="
R0=$(reqs); T0=$(date +%s)
sleep "$QUIET"
R1=$(reqs); T1=$(date +%s)
echo "requests_total $R0 -> $R1 over $((T1-T0))s  (delta $((R1-R0)) = $(( (R1-R0) / (T1-T0) )) req/s)"
echo "status.invalidation after the window = $(st invalidation)"

echo
echo "=== C. never-read path edited on the SERVER, then read through the mount ==="
NEVER="$TREE/never.txt"
printf 'version-1 %s\n' "$(date +%s)" > "$NEVER"
echo "server bytes  : $(cat "$NEVER")   sha256=$(sha "$NEVER")"
sleep 0.3
T=$(( $(date +%s%N) / 1000000 ))
GOT=$(timeout 30 cat "$MNT/never.txt")
T2=$(( $(date +%s%N) / 1000000 ))
echo "mount   bytes : $GOT   (read took $((T2-T)) ms)"
echo "MATCH         : $([ "$GOT" = "$(cat "$NEVER")" ] && echo YES || echo NO)"

echo
echo "=== D. path ALREADY READ (cached) edited on the SERVER, re-read after the poll interval ==="
CACHED="$TREE/cached.txt"
printf 'cached-version-1\n' > "$CACHED"
C1=$(timeout 30 cat "$MNT/cached.txt")
echo "mount first read  : $C1"
echo "status.cache after the first read = $(st cache)"
printf 'cached-version-2-EDITED-ON-THE-SERVER\n' > "$CACHED"
echo "server now        : $(cat "$CACHED")   sha256=$(sha "$CACHED")"
echo "waiting $EDIT_WAIT s (declared poll interval is 2 s)…"
sleep "$EDIT_WAIT"
C2=$(timeout 30 cat "$MNT/cached.txt")
echo "mount second read : $C2"
if [ "$C2" = "$(cat "$CACHED")" ]; then
  echo "VISIBLE           : YES"
else
  echo "VISIBLE           : NO — the mount served a STALE copy"
fi
echo "status.invalidation after the edit = $(st invalidation)"
echo "status.cache        after the edit = $(st cache)"

echo
echo "=== E. stat through the mount on a snapshotted path ==="
SNAP="$TREE/snap.txt"
printf 'snap-version-1\n' > "$SNAP"
timeout 30 cat "$MNT/snap.txt" >/dev/null
echo "before edit: size=$(timeout 30 stat -c %s "$MNT/snap.txt") mtime=$(timeout 30 stat -c %Y "$MNT/snap.txt")"
printf 'snap-version-2-much-longer-content-here\n' > "$SNAP"
echo "server now : size=$(stat -c %s "$SNAP") mtime=$(stat -c %Y "$SNAP")"
sleep "$EDIT_WAIT"
echo "after edit : size=$(timeout 30 stat -c %s "$MNT/snap.txt") mtime=$(timeout 30 stat -c %Y "$MNT/snap.txt")"

echo
echo "=== F. final channel state ==="
echo "status.invalidation = $(st invalidation)"
echo "status.transport    = $(st transport)"
cp "$STATUS" "$OUT/status-final.json"
echo "mount.log (last 15 lines):"; tail -15 "$OUT/mount.log" | sed 's/^/  /'
