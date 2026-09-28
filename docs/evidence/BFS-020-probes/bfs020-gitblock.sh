#!/usr/bin/env bash
# bfs020-gitblock.sh — the committed battery's git block, VERBATIM (probes/
# bunker-fs-battery.sh section 4), run through a fresh mount, with the per-op rc
# printed. It exists so the block's result can be read against the one thing that
# is NOT this row's defect, in two named passes:
#
#   pass A  the block exactly as the battery runs it
#   pass B  the same block with `.git/COMMIT_EDITMSG` removed before each
#           commit-shaped op — the ONE precondition of a DIFFERENT, pre-existing
#           refusal (an in-place rewrite of an existing file: BFS-012 clause 2 /
#           BFS-030) whose refusal is measured identically on the unfixed tree
#           (docs/evidence/BFS-020-rewrite-both.txt, R1)
#
# env: MNT, TREE, BIN, URL
set -uo pipefail
MNT="${MNT:?}"; TREE="${TREE:?}"; OUT="${OUT:?}"
BIN="${BIN:-unset}"; URL="${URL:-unset}"
BRANCH="bunker/probe-$(date +%Y%m%d%H%M%S)"
GIT="git -C $MNT -c user.name=battery -c user.email=battery@invalid"

run() { # run NAME the command (in our own subshell so $? is the op's)
  local name="$1"; shift
  local out rc
  out=$("$@" 2>&1); rc=$?
  printf '   %-24s rc=%-4s %s\n' "$name" "$rc" "$(printf '%s' "$out" | head -c 90 | tr '\n' ' ')"
  echo "$rc" >> "$OUT/rcs.txt"
}

: > "${OUT:?}/rcs.txt"

echo "== PASS A: the committed block, exactly as the battery runs it =="
run "rev-parse HEAD"     $GIT rev-parse HEAD
run "log -1"             $GIT log -1 --format=%h
run "log --oneline -20"  $GIT log --oneline -20
run "ls-files"           $GIT ls-files
run "diff --stat HEAD"   $GIT diff --stat HEAD
run "status --short"     $GIT status --short
run "status --porcelain" $GIT status --porcelain -uno
run "checkout -b"        $GIT checkout -q -b "$BRANCH"
printf 'battery %s\n' "$(date -Is)" > "$MNT/.battery-probe.txt"
run "add one file"       $GIT add .battery-probe.txt
run "commit"             $GIT commit -q -m "battery: git over bunker-fs"
run "commit --amend"     $GIT commit -q --amend --no-edit
run "rebase HEAD~1"      $GIT rebase HEAD~1
run "symbolic-ref"       $GIT rev-parse --abbrev-ref HEAD
AOK="$(grep -c '^0$' "$OUT/rcs.txt")"
echo "   PASS A: $AOK/13 rc=0"

# Back to a clean state for pass B.
$GIT rebase --abort >/dev/null 2>&1
$GIT checkout -q main >/dev/null 2>&1
git -C "$TREE" checkout -q main >/dev/null 2>&1
git -C "$TREE" reset -q --hard >/dev/null 2>&1
git -C "$TREE" branch -D "$BRANCH" >/dev/null 2>&1
rm -f "$MNT/.battery-probe.txt" "$TREE/.battery-probe.txt" 2>/dev/null
sleep 1

echo "== PASS B: the same block, with the OTHER row's precondition cleared =="
echo "   (removing .git/COMMIT_EDITMSG before each commit-shaped op: an in-place"
echo "    rewrite of an EXISTING file is refused by BFS-012/BFS-030, on the unfixed"
echo "    tree and on this one alike — it is not BFS-020's defect and not its fix)"
BRANCH2="bunker/probe2-$(date +%Y%m%d%H%M%S)"
GIT="git -C $MNT -c user.name=battery -c user.email=battery@invalid"
: > "$OUT/rcs.txt"
run "rev-parse HEAD"     $GIT rev-parse HEAD
run "log -1"             $GIT log -1 --format=%h
run "log --oneline -20"  $GIT log --oneline -20
run "ls-files"           $GIT ls-files
run "diff --stat HEAD"   $GIT diff --stat HEAD
run "status --short"     $GIT status --short
run "status --porcelain" $GIT status --porcelain -uno
run "checkout -b"        $GIT checkout -q -b "$BRANCH2"
rm -f "$MNT/.battery-probe.txt"
printf 'battery %s\n' "$(date -Is)" > "$MNT/.battery-probe.txt"
run "add one file"       $GIT add .battery-probe.txt
rm -f "$MNT/.git/COMMIT_EDITMSG"
run "commit"             $GIT commit -q -m "battery: git over bunker-fs"
rm -f "$MNT/.git/COMMIT_EDITMSG"
run "commit --amend"     $GIT commit -q --amend --no-edit
run "rebase HEAD~1"      $GIT rebase HEAD~1
run "symbolic-ref"       $GIT rev-parse --abbrev-ref HEAD
BOK="$(grep -c '^0$' "$OUT/rcs.txt")"
echo "   PASS B: $BOK/13 rc=0"

echo "== cleanup + the served tree's view =="
$GIT rebase --abort >/dev/null 2>&1
$GIT checkout -q main >/dev/null 2>&1
git -C "$TREE" checkout -q main >/dev/null 2>&1
git -C "$TREE" reset -q --hard >/dev/null 2>&1
git -C "$TREE" branch -D "$BRANCH" >/dev/null 2>&1
git -C "$TREE" branch -D "$BRANCH2" >/dev/null 2>&1
rm -f "$MNT/.battery-probe.txt" "$TREE/.battery-probe.txt" 2>/dev/null
sleep 1
echo "   server locks left: $(ls "$TREE/.git" | grep -c '\.lock$' || true) (.git/*.lock)"
echo "   server branches: $(git -C "$TREE" branch --list | tr '\n' ' ')"
echo "   mount branches : $($GIT branch --list | tr '\n' ' ')"
echo "PASS A ok=$AOK/13 ; PASS B ok=$BOK/13"
