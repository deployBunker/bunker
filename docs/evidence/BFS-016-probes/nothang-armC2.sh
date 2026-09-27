#!/usr/bin/env bash
# BFS-016 — arm C2, two questions the first stall arm left open:
#   1. is a file NEVER touched in this mount served locally while the server is
#      frozen, or does the local success only apply to data already read?
#   2. after one op has burned the full 30 s OpTimeout, does the NEXT op fail fast
#      (a circuit breaker) or pay another 30 s?
set -uo pipefail
BIN=${BIN:-/tmp/bfs016/bin/bunker}
BASE=/tmp/bfs016
RELAY=http://127.0.0.1:38902/dav
RELAY_PID=${RELAY_PID:?relay pid required}
MP="$BASE/mnt-frozen3"; CACHE="$BASE/cache-frozen3"
RING=${RING:-/usr/bin/fusermount}
: > "$BASE/nothang-armC2.txt"
say() { printf '%s\n' "$*" | tee -a "$BASE/nothang-armC2.txt"; }
op() {
  local label="$1"; shift; [ "${1:-}" = "--" ] && shift
  local s e rc out
  s=$(date +%s.%N); out=$(timeout 60 "$@" 2>&1); rc=$?; e=$(date +%s.%N)
  local cls="ok"; [ "$rc" = "124" ] && cls="STALL(>60s)"
  [ "$rc" != "0" ] && [ "$rc" != "124" ] && cls="error(rc=$rc)"
  printf '%-32s %8ss  rc=%-4s %-14s %s\n' "$label" \
    "$(awk -v a="$s" -v b="$e" 'BEGIN{printf "%.3f", b-a}')" "$rc" "$cls" \
    "$(printf '%s' "$out" | head -c 170 | tr '\n' ' ')" | tee -a "$BASE/nothang-armC2.txt"
}
say "=== BFS-016 arm C2 — cold reads and repeat failures against a frozen server ==="
say "relay pid $RELAY_PID; file under test is src/f042.txt, NEVER touched by this mount"
mkdir -p "$MP"
"$BIN" fs mount "$MP" --url "$RELAY" --no-snapshot --no-cache --concurrency 8 >"$BASE/mount-frozen3.log" 2>&1 &
MPID=$!
for i in $(seq 1 80); do grep -q "$MP" /proc/mounts && break; sleep 0.25; done
grep -q "$MP" /proc/mounts || { say "MOUNT FAILED"; tail -3 "$BASE/mount-frozen3.log" | tee -a "$BASE/nothang-armC2.txt"; exit 1; }
say "mounted (no snapshot, no cache). freeze the server:"
kill -STOP "$RELAY_PID" 2>/dev/null; sleep 0.5
op "stat src/f042.txt (NEVER touched)" -- stat -c '%s' "$MP/src/f042.txt"
op "cat  src/f042.txt (NEVER touched)" -- cat "$MP/src/f042.txt"
op "ls   src (2nd op after a timeout)" -- ls "$MP/src"
op "stat . (3rd op after timeouts)"    -- stat -c '%n' "$MP"
say ""
say "client transport block:"
find "$CACHE" -name 'status.json' 2>/dev/null | head -3 | tee -a "$BASE/nothang-armC2.txt"
cat "$CACHE/status.json" 2>/dev/null | grep -A 8 '"transport"' | sed 's/^/   /' | tee -a "$BASE/nothang-armC2.txt"
say ""
kill -CONT "$RELAY_PID" 2>/dev/null; sleep 1
op "RECOVERY: cat src/f042.txt" -- cat "$MP/src/f042.txt"
timeout 20 "$RING" -u "$MP" >/dev/null 2>&1
say "unmount rc=$? still-mounted=$(grep -c "$MP" /proc/mounts)"
wait "$MPID" 2>/dev/null
say done
