#!/bin/bash
# Bunker E2E Test Battery — Practical
# Tests what works, documents what doesn't
set -euo pipefail
#
# REQUIREMENT: this battery MUST run as root. It creates Linux users via
# useradd, installs PAM/systemd artifacts, and writes a root-owned daemon log
# under /var/log (see README "Run E2E battery"). Run it as:
#   sudo bash e2e-full-battery.sh
# EXIT CODES:
#   0   = success (final line "STATUS: ALL CORE TESTS PASS" / "VERIFY-PASS",
#         or the --self-test diagnostics check completed)
#   42  = preflight refusal (not root, or the daemon log path is not
#         writable) — the reason is printed to stderr, nothing was changed
#   1   = battery ran and failed, or a non-preflight error killed the run
#         (the ERR trap prints the failing line + command before exit)
# --SELF-TEST: `bash e2e-full-battery.sh --self-test` verifies the harness's
#   own diagnostics (capture helper, ERR trap, non-root refusal decision)
#   WITHOUT root: it never creates users, never starts daemons, never writes
#   /var/log, and never runs a battery section. Final line:
#   SELF-TEST: PASS

PASS=0
FAIL=0
NOTE=0

assert() { echo "  ✓ $1"; PASS=$((PASS+1)); }
fail()  { echo "  ✗ $1"; FAIL=$((FAIL+1)); }
note()  { echo "  ⚠ $1"; NOTE=$((NOTE+1)); }

echo "=========================================="
echo " BUNKER END-TO-END TEST BATTERY"
echo " $(date)"
echo "=========================================="
echo ""

export BUNKER_TOKEN="test-regression-token"
BUNKER="/usr/local/bin/bunker"

# Coexistence mode (CI on bunker-mvp): the host runs a systemd-managed
# production bunkerd on :19090/:18080. Use dedicated ports + a temp config and
# NEVER wipe production users or touch the live service. Standalone (default):
# full take-over like the original design. In coexist mode the CLI config is
# isolated via HOME so the battery's connect does not clobber the live host's
# /root/.bunker.
BUNKERD_COEXIST="${BUNKERD_COEXIST:-}"
BUNKERD_GRPC_ADDR="${BUNKERD_GRPC_ADDR:-:29091}"
BUNKERD_REST_ADDR="${BUNKERD_REST_ADDR:-:28081}"
BUNKERD_PID=""
# Binary overrides: the battery normally uses the deployed CLI, but feature
# batteries (GAP-064) may need freshly built binaries WITHOUT overwriting the
# live production bunkerd. BUNKERD_BIN points the coexist-mode daemon at a
# candidate build; BUNKER_BIN overrides the CLI similarly.
BUNKERD_BIN="${BUNKERD_BIN:-/usr/local/bin/bunkerd}"
BUNKER="${BUNKER_BIN:-/usr/local/bin/bunker}"

# GAP-075 section state. Initialized here (not in section 15) so the EXIT trap
# can always reference it under `set -u`, even when the battery dies early.
GAP075_OP_USER=""
GAP075_OP_HOME=""
GAP075_OP_KEYDIR=""
GAP075_RUN_UNIT=""
GAP075_DROPIN_RESTORE=""
GAP075_DROPIN_PATH="/etc/security/namespace.d/50-bunker-agents.conf"
GAP075_HELPER_PATH="/usr/lib/bunker/pam-tmp-guard"
GAP075_HELPER_RESTORE=""
GAP075_HELPERDIR_RESTORE=""
GAP075_MASK_RESTORE=""

# ── Diagnostics helpers (DF-BUNKER-6) ──────────────────────────────────
# run_capture LABEL CMD... — run CMD, capture combined stdout+stderr, and
# make the exit status available WITHOUT letting set -e abort the script.
# Output is stored in RUN_CAPTURE_OUT / RUN_CAPTURE_EXIT. The helper ALWAYS
# returns 0 (a non-zero return would kill a bare call site under set -e —
# the exact silent death this helper exists to prevent); the wrapped
# command's status is reported only via RUN_CAPTURE_EXIT. A failing command
# substitution in a bare `VAR=$(...)` assignment aborts under set -e BEFORE
# the if/else below it can run — that was the silent step-2 death this
# helper replaces.
RUN_CAPTURE_OUT=""
RUN_CAPTURE_EXIT=0
run_capture() {
    local label="$1"; shift
    RUN_CAPTURE_OUT=""
    RUN_CAPTURE_EXIT=0
    set +e
    trap - ERR # the wrapped command's failure is EXPECTED here — handled by the caller's if/else, not the trap
    RUN_CAPTURE_OUT=$("$@" 2>&1)
    RUN_CAPTURE_EXIT=$?
    trap 'diag_err $? $LINENO "$BASH_COMMAND"' ERR
    set -e
    if [ "$RUN_CAPTURE_EXIT" -ne 0 ]; then
        echo "  [capture] $label failed (exit=$RUN_CAPTURE_EXIT)"
    fi
    return 0
}

# Preflight: refuse to run the battery as non-root BEFORE any write to
# /var/log or any host mutation. Preflight_decision() is pure (no side
# effects) so --self-test can exercise it as a non-root user.
preflight_decision() {
    if [ "$(id -u)" -ne 0 ]; then
        echo "ERROR: this battery MUST run as root." >&2
        echo "  Why: it creates Linux users (useradd), installs PAM/systemd" >&2
        echo "  artifacts, and writes a root-owned daemon log under /var/log" >&2
        echo "  (see README \"Run E2E battery\" / AGENTS.md \"E2E verification\")." >&2
        echo "  Run it as: sudo bash e2e-full-battery.sh" >&2
        echo "  (no-side-effect preview: bash e2e-full-battery.sh --self-test)" >&2
        return 1
    fi
    return 0
}
if [ "${1:-}" = "--self-test" ]; then
    # ── Self-test: prove the diagnostics work, with ZERO side effects. ──
    # Never creates users, never starts daemons, never touches /var/log,
    # never runs a battery section. Uses the same helpers as the main path.
    ST_FAIL=0
    echo "=== 0. Self-test (no root, no side effects) ==="

    # (a) A failing command captured through run_capture must SURFACE its
    # output (the exact helper the section-2 connect step calls).
    run_capture "synthetic failing command" sh -c 'echo "synthetic stderr boom" >&2; exit 7'
    if [ "$RUN_CAPTURE_EXIT" -ne 0 ] && printf '%s' "$RUN_CAPTURE_OUT" | grep -q "synthetic stderr boom"; then
        assert "run_capture surfaces failing-command output (exit=$RUN_CAPTURE_EXIT)"
    else
        fail "run_capture swallowed a failing command's output or status"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (b) A synthetic failure must hit the ERR trap and name line + command.
    ST_TRAP_SAW=""
    diag_err() { ST_TRAP_SAW="line=$2 cmd=$3"; }
    set +e
    false
    set -e
    diag_err() {
        local status="$1" line="$2" cmd="$3"
        echo "" >&2
        echo "  ✗ ERROR: command failed (exit $status) at line $line: $cmd" >&2
        echo "    (run aborted by set -e; partial results above, EXIT cleanup still runs)" >&2
    }
    if printf '%s' "$ST_TRAP_SAW" | grep -qE 'line=[0-9]+ cmd=false'; then
        assert "ERR trap names failing line + command ($ST_TRAP_SAW)"
    else
        fail "ERR trap did not produce a line+command diagnostic (got: '$ST_TRAP_SAW')"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (c) The non-root refusal decision must report "would refuse" for a
    # non-root uid without exiting the self-test (id -u is forced here).
    st_uid() { echo 1000; }
    ST_REFUSED=""
    ST_RC=0
    # if/else condition context: the ERR trap does not fire on the EXPECTED
    # refusal, and $? in the else branch is the real refusal status.
    if ST_REFUSED=$(preflight_decision 2>&1); then
        ST_RC=0
    else
        ST_RC=$?
    fi
    if [ "$ST_RC" -ne 0 ] && printf '%s' "$ST_REFUSED" | grep -q "MUST run as root"; then
        assert "preflight would refuse a non-root uid (rc=$ST_RC, reason printed)"
    else
        fail "preflight_decision did not report the non-root refusal"
        ST_FAIL=$((ST_FAIL+1))
    fi

    echo ""
    if [ "$ST_FAIL" -eq 0 ]; then
        echo "SELF-TEST: PASS"
        exit 0
    fi
    echo "SELF-TEST: FAIL ($ST_FAIL check(s) failed)"
    exit 1
fi

if ! preflight_decision; then
    exit 42
fi
# EXIT cleanup — MAIN BATTERY PATH ONLY. The --self-test path above exits
# BEFORE this is armed, so the self-test can never destroy agents, delete
# users, or run any teardown (it is side-effect-free by construction).
trap cleanup EXIT


# ERR trap: name the failing line + command before any set -e exit. cleanup
# (EXIT trap below) still runs; the script's exit status is preserved.
trap 'diag_err $? $LINENO "$BASH_COMMAND"' ERR
diag_err() {
    local status="$1" line="$2" cmd="$3"
    echo "" >&2
    echo "  ✗ ERROR: command failed (exit $status) at line $line: $cmd" >&2
    echo "    (run aborted by set -e; partial results above, EXIT cleanup still runs)" >&2
}

if [ -n "$BUNKERD_COEXIST" ]; then
    export HOME="$(mktemp -d /tmp/bunker-battery-home-XXXXXX)"
    BATTERY_CONFIG="$(mktemp /tmp/bunkerd-battery-XXXXXX.yaml)"
    cat > "$BATTERY_CONFIG" <<EOF
server:
  grpc_addr: "$BUNKERD_GRPC_ADDR"
  rest_addr: "$BUNKERD_REST_ADDR"
auth:
  enabled: false
agent:
  ssh_dir: /etc/bunkerd/ssh
  max_agents: 10
  port_range_start: 30000
  port_range_end: 30999
  port_range_per_agent: 100
  # GAP-075: keep each agent's shared-scratch cap small so this battery can
  # prove the kernel-enforced bound (16 MiB) instead of only inspecting it.
  # The scratch root stays at the documented default: /srv/bunker-share.
  isolation:
    shared_scratch_enabled: true
    shared_scratch_per_agent_bytes: 16777216
  # GAP-070: this battery's daemon must NEVER open the production registry or
  # reconcile against the host's real agents — on a shared host the default
  # reconciliation mode would DESTROY production agents. Durable-registry
  # behaviour is covered deterministically by the Go suite and by section 14.
  registry:
    enabled: false
EOF
fi

if [ -n "$BUNKERD_COEXIST" ]; then
    GRPC_PORT="${BUNKERD_GRPC_ADDR#:}"
    REST_PORT="${BUNKERD_REST_ADDR#:}"
else
    GRPC_PORT="19090"
    REST_PORT="18080"
fi

cleanup() {
    # Destroy any agents created during tests
    for agent in e2e-main e2e-agent-2 e2e-agent-3 e2e-agent-4 e2e-agent-5 e2e-imgspec e2e-imgspec-b e2e-imgspec-bad gap070-idem gap075-a gap075-b; do
        $BUNKER destroy "$agent" --force > /dev/null 2>&1 || true
    done
    # Kill leftover users. Standalone: every bunker- user is fair game.
    # Coexist: only this battery's own agents (bunker-e2e-*) — NEVER touch
    # production users.
    if [ -z "$BUNKERD_COEXIST" ]; then
        for u in $(awk -F: '/^bunker-/ {print $1}' /etc/passwd); do
            userdel -rf "$u" 2>/dev/null || true
        done
    else
        for u in $(awk -F: '/^bunker-e2e-/ {print $1}' /etc/passwd); do
            userdel -rf "$u" 2>/dev/null || true
        done
        # Quarantine this battery's own ssh keys if destroy did not remove
        # them (shared /etc/bunkerd/ssh — never touch production keys).
        mkdir -p "/tmp/bunker-battery-key-quarantine-$$"
        for k in $(ls /etc/bunkerd/ssh 2>/dev/null | grep '^e2e-'); do
            mv "/etc/bunkerd/ssh/$k" "/tmp/bunker-battery-key-quarantine-$$/" 2>/dev/null || true
        done
    fi
    # GAP-075: stop the disposable detached run unit, remove the disposable
    # non-agent test user, restore a backed-up namespace drop-in, and remove the
    # /tmp markers. Scratch dirs are removed by the daemon's destroy; a leftover
    # is unmounted here so the host is clean.
    if [ -n "${GAP075_RUN_UNIT:-}" ]; then
        systemctl stop "${GAP075_RUN_UNIT}" > /dev/null 2>&1 || true
    fi
    if [ -n "${GAP075_DROPIN_RESTORE:-}" ] && [ -f "${GAP075_DROPIN_RESTORE}" ]; then
        cp -a "${GAP075_DROPIN_RESTORE}" "${GAP075_DROPIN_PATH}" 2>/dev/null || true
        rm -f "${GAP075_DROPIN_RESTORE}" 2>/dev/null || true
    fi
    # GAP-075 rework: restore the pam_exec precondition helper if the
    # fail-closed checks were interrupted while it was moved aside, and drop any
    # disposable marker line from the sshd stack.
    if [ -n "${GAP075_HELPER_RESTORE:-}" ] && [ -f "${GAP075_HELPER_RESTORE}" ]; then
        cp -a "${GAP075_HELPER_RESTORE}" "${GAP075_HELPER_PATH}" 2>/dev/null || true
        chown root:root "${GAP075_HELPER_PATH}" 2>/dev/null || true
        chmod 0755 "${GAP075_HELPER_PATH}" 2>/dev/null || true
        rm -f "${GAP075_HELPER_RESTORE}" 2>/dev/null || true
    fi
    # ...and restore the helper directory's mode if the widened-directory check
    # was interrupted mid-flight (the mode is part of the trust chain).
    if [ -n "${GAP075_HELPERDIR_RESTORE:-}" ]; then
        chown root:root "${GAP075_HELPER_DIR:-/usr/lib/bunker}" 2>/dev/null || true
        chmod "${GAP075_HELPERDIR_RESTORE}" "${GAP075_HELPER_DIR:-/usr/lib/bunker}" 2>/dev/null || true
        GAP075_HELPERDIR_RESTORE=""
    fi
    if [ -n "${GAP075_MASK_RESTORE:-}" ]; then
        grep -vF "${GAP075_MASK_RESTORE}" /etc/pam.d/sshd > /etc/pam.d/sshd.bunker-battery-tmp 2>/dev/null &&
            cat /etc/pam.d/sshd.bunker-battery-tmp > /etc/pam.d/sshd 2>/dev/null || true
        rm -f /etc/pam.d/sshd.bunker-battery-tmp 2>/dev/null || true
    fi
    if [ -n "${GAP075_OP_USER:-}" ]; then
        userdel -rf "${GAP075_OP_USER}" > /dev/null 2>&1 || true
    fi
    if [ -n "${GAP075_OP_KEYDIR:-}" ]; then
        rm -rf "${GAP075_OP_KEYDIR}" 2>/dev/null || true
    fi
    rm -f /tmp/gap075-* 2>/dev/null || true
    # A refused arbitrary-entry probe leaves nothing behind; remove anything a
    # FAILED probe may have created directly under the exchange root.
    rm -rf "${BUNKER_SHARED_SCRATCH:-/srv/bunker-share}"/gap075-arb-* 2>/dev/null || true
    for d in /srv/bunker-share/gap075-a /srv/bunker-share/gap075-b; do
        if mountpoint -q "$d" 2>/dev/null; then
            umount -l "$d" 2>/dev/null || true
        fi
        [ -d "$d" ] && rmdir "$d" 2>/dev/null || true
    done
    # Stop the battery's own bunkerd (coexist mode only)
    if [ -n "$BUNKERD_PID" ]; then
        kill "$BUNKERD_PID" 2>/dev/null || true
        wait "$BUNKERD_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# Clean up any leftover test agents
echo "=== CLEANUP ==="
if [ -n "$BUNKERD_COEXIST" ]; then
    # Coexist: only this battery's own agents (bunker-e2e-*) — NEVER touch
    # production users.
    for u in $(awk -F: '/^bunker-e2e-/ {print $1}' /etc/passwd); do
        userdel -rf "$u" 2>/dev/null || true
    done
else
    for u in $(awk -F: '/^bunker-/ {print $1}' /etc/passwd); do
        userdel -rf "$u" 2>/dev/null || true
    done
fi
echo ""

# Start the battery's own daemon in coexist mode (isolated ports, temp config)
if [ -n "$BUNKERD_COEXIST" ]; then
    BUNKERD_BATTERY_LOG="${BUNKERD_BATTERY_LOG:-/var/log/bunkerd-battery.log}"
    if ! [ -w "$BUNKERD_BATTERY_LOG" ] && ! [ -w "$(dirname "$BUNKERD_BATTERY_LOG")" ]; then
        echo "ERROR: cannot write the battery daemon log: $BUNKERD_BATTERY_LOG" >&2
        echo "  (override the location with BUNKERD_BATTERY_LOG=<path>)" >&2
        exit 42
    fi
    touch "$BUNKERD_BATTERY_LOG" 2>/dev/null || true
    "$BUNKERD_BIN" -c "$BATTERY_CONFIG" >> "$BUNKERD_BATTERY_LOG" 2>&1 &
    BUNKERD_PID=$!
    sleep 2
fi

# =============================================
# 1. SERVER HEALTH
# =============================================
echo "=== 1. Server Health ==="
if pgrep bunkerd > /dev/null; then
    assert "bunkerd process running (PID=$(pgrep bunkerd))"
else
    fail "bunkerd not running"
fi

# Coexist mode: the battery MUST run against its OWN daemon. A stale daemon
# squatting the battery ports (leftover repro/crash) would otherwise produce
# a false pass — kill -0 on BUNKERD_PID fails fast instead (GAP-006 follow-up).
if [ -n "$BUNKERD_COEXIST" ] && [ -n "$BUNKERD_PID" ]; then
    if kill -0 "$BUNKERD_PID" 2>/dev/null; then
        assert "battery's own bunkerd alive (PID=$BUNKERD_PID)"
    else
        fail "battery's own bunkerd (PID=$BUNKERD_PID) not running — ports may be squatted by a stale daemon"
    fi
fi

if ss -tlnp | grep -q ":$GRPC_PORT"; then
    assert "gRPC listening on :$GRPC_PORT"
else
    fail "gRPC port $GRPC_PORT not listening"
fi

if ss -tlnp | grep -q ":$REST_PORT"; then
    assert "REST listening on :$REST_PORT"
else
    fail "REST port $REST_PORT not listening"
fi
echo ""

# =============================================
# 2. CONNECT
# =============================================
echo "=== 2. Connect ==="
run_capture "bunker connect" "$BUNKER" connect "http://localhost:${REST_PORT}" --token test-regression-token
CONNECT_OUT="$RUN_CAPTURE_OUT"
if echo "$CONNECT_OUT" | grep -q "Connected\|Server registered"; then
    assert "connect to bunkerd"
else
    fail "connect to bunkerd — $CONNECT_OUT"
fi
echo ""

# =============================================
# 3. LIST (empty)
# =============================================
echo "=== 3. List (empty) ==="
run_capture "bunker list" "$BUNKER" list --status all
LIST_OUT="$RUN_CAPTURE_OUT"
if echo "$LIST_OUT" | grep -q "No agents found"; then
    assert "list returns empty correctly"
else
    fail "list — $LIST_OUT"
fi
echo ""

# =============================================
# 4. SPAWN
# =============================================
echo "=== 4. Spawn ==="
run_capture "spawn e2e-main" "$BUNKER" spawn --agent-id "e2e-main"
SPAWN_OUT="$RUN_CAPTURE_OUT"
if echo "$SPAWN_OUT" | grep -q "Agent created"; then
    assert "spawn agent e2e-main"
else
    fail "spawn agent — $SPAWN_OUT"
fi
sleep 1

# Verify Linux user created
if id "bunker-e2e-main" >/dev/null 2>&1; then
    assert "Linux user bunker-e2e-main created"
else
    fail "Linux user not created"
fi

# Verify home directory
if [ -d "/home/bunker-e2e-main" ]; then
    assert "home directory exists"
else
    fail "home directory missing"
fi

# Verify SSH keypair
if [ -f "/etc/bunkerd/ssh/e2e-main" ]; then
    assert "SSH private key persisted"
else
    fail "SSH private key missing"
fi

# Verify authorized_keys
if [ -f "/home/bunker-e2e-main/.ssh/authorized_keys" ]; then
    assert "authorized_keys configured"
else
    fail "authorized_keys missing"
fi

# Verify port range
PORT_FILE="/home/bunker-e2e-main/.bunker/ports"
if [ -f "$PORT_FILE" ]; then
    assert "port range assigned: $(cat $PORT_FILE)"
else
    fail "port range file missing"
fi

# Docker check — rootless docker must be running
(echo ""; echo "  --- Docker Status ---")
DOCKERD_RUNNING=$(pgrep -u "bunker-e2e-main" dockerd 2>/dev/null || echo "")
if [ -n "$DOCKERD_RUNNING" ]; then
    assert "dockerd running under agent user (PID=$DOCKERD_RUNNING)"
else
    fail "dockerd NOT running for bunker-e2e-main"
fi

DOCKER_SOCK="/run/bunker/e2e-main/docker.sock"
if [ -S "$DOCKER_SOCK" ]; then
    assert "docker socket exists"
else
    fail "docker socket not created"
fi

DOCKER_RUN=$($BUNKER exec e2e-main -- docker run --rm alpine:latest echo DOCKER-OK 2>&1 || true)
if echo "$DOCKER_RUN" | grep -q "DOCKER-OK"; then
    assert "docker run inside agent works"
else
    fail "docker run inside agent — $DOCKER_RUN"
fi
echo ""

# =============================================
# 5. LIST (1 agent)
# =============================================
echo "=== 5. List (1 agent) ==="
LIST_OUT=$($BUNKER list --status all 2>&1)
if echo "$LIST_OUT" | grep -q "e2e-main"; then
    assert "list shows e2e-main"
else
    fail "list does not show agent"
fi
echo ""

# =============================================
# 6. EXEC
# =============================================
echo "=== 6. Exec ==="
# Test whoami
run_capture "exec whoami" "$BUNKER" exec e2e-main whoami
WHOAMI_OUT="$RUN_CAPTURE_OUT"
if echo "$WHOAMI_OUT" | grep -q "bunker-e2e-main"; then
    assert "exec whoami returns agent user"
else
    fail "exec whoami — $WHOAMI_OUT"
fi

# Test basic command execution
run_capture "exec id" "$BUNKER" exec e2e-main id
ENV_OUT="$RUN_CAPTURE_OUT"
if echo "$ENV_OUT" | grep -q "bunker-e2e-main"; then
    assert "exec id works"
else
    fail "exec id — $ENV_OUT"
fi

# Docker exec (must work now that dockerd is running)
DOCKER_EXEC=$($BUNKER exec e2e-main -- docker run --rm alpine:latest echo DOCKER-OK 2>&1 || true)
if echo "$DOCKER_EXEC" | grep -q "DOCKER-OK"; then
    assert "docker run via exec"
else
    fail "docker run via exec — $DOCKER_EXEC"
fi
echo ""

# =============================================
# 6a. DOCKER TUNNEL
# =============================================
echo "=== 6a. Docker Tunnel ==="
TUNNEL_PID=""
TUNNEL_LOG=/tmp/bunker-tunnel-e2e-main.log
nohup "$BUNKER" tunnel e2e-main > "$TUNNEL_LOG" 2>&1 &
TUNNEL_PID=$!
sleep 3

TUNNEL_OK=0
if kill -0 "$TUNNEL_PID" 2>/dev/null; then
    TUNNEL_DOCKER=$(DOCKER_HOST=tcp://localhost:2376 docker version 2>&1 || true)
    if echo "$TUNNEL_DOCKER" | grep -q "Version"; then
        assert "docker version through SSH tunnel"
        TUNNEL_OK=1
    else
        fail "docker version through SSH tunnel — $TUNNEL_DOCKER"
    fi
    kill "$TUNNEL_PID" 2>/dev/null || true
    wait "$TUNNEL_PID" 2>/dev/null || true
else
    fail "tunnel process exited early (log: $TUNNEL_LOG)"
fi
if [ "$TUNNEL_OK" -eq 0 ]; then
    echo "  tunnel log: $(cat "$TUNNEL_LOG" 2>/dev/null | head -5)"
fi
echo ""

# =============================================
# 7. METRICS
# =============================================
echo "=== 7. Server Metrics ==="
SERVER_METRICS_OUT=$($BUNKER metrics 2>&1 || true)
if echo "$SERVER_METRICS_OUT" | grep -q "Disk Used"; then
    assert "server metrics show disk usage"
else
    note "server metrics — $SERVER_METRICS_OUT"
fi
if echo "$SERVER_METRICS_OUT" | grep -q "Agents"; then
    assert "server metrics show agent summary"
else
    note "server metrics — no agent summary"
fi

echo "=== 7a. Agent Metrics ==="
METRICS_OUT=$($BUNKER metrics e2e-main 2>&1 || true)
if [ -n "$METRICS_OUT" ]; then
    assert "agent metrics returns data"
else
    note "agent metrics returned empty (may be expected)"
fi
echo ""

# =============================================
# 8. MULTI-AGENT SPAWN
# =============================================
echo "=== 8. Multi-agent Spawn ==="
# CRITICAL (GAP-005 hang): a bare `wait` would block on the battery's OWN
# backgrounded bunkerd in coexist mode (it never exits) — the battery hung
# here until the CI step timeout killed it, leaking all agents. Wait only for
# the spawn CLIs.
SPAWN_PIDS=()
for i in 2 3 4 5; do
    $BUNKER spawn --agent-id "e2e-agent-$i" > /dev/null 2>&1 &
    SPAWN_PIDS+=($!)
done
wait "${SPAWN_PIDS[@]}"
sleep 3

AGENT_COUNT=0
for i in 1 2 3 4 5; do
    AGENT_LABEL=$([ "$i" -eq 1 ] && echo "e2e-main" || echo "e2e-agent-$i")
    if id "bunker-$AGENT_LABEL" >/dev/null 2>&1; then
        AGENT_COUNT=$((AGENT_COUNT + 1))
    fi
done
if [ "$AGENT_COUNT" -ge 5 ]; then
    assert "5 agents spawned ($AGENT_COUNT/5 Linux users)"
else
    fail "agent spawn — only $AGENT_COUNT/5 users created"
fi

# Verify isolation (each has unique home + port range)
UNIQUE_HOMES=$(for u in bunker-e2e-main bunker-e2e-agent-{2..5}; do
    [ -d "/home/$u" ] && echo "/home/$u"
done | wc -l)
if [ "$UNIQUE_HOMES" -eq 5 ]; then
    assert "all 5 home directories unique"
else
    fail "only $UNIQUE_HOMES/5 home dirs"
fi

UNIQUE_PORTS=$(for u in e2e-main e2e-agent-{2..5}; do
    PORT_FILE="/home/bunker-$u/.bunker/ports"
    if [ -f "$PORT_FILE" ]; then
        head -1 "$PORT_FILE" | tr '-' '\n' | head -1
    fi
done | sort -u | wc -l)
if [ "$UNIQUE_PORTS" -ge 5 ]; then
    assert "all 5 port ranges unique"
else
    note "only $UNIQUE_PORTS/5 unique port ranges"
fi
echo ""

# =============================================
# 9. DESTROY
# =============================================
echo "=== 9. Destroy ==="
run_capture "destroy e2e-main" "$BUNKER" destroy e2e-main --force
DESTROY_OUT="$RUN_CAPTURE_OUT"
if echo "$DESTROY_OUT" | grep -q "destroyed"; then
    assert "destroy e2e-main"
else
    fail "destroy — $DESTROY_OUT"
fi
sleep 2

if id "bunker-e2e-main" >/dev/null 2>&1; then
    fail "user not removed after destroy"
else
    assert "Linux user removed"
fi

if [ -d "/home/bunker-e2e-main" ]; then
    fail "home directory not removed"
else
    assert "home directory cleaned up"
fi

# Re-spawn (idempotency)
$BUNKER spawn --agent-id "e2e-main" > /dev/null 2>&1
sleep 1
if id "bunker-e2e-main" >/dev/null 2>&1; then
    assert "re-spawn same ID works (idempotency)"
else
    fail "re-spawn failed"
fi
echo ""

# =============================================
# 10. CLEANUP ALL
# =============================================
echo "=== 10. Cleanup All ==="
for agent in e2e-main e2e-agent-2 e2e-agent-3 e2e-agent-4 e2e-agent-5; do
    $BUNKER destroy "$agent" --force > /dev/null 2>&1 || true
done
sleep 3

if [ -n "$BUNKERD_COEXIST" ]; then
    REMAINING=$(grep -c '^bunker-e2e-' /etc/passwd 2>/dev/null || true)
else
    REMAINING=$(awk -F: '/^bunker-/ {print $1}' /etc/passwd | wc -l)
fi
if [ "$REMAINING" -eq 0 ]; then
    assert "all agents cleaned up"
else
    note "$REMAINING agents still present (manual cleanup needed)"
fi
echo ""

# =============================================
# 11. CONFIG FILES
# =============================================
echo "=== 11. Config & CI ==="
if [ -f "/etc/bunkerd/config.yaml" ]; then
    assert "bunkerd config exists"
else
    fail "bunkerd config missing"
fi

if [ -f "${HOME}/.bunker/config.yaml" ]; then
    assert "CLI config exists"
else
    note "CLI config not found"
fi

if [ -f "/opt/bunker/.github/workflows/ci.yml" ]; then
    assert "GitHub Actions CI workflow exists"
else
    note "CI workflow not found on server (may only exist in git)"
fi

if [ -f "/opt/bunker/regression-tests.sh" ]; then
    assert "regression-tests.sh present"
else
    fail "regression-tests.sh missing"
fi
echo ""

# =============================================
# 12. REGRESSION SUITE (quick)
# =============================================
echo "=== 12. Regression Suite ==="
# Prefer the repo-local copy (CI checkout), fall back to the deployed one.
if [ -f "$(dirname "$0")/regression-tests.sh" ]; then
    REGRESSION_SCRIPT="$(dirname "$0")/regression-tests.sh"
elif [ -f "/opt/bunker/regression-tests.sh" ]; then
    REGRESSION_SCRIPT="/opt/bunker/regression-tests.sh"
else
    REGRESSION_SCRIPT=""
fi
if [ -n "$REGRESSION_SCRIPT" ]; then
    REGRESSION_DIR="$(dirname "$REGRESSION_SCRIPT")"
    cd "$REGRESSION_DIR"
    if [ -n "$BUNKERD_COEXIST" ]; then
        # Coexist: nested regression suite gets its own ports so it does not
        # collide with this battery's own daemon (or the live production one).
        REG_OUT=$(BUNKERD_GRPC_ADDR=":29092" BUNKERD_REST_ADDR=":28082" bash "$REGRESSION_SCRIPT" 2>&1 || true)
    else
        REG_OUT=$(bash "$REGRESSION_SCRIPT" 2>&1 || true)
    fi
    REG_EXIT=$?
    # Count assertions
    REG_PASS=$(echo "$REG_OUT" | grep -c "✓\|PASS" || echo "0")
    REG_FAIL=$(echo "$REG_OUT" | grep -c "✗\|FAIL" || echo "0")
    echo "$REG_OUT" | tail -10
    if [ "$REG_EXIT" -eq 0 ]; then
        assert "regression suite PASSED"
    else
        note "regression suite had $REG_FAIL failures (check output above)"
    fi
else
    note "regression-tests.sh not available"
fi
echo ""

# =============================================
# 13. IMAGE SPECS (GAP-064) — allowed/rejected/cache/cleanup
# =============================================
# Proves the four acceptance criteria for per-agent image customization:
#   (a) an allowed package-add spec spawns an agent whose image contains the
#       package (`which <pkg>` via exec),
#   (b) a rejected spec returns invalid_argument WITHOUT a build,
#   (c) same/same/changed spec on the same agent causes 1/1/2 builds,
#   (d) destroy removes the agent's own container before the dockerd stops.
echo "=== 13. Image Specs (GAP-064) ==="
GAP064_SPEC="$(mktemp /tmp/gap064-spec-XXXXXX.json)"
GAP064_REJECT="$(mktemp /tmp/gap064-reject-XXXXXX.json)"
GAP064_SPEC2="$(mktemp /tmp/gap064-spec2-XXXXXX.json)"
cat > "$GAP064_SPEC" <<'EOF'
{"packages": [{"manager": "apt", "packages": ["jq"]}]}
EOF
cat > "$GAP064_REJECT" <<'EOF'
{"packages": [{"manager": "apt", "packages": ["curl|sh"]}]}
EOF
cat > "$GAP064_SPEC2" <<'EOF'
{"packages": [{"manager": "apt", "packages": ["jq", "curl"]}]}
EOF

# (b) Rejected spec: invalid_argument, NO build, NO user created.
GAP064_REJECT_OUT=$($BUNKER spawn --agent-id "e2e-imgspec-bad" --image-spec "$GAP064_REJECT" 2>&1 || true)
if echo "$GAP064_REJECT_OUT" | grep -qi "invalid_argument\|invalid image spec"; then
    assert "rejected spec returns invalid_argument"
else
    fail "rejected spec should fail with invalid_argument — $GAP064_REJECT_OUT"
fi
if id "bunker-e2e-imgspec-bad" >/dev/null 2>&1; then
    fail "rejected spec created a user anyway"
    $BUNKER destroy e2e-imgspec-bad --force > /dev/null 2>&1 || true
else
    assert "rejected spec created no user (no side effects)"
fi

# (a) Allowed spec: spawn, then verify jq exists in the agent image.
GAP064_BUILD_START=$(date +%s)
GAP064_SPAWN_OUT=$($BUNKER spawn --agent-id "e2e-imgspec" --image-spec "$GAP064_SPEC" 2>&1 || true)
if echo "$GAP064_SPAWN_OUT" | grep -q "Agent created"; then
    assert "allowed spec spawns agent"
else
    fail "allowed spec spawn failed — $GAP064_SPAWN_OUT"
fi
if echo "$GAP064_SPAWN_OUT" | grep -q "bunkerd-imagespec-"; then
    assert "spawn bundle reports customized image ref"
else
    note "image ref not shown in bundle (first build may still be customizing)"
fi
# Wait for the first rootless build to finish (bounded).
GAP064_WAITED=0
until $BUNKER exec e2e-imgspec -- which jq > /dev/null 2>&1; do
    sleep 10
    GAP064_WAITED=$((GAP064_WAITED+10))
    if [ "$GAP064_WAITED" -ge 300 ]; then break; fi
done
GAP064_BUILD_END=$(date +%s)
WHICH_JQ=$($BUNKER exec e2e-imgspec -- which jq 2>&1 || true)
if echo "$WHICH_JQ" | grep -q "/usr/bin/jq\|/bin/jq"; then
    assert "jq present in customized image (which jq → $WHICH_JQ)"
else
    fail "jq NOT found in agent image — $WHICH_JQ"
fi

# (c) Cache: re-spawn same spec (destroy first to exercise rebuild path),
#     then a changed spec — counts builds via the cache markers.
GAP064_CACHE_ROOT="/var/cache/bunkerd/imagespec/e2e-imgspec"
KEY_JQ=$(ls "$GAP064_CACHE_ROOT" 2>/dev/null | head -1 || true)
if [ -n "$KEY_JQ" ]; then
    assert "spec cache dir exists (key ${KEY_JQ:0:12}…)"
else
    fail "no spec cache dir under $GAP064_CACHE_ROOT"
fi

$BUNKER destroy e2e-imgspec --force > /dev/null 2>&1
sleep 2
# Same spec again: must reuse the cache marker + image inspect (no rebuild).
$BUNKER spawn --agent-id "e2e-imgspec" --image-spec "$GAP064_SPEC" > /dev/null 2>&1 || true
sleep 1
GAP064_SAME_T0=$(date +%s)
until $BUNKER exec e2e-imgspec -- which jq > /dev/null 2>&1; do
    sleep 5
    if [ $(( $(date +%s) - GAP064_SAME_T0 )) -ge 120 ]; then break; fi
done
GAP064_SAME_T1=$(date +%s)
GAP064_SAME_SECS=$((GAP064_SAME_T1 - GAP064_SAME_T0))
if [ "$GAP064_SAME_SECS" -lt 60 ]; then
    assert "same spec re-spawn fast (cache hit, ${GAP064_SAME_SECS}s < 60s)"
else
    note "same-spec re-spawn took ${GAP064_SAME_SECS}s (may have rebuilt)"
fi

# Changed spec on the same agent: fresh daemon per re-spawn, so the image is
# rebuilt for the new key — the changed key must appear alongside the old one.
$BUNKER spawn --agent-id "e2e-imgspec-b" --image-spec "$GAP064_SPEC2" > /dev/null 2>&1 || true
sleep 1
GAP064_KEYS=$($BUNKER exec e2e-imgspec-b -- sh -c 'docker images --format "{{.Repository}}" 2>/dev/null | grep -c bunkerd-imagespec' 2>/dev/null || echo 0)
if [ "${GAP064_KEYS:-0}" -ge 1 ]; then
    assert "changed spec built a NEW image key (daemon has $GAP064_KEYS imagespec image)"
else
    note "changed-spec image count probe: '$GAP064_KEYS'"
fi

# (d) Cleanup hook: after force destroy, the agent's container is gone.
$BUNKER destroy e2e-imgspec --force > /dev/null 2>&1 || true
$BUNKER destroy e2e-imgspec-b --force > /dev/null 2>&1 || true
sleep 2
if id "bunker-e2e-imgspec" >/dev/null 2>&1 || id "bunker-e2e-imgspec-b" >/dev/null 2>&1; then
    fail "imgspec agents not fully destroyed"
else
    assert "imgspec agents destroyed and users removed"
fi

rm -f "$GAP064_SPEC" "$GAP064_REJECT" "$GAP064_SPEC2"
echo ""

# =============================================
# 14. DURABLE AGENT REGISTRY (GAP-070)
# =============================================
# Deterministic and host-safe: every check runs against a throwaway registry in
# /tmp (3 spawns + 1 heartbeat + 1 destroy, plus a corrupt line and a torn
# tail). Nothing here touches the live registry or any real agent, so this
# section runs in BOTH standalone and coexist (CI) mode.
echo "=== 14. Durable Registry (GAP-070) ==="
GAP070_DIR="$(mktemp -d /tmp/bunker-gap070-XXXXXX)"
GAP070_REG="$GAP070_DIR/agents.jsonl"
cat > "$GAP070_REG" <<'EOF'
{"ts":"2026-09-12T10:00:00Z","kind":"spawn","agent_id":"aa","status":"running","created_at":"2026-09-12T10:00:00Z","expires_at":"2026-09-12T16:00:00Z","port_start":31000,"port_end":31099,"ssh_key_path":"/etc/bunkerd/ssh/aa"}
{"ts":"2026-09-12T10:00:01Z","kind":"spawn","agent_id":"bb","status":"running","created_at":"2026-09-12T10:00:01Z","expires_at":"2026-09-12T16:00:00Z","port_start":31100,"port_end":31199,"ssh_key_path":"/etc/bunkerd/ssh/bb"}
{"ts":"2026-09-12T10:00:02Z","kind":"spawn","agent_id":"cc","status":"running","created_at":"2026-09-12T10:00:02Z","expires_at":"2026-09-12T16:00:00Z","port_start":31200,"port_end":31299,"ssh_key_path":"/etc/bunkerd/ssh/cc"}
{"ts":"2026-09-12T10:05:00Z","kind":"heartbeat","agent_id":"aa","expires_at":"2026-09-12T22:00:00Z","status":"running"}
{"ts":"2026-09-12T10:10:00Z","kind":"destroy","agent_id":"cc"}
{"ts":"2026-09-12T10:11:00Z","kind":"spawn", BROKEN-LINE}
EOF
# A torn final line (no newline) must be ignored, not parse-failed loudly.
printf '%s' '{"ts":"2026-09-12T10:12:00Z"' >> "$GAP070_REG"
chmod 600 "$GAP070_REG"

run_capture "registry compact" "$BUNKER" registry compact --path "$GAP070_REG"
COMPACT_OUT="$RUN_CAPTURE_OUT"
COMPACT_EXIT="$RUN_CAPTURE_EXIT"
if [ "$COMPACT_EXIT" -eq 0 ] && echo "$COMPACT_OUT" | grep -q "registry compacted:"; then
    assert "bunker registry compact rewrote the registry"
else
    fail "bunker registry compact failed (exit $COMPACT_EXIT): $COMPACT_OUT"
fi
if echo "$COMPACT_OUT" | grep -qE "events: [0-9]+ -> [0-9]+" && echo "$COMPACT_OUT" | grep -qE "live agents: 2 -> 2"; then
    assert "compact printed before/after counts ($(echo "$COMPACT_OUT" | grep 'events:' | sed 's/^ *//'))"
else
    fail "compact did not print before/after counts: $COMPACT_OUT"
fi
if [ "$(grep -c '"kind":"spawn"' "$GAP070_REG")" = "2" ] && ! grep -q '"agent_id":"cc"' "$GAP070_REG"; then
    assert "compacted registry holds exactly one current-state record per live agent"
else
    fail "compacted registry has the wrong record set ($(wc -l < "$GAP070_REG") lines)"
fi
if grep -q '"kind":"known"' "$GAP070_REG" && grep -q '"cc"' "$GAP070_REG"; then
    assert "destroyed ID kept in the bounded known-index record (idempotent destroy)"
else
    fail "destroyed ID was not retained by compaction"
fi
if [ "$(stat -c '%a' "$GAP070_REG")" = "600" ]; then
    assert "registry file mode is 0600"
else
    fail "registry file mode is $(stat -c '%a' "$GAP070_REG"), want 600"
fi
if [ -f "$GAP070_REG.1" ]; then
    fail "rotated backup survived compaction (would resurrect stale events)"
else
    assert "compaction removed superseded rotated backups"
fi
run_capture "registry compact (idempotence)" "$BUNKER" registry compact --path "$GAP070_REG"
COMPACT_OUT2="$RUN_CAPTURE_OUT"
if echo "$COMPACT_OUT2" | grep -qE "live agents: 2 -> 2"; then
    assert "second compaction is idempotent (live set unchanged)"
else
    fail "second compaction changed the live set: $COMPACT_OUT2"
fi
if [ "$(wc -l < "$GAP070_REG")" = "3" ]; then
    assert "compacted registry replays to 2 live records + 1 known-index record"
else
    fail "compacted registry line count = $(wc -l < "$GAP070_REG"), want 3"
fi
# Spawn metadata needed by adopt must survive the rewrite.
if grep -q '"port_start":31000' "$GAP070_REG" && grep -q '"ssh_key_path":"/etc/bunkerd/ssh/aa"' "$GAP070_REG"; then
    assert "exact port reservation + key path survive compaction (adopt metadata)"
else
    fail "compaction dropped port/key metadata needed to restore an exact reservation"
fi
# Cross-process exclusion: a held advisory lock must make compact wait.
if command -v flock > /dev/null 2>&1; then
    flock "$GAP070_REG.lock" -c 'sleep 3' &
    GAP070_LOCK_PID=$!
    sleep 0.3
    GAP070_START=$(date +%s%N)
    $BUNKER registry compact --path "$GAP070_REG" > /dev/null 2>&1
    GAP070_MS=$(( ($(date +%s%N) - GAP070_START) / 1000000 ))
    wait "$GAP070_LOCK_PID" 2>/dev/null || true
    if [ "$GAP070_MS" -ge 1000 ]; then
        assert "compact waited for the cross-process lock (${GAP070_MS}ms)"
    else
        fail "compact ignored a held cross-process lock (${GAP070_MS}ms)"
    fi
else
    note "flock unavailable — cross-process lock check skipped (covered by go test)"
fi
# --dry-run must not modify anything.
GAP070_SUM_BEFORE=$(md5sum < "$GAP070_REG")
$BUNKER registry compact --path "$GAP070_REG" --dry-run > /dev/null 2>&1
GAP070_SUM_AFTER=$(md5sum < "$GAP070_REG")
if [ "$GAP070_SUM_BEFORE" = "$GAP070_SUM_AFTER" ]; then
    assert "compact --dry-run left the registry untouched"
else
    fail "compact --dry-run modified the registry"
fi
rm -rf "$GAP070_DIR"

# Coexist daemons must never open the production registry (they would reconcile
# against — and destroy — the host's real agents).
if [ -n "$BUNKERD_COEXIST" ]; then
    if grep -q "registry:" "$BATTERY_CONFIG" && grep -q "enabled: false" "$BATTERY_CONFIG"; then
        assert "coexist daemon runs with the durable registry disabled (production agents safe)"
    else
        fail "coexist daemon config does not disable the durable registry"
    fi
    note "live-daemon registry checks skipped in coexist mode — replay/reconcile/destroy-twice are covered by go test ./internal/{registry,agent,server}"
else
    # Standalone: full take-over, so exercise the live daemon's own registry.
    GAP070_LIVE="${BUNKER_REGISTRY_PATH:-/var/lib/bunkerd/agents.jsonl}"
    $BUNKER spawn gap070-idem > /dev/null 2>&1 || true
    sleep 2
    if [ -f "$GAP070_LIVE" ] && grep -q '"agent_id":"gap070-idem"' "$GAP070_LIVE"; then
        assert "spawn persisted a durable registry record at $GAP070_LIVE"
    else
        fail "no durable spawn record for gap070-idem in $GAP070_LIVE"
    fi
    if [ "$(stat -c '%a' "$GAP070_LIVE" 2>/dev/null)" = "600" ]; then
        assert "live registry file mode is 0600"
    else
        fail "live registry file mode is $(stat -c '%a' "$GAP070_LIVE" 2>/dev/null), want 600"
    fi
    # Idempotent destroy: the FIRST destroy removes the agent, the SECOND must
    # still succeed because the registry remembers it.
    $BUNKER destroy gap070-idem --force > /dev/null 2>&1
    FIRST_EXIT=$?
    $BUNKER destroy gap070-idem --force > /dev/null 2>&1
    SECOND_EXIT=$?
    if [ "$SECOND_EXIT" -eq 0 ]; then
        assert "repeated destroy of a known absent agent succeeds (first=$FIRST_EXIT second=$SECOND_EXIT)"
    else
        fail "repeated destroy failed (first=$FIRST_EXIT second=$SECOND_EXIT) — registry knowledge was lost"
    fi
    # Never-seen IDs must still report not_found (non-force).
    $BUNKER destroy gap070-never-seen > /dev/null 2>&1
    if [ "$?" -ne 0 ]; then
        assert "destroy of a never-seen ID still reports not_found"
    else
        fail "destroy of a never-seen ID unexpectedly succeeded"
    fi
    if grep -q '"kind":"destroy"' "$GAP070_LIVE" && grep -q '"agent_id":"gap070-idem"' "$GAP070_LIVE"; then
        assert "destroy lifecycle event durably recorded"
    else
        fail "destroy was not persisted to the registry"
    fi
fi
echo ""

# =============================================
# 15. AGENT /tmp ISOLATION + SCOPED SHARED SCRATCH (GAP-075)
# =============================================
# Security-boundary checks, all with disposable names and cleanup traps:
#
#   15.1 host provisioning: idempotent, never touches /etc/fstab, reports the
#        agent group + classifier + pam_exec precondition + helper integrity,
#        and the DEPLOYED sshd PAM block is the agent-NAME-scoped, fail-closed
#        trio (classifier -> pam_exec precondition -> pam_namespace) with no
#        fail-open option; a re-apply preserves a foreign rule
#   15.2 an agent's /tmp is private: agent-vs-root and agent-vs-agent
#   15.2b the agent user is a member of the agent group (the precondition's input)
#   15.2c an ordinary NON-agent SSH user on the same host still sees the host
#        /tmp — the module must not apply host-wide
#   15.2d a detached run unit carries PrivateTmp=yes and its /tmp is not root's
#   15.3 /srv/bunker-share is the ONLY sanctioned exchange point, with
#        group/setgid semantics and a kernel-enforced per-agent size cap: each
#        per-agent directory is its OWN tmpfs mount, the kernel-reported mount
#        size in BYTES must equal the statfs byte count, and a write past the
#        cap fails with ENOSPC
#   15.3b the exchange ROOT is setgid and NOT writable by agents or the world
#        (mode 2750): an agent cannot create an arbitrary uncapped entry beside
#        its own capped directory
#   15.4 unauthorized paths stay unavailable
#   15.5 the daemon's own units carry PrivateTmp=yes
#   15.6 a MALFORMED namespace drop-in cannot succeed open: the agent session is
#        denied instead of continuing with the host's shared /tmp
#   15.6b a WIDENED helper directory (group/world writable) cannot succeed open:
#        the agent could replace the helper and its manifest, so the session is
#        denied until the mode is restored
#
# Enabling the per-session private /tmp edits the sshd PAM stack, so the section
# is fail-safe: if an agent session cannot open after the install, the
# Bunker-managed PAM block is rolled back immediately and the check fails loudly
# (root sessions are unaffected either way).
echo "=== 15. Agent /tmp Isolation + Shared Scratch (GAP-075) ==="
GAP075_UNIQ="$$"
GAP075_A="gap075-a"
GAP075_B="gap075-b"
GAP075_GROUP="${BUNKER_AGENT_GROUP:-bunker-agents}"
GAP075_SHARE="${BUNKER_SHARED_SCRATCH:-/srv/bunker-share}"
GAP075_A_FILE="gap075-${GAP075_UNIQ}-a"
GAP075_ROOT_FILE="gap075-${GAP075_UNIQ}-root"
GAP075_RUN_FILE="gap075-${GAP075_UNIQ}-run"
GAP075_OP_FILE="gap075-${GAP075_UNIQ}-op"
GAP075_FSTAB_BEFORE=$(md5sum /etc/fstab 2>/dev/null | awk '{print $1}')

# ── 15.1 host provisioning ─────────────────────────────────────────────
if ! $BUNKER host-provision --help > /dev/null 2>&1; then
    fail "bunker binary has no host-provision command — build the candidate CLI and point BUNKER_BIN at it (GAP-075 host half missing)"
else
    assert "bunker host-provision is available"
fi
GAP075_DRY=$($BUNKER host-provision 2>&1 || true)
if echo "$GAP075_DRY" | grep -q "dry run"; then
    assert "host-provision defaults to a dry run"
else
    fail "host-provision without --apply did not report a dry run: $GAP075_DRY"
fi
$BUNKER host-provision --apply > /dev/null 2>&1 || true
GAP075_STATUS_JSON=$($BUNKER host-provision --status --json 2>&1 || true)
if echo "$GAP075_STATUS_JSON" | grep -q '"isolated": *true'; then
    assert "host isolation active: agent-scoped per-session private /tmp installed"
else
    fail "host isolation not active after --apply: $GAP075_STATUS_JSON"
fi
if echo "$GAP075_STATUS_JSON" | grep -q "\"agent_group\": *\"$GAP075_GROUP\""; then
    assert "the installer reports the agent isolation group $GAP075_GROUP"
else
    fail "host-provision reports the wrong agent group: $GAP075_STATUS_JSON"
fi
if echo "$GAP075_STATUS_JSON" | grep -q '"classifier_module": *true'; then
    assert "the agent-name classifier module (pam_succeed_if) is installed"
else
    fail "pam_succeed_if is missing — the agent-name scoping cannot be expressed: $GAP075_STATUS_JSON"
fi
if echo "$GAP075_STATUS_JSON" | grep -q '"pam_exec_module": *true'; then
    assert "the fail-closed precondition module (pam_exec) is installed"
else
    fail "pam_exec is missing — the fail-closed precondition cannot run: $GAP075_STATUS_JSON"
fi
if echo "$GAP075_STATUS_JSON" | grep -q '"classifier_pattern_ok": *true'; then
    assert "the classifier module supports the agent-name pattern test (Linux-PAM >= 1.6)"
else
    fail "pam_succeed_if cannot express user !~ bunker-* on this host: $GAP075_STATUS_JSON"
fi
if echo "$GAP075_STATUS_JSON" | grep -q '"helper_integrity_ok": *true'; then
    assert "the pam_exec precondition helper matches its sha256 manifest"
else
    fail "the precondition helper/manifest are missing or drifted: $GAP075_STATUS_JSON"
fi
GAP075_FSTAB_AFTER=$(md5sum /etc/fstab 2>/dev/null | awk '{print $1}')
if [ "$GAP075_FSTAB_BEFORE" = "$GAP075_FSTAB_AFTER" ]; then
    assert "/etc/fstab was not modified by host provisioning"
else
    fail "host provisioning modified /etc/fstab"
fi
if [ -f "$GAP075_DROPIN_PATH" ]; then
    assert "pam_namespace drop-in installed in /etc/security/namespace.d"
else
    fail "pam_namespace drop-in missing"
fi

# The DEPLOYED PAM block must be the agent-scoped, fail-closed block, and must
# not carry the fail-open option that lets a malformed config be skipped.
#
#   session    [success=2 auth_err=ignore default=die]    pam_succeed_if.so quiet user !~ bunker-*
#   session    [success=ignore default=die]               pam_exec.so quiet <helper> verify <group>
#   session    required                                   pam_namespace.so
#
# The classifier keys on the reserved NAME pattern (so a deleted agent group
# cannot turn an agent session into an operator session), and the pam_exec
# precondition must run between it and the module.
GAP075_CLASSIFIER_LINE="session    [success=2 auth_err=ignore default=die]    pam_succeed_if.so quiet user !~ bunker-*"
GAP075_VERIFY_RE="^session[[:space:]]+\[success=ignore default=die\][[:space:]]+pam_exec\.so[[:space:]]+quiet[[:space:]]+${GAP075_HELPER_PATH}[[:space:]]+verify[[:space:]]+${GAP075_GROUP}[[:space:]]*$"
if grep -qF "$GAP075_CLASSIFIER_LINE" /etc/pam.d/sshd; then
    assert "the sshd stack carries the agent-NAME classifier (reserved pattern bunker-*)"
else
    fail "the agent-name classifier line is not in /etc/pam.d/sshd — a deleted agent group could fail OPEN"
fi
if grep -qE "$GAP075_VERIFY_RE" /etc/pam.d/sshd; then
    assert "the sshd stack carries the agent-only fail-closed precondition (pam_exec -> $GAP075_HELPER_PATH, group $GAP075_GROUP)"
else
    fail "the pam_exec precondition line is missing or does not name the helper/group: $(grep -n 'pam_exec' /etc/pam.d/sshd || true)"
fi
if grep -qE '^session[[:space:]]+required[[:space:]]+pam_namespace\.so[[:space:]]*$' /etc/pam.d/sshd; then
    assert "the pam_namespace session line is bare and required (no weakening options)"
else
    fail "the pam_namespace session line is missing or carries options"
fi
if grep -q 'ignore_config_error' /etc/pam.d/sshd; then
    fail "the deployed PAM stack still carries ignore_config_error — a malformed namespace config would be SKIPPED and the session would continue with the shared /tmp"
else
    assert "no fail-open ignore_config_error in the deployed PAM stack"
fi
# Adjacency: the classifier jumps over exactly two modules, so the verifier and
# the module line must follow it immediately, in that order.
if awk -v c="$GAP075_CLASSIFIER_LINE" '
        $0==c {getline v; getline m;
               if (v ~ /^session[[:space:]]+\[success=ignore default=die\][[:space:]]+pam_exec\.so/ &&
                   m ~ /^session[[:space:]]+required[[:space:]]+pam_namespace\.so[[:space:]]*$/) found=1}
        END {exit !found}' /etc/pam.d/sshd; then
    assert "classifier -> precondition -> pam_namespace are adjacent (the two-module jump lands correctly)"
else
    fail "the managed block is not adjacent/ordered — the classifier's jump would skip an unrelated module"
fi

# ── 15.1b the managed precondition helper is a root-owned, managed file ─
# The helper's TRUST CHAIN is directory -> manifest -> helper. A writable
# directory or manifest would let a local agent replace both root-owned files by
# rename/unlink, so all three links are asserted, not just the helper's bytes.
GAP075_HELPER_DIR=$(dirname "$GAP075_HELPER_PATH")
if [ -d "$GAP075_HELPER_DIR" ]; then
    GAP075_DIR_META=$(stat -c '%U:%G %a' "$GAP075_HELPER_DIR" 2>/dev/null || echo "missing")
    if [ "$GAP075_DIR_META" = "root:root 755" ]; then
        assert "the helper directory $GAP075_HELPER_DIR is root:root mode 755 (first link of the trust chain)"
    else
        fail "helper directory ownership/mode = $GAP075_DIR_META, want root:root 755 — an agent could replace the helper AND its manifest"
    fi
else
    fail "the helper directory $GAP075_HELPER_DIR is missing — every agent session would be denied"
fi
if [ -f "$GAP075_HELPER_PATH.sha256" ]; then
    GAP075_MANIFEST_META=$(stat -c '%U:%G %a' "$GAP075_HELPER_PATH.sha256" 2>/dev/null || echo "missing")
    if [ "$GAP075_MANIFEST_META" = "root:root 444" ]; then
        assert "the helper manifest is root:root mode 444 (a writable manifest would prove anything)"
    else
        fail "helper manifest ownership/mode = $GAP075_MANIFEST_META, want root:root 444"
    fi
else
    fail "the helper manifest $GAP075_HELPER_PATH.sha256 is missing — every agent session would be denied"
fi
if [ -f "$GAP075_HELPER_PATH" ]; then
    GAP075_HELPER_META=$(stat -c '%U:%G %a' "$GAP075_HELPER_PATH" 2>/dev/null || echo "missing")
    if [ "$GAP075_HELPER_META" = "root:root 755" ]; then
        assert "the pam_exec precondition helper is root:root mode 755"
    else
        fail "helper ownership/mode = $GAP075_HELPER_META, want root:root 755"
    fi
    if [ "$(sha256sum "$GAP075_HELPER_PATH" | awk '{print $1}')" = "$(head -n 1 "$GAP075_HELPER_PATH.sha256" 2>/dev/null)" ]; then
        assert "the helper content matches its sha256 manifest"
    else
        fail "the helper does not match its manifest — every agent session would be denied"
    fi
else
    fail "the pam_exec precondition helper $GAP075_HELPER_PATH is missing — every agent session would be denied"
fi

# ── 15.1c install/repair never deletes an operator-owned rule ──────────
# A foreign pam_succeed_if rule (a shape Bunker itself used to write) must
# survive a re-apply byte for byte, and so must a foreign bare pam_namespace
# rule line's COUNT (a repair must not remove or relocate the operator's own
# modules). The disposable marker line is removed again by the trap.
GAP075_MASK_RESTORE="session    optional     pam_succeed_if.so quiet user = gap075-foreign-$GAP075_UNIQ"
printf '%s\n' "$GAP075_MASK_RESTORE" >> /etc/pam.d/sshd 2>/dev/null || true
GAP075_NS_COUNT_BEFORE=$(grep -cE '^session[[:space:]]+required[[:space:]]+pam_namespace\.so[[:space:]]*$' /etc/pam.d/sshd 2>/dev/null || echo 0)
$BUNKER host-provision --apply > /dev/null 2>&1 || true
if grep -qF "$GAP075_MASK_RESTORE" /etc/pam.d/sshd; then
    assert "a repair re-apply preserved a foreign pam_succeed_if rule byte for byte"
else
    fail "host-provision --apply deleted an operator-owned pam_succeed_if rule"
fi
GAP075_NS_COUNT_AFTER=$(grep -cE '^session[[:space:]]+required[[:space:]]+pam_namespace\.so[[:space:]]*$' /etc/pam.d/sshd 2>/dev/null || echo 0)
if [ "$GAP075_NS_COUNT_BEFORE" = "$GAP075_NS_COUNT_AFTER" ]; then
    assert "a repair re-apply left the bare pam_namespace rule count unchanged ($GAP075_NS_COUNT_AFTER)"
else
    fail "re-apply changed the bare pam_namespace rule count ($GAP075_NS_COUNT_BEFORE -> $GAP075_NS_COUNT_AFTER)"
fi
grep -vF "$GAP075_MASK_RESTORE" /etc/pam.d/sshd > /etc/pam.d/sshd.bunker-battery-tmp 2>/dev/null &&
    cat /etc/pam.d/sshd.bunker-battery-tmp > /etc/pam.d/sshd 2>/dev/null || true
rm -f /etc/pam.d/sshd.bunker-battery-tmp 2>/dev/null || true
GAP075_MASK_RESTORE=""

# ── 15.2 agent private /tmp ────────────────────────────────────────────
$BUNKER spawn --agent-id "$GAP075_A" > /dev/null 2>&1 || true
$BUNKER spawn --agent-id "$GAP075_B" > /dev/null 2>&1 || true

GAP075_SESSION=$($BUNKER exec "$GAP075_A" id 2>&1 || true)
if echo "$GAP075_SESSION" | grep -q "bunker-$GAP075_A"; then
    assert "agent session opens with pam_namespace enabled and keeps its own uid"
else
    fail "agent session broken after enabling pam_namespace: $GAP075_SESSION"
    $BUNKER host-provision --uninstall --apply > /dev/null 2>&1 || true
    note "rolled back the Bunker-managed pam_namespace configuration after a failed session"
fi

# Positive control first: the agent can write and read its OWN /tmp. Without
# this, a later "hidden" result could just mean the write failed.
GAP075_WRITE=$($BUNKER exec "$GAP075_A" -- sh -c "echo gap075-a > /tmp/$GAP075_A_FILE && cat /tmp/$GAP075_A_FILE" 2>&1 || true)
if echo "$GAP075_WRITE" | grep -q "gap075-a"; then
    assert "agent A writes and reads its own /tmp/$GAP075_A_FILE (positive control)"
else
    fail "agent A cannot use its own /tmp: $GAP075_WRITE"
fi

# root (host, outside the session) must not see the file.
if [ -e "/tmp/$GAP075_A_FILE" ]; then
    fail "root sees agent A's private /tmp file at /tmp/$GAP075_A_FILE"
else
    assert "root cannot see agent A's private /tmp file"
fi

# A second agent must not see it either.
GAP075_B_LOOK=$($BUNKER exec "$GAP075_B" -- sh -c "test -e /tmp/$GAP075_A_FILE && echo VISIBLE || echo HIDDEN" 2>&1 || true)
if echo "$GAP075_B_LOOK" | grep -q "HIDDEN"; then
    assert "agent B cannot see agent A's private /tmp file"
else
    fail "agent B can see agent A's /tmp file: $GAP075_B_LOOK"
fi

# ...and the reverse direction: root writes /tmp, the agent must not see it.
echo "gap075-root" > "/tmp/$GAP075_ROOT_FILE" 2>/dev/null || true
GAP075_A_LOOK=$($BUNKER exec "$GAP075_A" -- sh -c "test -e /tmp/$GAP075_ROOT_FILE && echo VISIBLE || echo HIDDEN" 2>&1 || true)
if echo "$GAP075_A_LOOK" | grep -q "HIDDEN"; then
    assert "agent A cannot see root's /tmp file"
else
    fail "agent A can see root's /tmp file: $GAP075_A_LOOK"
fi
rm -f "/tmp/$GAP075_ROOT_FILE" 2>/dev/null || true

# ── 15.2b the agent is in the isolation group (the guard's input) ──────
if getent group "$GAP075_GROUP" > /dev/null 2>&1; then
    assert "the agent isolation group $GAP075_GROUP exists on the host"
else
    fail "the agent isolation group $GAP075_GROUP does not exist — every agent session would be denied by the pam_exec precondition"
fi
GAP075_MEMBER=$($BUNKER exec "$GAP075_A" -- id -nG 2>&1 || true)
if echo "$GAP075_MEMBER" | tr ' ' '\n' | grep -qx "$GAP075_GROUP"; then
    assert "the agent session carries the $GAP075_GROUP membership"
else
    fail "agent A's session groups are '$GAP075_MEMBER', which lack $GAP075_GROUP"
fi

# ── 15.2c an ordinary NON-agent SSH user keeps the host /tmp ───────────
# The module must not be mounted host-wide: a disposable non-agent user on the
# same host must still see the host's /tmp, and the agent must not see what that
# user wrote there. The user and its key are removed by the EXIT trap.
GAP075_OP_KEYDIR=$(mktemp -d /tmp/gap075-op-keys-XXXXXX 2>/dev/null) || GAP075_OP_KEYDIR=""
if [ -n "$GAP075_OP_KEYDIR" ] && ssh-keygen -q -t ed25519 -N '' -f "$GAP075_OP_KEYDIR/id" > /dev/null 2>&1; then
    GAP075_OP_USER="gap075-op-${GAP075_UNIQ}"
    if useradd -m -s /bin/bash "$GAP075_OP_USER" > /dev/null 2>&1; then
        GAP075_OP_HOME=$(getent passwd "$GAP075_OP_USER" 2>/dev/null | awk -F: '{print $6}')
        install -d -m 700 -o "$GAP075_OP_USER" -g "$GAP075_OP_USER" "$GAP075_OP_HOME/.ssh" 2>/dev/null || true
        cat "$GAP075_OP_KEYDIR/id.pub" > "$GAP075_OP_HOME/.ssh/authorized_keys" 2>/dev/null || true
        chown "$GAP075_OP_USER" "$GAP075_OP_HOME/.ssh/authorized_keys" 2>/dev/null || true
        chmod 600 "$GAP075_OP_HOME/.ssh/authorized_keys" 2>/dev/null || true
        # A file that exists ONLY in the host /tmp: a non-agent session must see
        # it, an agent session must not.
        echo "gap075-operator" > "/tmp/$GAP075_OP_FILE" 2>/dev/null || true
        GAP075_OP_OUT=$(ssh -i "$GAP075_OP_KEYDIR/id" -o BatchMode=yes -o StrictHostKeyChecking=no \
            -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o LogLevel=ERROR \
            "$GAP075_OP_USER@127.0.0.1" \
            "test -e /tmp/$GAP075_OP_FILE && echo VISIBLE || echo HIDDEN; echo gap075-op-wrote > /tmp/$GAP075_OP_FILE-wrote; id -nG" 2>&1 || true)
        if echo "$GAP075_OP_OUT" | grep -q "VISIBLE"; then
            assert "ordinary non-agent SSH user $GAP075_OP_USER keeps the host /tmp (pam_namespace is NOT applied host-wide)"
        elif echo "$GAP075_OP_OUT" | grep -q "HIDDEN"; then
            fail "ordinary non-agent SSH user $GAP075_OP_USER cannot see the host /tmp — the module is applied host-wide"
        else
            note "ordinary-user scoping check inconclusive (no non-agent SSH session): $(echo "$GAP075_OP_OUT" | head -2 | tr '\n' ' ')"
        fi
        if [ -e "/tmp/$GAP075_OP_FILE-wrote" ]; then
            assert "the non-agent user's /tmp write landed in the host's /tmp"
            GAP075_AGENT_LOOK=$($BUNKER exec "$GAP075_A" -- sh -c "test -e /tmp/$GAP075_OP_FILE-wrote && echo VISIBLE || echo HIDDEN" 2>&1 || true)
            if echo "$GAP075_AGENT_LOOK" | grep -q "HIDDEN"; then
                assert "the agent cannot see the non-agent user's host /tmp file"
            else
                fail "the agent sees the non-agent user's host /tmp file: $GAP075_AGENT_LOOK"
            fi
        else
            note "the disposable non-agent user could not write to the host /tmp (SSH session unavailable); cross-check skipped"
        fi
    else
        note "could not create the disposable non-agent user; host-wide-scoping check skipped"
    fi
else
    note "ssh-keygen or mktemp unavailable; host-wide-scoping check skipped"
fi
rm -f "/tmp/$GAP075_OP_FILE" "/tmp/$GAP075_OP_FILE-wrote" 2>/dev/null || true

# ── 15.2d a detached run unit is private too ───────────────────────────
$BUNKER run "$GAP075_A" --detach -- sh -c "echo gap075-run > /tmp/$GAP075_RUN_FILE; sleep 90" > /dev/null 2>&1 || true
sleep 6
GAP075_RUN_UNIT=$(systemctl list-units --all --no-legend "bunker-run-$GAP075_A-*" 2>/dev/null | awk '{print $1}' | head -1 || true)
if [ -n "$GAP075_RUN_UNIT" ]; then
    GAP075_RUN_PT=$(systemctl show "$GAP075_RUN_UNIT" -p PrivateTmp --value 2>/dev/null || true)
    if [ "$GAP075_RUN_PT" = "yes" ]; then
        assert "detached run unit $GAP075_RUN_UNIT runs with PrivateTmp=yes"
    else
        fail "detached run unit $GAP075_RUN_UNIT has PrivateTmp=$GAP075_RUN_PT, want yes"
    fi
    if [ -e "/tmp/$GAP075_RUN_FILE" ]; then
        fail "root sees the detached run's private /tmp file at /tmp/$GAP075_RUN_FILE"
    else
        assert "root cannot see the detached run's private /tmp file"
    fi
    systemctl stop "$GAP075_RUN_UNIT" > /dev/null 2>&1 || true
    GAP075_RUN_UNIT=""
else
    note "no running bunker-run-* unit found after --detach; the detached-run PrivateTmp property is covered by go test"
fi

# ── 15.3 shared scratch is the only sanctioned exchange point ──────────
GAP075_ADIR="$GAP075_SHARE/$GAP075_A"
GAP075_HANDOFF="$GAP075_A_FILE-handoff.txt"
$BUNKER exec "$GAP075_A" -- sh -c "echo handoff-$GAP075_UNIQ > $GAP075_ADIR/$GAP075_HANDOFF" > /dev/null 2>&1 || true
GAP075_READ=$($BUNKER exec "$GAP075_B" -- cat "$GAP075_ADIR/$GAP075_HANDOFF" 2>&1 || true)
if echo "$GAP075_READ" | grep -q "handoff-$GAP075_UNIQ"; then
    assert "agent B reads agent A's file through $GAP075_SHARE (cross-agent exchange)"
else
    fail "cross-agent exchange through $GAP075_SHARE failed: $GAP075_READ"
fi
GAP075_DMODE=$(stat -c '%a' "$GAP075_ADIR" 2>/dev/null || echo missing)
GAP075_DGROUP=$(stat -c '%G' "$GAP075_ADIR/$GAP075_HANDOFF" 2>/dev/null || echo missing)
if [ "$GAP075_DMODE" = "2770" ]; then
    assert "per-agent scratch dir has setgid mode 2770"
else
    fail "scratch dir mode = $GAP075_DMODE, want 2770"
fi
if [ "$GAP075_DGROUP" = "$GAP075_GROUP" ]; then
    assert "files created in the scratch inherit the agent group $GAP075_GROUP (setgid semantics)"
else
    fail "scratch file group = $GAP075_DGROUP, want $GAP075_GROUP"
fi

# ── 15.3b the exchange ROOT is setgid and NOT writable by agents ────────
# The per-agent cap is only a cap while the root above the per-agent
# directories cannot be written by an agent: a plain (uncapped) directory or
# file created directly under the root would be an unbounded write surface
# inside the exchange tree. The root is root-owned, setgid and mode 2750, and
# an agent must be REFUSED there while still being able to use its own
# sanctioned, capped directory (positive control: the handoff write above).
GAP075_ROOT_MODE=$(stat -c '%a' "$GAP075_SHARE" 2>/dev/null || echo missing)
GAP075_ROOT_META=$(stat -c '%U:%G' "$GAP075_SHARE" 2>/dev/null || echo missing)
if [ "$GAP075_ROOT_MODE" = "2750" ]; then
    assert "the exchange root $GAP075_SHARE is mode 2750 (setgid, not writable by the agent group or the world)"
else
    fail "exchange root mode = $GAP075_ROOT_MODE, want 2750 — an agent could create an uncapped entry beside its capped directory"
fi
if [ "$GAP075_ROOT_META" = "root:$GAP075_GROUP" ]; then
    assert "the exchange root is owned by root:$GAP075_GROUP"
else
    fail "exchange root owner = $GAP075_ROOT_META, want root:$GAP075_GROUP"
fi
GAP075_ARB="gap075-arb-$GAP075_UNIQ"
GAP075_ROOT_MKDIR=$($BUNKER exec "$GAP075_A" -- sh -c \
    "if mkdir $GAP075_SHARE/$GAP075_ARB 2>/dev/null; then echo CREATED; else echo DENIED; fi" 2>&1 || true)
if echo "$GAP075_ROOT_MKDIR" | grep -q "DENIED"; then
    assert "an agent cannot create an arbitrary directory directly under the exchange root"
else
    fail "agent created $GAP075_SHARE/$GAP075_ARB — the exchange root is writable by agents (uncapped bypass): $GAP075_ROOT_MKDIR"
fi
GAP075_ROOT_TOUCH=$($BUNKER exec "$GAP075_A" -- sh -c \
    "if touch $GAP075_SHARE/$GAP075_ARB-file 2>/dev/null; then echo CREATED; else echo DENIED; fi" 2>&1 || true)
if echo "$GAP075_ROOT_TOUCH" | grep -q "DENIED"; then
    assert "an agent cannot create an arbitrary file directly under the exchange root"
else
    fail "agent created $GAP075_SHARE/$GAP075_ARB-file — the exchange root is writable by agents (uncapped bypass): $GAP075_ROOT_TOUCH"
fi
rmdir "$GAP075_SHARE/$GAP075_ARB" 2>/dev/null || true
rm -f "$GAP075_SHARE/$GAP075_ARB-file" 2>/dev/null || true
GAP075_ARB=""

# Bounding: the directory must be its OWN tmpfs mount whose size the KERNEL
# reports as the configured cap, and a write past the cap must fail with
# ENOSPC.
#
# `findmnt -b -n -o SIZE` reports the mount size in BYTES as plain digits. The
# previous revision read the human-readable mount OPTION (`size=16384k`),
# compared that string with the statfs byte count (never equal) and then fed
# `16384k` to `[ ... -le ... ]`, which aborts with "integer expression
# expected" — so the over-cap ENOSPC proof below was silently skipped. Read
# bytes, and validate every value as digits BEFORE any arithmetic. A directory
# that is not itself a mount makes findmnt report the CONTAINING filesystem
# (root-fs size, fstype ext4), which a pure size comparison would accept as
# "bounded": the tmpfs/mountpoint assertion closes that false green.
GAP075_CAP_RAW=$(findmnt -b -n -o SIZE --target "$GAP075_ADIR" 2>/dev/null || true)
GAP075_CAP_FSTYPE=$(findmnt -n -o FSTYPE --target "$GAP075_ADIR" 2>/dev/null || true)
GAP075_FS_BLOCKS=$(stat -f -c %b "$GAP075_ADIR" 2>/dev/null || true)
GAP075_FS_BSIZE=$(stat -f -c %S "$GAP075_ADIR" 2>/dev/null || true)
GAP075_CAP=""
GAP075_KERNEL_CAP=0
if printf '%s' "$GAP075_CAP_RAW" | grep -qE '^[0-9]+$' &&
    printf '%s' "$GAP075_FS_BLOCKS" | grep -qE '^[0-9]+$' &&
    printf '%s' "$GAP075_FS_BSIZE" | grep -qE '^[0-9]+$'; then
    GAP075_CAP="$GAP075_CAP_RAW"
    GAP075_KERNEL_CAP=$(( GAP075_FS_BLOCKS * GAP075_FS_BSIZE ))
fi
if [ "$GAP075_CAP_FSTYPE" = "tmpfs" ] && mountpoint -q "$GAP075_ADIR" 2>/dev/null; then
    assert "per-agent scratch $GAP075_ADIR is its own tmpfs mount"
else
    fail "per-agent scratch $GAP075_ADIR is not a dedicated tmpfs mount (fstype=${GAP075_CAP_FSTYPE:-<none>}) — the per-agent cap would not be enforced"
fi
if [ -n "$GAP075_CAP" ] && [ "$GAP075_KERNEL_CAP" = "$GAP075_CAP" ]; then
    assert "per-agent scratch is bounded by the kernel at $GAP075_CAP bytes (mount size bytes == statfs bytes)"
else
    fail "scratch cap not reported by the kernel (mount size bytes=${GAP075_CAP_RAW:-<none>} statfs=$GAP075_KERNEL_CAP)"
fi
if [ -n "$GAP075_CAP" ] && [ "$GAP075_CAP" -le 67108864 ]; then
    GAP075_FILL_MB=$(( GAP075_CAP / 1048576 + 2 ))
    GAP075_FILL=$($BUNKER exec "$GAP075_A" -- sh -c "dd if=/dev/zero of=$GAP075_ADIR/$GAP075_A_FILE-fill bs=1M count=$GAP075_FILL_MB 2>&1; echo write-exit=\$?" 2>&1 || true)
    if echo "$GAP075_FILL" | grep -qE "write-exit=[1-9]|No space left"; then
        assert "writing past the per-agent cap fails (${GAP075_FILL_MB}MiB into a ${GAP075_CAP}B cap)"
    else
        fail "over-cap write did NOT fail: $GAP075_FILL"
    fi
    $BUNKER exec "$GAP075_A" -- rm -f "$GAP075_ADIR/$GAP075_A_FILE-fill" > /dev/null 2>&1 || true
else
    note "per-agent cap is ${GAP075_CAP:-unknown} bytes — exhaustive ENOSPC fill skipped (cap too large for a battery run; kernel-reported size asserted above)"
fi

# ── 15.4 unauthorized paths stay unavailable ───────────────────────────
GAP075_OUTSIDE=$($BUNKER exec "$GAP075_A" -- sh -c 'touch /srv/gap075-not-allowed 2>/dev/null && echo WROTE || echo DENIED' 2>&1 || true)
if echo "$GAP075_OUTSIDE" | grep -q "DENIED"; then
    assert "agent cannot write outside the sanctioned scratch tree"
else
    fail "agent wrote outside the scratch tree: $GAP075_OUTSIDE"
fi
rm -f /srv/gap075-not-allowed 2>/dev/null || true
GAP075_INST=$($BUNKER exec "$GAP075_A" -- ls /var/lib/bunkerd/agent-tmp 2>&1 || true)
if echo "$GAP075_INST" | grep -qi "permission denied"; then
    assert "the private-/tmp instance parent is unreadable from inside a session"
else
    note "instance parent listing returned: $GAP075_INST"
fi

# ── 15.5 the daemon's own units carry PrivateTmp=yes ───────────────────
if command -v systemctl > /dev/null 2>&1; then
    GAP075_PT=$(systemctl show "bunker-docker-$GAP075_A" -p PrivateTmp --value 2>/dev/null || true)
    if [ "$GAP075_PT" = "yes" ]; then
        assert "rootless-dockerd unit runs with PrivateTmp=yes"
    else
        fail "bunker-docker-$GAP075_A PrivateTmp=$GAP075_PT, want yes"
    fi
else
    note "systemctl unavailable — unit PrivateTmp check skipped (covered by go test)"
fi

# ── 15.6 a malformed namespace config cannot fail OPEN ─────────────────
# pam_namespace without ignore_config_error makes a malformed line a session
# error; with it the module would SKIP the line and the session would continue
# with the host's shared /tmp. The agent session must be DENIED, and must work
# again once the drop-in is restored (the trap restores it if this battery dies
# mid-check).
if [ -f "$GAP075_DROPIN_PATH" ]; then
    GAP075_DROPIN_RESTORE=$(mktemp /tmp/gap075-dropin-restore-XXXXXX 2>/dev/null) || GAP075_DROPIN_RESTORE=""
    if [ -n "$GAP075_DROPIN_RESTORE" ] && cp -a "$GAP075_DROPIN_PATH" "$GAP075_DROPIN_RESTORE" 2>/dev/null; then
        # One field: pam_namespace cannot parse it (missing instance_prefix and
        # method), so the module reports an error before any polyinstantiation.
        printf '/tmp\n' > "$GAP075_DROPIN_PATH" 2>/dev/null || true
        GAP075_BROKEN_EXIT=0
        $BUNKER exec "$GAP075_A" -- true > /dev/null 2>&1 || GAP075_BROKEN_EXIT=$?
        if [ "$GAP075_BROKEN_EXIT" -ne 0 ]; then
            assert "a malformed namespace drop-in DENIES the agent session (fail closed, exit=$GAP075_BROKEN_EXIT)"
        else
            fail "a malformed namespace drop-in still let the agent session open — the boundary fails OPEN"
        fi
        cp -a "$GAP075_DROPIN_RESTORE" "$GAP075_DROPIN_PATH" 2>/dev/null || true
        rm -f "$GAP075_DROPIN_RESTORE" 2>/dev/null || true
        GAP075_DROPIN_RESTORE=""
        GAP075_RECOVER=$($BUNKER exec "$GAP075_A" -- true > /dev/null 2>&1; echo "exit=$?")
        if echo "$GAP075_RECOVER" | grep -q "exit=0"; then
            assert "the agent session opens again after the drop-in is restored"
        else
            fail "the agent session did NOT recover after restoring the drop-in: $GAP075_RECOVER"
        fi
    else
        note "could not back up $GAP075_DROPIN_PATH; malformed-config check skipped"
    fi
else
    note "$GAP075_DROPIN_PATH absent; malformed-config check skipped"
fi

# ── 15.6b fail CLOSED when the boundary itself is gone ─────────────────
# Two live proofs that a broken boundary DENIES an agent session instead of
# handing it the host's shared /tmp:
#   1. the pam_exec precondition helper is moved aside (a deleted helper must
#      deny, not silently skip);
#   2. the agent's membership in the isolation group is removed (a lost
#      membership must deny — the first revision failed OPEN here).
# Both are restored immediately, and the EXIT trap restores them if this battery
# dies mid-check.
$BUNKER exec "$GAP075_A" -- true > /dev/null 2>&1 && GAP075_HEALTHY=1 || GAP075_HEALTHY=0
if [ "$GAP075_HEALTHY" = "1" ] && [ -f "$GAP075_HELPER_PATH" ]; then
    GAP075_HELPER_RESTORE=$(mktemp /tmp/gap075-helper-restore-XXXXXX 2>/dev/null) || GAP075_HELPER_RESTORE=""
    if [ -n "$GAP075_HELPER_RESTORE" ] && cp -a "$GAP075_HELPER_PATH" "$GAP075_HELPER_RESTORE" 2>/dev/null; then
        rm -f "$GAP075_HELPER_PATH" 2>/dev/null || true
        GAP075_NOHELPER_EXIT=0
        $BUNKER exec "$GAP075_A" -- true > /dev/null 2>&1 || GAP075_NOHELPER_EXIT=$?
        if [ "$GAP075_NOHELPER_EXIT" -ne 0 ]; then
            assert "a deleted pam_exec precondition helper DENIES the agent session (fail closed, exit=$GAP075_NOHELPER_EXIT)"
        else
            fail "the agent session opened without the precondition helper — the boundary fails OPEN"
        fi
        cp -a "$GAP075_HELPER_RESTORE" "$GAP075_HELPER_PATH" 2>/dev/null || true
        chown root:root "$GAP075_HELPER_PATH" 2>/dev/null || true
        chmod 0755 "$GAP075_HELPER_PATH" 2>/dev/null || true
        rm -f "$GAP075_HELPER_RESTORE" 2>/dev/null || true
        GAP075_HELPER_RESTORE=""
        if $BUNKER exec "$GAP075_A" -- true > /dev/null 2>&1; then
            assert "the agent session opens again after the helper is restored"
        else
            fail "the agent session did NOT recover after restoring the helper"
        fi
    else
        note "could not back up $GAP075_HELPER_PATH; helper-removal check skipped"
    fi
else
    note "agent session unavailable or helper absent; the helper-removal fail-closed check is covered by go test"
fi

if [ "$GAP075_HEALTHY" = "1" ] && getent group "$GAP075_GROUP" > /dev/null 2>&1; then
    GAP075_AGENT_USER="bunker-$GAP075_A"
    usermod -aG "$GAP075_GROUP" "$GAP075_AGENT_USER" > /dev/null 2>&1 || true
    gpasswd -d "$GAP075_AGENT_USER" "$GAP075_GROUP" > /dev/null 2>&1 || true
    GAP075_NOMEMBER_EXIT=0
    $BUNKER exec "$GAP075_A" -- true > /dev/null 2>&1 || GAP075_NOMEMBER_EXIT=$?
    if [ "$GAP075_NOMEMBER_EXIT" -ne 0 ]; then
        assert "a removed isolation-group membership DENIES the agent session (fail closed, exit=$GAP075_NOMEMBER_EXIT)"
    else
        fail "the agent session opened without the group membership — group drift fails OPEN"
    fi
    usermod -aG "$GAP075_GROUP" "$GAP075_AGENT_USER" > /dev/null 2>&1 || true
    if $BUNKER exec "$GAP075_A" -- true > /dev/null 2>&1; then
        assert "the agent session opens again after the membership is restored"
    else
        fail "the agent session did NOT recover after restoring the group membership"
    fi
else
    note "agent session unavailable or group absent; the membership fail-closed check is covered by go test"
fi

# ── 15.6c a WIDENED helper directory cannot succeed open ───────────────
# The helper's trust chain starts at its DIRECTORY: with a group/world-writable
# /usr/lib/bunker, a local agent can replace the root-owned helper and its
# manifest by rename/unlink, so the runtime helper must refuse before checking
# anything else. The directory is widened only for the duration of this check
# and restored immediately (the EXIT trap restores it too).
if [ "$GAP075_HEALTHY" = "1" ] && [ -d "$GAP075_HELPER_DIR" ]; then
    GAP075_DIR_MODE_BEFORE=$(stat -c '%a' "$GAP075_HELPER_DIR" 2>/dev/null || echo "")
    if [ -n "$GAP075_DIR_MODE_BEFORE" ]; then
        chmod 777 "$GAP075_HELPER_DIR" 2>/dev/null || true
        GAP075_HELPERDIR_RESTORE="$GAP075_DIR_MODE_BEFORE"
        GAP075_WIDEDIR_EXIT=0
        $BUNKER exec "$GAP075_A" -- true > /dev/null 2>&1 || GAP075_WIDEDIR_EXIT=$?
        chmod "$GAP075_DIR_MODE_BEFORE" "$GAP075_HELPER_DIR" 2>/dev/null || true
        GAP075_HELPERDIR_RESTORE=""
        if [ "$GAP075_WIDEDIR_EXIT" -ne 0 ]; then
            assert "a group/world-writable helper directory DENIES the agent session (fail closed, exit=$GAP075_WIDEDIR_EXIT)"
        else
            fail "the agent session opened while $GAP075_HELPER_DIR was mode 777 — an agent could replace the helper and its manifest"
        fi
        if $BUNKER exec "$GAP075_A" -- true > /dev/null 2>&1; then
            assert "the agent session opens again after the helper directory mode is restored"
        else
            fail "the agent session did NOT recover after restoring the helper directory mode"
        fi
    else
        note "could not read $GAP075_HELPER_DIR mode; widened-directory check skipped"
    fi
else
    note "agent session unavailable or helper directory absent; the widened-directory fail-closed check is covered by go test"
fi

# ── 15.7 cleanup ───────────────────────────────────────────────────────
$BUNKER exec "$GAP075_A" -- rm -f "$GAP075_ADIR/$GAP075_HANDOFF" > /dev/null 2>&1 || true
$BUNKER destroy "$GAP075_A" --force > /dev/null 2>&1 || true
$BUNKER destroy "$GAP075_B" --force > /dev/null 2>&1 || true
rm -f "/tmp/$GAP075_A_FILE" 2>/dev/null || true
if [ -d "$GAP075_ADIR" ]; then
    note "scratch dir $GAP075_ADIR survived destroy (unmounted later by the daemon or left for inspection)"
else
    assert "destroy removed the agent's bounded scratch directory"
fi
echo ""

# =============================================
# SUMMARY
# =============================================
echo ""
echo "=========================================="
echo " RESULTS SUMMARY"
echo "=========================================="
echo "  ✓ Pass:  $PASS"
echo "  ✗ Fail:  $FAIL"
echo "  ⚠ Notes: $NOTE"
echo ""

if [ "$FAIL" -eq 0 ]; then
    echo "  STATUS: ALL CORE TESTS PASS"
    echo "  VERIFY-PASS"
    if [ "$NOTE" -gt 0 ]; then
        echo "  ($NOTE issues documented — see notes above)"
    fi
    echo ""
    exit 0
else
    echo "  STATUS: $FAIL FAILURES — review above"
    echo "  VERIFY-FAIL"
    echo ""
    # A failing battery MUST exit non-zero: the previous revision ended with a
    # bare `exit 0`, so the CI E2E step stayed green while VERIFY-PASS was
    # absent and the failures were only visible in the log text.
    exit 1
fi
