#!/usr/bin/env bash
# CRITERION 6 (QA), independent of every worker's self-report: clone what is ACTUALLY PUSHED and
# verify THAT, not my working tree. The guard verifies the working tree - this closes that gap.
# Nothing here touches /home/kara/bunker, so it cannot collide with the in-flight workers.
set -uo pipefail
Q=/tmp/qa-clone-$(date +%H%M%S)
rm -rf "$Q" 2>/dev/null
echo "=== clone the PUSHED state (no working-tree contamination) ==="
git clone -q --no-local /home/kara/bunker "$Q" 2>&1 | head -3 | sed 's/^/  /'
cd "$Q" || exit 1
echo "  cloned origin/main at: $(git rev-parse --short HEAD)"
echo "  is this the same commit I believe is pushed? local main = $(git -C /home/kara/bunker rev-parse --short HEAD)"
echo "  branch/remote: $(git rev-parse --abbrev-ref --symbolic-full-name @{u} 2>/dev/null || echo none)"
echo

echo "=== does the CLONED tree even build? (a commit that does not build must never count as landed) ==="
go build ./... > /tmp/qa-build.log 2>&1; echo "  build rc=$?"
head -5 /tmp/qa-build.log | sed 's/^/    /'
echo "  gofmt: $(gofmt -l ./cmd ./internal ./pkg 2>/dev/null | wc -l) unformatted files"
echo "  go vet: "; timeout 500 go vet ./... > /tmp/qa-vet.log 2>&1; echo "    vet rc=$?"; head -5 /tmp/qa-vet.log | sed 's/^/    /'
echo

echo "=== the full suite on the CLONED tree, timed (the independent green) ==="
date '+  start %H:%M:%S'
/usr/bin/time -f 'WALL_SECONDS=%e' go test -count=1 ./... > /tmp/qa-test.log 2>&1
rc=$?
echo "  go test rc=$rc"
grep -aE '^(FAIL|--- FAIL|ok)' /tmp/qa-test.log | grep -avE '^ok' | head -10 | sed 's/^/    /'
echo "  --- summary ---"
printf '    packages ok:      %s\n' "$(grep -acE '^ok' /tmp/qa-test.log)"
printf '    packages FAIL:    %s\n' "$(grep -acE '^FAIL' /tmp/qa-test.log)"
printf '    no test files:    %s\n' "$(grep -acE '^\?' /tmp/qa-test.log)"
grep -aE 'WALL_SECONDS' /tmp/qa-test.log | tail -1 | sed 's/^/    /'
echo

echo "=== the release's HARD CONSTRAINT, re-checked on the clone (independent of my earlier run) ==="
ls docs/evidence/ 2>/dev/null | sed 's/^/    /' | head -30
echo
echo "  evidence files present: $(ls docs/evidence/ 2>/dev/null | wc -l)"
echo
echo "=== commit integrity: did each landed commit actually carry its files? ==="
git log --oneline -8 | sed 's/^/    /'
echo "  --- the BFS-008 lesson: does the client commit carry go.mod wiring? ---"
git show --stat d1c0659 2>/dev/null | tail -6 | sed 's/^/    /'
echo
echo "=== cleanup ==="
echo "  clone kept at $Q for paging through if needed (it is a real clone, not a worktree)"
