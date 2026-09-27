#!/usr/bin/env bash
# BFS-034 — how long after a write through the mount does the SIDE see it?
#
# WHY THIS EXISTS: the WAN battery verifies every write with a read on the side.
# If that read is a single `cat` issued immediately after the write, it is a
# RACE, not a verification — a false MISMATCH on a healthy link, and an
# instrument that cries wolf is as useless as one that cannot fail. This probe
# measures the publication latency so the battery's wait can be justified with a
# number instead of a guess.
#
#   bash docs/evidence/BFS-034-probes/write-visibility.sh
set -uo pipefail

WT="${WT:-/home/kara/worktrees/bunker-BFS-034}"
T="${T:-/tmp/bfs034-vis}"
PORT="${PORT:-38414}"

rm -rf "$T"; mkdir -p "$T/bin" "$T/tree" "$T/mnt/x"
cd "$WT" || exit 1
go build -o "$T/bin/bunker" ./cmd/bunker || exit 1
go build -o "$T/bin/davserve" ./probes/davserve || exit 1
echo "== fixture (built by the WAN battery's own builder, --fixture-only) =="
bash probes/bunker-fs-battery-wan.sh --url "http://127.0.0.1:$PORT/dav" --tree "$T/tree" \
  --mnt "$T/mnt" --bin "$T/bin/bunker" --fixture-only 2>&1 | tail -3

"$T/bin/davserve" --root "$T/tree" --addr "127.0.0.1:$PORT" > "$T/srv.log" 2>&1 &
SRV=$!
for i in $(seq 1 60); do curl -sf -o /dev/null "http://127.0.0.1:$PORT/dav/" && break; sleep 0.25; done
echo "davserve pid=$SRV on :$PORT"

"$T/bin/bunker" fs mount "$T/mnt/x" --url "http://127.0.0.1:$PORT/dav" --cache-dir "$T/cache" >"$T/mount.log" 2>&1 &
MP=$!
for i in $(seq 1 80); do awk -v m="$T/mnt/x" '$2==m{f=1}END{exit !f}' /proc/mounts && break; sleep 0.25; done
echo "mounted: $(awk -v m="$T/mnt/x" '$2==m{print "yes"}' /proc/mounts | head -1)"
echo

probe_one() { # LABEL SIDE_PATH MOUNT_PATH
  local label="$1" sp="$2" mp="$3"
  rm -f "$sp" 2>/dev/null
  local t0 t1 wrc seen=""
  t0=$(date +%s%N)
  printf 'VIS-%s\n' "$label" > "$mp" 2>"$T/w.err"
  wrc=$?
  local i
  for i in $(seq 1 400); do
    if [ -e "$sp" ]; then
      t1=$(date +%s%N)
      seen=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.3f", (b-a)/1e9}')
      break
    fi
    sleep 0.025
  done
  printf '  %-22s write_rc=%-3s side_visible_after=%-9s content=%s\n' \
    "$label" "$wrc" "${seen:-NEVER(10s)}" "$(cat "$sp" 2>/dev/null | tr -d '\n')"
}

echo "== publication latency, loopback (root-level, subdir, dotfile, and again) =="
probe_one "root create"      "$T/tree/wsc-root.txt"     "$T/mnt/x/wsc-root.txt"
mkdir -p "$T/mnt/x/sub" 2>/dev/null
probe_one "subdir create"    "$T/tree/sub/wsc-sub.txt"  "$T/mnt/x/sub/wsc-sub.txt"
probe_one "root dotfile"     "$T/tree/.wsc-dot"         "$T/mnt/x/.wsc-dot"
probe_one "root create #2"   "$T/tree/wsc-root2.txt"    "$T/mnt/x/wsc-root2.txt"
echo
echo "== overwrite of an EXISTING file (the shape the conflict cell uses) =="
printf 'original\n' > "$T/tree/wsc-ow.txt"
probe_one "overwrite (refused?)" "$T/tree/wsc-ow.txt"   "$T/mnt/x/wsc-ow.txt"
echo "  note: the write above goes through the client's conflict rule; rc and the"
echo "  resulting bytes on the side are the answer, and they are read on the side."
echo
echo "== the side's own view afterwards =="
ls -la "$T/tree" | grep -i wsc | sed 's/^/  /'
ls -la "$T/tree/sub" 2>/dev/null | grep -i wsc | sed 's/^/  /'
echo "  stderr from the last write: '$(cat "$T/w.err")'"

timeout 20 fusermount -u "$T/mnt/x" 2>/dev/null
kill -KILL $MP 2>/dev/null
kill -KILL $SRV 2>/dev/null
sleep 0.5
echo "cleanup: davserve alive=$(kill -0 $SRV 2>/dev/null && echo yes || echo no); mounts=$(awk -v m="$T/mnt/x" '$2==m{n++}END{print n+0}' /proc/mounts)"
