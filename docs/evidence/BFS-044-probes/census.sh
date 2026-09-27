#!/usr/bin/env bash
# census.sh — the ENTRY-COUNT BOUND PROOF, counted on disk rather than reported.
#
# BFS-031's finding is why this cell exists: at a 1 KiB byte bound the cache
# directory reached 30,689 B while the client's own figures stayed inside it, so
# "a byte bound alone does not bound a directory". BFS-044's answer is the
# SECOND bound — the entry count. This reader reads N files through the mount and
# then counts, from the filesystem itself:
#
#   * the cache entries the client REPORTS (status.json: entries / max_entries)
#   * the blobs that are ACTUALLY on disk (ls <cache>/blobs | wc -l)
#   * the index records that are actually in index.json
#   * the directory's real size (du -sb), which is the figure BFS-031 measured
#
# and the byte figure the client reports beside it, so the two bounds can be told
# apart: if the byte figure sits far below the byte bound while the entry count
# is at the entry bound, the ENTRY bound is what bounded the directory.
#
# usage: census.sh --mnt DIR --tree DIR --out DIR [--files N]
set -uo pipefail

# MNT/TREE/OUT/CDIR arrive from the mount arm's environment (see cells.sh).
MNT="${MNT:-}"; TREE="${TREE:-}"; OUT="${OUT:-}"; FILES=0
while [ $# -gt 0 ]; do
  case "$1" in
    --mnt) MNT="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    --out) OUT="$2"; shift 2;;
    --files) FILES="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$MNT" ] && [ -n "$TREE" ] || { echo "missing --mnt/--tree" >&2; exit 2; }

LIST=$(find "$TREE" -maxdepth 1 -type f -name 'f*.txt' | sort)
N=0; for f in $LIST; do N=$((N+1)); done
[ "$FILES" != 0 ] && [ "$FILES" -lt "$N" ] && N="$FILES"
LIST=$(printf '%s\n' "$LIST" | head -n "$N")

read_ok=0
for f in $LIST; do
  b=$(basename "$f")
  if cmp -s "$MNT/$b" "$f"; then read_ok=$((read_ok+1)); fi
done

CDIR="${CDIR:-$OUT/../xdg}"
sleep 3   # let the status loop publish the last figures

num() { sed -n "s/.*\"$1\": \([0-9]*\).*/\1/p" "$CDIR/status.json" 2>/dev/null | head -1; }
entries=$(num entries); maxentries=$(num max_entries)
used=$(num used_bytes); maxbytes=$(num max_bytes)
bypass=$(num bypass_events); hits=$(num hits); misses=$(num misses)
oversize=$(num oversize_bypasses); evictions=$(num evictions_total)

blobs_on_disk=0
[ -d "$CDIR/blobs" ] && blobs_on_disk=$(find "$CDIR/blobs" -type f | wc -l | tr -d ' ')
index_records=0
[ -f "$CDIR/index.json" ] && index_records=$(grep -o '"path"' "$CDIR/index.json" | wc -l | tr -d ' ')
dir_bytes=$(du -sb "$CDIR" 2>/dev/null | awk '{print $1}')
other_bytes=$(( ${dir_bytes:-0} - ${used:-0} ))

echo "  census: files read through the mount = $read_ok/$N (byte-identical)"
echo "  reported   : entries=$entries max_entries=$maxentries   used_bytes=$used max_bytes=$maxbytes"
echo "  on disk    : blobs=$blobs_on_disk  index_records=$index_records  dir_du_bytes=${dir_bytes:-?}"
echo "  directory  : used_bytes covers $(( (${used:-0} * 100) / (${dir_bytes:-1} + 1) ))% of du; ${other_bytes} B are outside the byte accounting (BFS-031's class)"
echo "  cache work : hits=$hits misses=$misses bypass_events=$bypass oversize_bypasses=$oversize evictions=$evictions"

rc=0
if [ "$read_ok" != "$N" ]; then echo "  CENSUS FAIL: only $read_ok of $N reads were correct"; rc=1; fi
if [ -n "$maxentries" ] && [ -n "$entries" ] && [ "$entries" -gt "$maxentries" ]; then
  echo "  CENSUS FAIL: reported entries $entries exceed max_entries $maxentries"; rc=1
fi
if [ -n "$maxentries" ] && [ "$blobs_on_disk" -gt "$maxentries" ]; then
  echo "  CENSUS FAIL: $blobs_on_disk blobs are on disk at an entry bound of $maxentries"; rc=1
fi
if [ -n "$maxbytes" ] && [ -n "$used" ] && [ "$used" -gt "$maxbytes" ]; then
  echo "  CENSUS FAIL: used_bytes $used exceeds max_bytes $maxbytes"; rc=1
fi
echo "  census verdict: $( [ "$rc" = 0 ] && echo PASS || echo FAIL )"
exit $rc
