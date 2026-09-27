#!/usr/bin/env bash
# BFS-045 mutation battery — the negative control for EVERY counter the row's
# census proves moves.
#
# The row's acceptance is not "the counters exist" and not "the test is green":
# it is that THE TEST FAILS WHEN A COUNTER IS NEUTERED. A figure that reads zero
# in a test that should move it is a BFS-032 defect, and the test must catch it.
# So each group below removes the ONE line that makes a figure move, runs the
# cell that owns it, and requires the cell to go RED — then restores the file and
# requires it to go GREEN again, with a sha256 check on both sides of the
# restore. "NON-VACUITY: PROVEN" is printed only when both halves hold.
#
# Why bash+perl and not a Python helper: .gitignore forbids *.py outside tools/,
# and a probe that cannot be committed cannot be re-run by anyone else.
#
# Usage: bash bfs045-mutation-red.sh [worktree-root]
set -uo pipefail

WT=${1:-/home/kara/worktrees/bunker-BFS-045}
CACHE="$WT/internal/fsclient/cache.go"
INV="$WT/internal/fsclient/invalidate.go"
BAK=$(mktemp -d /tmp/bfs045-mutation-XXXXXX)
FSCLIENT=./internal/fsclient/
FSMOUNT=./internal/fsmount/

# exact_replace FILE OLD NEW — replace OLD with NEW in FILE, refusing unless OLD
# occurs exactly once. The file is read whole, checked, and only then written, so
# a refused anchor can never leave a half-edited source behind.
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
  ' "$file"
}

# run_group NAME FILE CELL PKG — back up FILE, run CELL green, mutate, run red,
# restore, verify, run green, and print the verdict.
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

# expect GREEN|RED LABEL PKG CELL
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
    echo "THE CELL STAYED GREEN UNDER A NEUTERED COUNTER (rc=0): the counter is not what the cell measures"
    exit 1
  fi
  if [ "$want" = RED ]; then
    echo "--- $label ($cell) ---"
    echo "$out" | grep -E "^\s+---? FAIL|status_census_test.go:|status_bounds_test.go:|cache\.go:|invalidate\.go:" | head -6
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
  echo "RESTORE VERIFIED: $(basename "$FILE") sha256=$after (unchanged)"
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

CENSUS=TestEveryFigureInTheStatusRecordMovesOrIsExplained
LIVEPATH=TestAnOversizeReadThroughTheLivePathCountsTheBypassWithItsReason
AGREEMENT=TestTheReportedDirectoryFigureAgreesWithAnIndependentWalk

echo "BFS-045 NON-VACUITY BATTERY"
echo "worktree: $WT"
echo "backups : $BAK"

# ── M1: the LIVE READ PATH's bypass counter ──────────────────────────────────
# BFS-032's exact shape, now guarded: the refusal is counted WHERE IT IS
# DECIDED, so removing that one call must turn the live-path arm red.
begin_group M1 "$CACHE"
expect GREEN "baseline" "$FSMOUNT" "$LIVEPATH"
M1_OLD=$'\t\tc.CountBypass(BypassReasonOverEntryCap)\n\t\tc.mu.Lock()\n'
M1_NEW=$'\t\t// MUTATION (BFS-045 control): the refusal is not counted.\n\t\tc.mu.Lock()\n'
exact_replace "$CACHE" "$M1_OLD" "$M1_NEW"
expect RED "MUTATED" "$FSMOUNT" "$LIVEPATH"
finish_group "$FSMOUNT" "$LIVEPATH"

# ── M2: the over-cap census AT THE DECISION SITE, seen from the census arm ───
begin_group M2 "$CACHE"
expect GREEN "baseline" "$FSCLIENT" "$CENSUS"
M2_OLD=$'\t\tc.mu.Lock()\n\t\tc.stats.OversizeBypasses++\n\t\tc.mu.Unlock()\n'
M2_NEW=$'\t\t// MUTATION (BFS-045 control): the aggregate figure is not moved.\n'
exact_replace "$CACHE" "$M2_OLD" "$M2_NEW"
expect RED "MUTATED" "$FSCLIENT" "$CENSUS"
finish_group "$FSCLIENT" "$CENSUS"

# ── M3: the FAILURE counter on the invalidation path ─────────────────────────
begin_group M3 "$INV"
expect GREEN "baseline" "$FSCLIENT" "$CENSUS"
M3_OLD=$'\ti.failures++\n\ti.lastFailure = err.Error()\n'
M3_NEW=$'\t// MUTATION (BFS-045 control): an attempt that produced no answer is not counted.\n'
exact_replace "$INV" "$M3_OLD" "$M3_NEW"
expect RED "MUTATED" "$FSCLIENT" "$CENSUS"
finish_group "$FSCLIENT" "$CENSUS"

# ── M4: the CONTENT-AGE evidence (the figure that did not exist at all) ──────
begin_group M4 "$INV"
expect GREEN "baseline" "$FSCLIENT" "$CENSUS"
M4_OLD=$'\ti.vouchedFrom = from\n\ti.vouchedOK = true\n'
M4_NEW=$'\ti.vouchedFrom = from\n\t// MUTATION (BFS-045 control): evidence recorded but never claimed.\n'
exact_replace "$INV" "$M4_OLD" "$M4_NEW"
expect RED "MUTATED" "$FSCLIENT" "$CENSUS"
finish_group "$FSCLIENT" "$CENSUS"

# ── M5: the independent directory measurement ────────────────────────────────
begin_group M5 "$CACHE"
expect GREEN "baseline" "$FSCLIENT" "$AGREEMENT"
M5_OLD=$'\tc.stats.DirBytes = c.dirBytes\n'
M5_NEW=$'\tc.stats.DirBytes = 0 // MUTATION (BFS-045 control): the walk is discarded.\n'
exact_replace "$CACHE" "$M5_OLD" "$M5_NEW"
expect RED "MUTATED" "$FSCLIENT" "$AGREEMENT"
finish_group "$FSCLIENT" "$AGREEMENT"

# ── M6: the staged-flow denominator (a refresh that never counts as started) ─
begin_group M6 "$CACHE"
expect GREEN "baseline" "$FSCLIENT" "$CENSUS"
M6_OLD=$'\tc.staged[s] = struct{}{}\n\tc.stats.StagedStartedTotal++\n'
M6_NEW=$'\tc.staged[s] = struct{}{}\n\t// MUTATION (BFS-045 control): an admitted refresh is not counted.\n'
exact_replace "$CACHE" "$M6_OLD" "$M6_NEW"
expect RED "MUTATED" "$FSCLIENT" "$CENSUS"
finish_group "$FSCLIENT" "$CENSUS"

echo
echo "==================================================================="
echo "ALL GROUPS: every counter's cell went RED under its mutation and GREEN"
echo "after a sha256-verified restore. The census is not vacuous."
echo "==================================================================="
