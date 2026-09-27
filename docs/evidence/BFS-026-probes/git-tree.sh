#!/usr/bin/env bash
# git-tree.sh — the poll on the tree shape the product actually serves: a git
# working tree. Two questions, both measured rather than reasoned about:
#
#   1. does a change made by an AGENT (an edit, and then a git operation) reach
#      the channel, and what does the event carry?
#   2. how noisy is the git metadata itself? `.git/index`, the reflog and the
#      refs move on every git command, so a metadata observer will report them.
#      The honest form of that is a measurement: how many paths, and does a
#      commit push the answer past the 4096 cap into an overflow?
#
# usage: git-tree.sh --davserve PATH
set -uo pipefail
DS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --davserve) DS="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$DS" ] || { echo "--davserve is required" >&2; exit 2; }
PROBES="$(cd "$(dirname "$0")" && pwd)"

OUT="$(mktemp -d -t bfs026-git.XXXXXX)"
TREE="$OUT/tree"
python3 "$PROBES/mkfixture.py" "$TREE" >/dev/null
git -C "$TREE" init -q
git -C "$TREE" -c user.name=bfs -c user.email=bfs@example.invalid add -A
git -C "$TREE" -c user.name=bfs -c user.email=bfs@example.invalid commit -qm "fixture"
echo "tree      = $TREE"
echo "files     = $(find "$TREE" -type f | wc -l) (including .git)"
echo "rev       = $(git -C "$TREE" rev-parse --short HEAD)"

"$DS" --root "$TREE" --addr 127.0.0.1:0 >"$OUT/davserve.out" 2>&1 &
SRV_PID=$!
trap '[ -n "${SRV_PID:-}" ] && kill -TERM "$SRV_PID" 2>/dev/null' EXIT
URL=""
for _ in $(seq 1 80); do
  URL=$(sed -n 's/^URL=//p' "$OUT/davserve.out" | head -1)
  [ -n "$URL" ] && break
  sleep 0.25
done
[ -n "$URL" ] || { echo "davserve did not start"; exit 1; }
echo "endpoint  = $URL"

poll() { curl -sS -o "$OUT/$1.json" -X POST "$URL" -H 'X-Bunker-Op: events' \
           -H 'Content-Type: application/json' --data "$2" 2>/dev/null; }
show() {
  python3 - "$OUT/$1.json" "$1" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
r = d["result"]
evs = r["events"]
print("  %-14s count=%d head_seq=%d scanned=%d truncated=%s" % (sys.argv[2], r["count"], r["head_seq"], r["scanned"], d["truncated"]))
for e in evs:
    paths = e["paths"]
    sample = ",".join(paths[:4]) + ("…" if len(paths) > 4 else "")
    dotgit = sum(1 for p in paths if p.startswith(".git"))
    print("    %-10s seq=%d paths=%d (dot-git paths: %d) %s" % (e["event"], e["seq"], len(paths), dotgit, sample))
PY
}

echo
echo "=== 1. seed, then confirm the quiet tree is quiet ==="
poll seed '{}'
show seed
poll quiet '{"since_seq":1}'
show quiet

echo
echo "=== 2. an edit made on the agent reaches the channel ==="
printf '# fixture, edited on the agent\n' > "$TREE/README.md"
poll edit '{"since_seq":1}'
show edit

echo
echo "=== 3. a git operation on the agent: how many paths does it move? ==="
git -C "$TREE" -c user.name=bfs -c user.email=bfs@example.invalid add -A
git -C "$TREE" -c user.name=bfs -c user.email=bfs@example.invalid commit -qm "edited"
poll commit '{"since_seq":2}'
show commit

echo
echo "=== 4. and the channel is caught up again ==="
poll after '{"since_seq":3}'
show after
echo
echo "transcripts: $OUT"
