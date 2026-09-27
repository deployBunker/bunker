#!/usr/bin/env bash
# fastpath.sh — the FAST-PATH COST, as numbers, on this build.
#
# BFS-044's brief forbids regressing the read path or the one-call snapshot, and
# asks for the fast-path cost as a NUMBER rather than as a claim. This reader is
# BFS-024's measurement repeated on the BFS-044 tree: the same operations a
# working tree's tools actually issue, each priced in REQUESTS (the client's own
# transport counter, read from the status document the mount keeps fresh) and in
# wall time.
#
# It is run twice — once on the stock mount and once with every --hot.* knob set
# to a non-default value and the feature ARMED — because "must not make any
# feature default-on in a way that a stock client or an old script notices" has a
# measurable form: the two runs' request counts must be IDENTICAL. The hot path
# is BFS-037's; this row only supplies the surface it reads, and the surface must
# cost nothing until something reads it.
#
# usage: fastpath.sh [--files N]     (MNT/TREE/CDIR come from the mount arm)
set -uo pipefail

MNT="${MNT:-}"; TREE="${TREE:-}"; CDIR="${CDIR:-}"; FILES=0
while [ $# -gt 0 ]; do
  case "$1" in
    --files) FILES="$2"; shift 2;;
    --mnt) MNT="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    --cdir) CDIR="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$MNT" ] && [ -n "$TREE" ] || { echo "missing MNT/TREE" >&2; exit 2; }

# the counter the mount publishes: transport.requests_total in status.json
req() { sed -n 's/.*"requests_total": \([0-9]*\).*/\1/p' "$CDIR/status.json" 2>/dev/null | head -1; }
settle() { sleep 1.6; }   # the status loop ticks at 1 Hz

LIST=$(find "$TREE" -maxdepth 1 -type f -name 'f*.txt' | sort)
N=0; for f in $LIST; do N=$((N+1)); done
[ "$FILES" != 0 ] && [ "$FILES" -lt "$N" ] && N="$FILES"
echo "  fixture: $N files, $(( $(du -sk "$TREE" | awk '{print $1}') )) KiB in $TREE"

measure() {
  local label="$1"; shift
  local a b t0 t1
  settle; a=$(req)
  t0=$(date +%s%N)
  "$@" >/dev/null 2>&1
  t1=$(date +%s%N)
  settle; b=$(req)
  printf '  %-34s requests=%-4s wall_ms=%-6s\n' "$label" "$(( ${b:-0} - ${a:-0} ))" "$(( (t1 - t0) / 1000000 ))"
}

catmnt() { cat $(find "$MNT" -maxdepth 1 -type f -name 'f*.txt' | sort); }

measure "ls -l (cold, first look)"  ls -l "$MNT"
measure "ls -l (again)"             ls -l "$MNT"
measure "ls -lR (a walk)"           ls -lR "$MNT"
measure "find -type f | wc -l"      find "$MNT" -type f
measure "stat one file"             stat "$MNT/f001.txt"
measure "read ALL files, cold"      catmnt
measure "read ALL files, warm"      catmnt
measure "read ONE file, warm"       cat "$MNT/f001.txt"

# the invalidation channel's own cost over an idle window: per INTERVAL, never
# per path (BFS-024's last row, kept because it is the row that says whether the
# channel's cost scales with the tree).
settle; a=$(req); sleep 6; settle; b=$(req)
echo "  idle 6s window                     requests=$(( ${b:-0} - ${a:-0} ))   (declared poll interval 2000ms => ~3)"
echo "  channel: $(sed -n 's/.*"mechanism": "\([a-z]*\)".*/mechanism=\1/p' "$CDIR/status.json" | head -1) $(sed -n 's/.*"poll_interval_ms": \([0-9]*\).*/poll_ms=\1/p' "$CDIR/status.json" | head -1)"
echo "  cache: $(sed -n 's/.*"used_bytes": \([0-9]*\).*/used_bytes=\1/p' "$CDIR/status.json" | head -1) $(sed -n 's/.*"entries": \([0-9]*\).*/entries=\1/p' "$CDIR/status.json" | head -1) $(sed -n 's/.*"hits": \([0-9]*\).*/hits=\1/p' "$CDIR/status.json" | head -1)"
exit 0
