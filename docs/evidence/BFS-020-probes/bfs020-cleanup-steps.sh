#!/usr/bin/env bash
# bfs020-cleanup-steps.sh — WHICH step of the between-passes cleanup empties the
# served tree's `.git`? It runs the same steps one at a time and counts the served
# tree's `.git` entries after every one, so the destructive step (if any) is named
# rather than inferred.
#
# env: MNT, TREE
set -uo pipefail
MNT="${MNT:?}"; TREE="${TREE:?}"
GIT="git -C $MNT -c user.name=probe -c user.email=probe@invalid"

count() { find "$TREE/.git" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l | tr -d ' '; }
step() { # step LABEL the command
  local label="$1"; shift
  local out rc
  out=$("$@" 2>&1); rc=$?
  printf '   %-34s rc=%-4s .git entries=%-4s %s\n' "$label" "$rc" "$(count)" "$(printf '%s' "$out" | head -c 70 | tr '\n' ' ')"
}

echo "   baseline .git entries: $(count)"
echo "== make a muddy state first (the shape the cleanup was written for) =="
step "checkout -b probe" $GIT checkout -q -b probe-x
printf 'battery probe\n' > "$MNT/.battery-probe.txt"
step "add" $GIT add .battery-probe.txt
step "commit" $GIT commit -q -m "probe commit"
echo "   .git entries after the commit attempt: $(count)"

echo "== the cleanup steps, one at a time =="
step "rebase --abort (through the mount)" $GIT rebase --abort
step "checkout main (through the mount)" $GIT checkout -q main
step "server: checkout main" git -C "$TREE" checkout -q main
step "server: reset --hard" git -C "$TREE" reset -q --hard
step "server: branch -D probe-x" git -C "$TREE" branch -D probe-x
rm -f "$MNT/.battery-probe.txt" "$TREE/.battery-probe.txt" 2>/dev/null
printf '   %-34s rc=%-4s .git entries=%s\n' "rm the probe file" "$?" "$(count)"
sleep 1
echo "   .git entries at the end: $(count)"
echo "   server .git listing:"; ls -la "$TREE/.git" | head -8 | sed 's/^/     /'
echo done
