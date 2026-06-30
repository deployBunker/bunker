#!/bin/bash
# Bunker JWT E2E Test — runs a temporary bunkerd with JWT enabled and verifies
# the agent-scoped sub-key flow end-to-end.
set -uo pipefail

PASS=0
FAIL=0

assert() { echo "  ✓ $1"; PASS=$((PASS+1)); }
fail()  { echo "  ✗ $1"; FAIL=$((FAIL+1)); }

echo "=========================================="
echo " BUNKER JWT END-TO-END TEST"
echo " $(date)"
echo "=========================================="
echo ""

JWT_SECRET="bunker-jwt-secret-must-be-at-least-32-bytes-long"
STATIC_TOKEN="test-regression-token"
TMP_CONFIG="/tmp/bunkerd-jwt-config.yaml"
GRPC_PORT=29090
REST_PORT=28080

cat > "$TMP_CONFIG" <<EOF
server:
  grpc_addr: ":${GRPC_PORT}"
  rest_addr: ":${REST_PORT}"

auth:
  enabled: true
  token: "${STATIC_TOKEN}"
  jwt_secret: "${JWT_SECRET}"
  jwt_ttl: "6h"

tls:
  enabled: false

agent:
  ssh_dir: /etc/bunkerd/ssh
  max_agents: 50
  default_cpu_quota: 2.0
  default_memory_bytes: 4294967296
  port_range_start: 30000
  port_range_end: 30999
  port_range_per_agent: 100

tunnel:
  enabled: false

named_tunnel:
  enabled: false

tailscale:
  enabled: false
EOF

# Start temporary bunkerd in the background.
echo "=== Starting temporary bunkerd with JWT enabled ==="
BUNKERD_CONFIG="$TMP_CONFIG" /usr/local/bin/bunkerd > /tmp/bunkerd-jwt.log 2>&1 &
BUNKERD_PID=$!
sleep 3

# Verify bunkerd is listening.
if ! ss -tlnp | grep -q ":${GRPC_PORT}"; then
    fail "temporary bunkerd not listening on :${GRPC_PORT}"
    echo "bunkerd log:"
    cat /tmp/bunkerd-jwt.log 2>/dev/null | tail -20 || true
    kill "$BUNKERD_PID" 2>/dev/null || true
    rm -f "$TMP_CONFIG" /tmp/bunkerd-jwt.log
    echo ""
    echo "  ✓ Pass:  $PASS"
    echo "  ✗ Fail:  $FAIL"
    exit 1
fi
assert "temporary bunkerd listening on :${GRPC_PORT}"

# Cleanup function.
cleanup() {
    echo ""
    echo "=== Cleanup ==="
    kill "$BUNKERD_PID" 2>/dev/null || true
    wait "$BUNKERD_PID" 2>/dev/null || true
    rm -f "$TMP_CONFIG" /tmp/bunkerd-jwt.log
}
trap cleanup EXIT

export BUNKER_TOKEN="${STATIC_TOKEN}"
BUNKER="/usr/local/bin/bunker"
URL="http://localhost:${GRPC_PORT}"

echo ""
echo "=== 1. Connect with static token fallback ==="
CONNECT_OUT=$($BUNKER connect "$URL" --token "$STATIC_TOKEN" --name jwt-test 2>&1)
if echo "$CONNECT_OUT" | grep -q "Connected\|Server registered"; then
    assert "connect with static token fallback"
else
    fail "connect with static token fallback — $CONNECT_OUT"
fi

echo ""
echo "=== 2. Reject missing token ==="
# Register a server entry with no saved token so the CLI cannot fall back.
CONNECT_NO_TOKEN_OUT=$($BUNKER connect "$URL" --name jwt-test-noauth 2>&1 || true)
NO_AUTH_OUT=$($BUNKER --server jwt-test-noauth list --status all 2>&1 || true)
if echo "$NO_AUTH_OUT" | grep -qi "unauthenticated\|invalid token\|missing\|401"; then
    assert "request without token rejected"
else
    fail "request without token — $NO_AUTH_OUT"
fi

echo ""
echo "=== 3. Reject invalid token ==="
BAD_AUTH_OUT=$(BUNKER_TOKEN="not-a-valid-token" $BUNKER --server jwt-test-noauth list --status all 2>&1 || true)
if echo "$BAD_AUTH_OUT" | grep -qi "unauthenticated\|invalid token\|401"; then
    assert "request with invalid token rejected"
else
    fail "request with invalid token — $BAD_AUTH_OUT"
fi

echo ""
echo "=== 4. Spawn agent and receive API sub-key ==="
SPAWN_OUT=$($BUNKER --server jwt-test spawn --agent-id "jwt-e2e" 2>&1)
if echo "$SPAWN_OUT" | grep -q "API Key:"; then
    assert "spawn returned API key"
    API_KEY=$(echo "$SPAWN_OUT" | grep "API Key:" | awk '{print $NF}')
else
    fail "spawn did not return API key — $SPAWN_OUT"
    API_KEY=""
fi

echo ""
echo "=== 5. Valid sub-key accepted ==="
if [ -n "$API_KEY" ]; then
    LIST_OUT=$(BUNKER_TOKEN="$API_KEY" $BUNKER --server jwt-test list --status all 2>&1 || true)
    if echo "$LIST_OUT" | grep -q "jwt-e2e\|No agents found"; then
        assert "valid sub-key accepted"
    else
        fail "valid sub-key rejected — $LIST_OUT"
    fi
else
    fail "skipped valid sub-key test (no API key)"
fi

echo ""
echo "=== 6. Destroy agent ==="
if [ -n "$API_KEY" ]; then
    DESTROY_OUT=$(BUNKER_TOKEN="$API_KEY" $BUNKER --server jwt-test destroy jwt-e2e --force 2>&1 || true)
    if echo "$DESTROY_OUT" | grep -q "destroyed"; then
        assert "destroy agent with sub-key"
    else
        fail "destroy with sub-key — $DESTROY_OUT"
    fi
else
    fail "skipped destroy with sub-key (no API key)"
fi

echo ""
echo "=========================================="
echo " JWT E2E RESULTS"
echo "=========================================="
echo "  ✓ Pass:  $PASS"
echo "  ✗ Fail:  $FAIL"
echo ""

if [ "$FAIL" -eq 0 ]; then
    echo "  STATUS: ALL JWT E2E TESTS PASS"
else
    echo "  STATUS: $FAIL FAILURES — review above"
fi

echo ""
exit "$FAIL"
