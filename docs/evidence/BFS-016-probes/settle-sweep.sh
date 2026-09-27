#!/usr/bin/env bash
# BFS-016 — bound the write->rename window, and check readdir vs lookup consistency.
set -uo pipefail
M=${M:-/tmp/bfs016/mnt-r1}
T=${T:-/tmp/bfs016/tree}
D="$M/windowed"
mkdir -p "$D"

printf '%-22s %-8s %s\n' "settle" "mv-rc" "server-state-after"
for d in 0 0.05 0.1 0.25 0.5 1.0 1.5 2.0 3.0; do
  n="f_$d"
  printf 'x\n' > "$D/$n.a" 2>/dev/null
  sleep "$d"
  mv "$D/$n.a" "$D/$n.b" 2>/dev/null; rc=$?
  printf '%-22s %-8s %s\n' "${d}s" "$rc" "$(ls "$T/windowed/" | grep "$n" | tr '\n' ' ')"
  rm -f "$T/windowed/$n.a" "$T/windowed/$n.b" 2>/dev/null
  printf 'x\n' > "$D/$n.a"; sleep 0.3; mv "$D/$n.a" "$D/$n.b" 2>/dev/null
  rm -f "$T/windowed/$n.a" "$T/windowed/$n.b" 2>/dev/null
done

echo
echo "=== readdir vs lookup consistency (the .git/refs/heads listing) ==="
echo "ls    through mount : $(ls -a "$M/.git/refs/heads/" 2>&1 | tr '\n' ' ')"
echo "ls -la through mount: $(ls -la "$M/.git/refs/heads/" 2>&1 | tail -n +2 | tr '\n' ' ')"
echo "ls    server        : $(ls -a "$T/.git/refs/heads/" 2>&1 | tr '\n' ' ')"
echo "stat a .lock through mount: $(stat -c '%s %n' "$M/.git/refs/heads/bfs016-repro.lock" 2>&1)"
echo "find through mount (all files under .git/refs): $(find "$M/.git/refs" 2>&1 | tr '\n' ' ')"
echo "find server        (all files under .git/refs): $(find "$T/.git/refs" 2>&1 | tr '\n' ' ')"

echo
echo "=== does the mount see a file created NATIVELY (agent-side) promptly? ==="
printf 'native\n' > "$T/windowed/native.txt"
echo "immediate mount stat: $(stat -c '%s' "$M/windowed/native.txt" 2>&1)  (server: $(stat -c '%s' "$T/windowed/native.txt"))"
sleep 3
echo "after 3s mount stat:  $(stat -c '%s' "$M/windowed/native.txt" 2>&1)"

# leave the tree as we found it
rm -f "$T/windowed/native.txt" 2>/dev/null
rm -f "$T"/windowed/f_*.a "$T"/windowed/f_*.b 2>/dev/null
rmdir "$T/windowed" 2>/dev/null
echo done
