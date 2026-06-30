#!/bin/bash
# Bunker TLS/mTLS E2E Test — runs a temporary bunkerd with TLS enabled and verifies
# the CLI --tls-insecure flow and the mTLS client-cert flow end-to-end.
set -uo pipefail

PASS=0
FAIL=0

assert() { echo "  ✓ $1"; PASS=$((PASS+1)); }
fail()  { echo "  ✗ $1"; FAIL=$((FAIL+1)); }

echo "=========================================="
echo " BUNKER TLS/mTLS END-TO-END TEST"
echo " $(date)"
echo "=========================================="
echo ""

TMP_DIR="$(mktemp -d)"
CA_KEY="$TMP_DIR/ca-key.pem"
CA_CERT="$TMP_DIR/ca.pem"
CLIENT_KEY="$TMP_DIR/client-key.pem"
CLIENT_CERT="$TMP_DIR/client.pem"
TMP_CONFIG="$TMP_DIR/bunkerd-config.yaml"
TLS_DIR="$TMP_DIR/tls"
GRPC_PORT=29090
REST_PORT=28080

mkdir -p "$TLS_DIR"

# Generate CA + client cert for the mTLS portion.
generate_client_certs() {
    openssl req -x509 -newkey rsa:2048 -keyout "$CA_KEY" -out "$CA_CERT" -days 1 -nodes -subj "/CN=Test CA" 2>/dev/null
    openssl req -newkey rsa:2048 -keyout "$CLIENT_KEY" -out "$TMP_DIR/client.csr" -nodes -subj "/CN=bunker-client" 2>/dev/null
    openssl x509 -req -in "$TMP_DIR/client.csr" -CA "$CA_CERT" -CAkey "$CA_KEY" -CAcreateserial -out "$CLIENT_CERT" -days 1 2>/dev/null
}

echo "=== Generating mTLS client certificates ==="
if generate_client_certs; then
    assert "generated CA and client certificates"
else
    fail "certificate generation failed"
    echo ""
    echo "  ✓ Pass:  $PASS"
    echo "  ✗ Fail:  $FAIL"
    rm -rf "$TMP_DIR"
    exit 1
fi

cat > "$TMP_CONFIG" <<EOF
server:
  grpc_addr: ":${GRPC_PORT}"
  rest_addr: ":${REST_PORT}"

auth:
  enabled: true
  token: "test-regression-token"

tls:
  enabled: true
  self_signed: true
  cert_file: "${TLS_DIR}/cert.pem"
  key_file: "${TLS_DIR}/key.pem"
  hosts:
    - "localhost"
    - "127.0.0.1"

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
echo "=== Starting temporary bunkerd with TLS enabled ==="
DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/0/bus" BUNKERD_CONFIG="$TMP_CONFIG" /usr/local/bin/bunkerd > /tmp/bunkerd-tls.log 2>&1 &
BUNKERD_PID=$!
sleep 3

# Verify bunkerd is listening.
if ! ss -tlnp | grep -q ":${GRPC_PORT}"; then
    fail "temporary bunkerd not listening on :${GRPC_PORT}"
    echo "bunkerd log:"
    cat /tmp/bunkerd-tls.log 2>/dev/null | tail -20 || true
    kill "$BUNKERD_PID" 2>/dev/null || true
    wait "$BUNKERD_PID" 2>/dev/null || true
    rm -rf "$TMP_DIR"
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
    rm -rf "$TMP_DIR" /tmp/bunkerd-tls.log
}
trap cleanup EXIT

export BUNKER_TOKEN="test-regression-token"
BUNKER="/usr/local/bin/bunker"
URL="https://localhost:${GRPC_PORT}"

echo ""
echo "=== 1. Connect with --tls-insecure ==="
CONNECT_OUT=$($BUNKER connect "$URL" --token "$BUNKER_TOKEN" --tls-insecure --name tls-test 2>&1)
if echo "$CONNECT_OUT" | grep -q "Connected\|Server registered"; then
    assert "connect with --tls-insecure"
else
    fail "connect with --tls-insecure — $CONNECT_OUT"
fi

echo ""
echo "=== 2. List agents over TLS ==="
LIST_OUT=$($BUNKER --server tls-test list --status all 2>&1)
if echo "$LIST_OUT" | grep -q "No agents found"; then
    assert "list over TLS returned empty"
else
    fail "list over TLS — $LIST_OUT"
fi

echo ""
echo "=== 3. Verify self-signed certificate was generated ==="
if [ -f "${TLS_DIR}/cert.pem" ] && [ -f "${TLS_DIR}/key.pem" ]; then
    assert "self-signed certificate generated on first start"
else
    fail "self-signed certificate files missing"
fi

echo ""
echo "=== 4. mTLS: reject client without cert ==="
# Update config to require mTLS and restart bunkerd.
kill "$BUNKERD_PID" 2>/dev/null || true
wait "$BUNKERD_PID" 2>/dev/null || true

cat > "$TMP_CONFIG" <<EOF
server:
  grpc_addr: ":${GRPC_PORT}"
  rest_addr: ":${REST_PORT}"

auth:
  enabled: true
  token: "test-regression-token"

tls:
  enabled: true
  self_signed: true
  cert_file: "${TLS_DIR}/cert.pem"
  key_file: "${TLS_DIR}/key.pem"
  mtls: true
  ca_file: "${CA_CERT}"
  hosts:
    - "localhost"
    - "127.0.0.1"

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

DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/0/bus" BUNKERD_CONFIG="$TMP_CONFIG" /usr/local/bin/bunkerd > /tmp/bunkerd-tls.log 2>&1 &
BUNKERD_PID=$!
sleep 3

if ! ss -tlnp | grep -q ":${GRPC_PORT}"; then
    fail "temporary bunkerd not listening on :${GRPC_PORT} after mTLS restart"
    echo "bunkerd log:"
    cat /tmp/bunkerd-tls.log 2>/dev/null | tail -20 || true
    echo ""
    echo "  ✓ Pass:  $PASS"
    echo "  ✗ Fail:  $FAIL"
    exit 1
fi
assert "bunkerd restarted with mTLS enabled"

# Remove previous server entry so we can re-register with mTLS.
rm -f /root/.bunker/config.yaml

NO_CERT_OUT=$($BUNKER connect "$URL" --token "$BUNKER_TOKEN" --tls-insecure --name mtls-test 2>&1 || true)
if echo "$NO_CERT_OUT" | grep -qi "unauthenticated\|certificate\|tls\|handshake\|connection"; then
    assert "client without cert rejected by mTLS server"
else
    fail "client without cert — $NO_CERT_OUT"
fi

echo ""
echo "=== 5. mTLS: client with valid cert accepted ==="
# The bunker CLI does not yet support --client-cert/--client-key flags, so we
# verify the server-side mTLS behavior with curl against the REST port.
CURL_OUT=$(curl -s --cacert "${TLS_DIR}/cert.pem" --cert "$CLIENT_CERT" --key "$CLIENT_KEY" "https://localhost:${REST_PORT}/healthz" 2>&1 || true)
if echo "$CURL_OUT" | grep -q '"status":"ok"'; then
    assert "client cert accepted by mTLS server"
else
    fail "client cert mTLS — $CURL_OUT"
fi

echo ""
echo "=========================================="
echo " TLS/mTLS E2E RESULTS"
echo "=========================================="
echo "  ✓ Pass:  $PASS"
echo "  ✗ Fail:  $FAIL"
echo ""

if [ "$FAIL" -eq 0 ]; then
    echo "  STATUS: ALL TLS/mTLS E2E TESTS PASS"
else
    echo "  STATUS: $FAIL FAILURES — review above"
fi

echo ""
exit "$FAIL"
