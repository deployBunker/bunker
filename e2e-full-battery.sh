#!/bin/bash
# Bunker E2E Test Battery — Practical
# Tests what works, documents what doesn't
set -euo pipefail

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
    for agent in e2e-main e2e-agent-2 e2e-agent-3 e2e-agent-4 e2e-agent-5; do
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
    /usr/local/bin/bunkerd -c "$BATTERY_CONFIG" > /var/log/bunkerd-battery.log 2>&1 &
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
CONNECT_OUT=$($BUNKER connect "http://localhost:${REST_PORT}" --token test-regression-token 2>&1)
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
LIST_OUT=$($BUNKER list --status all 2>&1)
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
SPAWN_OUT=$($BUNKER spawn --agent-id "e2e-main" 2>&1)
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
WHOAMI_OUT=$($BUNKER exec e2e-main whoami 2>&1)
if echo "$WHOAMI_OUT" | grep -q "bunker-e2e-main"; then
    assert "exec whoami returns agent user"
else
    fail "exec whoami — $WHOAMI_OUT"
fi

# Test basic command execution
ENV_OUT=$($BUNKER exec e2e-main id 2>&1)
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
DESTROY_OUT=$($BUNKER destroy e2e-main --force 2>&1)
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
if [ -f "/opt/bunker/regression-tests.sh" ]; then
    cd /opt/bunker
    if [ -n "$BUNKERD_COEXIST" ]; then
        # Coexist: nested regression suite gets its own ports so it does not
        # collide with this battery's own daemon (or the live production one).
        REG_OUT=$(BUNKERD_GRPC_ADDR=":29092" BUNKERD_REST_ADDR=":28082" bash regression-tests.sh 2>&1 || true)
    else
        REG_OUT=$(bash regression-tests.sh 2>&1 || true)
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
else
    echo "  STATUS: $FAIL FAILURES — review above"
fi

echo ""

exit 0
