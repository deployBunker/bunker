#!/usr/bin/env bash
# bfs020-content.sh — does a following operation on the SAME mount see the CONTENT
# of a name this mount just wrote and renamed over?  (The name-visibility half of
# BFS-020 is BFS-020-probes/red-repro.sh; this is the content half of the same
# claim, and it is the shape git's own `HEAD.lock -> HEAD` write has.)
#
# env: MNT, TREE
set -uo pipefail
MNT="${MNT:?}"; TREE="${TREE:?}"

echo "== V1: read a target, replace it by write+rename, read it again =="
echo "   server content before : $(cat "$TREE/.git/HEAD")"
echo "   mount  content before : $(cat "$MNT/.git/HEAD")   <- this read is what may populate a cache"
printf 'ref: refs/heads/measure-me\n' > "$MNT/.git/HEAD.lock"
mv "$MNT/.git/HEAD.lock" "$MNT/.git/HEAD"; echo "   write+rename rc=$?"
sleep 0.2
echo "   server content after  : $(cat "$TREE/.git/HEAD")"
echo "   mount  content after  : $(cat "$MNT/.git/HEAD")"
if [ "$(cat "$MNT/.git/HEAD")" = "$(cat "$TREE/.git/HEAD")" ]; then
  echo "   VERDICT: the mount agrees with the server (the rename's content is visible)"
else
  echo "   VERDICT: THE MOUNT SERVES STALE CONTENT for a name it just replaced itself"
fi
echo "   stat through the mount: $(stat -c 'size=%s mtime=%y' "$MNT/.git/HEAD" 2>&1)"

echo "== V2: the same for a plain file, and a fresh lookup of the new name =="
printf 'v1-content' > "$MNT/v.lock"
mv "$MNT/v.lock" "$MNT/v.txt"; echo "   write+rename rc=$?"
echo "   mount  : $(cat "$MNT/v.txt" 2>&1)"
echo "   server : $(cat "$TREE/v.txt" 2>&1)"
printf 'v2-content-longer' > "$MNT/v.lock"
mv "$MNT/v.lock" "$MNT/v.txt"; echo "   second write+rename rc=$?"
echo "   mount  : $(cat "$MNT/v.txt" 2>&1)"
echo "   server : $(cat "$TREE/v.txt" 2>&1)"
if [ "$(cat "$MNT/v.txt")" = "$(cat "$TREE/v.txt")" ]; then
  echo "   VERDICT: agrees after the second replacement too"
else
  echo "   VERDICT: THE MOUNT SERVES STALE CONTENT after the second replacement"
fi
rm -f "$MNT/v.txt" "$TREE/v.txt" 2>/dev/null
git -C "$TREE" checkout -q main 2>/dev/null
echo done
