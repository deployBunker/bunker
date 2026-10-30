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

# ── (g) QA-BUNKER-47: preflight_fail_writes_no_launch_meta ──
# A launch refused BEFORE spawn (config / ssh / reachability -> rc=2) must leave
# NO $EVIDENCE.meta launch record. The pre-fix prologue wrote one unconditionally
# with launched=/server=/project= and an EMPTY agent=, so a follow-up collect
# graded a healthy refusal "the launch was killed before the agent id was
# recorded" — a crash story for a run that never spawned anything, and exactly
# the stale-record shape TR-085 warns about.
lt() { # <desc> <a> <b> — assert 0 < a < b (both must be line numbers)
  if [ -n "$2" ] && [ -n "$3" ] && [ "$2" -gt 0 ] && [ "$3" -gt 0 ] && [ "$2" -lt "$3" ]; then
    echo "  ok   $1 ($2 < $3)"; pass=$((pass+1))
  else
    echo "  FAIL $1: expected [$2] < [$3]"; fail=$((fail+1))
  fi
}
no_launch_meta() { # <desc> <meta-path>
  if [ -f "$2" ]; then
    echo "  FAIL $1: launch record exists -> $(tr '\n' ' ' < "$2")"; fail=$((fail+1))
  else
    echo "  ok   $1"; pass=$((pass+1))
  fi
}

echo
echo "  (g) QA-BUNKER-47: preflight_fail_writes_no_launch_meta"

# g1: the staging helpers are real functions in the harness, and the writer keeps
# emitting the documented record fields — deleting the writer is NOT a valid way
# to pass this guard (the row requires the meta writer to stay present).
for fn in qa_launch_meta_reset qa_launch_meta_write; do
  check "(g1) $fn is a harness function" "1" "$(grep -c "^${fn}() {" "$TMPD/fns.sh" | tr -d ' ')"
done
writer_src=$(sed -n '/^qa_launch_meta_write() {/,/^}$/p' "$HARNESS")
contains "(g1b) meta writer still prints launched=" "launched=%s" "$writer_src"
contains "(g1b) meta writer still prints agent=" "agent=%s" "$writer_src"
contains "(g1b) meta writer stamps phase=" "phase=%s" "$writer_src"

# g2: ORDER inside launch() — the record is invalidated before the preflights,
# and the first write happens AFTER they pass (a pre-spawn refusal therefore
# leaves nothing behind).
sed -n '/^launch() {/,/^}$/p' "$HARNESS" > "$TMPD/launch.body"
g_pre=$(grep -n '^  preflights$' "$TMPD/launch.body" | head -1 | cut -d: -f1)
g_reset=$(grep -n 'qa_launch_meta_reset' "$TMPD/launch.body" | head -1 | cut -d: -f1)
g_write=$(grep -n 'qa_launch_meta_write' "$TMPD/launch.body" | head -1 | cut -d: -f1)
lt "(g2) launch invalidates a stale record before preflights" "$g_reset" "$g_pre"
lt "(g2) launch writes the record only after preflights pass" "$g_pre" "$g_write"

# g3: end-to-end refusal. A real `launch` against an unreachable (but configured)
# fixture host exits 2 at the ssh preflight, records the FAIL row, and must leave
# no launch record on disk.
mkdir -p "$TMPD/g47/home/.bunker" "$TMPD/g47/repo" "$TMPD/g47/run"
printf 'servers:\n    fixture-host:\n        name: fixture-host\n' > "$TMPD/g47/home/.bunker/config.yaml"
git -C "$TMPD/g47/repo" init -q 2>/dev/null || mkdir -p "$TMPD/g47/repo/.git"
export SSH_RC=255
export SSH_LOG="$TMPD/g47/ssh.log"; : > "$SSH_LOG"
EV47="$TMPD/g47/evidence.jsonl"
run_refused_launch() { # <evidence> — one refused launch, stdout+stderr on stdout
  HOME="$TMPD/g47/home" PATH="$TMPD/bin:$PATH" BUNKER_QA_HOST_KEY="$TMPD/nokey" \
  BUNKER_QA_SERVER=fixture-host BUNKER_QA_EVIDENCE="$1" SSH_LOG="$TMPD/g47/ssh.log" \
  SSH_RC=255 bash "$HARNESS" launch "$TMPD/g47/repo" 2>&1
}
out=$(run_refused_launch "$EV47"); rc=$?
check "(g3) refused launch rc (preflight exit 2)" "2" "$rc"
contains "(g3) preflight FAIL row is the verdict" '"status":"FAIL"' "$(cat "$EV47" 2>/dev/null)"
contains "(g3) the row says no spawn was attempted" "no spawn attempted" "$(cat "$EV47" 2>/dev/null)"
no_launch_meta "(g3) preflight_fail_writes_no_launch_meta" "$EV47.meta"

# g4: TR-085 hazard — a STALE record at the same evidence path must not survive a
# refused launch (collect would otherwise probe the stranger's agent id).
printf 'agent=stale-agent\nserver=fixture-host\nevidence=%s\nlaunched=2020-01-01T00:00:00Z\nkeep=0\nproject=stale\nphase=launched\n' "$EV47" > "$EV47.meta"
out=$(run_refused_launch "$EV47"); rc=$?
check "(g4) refused launch over a stale record rc" "2" "$rc"
no_launch_meta "(g4) refused launch drops the stale record" "$EV47.meta"

# g5: POSITIVE control — the writer still produces a collect-usable record once a
# run does reach spawn/launch (so the guard cannot pass by never writing).
out=$(bash -c "source '$TMPD/fns.sh'; qa_launch_meta_write '$TMPD/g47/ctl.meta' '$TMPD/g47/ctl.jsonl' launched qa-fixture-0001 fixture-host 1 demo"); rc=$?
check "(g5) writer rc" "0" "$rc"
ctl=$(cat "$TMPD/g47/ctl.meta" 2>/dev/null)
contains "(g5) record names the agent" "agent=qa-fixture-0001" "$ctl"
contains "(g5) record names the server" "server=fixture-host" "$ctl"
contains "(g5) record names the evidence path" "evidence=$TMPD/g47/ctl.jsonl" "$ctl"
contains "(g5) record carries launched=" "launched=" "$ctl"
contains "(g5) record carries phase=launched" "phase=launched" "$ctl"
contains "(g5) record names the project" "project=demo" "$ctl"

# g6: the empty-agent= story is honest — only a phase=spawning record (the run
# died INSIDE spawn) may be graded a killed launch. Assertions use tokens that
# survive the harness's own detail sanitiser (`tr '\\n' ' '`, which also maps a
# literal "n" to a space), so they pin the DISTINCTION, not the punctuation.
EV6="$TMPD/g47/collect.jsonl"; : > "$EV6"
printf 'agent=\nserver=fixture-host\nevidence=%s\nlaunched=%s\nkeep=0\nproject=demo\nphase=spawning\n' \
  "$EV6" "$(date -u +%FT%TZ)" > "$EV6.meta"
out=$(HOME="$TMPD/g47/home" PATH="$TMPD/bin:$PATH" BUNKER_QA_HOST_KEY="$TMPD/nokey" \
  BUNKER_QA_SERVER=fixture-host bash "$HARNESS" collect --evidence "$EV6" 2>&1); rc=$?
check "(g6) collect rc on a spawn-time record" "2" "$rc"
contains "(g6) message says the run died inside spawn" "INSIDE" "$out"
if printf '%s' "$out" | grep -q 'killed'; then
  echo "  FAIL (g6) spawn-time record still graded a killed launch"; fail=$((fail+1))
else
  echo "  ok   (g6) spawn-time record is not graded a killed launch"; pass=$((pass+1))
fi

# g7: backward compatibility — a LEGACY record with no phase= header (written by
# an older harness) keeps the historical wording, so this is a distinction and
# not a blanket rewording that hides real spawn-time kills.
EV7="$TMPD/g47/legacy.jsonl"; : > "$EV7"
printf 'agent=\nserver=fixture-host\nevidence=%s\nlaunched=%s\nkeep=0\nproject=demo\n' \
  "$EV7" "$(date -u +%FT%TZ)" > "$EV7.meta"
out=$(HOME="$TMPD/g47/home" PATH="$TMPD/bin:$PATH" BUNKER_QA_HOST_KEY="$TMPD/nokey" \
  BUNKER_QA_SERVER=fixture-host bash "$HARNESS" collect --evidence "$EV7" 2>&1); rc=$?
check "(g7) collect rc on a legacy record" "2" "$rc"
contains "(g7) legacy record keeps the historical wording" "killed" "$out"

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
