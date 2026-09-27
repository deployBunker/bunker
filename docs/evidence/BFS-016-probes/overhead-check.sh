#!/usr/bin/env bash
# BFS-016 — where do the battery's uniform ~110 ms per-op wall times come from?
# The battery's own numbers (0.109-0.119 s for EVERY op) do not match the same
# operations measured directly (3-6 ms). This isolates the difference: the
# battery's exact expression vs a direct call, on the same mount, back to back.
set -uo pipefail
M=${1:-/tmp/bfs016/mnt-r1}
OP_TIMEOUT=45
echo "mount: $M"
n=6

echo
echo "--- A: the battery's exact expression: out=\"\$(timeout \$OP_TIMEOUT \$@ 2>&1)\" ---"
for i in $(seq 1 $n); do
  s=$(date +%s.%N)
  out="$(timeout "$OP_TIMEOUT" cat "$M/go.mod" 2>&1)"; rc=$?
  e=$(date +%s.%N)
  awk -v a="$s" -v b="$e" -v r="$rc" 'BEGIN{printf "  run: %.3fs rc=%s\n", b-a, r}'
done

echo
echo "--- B: the same op WITHOUT the command substitution and timeout ---"
for i in $(seq 1 $n); do
  s=$(date +%s.%N)
  cat "$M/go.mod" >/dev/null 2>&1; rc=$?
  e=$(date +%s.%N)
  awk -v a="$s" -v b="$e" -v r="$rc" 'BEGIN{printf "  run: %.3fs rc=%s\n", b-a, r}'
done

echo
echo "--- C: a pure local op in the same shell (the harness floor) ---"
for i in $(seq 1 $n); do
  s=$(date +%s.%N)
  cat /etc/hostname >/dev/null 2>&1
  e=$(date +%s.%N)
  awk -v a="$s" -v b="$e" 'BEGIN{printf "  run: %.3fs\n", b-a}'
done

echo
echo "--- D: NOW() itself, twice, the way run_one does it ---"
for i in $(seq 1 3); do
  s=$(date +%s.%N); e=$(date +%s.%N)
  awk -v a="$s" -v b="$e" 'BEGIN{printf "  two date calls: %.3fs\n", b-a}'
done

echo
echo "--- E: stat/ls through the mount, direct ---"
for op in "stat -c %s $M/go.mod" "ls $M/src" "ls -l $M"; do
  s=$(date +%s.%N); $op >/dev/null 2>&1; e=$(date +%s.%N)
  awk -v a="$s" -v b="$e" -v o="$op" 'BEGIN{printf "  %-40s %.3fs\n", o, b-a}'
done
