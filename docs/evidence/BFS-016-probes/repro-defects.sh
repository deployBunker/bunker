#!/usr/bin/env bash
# BFS-016 supplementary repros — MEASUREMENT ONLY, nothing is fixed here.
# All mount/server interactions are bounded by `timeout`; no pkill -f anywhere.
set -uo pipefail
M=${M:-/tmp/bfs016/mnt-r1}
T=${T:-/tmp/bfs016/tree}

hr() { printf '%s\n' "──────────────────────────────────────────────"; }
say() { printf '%s\n' "$*"; }

hr; say "A. THE WRITE->RENAME RACE (the .git/index.lock pattern)"; hr
mkdir -p "$M/rep2"
say "A1 write then rename IMMEDIATELY (no settle):"
printf 'payload\n' > "$M/rep2/e1.lock"; say "   create rc=$?"
mv "$M/rep2/e1.lock" "$M/rep2/e1.final" 2>&1 | sed 's/^/   /'
say "   rename-immediate rc=${PIPESTATUS[0]}"
say "   server after 2s: $(sleep 2; ls -l "$T/rep2/" | tr '\n' ' ')"
say "A2 write, SETTLE 2s, then rename:"
printf 'payload2\n' > "$M/rep2/e1b.lock"; say "   create rc=$?"
sleep 2
mv "$M/rep2/e1b.lock" "$M/rep2/e1b.final" 2>&1 | sed 's/^/   /'
say "   rename-after-2s rc=${PIPESTATUS[0]}"
say "   server: $(ls -l "$T/rep2/" | tr '\n' ' ')"
say "A3 the same shape INSIDE .git (what git does with index.lock -> index):"
printf 'lockimage\n' > "$M/.git/bfs016.lock"; say "   create rc=$?"
mv "$M/.git/bfs016.lock" "$M/.git/bfs016.out" 2>&1 | sed 's/^/   /'
say "   rename-immediate rc=${PIPESTATUS[0]}"
sleep 2
mv "$M/.git/bfs016.lock" "$M/.git/bfs016.out" 2>&1 | sed 's/^/   /'
say "   rename-after-2s rc=${PIPESTATUS[0]}"

hr; say "B. APPEND (>>) — the battery's op 10, and the explicit O_APPEND case"; hr
say "B1 shell append 'printf >>' on a SETTLED file:"
sh -c "printf 'second line\n' >> '$M/rep2/e1b.final'"; say "   rc=$?"
say "   server content: $(cat "$T/rep2/e1b.final" | tr '\n' '|')"
say "   mount  content: $(cat "$M/rep2/e1b.final" | tr '\n' '|')"
say "B2 explicit O_APPEND fd (exec 3>>) — no '>>' redirection shorthand:"
sh -c "exec 3>>'$M/rep2/e1b.final'; printf 'third line\n' >&3; exec 3>&-"; say "   rc=$?"
say "   server content: $(cat "$T/rep2/e1b.final" | tr '\n' '|')"
say "B3 a plain full-file rewrite still works (the control for B1):"
sh -c "printf 'rewritten\n' > '$M/rep2/ctl.txt'"; say "   rc=$?"; sleep 1
say "   server content: $(cat "$T/rep2/ctl.txt" 2>&1 | tr '\n' '|')"

hr; say "C. FRESH-ATTRIBUTE VIEW AFTER A WRITE (the 0-byte reading)"; hr
printf 'abcdefghij\n' > "$M/rep2/size.txt"; say "   wrote 11 bytes; immediate mount stat: $(stat -c '%s' "$M/rep2/size.txt" 2>&1)"
say "   immediate server stat: $(stat -c '%s' "$T/rep2/size.txt" 2>&1)"
sleep 2
say "   after settle  mount stat: $(stat -c '%s' "$M/rep2/size.txt" 2>&1)"

hr; say "D. THE BATTERY'S GIT WRITE SEQUENCE, from a CLEAN lock state"; hr
rm -f "$T/.git/index.lock"
say "D1 index.lock on server before: $(ls -la "$T/.git/index.lock" 2>&1 | tail -1)"
s=$(date +%s.%N); git -C "$M" status --short >/dev/null 2>&1; rc=$?; e=$(date +%s.%N)
say "   git status --short rc=$rc wall=$(awk -v a="$s" -v b="$e" 'BEGIN{printf "%.3f", b-a}')s lock-after=$(ls "$T/.git/index.lock" 2>/dev/null || echo none)"
s=$(date +%s.%N); git -C "$M" diff --stat HEAD >/dev/null 2>&1; rc=$?; e=$(date +%s.%N)
say "   git diff --stat HEAD rc=$rc wall=$(awk -v a="$s" -v b="$e" 'BEGIN{printf "%.3f", b-a}')s lock-after=$(ls "$T/.git/index.lock" 2>/dev/null || echo none)"
s=$(date +%s.%N); git -C "$M" checkout -b bfs016-repro >/dev/null 2>&1; rc=$?; e=$(date +%s.%N)
say "   git checkout -b rc=$rc wall=$(awk -v a="$s" -v b="$e" 'BEGIN{printf "%.3f", b-a}')s lock-after=$(ls "$T/.git/index.lock" 2>/dev/null || echo none)"
say "   lock on server AFTER checkout attempt: $(ls -la "$T/.git/index.lock" 2>&1 | tail -1)"
say "D2 now retry the same checkout with the lock removed and a settle:"
rm -f "$T/.git/index.lock"; sleep 1
git -C "$M" checkout -b bfs016-repro 2>&1 | sed 's/^/   /'; say "   rc=${PIPESTATUS[0]}"
say "   server lock after retry: $(ls "$T/.git/index.lock" 2>/dev/null || echo none)"
say "   branch through mount: $(git -C "$M" rev-parse --abbrev-ref HEAD 2>&1)"
say "D3 native control on the SAME tree (no mount in the path):"
rm -f "$T/.git/index.lock"
git -C "$T" status --short >/dev/null 2>&1; say "   native status rc=$? lock-after=$(ls "$T/.git/index.lock" 2>/dev/null || echo none)"
git -C "$T" diff --stat HEAD >/dev/null 2>&1; say "   native diff   rc=$? lock-after=$(ls "$T/.git/index.lock" 2>/dev/null || echo none)"
say "   native branches: $(git -C "$T" branch -a | tr '\n' ' ')"

hr; say "E. CLEANUP"; hr
rm -f "$T/.git/bfs016.lock" "$T/.git/bfs016.out" "$T/.git/t.tmp" "$T/.git/t.dup" "$T/.git/n1.tmp" "$T/.git/n2.tmp" 2>/dev/null
rm -f "$T/rep2/e1.lock" "$T/rep2/e1.final" "$T/rep2/e1b.lock" "$T/rep2/e1b.final" "$T/rep2/ctl.txt" "$T/rep2/size.txt" "$T/rep2/a.tmp" "$T/rep/append.txt" 2>/dev/null
git -C "$T" branch -D bfs016-repro >/dev/null 2>&1
rmdir "$T/rep" "$T/rep2" 2>/dev/null
say "done"
