#!/usr/bin/env bash
# seed-order.sh — the order a real client reaches the poll in, and what the
# ledger answers at each step.
#
# The mount's own bind sequence is Handshake (OPTIONS, capabilities, a `watch`
# probe, an `events` probe) and only then the whole-tree snapshot. So the
# ledger's first observation is the HANDSHAKE's events probe, and the mount's
# first real poll is answered with the seed `overflow` — which is why a freshly
# mounted tree shows resyncs_total=1 with the reason "overflow: the server
# declared knowledge lost" before anything is edited.
#
# That is the honest answer (nothing had been observed before that instant) and
# it is the safe one (it makes the client re-establish its view, so the window
# between its own snapshot and the ledger's baseline cannot hide an edit). This
# arm shows the sequence explicitly rather than leaving it to be inferred from a
# status document.
#
# usage: seed-order.sh --davserve PATH
set -uo pipefail
DS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --davserve) DS="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$DS" ] || { echo "--davserve is required" >&2; exit 2; }
PROBES="$(cd "$(dirname "$0")" && pwd)"

OUT="$(mktemp -d -t bfs026-seed.XXXXXX)"
TREE="$OUT/tree"
python3 "$PROBES/mkfixture.py" "$TREE" >/dev/null

"$DS" --root "$TREE" --addr 127.0.0.1:0 >"$OUT/davserve.out" 2>&1 &
SRV_PID=$!
trap '[ -n "${SRV_PID:-}" ] && kill -TERM "$SRV_PID" 2>/dev/null' EXIT
URL=""
for _ in $(seq 1 80); do
  URL=$(sed -n 's/^URL=//p' "$OUT/davserve.out" | head -1)
  [ -n "$URL" ] && break
  sleep 0.25
done
[ -n "$URL" ] || { echo "davserve did not start"; exit 1; }
echo "endpoint = $URL"
echo "tree     = $TREE"

op() { # <op> [body]
  local op="$1" body="${2-}"
  printf '  %-11s -> ' "$op"
  curl -sS -o "$OUT/last.json" -w '%{http_code} ' -X POST "$URL" \
       -H "X-Bunker-Op: $op" -H 'Content-Type: application/json' --data "$body" 2>/dev/null
  python3 - "$OUT/last.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
r = d.get("result") or {}
if "events" in r:
    print("events=%s count=%s scanned=%s head_seq=%s"
          % ([(e["event"], len(e["paths"]), e["seq"]) for e in r["events"]], r["count"], r["scanned"], r["head_seq"]))
else:
    print("verdict=%s keys=%s" % (d.get("verdict"), sorted(r.keys())[:6]))
PY
}

echo
echo "=== the mount's bind order, step by step ==="
echo "1. the handshake's probes (this client probes 'events' BEFORE it snapshots)"
op events '{}'
echo "2. the bind snapshot (whole tree) — the ledger already began, so this is not a baseline"
op snapshot '{"depth":"infinity","include_hash":false}'
echo "3. the mount's first real poll, with the cursor it actually holds (0)"
op events '{"since_seq":0}'
echo "4. the client applies the overflow and re-snapshots; its next poll is caught up"
op events '{"since_seq":1}'
echo
echo "5. an edit on the agent, and the next poll — the precise, per-path answer"
printf 'bfs026-seed-order-edit\n' > "$TREE/cached.txt"
op events '{"since_seq":1}'
echo "6. and quiet again once caught up"
op events '{"since_seq":2}'
echo
echo "transcripts: $OUT"
