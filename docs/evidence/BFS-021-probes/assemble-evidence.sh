#!/usr/bin/env bash
# assemble-evidence.sh — fold the BFS-021 run outputs into the evidence files the
# row asks for. Every block is copied VERBATIM from a run's own output; only the
# section headers are added, so a reader can trace a claim back to the run that
# produced it rather than to this script.
#
# usage: assemble-evidence.sh [--red DIR] [--trace DIR] [--green DIR] [--arms FILE] [--out DIR]
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$(cd "$HERE/.." && pwd)"
RED=/tmp/bfs021/evidence-red3
TRACE=/tmp/bfs021/evidence-trace3
GREEN=/tmp/bfs021/evidence-green3
ARMS=/tmp/bfs021/arms-all2.txt
COST=/tmp/bfs021/w-cost1/run-cost1
while [ $# -gt 0 ]; do
  case "$1" in
    --red) RED="$2"; shift 2;;
    --trace) TRACE="$2"; shift 2;;
    --green) GREEN="$2"; shift 2;;
    --arms) ARMS="$2"; shift 2;;
    --cost) COST="$2"; shift 2;;
    --out) OUT="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done

banner() { echo; echo "================================================================================"; echo "$1"; echo "================================================================================"; }

# collect <outfile> <dir> <title> <arm...>
collect() {
  local out="$1" dir="$2" title="$3"; shift 3
  banner "$title" > "$out"
  local a f missing=0
  for a in "$@"; do
    f="$dir/BFS-021-$a.txt"
    echo >> "$out"; echo "### arm ${a%%-*}-${a#*-}   ($f)" >> "$out"
    if [ -f "$f" ]; then
      cat "$f" >> "$out"
    else
      echo "MISSING: $f" >> "$out"; missing=1
    fi
  done
  echo "wrote $out ($(wc -c <"$out") B)$([ "$missing" = 0 ] || echo '  ** A SOURCE WAS MISSING **')"
}

collect "$OUT/BFS-021-red.txt" "$RED" \
  "BFS-021 RED — the UNFIXED tree: an append through the mount FAILS and its bytes are LOST (read back)" \
  red3-sh red3-fd red3-ab red3-rdwr red3-multi red3-new red3-rplus red3-trunc

collect "$OUT/BFS-021-green.txt" "$GREEN" \
  "BFS-021 GREEN — the FIXED tree: every append shape lands byte-for-byte, the refusals are unchanged" \
  green3-sh green3-fd green3-ab green3-rdwr green3-multi green3-new green3-rplus green3-trunc green3-reader

collect "$OUT/BFS-021-trace.txt" "$TRACE" \
  "BFS-021 TRACE — WHICH shape an append is, measured one FUSE request at a time" \
  trace3-sh trace3-fd trace3-multi trace3-rplus trace3-trunc trace3-new

# The instrument's own lines, pulled out of the mount logs the trace arms left.
{
  banner "The instrument's lines, verbatim (BFS021-TRACE open/write), one block per arm"
  for arm in trace3-sh trace3-fd trace3-multi trace3-rplus trace3-trunc trace3-new; do
    log="/tmp/bfs021/w-trace3/run-trace3/$arm/mount.log"
    echo
    echo "### $arm  ($log)"
    if [ -f "$log" ]; then
      grep -E "BFS021-TRACE" "$log" || echo "(no BFS021-TRACE line)"
    else
      echo "MISSING: $log"
    fi
  done
  banner "What the instrument was (and why it does not change the measured thing)"
  cat <<'NOTE'
The instrument is a TEMPORARY edit to a scratch copy of the tree
(/tmp/bfs021/instr, rsync'd from the worktree — the worktree itself is untouched),
adding ONE trace line to node.Open (which already existed) and ONE temporary
node.Write that logs the request and then answers EXACTLY what go-fuse's bridge
answered before any Write method existed (fs/bridge.go: `return 0, fuse.ENOTSUP`)
UNLESS the handle can accept a write, in which case it delegates to it.

That second rule was added after the instrument's FIRST version broke the control:
a temporary node.Write that answered EOPNOTSUPP unconditionally also intercepted the
CREATE path's write handle, so `new` (the arm that must always land) failed. The
instrument was measuring itself. With the delegation in place the whole RED battery
reproduces exactly (sh/fd/ab/rdwr/multi/rplus/trunc as `loss`/`silent`, `new` as
`ok`), which is what makes it a measurement of the unfixed tree rather than of the
instrument.
NOTE
} >> "$OUT/BFS-021-trace.txt"
echo "appended the instrument's own lines to $OUT/BFS-021-trace.txt"

if [ -f "$ARMS" ]; then
  {
    banner "BFS-021 RED-PROOF ARMS — one source mutation per cell, declared RED and declared-green sets"
    cat "$ARMS"
  } > "$OUT/BFS-021-arms.txt"
  echo "wrote $OUT/BFS-021-arms.txt ($(wc -c <"$OUT/BFS-021-arms.txt") B)"
else
  echo "MISSING arms transcript: $ARMS" >&2
fi

# The successful-path cost, its own file: the arm prints one number per figure and
# the reader.out block is copied verbatim, together with the owner-facing status the
# mount printed at the end of the same run.
COST="$COST/reader.out"
COSTROOT="${COST%/reader.out}"
{
  banner "BFS-021 COST — the successful path as numbers: N appends of one tail to one file, through a real mount"
  echo "the arm's own output (verbatim):"
  if [ -f "$COST" ]; then sed 's/^/  /' "$COST"; else echo "  MISSING: $COST"; fi
  echo
  echo "the mount's own records for the same run:"
  echo "--- status.json (the append block, verbatim) ---"
  if [ -f "$COSTROOT/status.json" ]; then grep -A6 '"append"' "$COSTROOT/status.json" | sed 's/^/  /'; else echo "  MISSING: $COSTROOT/status.json"; fi
  echo
  echo "WHAT TO READ: an append is served by read-modify-publish, so it costs ONE base"
  echo "GET plus ONE whole-file conditional PUT. The latency is the two serialized round"
  echo "trips; the figure that grows with the file is bytes_put. Both are printed above"
  echo "and neither is inferred."
} > "$OUT/BFS-021-cost.txt"
echo "wrote $OUT/BFS-021-cost.txt ($(wc -c <"$OUT/BFS-021-cost.txt") B)"
