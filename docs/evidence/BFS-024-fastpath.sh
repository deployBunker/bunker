#!/usr/bin/env bash
# BFS-024-fastpath.sh — the cost of the fast path, as NUMBERS.
#
# The brief for this row forbids one thing above all others: turning the client's
# stat-free snapshot into a stat-per-file walk. So this probe measures what a
# caller actually pays, in REQUESTS (the mount's own transport counter) and in
# wall time, for the operations the release's whole-tree win rests on:
#
#   ls -l  on a directory of N files        ONE snapshot + in-process readdirplus
#   find -type f | wc -l  (a walk)          the same view, no per-file round trip
#   reading all N files, cold              N reads by construction
#   reading all N files again, warm        0 requests: the cache answers
#   an idle window of one poll interval     1 request per interval, not per path
#
# The last row is the one this row adds to: whatever the invalidation channel
# costs, it must be per INTERVAL, not per file.
#
# usage: BFS-024-fastpath.sh --tree DIR --bin PATH --davserve PATH [--files N]
set -uo pipefail

TREE=""; BIN=""; DS=""; N=200
while [ $# -gt 0 ]; do
  case "$1" in
    --tree) TREE="$2"; shift 2;;
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --files) N="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$TREE" ] && [ -n "$BIN" ] && [ -n "$DS" ] || { echo "missing arg" >&2; exit 2; }

OUT="$(mktemp -d -t bfs024fp-XXXXXX)"
export XDG_CACHE_HOME="$OUT/xdg"
MNT="$OUT/mnt"; mkdir -p "$MNT"
echo "out = $OUT"
echo "tree = $TREE   files = $N"

cleanup() {
  [ -n "${MNT_PID:-}" ] && { kill -TERM "$MNT_PID" 2>/dev/null; sleep 0.4; kill -KILL "$MNT_PID" 2>/dev/null; }
  timeout 20 fusermount3 -u "$MNT" >/dev/null 2>&1 || timeout 20 fusermount -u "$MNT" >/dev/null 2>&1
  [ -n "${SRV_PID:-}" ] && kill -TERM "$SRV_PID" 2>/dev/null
}
trap cleanup EXIT

for i in $(seq 1 "$N"); do printf 'file-%03d-payload-%s\n' "$i" "$(head -c 32 /dev/zero | tr '\0' x)" > "$TREE/f$i.txt"; done
echo "fixture bytes = $(du -sk "$TREE" | awk '{print $1}') KiB"

cat > "$OUT/inv.py" <<'PY'
import json, sys
try:
    inv = json.load(open(sys.argv[1]))["invalidation"]
except Exception as exc:
    print("UNREADABLE %s" % exc); raise SystemExit
print("events=%s dropped=%s seq=%s mechanism=%s available=%s poll_ms=%s resyncs=%s"
      % (inv.get("events_total"), inv.get("paths_dropped_total"), inv.get("seq"),
         inv.get("mechanism"), inv.get("channel_available"), inv.get("poll_interval_ms"),
         inv.get("resyncs_total")))
PY
cat > "$OUT/grp.py" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("UNREADABLE"); raise SystemExit
print(json.dumps(d.get(sys.argv[2], {}), sort_keys=True))
PY

"$DS" --root "$TREE" --addr 127.0.0.1:0 >"$OUT/davserve.out" 2>&1 &
SRV_PID=$!
URL=""
for _ in $(seq 1 80); do URL=$(sed -n 's/^URL=//p' "$OUT/davserve.out" | head -1); [ -n "$URL" ] && break; sleep 0.25; done
[ -n "$URL" ] || { echo "davserve did not start" >&2; exit 1; }
echo "endpoint = $URL"

timeout 900 "$BIN" fs mount "$MNT" --url "$URL" --concurrency 8 --verbose >"$OUT/mount.log" 2>&1 &
MNT_PID=$!
for _ in $(seq 1 240); do grep -q " $MNT " /proc/mounts && break; kill -0 "$MNT_PID" 2>/dev/null || { echo "MOUNT EXITED"; cat "$OUT/mount.log"; exit 1; }; sleep 0.25; done
grep -q " $MNT " /proc/mounts || { echo "MOUNT DID NOT COME UP"; cat "$OUT/mount.log"; exit 1; }
CDIR="$(ls -td "$OUT/xdg/bunker/fs"/* 2>/dev/null | head -1)"
STATUS="$CDIR/status.json"
echo "cache-dir = $CDIR"
sleep 2

reqs() { python3 - "$STATUS" <<'PY'
import json, sys
try:
    print(json.load(open(sys.argv[1]))["transport"]["requests_total"])
except Exception:
    print("?")
PY
}
snapcalls() { python3 - "$STATUS" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("?"); raise SystemExit
t = d.get("transport", {})
print(t.get("snapshot_calls", d.get("snapshot_calls", "n/a")))
PY
}
now_ms() { echo $(( $(date +%s%N) / 1000000 )); }
# cell NAME "command" -> prints requests delta and wall ms
cell() {
  local name="$1"; shift
  local r0 r1 t0 t1 out
  r0="$(reqs)"; t0="$(now_ms)"
  out="$("$@" 2>&1 | tail -1)"
  t1="$(now_ms)"; r1="$(reqs)"
  printf '%-34s requests=%-4s wall_ms=%-6s %s\n' "$name" "$(( r1 - r0 ))" "$(( t1 - t0 ))" "${out:0:60}"
}

echo
echo "=== the fast path, in requests and wall time ($N files) ==="
cell "ls -l (cold, first look)"      ls -l "$MNT"
cell "ls -l (again)"                 ls -l "$MNT"
cell "ls -lR (a walk)"               ls -lR "$MNT"
cell "find -type f | wc -l"          bash -c "find '$MNT' -type f | wc -l"
cell "stat one file"                 stat "$MNT/f1.txt"
cell "read ALL files, cold"          bash -c "for f in '$MNT'/*.txt; do cat \"\$f\" >/dev/null; done"
cell "read ALL files, warm (cache)"  bash -c "for f in '$MNT'/*.txt; do cat \"\$f\" >/dev/null; done"
cell "read ONE file, warm"           cat "$MNT/f1.txt"

echo
echo "=== the invalidation channel's own cost, over an idle window ==="
R0="$(reqs)"; T0="$(now_ms)"
sleep 6.3
R1="$(reqs)"; T1="$(now_ms)"
echo "  idle 6.3 s: requests $R0 -> $R1 (delta $(( R1 - R0 )))  means $(( (R1 - R0) )) request(s) for $(awk 'BEGIN{printf "%.1f", 6300/2000}') declared poll intervals of 2000 ms"
echo "  channel = $(python3 "$OUT/inv.py" "$STATUS")"

echo
echo "=== the same fast path with an out-of-band edit in the tree (one file) ==="
printf 'EDITED-OUT-OF-BAND\n' > "$TREE/f7.txt"
cell "ls -l right after an edit"     ls -l "$MNT"
cell "read the edited file (cold)"   cat "$MNT/f7.txt"
sleep 2.6
cell "read the edited file (again)"  cat "$MNT/f7.txt"
echo "  channel = $(python3 "$OUT/inv.py" "$STATUS")"
echo "  cache   = $(python3 "$OUT/grp.py" "$STATUS" cache)"

echo
echo "transcripts: $OUT"
echo "$OUT" > /tmp/bfs024fp-last
