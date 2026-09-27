#!/usr/bin/env bash
# driver-repro.sh — BFS-025's reproduction, in the driver's own shape, run under
# docs/evidence/BFS-012-probes/mount-arm.sh (which provides MNT/TREE/CDIR/OUT and
# owns every process it starts).
#
#   server side          : 'SHORT' (5 bytes)
#   mount stat (no read) : size=5     <- this alone is what the kernel bounds a read by
#   agent now            : 82 bytes of new content
#   mount stat (post)    : size=5     <- still the stale size
#   mount READ           : 'X-REP'    <- 5 chars, rc=0     *** TRUNCATED ***
#   native truth         : 82 bytes
#
# Both reader families are exercised, because the defect belongs to a READER: a
# reader that goes through the kernel's splice path (uutils cat) is clamped at
# i_size; a reader that calls read(2) is not. Reporting one number for "the read"
# would hide exactly the fact this row is about.
set -uo pipefail

NEW="X-REPLACEMENT-CONTENT-0123456789-abcdefghijklmnopqrstuvwxyz-ABCDEFGHIJKLMNOPQRSTUV"
F="$MNT/target.txt"
SRC="$TREE/target.txt"

echo "host               : $(uname -n) loadavg $(cut -d' ' -f1-3 /proc/loadavg)"
echo "mount              : $MNT   tree: $TREE"

printf 'SHORT' > "$SRC"
echo "server side        : '$(cat "$SRC")' ($(stat -c %s "$SRC") bytes)"
echo "mount stat (no read): size=$(timeout 30 stat -c %s "$F" 2>&1)"
printf '%s' "$NEW" > "$SRC"
echo "agent now          : size=$(stat -c %s "$SRC") sha256=$(sha256sum "$SRC" | cut -c1-16)…"
echo "mount stat (post)  : size=$(timeout 30 stat -c %s "$F" 2>&1)"

echo "--- the reads ---"
GOT=$(timeout 60 cat "$F" 2>"$OUT/cat.err"); RC=$?
echo "cat (splice)       : '${GOT}'  (${#GOT} chars) rc=$RC  stderr: $(tr -d '\n' < "$OUT/cat.err")"
python3 - "$F" "$SRC" <<'PY'
import os, sys
mnt, src = sys.argv[1], sys.argv[2]
truth = open(src, "rb").read()
got = open(mnt, "rb").read()
print("python read()      : %d bytes %s" % (len(got), "OK" if got == truth else "*** SHORT (truth %d) ***" % len(truth)))
PY
echo "native truth       : $(stat -c %s "$SRC") bytes"

echo "--- the verdict ---"
if [ "$RC" -ne 0 ]; then
  echo "READER VERDICT: LOUD — cat failed (rc=$RC) instead of returning a fragment"
elif [ "${#GOT}" -eq "$(stat -c %s "$SRC")" ]; then
  echo "READER VERDICT: COMPLETE — cat returned every byte the resource has"
else
  echo "READER VERDICT: SILENT TRUNCATION — cat returned ${#GOT} of $(stat -c %s "$SRC") bytes with rc=0"
fi
