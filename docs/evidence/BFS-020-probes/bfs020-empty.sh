#!/usr/bin/env bash
# bfs020-empty.sh — the ZERO-BYTE create: `: > f` (open O_CREAT|O_TRUNC, no write
# at all) through the mount. It is the same defect one shape further in: if no
# chunk ever arrives, does the name get published at all?
#
# env: MNT, TREE
set -uo pipefail
MNT="${MNT:?MNT required}"
TREE="${TREE:?TREE required}"

echo "== E1: ': > f' (create + close, ZERO bytes), then rename immediately =="
( : > "$MNT/e1.lock" ); echo "   create rc=$?"
mv "$MNT/e1.lock" "$MNT/e1.final" 2>&1 | sed 's/^/   /'; echo "   rename-immediate rc=${PIPESTATUS[0]}"
sleep 2
echo "   served tree after 2 s: $(test -e "$TREE/e1.lock" && echo 'e1.lock present' || echo 'e1.lock ABSENT (never published)')"
mv "$MNT/e1.lock" "$MNT/e1.final" 2>&1 | sed 's/^/   /'; echo "   rename-after-2s rc=${PIPESTATUS[0]}"
sleep 1
echo "   served tree: $(test -e "$TREE/e1.final" && echo 'e1.final present' || echo 'e1.final ABSENT')"
echo "   the name the MOUNT claims: $(stat -c '%s bytes' "$MNT/e1.lock" 2>&1)"

echo "== E2: the same, one command: printf '' > f =="
printf '' > "$MNT/e2.lock"; echo "   create rc=$?"
mv "$MNT/e2.lock" "$MNT/e2.final" 2>&1 | sed 's/^/   /'; echo "   rename rc=${PIPESTATUS[0]}"
sleep 2
echo "   served tree: $(test -e "$TREE/e2.lock" && echo 'e2.lock present' || echo 'e2.lock ABSENT') $(test -e "$TREE/e2.final" && echo 'e2.final present' || echo 'e2.final ABSENT')"
echo "== E3: touch (create, no write, no truncate) =="
touch "$MNT/e3.lock"; echo "   touch rc=$?"
sleep 2
echo "   served tree: $(test -e "$TREE/e3.lock" && echo present || echo ABSENT)"
rm -f "$MNT/e1.lock" "$MNT/e2.lock" "$MNT/e3.lock" 2>/dev/null
echo "done"
