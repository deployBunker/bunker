#!/bin/bash
# Root-suite wrapper (GAP-007): runs the root-gated go tests on the live
# self-hosted runner and guarantees ZERO leaked bunker- users / ssh keys /
# run-dirs afterwards — even when a test fails mid-way or spawns without
# cleanup. Pass criteria (GAP-005): 0 leaks after a push.
#
# Safety: only users/keys/dirs that did NOT exist before the run are removed.
# Production agents tracked by the live bunkerd (via `bunker list`) are never
# touched — even if they appear "new" (e.g. a dogfood run spawning during the
# test window).
set -uo pipefail

# INT-CI-006: the hardcoded 300s go-test budget equaled the suite's real cost
# on this runner (22 real spawns; run 35106585699 died at exactly 300s while
# still progressing), so the job reddened intermittently with no code change.
# 780s (13m) keeps strict headroom under the CI step's timeout-minutes: 15
# (900s) so this script's EXIT-trap leak cleanup still gets room; a genuine
# hang now takes 13m to red — the job still fails, and the budget line below
# makes the timeout self-attributing.
ROOT_SUITE_TIMEOUT="${ROOT_SUITE_TIMEOUT:-780s}"

SNAP_PASSWD="$(mktemp /tmp/root-suite-passwd-XXXXXX)"
SNAP_KEYS="$(mktemp /tmp/root-suite-keys-XXXXXX)"
SNAP_RUN="$(mktemp /tmp/root-suite-run-XXXXXX)"
QDIR="$(mktemp -d /tmp/root-suite-quarantine-XXXXXX)"

grep '^bunker-' /etc/passwd > "$SNAP_PASSWD" 2>/dev/null || true
ls /etc/bunkerd/ssh > "$SNAP_KEYS" 2>/dev/null || true
ls /run/bunker > "$SNAP_RUN" 2>/dev/null || true
PROD_IDS="$(/usr/local/bin/bunker list --status all 2>/dev/null | awk '/^  [a-z0-9]/{print $1}')" || true

cleanup() {
    local rc=$?
    for u in $(grep '^bunker-' /etc/passwd | cut -d: -f1); do
        grep -qx "$u" "$SNAP_PASSWD" && continue
        id_short="${u#bunker-}"
        if echo "$PROD_IDS" | grep -qx "$id_short"; then
            echo "keep production agent user $u"
            continue
        fi
        echo "removing leaked test user $u"
        pkill -u "$u" -9 2>/dev/null || true
        sleep 1
        userdel -rf "$u" 2>/dev/null || true
    done
    for k in $(ls /etc/bunkerd/ssh 2>/dev/null); do
        grep -qx "$k" "$SNAP_KEYS" && continue
        echo "$PROD_IDS" | grep -qx "$k" && continue
        mv "/etc/bunkerd/ssh/$k" "$QDIR/" 2>/dev/null || true
        mv "/etc/bunkerd/ssh/$k.pub" "$QDIR/" 2>/dev/null || true
    done
    for d in $(ls /run/bunker 2>/dev/null); do
        grep -qx "$d" "$SNAP_RUN" && continue
        echo "$PROD_IDS" | grep -qx "$d" && continue
        # Run dirs are ephemeral tmpfs (docker.sock + empty run/tmp) — delete
        # outright. mv-quarantine left the source behind when a socket was
        # briefly busy (observed 65fa62e1, GAP-006 audit).
        rm -rf "/run/bunker/$d"
    done
    exit $rc
}
trap cleanup EXIT

echo "root-suite: go test budget $ROOT_SUITE_TIMEOUT (CI step allows 15m; remainder is for leak cleanup)"
go test -count=1 -run 'TestSpawn|TestCgroup|TestConcurrency' ./... -timeout "$ROOT_SUITE_TIMEOUT"
rc=$?
echo "root-suite: go test finished rc=$rc budget=$ROOT_SUITE_TIMEOUT"
exit "$rc"
