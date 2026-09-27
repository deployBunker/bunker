#!/usr/bin/env bash
# BFS-016 — CONCURRENCY, measured by an independent server-side witness.
#
# The instrument's own counters are unusable (they read whichever mount last
# wrote status.json). So the witness here is a counting relay in front of the
# endpoint: max concurrent requests actually in flight, at the socket.
#
# Four arms, each with its OWN mount and its OWN cache dir, the relay reset
# before the measured operation, and the mount torn down with a BOUNDED
# fusermount (never pkill -f; PIDs are tracked explicitly):
#
#   1. serial whole-tree walk, no snapshot, concurrency 1
#   2. serial whole-tree walk, no snapshot, concurrency 8
#   3. 8 parallel cold reads, no snapshot, no cache, concurrency 1
#   4. 8 parallel cold reads, no snapshot, no cache, concurrency 8
#   5. serial whole-tree walk WITH the snapshot, concurrency 8
set -uo pipefail
BIN=${BIN:-/tmp/bfs016/bin/bunker}
RELAY=${RELAY:-http://127.0.0.1:38902/dav}
BASE=/tmp/bfs016
OUT=${OUT:-/tmp/bfs016/concurrency.txt}
RING=${RING:-/usr/bin/fusermount}
FAILS=0

say() { printf '%s\n' "$*" | tee -a "$OUT"; }
hr() { say "──────────────────────────────────────────────"; }

stats() { curl -s --max-time 5 "$RELAY_BASE/__stats"; }
relay_base() { printf '%s' "${RELAY%/dav}"; }
RELAY_BASE=$(relay_base)
reset_stats() { curl -s --max-time 5 "$RELAY_BASE/__reset" >/dev/null; }

mount_arm() {
  # mount_arm NAME [extra args...]
  local name="$1"; shift
  local mp="$BASE/mnt-$name" cache="$BASE/cache-$name"
  mkdir -p "$mp"
  "$BIN" fs mount "$mp" --url "$RELAY" --cache-dir "$cache" "$@" >"$BASE/mount-$name.log" 2>&1 &
  MPID=$!
  local i
  for i in $(seq 1 80); do
    grep -q "$mp" /proc/mounts && return 0
    sleep 0.25
  done
  say "MOUNT DID NOT COME UP: $name"; tail -5 "$BASE/mount-$name.log" | sed 's/^/    /'
  return 1
}

unmount_arm() {
  local name="$1"
  local mp="$BASE/mnt-$name"
  timeout 20 "$RING" -u "$mp" >/dev/null 2>&1
  local rc=$?
  for i in $(seq 1 30); do grep -q "$mp" /proc/mounts || break; sleep 0.25; done
  wait "$MPID" 2>/dev/null
  say "   (unmount $name rc=$rc, still-mounted=$(grep -c "$mp" /proc/mounts))"
}

wall() {  # wall LABEL -- argv...
  local label="$1"; shift
  [ "${1:-}" = "--" ] && shift
  local s e
  s=$(date +%s.%N)
  "$@" >/dev/null 2>&1
  e=$(date +%s.%N)
  awk -v a="$s" -v b="$e" -v l="$label" 'BEGIN{printf "%.3f", b-a}'
}

: > "$OUT"
say "=== BFS-016 concurrency measurement $(date -Is) ==="
say "relay      : $RELAY  (delay ${RELAY_DELAY:-20}ms upstream ${UPSTREAM:-http://127.0.0.1:38901})"
say "fixture    : /tmp/bfs016/tree  native entries: $(find /tmp/bfs016/tree -mindepth 1 | wc -l), dirs: $(find /tmp/bfs016/tree -mindepth 1 -type d | wc -l)"

# ── 1/2: the SERIAL whole-tree walk (the shape the battery measures) ──────────
for conc in 1 8; do
  hr
  name="walk-c$conc"
  say "ARM 1/2 — serial whole-tree walk, --no-snapshot, concurrency $conc"
  mount_arm "$name" --no-snapshot --concurrency "$conc" || { FAILS=$((FAILS+1)); continue; }
  reset_stats
  mnt="$BASE/mnt-$name"
  t=$(wall "find" -- find "$mnt" -mindepth 1 )
  n=$(find "$mnt" -mindepth 1 2>/dev/null | wc -l)
  s=$(stats)
  say "   find (whole tree)  wall=${t}s  entries=$n  native=421"
  say "   relay witness      $s"
  say "   client status      $(grep -o '"in_flight_max": *[0-9]*' "$BASE/cache-$name/status.json" 2>/dev/null | head -1) $(grep -o '"requests_total": *[0-9]*' "$BASE/cache-$name/status.json" 2>/dev/null | head -1)"
  unmount_arm "$name"
done

# ── 3/4: 8 PARALLEL cold reads (the workload concurrency is designed for) ─────
for conc in 1 8; do
  hr
  name="par-c$conc"
  say "ARM 3/4 — 8 parallel COLD reads (--no-snapshot --no-cache), concurrency $conc"
  mount_arm "$name" --no-snapshot --no-cache --concurrency "$conc" || { FAILS=$((FAILS+1)); continue; }
  reset_stats
  mnt="$BASE/mnt-$name"
  files=""
  for i in 001 002 003 004 005 006 007 008; do files="$files $mnt/src/f$i.txt"; done
  pids=""
  s=$(date +%s.%N)
  for f in $files; do cat "$f" >/dev/null 2>&1 & pids="$pids $!"; done
  for p in $pids; do wait "$p"; done      # ONLY the cats — never the mount daemon
  e=$(date +%s.%N)
  t=$(awk -v a="$s" -v b="$e" 'BEGIN{printf "%.3f", b-a}')
  st=$(stats)
  say "   8 x cat 15 KiB      wall=${t}s"
  say "   relay witness      $st"
  say "   client status      $(grep -o '"in_flight_max": *[0-9]*' "$BASE/cache-$name/status.json" 2>/dev/null | head -1) $(grep -o '"requests_total": *[0-9]*' "$BASE/cache-$name/status.json" 2>/dev/null | head -1)"
  unmount_arm "$name"
done

# ── 5: the same walk WITH the one-call snapshot ──────────────────────────────
hr
name="walk-snap"
say "ARM 5 — serial whole-tree walk WITH the snapshot op, concurrency 8"
mount_arm "$name" --concurrency 8 || FAILS=$((FAILS+1))
reset_stats
mnt="$BASE/mnt-$name"
t=$(wall "find" -- find "$mnt" -mindepth 1 )
n=$(find "$mnt" -mindepth 1 2>/dev/null | wc -l)
say "   find (whole tree)  wall=${t}s  entries=$n"
say "   relay witness      $(stats)"
say "   client status      $(grep -o '"in_flight_max": *[0-9]*' "$BASE/cache-$name/status.json" 2>/dev/null | head -1) $(grep -o '"requests_total": *[0-9]*' "$BASE/cache-$name/status.json" 2>/dev/null | head -1)"
say "   snapshot block     $(grep -o '"source": *"[^"]*"' "$BASE/cache-$name/status.json" 2>/dev/null | head -1) $(grep -o '"nodes": *[0-9]*' "$BASE/cache-$name/status.json" 2>/dev/null | head -1) $(grep -o '"calls": *[0-9]*' "$BASE/cache-$name/status.json" 2>/dev/null | head -1)"
unmount_arm "$name"

hr
say "arms with infrastructure failure: $FAILS"
say "done"
