#!/usr/bin/env bash
# run-arms.sh — the BFS-020 live arms: one fresh served tree and one fresh mount per
# probe, on whichever binary is handed in, so the SAME transcript can be produced
# before and after the change.
#
# usage: run-arms.sh --bin BUNKER --davserve DS --tag red|green [--outdir DIR]
#                    [--work DIR] [--arms "repro timing ..."]
#
# Every process is killed by explicit PID (never `pkill -f`, whose pattern matches
# the shell running it), every mount and every fusermount is bounded with `timeout`,
# and a stale run directory is refused rather than reused.
set -uo pipefail

BIN=""; DS=""; TAG="run"; OUTDIR=""; WORK=/tmp/bfs020; ARMS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --tag) TAG="$2"; shift 2;;
    --outdir) OUTDIR="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --arms) ARMS="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$BIN" ] && [ -n "$DS" ] || { echo "--bin and --davserve required" >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -n "$OUTDIR" ] || OUTDIR="$WORK/evidence-$TAG"
mkdir -p "$OUTDIR"
[ -n "$ARMS" ] || ARMS="repro timing empty poison content rmdir rewrite gitlock dupwrite gitblock"

probe_for() {
  case "$1" in
    repro)    echo "bash|$HERE/red-repro.sh";;
    timing)   echo "python3|$HERE/bfs020-timing.py";;
    empty)    echo "bash|$HERE/bfs020-empty.sh";;
    poison)   echo "bash|$HERE/bfs020-poison.sh";;
    content)  echo "bash|$HERE/bfs020-content.sh";;
    rmdir)    echo "bash|$HERE/bfs020-rmdir.sh";;
    rewrite)  echo "bash|$HERE/bfs020-rewrite.sh";;
    gitlock)  echo "bash|$HERE/bfs020-gitlock.sh";;
    dupwrite) echo "python3|$HERE/bfs020-dupwrite.py";;
    cost)     echo "python3|$HERE/bfs020-cost.py";;
    gitblock) echo "bash|$HERE/bfs020-gitblock.sh";;
    *) echo "";;
  esac
}

for arm in $ARMS; do
  spec="$(probe_for "$arm")"
  [ -n "$spec" ] || { echo "unknown arm: $arm" >&2; exit 2; }
  interp="${spec%%|*}"; probe="${spec##*|}"
  out="$OUTDIR/BFS-020-$TAG-$arm.txt"
  tree="$WORK/tree-$TAG-$arm"
  mkdir -p "$tree"
  bash "$HERE/mkfixture.sh" "$tree" >"$out" 2>&1
  echo "### arm=$arm tag=$TAG bin=$BIN" >> "$out"
  echo "### probe: $interp $probe" >> "$out"
  echo "### date: $(date -Is)  loadavg: $(cat /proc/loadavg)" >> "$out"
  bash "$HERE/arm.sh" --label "$TAG-$arm" --tree "$tree" --bin "$BIN" --davserve "$DS" \
    --work "$WORK/run-$TAG" --reader "$interp" --reader-args "$probe" >>"$out" 2>&1
  rc=$?
  echo "ARM $TAG-$arm rc=$rc -> $out"
done
