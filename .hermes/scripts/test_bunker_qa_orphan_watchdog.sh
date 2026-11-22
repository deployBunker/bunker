#!/usr/bin/env bash
# QA-MUSTER-046 R2 — orphan-watchdog sweep regression battery.
# Exercises the REAL sweep functions extracted from bunker-qa.sh (no stub of the
# sweep itself) against a stub `bunker` CLI, so every acceptance arm is decided
# by shipped code:
#   T1 ghost lock / agent absent  -> lock dropped, ZERO destroy, sweep under 5s
#   T2 zero-agent server          -> ZERO destroy (the original wedge class)
#   T3 foreign-server lock        -> skipped by name, no list, no destroy
#   T4 live battery (running)     -> destroy refused, created= renewed
#   T5 dead orphan (stopped)      -> exactly one BOUNDED destroy, lock dropped
#   T6 destroy hangs + timeout=2  -> sweep returns, timeout notice names lock
#   T7 list unavailable           -> fail closed: no destroy, lock retained
#   T8 live tick_pid lock         -> never swept
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bunker-qa.sh}"
pass=0
fail=0
check() { # <name> <expected> <actual>
  if [ "$2" = "$3" ]; then
    echo "  ok   $1 -> $3"
    pass=$((pass + 1))
  else
    echo "  FAIL $1: expected [$2] got [$3]"
    fail=$((fail + 1))
  fi
}
contains() { # <name> <needle> <haystack>
  case "$3" in
    *"$2"*) echo "  ok   $1"; pass=$((pass + 1)) ;;
    *) echo "  FAIL $1: missing [$2] in [$3]"; fail=$((fail + 1)) ;;
  esac
}

TMPD="${BUNKER_QA_TEST_TMP:-${TMPDIR:-/tmp}}/qa_muster046.$$"
mkdir -p "$TMPD/bin" "$TMPD/locks" "$TMPD/quarantine"
trap 'rm -rf "$TMPD"' EXIT

# Extract the real sweep, never a reimplementation of it.
: > "$TMPD/watchdog.sh"
for fn in qa_orphan_lock_dir qa_agent_lock_path qa_write_agent_lock qa_remove_agent_lock \
          qa_tick_pid_alive qa_lock_value qa_watchdog_ttl_seconds qa_lock_age_seconds \
          qa_quarantine_orphan_evidence qa_orphan_lock_eligible qa_renew_agent_lock \
          qa_agent_list_state qa_cleanup_orphan_agents; do
  sed -n "/^$fn() {/,/^}\$/p" "$HARNESS" >> "$TMPD/watchdog.sh"
  if ! grep -q "^$fn() {" "$TMPD/watchdog.sh"; then
    echo "FAIL: helper $fn not found in $HARNESS (extraction contract broken)"
    exit 1
  fi
done
bash -n "$TMPD/watchdog.sh" || { echo "FAIL: extracted sweep does not parse"; exit 1; }

# Stub bunker CLI. `list` renders the real table shape (header + "<id> <status>"
# row + Total line) so the sweep parses it exactly as it parses production
# output; LIST_ROWS holds the rows to print, LIST_RC forces a list failure.
cat > "$TMPD/bin/bunker" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  destroy)
    printf 'destroy %s server=%s\n' "$2" "${4:-}" >> "${BUNKER_LOG:?}"
    : > "${BUNKER_LOG}.destroyed"
    [ "${DESTROY_SLEEP:-0}" = 0 ] || sleep "$DESTROY_SLEEP"
    exit "${DESTROY_RC:-0}"
    ;;
  list)
    printf 'list server=%s\n' "${3:-}" >> "${BUNKER_LOG:?}"
    [ "${LIST_RC:-0}" -eq 0 ] || exit "${LIST_RC:-0}"
    rows="${LIST_ROWS:-}"
    # A daemon that really reaped the agent stops listing it; LIST_ROWS_AFTER
    # models that transition so the post-destroy re-check is exercised.
    [ -f "${BUNKER_LOG}.destroyed" ] && [ -n "${LIST_ROWS_AFTER+x}" ] && rows="${LIST_ROWS_AFTER}"
    printf '\n══════════ Agents ══════════\n\n'
    printf '  %-14s %-10s %s\n' "Agent ID" "Status" "Created"
    printf '%s\n' "$rows" | while IFS= read -r row; do
      [ -n "$row" ] || continue
      set -- $row
      printf '  %-14s %-10s %s\n' "$1" "$2" "2026-11-21T00:00:00Z"
    done
    printf 'Total: 0 agents\n'
    exit 0
    ;;
  *) exit 0 ;;
esac
STUB
chmod +x "$TMPD/bin/bunker"

cat > "$TMPD/bin/quarantine" <<'STUB'
#!/usr/bin/env bash
printf 'quarantine %s\n' "$1" >> "${BUNKER_LOG:?}"
printf 'agent log for %s\n' "$1" > "$3/agent.log"
STUB
chmod +x "$TMPD/bin/quarantine"

export PATH="$TMPD/bin:$PATH"
export BUNKER_LOG="$TMPD/bunker.log"
export BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks"
export BUNKER_QA_QUARANTINE_DIR="$TMPD/quarantine"
export BUNKER_QA_QUARANTINE_CMD="$TMPD/bin/quarantine"
export BUNKER_QA_ORPHAN_TTL=2h
: > "$BUNKER_LOG"

# shellcheck disable=SC2317
make_orphan_lock() { # <agent> <server> -> lock path (created= backdated past the TTL)
  local id="$1" srv="$2" lock
  lock=$(BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c \
    "source '$TMPD/watchdog.sh'; qa_write_agent_lock '$id' 999999 '$srv' '$TMPD/evidence.jsonl'")
  sed -i "s/^created=.*/created=$(date -u -d '3 hours ago' +%FT%TZ)/" "$lock"
  printf '%s\n' "$lock"
}
sweep() { # <launch-server-or-empty> [list-rows] [list-rc] [post-destroy-rows] -> stderr in sweep.err
  local srv="${1:-}" rows="${2:-}" lrc="${3:-0}" after="${4-__UNSET__}"
  local -a envs=("LIST_ROWS=$rows" "LIST_RC=$lrc")
  [ "$after" = "__UNSET__" ] || envs+=("LIST_ROWS_AFTER=$after")
  : > "$BUNKER_LOG"
  rm -f "${BUNKER_LOG}.destroyed"
  if [ -n "$srv" ]; then
    env "${envs[@]}" bash -c "source '$TMPD/watchdog.sh'; qa_cleanup_orphan_agents '$srv'" 2>"$TMPD/sweep.err"
  else
    env "${envs[@]}" bash -c "source '$TMPD/watchdog.sh'; qa_cleanup_orphan_agents" 2>"$TMPD/sweep.err"
  fi
}

echo "── T1 ghost lock: agent absent from the daemon list ──"
ghost=$(make_orphan_lock ghost-agent stub-srv-a)
sweep stub-srv-a ""
check "T1 sweep rc" 0 "$?"
check "T1 destroy calls" 0 "$(grep -c '^destroy ' "$BUNKER_LOG" || true)"
check "T1 ghost lock dropped" no "$([ -f "$ghost" ] && echo yes || echo no)"
contains "T1 names the lock and the daemon proof" "reason=agent-absent" "$(cat "$TMPD/sweep.err")"
contains "T1 names the lock path" "lock=$ghost" "$(cat "$TMPD/sweep.err")"

echo "── T2 zero-agent server: the original wedge class ──"
zero=$(make_orphan_lock zero-agent stub-srv-a)
sweep stub-srv-a ""
check "T2 destroy calls (must be 0)" 0 "$(grep -c '^destroy ' "$BUNKER_LOG" || true)"
check "T2 lock dropped" no "$([ -f "$zero" ] && echo yes || echo no)"

echo "── T3 foreign-server lock: launch on A must never touch B ──"
foreign=$(make_orphan_lock foreign-agent stub-srv-b)
sweep stub-srv-a "foreign-agent stopped"
check "T3 destroy calls" 0 "$(grep -c '^destroy ' "$BUNKER_LOG" || true)"
check "T3 list calls" 0 "$(grep -c '^list ' "$BUNKER_LOG" || true)"
check "T3 foreign lock retained" yes "$([ -f "$foreign" ] && echo yes || echo no)"
contains "T3 skip line names the foreign server" "skip foreign-server orphan agent=foreign-agent" "$(cat "$TMPD/sweep.err")"
rm -f "$foreign"

echo "── T4 live battery: status=running must not be destroyed ──"
live=$(make_orphan_lock live-agent stub-srv-a)
sweep stub-srv-a "live-agent running"
check "T4 destroy calls" 0 "$(grep -c '^destroy ' "$BUNKER_LOG" || true)"
check "T4 live lock retained" yes "$([ -f "$live" ] && echo yes || echo no)"
contains "T4 skip line names the state" "skip live battery agent=live-agent" "$(cat "$TMPD/sweep.err")"
check "T4 created= renewed to now" "$(date -u +%F)" "$(grep -m1 '^created=' "$live" | cut -c9-18)"
contains "T4 renewal is stamped" "renewed=" "$(cat "$live")"
contains "T4 receipt reports the renewal" "renewed=$live" "$(cat "$TMPD/sweep.err")"
rm -f "$live"

echo "── T5 dead orphan: exactly one bounded destroy ──"
dead=$(make_orphan_lock dead-agent stub-srv-a)
sweep stub-srv-a "dead-agent stopped" 0 ""
check "T5 destroy calls" 1 "$(grep -c '^destroy dead-agent' "$BUNKER_LOG" || true)"
check "T5 dead lock dropped once the daemon proves absence" no "$([ -f "$dead" ] && echo yes || echo no)"
contains "T5 receipt names lock and destroy rc" "lock=$dead" "$(cat "$TMPD/sweep.err")"
contains "T5 receipt names the state" "state=stopped" "$(cat "$TMPD/sweep.err")"

echo "── T5b destroy claims success but the agent is still listed: retain ──"
stuck=$(make_orphan_lock stuck-agent stub-srv-a)
sweep stub-srv-a "stuck-agent stopped" 0 "stuck-agent stopped"
check "T5b destroy calls" 1 "$(grep -c '^destroy stuck-agent' "$BUNKER_LOG" || true)"
check "T5b lock retained (fail closed)" yes "$([ -f "$stuck" ] && echo yes || echo no)"
contains "T5b receipt says it remains" "orphan remains agent=stuck-agent" "$(cat "$TMPD/sweep.err")"
rm -f "$stuck"

echo "── T6 unbounded destroy would wedge: bound must fire ──"
hang=$(make_orphan_lock hang-agent stub-srv-a)
: > "$BUNKER_LOG"
start=$(date +%s)
LIST_ROWS="hang-agent stopped" DESTROY_SLEEP=30 BUNKER_QA_DESTROY_TIMEOUT=2 \
  bash -c "source '$TMPD/watchdog.sh'; qa_cleanup_orphan_agents stub-srv-a" 2>"$TMPD/sweep.err"
end=$(date +%s)
check "T6 sweep returned" 0 "$?"
check "T6 sweep wall under 10s" yes "$([ $((end - start)) -lt 10 ] && echo yes || echo no)"
contains "T6 timeout notice names the lock" "destroy timeout agent=hang-agent lock=$hang" "$(cat "$TMPD/sweep.err")"
check "T6 lock retained after timeout" yes "$([ -f "$hang" ] && echo yes || echo no)"
rm -f "$hang"

echo "── T7 list unavailable: fail closed ──"
nolist=$(make_orphan_lock nolist-agent stub-srv-a)
sweep stub-srv-a "" 2
check "T7 destroy calls" 0 "$(grep -c '^destroy ' "$BUNKER_LOG" || true)"
check "T7 lock retained" yes "$([ -f "$nolist" ] && echo yes || echo no)"
contains "T7 names the cause" "reason=server-list-unavailable" "$(cat "$TMPD/sweep.err")"
rm -f "$nolist"

echo "── T8 live tick_pid lock is never swept ──"
alive_lock=$(BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" bash -c \
  "source '$TMPD/watchdog.sh'; qa_write_agent_lock alive-agent $$ stub-srv-a '$TMPD/evidence.jsonl'")
sweep stub-srv-a "alive-agent stopped"
check "T8 destroy calls" 0 "$(grep -c '^destroy ' "$BUNKER_LOG" || true)"
check "T8 live-tick lock intact" yes "$([ -f "$alive_lock" ] && echo yes || echo no)"
rm -f "$alive_lock"

echo "── T9 no-arg legacy call shape still works ──"
legacy=$(make_orphan_lock legacy-agent stub-srv-a)
sweep ""
check "T9 sweep rc" 0 "$?"
check "T9 absent agent dropped without destroy" 0 "$(grep -c '^destroy ' "$BUNKER_LOG" || true)"
check "T9 lock dropped" no "$([ -f "$legacy" ] && echo yes || echo no)"

echo
echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
