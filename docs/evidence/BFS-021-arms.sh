#!/usr/bin/env bash
# BFS-021 — the arms, and the controls that prove each claiming cell can FAIL.
#
# THE RULE: a test that cannot fail proves nothing. Every cell of this row appears
# below with the SOURCE MUTATION that turns it RED, applied with `git apply`,
# run BY NAME, and RESTORED from `git checkout` with the sha256 of every touched
# file re-checked afterwards — a restore that is not byte-identical aborts the run
# (so a green after an arm can never be read as a claim about a tree that is not
# the committed one). Each arm also names ATTRIBUTION cells that must stay GREEN
# under the mutation, so a red is caused by the requirement under test rather than
# by a broken tree.
#
# The cells (internal/fsmount/append_test.go):
#   C1  append through an append handle LANDS its bytes
#   C2  a retried chunk at the same offset does not double-apply (and is the
#       surface's reported no-op: mtime unmoved, BFS-039)
#   C3  a stale base REFUSES the append, nothing lands, the refusal HOLDS, and the
#       documented recovery (a caller read, then retry) works
#   C4  an append above the bound is refused LOUDLY and writes nothing
#   C5  a write through a NON-append handle is still refused (BFS-012/BFS-030)
#   C6  a later publication on the same handle carries the earlier ones bytes
#   C7  a reader during an append sees a WHOLE content, never a tear
#   C9  a cancelled append is retryable, applies once, and leaves no named buffer
#   C10 the append path never trips BFS-030's write-shape refusal
# plus the pre-existing BFS-030 cells in write_shape_test.go, which must stay green
# under every mutation of this row (they are the semantics this row may not change).
#
# usage: bash docs/evidence/BFS-021-arms.sh <mode>
#   green                                  the tree as committed: every cell passes.
#   all                                    green, then every mutation.
#   <mutation>                             one mutation; see the list below.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
FILES=(internal/fsmount/fs_linux.go internal/fsmount/append.go internal/server/webdav/tree.go)
WORK="$(mktemp -d /tmp/bfs021-arms-XXXXXX)"

C1=TestAppendThroughAnAppendHandleLandsTheBytes
C2=TestARetriedAppendChunkAtTheSameOffsetDoesNotDoubleApply
C3=TestAStaleBaseRefusesTheAppendAndNothingLands
C4=TestAnAppendAboveTheBoundIsRefusedLoudlyAndWritesNothing
C5=TestAWriteThroughANonAppendHandleIsStillRefused
C5B=TestAResizeWithAnAppendHandleLiveIsStillRefused
C6=TestALaterPublicationOnTheSameHandleCarriesTheEarlierOnesBytes
C7=TestAReaderDuringAnAppendSeesAWholeContentNeverATear
C9=TestACancelledAppendIsRetryableAppliesOnceAndLeavesNoResidue
C10=TestAppendIsServedWithoutPublishingASizeCarryingSetattr
BFS030=TestResizeThroughAWriteHandleIsRefusedBeforeItPublishes
ALL_CELLS="^(${C1}|${C2}|${C3}|${C4}|${C5}|${C5B}|${C6}|${C7}|${C9}|${C10})$"

MODES="green write-not-dispatched chunk-applied-at-the-end size-follows-the-write-not-the-offset publication-bypasses-the-refusal-hold bound-not-enforced any-write-intent-open-is-an-append commit-in-place-instead-of-rename base-does-not-advance cancel-is-terminal named-buffer resize-refusal-removed all"

# mutation -> "RED cells|GREEN (attribution) cells"
declare -A RED GREEN
RED[write-not-dispatched]="^(${C1})$"
GREEN[write-not-dispatched]="^(${C5}|${BFS030})$"
RED[chunk-applied-at-the-end]="^(${C2})$"
GREEN[chunk-applied-at-the-end]="^(${C1}|${C5})$"
RED[size-follows-the-write-not-the-offset]="^(${C2})$"
GREEN[size-follows-the-write-not-the-offset]="^(${C1}|${C5})$"
RED[publication-bypasses-the-refusal-hold]="^(${C3})$"
GREEN[publication-bypasses-the-refusal-hold]="^(${C1}|${C5})$"
RED[bound-not-enforced]="^(${C4})$"
GREEN[bound-not-enforced]="^(${C1}|${C5})$"
RED[any-write-intent-open-is-an-append]="^(${C5})$"
GREEN[any-write-intent-open-is-an-append]="^(${C1}|${C5B}|${BFS030})$"
RED[commit-in-place-instead-of-rename]="^(${C7})$"
GREEN[commit-in-place-instead-of-rename]="^(${C1}|${C5})$"
RED[base-does-not-advance]="^(${C6})$"
GREEN[base-does-not-advance]="^(${C1}|${C5})$"
RED[cancel-is-terminal]="^(${C9})$"
GREEN[cancel-is-terminal]="^(${C1}|${C5})$"
RED[named-buffer]="^(${C9})$"
GREEN[named-buffer]="^(${C1}|${C5})$"
# BFS-030's refusal lives in TWO places: its own cell (write_shape_test.go) and the
# cell that keeps it intact under the write-intent shape this row ADDS. Both go red
# when it is removed; the append cells must not care, which is the attribution.
RED[resize-refusal-removed]="^(${BFS030}|${C5B})$"
GREEN[resize-refusal-removed]="^(${C10}|${C1}|${C5})$"

usage() {
  printf 'usage: %s <mode>\n  modes: %s\n' "$(basename "$0")" "$MODES" >&2
  exit 2
}

snapshot() { (cd "$REPO" && sha256sum "${FILES[@]}") > "$WORK/before.sha"; }

restore() {
  git -C "$REPO" checkout -- "${FILES[@]}"
  if ! (cd "$REPO" && sha256sum "${FILES[@]}") | diff -q - "$WORK/before.sha" >/dev/null; then
    echo "  RESTORE IS NOT BYTE-IDENTICAL — aborting"
    return 1
  fi
  echo "  restore: sha256 byte-identical for ${#FILES[@]} file(s)"
}

# run_set <label> <regex> — 0 when the run PASSED (every matched test green)
run_set() {
  local label="$1" re="$2" out rc
  out=$(cd "$REPO" && go test ./internal/fsmount/ -count=1 -v -run "$re" 2>&1)
  rc=$?
  echo "  $label rc=$rc"
  printf '%s\n' "$out" | grep -E "^(--- FAIL|--- PASS|ok|FAIL|PASS)" | sed 's/^/      /'
  # A pattern that matches NOTHING makes `go test` succeed with "no tests to run",
  # which would read as a pass and prove nothing. It is a hard failure here.
  if printf '%s' "$out" | grep -q "no tests to run"; then
    echo "      NO TEST MATCHED ${re}: this arm is VACUOUS"
    return 1
  fi
  if ! printf '%s' "$out" | grep -qE "^--- (PASS|FAIL)"; then
    echo "      NO TEST RAN (no per-test result line): this arm is VACUOUS"
    return 1
  fi
  return $rc
}

ARM_FAILURES=0

arm() {
  local name="$1" rc=0
  echo
  echo "================================================================================"
  echo "MUTATION $name"
  echo "  RED (must FAIL)  = ${RED[$name]}"
  echo "  GREEN (must PASS)= ${GREEN[$name]}"
  echo "================================================================================"
  snapshot
  if ! git -C "$REPO" apply "$HERE/BFS-021-redproof-$name.patch"; then
    echo "  THE PATCH DID NOT APPLY — nothing was measured"
    restore
    ARM_FAILURES=$((ARM_FAILURES + 1))
    return 1
  fi
  echo "  applied. changed files: $(git -C "$REPO" diff --name-only | tr '\n' ' ')"
  if ! (cd "$REPO" && go build ./... >/dev/null 2>&1); then
    echo "  THE MUTATED TREE DOES NOT COMPILE — this mutation proves nothing"
    restore
    ARM_FAILURES=$((ARM_FAILURES + 1))
    return 1
  fi
  echo "  the mutated tree compiles"

  if run_set "RED   -run ${RED[$name]}" "${RED[$name]}"; then
    echo "  ARM FAILED: the claimed cell stayed GREEN under its own mutation"
    rc=1
  else
    echo "  RED confirmed: the mutation reddens the cell it is aimed at"
  fi
  if run_set "GREEN -run ${GREEN[$name]}" "${GREEN[$name]}"; then
    echo "  attribution confirmed: the named cells stay GREEN"
  else
    echo "  ARM FAILED: an attribution cell went RED too (the mutation is not specific)"
    rc=1
  fi
  restore || rc=1
  [ "$rc" = 0 ] || ARM_FAILURES=$((ARM_FAILURES + 1))
  return "$rc"
}

mode() {
  local m="${1:-}" n
  case "$m" in
    green)
      echo "================================================================================"
      echo "GREEN — the tree as committed: every claiming cell must PASS"
      echo "================================================================================"
      snapshot
      run_set "ALL     -run ${ALL_CELLS}" "$ALL_CELLS" || ARM_FAILURES=$((ARM_FAILURES + 1))
      run_set "BFS-030 -run ^${BFS030}\$" "^${BFS030}$" || ARM_FAILURES=$((ARM_FAILURES + 1))
      ;;
    all)
      mode green
      for n in write-not-dispatched chunk-applied-at-the-end size-follows-the-write-not-the-offset \
               publication-bypasses-the-refusal-hold bound-not-enforced \
               any-write-intent-open-is-an-append commit-in-place-instead-of-rename \
               base-does-not-advance cancel-is-terminal named-buffer resize-refusal-removed; do
        arm "$n"
      done
      ;;
    "")
      usage
      ;;
    *)
      [ -f "$HERE/BFS-021-redproof-$m.patch" ] || { echo "no such mutation: $m" >&2; usage; }
      arm "$m"
      ;;
  esac
  echo
  if [ "$ARM_FAILURES" = 0 ]; then
    echo "TOTAL: every declared outcome met (0 arm failures)"
  else
    echo "TOTAL: $ARM_FAILURES arm failure(s)"
  fi
  [ "$ARM_FAILURES" = 0 ]
}

mode "${1:-}"
