#!/usr/bin/env bash
# bfs020-rewrite.sh — the attribution probe for the ONE failure left in the battery's
# git block after BFS-020's fix: `git commit` opens `.git/COMMIT_EDITMSG` with
# O_WRONLY|O_CREAT|O_TRUNC, and that file already exists in any repository that has
# a commit. The question this probe answers is whose refusal that is:
#
#   R1  overwrite an EXISTING file through the mount (the O_TRUNC shape git uses)
#   R2  the same after REMOVING it (the create shape)
#   R3  `git commit` end to end, with and without the file present
#
# Run the same script against the pre-change and post-change binaries: the numbers
# for R1 must be identical (the refusal is BFS-012 clause 2 / BFS-030, untouched by
# BFS-020), and R3's second half is the shape BFS-020 is about.
#
# env: MNT, TREE
set -uo pipefail
MNT="${MNT:?}"; TREE="${TREE:?}"
GIT="git -C $MNT -c user.name=probe -c user.email=probe@invalid"

echo "== R1: O_TRUNC over an EXISTING file (what git does to COMMIT_EDITMSG) =="
echo "   exists on the served tree: $(test -e "$TREE/.git/COMMIT_EDITMSG" && echo yes || echo no)"
printf 'rewritten through the mount\n' > "$MNT/.git/COMMIT_EDITMSG" 2>/tmp/bfs020-r1.err; rc=$?
echo "   printf > existing rc=$rc  $(head -c 120 /tmp/bfs020-r1.err)"
echo "   served tree content: $(cat "$TREE/.git/COMMIT_EDITMSG" 2>/dev/null | head -c 40)"

echo "== R2: the same name after REMOVING it (the create shape) =="
rm -f "$MNT/.git/COMMIT_EDITMSG"; echo "   rm rc=$?"
printf 'created through the mount\n' > "$MNT/.git/COMMIT_EDITMSG" 2>/tmp/bfs020-r2.err; rc=$?
echo "   printf > absent rc=$rc  $(head -c 120 /tmp/bfs020-r2.err)"
echo "   served tree content: $(cat "$TREE/.git/COMMIT_EDITMSG" 2>/dev/null | head -c 40)"

echo "== R3a: git commit with COMMIT_EDITMSG present =="
printf 'battery-probe\n' > "$MNT/.battery-probe.txt"
$GIT add .battery-probe.txt 2>&1 | sed 's/^/     /'; echo "   add rc=$?"
out=$($GIT commit -q -m "probe: commit" 2>&1); echo "   commit rc=$? ${out:0:160}"

echo "== R3b: git commit with COMMIT_EDITMSG removed =="
rm -f "$MNT/.git/COMMIT_EDITMSG"
out=$($GIT commit -q -m "probe: commit again" 2>&1); echo "   commit rc=$? ${out:0:200}"
echo "   server HEAD: $(git -C "$TREE" log -1 --format='%h %s' 2>&1)"
echo "   mount  HEAD: $(git -C "$MNT" log -1 --format='%h %s' 2>&1)"
echo "   served .git/index.lock: $(test -e "$TREE/.git/index.lock" && echo PRESENT || echo absent)"
rm -f "$TREE/.battery-probe.txt" 2>/dev/null
echo done
