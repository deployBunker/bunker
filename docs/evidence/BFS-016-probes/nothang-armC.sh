#!/usr/bin/env bash
# BFS-016 — CRITERION 8, the arm that matters: MOUNTED and healthy, then the
# server FREEZES mid-flight (SIGSTOP: it accepts, it answers nothing).
#
# The ops must report a BOUNDED failure with a named cause, and the mount must
# stay mounted (not a phantom, not silently unmounted). No cache and no snapshot
# on this mount, so no local answer can hide a hang. Every op is wrapped in
# `timeout`; rc=124 is a STALL and is reported as one.
set -uo pipefail
BIN=${BIN:-/tmp/bfs016/bin/bunker}
BASE=/tmp/bfs016
RELAY=http://127.0.0.1:38902/dav
RELAY_PID=${RELAY_PID:?relay pid required}
OP_TIMEOUT=${OP_TIMEOUT:-60}
RING=${RING:-/usr/bin/fusermount}
MP="$BASE/mnt-frozen2"
CACHE="$BASE/cache-frozen2"
: > "$BASE/nothang-armC.txt"
say() { printf '%s\n' "$*" | tee -a "$BASE/nothang-armC.txt"; }

op() {
  local label="$1"; shift
  [ "${1:-}" = "--" ] && shift
  local s e rc out
  s=$(date +%s.%N); out=$(timeout "$OP_TIMEOUT" "$@" 2>&1); rc=$?; e=$(date +%s.%N)
  local cls="ok"; [ "$rc" = "124" ] && cls="STALL(>${OP_TIMEOUT}s)"
  [ "$rc" != "0" ] && [ "$rc" != "124" ] && cls="error(rc=$rc)"
  printf '%-30s %8ss  rc=%-4s %-16s %s\n' "$label" \
    "$(awk -v a="$s" -v b="$e" 'BEGIN{printf "%.3f", b-a}')" "$rc" "$cls" \
    "$(printf '%s' "$out" | head -c 190 | tr '\n' ' ')" | tee -a "$BASE/nothang-armC.txt"
}

say "=== BFS-016 arm C: server freezes mid-flight $(date -Is) ==="
say "relay: $RELAY (pid $RELAY_PID)   mount: $MP (--no-snapshot --no-cache)"
curl -s --max-time 5 http://127.0.0.1:38902/__stats | sed 's/^/relay up: /' | tee -a "$BASE/nothang-armC.txt"

mkdir -p "$MP"
"$BIN" fs mount "$MP" --url "$RELAY" --no-snapshot --no-cache --concurrency 8 >"$BASE/mount-frozen2.log" 2>&1 &
MPID=$!
for i in $(seq 1 80); do grep -q "$MP" /proc/mounts && break; sleep 0.25; done
if ! grep -q "$MP" /proc/mounts; then
  say "MOUNT FAILED against a healthy relay:"; tail -3 "$BASE/mount-frozen2.log" | tee -a "$BASE/nothang-armC.txt"; exit 1
fi
say "mount is up. control ops (server answering):"
op "control: cat go.mod"   -- cat "$MP/go.mod"
op "control: stat go.mod"  -- stat -c '%s' "$MP/go.mod"

say ""
say "now SIGSTOP the relay (pid $RELAY_PID): the server accepts but answers nothing"
kill -STOP "$RELAY_PID" 2>/dev/null
sleep 0.5
op "stat (frozen)"         -- stat -c '%s' "$MP/go.mod"
op "cat  (frozen)"         -- cat "$MP/go.mod"
op "ls   (frozen)"         -- ls "$MP"
op "write (frozen)"        -- sh -c "printf 'x\n' > '$MP/frozen.txt'"

say ""
say "mount still mounted (not a phantom, not silently unmounted): $(grep -c "$MP" /proc/mounts)"
say "proc entry: $(grep "$MP" /proc/mounts)"
say "client transport block:"
grep -o '"\(verdict\|cause\|last_ok_age_ms\|requests_total\)": *[^,}]*' "$CACHE/status.json" 2>/dev/null | sed 's/^/   /' | tee -a "$BASE/nothang-armC.txt"

say ""
say "now SIGCONT (recovery):"
kill -CONT "$RELAY_PID" 2>/dev/null
sleep 1
op "RECOVERY: cat go.mod"  -- cat "$MP/go.mod"

say ""
timeout 20 "$RING" -u "$MP" >/dev/null 2>&1
say "unmount rc=$? still-mounted=$(grep -c "$MP" /proc/mounts)"
wait "$MPID" 2>/dev/null
say "done"
