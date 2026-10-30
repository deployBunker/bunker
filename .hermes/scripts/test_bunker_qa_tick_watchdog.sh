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

# Orphan path: a dead PID is classified before any agent communication.
BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock dead-agent 999999 fixture-host '$TMPD/dead.jsonl' >/dev/null; qa_tick_watchdog_state dead-agent" 2>/dev/null
check "dead tick is classified orphaned" 1 "$?"
check "dead tick state is not accepted" 1 "$(BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c "source '$TMPD/watchdog.sh'; qa_tick_watchdog_state dead-agent >/dev/null 2>&1; printf '%s' \$?")"

# Cleanup path: an old dead lock is quarantined, destroyed, then removed only
# after list proves absence. The fresh lock proves TTL detection does not reap
# an interrupted-but-recent launch.
: > "$TMPD/bunker.log"
printf '{"cell":"battery","status":"PASS"}\n' > "$TMPD/cleanup.jsonl"
old_lock=$(BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock cleanup-agent 999999 fixture-host '$TMPD/cleanup.jsonl'")
old_time=$(date -u -d '3 hours ago' +%FT%TZ)
sed -i "s/^created=.*/created=$old_time/" "$old_lock"
BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c "source '$TMPD/watchdog.sh'; qa_cleanup_orphan_agents" 2>"$TMPD/cleanup.err"
check "cleanup destroys old dead-tick agent" 1 "$(grep -c '^destroy cleanup-agent' "$TMPD/bunker.log")"
check "cleanup quarantines before destroy" 1 "$(grep -c '^quarantine cleanup-agent' "$TMPD/bunker.log")"
quarantine_line=$(grep -n '^quarantine cleanup-agent' "$TMPD/bunker.log" | cut -d: -f1)
destroy_line=$(grep -n '^destroy cleanup-agent' "$TMPD/bunker.log" | cut -d: -f1)
check "evidence is preserved before destroy" yes "$([ "$quarantine_line" -lt "$destroy_line" ] && echo yes || echo no)"
check "quarantined evidence file exists" yes "$(find "$TMPD/quarantine" -name agent.log -type f | grep -q . && echo yes || echo no)"
check "cleanup lists after destroy" 1 "$(grep -c '^list ' "$TMPD/bunker.log")"
check "cleanup removes reaped lock" no "$([ -f "$TMPD/locks/cleanup-agent.lock" ] && echo yes || echo no)"
contains "cleanup reason is named" "tick-death-orphan" "$(cat "$TMPD/cleanup.err")"
recent_lock=$(BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c "source '$TMPD/watchdog.sh'; qa_write_agent_lock recent-agent 999999 fixture-host '$TMPD/recent.jsonl'")
: > "$TMPD/bunker.log"
BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c "source '$TMPD/watchdog.sh'; qa_cleanup_orphan_agents"
check "recent dead tick is protected by TTL" 0 "$(grep -c '^destroy recent-agent' "$TMPD/bunker.log" || true)"

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
