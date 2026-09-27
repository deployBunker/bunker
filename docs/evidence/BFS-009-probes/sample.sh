#!/usr/bin/env bash
# bfs009-sample.sh — sample the client's real on-disk cache size while a run is
# in flight, so the bound is measured DURING the run and not only after it.
#
# usage: bfs009-sample.sh <cache-dir> <out.csv> [interval_s]
# appends: <epoch_ms>,<du_total>,<du_blobs>,<du_index>,<used_bytes>,<entries>,<evictions>
set -uo pipefail
DIR="$1"; OUT="$2"; IV="${3:-0.5}"
: > "$OUT"
echo "epoch_ms,du_total,du_blobs,du_index,file_blobs_bytes,file_index_bytes,used_bytes,blobs_bytes,index_bytes,entries,evictions_total,bypass_events,oversize_bypasses,hits,misses" >> "$OUT"
while [ -d "$DIR" ]; do
  now=$(( $(date +%s%N) / 1000000 ))
  du_total=$(du -sb "$DIR" 2>/dev/null | awk '{print $1}')
  du_blobs=$(du -sb "$DIR/blobs" 2>/dev/null | awk '{print $1}')
  du_index=$(du -sb "$DIR/index.json" 2>/dev/null | awk '{print $1}')
  # real file byte sizes, no block rounding, for the exact comparison
  f_blobs=$(find "$DIR/blobs" -type f -printf '%s\n' 2>/dev/null | awk '{s+=$1} END{print s+0}')
  f_index=$(stat -c %s "$DIR/index.json" 2>/dev/null || echo 0)
  row=$(sed -n 's/.*"used_bytes": *\([0-9]*\).*/\1/p' "$DIR/status.json" 2>/dev/null | head -1)
  blobs=$(sed -n 's/.*"blobs_bytes": *\([0-9]*\).*/\1/p' "$DIR/status.json" 2>/dev/null | head -1)
  idx=$(sed -n 's/.*"index_bytes": *\([0-9]*\).*/\1/p' "$DIR/status.json" 2>/dev/null | head -1)
  ent=$(sed -n 's/.*"entries": *\([0-9]*\).*/\1/p' "$DIR/status.json" 2>/dev/null | head -1)
  ev=$(sed -n 's/.*"evictions_total": *\([0-9]*\).*/\1/p' "$DIR/status.json" 2>/dev/null | head -1)
  byp=$(sed -n 's/.*"bypass_events": *\([0-9]*\).*/\1/p' "$DIR/status.json" 2>/dev/null | head -1)
  ob=$(sed -n 's/.*"oversize_bypasses": *\([0-9]*\).*/\1/p' "$DIR/status.json" 2>/dev/null | head -1)
  hi=$(sed -n 's/.*"hits": *\([0-9]*\).*/\1/p' "$DIR/status.json" 2>/dev/null | head -1)
  mi=$(sed -n 's/.*"misses": *\([0-9]*\).*/\1/p' "$DIR/status.json" 2>/dev/null | head -1)
  printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
    "$now" "${du_total:-0}" "${du_blobs:-0}" "${du_index:-0}" "${f_blobs:-0}" "${f_index:-0}" \
    "${row:-}" "${blobs:-}" "${idx:-}" "${ent:-}" "${ev:-}" "${byp:-}" "${ob:-}" "${hi:-}" "${mi:-}" >> "$OUT"
  sleep "$IV"
done
