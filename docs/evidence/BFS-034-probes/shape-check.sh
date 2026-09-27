#!/usr/bin/env bash
# BFS-034 — mechanically prove the MEASURED SHAPE is unchanged.
#
# The row requires the numbers to stay comparable with the recorded baselines, so
# "the 14-operation shape and the stall classification are preserved" must be a
# check, not a sentence. This compares the (section, op) list of the two files and
# the classifier body, and fails loudly on any difference.
#
#   bash docs/evidence/BFS-034-probes/shape-check.sh
set -uo pipefail

WT="${WT:-/home/kara/worktrees/bunker-BFS-034}"
O="$WT/probes/bunker-fs-battery.sh"
W="$WT/probes/bunker-fs-battery-wan.sh"
TMP="$(mktemp -d /tmp/bfs034-shape-XXXXXX)"
MEASURED='^(ops|tree-read|git|bound|kill),'

# every measured cell, as SECTION,NAME, in file order
extract() {
  grep -oE 'run_one [a-z-]+ "[^"]+"' "$1" | sed 's/run_one //; s/ "/,/; s/"$//'
  grep -oE 'csv_row [a-z-]+ "[^"]+"' "$1" | sed 's/csv_row //; s/ "/,/; s/"$//'
  grep -oE 'echo "tree-read,[^,]+,'  "$1" | sed 's/echo "//; s/,$//'
}

extract "$O" | grep -E "$MEASURED" > "$TMP/orig.cells" || true
extract "$W" | grep -E "$MEASURED" > "$TMP/wan.cells"  || true

echo "original measured cells : $(wc -l < "$TMP/orig.cells")"
echo "wan      measured cells : $(wc -l < "$TMP/wan.cells")"
if diff -q "$TMP/orig.cells" "$TMP/wan.cells" >/dev/null; then
  echo "IDENTICAL (section, op) list in the SAME ORDER : YES"
  RC=0
else
  echo "DIFFERENT :"
  diff "$TMP/orig.cells" "$TMP/wan.cells"
  RC=1
fi
echo
echo "the WAN-only CSV sections (new work, NOT measurements):"
extract "$W" | grep -vE "$MEASURED" | cut -d, -f1 | sort -u | sed 's/^/  /'
echo
sed -n '/^classify() {/,/^}/p' "$O" > "$TMP/o.cls"
sed -n '/^classify() {/,/^}/p' "$W" > "$TMP/w.cls"
if cmp -s "$TMP/o.cls" "$TMP/w.cls"; then
  echo "classify() (the stall classification) byte-identical : YES"
else
  echo "classify() DIFFERS:"; diff "$TMP/o.cls" "$TMP/w.cls"; RC=1
fi
echo
echo "verdict: $([ "$RC" = 0 ] && echo 'the measured shape is unchanged' || echo 'SHAPE CHANGED — the numbers would not be comparable')"
exit $RC
