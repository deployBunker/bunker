#!/usr/bin/env bash
# read-all.sh — read every file through the mount, serially, and reconcile the two
# byte totals (the tree on disk vs the mount's own view of it).
#
# A serial reader is the WORST case for the bound, deliberately: each file is read
# exactly once, so every read is a fetch, an insert and an index flush, and a tree
# larger than the bound must evict continuously. It is not a concurrency claim —
# BFS-016 measured the concurrency lever and BFS-012 does not re-open it.
#
# env (set by mount-arm.sh): MNT, TREE, OUT
set -uo pipefail
FIND_ROOT="${READ_ROOT:-$MNT}"

mapfile -t SET < <(find "$FIND_ROOT" -type f | sort)
echo "reading ${#SET[@]} files through the mount (serial cat, one at a time)…"
START=$(( $(date +%s%N) / 1000000 ))
OK=0; FAILS=0; FIRST_FAIL=""
for f in "${SET[@]}"; do
  if timeout 60 cat "$f" >/dev/null 2>&1; then
    OK=$((OK + 1))
  else
    FAILS=$((FAILS + 1))
    [ -z "$FIRST_FAIL" ] && FIRST_FAIL="$f"
  fi
done
END=$(( $(date +%s%N) / 1000000 ))
SRC_BYTES=$(find "$TREE" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}')
MNT_BYTES=$(find "$FIND_ROOT" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}')
echo "read ok=$OK failures=$FAILS wall_ms=$((END - START))"
[ -n "$FIRST_FAIL" ] && echo "first failure: $FIRST_FAIL"
echo "tree_bytes_on_disk=$SRC_BYTES mount_bytes_seen=$MNT_BYTES"
[ "$OK" -gt 0 ] && [ "$FAILS" -eq 0 ]
