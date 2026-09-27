#!/usr/bin/env bash
# channel-arm.sh — BFS-026's live arm: does an edit made ON THE AGENT reach the
# mount, WHICH mechanism delivered it, and what does the record say?
#
# It is run twice with the same arguments: once with binaries built from the
# tree as it was filed (the RED) and once with the tree this row changes (the
# GREEN). Nothing here is time-sensitive except the declared poll interval, and
# nothing is inferred: every field printed is read from the mount's own
# status.json, and the op catalogue is probed live rather than quoted.
#
# Steps
#   A. the declared channel state, from the mount's status document
#   B. the E-4 catalogue, probed live (POST + X-Bunker-Op per name)
#   C. is the fallback even RUNNING? requests_total over a quiet window
#   D. a path NEVER READ, edited on the agent, then read through the mount
#   E. a path ALREADY READ (cached), edited on the agent: how long until the
#      mount serves the new bytes, measured, against the declared interval
#   F. `bunker fs op events` verbatim — the op a caller can drive by hand
#   G. the final record, and the verdict line this row turns on
#
# usage: channel-arm.sh --label L --tree DIR --bin PATH --davserve PATH [--hold S]
# NEVER uses pkill -f; explicit PIDs only; every fusermount is timeout-bounded.
set -uo pipefail

LABEL=""; TREE=""; BIN=""; DS=""; HOLD=15
while [ $# -gt 0 ]; do
  case "$1" in
    --label) LABEL="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --hold) HOLD="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$LABEL" ] && [ -n "$TREE" ] && [ -n "$BIN" ] && [ -n "$DS" ] || { echo "missing arg" >&2; exit 2; }

OUT="$(mktemp -d -t "bfs026-$LABEL.XXXXXX")"
export XDG_CACHE_HOME="$OUT/xdg"
MNT="$OUT/mnt"; mkdir -p "$MNT"
echo "label     = $LABEL"
echo "out       = $OUT"
echo "tree      = $TREE"
echo "binaries  = $BIN / $DS"

cleanup() {
  if [ -n "${MNT_PID:-}" ]; then kill -TERM "$MNT_PID" 2>/dev/null; sleep 0.5; kill -KILL "$MNT_PID" 2>/dev/null; fi
  timeout 20 fusermount3 -u "$MNT" >/dev/null 2>&1 || timeout 20 fusermount -u "$MNT" >/dev/null 2>&1
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
[ -n "$URL" ] || { echo "davserve did not start" >&2; cat "$OUT/davserve.out" >&2; exit 1; }
echo "endpoint  = $URL"

timeout 120 "$BIN" fs mount "$MNT" --url "$URL" --concurrency 8 --verbose >"$OUT/mount.log" 2>&1 &
MNT_PID=$!
for _ in $(seq 1 200); do
  grep -q " $MNT " /proc/mounts && break
  kill -0 "$MNT_PID" 2>/dev/null || { echo "MOUNT EXITED"; cat "$OUT/mount.log"; exit 1; }
  sleep 0.25
done
grep -q " $MNT " /proc/mounts || { echo "MOUNT DID NOT COME UP"; cat "$OUT/mount.log"; exit 1; }
CDIR=$(ls -td "$OUT/xdg/bunker/fs"/* 2>/dev/null | head -1)
STATUS="$CDIR/status.json"
echo "cache-dir = $CDIR"

python3 - "$STATUS" invalidation <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception as exc:
    print("UNREADABLE: %s" % exc); raise SystemExit
print("status.%s = %s" % (sys.argv[2], json.dumps(d.get(sys.argv[2], {}), sort_keys=True)))
PY
reqs() { sed -n 's/.*"requests_total": *\([0-9]*\).*/\1/p' "$STATUS" | head -1; }
field() { python3 - "$STATUS" "$1" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("?"); raise SystemExit
print(d.get("invalidation", {}).get(sys.argv[2], "?"))
PY
}
record() { python3 - "$STATUS" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
inv = d.get("invalidation", {})
print(json.dumps(inv, sort_keys=True))
PY
}
sha() { sha256sum "$1" | awk '{print $1}'; }
now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }

echo
echo "=== A. the declared channel state ==="
sleep 3
echo "status.invalidation = $(record)"
cp "$STATUS" "$OUT/status-A.json"
grep -iE 'degradation|bound to' "$OUT/mount.log" | sed 's/^/  mount.log: /' || true

echo
echo "=== B. the E-4 op catalogue, probed live ==="
for op in capabilities status diff rev-parse ls-files log snapshot events watch frobnicate; do
  body='{}'
  [ "$op" = "capabilities" ] && body=''
  out=$(curl -sS -o "$OUT/op-$op.json" -w '%{http_code}' -X POST "$URL" \
        -H "X-Bunker-Op: $op" -H 'Content-Type: application/json' \
        --data "$body" 2>"$OUT/op-$op.err")
  verdict=$(sed -n 's/.*"verdict":"\([^"]*\)".*/\1/p' "$OUT/op-$op.json" | head -1)
  cap=$(sed -n 's/.*"capability":"\([^"]*\)".*/\1/p' "$OUT/op-$op.json" | head -1)
  printf '  %-12s %s  verdict=%-24s capability=%s\n' "$op" "$out" "${verdict:-?}" "${cap:--}"
done

echo
echo "=== C. is the declared fallback RUNNING? (quiet window, no I/O of ours) ==="
R0=$(reqs); T0=$(date +%s)
sleep 6
R1=$(reqs); T1=$(date +%s)
echo "requests_total $R0 -> $R1 over $((T1-T0))s  ($((R1-R0)) requests)"
echo "status.invalidation after the window = $(record)"

echo
echo "=== D. a path NEVER READ (BFS-025's shape), edited on the AGENT ==="
echo "    Three facts, separated: the refusal, whether the READ PATH itself repairs"
echo "    it, and when the mount's RECORD reports the change."
python3 "$(dirname "$0")/visibility.py" --status "$STATUS" --tree "$TREE" --mount "$MNT" \
  --path never.txt --body 'never-version-2-EDITED-ON-THE-AGENT' --hold 12 >"$OUT/vis-D.txt" 2>&1
sed 's/^/  /' "$OUT/vis-D.txt"
echo "  (the mount's own log lines for that read, if any:)"
grep -iE "read bound divergence|longer than the size the kernel holds" "$OUT/mount.log" | tail -4 | sed 's/^/    /' || true

echo
echo "=== E. a path ALREADY READ (cached): the BFS-024 shape, with its control ==="
NEVER2="$TREE/cached.txt"
printf 'cached-version-1\n' > "$NEVER2"
C1=$(timeout 30 cat "$MNT/cached.txt")
echo "  mount first read : $C1   (so the path IS cached)"
python3 "$(dirname "$0")/visibility.py" --status "$STATUS" --tree "$TREE" --mount "$MNT" \
  --path cached.txt --body 'cached-version-2-EDITED-ON-THE-AGENT' --hold 12 >"$OUT/vis-E.txt" 2>&1
sed 's/^/  /' "$OUT/vis-E.txt"

echo
echo "=== F. the delegated verb, by hand ==="
seq_now=$(field seq)
(timeout 30 "$BIN" fs op events --url "$URL" --arg "since_seq=${seq_now:-0}" 2>&1 | head -8) | sed 's/^/  /'

echo
echo "=== G. the final record and the verdict this row turns on ==="
echo "status.invalidation = $(record)"
echo "status.transport    : $(python3 - "$STATUS" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
print(json.dumps(d.get("transport", {}), sort_keys=True))
PY
)"
echo "status.cache        : $(python3 - "$STATUS" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
print(json.dumps(d.get("cache", {}), sort_keys=True))
PY
)"
echo
echo "VERDICT-LINE label=$LABEL mode=$(field mode) mechanism=$(field mechanism) channel_available=$(field channel_available) events_total=$(field events_total) paths_dropped_total=$(field paths_dropped_total) resyncs_total=$(field resyncs_total) last_event_age_ms=$(field last_event_age_ms)"
echo "VERDICT-LINE never-read:  $(grep '^SUMMARY' "$OUT/vis-D.txt" | head -1)"
echo "VERDICT-LINE cached-path: $(grep '^SUMMARY' "$OUT/vis-E.txt" | head -1)"
echo "VERDICT-LINE reason=$(field reason)"
cp "$STATUS" "$OUT/status-final.json"
echo "transcripts: $OUT"
echo "$OUT" > "/tmp/bfs026-last-$LABEL"
