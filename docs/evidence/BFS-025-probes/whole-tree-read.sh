#!/usr/bin/env bash
# whole-tree-read.sh — the CONTENT half of BFS-025's fast-path measurement: read
# every file through the mount, serially, with the kernel's splice path (uutils
# cat). Serial is the worst case for the cache, which is the interesting case for
# a bound.
#
# WHY THIS ARM EXISTS. BFS-025's fix could have been built by validating a file's
# metadata live (one HEAD per attrs reply), which would have made every stat — and
# therefore every `cat`, since a reader stats before it opens — cost a round trip.
# The released win it would have spent is the whole-tree read and the snapshot op
# that serves it, so the fix has to be measured against exactly that: same tree,
# same reader, requests and wall time, pre-fix vs post-fix.
#
# env (set by mount-arm.sh): MNT TREE OUT
set -uo pipefail

mapfile -t SET < <(find "$MNT" -type f | sort)
echo "reading ${#SET[@]} files through the mount (serial cat, one at a time)…"
START=$(( $(date +%s%N) / 1000000 ))
OK=0
FAILS=0
FIRST_FAIL=""
for f in "${SET[@]}"; do
  if timeout 60 cat "$f" >/dev/null 2>&1; then
    OK=$((OK + 1))
  else
    FAILS=$((FAILS + 1))
    [ -z "$FIRST_FAIL" ] && FIRST_FAIL="$f"
  fi
done
END=$(( $(date +%s%N) / 1000000 ))
echo "read ok=$OK failures=$FAILS wall_ms=$((END - START))"
[ -n "$FIRST_FAIL" ] && echo "first failure: $FIRST_FAIL"
echo "mount_bytes_seen=$(find "$MNT" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}') tree_bytes_on_disk=$(find "$TREE" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}')"
[ "$FAILS" -eq 0 ]
