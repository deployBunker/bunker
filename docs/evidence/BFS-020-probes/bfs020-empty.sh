#!/usr/bin/env bash
# bfs020-empty.sh — the ZERO-BYTE create: `: > f`, `printf '' > f` and `touch f`
# all arrive as a create with NO WRITE at all. It is the same defect one shape
# further in, and it does NOT have a window: if nothing publishes the empty body,
# the name never appears on the served tree at all.
#
# Each shape reports, in order:
#   the create's rc; whether the name is on the SERVED TREE right after the close;
#   the immediate rename's rc; which name holds which content afterwards; and what
#   the MOUNT says about the source name afterwards.
#
# env: MNT, TREE
set -uo pipefail
MNT="${MNT:?MNT required}"
TREE="${TREE:?TREE required}"

shape() { # shape LABEL COMMAND...
  local label="$1"; shift
  echo "== $label =="
  "$@"; echo "   create rc=$?"
  if [ -e "$TREE/$label.lock" ]; then
    echo "   served tree right after the close : $label.lock PRESENT ($(stat -c '%s' "$TREE/$label.lock") bytes)"
  else
    echo "   served tree right after the close : $label.lock ABSENT (nothing was published)"
  fi
  mv "$MNT/$label.lock" "$MNT/$label.final" 2>&1 | sed 's/^/   /'
  echo "   rename immediately rc=${PIPESTATUS[0]}"
  sleep 0.5
  echo "   served tree: $(test -e "$TREE/$label.lock" && echo -n "$label.lock PRESENT " || echo -n "$label.lock ABSENT ")"\
"$(test -e "$TREE/$label.final" && echo "$label.final PRESENT ($(stat -c '%s' "$TREE/$label.final") bytes)" || echo "$label.final ABSENT")"
  echo "   the mount's view of the source name: $(stat -c '%s bytes' "$MNT/$label.lock" 2>&1)"
  rm -f "$MNT/$label.lock" "$TREE/$label.lock" "$TREE/$label.final" 2>/dev/null
}

shape e1 sh -c ": > \"\$1\"" _ "$MNT/e1.lock"
shape e2 sh -c "printf '' > \"\$1\"" _ "$MNT/e2.lock"
shape e3 touch "$MNT/e3.lock"
echo done
