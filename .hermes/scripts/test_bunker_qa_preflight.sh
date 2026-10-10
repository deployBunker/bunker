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
sed -n '/^qa_run_summary() {/,/^}$/p'        "$HARNESS" >> "$TMPD/fns.sh"
sed -n '/^preflights() {/,/^}$/p'            "$HARNESS" >> "$TMPD/fns.sh"
for fn in qa_target_preflight qa_run_summary preflights; do
  grep -q "^${fn}() {" "$TMPD/fns.sh" || { echo "FAIL: could not extract ${fn} from $HARNESS"; exit 1; }
done
grep -q '^}$' "$TMPD/fns.sh" || { echo "FAIL: extraction unbalanced"; exit 1; }

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

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
