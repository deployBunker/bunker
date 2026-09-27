#!/usr/bin/env bash
# bfs031-mutation-red.sh — the NEGATIVE CONTROL for BFS-031.
#
# The row's acceptance is not "the cell is green" and not "the figures exist": it
# is that THE CELL FAILS WHEN THE ACCOUNTING IS NEUTERED. So each group below
# removes the ONE thing that makes the property hold, runs the cell that owns it,
# and requires it to go RED — then restores the file and requires it to go GREEN
# again, with a sha256 check on both sides of the restore. "NON-VACUITY: PROVEN"
# is printed only when both halves hold.
#
# The mutations are the DEFECT ITSELF, one mechanism at a time:
#   M1  the layout split   — the cache directory is the mount directory again, so
#                            status.json and the refusal log are inside the thing
#                            the bound names (the RED shape, at 1 KiB);
#   M2  the peak           — the bound is checked against the occupancy at rest
#                            again, so the index's temp copy escapes it;
#   M3  the log's bound    — the refusal log is never rotated back under its cap;
#   M4  the document's cap — the status document is written whatever its size;
#   M5  the state's figure — the refusal log's bytes are not counted;
#   M6  the drop count     — entries the bound drops are not counted.
#
# Why bash+perl and not a Python helper: .gitignore forbids *.py outside tools/,
# and a probe that cannot be committed cannot be re-run by anyone else.
#
# Usage: bash probes/bfs031-mutation-red.sh [worktree-root]
set -uo pipefail

WT=${1:-/home/kara/worktrees/bunker-BFS-031}
CACHE="$WT/internal/fsclient/cache.go"
STATUS="$WT/internal/fsclient/status.go"
STATE="$WT/internal/fsclient/state.go"
LAYOUT="$WT/internal/fsclient/layout.go"
BAK=$(mktemp -d /tmp/bfs031-mutation-XXXXXX)
FSCLIENT=./internal/fsclient/

exact_replace() {
  local file="$1"
  OLD="$2" NEW="$3" perl -e '
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
  ' "$file" || {
    echo "MUTATION NOT APPLIED ($file): the battery cannot grade a group whose mutation never landed"
    exit 1
  }
}

begin_group() {
  local name="$1" file="$2"
  GROUP="$name"
  FILE="$file"
  BEFORE=$(sha256sum "$FILE" | awk '{print $1}')
  cp "$FILE" "$BAK/$(basename "$FILE").$name"
  echo
  echo "==================================================================="
  echo "MUTATION GROUP: $name"
  echo "FILE           : ${FILE#$WT/}"
  echo "sha256 before  : $BEFORE"
  echo "==================================================================="
}

expect() {
  local want="$1" label="$2" pkg="$3" cell="$4"
  local out rc
  out=$(cd "$WT" && go test "$pkg" -run "$cell" -count=1 2>&1)
  rc=$?
  if [ "$want" = GREEN ] && [ $rc -ne 0 ]; then
    echo "--- $label ($cell) ---"
    echo "$out" | head -40
    echo "THE CELL IS RED ON THE UNMUTATED TREE (rc=$rc): this group proves nothing until the tree is green"
    exit 1
  fi
  if [ "$want" = RED ] && [ $rc -eq 0 ]; then
    echo "--- $label ($cell) ---"
    echo "THE CELL STAYED GREEN UNDER A NEUTERED ACCOUNTING (rc=0): the cell does not measure it"
    exit 1
  fi
  if [ "$want" = RED ]; then
    echo "--- $label ($cell) ---"
    echo "$out" | grep -E "^\s+---? FAIL|_test\.go:[0-9]+:|cache\.go:|status\.go:|state\.go:|layout\.go:" | head -6
    echo "mutated cell exit code = $rc (non-zero = the cell CATCHES the mutation)"
  fi
}

finish_group() {
  local cell_pkg="$1" cell="$2"
  cp "$BAK/$(basename "$FILE").$GROUP" "$FILE"
  local after
  after=$(sha256sum "$FILE" | awk '{print $1}')
  if [ "$after" != "$BEFORE" ]; then
    echo "RESTORE FAILED: $FILE sha256=$after, want $BEFORE"
    exit 1
  fi
  echo "RESTORE VERIFIED: $(basename "$FILE") sha256=$after (byte-identical)"
  local out rc
  out=$(cd "$WT" && go test "$cell_pkg" -run "$cell" -count=1 2>&1)
  rc=$?
  if [ $rc -ne 0 ]; then
    echo "--- RESTORED ($cell) ---"
    echo "$out" | head -12
    echo "THE CELL IS RED AFTER THE RESTORE (rc=$rc): the restore is not byte-identical in effect"
    exit 1
  fi
  echo "--- RESTORED ($cell) ---"
  echo "ok (rc=0)"
  echo "NON-VACUITY: PROVEN for $GROUP"
}

TINY=TestTheBoundBoundsTheCacheDirectoryAtATinyBound
AGREEMENT=TestTheReportedDirectoryFigureAgreesWithAnIndependentWalk
LOGBOUND=TestTheRefusalLogIsBoundedAndTheDropIsCounted
STATUSCAP=TestTheStatusDocumentIsBoundedByItsOwnCap
STATEARM=TestTheStateMeasurementNamesEveryResidentAndItsBound
CENSUS=TestEveryFigureInTheStatusRecordMovesOrIsExplained

echo "BFS-031 NON-VACUITY BATTERY"
echo "worktree: $WT"
echo "backups : $BAK"

# ── M1: the LAYOUT SPLIT — the cache directory is the mount directory again ──
begin_group M1 "$LAYOUT"
expect GREEN "baseline" "$FSCLIENT" "$AGREEMENT"
M1_OLD=$'\treturn filepath.Join(mountDir, CacheSubdir)\n'
M1_NEW=$'\treturn mountDir // MUTATION (BFS-031 control): the cache directory is the mount directory, so the observability files are back inside the bound.\n'
exact_replace "$LAYOUT" "$M1_OLD" "$M1_NEW"
expect RED "MUTATED" "$FSCLIENT" "$AGREEMENT"
finish_group "$FSCLIENT" "$AGREEMENT"

# ── M2: the PEAK — the bound is checked against the occupancy at rest again ──
begin_group M2 "$CACHE"
expect GREEN "baseline" "$FSCLIENT" "$TINY"
M2_OLD=$'\treturn c.blobsBytesLocked() + c.stagedReservedLocked() + extraBlob + 2*index + c.dirForeignBytes\n'
M2_NEW=$'\treturn c.blobsBytesLocked() + c.stagedReservedLocked() + extraBlob + index + c.dirForeignBytes // MUTATION (BFS-031 control): the index temp copy is not reserved.\n'
exact_replace "$CACHE" "$M2_OLD" "$M2_NEW"
expect RED "MUTATED" "$FSCLIENT" "$TINY"
finish_group "$FSCLIENT" "$TINY"

# ── M3: the REFUSAL LOG'S BOUND — rotation never fires ───────────────────────
begin_group M3 "$STATUS"
expect GREEN "baseline" "$FSCLIENT" "$LOGBOUND"
M3_OLD=$'\tif info, serr := os.Stat(path); serr == nil && info.Size()+int64(len(line)) > ConflictsMaxBytes {\n'
M3_NEW=$'\tif _, serr := os.Stat(path); serr == nil && false { // MUTATION (BFS-031 control): the log is never rotated back under its cap.\n'
exact_replace "$STATUS" "$M3_OLD" "$M3_NEW"
expect RED "MUTATED" "$FSCLIENT" "$LOGBOUND"
finish_group "$FSCLIENT" "$LOGBOUND"

# ── M4: the DOCUMENT'S CAP — the status document is written whatever its size ─
begin_group M4 "$STATUS"
expect GREEN "baseline" "$FSCLIENT" "$STATUSCAP"
M4_OLD=$'\traw, err := encodeStatus(st)\n\tif err != nil {\n\t\treturn err\n\t}\n\tif int64(len(raw)) > StatusMaxBytes {\n'
M4_NEW=$'\traw, err := encodeStatus(st)\n\tif err != nil {\n\t\treturn err\n\t}\n\tif false { // MUTATION (BFS-031 control): the document bound is declared and not enforced.\n'
exact_replace "$STATUS" "$M4_OLD" "$M4_NEW"
expect RED "MUTATED" "$FSCLIENT" "$STATUSCAP"
finish_group "$FSCLIENT" "$STATUSCAP"

# ── M5: the STATE'S FIGURE — the refusal log's bytes are not counted ─────────
begin_group M5 "$STATE"
expect GREEN "baseline" "$FSCLIENT" "$STATEARM"
M5_OLD=$'\t\t\tst.ConflictsBytes += size\n'
M5_NEW=$'\t\t\tst.ConflictsBytes += 0 // MUTATION (BFS-031 control): the refusal log\'s bytes are not measured.\n'
exact_replace "$STATE" "$M5_OLD" "$M5_NEW"
expect RED "MUTATED" "$FSCLIENT" "$STATEARM"
finish_group "$FSCLIENT" "$STATEARM"

# ── M6: the DROP COUNT — entries the bound drops are not counted ─────────────
begin_group M6 "$STATE"
expect GREEN "baseline" "$FSCLIENT" "$LOGBOUND"
M6_OLD=$'\treturn os.WriteFile(path, []byte(strconv.FormatInt(have+n, 10)+"\\n"), 0o600)\n'
M6_NEW=$'\treturn os.WriteFile(path, []byte(strconv.FormatInt(have, 10)+"\\n"), 0o600) // MUTATION (BFS-031 control): the dropped count is never raised.\n'
exact_replace "$STATE" "$M6_OLD" "$M6_NEW"
expect RED "MUTATED" "$FSCLIENT" "$LOGBOUND"
finish_group "$FSCLIENT" "$LOGBOUND"

echo
echo "==================================================================="
echo "ALL GROUPS: the accounting was neutered six ways, each owning cell went"
echo "RED, and every restore was sha256-verified byte-identical."
echo "==================================================================="
