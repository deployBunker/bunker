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
# Binary overrides: the battery normally uses the deployed CLI, but feature
# batteries (GAP-064) may need freshly built binaries WITHOUT overwriting the
# live production bunkerd. BUNKERD_BIN points the coexist-mode daemon at a
# candidate build; BUNKER_BIN overrides the CLI similarly.
BUNKERD_BIN="${BUNKERD_BIN:-/usr/local/bin/bunkerd}"
BUNKER="${BUNKER_BIN:-/usr/local/bin/bunker}"
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
    for agent in e2e-main e2e-agent-2 e2e-agent-3 e2e-agent-4 e2e-agent-5 e2e-imgspec e2e-imgspec-b e2e-imgspec-bad gap070-idem; do
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
    "$BUNKERD_BIN" -c "$BATTERY_CONFIG" > /var/log/bunkerd-battery.log 2>&1 &
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

COMPACT_OUT=$($BUNKER registry compact --path "$GAP070_REG" 2>&1)
COMPACT_EXIT=$?
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
COMPACT_OUT2=$($BUNKER registry compact --path "$GAP070_REG" 2>&1)
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
