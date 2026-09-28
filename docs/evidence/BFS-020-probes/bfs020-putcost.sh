#!/usr/bin/env bash
# bfs020-putcost.sh — the REFERENCE for the cost section: what one PUT costs with NO
# mount in the path, measured in the same window as the arms.
# usage: bfs020-putcost.sh <davserve-binary> <tree> <addr>
set -uo pipefail
BIN="${1:?davserve}"; TREE="${2:?tree}"; ADDR="${3:-127.0.0.1:38590}"
mkdir -p "$TREE"
"$BIN" --root "$TREE" --addr "$ADDR" >/dev/null 2>&1 &
SERVE=$!
sleep 2
echo "load: $(cut -d' ' -f1-3 /proc/loadavg)"
S=$(date +%s%N)
for i in $(seq 1 30); do curl -s -o /dev/null -X PUT --data-binary x "http://$ADDR/dav/p$i.txt"; done
E=$(date +%s%N)
printf '30 raw PUTs (no mount): %s ms total, %s ms each\n' "$(( (E-S)/1000000 ))" "$(( (E-S)/30000000 ))"
S=$(date +%s%N)
for i in $(seq 1 30); do curl -s -o /dev/null "http://$ADDR/dav/p$i.txt"; done
E=$(date +%s%N)
printf '30 raw GETs (no mount): %s ms total, %s ms each\n' "$(( (E-S)/1000000 ))" "$(( (E-S)/30000000 ))"
kill "$SERVE" 2>/dev/null
wait "$SERVE" 2>/dev/null
echo "server stopped"