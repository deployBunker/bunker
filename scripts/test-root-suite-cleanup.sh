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
#   race   — call #1 (start snapshot) sees no agents; every later call lists liveagent
#   empty  — daemon reachable, registry genuinely empty
#   fail   — daemon unreachable (rc 1)
n=$(cat "$STUB_COUNT" 2>/dev/null || echo 0); n=$((n+0))
echo $((n+1)) > "$STUB_COUNT"
[ "${1:-}" = "list" ] || { echo "stub: unsupported verb ${1:-}" >&2; exit 2; }
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
    *)     echo "stub: unknown STUB_MODE=$STUB_MODE" >&2; exit 2 ;;
esac
STUB
    chmod +x "$T/bunker"
}

run_suite() { # mode
    env ROOT_SUITE_SKIP_SUITE=1 \
        ROOT_SUITE_POST_SNAPSHOT_HOOK="bash '$T/hook.sh'" \
        BUNKER_BIN="$T/bunker" \
        ROOT_SUITE_PASSWD="$T/passwd" \
        ROOT_SUITE_SSHDIR="$T/ssh" \
        ROOT_SUITE_RUNDIR="$T/run" \
        STUB_MODE="$1" STUB_COUNT="$T/count" \
        bash "$SUITE" > "$T/out.$1" 2> "$T/err.$1"
}

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
run_suite fail

if grep -q "FAIL-CLOSED" "$T/err.fail"; then
    ok "loud FAIL-CLOSED diagnostic on stderr"
else
    bad "no FAIL-CLOSED diagnostic; err:"; sed 's/^/    /' "$T/err.fail"
fi
grep -q "PRESERVING candidate" "$T/err.fail" \
    && ok "diagnostic is actionable (states preservation + next step)" \
    || bad "diagnostic does not state preservation"

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

echo
if [ "$RC" -eq 0 ]; then
    echo "RESULT: PASS"
else
    echo "RESULT: FAIL"
fi
exit "$RC"
