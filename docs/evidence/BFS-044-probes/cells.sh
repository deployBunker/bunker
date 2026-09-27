#!/usr/bin/env bash
# cells.sh — the READ/WRITE CORRECTNESS CELLS, run against a live mount.
#
# This is the reader BFS-044's hardest requirement needs: run the same cells with
# every invalidation and hot-file FEATURE at its most degraded legal value, and
# require the BYTES to be unchanged. Correctness cannot be a function of a knob,
# so the cells do not know which arm they are in; the only thing an arm chooses is
# whether the propagation cell EXPECTS a change to be visible, and that is a
# statement about the channel's cadence rather than about correctness (the arm
# that declares `delayed` is what proves the cell can fail at all).
#
# Every cell prints PASS/FAIL and the exit code is the arm's verdict, so the
# evidence is a number rather than a transcript somebody has to read.
#
# THE WRITE SHAPES ARE THE ONES THIS SURFACE SUPPORTS, and the refusal is a cell
# of its own. Measured (BFS-030, re-measured here): a NEW path can be created, a
# large file can be streamed into it, and it can be unlinked; an overwrite of an
# existing file (`>`, open O_TRUNC) is REFUSED with EOPNOTSUPP by BFS-030's rule
# and must leave the original byte-identical. Cells for shapes the surface
# refuses would measure the refusal, not correctness, so the refusal is asserted
# once, explicitly, as the control that proves the write half is not vacuous.
#
# usage: cells.sh [--mnt DIR] [--tree DIR] [--out DIR] [--files N] \
#                 [--propagation visible|delayed] [--propagation-wait-s N] \
#                 [--publish-wait-s N]
set -uo pipefail

# The mount arm exports MNT/TREE/OUT/CDIR into this reader's environment, so the
# defaults come from there and the flags are only an override.
MNT="${MNT:-}"; TREE="${TREE:-}"; OUT="${OUT:-}"
FILES=0; PROP=visible; WAIT=8; PUBWAIT=15
while [ $# -gt 0 ]; do
  case "$1" in
    --mnt) MNT="$2"; shift 2;;
    --tree) TREE="$2"; shift 2;;
    --out) OUT="$2"; shift 2;;
    --files) FILES="$2"; shift 2;;
    --propagation) PROP="$2"; shift 2;;
    --propagation-wait-s) WAIT="$2"; shift 2;;
    --publish-wait-s) PUBWAIT="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$MNT" ] && [ -n "$TREE" ] || { echo "missing --mnt/--tree" >&2; exit 2; }
[ -n "$OUT" ] || OUT="."

FAILED=0
pass() { echo "  CELL $1: PASS  $2"; }
fail() { echo "  CELL $1: FAIL  $2"; FAILED=1; }

LIST=$(find "$TREE" -maxdepth 1 -type f -name 'f*.txt' | sort)
[ -n "$LIST" ] || { echo "fixture tree $TREE has no f*.txt files" >&2; exit 2; }
N=0; for f in $LIST; do N=$((N+1)); done
[ "$FILES" != 0 ] && [ "$FILES" -lt "$N" ] && N="$FILES"
LIST=$(printf '%s\n' "$LIST" | head -n "$N")
echo "  cells over $N file(s) in $TREE (mnt=$MNT)"

# ---- CELL 1: every file read through the mount is byte-identical -------------
bad=0; checked=0
for f in $LIST; do
  b=$(basename "$f")
  if cmp -s "$MNT/$b" "$f"; then checked=$((checked+1)); else bad=$((bad+1)); fi
done
if [ "$bad" = 0 ]; then pass read-cold "$checked/$checked files byte-identical"; else fail read-cold "$bad of $((bad+checked)) differ"; fi

# ---- CELL 2: the same read again (the cache path) ---------------------------
bad=0; checked=0
for f in $LIST; do
  b=$(basename "$f")
  if cmp -s "$MNT/$b" "$f"; then checked=$((checked+1)); else bad=$((bad+1)); fi
done
if [ "$bad" = 0 ]; then pass read-warm "$checked/$checked files byte-identical on the second read"; else fail read-warm "$bad of $((bad+checked)) differ"; fi

# ---- CELL 3: the walk — same population, same sizes -------------------------
MW=$(find "$MNT" -maxdepth 1 -type f -name 'f*.txt' | wc -l | tr -d ' ')
TW=$(find "$TREE" -maxdepth 1 -type f -name 'f*.txt' | wc -l | tr -d ' ')
SZBAD=0
for f in $LIST; do
  b=$(basename "$f")
  a=$(stat -c %s "$MNT/$b" 2>/dev/null); c=$(stat -c %s "$f")
  [ "$a" = "$c" ] || SZBAD=$((SZBAD+1))
done
if [ "$MW" = "$TW" ] && [ "$SZBAD" = 0 ]; then
  pass walk "find sees $MW/$TW and every size agrees"
else
  fail walk "find sees $MW of $TW, $SZBAD size mismatches"
fi

# ---- CELL 4: an out-of-band change reaches the reader ------------------------
# The change is made in the SERVED tree, so nothing local can explain it: the
# client must find out from the invalidation channel (or from a cold read).
PROBE=f001.txt
NEW="EDITED-OUT-OF-BAND-$(date +%s%N)"
printf '%s\n' "$NEW" > "$TREE/$PROBE"
t0=$(date +%s%N); seen=0
end=$(( $(date +%s) + WAIT ))
while [ "$(date +%s)" -lt "$end" ]; do
  if [ "$(cat "$MNT/$PROBE" 2>/dev/null)" = "$NEW" ]; then seen=1; break; fi
  sleep 0.25
done
t1=$(date +%s%N); ms=$(( (t1 - t0) / 1000000 ))
if [ "$seen" = 1 ]; then
  if [ "$PROP" = delayed ]; then
    fail propagation "the change became visible in ${ms}ms although this arm declared the channel too slow to see it — the cell is falsifiable and it went RED where the arm said it would"
  else
    pass propagation "the out-of-band edit was observed in ${ms}ms"
  fi
else
  if [ "$PROP" = delayed ]; then
    echo "  CELL propagation: PASS(as declared)  the change was NOT visible within ${WAIT}s — the declared cadence, not correctness"
  else
    fail propagation "the out-of-band edit was never observed within ${WAIT}s"
  fi
fi

# ---- CELL 5: a file created through the mount lands on both views -----------
# The publish is asynchronous with respect to close(2) (the FUSE release runs
# after the syscall returns), so the cell POLLS for the served tree and reports
# the latency it measured rather than racing it. An instant check would be a
# flaky cell, not a stricter one.
W="bfs044-written.txt"
CREATED="CREATED-THROUGH-THE-MOUNT-$(date +%s%N)"
t0=$(date +%s%N)
if printf '%s\n' "$CREATED" > "$MNT/$W" 2>"$OUT/create.err"; then open_rc=0; else open_rc=1; fi
seen=0; end=$(( $(date +%s) + PUBWAIT ))
while [ "$(date +%s)" -lt "$end" ]; do
  if [ "$(cat "$TREE/$W" 2>/dev/null)" = "$CREATED" ]; then seen=1; break; fi
  sleep 0.25
done
t1=$(date +%s%N); pubms=$(( (t1 - t0) / 1000000 ))
if [ "$open_rc" = 0 ] && [ "$seen" = 1 ]; then
  pass write-create "the new file is byte-identical in the served tree and through the mount (published within ${pubms}ms)"
else
  fail write-create "open_rc=$open_rc published=$seen served=[$(cat "$TREE/$W" 2>/dev/null)] mount=[$(cat "$MNT/$W" 2>/dev/null)] err=[$(cat "$OUT/create.err" 2>/dev/null)]"
fi

# ---- CELL 6: the REFUSED overwrite is refused AND non-destructive -----------
# The control that proves the write half is not vacuous: this surface cannot
# complete an in-place rewrite, so it must refuse it loudly (EOPNOTSUPP) and leave
# the original bytes alone (BFS-030's rule, re-measured here rather than cited).
ORIG=$(sha256sum "$TREE/f002.txt" | cut -d' ' -f1)
# The subshell + its own stderr redirect keep bash's own "redirection failed"
# message out of the transcript: the refusal IS the expected result here, and a
# transcript that looks like it hit an error is a transcript nobody trusts.
if ( printf 'OVERWRITE-ATTEMPT\n' > "$MNT/f002.txt" ) 2>/dev/null; then ow_rc=0; else ow_rc=1; fi
NOW=$(sha256sum "$TREE/f002.txt" | cut -d' ' -f1)
if [ "$ow_rc" != 0 ] && [ "$ORIG" = "$NOW" ]; then
  pass write-refused-overwrite "the O_TRUNC overwrite was refused and f002.txt is still $ORIG"
else
  fail write-refused-overwrite "overwrite rc=$ow_rc and the hash went $ORIG -> $NOW"
fi

# ---- CELL 7: a LARGE file streamed through the mount ------------------------
# The shape an editor's writeback or a build tool actually uses: create a new
# path and stream 256 KiB into it. Both views must agree by content hash.
BIG="bfs044-large.bin"
BYTES=262144
head -c "$BYTES" /dev/urandom > "$MNT/$BIG" 2>>"$OUT/create.err"
seen=0; end=$(( $(date +%s) + PUBWAIT ))
want=""
while [ "$(date +%s)" -lt "$end" ]; do
  if [ -f "$TREE/$BIG" ] && [ "$(stat -c %s "$TREE/$BIG")" = "$BYTES" ]; then
    sleep 0.5   # let the last chunk land before hashing
    want=$(sha256sum "$TREE/$BIG" 2>/dev/null | cut -d' ' -f1)
    got=$(sha256sum "$MNT/$BIG" 2>/dev/null | cut -d' ' -f1)
    [ -n "$want" ] && [ "$want" = "$got" ] && { seen=1; break; }
  fi
  sleep 0.25
done
if [ "$seen" = 1 ]; then
  pass write-large "$BYTES B created through the mount, sha256 $want on both views"
else
  fail write-large "served size=$(stat -c %s "$TREE/$BIG" 2>/dev/null) served_hash=$want mount_hash=$(sha256sum "$MNT/$BIG" 2>/dev/null | cut -d' ' -f1)"
fi

# ---- CELL 8: the read-back agrees, and the unlink lands ---------------------
if cmp -s "$MNT/$W" "$TREE/$W"; then
  pass read-after-write "cmp agrees on the created file"
else
  fail read-after-write "the two views disagree on the created file"
fi
if rm -f "$MNT/$W" 2>/dev/null; then
  if [ -f "$TREE/$W" ]; then
    # the unlink may publish after close(2) returns, exactly as the create does
    end=$(( $(date +%s) + PUBWAIT )); gone=0
    while [ "$(date +%s)" -lt "$end" ]; do
      [ -f "$TREE/$W" ] || { gone=1; break; }
      sleep 0.25
    done
    if [ "$gone" = 1 ]; then pass unlink "the unlink reached the served tree"; else fail unlink "the file is still in the served tree after ${PUBWAIT}s"; fi
  else
    pass unlink "the unlink reached the served tree immediately"
  fi
else
  fail unlink "rm through the mount failed"
fi

# ---- CELL 9: the cache census against the bounds the status reports ---------
if [ -n "${CDIR:-}" ] && [ -f "$CDIR/status.json" ]; then
  sleep 2.5  # let the status loop publish the last figures
  num() { sed -n "s/.*\"$1\": \([0-9]*\).*/\1/p" "$CDIR/status.json" | head -1; }
  entries=$(num entries); maxentries=$(num max_entries)
  used=$(num used_bytes); maxbytes=$(num max_bytes); bypass=$(num bypass_events)
  echo "  CELL cache-census: entries=$entries max_entries=$maxentries used_bytes=$used max_bytes=$maxbytes bypass_events=$bypass"
  if [ -n "$maxentries" ] && [ -n "$entries" ] && [ "$entries" -gt "$maxentries" ]; then
    fail cache-census "the cache holds $entries entries at an entry bound of $maxentries"
  else
    pass cache-census "the entry census is inside the entry bound the status reports"
  fi
else
  echo "  CELL cache-census: SKIPPED (no CDIR/status.json in this arm)"
fi

echo "  cells verdict: $( [ "$FAILED" = 0 ] && echo PASS || echo FAIL )"
exit $FAILED
