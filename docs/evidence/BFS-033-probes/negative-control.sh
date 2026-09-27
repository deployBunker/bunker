#!/usr/bin/env bash
# negative-control.sh — BFS-033: prove the cell can FAIL.
#
# A cell that cannot fail proves nothing, and this repo has demonstrated that
# twice already. So the enforcement is NEUTERED here — one `return nil` in front
# of `checkHold`, applied as a patch and reverse-applied afterwards — and the
# SAME live cell is run against the neutered binary. It must reproduce the defect
# it exists for: a PUT 412 recorded in conflicts.jsonl, then a PUT 204 that LANDS,
# with the caller told it succeeded.
#
# The restore is verified by sha256 against the file's hash BEFORE the patch, and
# by a byte-comparison with a copy taken before the patch. The reverse-apply runs
# from a trap, so a failure anywhere still leaves the tree as it was found.
#
# usage: negative-control.sh [--work DIR] [--bin BUNKER] [--out FILE]
set -uo pipefail

WORK=/tmp/bfs033
BIN=""
OUT=""
LABEL=negcontrol
while [ $# -gt 0 ]; do
  case "$1" in
    --work) WORK="$2"; shift 2;;
    --bin) BIN="$2"; shift 2;;
    --out) OUT="$2"; shift 2;;
    --label) LABEL="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
PATCH="$HERE/../BFS-033-negative-control.patch"
TARGET_FILE="$REPO/internal/fsclient/write.go"
[ -n "$BIN" ] || BIN="$WORK/bin/bunker"
[ -n "$OUT" ] || OUT="$WORK/negative-control"
mkdir -p "$WORK/bin" "$OUT"

sha() { sha256sum "$1" | awk '{print $1}'; }

BEFORE=$(sha "$TARGET_FILE")
cp "$TARGET_FILE" "$WORK/write.go.fixed"
cp "$TARGET_FILE" "$WORK/write.go.at-start"

restore() {
  if [ "$(sha "$TARGET_FILE")" != "$BEFORE" ]; then
    ( cd "$REPO" && git checkout -- internal/fsclient/write.go )
  fi
}
trap restore EXIT

echo "=== BFS-033 negative control: neuter the enforcement, re-run the cell ==="
echo "fixed write.go sha256     : $BEFORE"
( cd "$REPO" && git apply "$PATCH" ) || { echo "the neuter patch did not apply"; exit 2; }
MUTATED=$(sha "$TARGET_FILE")
echo "neutered write.go sha256  : $MUTATED"
if [ "$MUTATED" = "$BEFORE" ]; then
  echo "the patch did not change the file — the control is vacuous"; exit 2
fi
echo "the two hashes differ      : OK (the mutation is real)"

echo
echo "--- building the neutered binary ---"
( cd "$REPO" && go build -o "$WORK/bin/bunker-neutered" ./cmd/bunker ) || { echo "build failed"; exit 2; }
echo "built $WORK/bin/bunker-neutered"

echo
echo "--- the UNIT cells under the same neutered tree (they must fail too) ---"
( cd "$REPO" && go test ./internal/fsclient/ -run 'TestARefusalHoldsAgainstTheReissuedWrite|TestOnlyTheCallersReadClearsTheHold|TestWriteRefusalOnStaleBase' -count=1 2>&1 ) | tee "$OUT/neutered-unit.txt" | sed 's/^/  /'
( cd "$REPO" && go test ./internal/fsmount/ -run 'TestAReissuedResizeIsRefusedUntilTheCallerReReads' -count=1 2>&1 ) | tee "$OUT/neutered-unit-mount.txt" | sed 's/^/  /'
echo "  (a FAIL here is the point: the cells can fail, so they catch the bug they exist for)"

echo
echo "--- the SAME cell, against the neutered binary (it must go RED) ---"
mkdir -p "$WORK/tree-$LABEL"
if [ -e "$WORK/run-$LABEL" ]; then
  echo "refusing to reuse $WORK/run-$LABEL"; exit 2
fi
bash "$HERE/mount-arm.sh" --label "$LABEL" --tree "$WORK/tree-$LABEL" \
  --bin "$WORK/bin/bunker-neutered" --davserve "$WORK/bin/davserve" --work "$WORK" \
  --trace --trace-port 18510 \
  --reader python3 --reader-args "$HERE/bfs033-arms.py --mode cell --expect red" \
  > "$OUT/neutered-cell.txt" 2>&1
NRC=$?
sed 's/^/  /' "$OUT/neutered-cell.txt"
echo "neutered cell rc=$NRC (0 = the defect was reproduced, which is what the control requires)"

echo
echo "--- restoring ---"
restore
AFTER=$(sha "$TARGET_FILE")
echo "restored write.go sha256  : $AFTER"
if [ "$AFTER" = "$BEFORE" ]; then echo "RESTORE VERIFIED (sha256 identical)"; else echo "RESTORE FAILED"; exit 2; fi
if cmp -s "$TARGET_FILE" "$WORK/write.go.fixed"; then echo "byte-identical to the pre-patch copy: OK"; else echo "byte comparison FAILED"; exit 2; fi
if cmp -s "$TARGET_FILE" "$WORK/write.go.at-start"; then echo "unchanged from the tree as found: OK"; else echo "the tree differs from how it was found"; exit 2; fi
echo "git status for the file   : $( cd "$REPO" && git status --short internal/fsclient/write.go | wc -l ) line(s) (0 = clean)"

if [ "$NRC" = 0 ]; then
  echo
  echo "NEGATIVE CONTROL PASSES: with the enforcement neutered the cell reproduces the defect"
  echo "(412 recorded, 204 landed). With the enforcement in place the same cell is green."
  exit 0
fi
echo
echo "NEGATIVE CONTROL FAILS: the cell did not reproduce the defect without the enforcement"
exit 1
