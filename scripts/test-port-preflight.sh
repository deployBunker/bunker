#!/usr/bin/env bash
# Hermetic regression test for the INT-CI-077 port preflight (port_preflight)
# defined by BOTH harness startup paths: regression-tests.sh (around the
# generated config / `bunkerd -c`) and e2e-full-battery.sh (around
# `BUNKERD_BIN -c`).
#
# Why: on GitHub run 39544219096 (regression job 119038796197, head 85c802e6)
# the battery died BEFORE any cell — a previous job's bunkerd had survived on
# the persistent runner and still held :28081/:29091, so the new daemon lost
# the bind ("address already in use", visible only in its log) and the run
# never reached a single cell. The acceptance contract: a battery run must
# either pre-flight the requested ports and report ONE actionable cell naming
# the owning stale pid, instead of dying at startup.
#
# What this test proves, hermetically (no root, no daemon, no host state; one
# mktemp dir and a stub `ss` on PATH):
#   1. port_preflight returns 0 when neither requested port is LISTENing —
#      a clean runner starts unchanged;
#   2. when a requested port IS listening, the preflight refuses (rc != 0)
#      and its ONE cell names the exact port, the owning stale pid and the
#      reclaim action — and warns off the systemd-managed production daemon
#      (INT-CI-012) instead of inviting a kill of the wrong process;
#   3. when the owner pid is not visible (non-root `ss -tlnp`), the cell
#      still refuses and names the port, without inventing a pid;
#   4. both scripts DEFINE the helper exactly once and invoke it BEFORE
#      their daemon start line, refusing with their documented preflight
#      exit code (regression: 2, battery: 42);
#   5. the helper stays detection-only: its body contains no kill/pkill
#      statement (the kill lives in the printed guidance, the operator runs
#      it) and never sleeps a bind failure away.
#
# RED/GREEN proof (the contract this test pins is RED against the
# pre-change source):
#   git show dae24fdb:regression-tests.sh  > /tmp/pre/regression-tests.sh
#   git show dae24fdb:e2e-full-battery.sh  > /tmp/pre/e2e-full-battery.sh
#   bash scripts/test-port-preflight.sh /tmp/pre/regression-tests.sh /tmp/pre/e2e-full-battery.sh
#     → FAIL: could not extract port_preflight (RED)
#   bash scripts/test-port-preflight.sh     → all PASS (GREEN)
#
# Run: bash scripts/test-port-preflight.sh [REGRESSION-SH] [BATTERY-SH]
# The optional arguments point at MUTATED copies of the scripts; that is how
# every case below is RED-proven (same technique
# scripts/test-regression-spawn-cell.sh uses).
#
# ch:trace row=INT-CI-077 spec=none:ci-runner-hygiene-contract wave=.coding-hermes/waves/bunker-foreman-2026-10-21-07-49-18.json#rows[0] test=scripts/test-port-preflight.sh doc=none:not-doc-task prompt=none:worker-brief-file evidence=scripts/test-port-preflight.sh witness=run:39544219096 verdict=28d49b36 commit=894f8aeb8ac84ba03f83c3eb204857ec9a44e459 memory=none:not-written-by-worker
#
# STATUS: complete — port_preflight (detection-only) added to BOTH battery
#   startup paths before the daemon start; a stale listener on an exact
#   requested port produces ONE actionable refusal naming the port, the
#   owning stale pid, the reclaim action and the systemd-daemon warning;
#   regression-tests.sh exits 2, e2e-full-battery.sh exits 42; pinned by
#   scripts/test-port-preflight.sh (behaviour, hermetic) and
#   internal/hostsetup/ciwiring_test.go TestPortPreflightGuardsDaemonStartup
#   (wiring, static).
#   Trace binding repaired: the marker originally named the pre-amend content
#   commit f52c01e9 (a placeholder-substitution commit, now unreachable), which
#   the Tier 2 judge correctly rejected as unbound (verdict dir 28d49b36). It
#   now names the landed implementation commit 894f8aeb, the same commit the
#   verdict binds to.
# TIER: T1 (worker) + T3 (foreman, independently observed) — RED proofs against
#   git show dae24fdb:<path> (shell test exit 1; Go pin FAIL), then GREEN:
#   bash scripts/test-port-preflight.sh (30 checks PASS), bash
#   e2e-full-battery.sh --self-test (SELF-TEST: PASS, +2 arms), go build ./...,
#   go vet ./..., go test ./... -count=1 (all packages ok on the merged tree),
#   gitreins guard (DEGRADED PASS — nothing staged, secrets clean). Foreman
#   reproduced the RED/GREEN arms and the self-test arm in the merged tree; the
#   live runner run reaching VERIFY-PASS with no cell skipped is tracked on the
#   board row as the post-merge CI observation.
# EVIDENCE: this file (RED/GREEN transcripts in its own run output),
#   witness CI run 39544219096 job 119038796197 head 85c802e6, sibling fix
#   9ae65606 (INT-CI-042 keyfile residue, same runner-residue class).

set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REGRESSION="${1:-$REPO/regression-tests.sh}"
BATTERY="${2:-$REPO/e2e-full-battery.sh}"
[ -f "$REGRESSION" ] || { echo "FAIL: regression script not found at $REGRESSION"; exit 1; }
[ -f "$BATTERY" ] || { echo "FAIL: battery script not found at $BATTERY"; exit 1; }

T="$(mktemp -d /tmp/bunker-port-preflight-XXXXXX)"
# Keep the scratch dir on failure so a red run can be inspected; the fleet's
# unattended sessions refuse rm -rf anyway.
trap 'echo "  (scratch kept at $T)"' EXIT

RC=0
ok()  { echo "  PASS $1"; }
bad() { echo "  FAIL $1"; RC=1; }
check_rc() { # check_rc GOT WANT(0|nz) LABEL
    case "$2" in
        0)  if [ "$1" -eq 0 ]; then ok "$3"; else bad "$3 (rc=$1, want 0)"; fi ;;
        nz) if [ "$1" -ne 0 ]; then ok "$3"; else bad "$3 (rc=0, want non-zero)"; fi ;;
    esac
}
check_contains() { # check_contains HAYSTACK NEEDLE LABEL
    case "$1" in
        *"$2"*) ok "$3" ;;
        *) bad "$3 (needle '$2' not found)"; RC=1 ;;
    esac
}
check_missing() { # check_missing HAYSTACK NEEDLE LABEL
    case "$1" in
        *"$2"*) bad "$3 (needle '$2' unexpectedly present)"; RC=1 ;;
        *) ok "$3" ;;
    esac
}

# ── Extract the helper from a script (sed range, the same technique
# scripts/test-gc-linger-helpers.sh uses) and expose run_preflight <grpc> <rest>,
# which sources the extraction with a stub `ss` FIRST on PATH. ──
run_preflight() { # run_preflight SCRIPT_FILE SS_FIXTURE GRPC_PORT REST_PORT
    local SCRIPT_FILE="$1" FIXTURE="$2" GRPC="$3" REST="$4"
    local EXTRACT="$T/helper-$(basename "$SCRIPT_FILE").sh"
    sed -n '/^port_preflight() {/,/^}/p' "$SCRIPT_FILE" > "$EXTRACT"
    if ! grep -q '^port_preflight() {' "$EXTRACT"; then
        echo "FAIL: could not extract port_preflight from $SCRIPT_FILE"
        return 97
    fi
    ( PATH="$T/bin:$PATH" SS_FIXTURE="$FIXTURE" \
      bash -c 'source "$1"; port_preflight "$2" "$3"' _ "$EXTRACT" "$GRPC" "$REST" )
}

mkdir -p "$T/bin"
cat > "$T/bin/ss" <<'STUB'
#!/usr/bin/env bash
# Stub `ss -tlnp`: prints the fixture snapshot, ignoring arguments.
cat "${SS_FIXTURE:?}"
STUB
chmod +x "$T/bin/ss"

# Realistic `ss -tlnp` snapshots (LISTEN lines only — the real shape on the
# runner; the users:(...) field is what the preflight names the owner from).
cat > "$T/ss-free.listen" <<'SNAP'
LISTEN 0 4096   127.0.0.53%lo:53      0.0.0.0:*  users:(("systemd-resolve",pid=666,fd=17))
LISTEN 0 4096   0.0.0.0:22            0.0.0.0:*  users:(("sshd",pid=777,fd=3))
LISTEN 0 4096      [::]:22               [::]:*  users:(("sshd",pid=777,fd=4))
SNAP
cat > "$T/ss-stale-grpc.listen" <<'SNAP'
LISTEN 0 4096   127.0.0.53%lo:53      0.0.0.0:*  users:(("systemd-resolve",pid=666,fd=17))
LISTEN 0 4096   0.0.0.0:29091         0.0.0.0:*  users:(("bunkerd",pid=4242,fd=18))
LISTEN 0 4096   0.0.0.0:22            0.0.0.0:*  users:(("sshd",pid=777,fd=3))
SNAP
cat > "$T/ss-stale-rest.listen" <<'SNAP'
LISTEN 0 4096   0.0.0.0:28081         0.0.0.0:*  users:(("bunkerd",pid=5150,fd=18))
LISTEN 0 4096   0.0.0.0:22            0.0.0.0:*  users:(("sshd",pid=777,fd=3))
SNAP
cat > "$T/ss-stale-noperm.listen" <<'SNAP'
LISTEN 0 4096   0.0.0.0:28081         0.0.0.0:*
LISTEN 0 4096   0.0.0.0:22            0.0.0.0:*
SNAP

echo "1. clean runner: neither requested port LISTENing → preflight passes"
OUT="$(run_preflight "$REGRESSION" "$T/ss-free.listen" 29091 28081 2>&1)"; check_rc "$?" 0 "free ports → rc 0 (regression copy of the helper)"
check_missing "$OUT" "FATAL" "no FATAL cell on a clean runner"
OUT="$(run_preflight "$BATTERY" "$T/ss-free.listen" 29091 28081 2>&1)"; check_rc "$?" 0 "free ports → rc 0 (battery copy of the helper)"

echo "2. stale gRPC listener: ONE actionable cell naming port + stale pid"
OUT="$(run_preflight "$REGRESSION" "$T/ss-stale-grpc.listen" 29091 28081 2>&1)"; PRE_RC="$?"
check_rc "$PRE_RC" nz "stale :29091 listener → preflight refuses (regression helper)"
check_contains "$OUT" ":29091" "cell names the exact requested port"
check_contains "$OUT" "4242" "cell names the owning stale pid"
check_contains "$OUT" "kill 4242" "cell carries the reclaim action"
check_contains "$OUT" "systemctl is-active bunkerd" "cell warns off the systemd-managed production daemon (INT-CI-012)"
check_contains "$OUT" "address already in use" "cell attributes the failure shape (run 39544219096)"
OUT="$(run_preflight "$BATTERY" "$T/ss-stale-grpc.listen" 29091 28081 2>&1)"; check_rc "$?" nz "stale :29091 listener → preflight refuses (battery helper)"

echo "3. stale REST listener: same contract on the second requested port"
OUT="$(run_preflight "$BATTERY" "$T/ss-stale-rest.listen" 29091 28081 2>&1)"; PRE_RC="$?"
check_rc "$PRE_RC" nz "stale :28081 listener → preflight refuses"
check_contains "$OUT" ":28081" "cell names the exact requested port"
check_contains "$OUT" "5150" "cell names the owning stale pid"
check_contains "$OUT" "kill 5150" "cell carries the reclaim action"

echo "4. owner pid not visible (non-root ss): still refuses, invents no pid"
OUT="$(run_preflight "$BATTERY" "$T/ss-stale-noperm.listen" 29091 28081 2>&1)"; PRE_RC="$?"
check_rc "$PRE_RC" nz "pid-less listener → preflight refuses"
check_contains "$OUT" ":28081" "cell names the exact requested port"
check_contains "$OUT" "ss -tlnp" "cell tells the operator how to identify the owner"
check_missing "$OUT" "kill [0-9]" "no invented pid in the reclaim action"

echo "5. wiring: helper defined once, invoked BEFORE the daemon start, refusal exits the harness"
for PAIR in "$REGRESSION:2" "$BATTERY:42"; do
    SCRIPT_FILE="${PAIR%:*}"; WANT_RC="${PAIR##*:}"
    LABEL="$(basename "$SCRIPT_FILE")"
    grep -q '^port_preflight() {' "$SCRIPT_FILE" \
        && [ "$(grep -c '^port_preflight() {' "$SCRIPT_FILE")" = "1" ] \
        && ok "$LABEL defines port_preflight() exactly once" \
        || bad "$LABEL must define port_preflight() exactly once"
    INV_LINE="$(grep -n 'if ! port_preflight ' "$SCRIPT_FILE" | head -n 1 | cut -d: -f1)"
    case "$INV_LINE" in ''|*[!0-9]*) bad "$LABEL never invokes the port preflight"; continue ;; esac
    START_LINE="$(grep -n -m1 -e 'bunkerd -c "$REGRESSION_CONFIG"' -e '"$BUNKERD_BIN" -c "$BATTERY_CONFIG"' "$SCRIPT_FILE" | cut -d: -f1)"
    case "$START_LINE" in ''|*[!0-9]*) bad "$LABEL lost its daemon start line"; continue ;; esac
    if [ "$INV_LINE" -lt "$START_LINE" ]; then
        ok "$LABEL invokes the preflight (line $INV_LINE) before the daemon start (line $START_LINE)"
    else
        bad "$LABEL invokes the preflight AFTER the daemon start ($INV_LINE >= $START_LINE) — too late to matter"
    fi
    if sed -n "${INV_LINE},$((START_LINE - 1))p" "$SCRIPT_FILE" | grep -q "exit $WANT_RC"; then
        ok "$LABEL refusal exits $WANT_RC (its documented preflight code)"
    else
        bad "$LABEL refusal must exit $WANT_RC between the invocation and the daemon start"
    fi
done

echo "6. the helper stays detection-only: no kill/pkill statement, no sleep"
for SCRIPT_FILE in "$REGRESSION" "$BATTERY"; do
    BODY="$(sed -n '/^port_preflight() {/,/^}/p' "$SCRIPT_FILE")"
    [ -n "$BODY" ] || { bad "$(basename "$SCRIPT_FILE"): no port_preflight body"; continue; }
    KILL_LINES="$(printf '%s\n' "$BODY" | grep -nE '^[[:space:]]*(kill|pkill)\b' || true)"
    if [ -z "$KILL_LINES" ]; then
        ok "$(basename "$SCRIPT_FILE"): port_preflight executes no kill/pkill (guidance only)"
    else
        bad "$(basename "$SCRIPT_FILE"): port_preflight must stay detection-only, found: $KILL_LINES"
    fi
    check_missing "$BODY" "sleep " "$(basename "$SCRIPT_FILE"): no sleep papering over the bind"
done

echo ""
if [ "$RC" -eq 0 ]; then
    echo "PORT-PREFLIGHT TEST: PASS"
else
    echo "PORT-PREFLIGHT TEST: FAIL"
fi
exit "$RC"
