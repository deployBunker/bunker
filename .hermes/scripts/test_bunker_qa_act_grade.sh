#!/bin/bash
# QA-BUNKER-41 — regression guard `act_rc1_fails_grade`.
#
# THE DEFECT THIS PINS: the JIT battery's graded outcome was decided by the
# NATIVE go suite alone, while the act leg (nektos/act replaying
# .github/workflows/ci.yml — the workflow that gates every bunker push) could
# exit rc=1 and still be carried as free-text detail on a cell that graded on
# the native leg. Battery 2026-09-30 (JIT agent 67928b30 @ bunker-las-03,
# HEAD 47377c3) recorded verbatim:
#     ci-pass | OK   | "act failed (rc=1: [CI/gitreins-guard] Run Complete job|
#                      Cleaning up container for job gitreins-) + native suite PASS"
# i.e. a false green: the CI-simulation leg failed and the battery still passed.
#
# WHAT THIS GUARD DOES — against the harness's own output, no reimplemented copy:
#   1. RENDERS the generated remote script twice through the harness's
#      __gen-remote entry (native leg = `true` and = `false`) and requires both
#      renders to PARSE (`bash -n`). A harness edit that breaks the heredoc drops
#      the whole battery and nothing else catches it (the QA-BUNKER-38 comment
#      that lost its leading '#' at HEAD rendered an unparseable script while
#      every other suite stayed green);
#   2. extracts the ci-pass cell block from the RENDERED bytes (what the agent
#      actually executes) and drives it with a stubbed act rc;
#   3. asserts act rc=1 grades ci-pass FAIL (never OK) and carries the act leg's
#      own token (act_result=fail), with the ci-act row stating the same verdict;
#   4. asserts the GRADED OUTCOME — qa_run_summary, extracted from the harness
#      and run over the evidence rows the cell itself recorded — comes back
#      NON-ZERO for act rc=1. That is the row's acceptance: the act leg's own
#      result is part of the battery's grade, not a footnote on the native leg's;
#   5. asserts act rc=0 and act "skip" still grade OK (no over-correction) and
#      that a timeout (rc=124) fails while naming the BUNKER_QA_ACT_TIMEOUT
#      budget instead of reading like a repo defect;
#   6. pins the CI wiring: .github/workflows/ci.yml keeps a `gitreins-guard:`
#      JOB (the graded leg stays in the workflow that gates every push) and
#      names this guard; the harness keeps the act-leg vocabulary the AC counts.
#
# NOTE ON THE NATIVE LEG: the generated script's native command is interpolated
# at GENERATION time, so each arm renders the variant it needs (`true` / `false`)
# rather than overriding a variable. Mirrors the extraction/driver pattern of the
# other test_bunker_qa_*.sh suites; HARNESS overridable via BUNKER_QA_SH so a
# foreman can point this at a specific commit's harness.
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
HARNESS="${BUNKER_QA_SH:-$HERE/bunker-qa.sh}"
CI_WF="${BUNKER_QA_CI_WF:-$HERE/../../.github/workflows/ci.yml}"
pass=0; fail=0
check() { # <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  ok   $1 -> $3"; pass=$((pass+1))
  else echo "  FAIL $1: expected [$2] got [$3]"; fail=$((fail+1)); fi
}
check_contains() { # <desc> <needle> <haystack>
  case "$3" in
    *"$2"*) echo "  ok   $1"; pass=$((pass+1)) ;;
    *)      echo "  FAIL $1: missing [$2] in [$3]"; fail=$((fail+1)) ;;
  esac
}
check_absent() { # <desc> <needle> <haystack>
  case "$3" in
    *"$2"*) echo "  FAIL $1: found forbidden [$2] in [$3]"; fail=$((fail+1)) ;;
    *)      echo "  ok   $1"; pass=$((pass+1)) ;;
  esac
}
check_atleast() { # <desc> <min> <actual>
  if [ "$3" -ge "$2" ] 2>/dev/null; then echo "  ok   $1 -> $3 (>= $2)"; pass=$((pass+1))
  else echo "  FAIL $1: expected >= $2 got [$3]"; fail=$((fail+1)); fi
}

[ -r "$HARNESS" ] || { echo "FAIL act_rc1_fails_grade: harness not readable: $HARNESS"; exit 1; }
command -v bash >/dev/null 2>&1 || { echo "FAIL act_rc1_fails_grade: bash missing"; exit 1; }

TMPD=${TMPDIR:-/tmp}/qa_bunker_41.$$
mkdir -p "$TMPD"
trap 'rm -rf "$TMPD"' EXIT

echo "QA-BUNKER-41: act_rc1_fails_grade — the act leg's rc is a graded participant"
echo

# ── render the REAL generated script, one variant per native-leg outcome ─────
# render_gen <native-cmd> <slug> ; sets GEN_<slug>, BLOCK_<slug>
render_gen() { # <native-cmd> <slug> — 0 on success
  local native="$1" slug="$2" gen="$TMPD/gen-$2.sh"
  bash "$HARNESS" __gen-remote qa41-fixture ':' ':' "$native" > "$gen" 2> "$gen.err"
  local rc=$?
  check "render __gen-remote (native=$native) rc" "0" "$rc"
  local bytes; bytes=$(wc -c < "$gen" | tr -d ' ')
  if [ "${bytes:-0}" -lt 20000 ]; then
    echo "  FAIL rendered script too small ($bytes bytes, native=$native) — rendering is broken"
    fail=$((fail+1))
    return 1
  fi
  echo "  ok   rendered script is $bytes bytes (native=$native)"
  pass=$((pass+1))
  bash -n "$gen" 2> "$gen.syntax"
  local src=$?
  check "rendered battery parses (bash -n, native=$native)" "0" "$src"
  if [ "$src" -ne 0 ]; then
    echo "----- render syntax error (native=$native) -----"; sed -n '1,6p' "$gen.syntax"; echo "-----------------------------------------------"
    return 1
  fi
  awk '/^#QA-CELL:ci-pass:start/{f=1;next} /^#QA-CELL:ci-pass:end/{f=0} f{print}' "$gen" > "$TMPD/block-$slug.sh"
  if grep -q 'cell ci-pass' "$TMPD/block-$slug.sh"; then
    echo "  ok   ci-pass block extracted from the render (native=$native, $(wc -l < "$TMPD/block-$slug.sh" | tr -d ' ') lines)"
    pass=$((pass+1))
    return 0
  fi
  echo "  FAIL could not extract the ci-pass block from the render (native=$native) — markers moved?"
  fail=$((fail+1))
  return 1
}

render_gen 'true'  ok
render_gen 'false' bad

# ── the GRADED OUTCOME function, straight out of the harness ────────────────
awk '/^qa_run_summary\(\) \{/{f=1} f{print} f && /^\}$/{exit}' "$HARNESS" > "$TMPD/summary.sh"
if grep -q '^qa_run_summary() {' "$TMPD/summary.sh" && grep -q '^}$' "$TMPD/summary.sh"; then
  echo "  ok   qa_run_summary extracted from the harness"
  pass=$((pass+1))
else
  echo "  FAIL could not extract qa_run_summary from $HARNESS"
  fail=$((fail+1))
fi

# ── driver: stubs (same names/shape the generated script provides) + block ──
# The generated cell() writes one JSON object per line; the stub mirrors that
# key order so qa_run_summary parses these rows exactly like real evidence.
cat > "$TMPD/driver.head" <<'DRIVER'
#!/bin/bash
set -u
: > "$EVID"; : > "$CELLS"; mkdir -p "$LOGD"; : > "$LOGD/ci.log"
printf 'Job succeeded\n' > "$LOGD/ci.log"
QA_ACT_NOTE=" (no workflow can fire on a push to this branch)"
ACT_WF_COUNT=1
ci_rc="$CI_RC_IN"
vacuous_native_suite()        { return "$VACUOUS_IN"; }
native_runner_missing()       { return 1; }
native_runner_missing_cause() { printf 'pytest: command not found\n'; }
build_env_failure()           { return 1; }
go_suite_env_failure()        { return 1; }
ci_only_failure()             { return 1; }
act_failure_context()         { printf 'stub-act-context'; }
cell() {
  local d
  d=$(printf '%s' "$3" | tr '\n\t\r' '   ' | sed 's/"/'"'"'/g')
  printf '{"project":"qa41","cell":"%s","status":"%s","detail":"%s","ts":"%s"}\n' \
    "$1" "$2" "$d" "$(date -u +%FT%TZ)" >> "$EVID"
  printf '%s\t%s\t%s\n' "$1" "$2" "$d" >> "$CELLS"
}
DRIVER

# run_arm <block-slug> <act-rc> <vacuous 0|1>
#   prints "cell<TAB>status<TAB>detail" rows; leaves the graded summary line in
#   $TMPD/summary.txt and its rc in ARM_SUM_RC.
run_arm() {
  local slug="$1" ci="$2" vac="${3:-1}"
  local dir="$TMPD/arm-$slug-$ci-$vac"
  mkdir -p "$dir"
  cat "$TMPD/driver.head" "$TMPD/block-$slug.sh" > "$dir/driver.sh"
  EVID="$dir/evidence.jsonl" LOGD="$dir/logs" CELLS="$dir/cells.tsv" \
    CI_RC_IN="$ci" VACUOUS_IN="$vac" \
    timeout 60 bash "$dir/driver.sh" > "$dir/out.txt" 2> "$dir/err.txt"
  local drc=$?
  if [ "$drc" -ne 0 ]; then
    echo "  FAIL ci-pass block driver died (block=$slug act rc=$ci vacuous=$vac) rc=$drc"
    sed -n '1,6p' "$dir/err.txt"
    fail=$((fail+1))
  fi
  bash -c 'source "$1"; qa_run_summary "$2"' _ "$TMPD/summary.sh" "$dir/evidence.jsonl" \
    > "$TMPD/summary.txt" 2>/dev/null
  ARM_SUM_RC=$?
  cat "$dir/cells.tsv" 2>/dev/null
}
cell_status() { awk -F'\t' -v c="$1" '$1==c{print $2; exit}' "$TMPD/arm_rows"; }
cell_detail() { awk -F'\t' -v c="$1" '$1==c{print $3; exit}' "$TMPD/arm_rows"; }

# ── ARM 1 · act rc=1 with a PASSING native suite must FAIL the grade ────────
echo
echo "-- arm: act rc=1 + native suite PASS (the false green this row files) --"
run_arm ok 1 1 > "$TMPD/arm_rows"
cat "$TMPD/arm_rows"
check "act rc=1 + native PASS -> ci-pass status" "FAIL" "$(cell_status ci-pass)"
check_contains "ci-pass detail names the act rc" "act rc=1" "$(cell_detail ci-pass)"
check_contains "ci-pass detail names act_result=fail" "act_result=fail" "$(cell_detail ci-pass)"
check_contains "ci-pass detail is the act verdict" "act CI simulation FAILED" "$(cell_detail ci-pass)"
check "act rc=1 -> ci-act row status" "UNVERIFIED" "$(cell_status ci-act)"
check_contains "ci-act detail names act_result=fail" "act_result=fail" "$(cell_detail ci-act)"
check "GRADED OUTCOME (qa_run_summary rc) is non-zero for act rc=1" "1" "$ARM_SUM_RC"
check_contains "graded summary counts the act failure as a finding" "Failed: 1" "$(cat "$TMPD/summary.txt")"
check_contains "graded summary attributes it to an audit cell" "Audited: 1" "$(cat "$TMPD/summary.txt")"
check_absent "no OK cell for a failed act leg" "$(printf 'ci-pass\tOK')" "$(cat "$TMPD/arm_rows")"

# ── ARM 2 · act rc=0 grades OK on the act leg itself ────────────────────────
echo
echo "-- arm: act rc=0 (the leg itself is the grade) --"
run_arm ok 0 1 > "$TMPD/arm_rows"
cat "$TMPD/arm_rows"
check "act rc=0 -> ci-pass status" "OK" "$(cell_status ci-pass)"
check_contains "act rc=0 detail names act_result=pass" "act_result=pass" "$(cell_detail ci-pass)"
check "GRADED OUTCOME is zero for act rc=0" "0" "$ARM_SUM_RC"
check_contains "graded summary has no finding" "Failed: 0" "$(cat "$TMPD/summary.txt")"

# ── ARM 3 · act skipped (no triggerable workflow) keeps the native OK ───────
echo
echo "-- arm: act skipped (no triggerable workflow) --"
run_arm ok skip 1 > "$TMPD/arm_rows"
cat "$TMPD/arm_rows"
check "act skip -> ci-pass status" "OK" "$(cell_status ci-pass)"
check_contains "act skip detail names act_result=skip" "act_result=skip" "$(cell_detail ci-pass)"
check "GRADED OUTCOME is zero for act skip + native PASS" "0" "$ARM_SUM_RC"

# ── ARM 4 · act rc=1 AND native FAIL — both legs named, still one FAIL ──────
echo
echo "-- arm: act rc=1 + native suite FAIL --"
run_arm bad 1 1 > "$TMPD/arm_rows"
cat "$TMPD/arm_rows"
check "act rc=1 + native FAIL -> ci-pass status" "FAIL" "$(cell_status ci-pass)"
check_contains "detail names the native rc" "native rc=1" "$(cell_detail ci-pass)"
check_contains "detail names the act leg too" "act_result=fail" "$(cell_detail ci-pass)"
check "GRADED OUTCOME is non-zero when both legs fail" "1" "$ARM_SUM_RC"

# ── ARM 5 · act timeout (timeout(1) rc=124) fails and names the BUDGET ──────
echo
echo "-- arm: act leg hit its wall budget (rc=124) --"
run_arm ok 124 1 > "$TMPD/arm_rows"
cat "$TMPD/arm_rows"
check "act rc=124 -> ci-pass status" "FAIL" "$(cell_status ci-pass)"
check_contains "timeout detail names act_result=timeout" "act_result=timeout" "$(cell_detail ci-pass)"
check_contains "timeout detail names the budget, not a repo defect" "BUNKER_QA_ACT_TIMEOUT" "$(cell_detail ci-pass)"
check "GRADED OUTCOME is non-zero for an act timeout" "1" "$ARM_SUM_RC"

# ── ARM 6 · RESIDUAL (pinned, not celebrated): vacuous native suite ─────────
# With a vacuous native log the cell has no repo signal and grades UNVERIFIED
# (QA-BUNKER-29), so act's rc is not the deciding token there. Pinned so it can
# never drift into a silent OK; the graded summary is still non-zero (NOTHING
# AUDITED), so an act failure can never produce a green run.
echo
echo "-- residual: act rc=1 + vacuous native suite (UNVERIFIED, never OK) --"
run_arm ok 1 0 > "$TMPD/arm_rows"
cat "$TMPD/arm_rows"
check "vacuous native + act rc=1 -> ci-pass status" "UNVERIFIED" "$(cell_status ci-pass)"
check_absent "vacuous native + act rc=1 is never OK" "$(printf 'ci-pass\tOK')" "$(cat "$TMPD/arm_rows")"
check "vacuous native + act rc=1 -> graded outcome non-zero" "1" "$ARM_SUM_RC"

# ── static pins: CI wiring + the act-leg vocabulary the AC counts ───────────
echo
echo "-- static pins --"
check_atleast "harness act-leg/act_result references" 8 "$(grep -icE 'act leg|act_result' "$HARNESS")"
check_contains "harness grades on the act_result token" 'act_result=pass' "$(grep -F 'cell ci-pass OK' "$HARNESS")"
check_absent "false-green wording (native suite PASS + see ci-act) is gone" \
  'native suite PASS (see ci-act' "$(cat "$HARNESS")"
if [ -r "$CI_WF" ]; then
  echo "  ok   ci workflow readable: ${CI_WF##*/}"; pass=$((pass+1))
  if grep -Eq '^  gitreins-guard:$' "$CI_WF"; then
    echo "  ok   .github/workflows/ci.yml keeps the gitreins-guard JOB (the graded leg stays in CI)"
    pass=$((pass+1))
  else
    echo "  FAIL .github/workflows/ci.yml lost the 'gitreins-guard:' job — the graded leg left CI"
    fail=$((fail+1))
  fi
  if grep -q 'act_rc1_fails_grade' "$CI_WF"; then
    echo "  ok   ci.yml names this guard (act_rc1_fails_grade) next to that job"
    pass=$((pass+1))
  else
    echo "  FAIL ci.yml does not name act_rc1_fails_grade next to the gitreins-guard job"
    fail=$((fail+1))
  fi
else
  echo "  FAIL ci workflow not readable: $CI_WF"
  fail=$((fail+1))
fi

echo
echo "act_rc1_fails_grade: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
