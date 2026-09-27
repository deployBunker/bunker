#!/usr/bin/env bash
# BFS-034 — the FAIL-CLOSED arm: the same instrument, with a tree that is NOT the
# tree the endpoint serves (exactly what a mis-pointed run against a datacentre
# produces). The battery must REFUSE to measure rather than print a green number.
#
#   bash docs/evidence/BFS-034-probes/run-wrong-side.sh
set -uo pipefail

WT="${WT:-/home/kara/worktrees/bunker-BFS-034}"
T="${T:-/tmp/bfs034-wrong}"
PORT="${PORT:-38412}"

rm -rf "$T"; mkdir -p "$T/bin" "$T/mirror" "$T/mnt"
cd "$WT" || exit 1
go build -o "$T/bin/bunker" ./cmd/bunker || exit 1
go build -o "$T/bin/davserve" ./probes/davserve || exit 1

echo "== step 1: build the fixture that will be SERVED, and keep it =="
bash probes/bunker-fs-battery-wan.sh \
  --url "http://127.0.0.1:$PORT/dav" --tree "$T/tree" --mnt "$T/mnt" \
  --bin "$T/bin/bunker" --fixture-only 2>&1 | tail -8

echo
echo "== step 2: serve it =="
"$T/bin/davserve" --root "$T/tree" --addr "127.0.0.1:$PORT" > "$T/srv.log" 2>&1 &
SRV=$!
for i in $(seq 1 60); do curl -sf -o /dev/null "http://127.0.0.1:$PORT/dav/" && break; sleep 0.25; done
echo "davserve pid=$SRV on :$PORT, serving $T/tree"

echo
echo "== step 3: a local tree that is NOT the served tree (a copy = a stale mirror) =="
cp -a "$T/tree/." "$T/mirror/"
echo "served : $T/tree   (go.mod sha $(sha256sum "$T/tree/go.mod" | cut -c1-12))"
echo "mirror : $T/mirror (go.mod sha $(sha256sum "$T/mirror/go.mod" | cut -c1-12))"

echo
echo "== step 4: run the WAN battery declaring the MIRROR as the side =="
echo "   expected: exit 3, and a refusal instead of a number"
bash probes/bunker-fs-battery-wan.sh \
  --url "http://127.0.0.1:$PORT/dav" \
  --tree "$T/mirror" \
  --mnt "$T/mnt" \
  --bin "$T/bin/bunker" \
  --csv "$T/wrong.csv"
RC=$?
echo "battery rc=$RC"
case "$RC" in
  3) echo "PASS: the instrument refused to measure against a tree that is not the write side." ;;
  0) echo "FAIL: it measured anyway — the guard is broken." ;;
  *) echo "UNEXPECTED rc=$RC" ;;
esac

kill -KILL $SRV 2>/dev/null
sleep 0.5
rm -rf "$T/tree" "$T/mirror"
echo "cleanup: davserve alive=$(kill -0 $SRV 2>/dev/null && echo yes || echo no); mounts left=$(awk -v m="$T/mnt" '$2 ~ m {n++} END{print n+0}' /proc/mounts)"
exit $RC
