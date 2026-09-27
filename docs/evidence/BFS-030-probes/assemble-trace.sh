#!/usr/bin/env bash
# assemble-trace-tests.sh — fold the ATTRIBUTION trace and the TEST/COST runs into
# two evidence files, verbatim, with headers naming the run each block came from.
#
# usage: assemble-trace-tests.sh [--work /tmp/bfs030] [--out DIR]
set -uo pipefail
WORK=/tmp/bfs030; OUT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --work) WORK="$2"; shift 2;;
    --out) OUT="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -n "$OUT" ] || OUT="$(cd "$HERE/.." && pwd)"

# --- the trace -----------------------------------------------------------------
{
  echo "================================================================================"
  echo "BFS-030 TRACE — the destructive half, attributed to one HTTP request"
  echo "================================================================================"
  echo
  echo "Instrument: the mount was built from the UNFIXED tree plus a temporary trace"
  echo "line in node.Setattr (removed before the commit; the line it printed is below)."
  echo "The endpoint is behind the logging reverse proxy, so every request the mount"
  echo "made is in the table, with a monotonic timestamp."
  echo
  echo "### mount.log — what the kernel actually sent for the O_TRUNC open"
  grep "BFS030-TRACE" "$WORK/run-redtrace/mount.log" || echo "(trace line absent)"
  echo
  echo "  valid=0x208 is FATTR_SIZE|FATTR_FH; fh is 0, i.e. the SETATTR names NO handle."
  echo "  That is why the fix's predicate cannot be 'the Setattr carries a handle':"
  echo "  the kernel sends the same shape for a deliberate path-based resize."
  echo
  echo "### the same five truncation shapes, one trace line each (run-shapes4)"
  grep "BFS030-TRACE" "$WORK/run-shapes4/mount.log" || echo "(absent)"
  echo
  echo "  a = os.truncate(path, 32)      : no handle  -> deliberate, must keep working"
  echo "  b = open('r+b') + ftruncate(0) : HANDLE      -> in-place resize (emptied the file on the unfixed tree)"
  echo "  c = open('wb') + write         : no handle  -> the shell's '>', the data loss"
  echo "  d = open('wb') + nothing       : no handle  -> ': > file' (a deliberate empty)"
  echo "  e = os.truncate(path, 0)       : no handle  -> deliberate empty, must keep working"
  echo
  echo "### the RED arm's own run (redtrace.txt): the '>' shape on the UNFIXED tree"
  grep -E "SHAPE |phase |SHAPE RESULT|SERVER before|SERVER after|ORIGINAL INTACT|SERVER EMPTIED|FAILURE REPORTED|VERDICT" "$WORK/redtrace.txt"
  echo
  echo "### every request the mount made during that arm (t_mono seconds)"
  jq -r '[(.t_mono),(.method),(.status|tostring),(.req_bytes|tostring),(.if_match),(.path)]|@tsv' \
    "$WORK/run-redtrace/requests.jsonl" | nl -ba
  echo
  echo "  Request 11 is the defect, as one line: PUT, 204, 0 bytes, and If-Match is the"
  echo "  hash of the ORIGINAL content (df601c57...) — the truncation was published as a"
  echo "  successful conditional write of nothing, INSIDE the open that then reported"
  echo "  failure (errno 95) from its write half. The file was 4352 B before and 0 B after."
  echo
  echo "### the FIXED tree, the same shape (greentime.txt): refused, and cheap"
  grep -E "SHAPE |phase |SERVER after|ORIGINAL INTACT|VERDICT" "$WORK/greentime.txt"
  echo
  echo "### the FIXED tree's requests for that arm: NO PUT at all"
  jq -r '[(.t_mono),(.method),(.status|tostring),(.req_bytes|tostring),(.path)]|@tsv' \
    "$WORK/run-greentime/requests.jsonl" | nl -ba
  echo
  echo "  The refused shape costs 1 ms (t_mono 296112.493 -> 296112.494) against the"
  echo "  119 ms of the destructive round trip on the unfixed tree (296084.977 -> .096)."
} > "$OUT/BFS-030-trace.txt"
echo "wrote $OUT/BFS-030-trace.txt ($(wc -c <"$OUT/BFS-030-trace.txt") B)"
