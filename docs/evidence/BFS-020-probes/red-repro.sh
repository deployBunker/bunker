#!/usr/bin/env bash
# BFS-020 RED repro, as the arms run it: create-and-close a file on the mount,
# then rename it IMMEDIATELY (expect ENOENT) and again after a 2 s settle
# (expect rc=0). Both halves are printed, with the served tree's own view after
# each, because the pair IS the defect: the caller cannot tell a successful close
# from a visible name.
#
# env: MNT (mountpoint), TREE (served tree)
set -uo pipefail
MNT="${MNT:?MNT required}"
TREE="${TREE:?TREE required}"

echo "== A1: create+close, then rename IMMEDIATELY =="
printf x > "$MNT/f.lock"; echo "   create rc=$?"
mv "$MNT/f.lock" "$MNT/f.final"; echo "   rename-immediate rc=$?"
echo "   served tree after 2 s:"; sleep 2; ls -l "$TREE" | sed 's/^/     /'

echo "== A2: create+close, SETTLE 2 s, then rename =="
printf x > "$MNT/g.lock"; echo "   create rc=$?"
sleep 2
mv "$MNT/g.lock" "$MNT/g.final"; echo "   rename-after-2s rc=$?"
sleep 1; echo "   served tree:"; ls -l "$TREE" | sed 's/^/     /'

echo "== A3: the same shape inside .git (what git does with <ref>.lock) =="
printf x > "$MNT/.git/bfs.lock"; echo "   create rc=$?"
mv "$MNT/.git/bfs.lock" "$MNT/.git/bfs.out"; echo "   rename-immediate rc=$?"
sleep 2
printf x > "$MNT/.git/bfs2.lock"; echo "   create rc=$?"
sleep 2
mv "$MNT/.git/bfs2.lock" "$MNT/.git/bfs2.out"; echo "   rename-after-2s rc=$?"
sleep 1; echo "   served .git:"; ls -l "$TREE/.git" | grep bfs | sed 's/^/     /' || true

echo "== A4: what the MOUNT says the name is (the poison check) =="
stat -c '%n size=%s' "$MNT/.git/bfs.lock" 2>&1 | sed 's/^/     /'
echo "   and a fresh O_EXCL create of a name the rename above failed to move onto:"
if : > "$MNT/.git/bfs.lock" 2>/tmp/bfs020-excl.err; then echo "     plain truncate create rc=0"; else echo "     plain truncate create rc=$?"; fi
cat /tmp/bfs020-excl.err 2>/dev/null | sed 's/^/     /'
rm -f "$MNT/.git/bfs.lock" "$MNT/f.lock" 2>/dev/null
echo "done"
