#!/usr/bin/env bash
# bfs020-rmdir.sh — does `rmdir` through the mount refuse a NON-EMPTY directory?
#
# Found while running this row's acceptance: `git checkout main` through the mount
# (from an unborn branch) deleted the branch's reflog and then walked up the
# directory chain, and the mount answered `rmdir .git` with rc=0 — the served
# tree's `.git` went from 10 entries to 0. git stops that walk only when `rmdir`
# FAILS, so a mount that passes the request through turns a ref cleanup into a
# recursive delete of the served tree.
#
# env: MNT, TREE
set -uo pipefail
MNT="${MNT:?}"; TREE="${TREE:?}"

echo "== R: rmdir over a non-empty directory, and over an empty one =="
mkdir -p "$MNT/rm/sub"; printf 'keep me\n' > "$MNT/rm/sub/f.txt"; printf 'keep me too\n' > "$MNT/rm/g.txt"
sleep 0.3
echo "   served tree before : $(find "$TREE/rm" | sort | tr '\n' ' ')"
rmdir "$MNT/rm" 2>/tmp/bfs020-rmdir.err; rc=$?
echo "   rmdir non-empty rc=$rc  $(head -c 90 /tmp/bfs020-rmdir.err)"
sleep 0.5
echo "   served tree after  : $(find "$TREE/rm" 2>/dev/null | sort | tr '\n' ' ')"
if [ -e "$TREE/rm/sub/f.txt" ] && [ -e "$TREE/rm/g.txt" ]; then
  echo "   VERDICT: the non-empty directory and its contents SURVIVE (rmdir refused it)"
else
  echo "   VERDICT: RMDIR OF A NON-EMPTY DIRECTORY DESTROYED THE SERVED SUBTREE"
fi
rm -f "$MNT/rm/sub/f.txt" "$MNT/rm/g.txt" 2>/dev/null
rmdir "$MNT/rm/sub" 2>/dev/null; echo "   rmdir of the emptied subdir rc=$?"
rmdir "$MNT/rm" 2>/dev/null; echo "   rmdir of the emptied dir rc=$?"
sleep 0.3
echo "   served tree at the end: $(find "$TREE/rm" 2>/dev/null | sort | tr '\n' ' ')(gone means both empty dirs were removed)"

echo "== R2: the shape git walks — a chain of directories, one file deep =="
mkdir -p "$MNT/chain/a/b/c"; printf 'x' > "$MNT/chain/a/b/c/deep.txt"
sleep 0.3
for d in "$MNT/chain" "$MNT/chain/a" "$MNT/chain/a/b" "$MNT/chain/a/b/c"; do
  rmdir "$d" 2>/dev/null; echo "   rmdir $(basename "$d") rc=$?"
done
sleep 0.5
echo "   served tree: $(find "$TREE/chain" 2>/dev/null | sort | tr '\n' ' ')"
if [ -e "$TREE/chain/a/b/c/deep.txt" ]; then
  echo "   VERDICT: the chain SURVIVED (every non-empty level was refused)"
else
  echo "   VERDICT: THE CHAIN WAS DESTROYED"
fi
rm -f "$MNT/chain/a/b/c/deep.txt" 2>/dev/null
rmdir "$MNT/chain/a/b/c" "$MNT/chain/a/b" "$MNT/chain/a" "$MNT/chain" 2>/dev/null
echo done
