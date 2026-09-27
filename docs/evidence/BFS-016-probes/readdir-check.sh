#!/usr/bin/env bash
# BFS-016 — readdir consistency: the mount's listing vs the server's, per directory,
# and whether a natively-created file appears in the mount's readdir.
set -uo pipefail
M=${M:-/tmp/bfs016/mnt-r1}
T=${T:-/tmp/bfs016/tree}

cmp_dir() {
  local rel="$1"
  local m n
  m=$(ls -1 -a "$M/$rel" 2>&1 | sort | tr '\n' ' ')
  n=$(ls -1 -a "$T/$rel" 2>&1 | sort | tr '\n' ' ')
  if [ "$m" = "$n" ]; then
    printf '  %-24s MATCH   %s\n' "${rel:-/}" "$(printf '%s' "$m" | head -c 200)"
  else
    printf '  %-24s MISMATCH\n     mount : %s\n     server: %s\n' "${rel:-/}" "$m" "$n"
  fi
}

echo "=== readdir: mount vs server, per directory ==="
cmp_dir ""
cmp_dir "src"
cmp_dir "pkg"
cmp_dir "pkg/deep"
cmp_dir "pkg/deep/a"
cmp_dir "scratch"
cmp_dir ".git"
cmp_dir ".git/refs"
cmp_dir ".git/refs/heads"
cmp_dir ".git/objects"

echo
echo "=== repeat the .git/refs/heads listing three times (is it unstable?) ==="
for i in 1 2 3; do
  printf '  try %d mount : %s\n' "$i" "$(ls -1 -a "$M/.git/refs/heads" 2>&1 | tr '\n' ' ')"
  sleep 1
done

echo
echo "=== a file created NATIVELY: visible by lookup AND by readdir? ==="
printf 'native2\n' > "$T/src/native2.txt"
echo "  lookup (stat)     : $(stat -c '%s' "$M/src/native2.txt" 2>&1)"
echo "  readdir contains? : $(ls -1 "$M/src" | grep -c native2)"
echo "  direct read       : $(cat "$M/src/native2.txt" 2>&1)"
sleep 3
echo "  after 3s readdir? : $(ls -1 "$M/src" | grep -c native2)"
echo "  after 3s lookup   : $(stat -c '%s' "$M/src/native2.txt" 2>&1)"
rm -f "$T/src/native2.txt"

echo
echo "=== whole-tree walk through the mount vs native (entry counts) ==="
mc=$(find "$M" -mindepth 1 2>/dev/null | wc -l)
nc=$(find "$T" -mindepth 1 2>/dev/null | wc -l)
echo "  mount  find count: $mc"
echo "  server find count: $nc"
echo "  native ls -lR lines: $(ls -lR "$T" 2>/dev/null | wc -l)"
echo "  mount  ls -lR lines: $(ls -lR "$M" 2>/dev/null | wc -l)"
echo "  entries the server has that the mount's walk does NOT:"
comm -23 <(cd "$T" && find . -mindepth 1 | sort) <(cd "$M" && find . -mindepth 1 | sort) | head -20
echo done
