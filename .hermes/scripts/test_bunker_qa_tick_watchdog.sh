#!/usr/bin/env bash
# QA-BUNKER-75 — tick-death watchdog regression battery.
# Exercises the real watchdog helpers from bunker-qa.sh with local stubs only:
#   normal: a live tick keeps its lock and cleanup leaves it alone;
#   orphan: collect's pre-communication guard sees a dead tick and destroys
#            without making an SSH request;
#   cleanup: battery-start sweep destroys a previous dead-tick agent and
#             removes its lock only after list proves absence.
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bunker-qa.sh}"
pass=0
fail=0
check() {
  if [ "$2" = "$3" ]; then
    echo "  ok   $1 -> $3"
    pass=$((pass + 1))
  else
    echo "  FAIL $1: expected [$2] got [$3]"
    fail=$((fail + 1))
  fi
}
contains() {
  case "$3" in
    *"$2"*) echo "  ok   $1"; pass=$((pass + 1)) ;;
    *) echo "  FAIL $1: missing [$2]"; fail=$((fail + 1)) ;;
  esac
}

TMPD="${TMPDIR:-/tmp}/qa_bunker_75.$$"
mkdir -p "$TMPD/bin" "$TMPD/home/.bunker"
trap 'rm -rf "$TMPD"' EXIT

# Extract the real watchdog helpers, not a reimplementation.
sed -n '/^qa_orphan_lock_dir() {/,/^}$/p' "$HARNESS" > "$TMPD/watchdog.sh"
sed -n '/^qa_agent_lock_path() {/,/^}$/p' "$HARNESS" >> "$TMPD/watchdog.sh"
sed -n '/^qa_write_agent_lock() {/,/^}$/p' "$HARNESS" >> "$TMPD/watchdog.sh"
sed -n '/^qa_remove_agent_lock() {/,/^}$/p' "$HARNESS" >> "$TMPD/watchdog.sh"
sed -n '/^qa_tick_pid_alive() {/,/^}$/p' "$HARNESS" >> "$TMPD/watchdog.sh"
sed -n '/^qa_tick_watchdog_state() {/,/^}$/p' "$HARNESS" >> "$TMPD/watchdog.sh"
sed -n '/^qa_lock_value() {/,/^}$/p' "$HARNESS" >> "$TMPD/watchdog.sh"
sed -n '/^qa_watchdog_ttl_seconds() {/,/^}$/p' "$HARNESS" >> "$TMPD/watchdog.sh"
sed -n '/^qa_lock_age_seconds() {/,/^}$/p' "$HARNESS" >> "$TMPD/watchdog.sh"
sed -n '/^qa_quarantine_orphan_evidence() {/,/^}$/p' "$HARNESS" >> "$TMPD/watchdog.sh"
sed -n '/^qa_orphan_lock_eligible() {/,/^}$/p' "$HARNESS" >> "$TMPD/watchdog.sh"
sed -n '/^qa_cleanup_orphan_agents() {/,/^}$/p' "$HARNESS" >> "$TMPD/watchdog.sh"
bash -n "$TMPD/watchdog.sh" || { echo "FAIL: watchdog helper extraction does not parse"; exit 1; }

# bunker stub: destroy records ids; list reports the agent only when LIST_HAS is 1.
cat > "$TMPD/bin/bunker" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  destroy) printf 'destroy %s server=%s\n' "$2" "${4:-}" >> "${BUNKER_LOG:?}"; exit "${DESTROY_RC:-0}" ;;
  list) printf 'list server=%s\n' "${3:-}" >> "${BUNKER_LOG:?}"; [ "${LIST_HAS:-0}" = 1 ] && printf '%s\n' "${LIST_AGENT:?}"; exit 0 ;;
  *) exit 0 ;;
esac
STUB
chmod +x "$TMPD/bin/bunker"
cat > "$TMPD/bin/quarantine" <<'STUB'
#!/usr/bin/env bash
id="$1"; destination="$3"; evidence="$4"
printf 'quarantine %s\n' "$id" >> "${BUNKER_LOG:?}"
printf 'agent log for %s\n' "$id" > "$destination/agent.log"
[ -z "$evidence" ] || cp -p "$evidence" "$destination/evidence-copy.jsonl"
STUB
chmod +x "$TMPD/bin/quarantine"
: > "$TMPD/bunker.log"

export BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks"
export BUNKER_QA_QUARANTINE_DIR="$TMPD/quarantine"
export BUNKER_QA_QUARANTINE_CMD="$TMPD/bin/quarantine"
export BUNKER_QA_ORPHAN_TTL=2h
export PATH="$TMPD/bin:$PATH"
export BUNKER_LOG="$TMPD/bunker.log"

# Spawn contract: lock includes tick PID and agent ID with private permissions.
normal_pid=$$
lock=$(BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock normal-agent '$normal_pid' fixture-host '$TMPD/normal.jsonl'")
check "spawn writes lock" yes "$([ -f "$lock" ] && echo yes || echo no)"
contains "lock records agent id" 'agent=normal-agent' "$(cat "$lock")"
contains "lock records tick pid" "tick_pid=$normal_pid" "$(cat "$lock")"
check "lock permissions are private" 600 "$(stat -c '%a' "$lock")"

# Normal path: live PID is not orphaned and cleanup must not call destroy.
bash -c "source '$TMPD/watchdog.sh'; qa_tick_watchdog_state normal-agent"
check "live tick is accepted" 0 "$?"
LIST_AGENT=normal-agent LIST_HAS=1 bash -c "source '$TMPD/watchdog.sh'; qa_cleanup_orphan_agents" 2>"$TMPD/normal.err"
check "live tick cleanup does not destroy" 0 "$(grep -c '^destroy ' "$TMPD/bunker.log" || true)"

# Orphan path: an old dead PID is classified before any agent communication.
dead_lock=$(BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock dead-agent 999999 fixture-host '$TMPD/dead.jsonl'")
sed -i "s/^created=.*/$(date -u -d '3 hours ago' +created=%FT%TZ)/" "$dead_lock"
BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c "source '$TMPD/watchdog.sh'; qa_tick_watchdog_state dead-agent" 2>/dev/null
check "expired dead tick is classified orphaned" 1 "$?"
check "expired dead tick state is not accepted" 1 "$(BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c "source '$TMPD/watchdog.sh'; qa_tick_watchdog_state dead-agent >/dev/null 2>&1; printf '%s' \$?")"

# Cleanup path uses its own lock directory so live/dead classification fixtures
# from the earlier cases cannot pollute the exact post-destroy list assertion.
CLEANUP_LOCKS="$TMPD/cleanup-locks"
mkdir -p "$CLEANUP_LOCKS"
: > "$TMPD/bunker.log"
printf '{"cell":"battery","status":"PASS"}\n' > "$TMPD/cleanup.jsonl"
old_lock=$(BUNKER_QA_ORPHAN_LOCK_DIR="$CLEANUP_LOCKS" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock cleanup-agent 999999 fixture-host '$TMPD/cleanup.jsonl'")
old_time=$(date -u -d '3 hours ago' +%FT%TZ)
sed -i "s/^created=.*/created=$old_time/" "$old_lock"
BUNKER_QA_ORPHAN_LOCK_DIR="$CLEANUP_LOCKS" bash -c "source '$TMPD/watchdog.sh'; qa_cleanup_orphan_agents" 2>"$TMPD/cleanup.err"
check "cleanup destroys old dead-tick agent" 1 "$(grep -c '^destroy cleanup-agent' "$TMPD/bunker.log")"
check "cleanup quarantines before destroy" 1 "$(grep -c '^quarantine cleanup-agent' "$TMPD/bunker.log")"
quarantine_line=$(grep -n '^quarantine cleanup-agent' "$TMPD/bunker.log" | cut -d: -f1)
destroy_line=$(grep -n '^destroy cleanup-agent' "$TMPD/bunker.log" | cut -d: -f1)
check "evidence is preserved before destroy" yes "$([ "$quarantine_line" -lt "$destroy_line" ] && echo yes || echo no)"
check "quarantined evidence file exists" yes "$(find "$TMPD/quarantine" -name agent.log -type f | grep -q . && echo yes || echo no)"
check "cleanup lists after destroy" 1 "$(grep -c '^list ' "$TMPD/bunker.log")"
check "cleanup removes reaped lock" no "$([ -f "$CLEANUP_LOCKS/cleanup-agent.lock" ] && echo yes || echo no)"
contains "cleanup reason is named" "tick-death-orphan" "$(cat "$TMPD/cleanup.err")"
recent_lock=$(BUNKER_QA_ORPHAN_LOCK_DIR="$CLEANUP_LOCKS" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock recent-agent 999999 fixture-host '$TMPD/recent.jsonl'")
: > "$TMPD/bunker.log"
BUNKER_QA_ORPHAN_LOCK_DIR="$CLEANUP_LOCKS" bash -c "source '$TMPD/watchdog.sh'; qa_cleanup_orphan_agents"
check "recent dead tick is protected by TTL" 0 "$(grep -c '^destroy recent-agent' "$TMPD/bunker.log" || true)"
BUNKER_QA_ORPHAN_LOCK_DIR="$CLEANUP_LOCKS" bash -c "source '$TMPD/watchdog.sh'; qa_tick_watchdog_state recent-agent" 2>/dev/null
check "recent dead tick has distinct retry state" 3 "$?"
future_lock=$(BUNKER_QA_ORPHAN_LOCK_DIR="$CLEANUP_LOCKS" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock future-agent 999999 fixture-host '$TMPD/future.jsonl'")
sed -i "s/^created=.*/created=$(date -u -d '1 hour ago' +%FT%TZ)/" "$future_lock"
# A future timestamp is invalid for the state classifier, even if the PID is dead.
sed -i "s/^created=.*/created=$(date -u -d '1 hour ago' +%FT%TZ)/" "$future_lock"
# Use an explicit impossible future value rather than relying on clock granularity.
sed -i "s/^created=.*/created=2999-01-01T00:00:00Z/" "$future_lock"
BUNKER_QA_ORPHAN_LOCK_DIR="$CLEANUP_LOCKS" bash -c "source '$TMPD/watchdog.sh'; qa_tick_watchdog_state future-agent" 2>/dev/null
check "future timestamp fails closed" 2 "$?"
malformed_lock=$(BUNKER_QA_ORPHAN_LOCK_DIR="$CLEANUP_LOCKS" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock malformed-agent 999999 fixture-host '$TMPD/malformed.jsonl'")
sed -i 's/^created=.*/created=not-a-timestamp/' "$malformed_lock"
BUNKER_QA_ORPHAN_LOCK_DIR="$CLEANUP_LOCKS" bash -c "source '$TMPD/watchdog.sh'; qa_tick_watchdog_state malformed-agent" 2>/dev/null
check "invalid timestamp fails closed" 2 "$?"

# Actual collect integration: exercise the command entry point, not extracted helpers.
# The expired dead lock must quarantine evidence before destroy, emit the named
# harness failure, skip the normal SSH probe, and then remove the lock.
COLLECT_LOCKS="$TMPD/collect-locks"
COLLECT_ROOT="$TMPD/collect-quarantine"
mkdir -p "$COLLECT_LOCKS" "$COLLECT_ROOT"
collect_evidence="$TMPD/collect.jsonl"
printf '{"cell":"launch","status":"OK"}\n' > "$collect_evidence"
{
  printf '%s\n' 'agent=collect-agent'
  printf '%s\n' 'server=fixture-host'
  printf 'evidence=%s\n' "$collect_evidence"
  printf 'launched=%s\n' "$(date -u -d '3 hours ago' +%FT%TZ)"
  printf '%s\n' 'keep=0' 'project=fixture' 'phase=launched'
} > "$collect_evidence.meta"
collect_lock=$(BUNKER_QA_ORPHAN_LOCK_DIR="$COLLECT_LOCKS" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock collect-agent 999999 fixture-host '$collect_evidence'")
sed -i "s/^created=.*/created=$(date -u -d '3 hours ago' +%FT%TZ)/" "$collect_lock"
: > "$TMPD/collect-bunker.log"
: > "$TMPD/collect-ssh.log"
collect_rc=0
BUNKER_QA_ORPHAN_LOCK_DIR="$COLLECT_LOCKS" BUNKER_QA_QUARANTINE_DIR="$COLLECT_ROOT" BUNKER_QA_QUARANTINE_CMD="$TMPD/bin/quarantine" BUNKER_QA_EVIDENCE="$collect_evidence" BUNKER_QA_ORPHAN_TTL=2h "$HARNESS" collect --evidence "$collect_evidence" >"$TMPD/collect.out" 2>"$TMPD/collect.err" || collect_rc=$?
check "collect expired orphan returns harness failure" 2 "$collect_rc"
contains "collect names tick-death-orphan" "tick-death-orphan" "$(cat "$collect_evidence")"
contains "collect records harness-failure" '"cell":"harness-failure"' "$(cat "$collect_evidence")"
check "collect quarantines evidence" 1 "$(grep -c '^quarantine collect-agent' "$TMPD/bunker.log" || true)"
check "collect destroys expired orphan" 1 "$(grep -c '^destroy collect-agent' "$TMPD/bunker.log" || true)"
check "collect skips normal agent probe" 0 "$(grep -c . "$TMPD/collect-ssh.log" || true)"
check "collect removes destroyed orphan lock" no "$([ -f "$COLLECT_LOCKS/collect-agent.lock" ] && echo yes || echo no)"
check "collect evidence was quarantined" yes "$(find "$COLLECT_ROOT" -name evidence.jsonl -type f | grep -q . && echo yes || echo no)"

# Recent dead locks use the same real collect path but must be retained for a
# retry-later decision; no quarantine, destroy, or SSH probe is permitted.
recent_evidence="$TMPD/recent-collect.jsonl"
printf '{"cell":"launch","status":"OK"}\n' > "$recent_evidence"
{
  printf '%s\n' 'agent=recent-collect'
  printf '%s\n' 'server=fixture-host'
  printf 'evidence=%s\n' "$recent_evidence"
  printf 'launched=%s\n' "$(date -u +%FT%TZ)"
  printf '%s\n' 'keep=0' 'project=fixture' 'phase=launched'
} > "$recent_evidence.meta"
BUNKER_QA_ORPHAN_LOCK_DIR="$COLLECT_LOCKS" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock recent-collect 999999 fixture-host '$recent_evidence'" >/dev/null
: > "$TMPD/bunker.log"
: > "$TMPD/collect-ssh.log"
recent_collect_rc=0
BUNKER_QA_ORPHAN_LOCK_DIR="$COLLECT_LOCKS" BUNKER_QA_QUARANTINE_DIR="$COLLECT_ROOT" BUNKER_QA_QUARANTINE_CMD="$TMPD/bin/quarantine" BUNKER_QA_EVIDENCE="$recent_evidence" BUNKER_QA_ORPHAN_TTL=2h "$HARNESS" collect --evidence "$recent_evidence" >"$TMPD/recent.out" 2>"$TMPD/recent.err" || recent_collect_rc=$?
check "collect recent orphan returns retry-later failure" 2 "$recent_collect_rc"
contains "collect names recent-orphan retry" "retry collect later" "$(cat "$recent_evidence")"
check "collect retains recent orphan lock" yes "$([ -f "$COLLECT_LOCKS/recent-collect.lock" ] && echo yes || echo no)"
check "collect does not destroy recent orphan" 0 "$(grep -c '^destroy recent-collect' "$TMPD/bunker.log" || true)"
check "collect does not quarantine recent orphan" 0 "$(grep -c '^quarantine recent-collect' "$TMPD/bunker.log" || true)"
check "collect skips probe for recent orphan" 0 "$(grep -c . "$TMPD/collect-ssh.log" || true)"

# RED proof: disabling the watchdog dispatch makes the expired collect lose the
# named orphan handling, so this acceptance battery would fail on the old shape.
red_harness="$TMPD/bunker-qa-red.sh"
cp "$HARNESS" "$red_harness"
sed -i 's/  qa_tick_watchdog_state "$agent" || watchdog_state=$?/  watchdog_state=0 # RED proof: watchdog disabled/' "$red_harness"
red_evidence="$TMPD/red.jsonl"
printf '{"cell":"launch","status":"OK"}\n' > "$red_evidence"
{
  printf '%s\n' 'agent=red-agent'
  printf '%s\n' 'server=fixture-host'
  printf 'evidence=%s\n' "$red_evidence"
  printf 'launched=%s\n' "$(date -u -d '3 hours ago' +%FT%TZ)"
  printf '%s\n' 'keep=0' 'project=fixture' 'phase=launched'
} > "$red_evidence.meta"
BUNKER_QA_ORPHAN_LOCK_DIR="$COLLECT_LOCKS" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock red-agent 999999 fixture-host '$red_evidence'" >/dev/null
: > "$TMPD/bunker.log"
red_rc=0
BUNKER_QA_ORPHAN_LOCK_DIR="$COLLECT_LOCKS" BUNKER_QA_QUARANTINE_DIR="$COLLECT_ROOT" BUNKER_QA_QUARANTINE_CMD="$TMPD/bin/quarantine" BUNKER_QA_EVIDENCE="$red_evidence" BUNKER_QA_ORPHAN_TTL=2h "$red_harness" collect --evidence "$red_evidence" >"$TMPD/red.out" 2>"$TMPD/red.err" || red_rc=$?
red_proof=0
[ "$red_rc" -ne 0 ] && ! grep -q 'tick-death-orphan' "$red_evidence" && [ "$(grep -c '^destroy red-agent' "$TMPD/bunker.log" || true)" -eq 0 ] && red_proof=1
check "RED proof: disabling watchdog fails collect acceptance" 1 "$red_proof"

# Source-level wiring guards the call sites that the helper-only tests cannot prove.
source_text=$(cat "$HARNESS")
contains "spawn path writes watchdog lock" "qa_write_agent_lock" "$source_text"
contains "collect checks watchdog before probe" "qa_tick_watchdog_state" "$source_text"
contains "battery start sweeps watchdog locks" "qa_cleanup_orphan_agents" "$source_text"

# The lock must not be removed when daemon list still proves the agent exists.
: > "$TMPD/bunker.log"
LIST_AGENT=stuck-agent LIST_HAS=1 BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock stuck-agent 999999 fixture-host '$TMPD/stuck.jsonl' >/dev/null; qa_cleanup_orphan_agents" 2>"$TMPD/stuck.err"
check "unproven destroy retains lock" yes "$([ -f "$TMPD/locks/stuck-agent.lock" ] && echo yes || echo no)"

printf '\npass=%s fail=%s\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
