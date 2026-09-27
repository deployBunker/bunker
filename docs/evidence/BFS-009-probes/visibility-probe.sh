#!/usr/bin/env bash
# bfs009-visibility-probe.sh — three measurements BFS-009 needs that the mount
# battery does not take. Read-only on the served tree except for the edits it
# makes deliberately and names.
#
#   P1. REV MOTION — does X-Bunker-Rev move for an edit made on the agent (out of
#       band), for a mutation made THROUGH the surface, and for a git commit?
#       This decides whether the client's third mechanism (`rev`) could ever
#       deliver an agent-side edit.
#   P2. THE TWO STALENESS SHAPES, separated because they are different failures:
#       (a) a path NEVER read through the mount, replaced with DIFFERENT-length
#           content on the agent, then read — no local copy exists to hide it,
#           so what the mount returns is the whole answer;
#       (b) a path ALREADY READ (so cached), replaced with different-length
#           content on the agent, then read again.
#       Each arm carries a NATIVE control read of the same file at the same
#       instant.
#   P3. THE UN-CACHEABLE READ — a same-size, mtime-restored agent-side edit makes
#       the surface report an identity hash that is not the hash of the bytes it
#       serves. The client then refuses to store those bytes under that hash.
#       Is that refusal VISIBLE in the status document, as BFS-005 §3.4 requires
#       of every bypass?
#
# usage: bfs009-visibility-probe.sh --bin PATH --davserve PATH --tree DIR --label L
set -uo pipefail

BIN=""; DS=""; TREE=""; LABEL=""
while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    --label) LABEL="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$BIN" ] && [ -n "$DS" ] && [ -n "$TREE" ] && [ -n "$LABEL" ] || { echo "missing arg" >&2; exit 2; }

OUT="/tmp/bfs009/run-vis-$LABEL"
[ -e "$OUT" ] && { echo "refusing to reuse $OUT" >&2; exit 2; }
mkdir -p "$OUT"
export XDG_CACHE_HOME="$OUT/xdg"
MNT="$OUT/mnt"; mkdir -p "$MNT"

printf 'arm-a-original-content\n' > "$TREE/arm-a.txt"   # never read through the mount
printf 'arm-b-original-content\n' > "$TREE/arm-b.txt"   # read + cached before the edit
printf 'arm-c-identity\n'         > "$TREE/arm-c.txt"   # P3, never read through the mount
printf 'rev-probe.txt\n'          > "$TREE/rev-probe.txt"
echo "tree: $TREE   git=$( [ -e "$TREE/.git" ] && echo yes || echo no )"

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
echo "endpoint: $URL"
rev() { curl -sS -X OPTIONS -D - -o /dev/null "$URL/" | sed -n 's/^[Xx]-[Bb]unker-[Rr]ev: *//p' | tr -d '\r'; }

echo
echo "=== P1. REV MOTION ($TREE) ==="
R0=$(rev); echo "initial                          rev=$R0"
printf 'out-of-band edit\n' > "$TREE/rev-probe.txt"
R1=$(rev); echo "after an OUT-OF-BAND edit        rev=$R1  $([ "$R1" = "$R0" ] && echo 'NO MOTION' || echo 'moved')"
printf 'through the surface\n' > "$OUT/put.bin"
curl -sS -o /dev/null -X PUT --data-binary @"$OUT/put.bin" "$URL/put-through-surface.txt"
R2=$(rev); echo "after a mutation THROUGH PUT     rev=$R2  $([ "$R2" != "$R1" ] && echo 'moved' || echo 'NO MOTION')"
if [ -e "$TREE/.git" ]; then
  ( cd "$TREE" && git add -A && git -c user.email=b@b -c user.name=b commit -qm "rev probe" ) >/dev/null 2>&1
  R3=$(rev); echo "after a git COMMIT               rev=$R3  $([ "$R3" != "$R2" ] && echo 'moved' || echo 'NO MOTION')"
fi

echo
echo "=== THE MOUNT ==="
timeout 90 "$BIN" fs mount "$MNT" --url "$URL" --concurrency 4 > "$OUT/mount.log" 2>&1 &
MNT_PID=$!
for _ in $(seq 1 160); do grep -q " $MNT " /proc/mounts && break; sleep 0.25; done
grep -q " $MNT " /proc/mounts || { echo "MOUNT DID NOT COME UP"; cat "$OUT/mount.log"; exit 1; }
CDIR=$(ls -td "$OUT/xdg/bunker/fs"/* 2>/dev/null | head -1)
STATUS="$CDIR/status.json"
cstats() { python3 -c 'import json,sys; c=json.load(open(sys.argv[1]))["cache"]; print("entries=%-3d blobs=%-3d used=%-7d blobs_bytes=%-7d index_bytes=%-6d evictions=%-3d bypass=%-3d oversize=%-3d hits=%-3d misses=%-3d"%(c["entries"],c["blobs"],c["used_bytes"],c["blobs_bytes"],c["index_bytes"],c["evictions_total"],c["bypass_events"],c["oversize_bypasses"],c["hits"],c["misses"]))' "$STATUS"; }
echo "cache-dir: $CDIR"

# --- P2b: a path already read (cached) -------------------------------------
echo
echo "-- P2b. a path ALREADY READ through the mount, replaced on the agent --"
B=$(timeout 30 cat "$MNT/arm-b.txt"); echo "mount (pre-edit) : '$B'"
sleep 1.2
echo "status.cache     : $(cstats)"
printf 'arm-b-REPLACED-with-different-length-content\n' > "$TREE/arm-b.txt"
echo "agent now        : '$(cat "$TREE/arm-b.txt")'  size=$(stat -c %s "$TREE/arm-b.txt")"
sleep 3
GOT=$(timeout 30 cat "$MNT/arm-b.txt")
echo "mount (post-edit): '$GOT'  (${#GOT} chars)"
echo "NATIVE control   : '$(cat "$TREE/arm-b.txt")'  ($(stat -c %s "$TREE/arm-b.txt") bytes)"
[ "$GOT" = "$(cat "$TREE/arm-b.txt")" ] && echo "VERDICT: fresh" || echo "VERDICT: STALE — the mount served the pre-edit bytes"
echo "status.cache     : $(cstats)"

# --- P2a: a path never read through the mount ------------------------------
echo
echo "-- P2a. a path NEVER READ through the mount, replaced on the agent --"
echo "mount stat (pre) : size=$(timeout 30 stat -c %s "$MNT/arm-a.txt")  (stat only, no read, so nothing is cached)"
printf 'arm-a-REPLACED-with-a-much-longer-content-string-0123456789\n' > "$TREE/arm-a.txt"
echo "agent now        : '$(cat "$TREE/arm-a.txt")'  size=$(stat -c %s "$TREE/arm-a.txt")"
sleep 3
echo "mount stat (post): size=$(timeout 30 stat -c %s "$MNT/arm-a.txt")"
GOT2=$(timeout 30 cat "$MNT/arm-a.txt")
echo "mount read       : '$GOT2'  (${#GOT2} chars)"
echo "NATIVE control   : '$(cat "$TREE/arm-a.txt")'  ($(stat -c %s "$TREE/arm-a.txt") bytes)"
[ "$GOT2" = "$(cat "$TREE/arm-a.txt")" ] && echo "VERDICT: fresh" || echo "VERDICT: WRONG — the mount returned ${#GOT2} chars of a $(stat -c %s "$TREE/arm-a.txt")-byte file"
echo "status.cache     : $(cstats)"

# --- P3: the un-cacheable read --------------------------------------------
echo
echo "-- P3. the un-cacheable read (same-size, mtime-restored agent edit) --"
# Warm the SURFACE's identity cache with curl, NOT through the mount, so the
# mount has never seen this path and cannot answer from a local copy.
curl -sS -o /dev/null "$URL/arm-c.txt"
cp -p "$TREE/arm-c.txt" "$OUT/arm-c.orig"
printf 'ARM-C-IDENTITY\n' > "$TREE/arm-c.txt"   # same byte length as the original
touch -r "$OUT/arm-c.orig" "$TREE/arm-c.txt"
stat -c 'agent: size=%s mtime=%Y' "$TREE/arm-c.txt"
echo "surface X-Bunker-Hash : $(curl -sS -I "$URL/arm-c.txt" | sed -n 's/^[Xx]-[Bb]unker-[Hh]ash: *//p' | tr -d '\r')"
echo "GET body sha256       : $(curl -sS "$URL/arm-c.txt" | sha256sum | awk '{print $1}')"
echo "status.cache before   : $(cstats)"
GOT3=$(timeout 30 cat "$MNT/arm-c.txt")
echo "mount served          : '$GOT3'  (${#GOT3} chars; the agent's bytes are $(stat -c %s "$TREE/arm-c.txt"))"
sleep 2
echo "status.cache after    : $(cstats)"
echo "(§3.4 requires every read the client did NOT cache to be REPORTED; compare the two status lines)"
cp "$STATUS" "$OUT/status-final.json"
echo
echo "channel: $(python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1]))["invalidation"],sort_keys=True))' "$STATUS")"
