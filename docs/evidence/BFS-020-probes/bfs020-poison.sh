#!/usr/bin/env bash
# bfs020-poison.sh — the LEFTOVER-MUST-NOT-POISON cells.
#
# Cell P1 (does the mount tell the truth about a name the server does not have?):
#   create+close a file through the mount, wait for it to be published, then
#   remove it ON THE SERVER out of band, and ask the mount what it thinks: a
#   truthful mount answers ENOENT and lets an O_EXCL create of that name succeed.
#
# Cell P2 (is a name the mount is holding unpublished visible to a following
# operation?): create+close, and immediately ask the mount for the name and the
# served tree for the name -- the pair is the defect's own signature.
#
# Cell P3 (a stale lock left by a crash): plant a real `.git/index.lock` on the
# served tree, show git refuses with its OWN message (the file is stale; git
# decides, and git is right), then remove it THROUGH THE MOUNT and show the next
# git command works -- so a surviving lock is not a permanently dead mount.
#
# env: MNT, TREE
set -uo pipefail
MNT="${MNT:?MNT required}"
TREE="${TREE:?TREE required}"

echo "== P1: a name removed on the SERVER, asked of the mount =="
printf 'x' > "$MNT/p1.lock"
sleep 2
echo "   server has it: $(test -e "$TREE/p1.lock" && echo yes || echo no)"
rm -f "$TREE/p1.lock"
echo "   removed on the server; mount view now:"
stat -c '     stat: %n size=%s' "$MNT/p1.lock" 2>&1 | sed 's/^/   /'
if ls "$MNT" 2>/dev/null | grep -qx 'p1.lock'; then echo "   readdir CLAIMS p1.lock"; else echo "   readdir does not list p1.lock"; fi
if ( set -C; : > "$MNT/p1.lock" ) 2>/tmp/bfs020-p1.err; then echo "   O_EXCL create: rc=0 (truthful)"; else echo "   O_EXCL create: rc=$? $(cat /tmp/bfs020-p1.err)"; fi
sleep 1
echo "   server now: $(test -e "$TREE/p1.lock" && echo present || echo absent)"
rm -f "$MNT/p1.lock" "$TREE/p1.lock" 2>/dev/null

echo "== P2: a name the mount is holding unpublished, asked immediately =="
printf 'x' > "$MNT/p2.lock"
if stat -c '   mount stat: %n size=%s' "$MNT/p2.lock" 2>/tmp/bfs020-p2.err; then :; else echo "   mount stat: FAILED $(cat /tmp/bfs020-p2.err)"; fi
if test -e "$TREE/p2.lock"; then echo "   server: present"; else echo "   server: ABSENT (the mount said the name exists, the served tree does not have it)"; fi
sleep 2
echo "   after 2 s: server $(test -e "$TREE/p2.lock" && echo present || echo absent)"
rm -f "$MNT/p2.lock" 2>/dev/null

echo "== P3: a stale <ref>.lock that survived a crash =="
printf x > "$TREE/.git/index.lock"
git -C "$TREE" checkout -q main 2>/dev/null
git -C "$TREE" branch -D poison-branch poison-branch2 >/dev/null 2>&1
echo "   planted on the served tree: $(test -e "$TREE/.git/index.lock" && echo yes)"
echo "   mount view : $(stat -c '%s bytes' "$MNT/.git/index.lock" 2>&1)"
echo "   served view: $(stat -c '%s bytes' "$TREE/.git/index.lock" 2>&1)"
if [ "$(stat -c '%s' "$MNT/.git/index.lock" 2>/dev/null)" = "$(stat -c '%s' "$TREE/.git/index.lock" 2>/dev/null)" ]; then
  echo "   VERDICT: the mount and the served tree AGREE about the stale lock (no lie)"
else
  echo "   VERDICT: THE MOUNT AND THE SERVED TREE DISAGREE about the stale lock"
fi
echo "   a git command that needs that lock, through the mount AND on the served tree:"
out=$(git -C "$MNT" -c user.name=p -c user.email=p@invalid checkout -q -b poison-branch 2>&1); echo "     mount  rc=$? ${out:0:90}"
out=$(git -C "$TREE" -c user.name=p -c user.email=p@invalid checkout -q -b native-a 2>&1); echo "     native rc=$? ${out:0:90}"
git -C "$TREE" checkout -q main 2>/dev/null; git -C "$TREE" branch -D native-a >/dev/null 2>&1
echo "   removing the stale lock THROUGH THE MOUNT:"
rm -f "$MNT/.git/index.lock" 2>/dev/null; echo "     rm rc=$?"
sleep 1
echo "   server now: $(test -e "$TREE/.git/index.lock" && echo STILL PRESENT || echo gone)"
echo "   the NEXT git command through the mount (a FRESH branch name, so the answer"
echo "   says whether the path is usable rather than whether the branch existed):"
out=$(git -C "$MNT" -c user.name=p -c user.email=p@invalid checkout -q -b poison-branch2 2>&1); echo "     rc=$? ${out:0:120}"
git -C "$TREE" checkout -q main 2>/dev/null
git -C "$TREE" branch -D poison-branch poison-branch2 >/dev/null 2>&1
echo "done"
