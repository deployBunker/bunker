#!/bin/bash
# QA-BUNKER-B20 — regression test: the QA harness must REFUSE to run against a
# target host it cannot prove, and must never print SUCCESS over a run that
# audited nothing. Extracts the REAL functions from bunker-qa.sh (same pattern
# as test_bunker_qa_gofirst.sh / test_bunker_qa_pytestini_arm.sh) and drives
# them against fixtures — no live host, no network:
#
#   (a) unknown/removed host  -> qa_target_preflight rc!=0 + NAMED cause
#       "target host <name> not configured in ~/.bunker/config.yaml"; the ssh
#       stub proves ZERO probes ran (refusal precedes every step).
#   (b) configured-but-dead host -> ssh stub exits 255 -> rc!=0 + named cause
#       "SSH probe failed: target host <name> unreachable (exit=255)"; stub log
#       proves exactly ONE probe attempt.
#   (c) reachable host        -> ssh stub exits 0 -> rc=0.
#   (c2) target disk preflight -> 95% grades the launch cell FAIL before spawn;
#        40% passes and the preflight proceeds.
#   (d) harness-level refusal -> preflights() exits 2, writes exactly ONE FAIL
#       evidence row carrying the cause, before any spawn/step.
#   (e) honest summary        -> qa_run_summary() over fixture evidence files:
#         e1 mixed audit+wrapper  -> counts exclude wrapper cells (collect/
#                                    launch/run_battery/destroy-verify)
#         e2 wrapper-only (THE audited-nothing shape, all wrapper rows OK)
#                                 -> rc!=0 + "NOTHING AUDITED" in the text, so
#                                    a caller that ignores rc still cannot read
#                                    success; "Passed:"/"Failed:" fields remain
#                                    (additive format, consumers survive)
#         e3 INFO grades audit the environment (audited, no findings)
#         e4 FAIL verdicts        -> Findings == Failed count, rc!=0
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bunker-qa.sh}"
pass=0; fail=0
check() { # <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  ok   $1 -> $3"; pass=$((pass+1))
  else echo "  FAIL $1: expected [$2] got [$3]"; fail=$((fail+1)); fi
}
contains() { # <desc> <needle> <haystack>
  case "$3" in *"$2"*) echo "  ok   $1"; pass=$((pass+1)) ;;
  *) echo "  FAIL $1: missing [$2] in [$(printf '%s' "$3" | cut -c1-160)]"; fail=$((fail+1)) ;; esac
}

TMPD="${TMPDIR:-/tmp}/qa_bunker_b20.$$"
mkdir -p "$TMPD"
trap 'rm -rf "$TMPD"' EXIT

# ── extract the REAL functions from the harness ──
sed -n '/^qa_target_preflight() {/,/^}$/p' "$HARNESS" > "$TMPD/fns.sh"
sed -n '/^qa_disk_preflight() {/,/^}$/p'    "$HARNESS" >> "$TMPD/fns.sh"
sed -n '/^qa_run_summary() {/,/^}$/p'        "$HARNESS" >> "$TMPD/fns.sh"
sed -n '/^preflights() {/,/^}$/p'            "$HARNESS" >> "$TMPD/fns.sh"
for fn in qa_target_preflight qa_disk_preflight qa_run_summary preflights; do
  grep -q "^${fn}() {" "$TMPD/fns.sh" || { echo "FAIL: could not extract ${fn} from $HARNESS"; exit 1; }
done
grep -q '^}$' "$TMPD/fns.sh" || { echo "FAIL: extraction unbalanced"; exit 1; }
# QA-BUNKER-47 launch-record staging helpers (extracted separately: their absence
# is a GUARD failure reported by section (g) under its own name, not a suite-wide
# extraction abort).
sed -n '/^qa_launch_meta_reset() {/,/^}$/p' "$HARNESS" >> "$TMPD/fns.sh"
sed -n '/^qa_launch_meta_write() {/,/^}$/p' "$HARNESS" >> "$TMPD/fns.sh"

# ── fixtures ──
# config with ONE configured host (4-space indent, the shape ~/.bunker/config.yaml
# actually has; the harness greps '^ +<name>:').
mkdir -p "$TMPD/home/.bunker"
printf 'servers:\n    fixture-host:\n        name: fixture-host\n' > "$TMPD/home/.bunker/config.yaml"
# ssh stub: records every invocation, exits $SSH_RC (default 255 = unreachable)
mkdir -p "$TMPD/bin"
cat > "$TMPD/bin/ssh" <<'STUB'
#!/bin/bash
echo "$*" >> "${SSH_LOG:?SSH_LOG unset}"
if [ "${*: -1}" = "df -P /" ]; then
  printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/root 100 95 5 %s%% /\n' "${DF_USE:-40}"
fi
exit "${SSH_RC:-255}"
STUB
chmod +x "$TMPD/bin/ssh"
# fixture evidence rows (cell() emits one JSON object per line)
J() { printf '{"project":"p","cell":"%s","status":"%s","detail":"d","ts":"t%s"}\n' "$1" "$2" "$2"; }

echo "QA-BUNKER-B20: pre-flight the target host; never SUCCESS on zero audited steps"
echo

# ── (a) unknown/removed host: named refusal, ZERO probes ──
export SSH_LOG="$TMPD/ssh-a.log"; : > "$SSH_LOG"; export SSH_RC=255
out=$(HOME="$TMPD/home" PATH="$TMPD/bin:$PATH" BUNKER_QA_HOST_KEY="$TMPD/nokey" \
  bash -c "source '$TMPD/fns.sh'; qa_target_preflight ghost-host"); rc=$?
check "(a) unknown host rc" "1" "$rc"
contains "(a) named cause (not-configured)" \
  "target host ghost-host not configured in ~/.bunker/config.yaml" "$out"
check "(a) zero ssh probes ran (refusal precedes every step)" "0" "$(wc -l < "$SSH_LOG" | tr -d ' ')"

# missing config file entirely -> same refusal class
out=$(HOME="$TMPD/nohome" PATH="$TMPD/bin:$PATH" BUNKER_QA_HOST_KEY="$TMPD/nokey" \
  bash -c "source '$TMPD/fns.sh'; qa_target_preflight ghost-host"); rc=$?
check "(a2) missing config rc" "1" "$rc"
contains "(a2) named cause on missing config" \
  "target host ghost-host not configured in ~/.bunker/config.yaml" "$out"

# ── (b) configured-but-dead host: SSH probe named refusal ──
out=$(HOME="$TMPD/home" PATH="$TMPD/bin:$PATH" BUNKER_QA_HOST_KEY="$TMPD/nokey" \
  bash -c "source '$TMPD/fns.sh'; qa_target_preflight fixture-host"); rc=$?
check "(b) dead host rc" "1" "$rc"
contains "(b) named cause (SSH probe failed)" \
  "SSH probe failed: target host fixture-host unreachable (exit=255)" "$out"
check "(b) exactly one ssh probe attempt" "1" "$(wc -l < "$SSH_LOG" | tr -d ' ')"

# ── (c) reachable host: rc 0 ──
export SSH_RC=0
out=$(HOME="$TMPD/home" PATH="$TMPD/bin:$PATH" BUNKER_QA_HOST_KEY="$TMPD/nokey" \
  bash -c "source '$TMPD/fns.sh'; qa_target_preflight fixture-host"); rc=$?
check "(c) reachable host rc" "0" "$rc"
check "(c) no refusal text" "" "$out"

# ── (c2) target disk preflight: the launch cell fails above 94% ──
export SSH_RC=0
export DF_USE=95
: > "$SSH_LOG"
EV="$TMPD/ev-c2-red.jsonl"; : > "$EV"
out=$(HOME="$TMPD/home" PATH="$TMPD/bin:$PATH" BUNKER_QA_HOST_KEY="$TMPD/nokey" \
  SERVER="fixture-host" EVIDENCE="$EV" \
  bash -c "source '$TMPD/fns.sh'; preflights" 2>&1); rc=$?
check "(c2) 95% disk preflight rc" "2" "$rc"
contains "(c2) names disk threshold" "disk usage 95% exceeds 94% launch threshold" "$out"
contains "(c2) launch cell grades FAIL" '"cell":"launch","status":"FAIL"' "$(cat "$EV")"
check "(c2) disk check follows ssh reachability" "2" "$(wc -l < "$SSH_LOG" | tr -d ' ')"

# Healthy control: 40% passes the disk preflight and reaches the capacity probe.
export DF_USE=40
: > "$SSH_LOG"
EV="$TMPD/ev-c2-green.jsonl"; : > "$EV"
out=$(HOME="$TMPD/home" PATH="$TMPD/bin:$PATH" BUNKER_QA_HOST_KEY="$TMPD/nokey" \
  SERVER="fixture-host" EVIDENCE="$EV" \
  bash -c "source '$TMPD/fns.sh'; qa_disk_preflight fixture-host" 2>&1); rc=$?
check "(c2-control) 40% disk preflight rc" "0" "$rc"
contains "(c2-control) healthy disk output" "disk usage 40% is within 94% launch threshold" "$out"
check "(c2-control) disk probe issued" "1" "$(wc -l < "$SSH_LOG" | tr -d ' ')"

# ── (d) harness-level refusal: preflights() exits 2 with ONE FAIL row ──
export SSH_RC=255
EV="$TMPD/ev-d.jsonl"; : > "$EV"
out=$(HOME="$TMPD/home" PATH="$TMPD/bin:$PATH" BUNKER_QA_HOST_KEY="$TMPD/nokey" \
  SERVER="ghost-host" EVIDENCE="$EV" PROJ=f \
  bash -c "source '$TMPD/fns.sh'; preflights" 2>&1); rc=$?
check "(d) preflights rc" "2" "$rc"
contains "(d) stderr names the cause" "target host ghost-host not configured" "$out"
check "(d) evidence rows written" "1" "$(grep -c '^{' "$EV")"
contains "(d) evidence row carries the cause" \
  '"status":"FAIL"' "$(cat "$EV")"
contains "(d) evidence row names the host" "target host ghost-host not configured" "$(cat "$EV")"

# ── (e) honest summary ──
# e1: mixed audit + wrapper rows — wrapper cells NEVER count as audit signal
{ J fresh-install OK; J chaos-resource FAIL; J ci-pass UNVERIFIED; J collect OK; } > "$TMPD/ev1.jsonl"
out=$(bash -c "source '$TMPD/fns.sh'; qa_run_summary '$TMPD/ev1.jsonl'"); rc=$?
check "(e1) mixed rc (findings=1)" "1" "$rc"
contains "(e1) audited excludes wrapper+UNVERIFIED" \
  "Passed: 1 | Failed: 1 | Audited: 2 | Findings: 1 | NotVerified: 1" "$out"
contains "(e1) format stays parseable (Passed:)" "Passed:" "$out"
contains "(e1) format stays parseable (Failed:)" "Failed:" "$out"
# e2: wrapper-only file = THE audited-nothing shape — never a SUCCESS verdict
{ J launch OK; J collect OK; } > "$TMPD/ev2.jsonl"
out=$(bash -c "source '$TMPD/fns.sh'; qa_run_summary '$TMPD/ev2.jsonl'"); rc=$?
check "(e2) wrapper-only rc nonzero" "1" "$rc"
contains "(e2) verdict text carries NOTHING AUDITED" "NOTHING AUDITED" "$out"
contains "(e2) counts still reported" "Audited: 0" "$out"
# e3: INFO grades audit the environment (audited, zero findings)
{ J toolchain-bootstrap INFO; J fresh-install OK; } > "$TMPD/ev3.jsonl"
out=$(bash -c "source '$TMPD/fns.sh'; qa_run_summary '$TMPD/ev3.jsonl'"); rc=$?
check "(e3) INFO-audited rc" "0" "$rc"
contains "(e3) INFO counts as audited" "Audited: 2 | Findings: 0" "$out"
# e4: FAIL verdicts are findings
{ J fresh-install FAIL; J chaos-resource FAIL; J ci-pass OK; } > "$TMPD/ev4.jsonl"
out=$(bash -c "source '$TMPD/fns.sh'; qa_run_summary '$TMPD/ev4.jsonl'"); rc=$?
check "(e4) findings rc nonzero" "1" "$rc"
contains "(e4) findings counted" "Failed: 2 | Audited: 3 | Findings: 2" "$out"

# ── (f) QA-BUNKER-81: fresh-install artifact guard ──
# The generated remote cell must compare the executable install destination
# generically.  A make verify command that exits 0 but creates no artifact must
# be a FAIL row with rc=1; toolsd remains an allowed artifact name, not the
# only artifact name.  These source assertions are intentionally independent
# of a bunker agent and protect the generated-script contract.
qa="$(cat "$HARNESS")"
contains "(f1) missing-artifact message is present" "no artifact found" "$qa"
contains "(f2) missing-artifact cell rc is nonzero" "rc=1: install rc=0 but no artifact found" "$qa"
contains "(f3) artifact check snapshots PATH/bin" "install_artifact_snapshot" "$qa"
contains "(f4) toolsd remains an allowed install artifact" "toolsd is allowed when present" "$qa"
if printf '%s' "$qa" | grep -qE 'command -v toolsd|/bin/toolsd'; then
  echo "  FAIL (f5) artifact check still hard-codes toolsd"; fail=$((fail+1))
else
  echo "  ok   (f5) artifact check does not hard-code toolsd"; pass=$((pass+1))
fi

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
