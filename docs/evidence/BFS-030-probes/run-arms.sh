#!/usr/bin/env bash
# run-arms.sh — the BFS-030 arm battery: one mount per shape, each with its own
# fresh tree, and each arm's expectation encoded so the exit code says whether it
# passed (not a human reading the transcript).
#
# usage: run-arms.sh --bin BUNKER [--tag TAG] [--outdir DIR] [--work DIR]
#                    [--arms "trunc_open ftrunc_fd ..."]
set -uo pipefail

BIN=""; TAG="run"; OUTDIR=""; WORK=/tmp/bfs030; ARMS="trunc_open ftrunc_fd rplus append trunc_path serverdown"; ESET=green
while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN="$2"; shift 2;;
    --tag) TAG="$2"; shift 2;;
    --outdir) OUTDIR="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --arms) ARMS="$2"; shift 2;;
    --expect-set) ESET="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$BIN" ] || { echo "--bin required" >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
[ -n "$OUTDIR" ] || OUTDIR="$WORK/evidence"
mkdir -p "$OUTDIR"
DS="$WORK/bin/davserve"
TRACE_PORT=18491
FAILED=0

run_arm() {
  local label="$1" mode="$2" expect="$3" extra="$4" args="$5"
  local tree="$WORK/tree-$TAG-$label"
  local out="$OUTDIR/BFS-030-$TAG-$label.txt"
  mkdir -p "$tree"
  # a fresh tree per arm: nothing inherited, and the fixture is created by the probe
  echo "### arm=$label mode=$mode bin=$BIN expect=$expect" > "$out"
  bash "$HERE/mount-arm.sh" --label "$TAG-$label" --tree "$tree" --bin "$BIN" --davserve "$DS" \
    --work "$WORK" --flag "$extra" --trace --trace-port "$TRACE_PORT" \
    --reader python3 --reader-args "$HERE/bfs030-arms.py --mode $mode --expect $expect $args" \
    >>"$out" 2>&1
  local rc=$?
  if [ "$rc" = 0 ]; then
    echo "ARM $TAG-$label: PASS (rc=0)"
  else
    echo "ARM $TAG-$label: FAIL (rc=$rc) -> $out"
    FAILED=1
  fi
}

# The expectation set says what this TREE is supposed to do, so the same battery
# is the RED on the unfixed tree and the GREEN on the fixed one:
#   red   : the defect is present — a failed rewrite destroys the original, and
#           an in-place ftruncate destroys it too;
#   green : the fix is present — every failed write shape leaves the original
#           byte-identical, and the deliberate resize still lands.
exp_for() {
  case "$ESET:$1" in
    red:trunc_open)   echo loss;;
    red:ftrunc_fd)    echo loss;;
    red:rplus)        echo intact;;
    red:append)       echo intact;;
    red:trunc_path)   echo resized;;
    red:serverdown)   echo intact;;
    *:trunc_open)     echo intact;;
    *:ftrunc_fd)      echo intact;;
    *:rplus)          echo intact;;
    *:append)         echo intact;;
    *:trunc_path)     echo resized;;
    *:serverdown)     echo intact;;
    *)                echo either;;
  esac
}

for a in $ARMS; do
  case "$a" in
    trunc_open)   run_arm trunc_open   trunc_open "$(exp_for trunc_open)" ""      "";;
    ftrunc_fd)    run_arm ftrunc_fd    ftrunc_fd  "$(exp_for ftrunc_fd)"  ""      "";;
    rplus)        run_arm rplus        rplus      "$(exp_for rplus)"      ""      "";;
    append)       run_arm append       append     "$(exp_for append)"     ""      "";;
    trunc_path)   run_arm trunc_path   trunc_path "$(exp_for trunc_path)" ""      "";;
    serverdown)   # the transport-kill control: nothing CAN land, so the original survives
                  run_arm serverdown   trunc_open "$(exp_for serverdown)" ""      "--kill-server";;
    *) echo "unknown arm $a" >&2; FAILED=1;;
  esac
done
exit $FAILED
