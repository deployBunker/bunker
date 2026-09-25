#!/usr/bin/env bash
# Full agent lifecycle over raw unary REST — validated live on bunker-mvp 2026-09-25.
# Usage: BUNKER_TOKEN=... AGENT_ID=... SERVER=host:port bash rest_lifecycle.sh
set -u
: "${BUNKER_TOKEN:?set BUNKER_TOKEN}"
: "${AGENT_ID:-df-rest-demo}"
: "${SERVER:-127.0.0.1:8080}"
BASE="http://$SERVER/bunker.v1.Bunkerd"
H1='Content-Type: application/json'
H2="Authorization: Bearer $BUNKER_TOKEN"

step() { echo; echo "=== $1 ==="; }

step "SpawnAgent (snake_case in)"
curl -s -X POST "$BASE/SpawnAgent" -H "$H1" -H "$H2" \
  -d "{\"agent_id\":\"$AGENT_ID\",\"ttl\":\"15m\"}" | head -c 300; echo

step "GetAgent"
curl -s -X POST "$BASE/GetAgent" -H "$H1" -H "$H2" -d "{\"agent_id\":\"$AGENT_ID\"}" | head -c 300; echo

step "HeartbeatAgent"
curl -s -X POST "$BASE/HeartbeatAgent" -H "$H1" -H "$H2" -d "{\"agent_id\":\"$AGENT_ID\"}"; echo

step "AgentMetrics"
curl -s -X POST "$BASE/AgentMetrics" -H "$H1" -H "$H2" -d "{\"agent_id\":\"$AGENT_ID\"}"; echo

step "DestroyAgent (idempotency: 200 destroyed, re-destroy = 200 destroyed, never-known = 404)"
curl -s -X POST "$BASE/DestroyAgent" -H "$H1" -H "$H2" -d "{\"agent_id\":\"$AGENT_ID\"}"; echo
curl -s -X POST "$BASE/DestroyAgent" -H "$H1" -H "$H2" -d "{\"agent_id\":\"$AGENT_ID\"}"; echo

# Notes verified live 09-25:
# - no Authorization header -> 401 {"code":"unauthenticated",...}; GET -> 405
# - unknown RPC path -> plain-text 404 (router), NOT a JSON envelope
# - int64 fields are JSON strings (uptimeSeconds:"71420"), int32/float are numbers
# - ExecAgent is the ONLY streaming RPC: needs application/connect+json envelope
#   framing; see exec_agent_rest.py in this directory
# - RunAgent (unary) is detach-only: {"runId","status":"running","exitCode":-1,"unitName":...}
