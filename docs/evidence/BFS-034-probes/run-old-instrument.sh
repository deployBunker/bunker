#!/usr/bin/env bash
# BFS-034 — the OLD-instrument arm: the committed probes/bunker-fs-battery.sh,
# UNMODIFIED, pointed at a server whose tree it does not own — i.e. the exact
# configuration the row says looks legitimate and proves nothing.
#
#   bash docs/evidence/BFS-034-probes/run-old-instrument.sh
#
# Two safety measures for a shared box, both stated rather than hidden:
#   * the client binary is copied to a UNIQUE name (bunker-old) so the old
#     script's cleanup `pkill -x "$(basename "$BIN")"` cannot reach another
#     session's `bunker` mount. That pkill hazard is the one the row itself
#     documents (it hung a worker for 9 hours, four times).
#   * nothing here touches a datacentre: the "remote" server is a local davserve,
#     which is enough, because the defect being demonstrated is about WHICH TREE
#     the verification reads, not about latency.
set -uo pipefail

WT="${WT:-/home/kara/worktrees/bunker-BFS-034}"
T="${T:-$(mktemp -d /tmp/bfs034-old-instrument-XXXXXX)}"
PORT="${PORT:-38413}"

mkdir -p "$T/bin" "$T/mirror" "$T/mnt"
cd "$WT" || exit 1
go build -o "$T/bin/bunker" ./cmd/bunker || exit 1
go build -o "$T/bin/davserve" ./probes/davserve || exit 1

echo "== step 1: the SERVED tree (built by the WAN battery's own fixture builder) =="
bash probes/bunker-fs-battery-wan.sh \
  --url "http://127.0.0.1:$PORT/dav" --tree "$T/tree" --mnt "$T/mnt" \
  --bin "$T/bin/bunker" --fixture-only 2>&1 | tail -6

echo
echo "== step 2: serve it =="
"$T/bin/davserve" --root "$T/tree" --addr "127.0.0.1:$PORT" > "$T/srv.log" 2>&1 &
SRV=$!
for i in $(seq 1 60); do curl -sf -o /dev/null "http://127.0.0.1:$PORT/dav/" && break; sleep 0.25; done
echo "davserve pid=$SRV on :$PORT serving $T/tree"

echo
echo "== step 3: the local tree the old instrument will be given (--tree) =="
echo "   a copy, PLUS two files the served tree does NOT have, so that anything the"
echo "   old script reports from --tree is provably not the server's state:"
cp -a "$T/tree/." "$T/mirror/"
mkdir -p "$T/mirror/newdir"
printf 'MIRROR-ONLY hello bytes\n'   > "$T/mirror/newdir/hello.txt"
printf 'MIRROR-ONLY renamed bytes\n' > "$T/mirror/newdir/renamed.txt"
echo "   mirror has newdir/hello.txt + newdir/renamed.txt; the served tree has neither:"
echo "     served: $(ls "$T/tree/newdir" 2>/dev/null | tr '\n' ' ' || echo '(no newdir)')"
echo "     mirror: $(ls "$T/mirror/newdir" | tr '\n' ' ')"

echo
echo "== step 4: run the OLD instrument, unmodified, against the served endpoint =="
cp -f "$T/bin/bunker" "$T/bin/bunker-old"
bash probes/bunker-fs-battery.sh \
  --url "http://127.0.0.1:$PORT/dav" \
  --tree "$T/mirror" \
  --mnt "$T/mnt" \
  --bin "$T/bin/bunker-old" \
  --csv "$T/old.csv" > "$T/old-transcript.txt" 2>&1
echo "old battery rc=$?  (transcript: $T/old-transcript.txt)"

echo
echo "== step 5: what the old instrument reported AS THE SERVER'S, and the truth =="
echo "--- the old script's own 'server side' lines (transcript) ---"
sed -n '/POSIX-level proof/,/cache after the ops/p' "$T/old-transcript.txt" | sed 's/^/   /'
echo "--- §5's server-hash label ---"
grep -n 'sha256 on the server' "$T/old-transcript.txt" | sed 's/^/   /'
grep -n 'the file.s bytes on the server' "$T/old-transcript.txt" | sed 's/^/   /'
echo
echo "--- the TRUTH, read on the side (the served tree) ---"
for f in newdir/hello.txt newdir/renamed.txt; do
  if [ -e "$T/tree/$f" ]; then
    echo "   served  $f: PRESENT ($(wc -c < "$T/tree/$f") bytes) sha=$(sha256sum "$T/tree/$f" | cut -c1-12)"
  else
    echo "   served  $f: ABSENT"
  fi
  if [ -e "$T/mirror/$f" ]; then
    echo "   mirror  $f: present ($(wc -c < "$T/mirror/$f") bytes) sha=$(sha256sum "$T/mirror/$f" | cut -c1-12)"
  else
    echo "   mirror  $f: absent"
  fi
done
echo
echo "--- the served repo's state after the old run (its git cleanup targets --tree) ---"
echo "   served branch: $(git -C "$T/tree" rev-parse --abbrev-ref HEAD 2>/dev/null)"
echo "   served status: $(git -C "$T/tree" status --porcelain 2>/dev/null | head -5 | tr '\n' ' ')"
echo "   served .battery-probe.txt: $([ -e "$T/tree/.battery-probe.txt" ] && echo PRESENT || echo absent)"
echo "   mirror .battery-probe.txt: $([ -e "$T/mirror/.battery-probe.txt" ] && echo PRESENT || echo absent)"
echo "   mirror branch: $(git -C "$T/mirror" rev-parse --abbrev-ref HEAD 2>/dev/null)"
echo "   mounts left under $T/mnt: $(awk -v m="$T/mnt" '$2 ~ m {n++} END{print n+0}' /proc/mounts)"

echo
echo "== cleanup =="
kill -KILL $SRV 2>/dev/null
pkill -x bunker-old 2>/dev/null
sleep 0.5
rm -rf "$T/tree" "$T/mirror"
echo "davserve alive=$(kill -0 $SRV 2>/dev/null && echo yes || echo no); bunker-old procs=$(pgrep -xc bunker-old 2>/dev/null || echo 0)"
