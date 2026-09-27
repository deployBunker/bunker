#!/usr/bin/env bash
# bfs009-diff-probe.sh — is the E-4 `diff` op still unimplemented, and does the
# CLIENT-side diff feature exist? Both asked for by evidence, not by trust.
#
# usage: bfs009-diff-probe.sh <surface-root-url>   e.g. http://127.0.0.1:18491/dav
set -uo pipefail
U="$1"; OUT="${2:-/tmp/bfs009/diff-probe}"
mkdir -p "$OUT"

echo "=== 1. the E-4 op catalogue, as the running build advertises it ==="
curl -sS -X POST -H 'X-Bunker-Op: capabilities' -H 'Content-Type: application/json' \
  --data '{}' "$U/" > "$OUT/capabilities.json"
python3 - "$OUT/capabilities.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))["result"]["capabilities"]
ext = d["extensions"]
print("op.ops (implemented)   :", ext["op"]["ops"])
print("watch.modes            :", json.dumps(ext["watch"]["modes"]))
print("degradations (op:*)    :", [x["capability"] for x in d["degradations"] if x["capability"].startswith("op:")])
print("op versions/limits     :", json.dumps(ext["op"].get("default_max_bytes")), ext["op"].get("abs_max_bytes"))
PY

echo
echo "=== 2. LIVE: every catalogue op that E-4 declares, one POST each ==="
printf '%-12s %-6s %s\n' OP HTTP DETAIL > "$OUT/ops.txt"
for op in capabilities snapshot status diff rev-parse ls-files log events watch bogus-op; do
  code=$(curl -sS -o "$OUT/op-$op.json" -w '%{http_code}' -X POST \
    -H "X-Bunker-Op: $op" -H 'Content-Type: application/json' --data '{}' "$U/")
  detail=$(python3 - "$OUT/op-$op.json" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("(not JSON)"); raise SystemExit
e = d.get("error") or {}
print("verdict=%-24s capability=%-12s scope=%-8s phase=%-4s mode=%-5s detail=%s" % (
    d.get("verdict"), e.get("capability", "-"), e.get("scope", "-"), e.get("phase", "-"),
    e.get("mode", "-"), (e.get("detail") or "-")[:70]))
PY
)
  printf '%-12s %-6s %s\n' "$op" "$code" "$detail" | tee -a "$OUT/ops.txt"
done

echo
echo "=== 3. the CLIENT side: does a local (non-delegated) diff exist? ==="
echo "-- what the client's own command surface offers:"
cd /home/kara/worktrees/bunker-BFS-009
grep -n 'DelegatedOps = \|Catalogue = ' internal/fsclient/delegate.go
echo "-- the diff command's own help (the client's user-facing answer):"
timeout 30 /tmp/bfs009/bin/bunker fs diff --help > "$OUT/diff-help.txt" 2>&1
sed -n '1,20p' "$OUT/diff-help.txt"
echo "-- source: is there any LOCAL diff implementation (a hunk/@@/patch generator)?"
grep -rn 'hunk\|@@ -\|unified diff\|diffLines\|lcs' internal/fsclient/*.go internal/cli/fs.go | grep -v '_test.go' | head -10 || echo "  (no local diff engine anywhere in internal/fsclient or internal/cli/fs.go)"

echo
echo "=== 4. LIVE through the client CLI: bunker fs diff against the landed server ==="
timeout 40 /tmp/bfs009/bin/bunker fs diff --url "$U" --path src --json > "$OUT/cli-diff.out" 2> "$OUT/cli-diff.err"
echo "exit code = $?"
echo "-- stdout:"; sed -n '1,15p' "$OUT/cli-diff.out"
echo "-- stderr:"; sed -n '1,15p' "$OUT/cli-diff.err"
