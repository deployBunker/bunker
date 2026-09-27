#!/usr/bin/env bash
# run-battery.sh — the COMMITTED battery's git block as the row asks for it:
# probes/bunker-fs-battery.sh run against a fresh served tree, with the git
# section's per-op rc extracted and counted. Nothing about the battery's own
# shape is changed; the wrapper only builds the fixture, starts the endpoint on a
# chosen port, runs the battery with KEEP=1 (so its `pkill -x` cleanup, which is
# not needed here, cannot touch anything else) and prints the git-block count.
#
# usage: run-battery.sh --bin BIN --davserve DS --work DIR --tag TAG [--port P]
set -uo pipefail
BIN=""; DS=""; WORK=""; TAG=""; PORT=38511
while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --tag) TAG="$2"; shift 2;;
    --port) PORT="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$BIN" ] && [ -n "$DS" ] && [ -n "$WORK" ] && [ -n "$TAG" ] || { echo "missing arg" >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
OUT="$WORK/battery-$TAG"
[ -e "$OUT" ] && { echo "refusing to reuse $OUT" >&2; exit 2; }
mkdir -p "$OUT"
TREE="$OUT/tree"
export XDG_CACHE_HOME="$OUT/xdg"
bash "$HERE/mkfixture.sh" "$TREE" | sed 's/^/  /'

cleanup() {
  RC=$?
  [ -n "${SRV_PID:-}" ] && { kill -TERM "$SRV_PID" 2>/dev/null; sleep 0.2; kill -KILL "$SRV_PID" 2>/dev/null; }
  for m in "$OUT/mnt-snap" "$OUT/mnt-nosnap" "$OUT/mnt-nosnap1" "$OUT/mnt-lowcache"; do
    timeout 20 fusermount -u "$m" >/dev/null 2>&1
  done
  exit $RC
}
trap cleanup EXIT

"$DS" --root "$TREE" --addr "127.0.0.1:$PORT" >"$OUT/davserve.out" 2>&1 &
SRV_PID=$!
URL=""
for _ in $(seq 1 80); do
  URL=$(sed -n 's/^URL=//p' "$OUT/davserve.out" | head -1)
  [ -n "$URL" ] && break
  sleep 0.25
done
[ -n "$URL" ] || { echo "davserve did not start"; cat "$OUT/davserve.out"; exit 1; }

KEEP=1 bash "$REPO/probes/bunker-fs-battery.sh" --url "$URL" --tree "$TREE" \
  --mnt "$OUT/mnt" --bin "$BIN" --concurrency 25 --csv "$OUT/battery.csv" \
  >"$OUT/battery.txt" 2>&1
echo "battery exit=$?  (transcript $OUT/battery.txt)"
sed -n '/^4\. THE 14-OPERATION/,/^section\|^5\./p' "$OUT/battery.txt" | sed 's/^/  | /'
echo "--- git block, from the CSV (section=git) ---"
awk -F, 'NR==1 || $1=="git"' "$OUT/battery.csv" | sed 's/^/  /'
GIT_ROWS=$(awk -F, '$1=="git"' "$OUT/battery.csv" | wc -l)
GIT_OK=$(awk -F, '$1=="git" && $4=="0"' "$OUT/battery.csv" | wc -l)
GIT_128=$(awk -F, '$1=="git" && $4=="128"' "$OUT/battery.csv" | wc -l)
echo "GIT BLOCK: $GIT_OK/$GIT_ROWS ok, rc=128 count=$GIT_128  (tag=$TAG)"
