#!/usr/bin/env bash
# bfs020-gitlock.sh — the ref-lock shape git actually runs, with the mount's own
# trace beside it: `git checkout -b <name>` through the mount, which creates and
# closes `<ref>.lock` files (some of them EMPTY, for a ref the command only
# deletes) and renames them over their refs.
#
# A stale lock left by a crash is planted first, so the two behaviours are told
# apart: the stale file (git's own message, no mount involvement) and the locks
# this command creates itself.
#
# env: MNT, TREE, OUT
set -uo pipefail
MNT="${MNT:?}"; TREE="${TREE:?}"; OUT="${OUT:?}"
GIT="git -C $MNT -c user.name=probe -c user.email=probe@invalid"

echo "== G1: a stale ref lock on the served tree, then a lock-taking git command =="
printf x > "$TREE/.git/stale.lock"
echo "   planted: $(test -e "$TREE/.git/stale.lock" && echo yes)"
out=$($GIT checkout -q -b probe-a 2>&1); rc=$?
echo "   git checkout -b probe-a rc=$rc ${out:0:200}"
sleep 1
echo "   served tree locks now:"; ls -l "$TREE/.git" | grep -E '\.lock' | sed 's/^/     /' || echo "     (none)"

echo "== G2: the same with a FRESH branch (no stale lock involvement) =="
out=$($GIT checkout -q -b probe-b 2>&1); rc=$?
echo "   git checkout -b probe-b rc=$rc ${out:0:200}"
echo "   branch: $($GIT rev-parse --abbrev-ref HEAD 2>&1)"
sleep 1
echo "   served .git/index.lock: $(test -e "$TREE/.git/index.lock" && echo PRESENT || echo absent)"
echo "   served .git/AUTO_MERGE.lock: $(test -e "$TREE/.git/AUTO_MERGE.lock" && echo PRESENT || echo absent)"
echo "   server branches: $(git -C "$TREE" branch --list | tr '\n' ' ')"

echo "== G3: the write-lock-then-rename sequence git uses for a ref, by hand =="
printf 'ref: refs/heads/probe-b\n' > "$MNT/.git/probe-c.lock"
mv "$MNT/.git/probe-c.lock" "$MNT/.git/probe-c"
echo "   hand-written lock renamed rc=$?; server has probe-c: $(test -e "$TREE/.git/probe-c" && echo yes)"
cat "$TREE/.git/probe-c" | sed 's/^/     /'

sleep 1
echo "== MOUNT TRACE (the git locks this run touched) =="
grep -E 'BFS020-TRACE' "$OUT/mount.log" | grep -E 'lock|AUTO_MERGE|flush|publish-in|publish-out|rename' | sed 's/^/  | /' | tail -60
echo "== CONFLICTS =="
cat "$OUT/conflicts.jsonl" 2>/dev/null | sed 's/^/  | /' || echo "  | (none)"
