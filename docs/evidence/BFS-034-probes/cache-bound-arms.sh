#!/usr/bin/env bash
# probe: at a 64 KiB bound on a FRESH endpoint (no earlier mount has used its
# default cache dir), does the client's own cache accounting stay inside the bound?
set -uo pipefail
T="${T:-$(mktemp -d /tmp/bfs034-bound-XXXXXX)}"
WT="${WT:-/home/kara/worktrees/bunker-BFS-034}"
PORT=38416; PORT2=38417
mkdir -p "$T/bin" "$T/tree" "$T/mnt"
cd "$WT" || exit 1
go build -o "$T/bin/bunker" ./cmd/bunker || exit 1
go build -o "$T/bin/davserve" ./probes/davserve || exit 1
bash probes/bunker-fs-battery-wan.sh --url "http://127.0.0.1:$PORT/dav" --tree "$T/tree" \
  --mnt "$T/mnt" --bin "$T/bin/bunker" --fixture-only >/dev/null 2>&1

"$T/bin/davserve" --root "$T/tree" --addr "127.0.0.1:$PORT"  > "$T/srv1.log" 2>&1 &
S1=$!
"$T/bin/davserve" --root "$T/tree" --addr "127.0.0.1:$PORT2" > "$T/srv2.log" 2>&1 &
S2=$!
for i in $(seq 1 60); do curl -sf -o /dev/null "http://127.0.0.1:$PORT/dav/" && curl -sf -o /dev/null "http://127.0.0.1:$PORT2/dav/" && break; sleep 0.25; done

arm() { # LABEL URL EXTRAS...
  local label="$1" url="$2"; shift 2
  local m="$T/$label"
  rm -rf "$m"; mkdir -p "$m"
  "$T/bin/bunker" fs mount "$m" --url "$url" --concurrency 8 "$@" >"$T/$label.log" 2>&1 &
  local pid=$!
  for i in $(seq 1 80); do awk -v mm="$m" '$2==mm{f=1}END{exit !f}' /proc/mounts && break; sleep 0.25; done
  local cdir; cdir="$(sed -n 's/^  cache dir    : \([^ ]*\) .*/\1/p' "$T/$label.log")"
  for f in $(ls "$m/src" | head -40); do cat "$m/src/$f" >/dev/null; done
  sleep 1.5
  local mb ub en ev
  mb="$(grep -o '"max_bytes": *[0-9]*' "$cdir/status.json" | grep -o '[0-9]*')"
  ub="$(grep -o '"used_bytes": *[0-9]*' "$cdir/status.json" | grep -o '[0-9]*')"
  en="$(grep -o '"entries": *[0-9]*' "$cdir/status.json" | grep -o '[0-9]*')"
  ev="$(grep -o '"evictions_total": *[0-9]*' "$cdir/status.json" | grep -o '[0-9]*')"
  printf '  %-34s bound=%-8s used=%-9s entries=%-5s evictions=%-4s dir=%s du=%s\n' \
    "$label" "$mb" "$ub" "$en" "$ev" "$(basename "$cdir")" "$(du -s --block-size=1 "$cdir" 2>/dev/null | cut -f1)"
  timeout 20 fusermount -u "$m" 2>/dev/null; kill -KILL $pid 2>/dev/null
}

echo "== 64 KiB bound, read 40 x 15 KiB files =="
echo "   arm A: FRESH endpoint (:38417, no mount has ever used its default dir)"
arm bound-fresh-endpoint "http://127.0.0.1:$PORT2/dav" --cache-max-size 65536 --cache-max-entry-bytes 32768
echo "   arm B: same, on the endpoint two other mounts already used (:38416)"
arm bound-shared-endpoint "http://127.0.0.1:$PORT/dav" --cache-max-size 65536 --cache-max-entry-bytes 32768
echo "   arm C: fresh endpoint again, but with its own --cache-dir"
arm bound-own-cachedir "http://127.0.0.1:$PORT2/dav" --cache-max-size 65536 --cache-max-entry-bytes 32768 --cache-dir "$T/cacheC"

kill -KILL $S1 $S2 2>/dev/null
sleep 0.5
echo "cleanup: servers alive=$(kill -0 $S1 2>/dev/null && echo yes || echo no)/$(kill -0 $S2 2>/dev/null && echo yes || echo no)"
