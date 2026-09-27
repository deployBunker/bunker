#!/usr/bin/env bash
# BFS-034 — reproduce the LOCAL arm of the WAN battery.
#
# This is the local server the row says is enough to prove the instrument is
# correct ("a local davserve is fine"). The point of the local arm is NOT the
# numbers — it is that the instrument's same-side verification cells and its
# negative control both behave, on a link we control end to end, before the
# instrument is pointed at a datacentre.
#
#   bash docs/evidence/BFS-034-probes/run-local.sh 2>&1 | tee /tmp/bfs034-local.txt
set -uo pipefail

WT="${WT:-/home/kara/worktrees/bunker-BFS-034}"
T="${T:-$(mktemp -d /tmp/bfs034-local-XXXXXX)}"
PORT="${PORT:-38411}"

mkdir -p "$T/bin" "$T/tree" "$T/mnt"
cd "$WT" || exit 1
go build -o "$T/bin/bunker" ./cmd/bunker || exit 1
go build -o "$T/bin/davserve" ./probes/davserve || exit 1

"$T/bin/davserve" --root "$T/tree" --addr "127.0.0.1:$PORT" > "$T/srv.log" 2>&1 &
SRV=$!
echo "$SRV" > "$T/srv.pid"
for i in $(seq 1 60); do
  curl -sf -o /dev/null "http://127.0.0.1:$PORT/dav/" && break
  kill -0 $SRV 2>/dev/null || { echo "server died"; cat "$T/srv.log"; exit 1; }
  sleep 0.25
done
echo "davserve pid=$SRV port=$PORT root=$T/tree (empty; the battery builds the fixture)"

bash probes/bunker-fs-battery-wan.sh \
  --url "http://127.0.0.1:$PORT/dav" \
  --tree "$T/tree" \
  --mnt "$T/mnt" \
  --bin "$T/bin/bunker" \
  --fresh-tree \
  --control \
  --stop-endpoint "kill \$(cat $T/srv.pid)" \
  --csv "$T/battery.csv"
RC=$?
echo "battery rc=$RC"

kill -KILL $SRV 2>/dev/null
sleep 0.5
echo "cleanup: davserve alive=$(kill -0 $SRV 2>/dev/null && echo yes || echo no); mounts left=$(grep -c "$T/mnt" /proc/mounts 2>/dev/null || echo 0); tree left=$([ -d "$T/tree" ] && echo yes || echo no)"
exit $RC
