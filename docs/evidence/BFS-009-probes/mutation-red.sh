#!/usr/bin/env bash
# bfs009-mutation-red.sh — non-vacuity proof for the new refusal test.
#
# The new test claims the commit-time re-validation re-reads the BYTES
# (tree.freshEntry) rather than trusting the (size, mtime)-keyed identity cache
# (tree.hashFile). This script NEUTERS exactly that mechanism with ONE inserted
# short-circuit at the top of freshEntry, runs the test, restores the file from
# a copy and sha256-verifies the restore.
#
# Expected: MUTATION -> the test FAILS (the write lands = the clobber the rule
# forbids); RESTORED -> the test PASSES.
set -uo pipefail
WT=/home/kara/worktrees/bunker-BFS-009
SRC="$WT/internal/server/webdav/tree.go"
BAK=/tmp/bfs009/tree.go.orig
TEST='TestConflictRefusalIgnoresMtimePreservedEdit'

BEFORE=$(sha256sum "$SRC" | awk '{print $1}')
cp -p "$SRC" "$BAK"

restore() {
  cp -p "$BAK" "$SRC"
  AFTER=$(sha256sum "$SRC" | awk '{print $1}')
  if [ "$AFTER" = "$BEFORE" ]; then
    echo "RESTORE VERIFIED: tree.go sha256=$AFTER (unchanged)"
  else
    echo "RESTORE FAILED: $BEFORE -> $AFTER" >&2
    exit 3
  fi
}
trap restore EXIT

echo "tree.go before : sha256=$BEFORE"

python3 - "$SRC" <<'PY'
import sys
p = sys.argv[1]
src = open(p).read()
anchor = '''func (t *tree) freshEntry(abs string) (hashEntry, error) {
	fi, err := os.Stat(abs)
	if err != nil {
		return hashEntry{}, err
	}
	if !fi.Mode().IsRegular() {
		return hashEntry{}, fmt.Errorf("%s is not a regular file", abs)
	}
'''
assert src.count(anchor) == 1, "anchor not unique: %d" % src.count(anchor)
mutation = anchor + '''	// MUTATION (BFS-009 non-vacuity proof, reverted immediately): trust the
	// metadata-keyed identity cache instead of re-reading the bytes.
	t.mu.Lock()
	if ce, ok := t.cache[abs]; ok && ce.size == fi.Size() && ce.mtime == fi.ModTime().UnixNano() {
		t.mu.Unlock()
		return ce, nil
	}
	t.mu.Unlock()
'''
open(p, "w").write(src.replace(anchor, mutation))
print("mutation inserted after freshEntry's regular-file check")
PY

go -C "$WT" build ./... || { echo "BUILD BROKEN"; exit 2; }

echo "--- MUTATION: go test -run $TEST ---"
OUT=$(go -C "$WT" test ./internal/fsclient/... -count=1 -run "$TEST" -v 2>&1)
MRC=$?
printf '%s\n' "$OUT" | grep -E '^(---|===|    ---|\s+conflict_test)' | tail -12
echo "mutation test exit code = $MRC (non-zero = the test CATCHES the mutation)"

restore

echo "--- RESTORED: go test -run $TEST ---"
OUT2=$(go -C "$WT" test ./internal/fsclient/... -count=1 -run "$TEST" 2>&1)
RRC=$?
printf '%s\n' "$OUT2" | tail -4
echo "restored test exit code = $RRC (0 = green on the real tree)"

if [ "$MRC" -ne 0 ] && [ "$RRC" -eq 0 ]; then echo "NON-VACUITY: PROVEN"; else echo "NON-VACUITY: NOT PROVEN"; fi
