#!/usr/bin/env bash
# BFS-016 — CRITERION 8, the NOT-HANG proof. Bounded errors against a dead, a
# refused and a STALLED endpoint, with the ops that MUST round-trip (no snapshot,
# no cache) so no local answer can hide a hang.
#
# Every op is wrapped in `timeout N`; rc=124 IS the stall symptom and is reported
# as a stall, not smoothed over. No pkill -f: PIDs are explicit, and the only
# signal sent to a foreign process is SIGSTOP/SIGCONT to simulate a stall.
set -uo pipefail
BIN=${BIN:-/tmp/bfs016/bin/bunker}
BASE=/tmp/bfs016
OUT=${OUT:-/tmp/bfs016/nothang.txt}
LIVE_DOC=${LIVE_DOC:-http://127.0.0.1:38901/dav}      # the davserve being used
RELAY=${RELAY:-http://127.0.0.1:38902/dav}            # the counting relay (stalled in arm C)
RELAY_PID=${RELAY_PID:-0}
DEAD=http://127.0.0.1:38999/dav                       # nothing listens here
OP_TIMEOUT=${OP_TIMEOUT:-60}
RING=${RING:-/usr/bin/fusermount}
: > "$OUT"
say() { printf '%s\n' "$*" | tee -a "$OUT"; }
hr() { say "──────────────────────────────────────────────"; }
t0() { date +%s.%N; }
dt() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.3f", b-a}'; }

# op LABEL -- argv...   (bounded; rc=124 is a STALL and is named as one)
op() {
  local label="$1"; shift
  [ "${1:-}" = "--" ] && shift
  local s e rc out
  s=$(t0); out=$(timeout "$OP_TIMEOUT" "$@" 2>&1); rc=$?; e=$(t0)
  local cls="ok"
  [ "$rc" = "124" ] && cls="STALL(>${OP_TIMEOUT}s)"
  [ "$rc" != "0" ] && [ "$rc" != "124" ] && cls="error(rc=$rc)"
  printf '%-34s %8ss  rc=%-4s %s  %s\n' "$label" "$(dt "$s" "$e")" "$rc" "$cls" \
    "$(printf '%s' "$out" | head -c 160 | tr '\n' ' ')" | tee -a "$OUT"
}

say "=== BFS-016 not-hang proof $(date -Is) ==="
say "op deadline per call: ${OP_TIMEOUT}s   (the client declares BindTimeout=5s, OpTimeout=30s)"

hr
say "ARM A — NOTHING LISTENING (connection refused), no mount involved"
op "fs probe (dead endpoint)"  -- "$BIN" fs probe --url "$DEAD"
op "fs snapshot (dead)"        -- "$BIN" fs snapshot --url "$DEAD"
op "fs mount (dead endpoint)"  -- "$BIN" fs mount "$BASE/mnt-dead" --url "$DEAD"
say "   mountpoint left behind? $(ls -A "$BASE/mnt-dead" 2>/dev/null | wc -l) entries, mounted=$(grep -c "$BASE/mnt-dead" /proc/mounts)"

hr
say "ARM B — STALLED LISTENER (accepts TCP, never answers): the relay is SIGSTOPped"
if [ "$RELAY_PID" != "0" ]; then
  kill -STOP "$RELAY_PID" 2>/dev/null && say "   SIGSTOP sent to relay pid $RELAY_PID"
  sleep 0.5
  op "fs probe (stalled endpoint)" -- "$BIN" fs probe --url "$RELAY"
  op "fs mount (stalled endpoint)" -- "$BIN" fs mount "$BASE/mnt-stall" --url "$RELAY"
  say "   mountpoint left behind? $(ls -A "$BASE/mnt-stall" 2>/dev/null | wc -l) entries"
else
  say "   SKIPPED: no --relay-pid given"
fi

hr
say "ARM C — MOUNTED, THEN THE SERVER FREEZES (the real stall: it stops answering mid-flight)"
mkdir -p "$BASE/mnt-frozen"
"$BIN" fs mount "$BASE/mnt-frozen" --url "$RELAY" --no-snapshot --no-cache --concurrency 8 \
  >"$BASE/mount-frozen.log" 2>&1 &
FPID=$!
for i in $(seq 1 80); do grep -q "$BASE/mnt-frozen" /proc/mounts && break; sleep 0.25; done
if ! grep -q "$BASE/mnt-frozen" /proc/mounts; then
  say "   the mount did NOT come up against the stalled relay — that is itself the bounded answer:"
  tail -3 "$BASE/mount-frozen.log" | sed 's/^/     /'
else
  say "   mount is up; a healthy op first (the control):"
  op "control: cat go.mod (server up)" -- cat "$BASE/mnt-frozen/go.mod"
  [ "$RELAY_PID" != "0" ] && { kill -STOP "$RELAY_PID" 2>/dev/null; say "   relay pid $RELAY_PID SIGSTOPped (server answers nothing now)"; }
  sleep 0.5
  op "stat (stalled, no cache)"  -- stat -c '%n' "$BASE/mnt-frozen/go.mod"
  op "cat  (stalled, no cache)"  -- cat "$BASE/mnt-frozen/go.mod"
  op "ls   (stalled, no cache)"  -- ls "$BASE/mnt-frozen"
  op "write (stalled)"           -- sh -c "printf 'x\n' > '$BASE/mnt-frozen/frozen.txt'"
  say "   still mounted (not a phantom, not silently unmounted): $(grep -c "$BASE/mnt-frozen" /proc/mounts)"
  say "   transport verdict:"
  "$BIN" fs status --json --mount "$(basename "$(readlink -f "$BASE/cache-frozen" 2>/dev/null || echo x)")" 2>/dev/null | grep -A 6 '"transport"' | sed 's/^/     /' || true
  grep -o '"\(verdict\|cause\|last_ok_age_ms\|proto\)": *[^,}]*' "$BASE/cache-frozen/status.json" 2>/dev/null | sed 's/^/     /'
  if [ "$RELAY_PID" != "0" ]; then
    kill -CONT "$RELAY_PID" 2>/dev/null; say "   relay pid $RELAY_PID resumed (SIGCONT)"
    sleep 0.5
    op "RECOVERY: cat go.mod after resume" -- cat "$BASE/mnt-frozen/go.mod"
  fi
  timeout 20 "$RING" -u "$BASE/mnt-frozen" >/dev/null 2>&1
  say "   unmount rc=$? still-mounted=$(grep -c "$BASE/mnt-frozen" /proc/mounts)"
  wait "$FPID" 2>/dev/null
fi
[ "$RELAY_PID" != "0" ] && kill -CONT "$RELAY_PID" 2>/dev/null
hr
say "done"
