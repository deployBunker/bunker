#!/usr/bin/env bash
# Deterministic regression probe for DF-BUNKER-78: scripts/root-suite.sh's
# EXIT-trap cleanup must classify every removal candidate against the LIVE
# daemon immediately before destroying it, and must FAIL CLOSED (preserve +
# loud diagnostic) when that membership query is unavailable.
#
# The race this pins (2026-09-26 01:53:05): agent dfspec-d registered
# 01:53:04 — AFTER the cleanup-time `bunker list` refresh but BEFORE the
# sweep reached it — and was quarantined + userdel'd while the daemon served
# it. The probe reproduces that ordering deterministically:
#   * a stub `bunker` binary whose FIRST `list` call (the start-snapshot
#     PROD_IDS read) does not know the agent and whose LATER calls do, and
#   * root-suite.sh's ROOT_SUITE_POST_SNAPSHOT_HOOK seam, which creates the
#     candidate's user/key/run-dir state AFTER the start snapshots — exactly
#     the shape of a production spawn landing mid-run.
#
# INT-CI-071 additions (arms 4-6): the per-candidate query is now RETRIED
# (bounded, default 4 attempts / backoff 1 2 4) before the rc=2 fail-closed
# decision, because CI runs 38590365280 / 38590390418 / 38590627192 /
# 38597655288 saw a one-shot query fail or answer empty during the five-way
# concurrent spawn and preserve every candidate. The new arms pin both
# directions: a TRANSIENT failure (the first 1-2 invocations fail, a later one
# answers) must still classify each candidate — removed or preserved per its
# registration — while a PERMANENT failure must still fail closed after the
# budget; the sweep-level sleep cap is pinned too, so a sustained outage
# cannot eat the CI job's cleanup headroom.
#
# Hermetic: no root, no daemon, no real users. root-suite.sh is pointed at
# fixture paths via its ROOT_SUITE_* / BUNKER_BIN env seams and the suite
# body is skipped (ROOT_SUITE_SKIP_SUITE=1) so only the snapshot + cleanup
# trap run. Privileged verbs (userdel/pkill) fail harmlessly as non-root;
# classification is asserted on the script's own decision lines and on the
# fixture ssh/run dirs, which really are removed/preserved.
#
# Run: bash scripts/test-root-suite-cleanup.sh

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SUITE="$ROOT/scripts/root-suite.sh"
[ -f "$SUITE" ] || { echo "FAIL: $SUITE not found"; exit 1; }

RC=0
ok()  { echo "  PASS $1"; }
bad() { echo "  FAIL $1"; RC=1; }

# build_fixtures <new-name>... — bunker-old is the pre-existing production
# user/key/dir (present at snapshot time, must never be touched). Each
# <new-name> is created ONLY via the post-snapshot hook, modeling an agent
# spawned during the suite window.
build_fixtures() {
    T="$(mktemp -d /tmp/root-suite-cleanup-test-XXXXXX)"
    mkdir -p "$T/ssh" "$T/run"
    printf 'bunker-old:x:9001:9001::/home/bunker-old:/bin/bash\n' > "$T/passwd"
    : > "$T/ssh/old"; : > "$T/ssh/old.pub"
    mkdir -p "$T/run/old"
    : > "$T/count"

    # Post-snapshot hook: adds the "spawned during the run" candidates.
    : > "$T/hook.sh"
    local n
    for n in "$@"; do
        cat >> "$T/hook.sh" <<EOF
printf 'bunker-$n:x:9010:9010::/home/bunker-$n:/bin/bash\n' >> "$T/passwd"
: > "$T/ssh/$n"; : > "$T/ssh/$n.pub"
mkdir -p "$T/run/$n"
EOF
    done

    cat > "$T/bunker" <<'STUB'
#!/bin/bash
# Stub bunker CLI. Modes:
#   race      — call #1 (start snapshot) sees no agents; every later call lists liveagent
#   empty     — daemon reachable, registry genuinely empty
#   fail      — daemon unreachable (rc 1) on EVERY call (permanent outage)
#   transient — STUB_FAIL_CALLS (space-separated, 0-based call indices) fail,
#               every other call answers with STUB_LIST_BODY (INT-CI-071)
n=$(cat "$STUB_COUNT" 2>/dev/null || echo 0); n=$((n+0))
echo $((n+1)) > "$STUB_COUNT"
[ "${1:-}" = "list" ] || { echo "stub: unsupported verb ${1:-}" >&2; exit 2; }
# INT-CI-071 transient arm: fail exactly the configured call numbers, so the
# retry budget is what decides the outcome.
# shellcheck disable=SC2086
for f in ${STUB_FAIL_CALLS:-}; do
    if [ "$n" -eq "$f" ]; then
        echo "rpc error: connection refused (transient saturation)" >&2
        exit 1
    fi
done
case "$STUB_MODE" in
    race)
        if [ "$n" -eq 0 ]; then
            printf 'ID          STATUS\n'
        else
            printf 'ID          STATUS\n  liveagent   running\n'
        fi
        ;;
    empty) printf 'ID          STATUS\n' ;;
    fail)  echo "rpc error: connection refused" >&2; exit 1 ;;
    transient)
        # The start-snapshot read (call #1) never lists agents — it models the
        # DF-BUNKER-78 race (the agent registers AFTER the snapshot), so the
        # per-candidate query, not the snapshot fast path, decides each
        # candidate's fate.
        if [ "$n" -eq 0 ]; then
            printf 'ID          STATUS\n'
        else
            printf '%s' "${STUB_LIST_BODY:-ID          STATUS
}"
        fi
        ;;
    *)     echo "stub: unknown STUB_MODE=$STUB_MODE" >&2; exit 2 ;;
esac
STUB
    chmod +x "$T/bunker"
}

run_suite() { # mode [extra env assignments...]
    local mode="$1"
    shift
    env ROOT_SUITE_SKIP_SUITE=1 \
        ROOT_SUITE_POST_SNAPSHOT_HOOK="bash '$T/hook.sh'" \
        BUNKER_BIN="$T/bunker" \
        ROOT_SUITE_PASSWD="$T/passwd" \
        ROOT_SUITE_SSHDIR="$T/ssh" \
        ROOT_SUITE_RUNDIR="$T/run" \
        STUB_MODE="$mode" STUB_COUNT="$T/count" \
        "$@" \
        bash "$SUITE" > "$T/out.$mode" 2> "$T/err.$mode"
}

# stub_calls — how many times the stub was invoked in the current fixture
# (start-snapshot read + one call per attempt of every candidate check).
stub_calls() { cat "$T/count" 2>/dev/null || echo 0; }

# ===========================================================================
echo "1. race arm: agent registers AFTER the start snapshot — survives; genuine leak in the same run is removed"
build_fixtures liveagent leaky
run_suite race

if grep -q "keep live-daemon agent user bunker-liveagent" "$T/out.race"; then
    ok "late-registered user bunker-liveagent preserved (DF-BUNKER-78 race)"
else
    bad "bunker-liveagent not preserved; out:"; sed 's/^/    /' "$T/out.race"
fi

grep -q "removing leaked test user bunker-leaky" "$T/out.race" \
    && ok "genuine test leak bunker-leaky still removed" \
    || bad "bunker-leaky was not classified as a leak"

[ -f "$T/ssh/liveagent" ] && [ -f "$T/ssh/liveagent.pub" ] \
    && ok "live agent ssh keys preserved" \
    || bad "live agent ssh keys were quarantined"
[ -d "$T/run/liveagent" ] \
    && ok "live agent run dir preserved" \
    || bad "live agent run dir was removed"

[ ! -e "$T/ssh/leaky" ] && [ ! -e "$T/ssh/leaky.pub" ] \
    && ok "leak ssh keys quarantined" \
    || bad "leak ssh keys left in place"
[ ! -d "$T/run/leaky" ] \
    && ok "leak run dir removed" \
    || bad "leak run dir left in place"

grep -q "bunker-old" "$T/out.race" \
    && bad "pre-existing user bunker-old was touched" \
    || ok "pre-existing user bunker-old untouched"
[ -f "$T/ssh/old" ] && [ -d "$T/run/old" ] \
    && ok "pre-existing key/run dir untouched" \
    || bad "pre-existing key/run dir touched"
rm -rf "$T"

# ===========================================================================
echo "2. fail-closed arm: membership query unavailable — every candidate preserved with a loud diagnostic"
build_fixtures liveagent leaky
# INT-CI-071: the retry sleeps are skipped here (budget 0) so this arm stays
# fast; arm 5 below pins the real sleep budget and arm 6 the sweep cap.
run_suite fail ROOT_SUITE_QUERY_BACKOFF="0 0 0"

if grep -q "FAIL-CLOSED" "$T/err.fail"; then
    ok "loud FAIL-CLOSED diagnostic on stderr"
else
    bad "no FAIL-CLOSED diagnostic; err:"; sed 's/^/    /' "$T/err.fail"
fi
grep -q "PRESERVING candidate" "$T/err.fail" \
    && ok "diagnostic is actionable (states preservation + next step)" \
    || bad "diagnostic does not state preservation"
# INT-CI-071: the diagnostic must say the retry budget was SPENT, so an
# operator can tell a one-shot miss from an exhausted budget.
grep -q "after 4 attempts / 3 retries" "$T/err.fail" \
    && ok "diagnostic counts the spent retry budget (4 attempts / 3 retries)" \
    || bad "diagnostic does not name the retry budget"
# 1 start-snapshot read + 8 candidates (2 users, 4 keys, 2 run dirs) x 4 attempts.
[ "$(stub_calls)" -eq 33 ] \
    && ok "every candidate was retried to the full budget (33 stub calls)" \
    || bad "stub calls = $(stub_calls), want 33 (retries not attempted?)"

! grep -q "removing leaked test" "$T/out.fail" \
    && ok "no candidate classified as leak while the query is down" \
    || bad "a candidate was removed despite the failed query"
[ -f "$T/ssh/leaky" ] && [ -d "$T/run/leaky" ] && [ -f "$T/ssh/liveagent" ] && [ -d "$T/run/liveagent" ] \
    && ok "ALL candidate keys/run dirs preserved (nothing destroyed blind)" \
    || bad "state was destroyed while the daemon was unreachable"
rm -rf "$T"

# ===========================================================================
echo "3. definitive-absent arm: daemon answers, registry genuinely empty — real leak cleanup still works"
build_fixtures leaky
run_suite empty

grep -q "removing leaked test user bunker-leaky" "$T/out.empty" \
    && ok "leak user removed on a definitive 'not registered' answer" \
    || bad "leak user preserved despite a definitive empty registry"
[ ! -e "$T/ssh/leaky" ] && [ ! -d "$T/run/leaky" ] \
    && ok "leak key + run dir removed" \
    || bad "leak key/run dir left behind on a definitive empty registry"
rm -rf "$T"

# ===========================================================================
# INT-CI-071 arms. The transient arms fail calls #2 and #3 (0-based indices 1
# and 2) — the start-snapshot read is call #1 and succeeds, so ONLY the
# cleanup-time per-candidate query is subjected to the hiccup, exactly the
# shape CI runs 38590365280 / 38590390418 / 38590627192 / 38597655288 showed.
EMPTY_BODY=$'ID          STATUS\n'
LIVE_BODY=$'ID          STATUS\n  liveagent   running\n'

echo "4a. transient-recovery arm: first 2 membership queries fail, a later one answers — the leak is still classified and removed, NOT preserved fail-closed"
build_fixtures leaky
run_suite transient STUB_FAIL_CALLS="1 2" STUB_LIST_BODY="$EMPTY_BODY" ROOT_SUITE_QUERY_BACKOFF="0 0 0"

grep -q "removing leaked test user bunker-leaky" "$T/out.transient" \
    && ok "leak user removed after the query recovered on retry (not fail-closed)" \
    || { bad "leak user was not removed after a transient query failure; out:"; sed 's/^/    /' "$T/out.transient"; sed 's/^/    /' "$T/err.transient"; }
grep -q "recovered on attempt 3 for 'leaky'" "$T/err.transient" \
    && ok "recovery is reported (attempt 3, named candidate)" \
    || bad "no recovery diagnostic for the retried query"
! grep -q "FAIL-CLOSED" "$T/err.transient" \
    && ok "transient hiccup did not trigger the fail-closed path" \
    || bad "a transient hiccup still failed closed"
# 1 start read + 3 attempts for the leaky user + 2 key candidates + 1 run dir.
[ "$(stub_calls)" -eq 7 ] \
    && ok "exactly 3 attempts were spent on the hiccupping query (7 stub calls)" \
    || bad "stub calls = $(stub_calls), want 7"
[ ! -e "$T/ssh/leaky" ] && [ ! -d "$T/run/leaky" ] \
    && ok "leak key + run dir cleaned up after recovery" \
    || bad "leak state left behind after recovery"
rm -rf "$T"

# ===========================================================================
echo "4b. transient-recovery arm (registered): same 2 failed queries, then a definitive 'registered' answer — candidate preserved as live-daemon state, still not fail-closed"
build_fixtures liveagent
run_suite transient STUB_FAIL_CALLS="1 2" STUB_LIST_BODY="$LIVE_BODY" ROOT_SUITE_QUERY_BACKOFF="0 0 0"

grep -q "keep live-daemon agent user bunker-liveagent" "$T/out.transient" \
    && ok "registered agent preserved on the retry's answer (membership, not fail-closed)" \
    || { bad "registered agent was not preserved via membership; out:"; sed 's/^/    /' "$T/out.transient"; }
grep -q "recovered on attempt 3 for 'liveagent'" "$T/err.transient" \
    && ok "recovery is reported for the registered candidate too" \
    || bad "no recovery diagnostic for the registered candidate"
! grep -q "FAIL-CLOSED" "$T/err.transient" \
    && ok "no fail-closed on the recovered registered candidate" \
    || bad "registered candidate hit the fail-closed path"
[ -f "$T/ssh/liveagent" ] && [ -d "$T/run/liveagent" ] \
    && ok "registered candidate's key + run dir untouched" \
    || bad "registered candidate state was destroyed"
rm -rf "$T"

# ===========================================================================
echo "5. permanent-failure arm: EVERY attempt fails — bounded retry budget is spent, then fail-closed still preserves everything"
build_fixtures leaky
START_T=$(date +%s)
run_suite fail ROOT_SUITE_QUERY_ATTEMPTS=2 ROOT_SUITE_QUERY_BACKOFF=1
ELAPSED=$(( $(date +%s) - START_T ))

grep -q "after 2 attempts / 1 retry" "$T/err.fail" \
    && ok "diagnostic reports the configured budget (2 attempts / 1 retry)" \
    || bad "diagnostic does not report the blown retry budget"
grep -q "PRESERVING candidate" "$T/err.fail" && grep -q "FAIL-CLOSED" "$T/err.fail" \
    && ok "fail-closed preserved the candidates after the budget was exhausted" \
    || bad "fail-closed path did not hold after the retry budget"
! grep -q "removing leaked test" "$T/out.fail" \
    && ok "no candidate removed while every query failed" \
    || bad "a candidate was removed on a permanent outage"
[ -f "$T/ssh/leaky" ] && [ -d "$T/run/leaky" ] \
    && ok "candidate key + run dir preserved on a permanent outage" \
    || bad "state destroyed on a permanent outage"
# 4 candidates x 2 attempts x 1s backoff: the waits are REAL, not cosmetic.
[ "$ELAPSED" -ge 3 ] \
    && ok "retry backoff really slept (${ELAPSED}s for 4 candidates x 1s)" \
    || bad "backoff did not sleep (elapsed ${ELAPSED}s, want >= 3s)"
[ "$(stub_calls)" -eq 9 ] \
    && ok "both attempts ran for every candidate (9 stub calls)" \
    || bad "stub calls = $(stub_calls), want 9"
# Source invariant: the SHIPPED defaults are the ticket's budget (4 attempts
# over 1 2 4), not whatever this probe passes in.
grep -qF 'ROOT_SUITE_QUERY_ATTEMPTS:-4}' "$SUITE" && grep -qF 'ROOT_SUITE_QUERY_BACKOFF:-1 2 4}' "$SUITE" \
    && ok "shipped default budget is 4 attempts over backoff 1 2 4" \
    || bad "shipped default retry budget is not 4 attempts over 1 2 4"
rm -rf "$T"

# ===========================================================================
echo "6. sweep sleep cap: a sustained outage stops sleeping once the sweep budget is spent — attempts, rc contract and fail-closed unchanged"
build_fixtures leaky
START_T=$(date +%s)
run_suite fail ROOT_SUITE_QUERY_ATTEMPTS=4 ROOT_SUITE_QUERY_BACKOFF="1 2 4" ROOT_SUITE_QUERY_SWEEP_SLEEP=2
ELAPSED=$(( $(date +%s) - START_T ))

grep -q "retry sleep budget 2s is exhausted" "$T/err.fail" \
    && ok "sweep sleep cap is reported once it binds" \
    || bad "no sweep sleep-cap diagnostic"
[ "$(grep -c 'retry sleep budget' "$T/err.fail")" -eq 1 ] \
    && ok "the cap note is emitted once, not per candidate" \
    || bad "cap note emitted $(grep -c 'retry sleep budget' "$T/err.fail") times, want 1"
[ "$(grep -c 'after 4 attempts / 3 retries' "$T/err.fail")" -eq 4 ] \
    && ok "every candidate still spent the FULL 4-attempt budget" \
    || bad "attempt budget changed under the sweep cap"
[ "$(grep -c 'FAIL-CLOSED' "$T/err.fail")" -eq 4 ] \
    && ok "all 4 candidates still fail closed (rc contract unchanged)" \
    || bad "fail-closed count changed under the sweep cap"
! grep -q "removing leaked test" "$T/out.fail" \
    && ok "nothing removed under the sweep cap" \
    || bad "a candidate was removed while the queries were failing"
[ "$(stub_calls)" -eq 17 ] \
    && ok "attempt count unchanged by the cap (17 stub calls)" \
    || bad "stub calls = $(stub_calls), want 17"
# 4 candidates x 7s would be 28s of sleeping; the 2s cap bounds the sweep.
[ "$ELAPSED" -le 12 ] \
    && ok "sustained outage did not eat the job headroom (${ELAPSED}s, uncapped would be ~28s)" \
    || bad "sweep cap did not bound the wall clock (${ELAPSED}s)"
rm -rf "$T"

echo
if [ "$RC" -eq 0 ]; then
    echo "RESULT: PASS"
else
    echo "RESULT: FAIL"
fi
exit "$RC"
