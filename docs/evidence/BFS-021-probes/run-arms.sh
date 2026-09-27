#!/usr/bin/env bash
# run-arms.sh — the BFS-021 arm battery: one fresh tree and one fresh mount per
# shape, each arm's expectation passed to the probe so the EXIT CODE says whether
# the arm passed (not a human reading the transcript).
#
# usage: run-arms.sh --bin BUNKER [--davserve DS] [--tag TAG] [--set red|green]
#                    [--outdir DIR] [--work DIR] [--arms "sh fd ..."]
#
# The tree, the endpoint, the mount and the cache directory are all fresh per
# arm: nothing is inherited, and a stale run directory is refused rather than
# reused. Every process is killed by explicit PID (never `pkill -f`, whose
# pattern matches the shell running it) and every fusermount is bounded.
set -uo pipefail

BIN=""; DS=""; TAG="run"; SET="green"; OUTDIR=""; WORK=/tmp/bfs021; ARMS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --tag) TAG="$2"; shift 2;;
    --set) SET="$2"; shift 2;;
    --outdir) OUTDIR="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --arms) ARMS="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$BIN" ] || { echo "--bin required" >&2; exit 2; }
[ -n "$DS" ] || { echo "--davserve required" >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -n "$OUTDIR" ] || OUTDIR="$WORK/evidence"

# Each arm: label:expect.  `new` is the CONTROL that must always land: it proves
# the battery cannot be green by refusing everything. `rplus`/`trunc` carry the
# SAME expectation on both trees — they are the regression arms for the
# refusals BFS-012/BFS-030 established, which this row must not change.
case "$SET" in
  red)   DEFAULT_ARMS="sh:loss fd:loss ab:loss rdwr:loss multi:silent new:ok rplus:loss trunc:loss";;
  green) DEFAULT_ARMS="sh:ok fd:ok ab:ok rdwr:ok multi:ok new:ok rplus:loss trunc:loss reader:ok";;
  *) echo "--set must be red or green" >&2; exit 2;;
esac
[ -n "$ARMS" ] || ARMS="$DEFAULT_ARMS"

mkdir -p "$OUTDIR"
TRACE_PORT=18493
FAILED=0

run_arm() {
  local label="$1" expect="$2"
  local tree="$WORK/tree-$TAG-$label"
  local out="$OUTDIR/BFS-021-$TAG-$label.txt"
  mkdir -p "$tree"
  echo "### arm=$label expect=$expect bin=$BIN" > "$out"
  bash "$HERE/mount-arm.sh" --label "$TAG-$label" --tree "$tree" --bin "$BIN" --davserve "$DS" \
    --work "$WORK/run-$TAG" --trace --trace-port "$TRACE_PORT" \
    --reader python3 --reader-args "$HERE/bfs021-arms.py --mode $label --expect $expect" \
    >>"$out" 2>&1
  local rc=$?
  if [ "$rc" = 0 ]; then
    echo "ARM $TAG-$label ($expect): PASS (rc=0)"
  else
    echo "ARM $TAG-$label ($expect): FAIL (rc=$rc) -> $out"
    FAILED=1
  fi
}

for spec in $ARMS; do
  run_arm "${spec%%:*}" "${spec##*:}"
done

echo
if [ "$FAILED" = 0 ]; then
  echo "BATTERY $TAG ($SET): every arm met its declared expectation"
else
  echo "BATTERY $TAG ($SET): at least one arm FAILED its declared expectation"
fi
exit $FAILED
