#!/usr/bin/env bash
# Hermetic regression test for the GAP-089 linger-cleanup helpers
# (purge_user_linger_state, gc_orphan_linger_files) defined in
# regression-tests.sh.
#
# Why: test-harness teardown deletes throwaway bunker-* users with userdel but
# never disabled systemd linger, so user@UID.service managers kept running for
# deleted users and /var/lib/systemd/linger/<user> orphan files accumulated.
# These helpers close the leak. This test proves their behaviour with no root,
# no real systemd/loginctl, no Linux users and no host state: the two functions
# are sed-extracted from the live suite (exactly like
# scripts/test-regression-cleanup-helpers.sh), pointed at a fake linger dir,
# and driven with fake id/systemctl/loginctl/sss_cache/systemd-escape shims
# that record every call.
#
# Run: bash scripts/test-gc-linger-helpers.sh

set -uo pipefail

SUITE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/regression-tests.sh"
[ -f "$SUITE" ] || { echo "FAIL: suite not found at $SUITE"; exit 1; }

HELPERS="$(mktemp /tmp/linger-helpers-XXXXXX.sh)"
T="$(mktemp -d /tmp/linger-helpers-test-XXXXXX)"
# shellcheck disable=SC2064
trap "rm -rf '$HELPERS' '$T'" EXIT

sed -n '/^purge_user_linger_state() {/,/^}/p;/^gc_orphan_linger_files() {/,/^}/p' "$SUITE" > "$HELPERS"
if ! grep -q '^purge_user_linger_state() {' "$HELPERS" || ! grep -q '^gc_orphan_linger_files() {' "$HELPERS"; then
    echo "FAIL: could not extract both linger helpers from $SUITE"
    exit 1
fi
# shellcheck source=/dev/null
source "$HELPERS"

RC=0
ok()  { echo "  PASS $1"; }
bad() { echo "  FAIL $1"; RC=1; }

mkdir -p "$T/bin" "$T/linger"

# ── Stub command shims (record argv to CALLS_LOG; id resolves via ID_USERS) ──
cat > "$T/bin/id" <<'STUB'
#!/bin/bash
name="${2:-}"
[ -n "$name" ] || exit 1
uid="$(awk -F: -v n="$name" '$1==n{print $2; exit}' "${ID_USERS:-/dev/null}" 2>/dev/null)"
if [ -z "$uid" ]; then exit 1; fi
case "$1" in
    -u)  printf '%s\n' "$uid"; exit 0 ;;
    -nG) exit 0 ;;
    *)   exit 1 ;;
esac
STUB
cat > "$T/bin/loginctl" <<'STUB'
#!/bin/bash
printf 'loginctl %s\n' "$*" >> "${CALLS_LOG:-/dev/null}"
exit 0
STUB
cat > "$T/bin/systemctl" <<'STUB'
#!/bin/bash
printf 'systemctl %s\n' "$*" >> "${CALLS_LOG:-/dev/null}"
exit 0
STUB
cat > "$T/bin/sss_cache" <<'STUB'
#!/bin/bash
printf 'sss_cache %s\n' "$*" >> "${CALLS_LOG:-/dev/null}"
exit 0
STUB
cat > "$T/bin/systemd-escape" <<'STUB'
#!/bin/bash
printf 'systemd-escape %s\n' "$*" >> "${CALLS_LOG:-/dev/null}"
for a in "$@"; do
    case "$a" in --user=*) printf '%s\n' "${a#--user=}"; exit 0 ;; esac
done
printf '%s\n' "${2:-}"
STUB
chmod +x "$T/bin/"*

# Point the helpers at the shims + fixtures.
# The helper functions (sourced above) consume these shims under the same
# names; they are exported so shellcheck does not flag them as unused.
export LINGER_DIR="$T/linger"
export LINGER_ID_CMD="$T/bin/id"
export LINGER_LOGINCTL_CMD="$T/bin/loginctl"
export LINGER_SYSTEMCTL_CMD="$T/bin/systemctl"
export LINGER_SSS_CACHE_CMD="$T/bin/sss_cache"
export LINGER_SYSTEMD_ESCAPE_CMD="$T/bin/systemd-escape"
export LINGER_NSSWITCH_FILE="$T/nsswitch.conf"
export ID_USERS="$T/users"
export CALLS_LOG="$T/calls.log"
printf 'passwd: files\n' > "$LINGER_NSSWITCH_FILE"

reset_calls() { : > "$CALLS_LOG"; }

# ── Test 1: orphan linger file removed, real-user file untouched ──
echo "1. gc removes orphan linger markers and leaves real users' markers alone"
printf 'realuser:1001\n' > "$ID_USERS"
: > "$T/linger/bunker-orphan"
: > "$T/linger/realuser"
reset_calls
gc_orphan_linger_files
[ ! -e "$T/linger/bunker-orphan" ] && ok "orphan linger file removed" || bad "orphan linger file still present"
[   -e "$T/linger/realuser" ]      && ok "real-user linger file untouched" || bad "real-user linger file removed"
grep -q 'systemctl stop user@bunker-orphan.service' "$CALLS_LOG" && ok "orphan manager stop attempted (name-derived unit)" || bad "no stop attempt for orphan"
! grep -q 'systemctl stop user@realuser.service' "$CALLS_LOG" && ok "real user's manager not stopped" || bad "real user's manager stop attempted"

# ── Test 2: disable-linger + marker removal + UID-resolved stop ──
echo "2. purge disables linger, removes the marker, and stops the manager by resolved UID"
: > "$T/linger/bunker-gone"
printf 'bunker-gone:1042\n' > "$ID_USERS"
reset_calls
purge_user_linger_state bunker-gone
grep -q 'loginctl disable-linger bunker-gone' "$CALLS_LOG" && ok "loginctl disable-linger called" || bad "disable-linger not called"
grep -q 'systemctl stop user@1042.service' "$CALLS_LOG" && ok "manager stopped by resolved UID (1042)" || bad "no stop at user@1042.service"
grep -q 'systemctl reset-failed user@1042.service' "$CALLS_LOG" && ok "manager reset-failed" || bad "no reset-failed"
[ ! -e "$T/linger/bunker-gone" ] && ok "linger marker removed" || bad "linger marker still present"

# Deleted user (id -u resolves to nothing): disable-linger still fires, stop skipped.
: > "$T/linger/bunker-orphan2"
: > "$ID_USERS"   # empty: no users exist
reset_calls
purge_user_linger_state bunker-orphan2
grep -q 'loginctl disable-linger bunker-orphan2' "$CALLS_LOG" && ok "deleted user still gets disable-linger" || bad "deleted user not disabled"
[ ! -e "$T/linger/bunker-orphan2" ] && ok "deleted user marker removed" || bad "deleted user marker left"
if grep -q 'systemctl stop' "$CALLS_LOG"; then bad "stop attempted for user with no resolvable UID"; else ok "no stop for unresolvable UID"; fi

# ── Test 3: SSSD branch only fires when nsswitch says sss ──
echo "3. sss_cache only fires when nsswitch.conf actually uses sss"
printf 'passwd: files\n' > "$LINGER_NSSWITCH_FILE"
reset_calls
purge_user_linger_state bunker-orphan2
if grep -q 'sss_cache' "$CALLS_LOG"; then bad "sss_cache called without sss in nsswitch"; else ok "no sss_cache when nsswitch lacks sss"; fi
printf 'passwd: files sss\n' > "$LINGER_NSSWITCH_FILE"
reset_calls
purge_user_linger_state bunker-orphan2
grep -q 'sss_cache -u bunker-orphan2' "$CALLS_LOG" && ok "sss_cache -u called when nsswitch uses sss" || bad "sss_cache not called"

# ── Test 4: helpers never return nonzero (best-effort) ──
echo "4. both helpers are best-effort (never nonzero)"
if purge_user_linger_state ""; then ok "empty username → rc 0"; else bad "empty username returned nonzero"; fi
if purge_user_linger_state no-such-user; then ok "unresolvable user → rc 0"; else bad "unresolvable user returned nonzero"; fi
# shellcheck disable=SC2034
if ( LINGER_DIR="$T/does-not-exist"; gc_orphan_linger_files ); then ok "missing linger dir → rc 0"; else bad "missing linger dir returned nonzero"; fi
mkdir -p "$T/empty-linger"
# shellcheck disable=SC2034
if ( LINGER_DIR="$T/empty-linger"; gc_orphan_linger_files ); then ok "empty linger dir → rc 0"; else bad "empty linger dir returned nonzero"; fi
# shellcheck disable=SC2034
if ( LINGER_ID_CMD="$T/bin/nonexistent-id"; gc_orphan_linger_files ); then ok "id unavailable → rc 0 (fail-closed)"; else bad "id unavailable returned nonzero"; fi

echo
if [ "$RC" -eq 0 ]; then
    echo "RESULT: PASS"
else
    echo "RESULT: FAIL"
fi
exit "$RC"
