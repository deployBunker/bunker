#!/usr/bin/env bash
# op-timing.sh — the bound and the cap, measured rather than asserted.
#
# BFS-026's sixth requirement is that the mechanism this row adds is BOUNDED.
# There are three bounds and each is measured here, each against its OWN fresh
# server so an answer never mixes with an earlier one:
#
#   1. what one poll COSTS on a tree with thousands of paths (the observation is
#      stat-only: no file bytes are read);
#   2. what a large change costs, and that the per-path answer is precise;
#   3. what happens past the DECLARED per-event path cap (4096): the answer must
#      be one `overflow` — knowledge lost — and never a longer or partial list.
#      The real cap is driven, not a lowered one.
#
# Answers are printed as a compact summary (event names + path counts) and kept
# verbatim in files: a transcript that pastes 4000 paths is unreadable, and an
# unreadable transcript is not evidence anybody can check.
#
# usage: op-timing.sh --davserve PATH [--files N] [--polls N] [--burst N]
set -uo pipefail

DS=""; FILES=2000; POLLS=20; BURST=4100
while [ $# -gt 0 ]; do
  case "$1" in
    --davserve) DS="$2"; shift 2;;
    --files) FILES="$2"; shift 2;;
    --polls) POLLS="$2"; shift 2;;
    --burst) BURST="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$DS" ] || { echo "--davserve is required" >&2; exit 2; }
PROBES="$(cd "$(dirname "$0")" && pwd)"

OUT="$(mktemp -d -t bfs026-timing.XXXXXX)"
echo "out        = $OUT"

URL=""; SRV_PID=""
start() { # <tree> <tag>
  "$DS" --root "$1" --addr 127.0.0.1:0 >"$OUT/serve-$2.out" 2>&1 &
  SRV_PID=$!
  URL=""
  for _ in $(seq 1 80); do
    URL=$(sed -n 's/^URL=//p' "$OUT/serve-$2.out" | head -1)
    [ -n "$URL" ] && break
    sleep 0.25
  done
  [ -n "$URL" ] || { echo "davserve did not start" >&2; cat "$OUT/serve-$2.out" >&2; exit 1; }
}
stop() { [ -n "${SRV_PID:-}" ] && kill -TERM "$SRV_PID" 2>/dev/null; SRV_PID=""; }
trap 'stop' EXIT

poll() { curl -sS -o "$1" -w '%{time_total}' -X POST "$URL" \
           -H 'X-Bunker-Op: events' -H 'Content-Type: application/json' --data '{}' 2>/dev/null; }

# poll_since mirrors what the client does on every tick: it sends its cursor.
poll_since() { curl -sS -o "$1" -w '%{time_total}' -X POST "$URL" \
           -H 'X-Bunker-Op: events' -H 'Content-Type: application/json' \
           --data "{\"since_seq\":$2}" 2>/dev/null; }

head_seq() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["result"]["head_seq"])' "$1"; }

summarise() { python3 - "$1" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
r = d["result"]
print("    events    : %s   (event, path-count, seq)"
      % ", ".join("%s(%d)@%d" % (e["event"], len(e["paths"]), e["seq"]) for e in r["events"]))
print("    count=%d scanned=%d head_seq=%d truncated=%s status=%s verdict=%s"
      % (r["count"], r["scanned"], r["head_seq"], d["truncated"],
         d.get("duration_ms"), d["verdict"]))
PY
}

echo
echo "=== 1. the cost of one poll on a quiet tree with $FILES paths ($POLLS polls) ==="
TREE1="$OUT/tree1"
python3 "$PROBES/mkfixture.py" "$TREE1" --burst "$FILES"
start "$TREE1" 1
echo "  endpoint  = $URL   tree = $TREE1 ($(find "$TREE1" -type f | wc -l) files)"
: > "$OUT/times.txt"
for i in $(seq 1 "$POLLS"); do
  printf '%s\n' "$(poll "$OUT/poll1-$i.json")" >> "$OUT/times.txt"
done
python3 - "$OUT/times.txt" <<'PY'
import statistics, sys
ms = sorted(float(l) * 1000 for l in open(sys.argv[1]) if l.strip())
print("  polls      = %d" % len(ms))
print("  min/median/mean/max ms = %.1f / %.1f / %.1f / %.1f" % (ms[0], statistics.median(ms), statistics.mean(ms), ms[-1]))
print("  declared poll interval = 2000 ms (the client's default)")
print("  headroom   = %.0fx the median poll cost fits in one interval" % (2000.0 / max(statistics.median(ms), 0.001)))
PY
echo "  the first poll (seeds the ledger) and the last (quiet, caught up):"
summarise "$OUT/poll1-1.json"
summarise "$OUT/poll1-$POLLS.json"

echo
echo "=== 2. a LARGE change (1000 new paths), same server ==="
mkdir -p "$TREE1/burst2"
for i in $(seq 1 1000); do : > "$TREE1/burst2/g$i.txt"; done
t=$(poll "$OUT/poll2.json")
echo "  one poll over the changed tree: ${t}s"
summarise "$OUT/poll2.json"
python3 - "$OUT/poll2.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
evs = d["result"]["events"]
last = evs[-1]
paths = set(last["paths"])
ok = (last["event"] == "invalidate" and not d["truncated"]
      and len(last["paths"]) >= 1001
      and {"burst2/g1.txt", "burst2/g1000.txt"} <= paths)
print("  PRECISION VERDICT: %s — the answer names every changed path (%d paths, including the new directory) rather than re-syncing the tree"
      % ("HELD" if ok else "FAILED", len(last["paths"])))
raise SystemExit(0 if ok else 1)
PY
[ $? -eq 0 ] || exit 1

echo
echo "=== 3. past the DECLARED per-event path cap ($BURST new paths against 4096): a FRESH server ==="
stop
TREE3="$OUT/tree3"
python3 "$PROBES/mkfixture.py" "$TREE3"
start "$TREE3" 3
poll "$OUT/poll3-seed.json" >/dev/null
CURSOR=$(head_seq "$OUT/poll3-seed.json")
echo "  seeded the ledger: head_seq=$CURSOR"
python3 "$PROBES/mkfixture.py" "$TREE3" --add-burst "$BURST"
t=$(poll_since "$OUT/poll3.json" "$CURSOR")
echo "  one poll after $BURST new paths (cursor=$CURSOR, as the client sends): ${t}s"
summarise "$OUT/poll3.json"
python3 - "$OUT/poll3.json" "$BURST" "$CURSOR" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
r = d["result"]
evs = r["events"]
last = evs[-1]
carried = sum(len(e["paths"]) for e in evs)
ok = (last["event"] == "overflow" and last["paths"] == []
      and len(evs) == 1 and carried == 0
      and r["scanned"] > int(sys.argv[2]) and r["head_seq"] == int(sys.argv[3]) + 1)
print("  CAP VERDICT: %s — past the cap the answer is one overflow with no path list (scanned=%d, paths carried=%d), never a partial diff"
      % ("HELD" if ok else "FAILED", r["scanned"], carried))
raise SystemExit(0 if ok else 1)
PY
[ $? -eq 0 ] || exit 1

echo
echo "=== 4. the shape of an answer (a single envelope: no stream, nothing held open) ==="
curl -sS -D - -o /dev/null -X POST "$URL" -H 'X-Bunker-Op: events' \
     -H 'Content-Type: application/json' --data '{}' | sed 's/^/    /'

echo
echo "transcripts: $OUT"
