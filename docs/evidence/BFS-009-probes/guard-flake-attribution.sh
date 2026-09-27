#!/usr/bin/env bash
# bfs009-guard-flake-attribution.sh — attribute the Tier-1 go_tests failure the
# commit hit (internal/server/webdav, TestSameSurfaceOverHTTP1AndHTTP2/op_capabilities,
# "body differs between versions") to the BASELINE / host contention rather than to
# this row's change.
#
# Three arms:
#   A. the failing package is byte-identical to HEAD (git diff empty) -> the
#      failure cannot be content this row introduced;
#   B. the test alone, on this tree, quiet -> green;
#   C. the same test under CPU contention, repeated -> and if it fails, the two
#      bodies are diffed FIELD BY FIELD so the differing field is NAMED.
#
# usage: bfs009-guard-flake-attribution.sh
set -uo pipefail
WT=${WT:-/home/kara/worktrees/bunker-BFS-009}
OUT=/tmp/bfs009/flake
mkdir -p "$OUT"
TEST='TestSameSurfaceOverHTTP1AndHTTP2'

echo "=== ARM A: is internal/server/webdav modified relative to HEAD? ==="
DIFF=$(git -C "$WT" diff HEAD --stat -- internal/server/webdav)
if [ -z "$DIFF" ]; then echo "git diff HEAD -- internal/server/webdav : EMPTY (untouched)"; else echo "$DIFF"; fi
echo "files this row changes (staged):"
git -C "$WT" diff --cached --name-only

echo
echo "=== ARM B: the test alone, this tree, quiet ==="
( cd "$WT" && go test ./internal/server/webdav/ -count=1 -run "$TEST" 2>&1 | tail -3 )

echo
echo "=== ARM C: the same test under CPU contention (${BURNERS:-12} burners for ${SECS:-75}s) ==="
BURNERS=${BURNERS:-12}; SECS=${SECS:-75}
PIDS=""
cleanup() { for p in $PIDS; do kill -KILL "$p" 2>/dev/null; done; }
trap cleanup EXIT
for i in $(seq 1 "$BURNERS"); do
  ( end=$((SECONDS+SECS)); while [ $SECONDS -lt $end ]; do :; done ) &
  PIDS="$PIDS $!"
done

( cd "$WT" && go test ./internal/server/webdav/ -count=${COUNT:-25} -run "$TEST" -v > "$OUT/contended.txt" 2>&1 )
RC=$?
cleanup; PIDS=""
echo "contended run rc=$RC  (0 = green under load, non-zero = REPRODUCED)"
grep -c 'FAIL' "$OUT/contended.txt" | sed 's/^/FAIL lines: /'
grep -c '^    --- PASS' "$OUT/contended.txt" | sed 's/^/cells passed: /'

if grep -q 'body differs between versions' "$OUT/contended.txt"; then
  echo
  echo "REPRODUCED. Diffing the two bodies FIELD BY FIELD:"
  python3 - "$OUT/contended.txt" <<'PY'
import json, re, sys
raw = open(sys.argv[1], encoding="utf-8", errors="replace").read()
m = re.search(r"body differs between versions:\nh1: (\{.*?\})\nh2: (\{.*?\})\n", raw, re.S)
if not m:
    print("could not extract the two bodies"); raise SystemExit
a, b = m.group(1), m.group(2)
try:
    ja, jb = json.loads(a), json.loads(b)
except Exception as e:
    print("bodies are not single JSON objects: %s" % e); print(a[:400]); raise SystemExit

def walk(x, y, path=""):
    if type(x) is not type(y):
        print("  TYPE  %s: %r vs %r" % (path, type(x), type(y))); return
    if isinstance(x, dict):
        for k in sorted(set(x) | set(y)):
            walk(x.get(k), y.get(k), path + "/" + k)
    elif isinstance(x, list):
        if len(x) != len(y): print("  LEN   %s: %d vs %d" % (path, len(x), len(y)))
        for i, (u, v) in enumerate(zip(x, y)): walk(u, v, "%s[%d]" % (path, i))
    elif x != y:
        print("  DIFF  %s: %r  vs  %r" % (path, x, y))

print("differing fields:")
walk(ja, jb)
PY
else
  echo "not reproduced in ${COUNT:-25} runs under contention (that is a result too: it is a flake)"
fi
