#!/usr/bin/env bash
# probe: is the DEFAULT cache directory shared between two mounts of the same URL?
set -uo pipefail
T=/tmp/bfs034-cacheid; WT=/home/kara/worktrees/bunker-BFS-034; PORT=38415
rm -rf "$T"; mkdir -p "$T/bin" "$T/tree" "$T/m1" "$T/m2" "$T/m3"
cd "$WT" || exit 1
go build -o "$T/bin/bunker" ./cmd/bunker || exit 1
go build -o "$T/bin/davserve" ./probes/davserve || exit 1
bash probes/bunker-fs-battery-wan.sh --url "http://127.0.0.1:$PORT/dav" --tree "$T/tree" \
  --mnt "$T/mnt" --bin "$T/bin/bunker" --fixture-only >/dev/null 2>&1
"$T/bin/davserve" --root "$T/tree" --addr "127.0.0.1:$PORT" > "$T/srv.log" 2>&1 &
SRV=$!
for i in $(seq 1 60); do curl -sf -o /dev/null "http://127.0.0.1:$PORT/dav/" && break; sleep 0.25; done
ROOT="${XDG_CACHE_HOME:-$HOME/.cache}/bunker/fs"
echo "cache root: $ROOT"
BEFORE=$(ls "$ROOT" 2>/dev/null | wc -l | tr -d ' ')
echo "mount dirs before: $BEFORE"

start() { # NAME [extra flags]
  local n="$1"; shift
  "$T/bin/bunker" fs mount "$T/$n" --url "http://127.0.0.1:$PORT/dav" "$@" >"$T/$n.log" 2>&1 &
  echo $! > "$T/$n.pid"
  for i in $(seq 1 80); do awk -v m="$T/$n" '$2==m{f=1}END{exit !f}' /proc/mounts && break; sleep 0.25; done
}
start m1
sleep 1
start m2
sleep 1
start m3 --cache-dir "$T/explicit-cache"
sleep 1
echo
echo "== new dirs in the default root since BEFORE =="
ls -td "$ROOT"/* 2>/dev/null | head -5 | while read -r d; do
  printf '  %s  mount=%s max_bytes=%s entries=%s\n' "$(basename "$d")" \
    "$(grep -o '"mount": *"[^"]*"' "$d/status.json" 2>/dev/null | cut -d'"' -f4)" \
    "$(grep -o '"max_bytes": *[0-9]*' "$d/status.json" 2>/dev/null | grep -o '[0-9]*')" \
    "$(grep -o '"entries": *[0-9]*' "$d/status.json" 2>/dev/null | grep -o '[0-9]*')"
done
echo "  (dir count before=$BEFORE, after=$(ls "$ROOT" 2>/dev/null | wc -l | tr -d ' '))"
echo
echo "== what each mount reports as its cache dir =="
for n in m1 m2 m3; do
  echo "  $n: $(sed -n 's/^  cache dir    : //p' "$T/$n.log")"
done
echo
echo "== the SAME document? sha of each mount's status.json =="
for d in $(ls -td "$ROOT"/* 2>/dev/null | head -3); do echo "  $(basename "$d") $(sha256sum "$d/status.json" 2>/dev/null | cut -c1-16)"; done
echo "  explicit: $(sha256sum "$T/explicit-cache/status.json" 2>/dev/null | cut -c1-16)"
for n in m1 m2 m3; do timeout 20 fusermount -u "$T/$n" 2>/dev/null; kill -KILL "$(cat "$T/$n.pid")" 2>/dev/null; done
kill -KILL $SRV 2>/dev/null
