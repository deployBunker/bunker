#!/usr/bin/env bash
# bfs020-passA-steps.sh — run PASS A of the committed git block one op at a time,
# counting the served tree's `.git` entries after EVERY one, then the cleanup
# steps. It exists because the block's own run emptied the served `.git` directory
# once (docs/evidence/BFS-020-gitblock.txt, PASS B), and a destructive delete
# through the mount is a P0-shaped claim that must be attributed rather than
# inferred.
#
# env: MNT, TREE
set -uo pipefail
MNT="${MNT:?}"; TREE="${TREE:?}"
BRANCH="bunker/attrs-$(date +%H%M%S)"
GIT="git -C $MNT -c user.name=battery -c user.email=battery@invalid"

count() { find "$TREE/.git" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l | tr -d ' '; }
step() {
  local label="$1"; shift
  local out rc
  out=$("$@" 2>&1); rc=$?
  printf '   %-30s rc=%-4s .git=%-4s %s\n' "$label" "$rc" "$(count)" "$(printf '%s' "$out" | head -c 80 | tr '\n' ' ')"
}
echo "   baseline .git entries: $(count)"
step "rev-parse HEAD"    $GIT rev-parse HEAD
step "log -1"            $GIT log -1 --format=%h
step "status --short"    $GIT status --short
step "checkout -b"       $GIT checkout -q -b "$BRANCH"
printf 'battery %s\n' "$(date -Is)" > "$MNT/.battery-probe.txt"
step "add one file"      $GIT add .battery-probe.txt
step "commit"            $GIT commit -q -m "battery: git over bunker-fs"
step "commit --amend"    $GIT commit -q --amend --no-edit
step "rebase HEAD~1"     $GIT rebase HEAD~1
step "symbolic-ref"      $GIT rev-parse --abbrev-ref HEAD
echo "   --- now the cleanup steps ---"
step "rebase --abort (mount)"  $GIT rebase --abort
step "checkout main (mount)"   $GIT checkout -q main
echo "   server .git entries: $(count)"
step "server checkout main"    git -C "$TREE" checkout -q main
step "server reset --hard"     git -C "$TREE" reset -q --hard
step "server branch -D"        git -C "$TREE" branch -D "$BRANCH"
echo "   final .git entries: $(count)"
ls -la "$TREE/.git" | head -6 | sed 's/^/     /'
echo done
