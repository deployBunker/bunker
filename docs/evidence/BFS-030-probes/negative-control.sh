#!/usr/bin/env bash
# negative-control.sh — disable the BFS-030 guard, prove the arms and the package
# tests go RED, then restore the fixed file and prove the restore is byte-identical
# by sha256. Nothing here is a claim: every line is printed by the run.
#
# usage: negative-control.sh [--repo DIR]
set -uo pipefail
REPO=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -n "$REPO" ] || REPO="$(cd "$HERE/../../.." && pwd)"
PATCH="$HERE/../BFS-030-negative-control.patch"
cd "$REPO" || exit 2

FILE=internal/fsmount/fs_linux.go
echo "=== the negative control applies to the FIXED tree ==="
echo "fixed-tree sha256 (before): $(sha256sum "$FILE" | awk '{print $1}')"
cp "$FILE" /tmp/bfs030-negctl-backup.go
if ! git apply --check "$PATCH"; then
  echo "the patch does not apply: refusing to continue"
  exit 1
fi
git apply "$PATCH"
echo "patch applied; sha256 (disabled): $(sha256sum "$FILE" | awk '{print $1}')"
echo "the working tree differs from HEAD by (all of this row's changes): $(git diff --name-only | tr '\n' ' ')"
echo
echo "=== build (the disabled tree must still compile) ==="
go build ./... && echo "go build ./... OK"
echo
echo "=== go test ./internal/fsmount/ -count=1  (the guard's tests must FAIL) ==="
go test ./internal/fsmount/ -count=1 2>&1 | grep -E "^(--- FAIL|--- PASS|ok|FAIL)" || true
echo
echo "=== restore ==="
cp /tmp/bfs030-negctl-backup.go "$FILE"
echo "restored sha256 : $(sha256sum "$FILE" | awk '{print $1}')"
echo "backup sha256   : $(sha256sum /tmp/bfs030-negctl-backup.go | awk '{print $1}')"
rm -f /tmp/bfs030-negctl-backup.go
echo "the tree now differs from HEAD by: $(git diff --stat | tail -1)"
echo
echo "=== the guard's tests must be GREEN again ==="
go test ./internal/fsmount/ -count=1 2>&1 | tail -2
