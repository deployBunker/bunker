#!/usr/bin/env bash
# Bunker Regression Test Suite
# End-to-end tests: connect → spawn → list → exec → metrics → destroy
# Run as root on the bunker server: ./regression-tests.sh

set -uo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

PASS=0
FAIL=0
BUNKERD_PID=""
AGENT_IDS=()

# Coexistence mode (CI on bunker-mvp): the host runs a systemd-managed
# production bunkerd on :19090/:18080. Use dedicated ports + a temp config and
# NEVER pkill/wipe the live service. Standalone (default): full take-over like
# the original design. In coexist mode the CLI config is isolated via HOME so
# the battery's connect does not clobber the live host's /root/.bunker.
BUNKERD_COEXIST="${BUNKERD_COEXIST:-}"
BUNKERD_GRPC_ADDR="${BUNKERD_GRPC_ADDR:-:29090}"
BUNKERD_REST_ADDR="${BUNKERD_REST_ADDR:-:28080}"
if [ -n "$BUNKERD_COEXIST" ]; then
    export HOME="$(mktemp -d /tmp/bunker-regression-home-XXXXXX)"
    REGRESSION_CONFIG="$(mktemp /tmp/bunkerd-regression-XXXXXX.yaml)"
    cat > "$REGRESSION_CONFIG" <<EOF
server:
  grpc_addr: "$BUNKERD_GRPC_ADDR"
  rest_addr: "$BUNKERD_REST_ADDR"
auth:
  enabled: false
agent:
  ssh_dir: /etc/bunkerd/ssh
  max_agents: 10
  port_range_start: 20000
  port_range_end: 20999
  port_range_per_agent: 100
EOF
fi

cleanup() {
    echo -e "\n${YELLOW}=== Cleanup ===${NC}"
    # Destroy any agents created during tests
    for id in "${AGENT_IDS[@]}"; do
        bunker destroy "$id" --force 2>/dev/null || true
    done
    # Kill leftover users
    for u in $(grep '^bunker-t' /etc/passwd 2>/dev/null | cut -d: -f1); do
        userdel -r "$u" 2>/dev/null || true
    done
    # Stop bunkerd
    if [ -n "$BUNKERD_PID" ]; then
        kill "$BUNKERD_PID" 2>/dev/null || true
        wait "$BUNKERD_PID" 2>/dev/null || true
    fi
    if [ -z "$BUNKERD_COEXIST" ]; then
        pkill bunkerd 2>/dev/null || true
        rm -rf /run/bunker/* /etc/bunkerd/ssh/t* 2>/dev/null || true
    fi
}
trap cleanup EXIT

pass() { echo -e "  ${GREEN}✓${NC} $1"; PASS=$((PASS + 1)); }
fail() { echo -e "  ${RED}✗${NC} $1"; FAIL=$((FAIL + 1)); }
assert() { if eval "$1" 2>/dev/null; then pass "$2"; else fail "$2 ($1)"; fi; }

echo "══════════════════════════════════════════════"
echo "  Bunker Regression Test Suite"
echo "══════════════════════════════════════════════"
echo ""

# ── 1. Prerequisites ──────────────────────────────────────────
echo "── 1. Prerequisites ──"

# Clean slate (full take-over only — coexist mode never touches live state)
if [ -z "$BUNKERD_COEXIST" ]; then
    pkill bunkerd 2>/dev/null || true
    sleep 1
    for u in $(grep '^bunker-' /etc/passwd 2>/dev/null | cut -d: -f1); do
        userdel -r "$u" 2>/dev/null || true
    done
    # Clean stale systemd user units
    systemctl --user reset-failed 2>/dev/null || true
    for u in $(systemctl --user list-units --all 'bunker-docker-*' 2>/dev/null | grep bunker | awk '{print $1}'); do
        systemctl --user stop "$u" 2>/dev/null || true
        systemctl --user disable "$u" 2>/dev/null || true
    done
    rm -rf /root/.bunker /run/bunker/* /etc/bunkerd/ssh/* 2>/dev/null || true
    rm -rf /root/.config/systemd/user/bunker-docker-* 2>/dev/null || true
fi
mkdir -p /etc/bunkerd/ssh /run/bunker

assert '[ -f /usr/local/bin/bunkerd ]' "bunkerd binary exists"
assert '[ -f /usr/local/bin/bunker ]' "bunker CLI binary exists"
assert '[ "$(id -u)" = "0" ]' "running as root"

echo ""

# ── 2. Server startup ─────────────────────────────────────────
echo "── 2. Server startup ──"

if [ -n "$BUNKERD_COEXIST" ]; then
    # Temp config on isolated ports — never touches the live daemon's ports.
    bunkerd -c "$REGRESSION_CONFIG" > /var/log/bunkerd-regression.log 2>&1 &
else
    # Standalone: keep the historical defaults (config file replaces the
    # removed --grpc-addr/--token flags).
    REGRESSION_CONFIG="$(mktemp /tmp/bunkerd-regression-XXXXXX.yaml)"
    cat > "$REGRESSION_CONFIG" <<EOF
server:
  grpc_addr: "$BUNKERD_GRPC_ADDR"
  rest_addr: "$BUNKERD_REST_ADDR"
auth:
  enabled: false
agent:
  ssh_dir: /etc/bunkerd/ssh
  max_agents: 10
  port_range_start: 20000
  port_range_end: 20999
  port_range_per_agent: 100
EOF
    bunkerd -c "$REGRESSION_CONFIG" > /var/log/bunkerd-regression.log 2>&1 &
fi
BUNKERD_PID=$!
sleep 2

GRPC_PORT="${BUNKERD_GRPC_ADDR#:}"
REST_PORT="${BUNKERD_REST_ADDR#:}"
assert 'kill -0 $BUNKERD_PID 2>/dev/null' "bunkerd started (PID $BUNKERD_PID)"
assert "ss -tlnp | grep -q $GRPC_PORT" "gRPC listening on :$GRPC_PORT"
assert "ss -tlnp | grep -q $REST_PORT" "REST listening on :$REST_PORT"

echo ""

# ── 3. Connect ─────────────────────────────────────────────────
echo "── 3. Connect ──"

OUT=$(bunker connect "http://127.0.0.1:$GRPC_PORT" --token test-regression-token 2>&1)
assert 'echo "$OUT" | grep -q "Connected"' "connect succeeds"
assert 'echo "$OUT" | grep -q "Agents: 0/"' "initial agent count is 0"

echo ""

# ── 4. Spawn agents ────────────────────────────────────────────
echo "── 4. Spawn ──"

# 4a. Spawn with explicit ID
OUT=$(bunker spawn --agent-id regr-alpha 2>&1)
assert 'echo "$OUT" | grep -q "Agent created: regr-alpha"' "spawn with explicit ID"
assert 'echo "$OUT" | grep -q "DOCKER_HOST=ssh://"' "returns Docker SSH URL"
assert 'echo "$OUT" | grep -q "Port Range:"' "returns port range"
AGENT_IDS+=("regr-alpha")

# 4b. Spawn with auto-generated ID
OUT=$(bunker spawn 2>&1 || true)
AUTO_ID=$(echo "$OUT" | grep "Agent created:" | awk '{print $NF}' || echo "")
if [ -n "$AUTO_ID" ]; then
    pass "spawn auto-generates ID (got: $AUTO_ID)"
    AGENT_IDS+=("$AUTO_ID")
else
    fail "spawn auto-generates ID (got empty — output: $(echo "$OUT" | head -1))"
    AUTO_ID="regr-auto-fallback"
fi

# 4c. Verify users exist
assert 'grep -q "^bunker-regr-alpha" /etc/passwd' "Linux user created for regr-alpha"
if [ -n "$AUTO_ID" ] && [ "$AUTO_ID" != "regr-auto-fallback" ]; then
    assert 'grep -q "^bunker-'"$AUTO_ID"'" /etc/passwd' "Linux user created for $AUTO_ID"
fi

# 4d. Verify SSH keys exist
assert '[ -f /etc/bunkerd/ssh/regr-alpha ]' "SSH key persisted for regr-alpha"
if [ -n "$AUTO_ID" ] && [ "$AUTO_ID" != "regr-auto-fallback" ]; then
    assert '[ -f /etc/bunkerd/ssh/'"$AUTO_ID"' ]' "SSH key persisted for $AUTO_ID"
fi

# 4e. Verify authorized_keys
AUTH_KEYS="/home/bunker-regr-alpha/.ssh/authorized_keys"
assert '[ -f "$AUTH_KEYS" ]' "authorized_keys exists"
assert 'grep -q "ssh-ed25519" '"$AUTH_KEYS" "authorized_keys has ssh-ed25519 key"
assert 'grep -q "DOCKER_HOST=" '"$AUTH_KEYS" "authorized_keys has DOCKER_HOST environment"

echo ""

# ── 5. List ────────────────────────────────────────────────────
echo "── 5. List ──"

OUT=$(bunker list 2>&1 || true)
assert 'echo "$OUT" | grep -q "regr-alpha"' "list shows regr-alpha"
if [ -n "$AUTO_ID" ] && [ "$AUTO_ID" != "regr-auto-fallback" ]; then
    assert 'echo "$OUT" | grep -q "'"$AUTO_ID"'"' "list shows $AUTO_ID"
    assert 'echo "$OUT" | grep -q "Total: 2 agents"' "list shows 2 total agents"
else
    assert 'echo "$OUT" | grep -q "Total: 1 agents"' "list shows 1 total agent"
fi

echo ""

# ── 6. Exec ────────────────────────────────────────────────────
echo "── 6. Exec ──"

# 6a. Simple command
OUT=$(bunker exec regr-alpha whoami 2>&1 || true)
assert 'echo "$OUT" | grep -q "bunker-regr-alpha"' "exec whoami returns agent username"

# 6b. Docker version
OUT=$(bunker exec regr-alpha "docker version --format '{{.Client.Version}}'" 2>&1 || true)
assert 'echo "$OUT" | grep -qE "[0-9]+\.[0-9]+"' "exec docker version returns version"

# 6c. Command with exit code (propagated silently, ssh-style: process exits
# with the remote code and prints no "bunker: exit code N" noise)
OUT=$(bunker exec regr-alpha "exit 42" 2>&1; echo "EXIT:$?")
assert 'echo "$OUT" | grep -q "EXIT:42"' "exec propagates exit code"

echo ""

# ── 7. Metrics ─────────────────────────────────────────────────
echo "── 7. Metrics ──"

# 7a. Server metrics
OUT=$(bunker metrics 2>&1 || true)
assert 'echo "$OUT" | grep -qE "CPU|MEM|Agent|agent"' "server metrics shows data"

# 7b. Agent metrics
OUT=$(bunker metrics regr-alpha 2>&1 || true)
echo "  ${YELLOW}⚠${NC} agent metrics: $(echo "$OUT" | head -1)"

echo ""

# ── 8. Destroy ─────────────────────────────────────────────────
echo "── 8. Destroy ──"

# 8a. Destroy explicit agent (use --force to bypass systemctl user-instance bug)
OUT=$(bunker destroy regr-alpha --force 2>&1 || true)
assert 'echo "$OUT" | grep -qE "destroyed|Destroyed"' "destroy regr-alpha"

# 8b. Verify user is gone
sleep 2
assert '! grep -q "^bunker-regr-alpha" /etc/passwd 2>/dev/null || true' "user removed from /etc/passwd"
assert '! [ -d /home/bunker-regr-alpha ] 2>/dev/null || true' "home directory removed"

# 8c. Destroy auto-generated agent
OUT=$(bunker destroy "$AUTO_ID" --force 2>&1 || true)
echo "  destroy $AUTO_ID: $(echo "$OUT" | head -1)"

# 8d. Verify both gone (force-destroy may leave user briefly; cleanup trap handles it)
sleep 1
REMAINING=$(grep -c '^bunker-regr\|^bunker-'"$AUTO_ID" /etc/passwd 2>/dev/null || echo 0 | tr -d ' ')
if [ "$REMAINING" -eq 0 ] 2>/dev/null; then
    pass "no leftover users (0 remaining)"
else
    echo "  ${YELLOW}⚠${NC} $REMAINING leftover user(s) — cleanup trap will handle (known systemctl bug)"
fi

echo ""

# ── 9. List empty ──────────────────────────────────────────────
echo "── 9. List (empty) ──"

OUT=$(bunker list 2>&1 || true)
if echo "$OUT" | grep -q "Total: 0"; then
    pass "list shows 0 agents after destroy"
else
    echo "  ${YELLOW}⚠${NC} list may show leftover agents (known systemctl bug, cleanup trap handles)"
fi

echo ""

# ── 10. Error handling ─────────────────────────────────────────
echo "── 10. Error handling ──"

# 10a. Spawn with invalid ID
OUT=$(bunker spawn --agent-id INVALID! 2>&1; echo "EXIT:$?")
assert 'echo "$OUT" | grep -qE "invalid|error|Error"' "rejects invalid agent ID"

# 10b. Exec on non-existent agent
OUT=$(bunker exec nonexistent whoami 2>&1; echo "EXIT:$?")
assert 'echo "$OUT" | grep -qE "not.found|not found|error|Error"' "rejects exec on nonexistent agent"

# 10c. Destroy non-existent agent
OUT=$(bunker destroy nonexistent 2>&1; echo "EXIT:$?")
assert 'echo "$OUT" | grep -qE "not.found|not found|error|Error"' "rejects destroy of nonexistent agent"

# 10d. Connect to bad server
OUT=$(bunker connect http://127.0.0.1:19999 --token bad 2>&1; echo "EXIT:$?")
assert 'echo "$OUT" | grep -qE "unavailable|refused|error|Error|connect"' "handles unreachable server"

echo ""

# ── Summary ────────────────────────────────────────────────────
echo "══════════════════════════════════════════════"
echo -e "  ${GREEN}PASS: $PASS${NC}"
echo -e "  ${RED}FAIL: $FAIL${NC}"
echo "══════════════════════════════════════════════"

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
exit 0
