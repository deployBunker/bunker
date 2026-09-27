#!/usr/bin/env bash
# write-then-read.sh — the regression arm next to the fix: the read-bound rule must
# not refuse a read of bytes the mount itself just wrote.
#
# A file created through the mount replies attrs (size 0), the kernel grows i_size
# on each acknowledged write, and the read that follows must be judged against the
# grown size — not against the size the file had when the path was first resolved.
# If the fix gets that wrong, every write followed by a read through the mount
# starts failing, so this arm is run on both binaries.
#
# env (set by mount-arm.sh): MNT TREE OUT
set -uo pipefail

BODY="HELLO-WRITTEN-THROUGH-THE-MOUNT"
echo "== write, then read, through the mount =="
printf '%s' "$BODY" > "$MNT/created.txt" 2>"$OUT/write.err"
WRC=$?
echo "write              : rc=$WRC $(cat "$OUT/write.err")"
echo "server side        : size=$(stat -c %s "$TREE/created.txt" 2>/dev/null || echo missing)"
cat "$MNT/created.txt" > "$OUT/created.read" 2>"$OUT/read.err"
RRC=$?
GOT=$(cat "$OUT/created.read")
echo "read back (cat)    : rc=$RRC '${GOT}'  (${#GOT} chars) $(cat "$OUT/read.err")"
if [ "$GOT" = "$BODY" ] && [ "$WRC" -eq 0 ]; then
  echo "WRITE-READ VERDICT: OK — the read of our own write was served in full"
  exit 0
fi
echo "WRITE-READ VERDICT: FAIL — write rc=$WRC, read rc=$RRC returned ${#GOT} of ${#BODY} chars"
exit 1
