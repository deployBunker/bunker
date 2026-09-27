#!/usr/bin/env bash
# BFS-038 mutation battery — every cell's negative control, as a source mutation.
#
# For each group: sha256 the file, back it up, apply the group's EXACT replacements
# (each refusing unless its anchor appears exactly once), build, run the named cell
# (it MUST fail), restore from the backup, sha256-verify the restore, and run the
# cell again (it MUST pass). "NON-VACUITY: PROVEN" is printed only when the
# mutation turned the cell red AND the restore turned it green.
#
# Why bash+perl and not a Python helper: .gitignore forbids *.py outside tools/,
# and a probe that cannot be committed cannot be re-run by anyone else.
#
# Usage: bash mutation-red.sh [worktree-root]        (default: this worktree)
set -uo pipefail

WT=${1:-/home/kara/worktrees/bunker-BFS-038}
SRC="$WT/internal/fsclient/cache.go"
BAK=$(mktemp -d /tmp/bfs038-mutation-XXXXXX)
PKG=./internal/fsclient/

# exact_replace OLD NEW — replace OLD with NEW in $SRC, refusing unless OLD occurs
# exactly once. The file is read whole, checked, and only then written, so a
# refused anchor can never leave a half-edited source behind.
exact_replace() {
  OLD="$1" NEW="$2" perl -e '
    my ($path) = @ARGV;
    my $o = $ENV{OLD};
    my $n = $ENV{NEW};
    open my $in, "<", $path or die "open $path: $!";
    local $/;
    my $src = <$in>;
    close $in;
    my $count = () = ($src =~ /\Q$o\E/g);
    die "ANCHOR NOT UNIQUE ($count occurrences) — refusing to mutate\n" unless $count == 1;
    $src =~ s/\Q$o\E/$n/ or die "ANCHOR NOT REPLACED\n";
    open my $out, ">", $path or die "write $path: $!";
    print $out $src;
    close $out;
    print "anchor found exactly once; replacement applied\n";
  ' "$SRC"
}

# ── the mutations ────────────────────────────────────────────────────────────
# $'...' carries the tabs and newlines verbatim. Each pair is one group.

# M1 — the accounting split (F-1): bound the state we CHOSE to count.
M1_OLD=$'func (c *Cache) reservedLocked() int64 {\n\treturn c.usedLocked() + c.stagedReservedLocked()\n}\n'
M1_NEW=$'func (c *Cache) reservedLocked() int64 {\n\t// MUTATION (BFS-038 control 2, reverted immediately): bound the published\n\t// figure only - the state the cache chose to count.\n\treturn c.usedLocked()\n}\n'

# M2 — the refcount: a read takes the bytes and the reference on them in one step,
# or it takes no reference at all.
M2_OLD=$'\te.LastHitMS = c.cfg.Now().UnixMilli()\n\tc.stats.Hits++\n\tif b := c.blobs[e.Hash]; b != nil {\n\t\tb.Pins++\n\t}\n\treturn data, true\n}\n'
M2_NEW=$'\te.LastHitMS = c.cfg.Now().UnixMilli()\n\tc.stats.Hits++\n\t// MUTATION: the reader takes no reference on the blob it is reading.\n\treturn data, true\n}\n'

# M3 — the entry dimension of the bound (BFS-031's lesson).
M3_OLD=$'\tfor c.usedLocked()+newBlobBytes+indexGrowth > c.cfg.MaxBytes ||\n\t\tlen(c.entries)+newEntries > c.cfg.MaxEntries {\n'
M3_NEW=$'\t// MUTATION: the entry dimension of the bound is gone.\n\tfor c.usedLocked()+newBlobBytes+indexGrowth > c.cfg.MaxBytes {\n'

# M4 — abandonment discards its blob. Drop the discard.
M4_OLD=$'\t_ = os.Remove(s.tmpName)\n\tc := s.c\n\tc.mu.Lock()\n'
M4_NEW=$'\t// MUTATION: the abandoned refresh leaves its blob behind.\n\tc := s.c\n\tc.mu.Lock()\n'

# M5 — the representation itself, applied to the SHIPPED code: publish the pointer
# at Stage time and stream the bytes into the destination blob in place, which is
# the mutate-in-place shape the row forbids.
M5A_OLD=$'\ts.reserved = expectedBytes + s.entryGrowth\n\tc.staged[s] = struct{}{}\n\treturn s, nil\n'
M5A_NEW=$'\ts.reserved = expectedBytes + s.entryGrowth\n\tc.staged[s] = struct{}{}\n\t// MUTATION: publish the pointer now, let the bytes follow into the\n\t// destination blob written in place.\n\tif c.blobs[s.hash] == nil {\n\t\tc.blobs[s.hash] = &blobRef{Hash: s.hash, Size: expectedBytes}\n\t}\n\tc.blobs[s.hash].Refs++\n\tif old := c.entries[path]; old != nil {\n\t\tc.dropPathLocked(path)\n\t}\n\tc.entries[path] = &cacheEntry{Path: path, Hash: s.hash, Size: expectedBytes,\n\t\tLastHitMS: c.cfg.Now().UnixMilli(), Gen: c.gen}\n\tc.recountLocked()\n\treturn s, nil\n'
M5B_OLD=$'\tn, err := s.file.Write(p)\n'
M5B_NEW=$'\t// MUTATION: the bytes go into the DESTINATION blob, in place.\n\tdst, derr := os.OpenFile(s.c.blobPath(s.hash), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)\n\tif derr != nil {\n\t\treturn 0, derr\n\t}\n\tn, err := dst.Write(p)\n\t_ = dst.Close()\n'

BEFORE=$(sha256sum "$SRC" | awk '{print $1}')
cp -p "$SRC" "$BAK/cache.go.orig"
echo "cache.go before : sha256=$BEFORE"
echo "backup dir      : $BAK"

restore() {
  cp -p "$BAK/cache.go.orig" "$SRC"
  local after
  after=$(sha256sum "$SRC" | awk '{print $1}')
  if [ "$after" = "$BEFORE" ]; then
    echo "RESTORE VERIFIED: cache.go sha256=$after (unchanged)"
  else
    echo "RESTORE FAILED: $BEFORE -> $after" >&2
    exit 3
  fi
}
trap restore EXIT

# ── the groups: mutation(s) | the cell that must catch them ──────────────────
GROUP_MUTS=("M1" "M2" "M3" "M4" "M5A M5B")
GROUP_CELLS=(
  "TestInFlightBytesDecideAdmission"
  "TestReaderHoldingTheOldBlobReadsTheOldCompleteContent"
  "TestEntryBoundIsEnforcedAndReported"
  "TestAbandonedRefreshDiscardsItsBlobAndNeverSwaps"
  "TestAtomicRefreshHalfFileCell"
)

proven=0
for i in "${!GROUP_MUTS[@]}"; do
  muts="${GROUP_MUTS[$i]}"
  cell="${GROUP_CELLS[$i]}"
  echo
  echo "==================================================================="
  echo "MUTATION GROUP: $muts"
  echo "CELL UNDER TEST: $cell"
  echo "==================================================================="

  for mut in $muts; do
    old_var="${mut}_OLD"
    new_var="${mut}_NEW"
    if ! exact_replace "${!old_var}" "${!new_var}"; then
      echo "MUTATION $mut FAILED TO APPLY" >&2
      exit 2
    fi
  done

  if ! go -C "$WT" build ./... >"$BAK/build.log" 2>&1; then
    echo "BUILD BROKEN BY MUTATION (not a valid control):"
    tail -5 "$BAK/build.log"
    restore
    exit 2
  fi

  echo "--- MUTATED: go test -run $cell ---"
  OUT=$(go -C "$WT" test "$PKG" -count=1 -run "$cell" 2>&1)
  MRC=$?
  printf '%s\n' "$OUT" | grep -E '^(---|    ---|ok|FAIL|\s+cache_staged)' | tail -12
  echo "mutated test exit code = $MRC (non-zero = the cell CATCHES the mutation)"

  restore

  echo "--- RESTORED: go test -run $cell ---"
  OUT2=$(go -C "$WT" test "$PKG" -count=1 -run "$cell" 2>&1)
  RRC=$?
  printf '%s\n' "$OUT2" | tail -3
  echo "restored test exit code = $RRC (0 = green on the real tree)"

  if [ "$MRC" -ne 0 ] && [ "$RRC" -eq 0 ]; then
    echo "NON-VACUITY: PROVEN for $cell"
    proven=$((proven + 1))
  else
    echo "NON-VACUITY: NOT PROVEN for $cell"
  fi
done

FINAL=$(sha256sum "$SRC" | awk '{print $1}')
echo
echo "==================================================================="
echo "cells with a proven negative control: $proven of ${#GROUP_CELLS[@]}"
echo "cache.go final sha256=$FINAL"
if [ "$FINAL" = "$BEFORE" ]; then
  echo "FINAL RESTORE VERIFIED: the tree is byte-identical to the start"
else
  echo "FINAL RESTORE MISMATCH" >&2
  exit 3
fi
