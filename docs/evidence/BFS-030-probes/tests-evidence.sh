#!/usr/bin/env bash
# tests-evidence.sh — BFS-030's gate and regression runs, captured verbatim into
# one evidence file: the row's own tests, BFS-025's read-side tests, BFS-015's
# commit-window tests under -race, the guard's benchmark, the platform seam, the
# whole-module suite, and the static gates.
#
# usage: tests-evidence.sh [--repo DIR] [--out FILE]
set -uo pipefail
REPO=""; OUT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2;;
    --out) OUT="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -n "$REPO" ] || REPO="$(cd "$HERE/../../.." && pwd)"
[ -n "$OUT" ] || OUT="$HERE/../BFS-030-tests.txt"
cd "$REPO" || exit 2

BFS015='TestExpectedHashIsRevalidatedInsideTheCommit|TestCreateOnlyRuleIsRevalidatedInsideTheCommit|TestCommitSectionIsExclusive|TestConcurrentConditionalWritesCannotBothLand'
BFS030='TestResize|TestFtruncateWithAWriteHandle|TestDeliberateResize|TestReadsThroughAWriteHandle|TestWriteIntentFollows|TestRefusedResizeDoesNotPoison'

{
  echo "================================================================================"
  echo "BFS-030 TESTS — the fix's own tests, BFS-025's read-side tests, BFS-015 under -race,"
  echo "               the guard's benchmark, the platform seam, and the whole-module suite"
  echo "================================================================================"
  echo
  echo "### sha256 of the tree under test"
  echo "internal/fsmount/fs_linux.go     $(sha256sum internal/fsmount/fs_linux.go | awk '{print $1}')"
  echo "internal/fsclient/status.go      $(sha256sum internal/fsclient/status.go | awk '{print $1}')"
  echo
  echo "### 1. the fix's own tests (internal/fsmount), -race -count=1"
  go test -race ./internal/fsmount/ -run "$BFS030" -count=1 -v 2>&1 | grep -E "^(=== RUN|--- (PASS|FAIL)|ok|FAIL|PASS)" | sed -n '1,80p'
  echo
  echo "### 2. the whole fsmount package (BFS-025's read-bound tests included), -race -count=1"
  go test -race ./internal/fsmount/ -count=1 2>&1 | tail -3
  echo
  echo "### 3. BFS-015's commit-window tests, -race -count=1  (the row that must not be weakened)"
  go test -race ./internal/server/webdav/ -run "$BFS015" -count=1 -v 2>&1 | grep -E "^(=== RUN|--- (PASS|FAIL)|ok|FAIL|PASS)"
  echo
  echo "### 4. the read path's own package (BFS-025/BFS-026 read-side tests), -race -count=1"
  go test -race ./internal/fsclient/ -count=1 2>&1 | tail -3
  echo
  echo "### 5. the server-side surface package, -race -count=1"
  go test -race ./internal/server/webdav/ -count=1 2>&1 | tail -3
  echo
  echo "### 6. the guard's own cost: go test -bench (internal/fsmount)"
  go test ./internal/fsmount/ -run XXX -bench 'BenchmarkWriteIntentOn|BenchmarkRefusedResize' -benchtime 20000x -count=1 2>&1 | grep -E "Benchmark|ok|FAIL"
  echo
  echo "### 7. the static gates"
  echo "\$ go build ./..."  && go build ./...  && echo "OK"
  echo "\$ go vet ./..."    && go vet ./...    && echo "OK"
  echo "\$ gofmt -l (my files)" && gofmt -l internal/fsmount/fs_linux.go internal/fsmount/write_shape_test.go internal/fsmount/write_shape_bench_test.go internal/fsmount/read_bound_test.go internal/fsclient/status.go internal/fsclient/errors.go internal/cli/fs.go && echo "OK (no output above)"
  echo
  echo "### 8. the platform seam (non-Linux build of the whole module)"
  bash probes/cross-GOOS-build.sh 2>&1 | tail -25
  echo
  echo "### 9. the whole-module suite: go test ./... (count=1)"
  go test ./... -count=1 > /tmp/bfs030-fullsuite-evidence.txt 2>&1
  echo "go test ./... exit: $?"
  grep -vE "^(ok|\?)" /tmp/bfs030-fullsuite-evidence.txt | head -20
  echo "(only non-ok lines are shown above; an empty block means every package passed)"
  echo "packages ok: $(grep -cE '^ok' /tmp/bfs030-fullsuite-evidence.txt)"
  echo "failures:    $(grep -cE '^(FAIL|--- FAIL)' /tmp/bfs030-fullsuite-evidence.txt)"
} > "$OUT" 2>&1
echo "wrote $OUT ($(wc -c <"$OUT") B)"
grep -E "failures:|packages ok:|^FAIL" "$OUT" | head
