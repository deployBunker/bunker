#!/usr/bin/env bash
# bfs009-rev-probe.sh — P1 of the visibility probe, re-measured with a sleep
# longer than the surface's 500 ms git-rev cache between EVERY observation.
#
# The first attempt read the rev within the cache window (tree.go
# gitRevCacheTTL = 500 ms), which would have reported "no motion after a commit"
# for a value that was simply cached. A measurement that cannot see its own
# instrument reads clean only by luck.
#
# usage: bfs009-rev-probe.sh --davserve PATH --tree DIR (git-rooted or not)
set -uo pipefail
DS=""; TREE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --davserve) DS="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
OUT="/tmp/bfs009/revprobe-$(basename "$TREE")"; mkdir -p "$OUT"
SRV_PID=""
trap 'if [ -n "$SRV_PID" ]; then kill -TERM "$SRV_PID" 2>/dev/null; fi' EXIT
"$DS" --root "$TREE" --addr 127.0.0.1:0 > "$OUT/davserve.out" 2>&1 &
SRV_PID=$!
URL=""
for _ in $(seq 1 80); do URL=$(sed -n 's/^URL=//p' "$OUT/davserve.out" | head -1); [ -n "$URL" ] && break; sleep 0.25; done
[ -n "$URL" ] || { echo "davserve did not start"; exit 1; }
echo "tree: $TREE   git=$([ -e "$TREE/.git" ] && echo yes || echo no)"
# 1 s > the surface's 500 ms rev cache, so every reading is a fresh resolution.
rev() { sleep 1.1; curl -sS -X OPTIONS -D - -o /dev/null "$URL/" | sed -n 's/^[Xx]-[Bb]unker-[Rr]ev: *//p' | tr -d '\r'; }

R0=$(rev); echo "initial                          rev=$R0"
printf 'out-of-band edit %s\n' "$(date +%s)" > "$TREE/rev-probe.txt"
R1=$(rev); echo "after an OUT-OF-BAND edit        rev=$R1  $([ "$R1" = "$R0" ] && echo 'NO MOTION' || echo 'moved')"
printf 'through the surface\n' > "$OUT/put.bin"
curl -sS -o /dev/null -X PUT --data-binary @"$OUT/put.bin" "$URL/put-through-surface.txt"
R2=$(rev); echo "after a mutation THROUGH PUT     rev=$R2  $([ "$R2" != "$R1" ] && echo 'moved' || echo 'NO MOTION')"
if [ -e "$TREE/.git" ]; then
  ( cd "$TREE" && git add -A && git -c user.email=b@b -c user.name=b commit -qm "rev probe $RANDOM" ) && echo "  (commit made)"
  R3=$(rev); echo "after a git COMMIT               rev=$R3  $([ "$R3" != "$R2" ] && echo 'moved' || echo 'NO MOTION')"
fi
echo
echo "SUMMARY: the rev token is HEAD for a git tree and a surface-mutation counter"
echo "otherwise; the question this probe answers is whether an edit made ON THE"
echo "AGENT moves it. Above, that line is the OUT-OF-BAND one."
