#!/usr/bin/env bash
# meta-walk.sh — the METADATA half of BFS-025's fast-path measurement: stat every
# file through the mount and sum the sizes, with no content read at all.
#
# This is the arm a live-metadata fix would have paid for: one round trip per
# attrs reply, on every stat of every file. The fix under test adds none, so this
# arm and whole-tree-read.sh must both show the same request count and the same
# wall time pre-fix and post-fix.
#
# env (set by mount-arm.sh): MNT TREE OUT
set -uo pipefail

mapfile -t SET < <(find "$MNT" -type f | sort)
echo "stating ${#SET[@]} files through the mount (no content read)…"
START=$(( $(date +%s%N) / 1000000 ))
TOTAL=0
FAILS=0
for f in "${SET[@]}"; do
  sz=$(timeout 30 stat -c %s "$f" 2>/dev/null) || { FAILS=$((FAILS + 1)); continue; }
  TOTAL=$(( TOTAL + sz ))
done
END=$(( $(date +%s%N) / 1000000 ))
echo "stat_bytes_total=$TOTAL stat_failures=$FAILS stat_wall_ms=$((END - START))"
[ "$FAILS" -eq 0 ]
