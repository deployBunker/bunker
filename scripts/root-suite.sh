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

# INT-SPAWN-006: the snapshot is taken ONCE at script start, but the suite
# runs for up to 17 minutes — any agent a live daemon spawns DURING that
# window reads as a "leaked test user" and the old sweep deleted it (proven
# 2026-09-21 09:38:24Z: userdel -rf bunker-t517mount while the demo daemon
# served it).
#
# DF-BUNKER-78: the INT-SPAWN-006 fix (one `bunker list` refresh at cleanup
# start, unioned with the snapshot) NARROWED but did not CLOSE the race —
# proven 2026-09-26 01:53:05: agent dfspec-d registered 01:53:04, one second
# AFTER the cleanup-time refresh, and was quarantined + userdel'd while the
# daemon served it. A single refresh, however late, always loses to a spawn
# that lands between the refresh and the destructive op. Membership is
# therefore re-queried PER CANDIDATE, immediately before that candidate's
# removal (daemon_has_agent below), and the query is FAIL-CLOSED: any query
# failure or ambiguity preserves the candidate with a loud diagnostic — a
# missed leak is recoverable, a destroyed production agent is not.

# Test/override seams (deterministic regression probes point these at
# fixtures; production defaults are unchanged):
BUNKER_BIN="${BUNKER_BIN:-/usr/local/bin/bunker}"
ROOT_SUITE_PASSWD="${ROOT_SUITE_PASSWD:-/etc/passwd}"
ROOT_SUITE_SSHDIR="${ROOT_SUITE_SSHDIR:-/etc/bunkerd/ssh}"
ROOT_SUITE_RUNDIR="${ROOT_SUITE_RUNDIR:-/run/bunker}"

refresh_live_agent_ids() {
    "$BUNKER_BIN" list --status all 2>/dev/null \
        | awk '/^  [a-z0-9]/{print $1}' || true
}

# daemon_has_agent <id>: authoritative membership check against the live
# daemon, made IMMEDIATELY before any destructive removal of <id>'s state.
# Returns:
#   0 — daemon answered, id IS registered: preserve, it is production state.
#   1 — daemon answered, id is NOT registered: genuine leak, safe to remove.
#   2 — query failed or was empty/ambiguous: FAIL CLOSED, preserve.
daemon_has_agent() {
    local id="$1" out
    if ! out="$("$BUNKER_BIN" list --status all 2>/dev/null)"; then
        return 2
    fi
    # A reachable daemon prints at least its header; completely empty stdout
    # is ambiguous (transport hiccup, truncated reply) — fail closed.
    [ -n "$out" ] || return 2
    if echo "$out" | awk '/^  [a-z0-9]/{print $1}' | grep -qx "$id"; then
        return 0
    fi
    return 1
}

# INT-CI-006: the hardcoded 300s go-test budget equaled the suite's real cost
# on this runner (22 real spawns; run 35106585699 died at exactly 300s while
# still progressing), so the job reddened intermittently with no code change.
# 780s (13m) keeps strict headroom under the CI step's timeout-minutes: 15
# (900s) so this script's EXIT-trap leak cleanup still gets room; a genuine
# hang now takes 13m to red — the job still fails, and the budget line below
# makes the timeout self-attributing.
# INT-CI-031 (second cliff): run 35565852250 reddened again with
# internal/agent consuming the FULL 780s while spawns/destroys still flowed —
# the GAP-126..142 security wave grew the root-gated suite past 780s. The CI
# step timeout is now 20m (1200s), so a 1050s budget leaves 150s for this
# script's EXIT-trap leak cleanup.
ROOT_SUITE_TIMEOUT="${ROOT_SUITE_TIMEOUT:-1050s}"

SNAP_PASSWD="$(mktemp /tmp/root-suite-passwd-XXXXXX)"
SNAP_KEYS="$(mktemp /tmp/root-suite-keys-XXXXXX)"
SNAP_RUN="$(mktemp /tmp/root-suite-run-XXXXXX)"
QDIR="$(mktemp -d /tmp/root-suite-quarantine-XXXXXX)"

# Usernames ONLY (not full passwd lines) so the `grep -qx` snapshot check in
# cleanup() can actually match.
grep '^bunker-' "$ROOT_SUITE_PASSWD" 2>/dev/null | cut -d: -f1 > "$SNAP_PASSWD" || true
ls "$ROOT_SUITE_SSHDIR" > "$SNAP_KEYS" 2>/dev/null || true
ls "$ROOT_SUITE_RUNDIR" > "$SNAP_RUN" 2>/dev/null || true
PROD_IDS="$(refresh_live_agent_ids)"

# Test seam (scripts/test-root-suite-cleanup.sh): a command run synchronously
# right AFTER the start snapshots, letting a probe create "spawned during the
# run" state deterministically. Never set in CI/production.
if [ -n "${ROOT_SUITE_POST_SNAPSHOT_HOOK:-}" ]; then
    eval "$ROOT_SUITE_POST_SNAPSHOT_HOOK"
fi

cleanup() {
    local rc=$?
    for u in $(grep '^bunker-' "$ROOT_SUITE_PASSWD" | cut -d: -f1); do
        grep -qx "$u" "$SNAP_PASSWD" && continue
        local id_short="${u#bunker-}"
        if echo "$PROD_IDS" | grep -qx "$id_short"; then
            echo "keep production agent user $u"
            continue
        fi
        # DF-BUNKER-78: authoritative per-candidate membership check, made
        # NOW — immediately before this user's destructive removal. A spawn
        # that registered after the start snapshot (and after any single
        # refresh) is still caught here.
        daemon_has_agent "$id_short"
        local drc=$?
        if [ "$drc" -eq 0 ]; then
            echo "keep live-daemon agent user $u (membership confirmed immediately before removal — DF-BUNKER-78)"
            continue
        fi
        if [ "$drc" -eq 2 ]; then
            echo "root-suite: FAIL-CLOSED: '$BUNKER_BIN list --status all' unavailable while classifying user $u — PRESERVING candidate. Investigate the daemon, then remove manually only if '$BUNKER_BIN list' confirms $id_short is not registered." >&2
            continue
        fi
        echo "removing leaked test user $u"
        pkill -u "$u" -9 2>/dev/null || true
        sleep 1
        userdel -rf "$u" 2>/dev/null || true
    done
    for k in $(ls "$ROOT_SUITE_SSHDIR" 2>/dev/null); do
        grep -qx "$k" "$SNAP_KEYS" && continue
        echo "$PROD_IDS" | grep -qx "$k" && continue
        # DF-BUNKER-78: same per-candidate fail-closed check (a key file may
        # be <id> or <id>.pub; query the agent id).
        local kid="${k%.pub}"
        daemon_has_agent "$kid"
        local krc=$?
        if [ "$krc" -eq 0 ]; then
            echo "keep live-daemon agent key $k (DF-BUNKER-78)"
            continue
        fi
        if [ "$krc" -eq 2 ]; then
            echo "root-suite: FAIL-CLOSED: daemon membership query unavailable for key $k — PRESERVING candidate. Investigate the daemon before manual removal." >&2
            continue
        fi
        echo "quarantining leaked test key $k"
        mv "$ROOT_SUITE_SSHDIR/$k" "$QDIR/" 2>/dev/null || true
        mv "$ROOT_SUITE_SSHDIR/$k.pub" "$QDIR/" 2>/dev/null || true
    done
    for d in $(ls "$ROOT_SUITE_RUNDIR" 2>/dev/null); do
        grep -qx "$d" "$SNAP_RUN" && continue
        echo "$PROD_IDS" | grep -qx "$d" && continue
        # DF-BUNKER-78: same per-candidate fail-closed check.
        daemon_has_agent "$d"
        local drc2=$?
        if [ "$drc2" -eq 0 ]; then
            echo "keep live-daemon agent run dir $d (DF-BUNKER-78)"
            continue
        fi
        if [ "$drc2" -eq 2 ]; then
            echo "root-suite: FAIL-CLOSED: daemon membership query unavailable for run dir $d — PRESERVING candidate. Investigate the daemon before manual removal." >&2
            continue
        fi
        # Run dirs are ephemeral tmpfs (docker.sock + empty run/tmp) — delete
        # outright. mv-quarantine left the source behind when a socket was
        # briefly busy (observed 65fa62e1, GAP-006 audit).
        echo "removing leaked test run dir $d"
        rm -rf "$ROOT_SUITE_RUNDIR/$d"
    done
    exit $rc
}
trap cleanup EXIT

if [ "${ROOT_SUITE_SKIP_SUITE:-}" = "1" ]; then
    # Deterministic regression-probe hook: drive ONLY the snapshot + EXIT-trap
    # cleanup against fixture paths (scripts/test-root-suite-cleanup.sh).
    echo "root-suite: suite body skipped (ROOT_SUITE_SKIP_SUITE=1) — cleanup trap only"
    exit 0
fi

echo "root-suite: go test budget $ROOT_SUITE_TIMEOUT (CI step allows 20m; remainder is for leak cleanup)"
go test -count=1 -run 'TestSpawn|TestCgroup|TestConcurrency' ./... -timeout "$ROOT_SUITE_TIMEOUT"
rc=$?
echo "root-suite: go test finished rc=$rc budget=$ROOT_SUITE_TIMEOUT"
exit "$rc"
