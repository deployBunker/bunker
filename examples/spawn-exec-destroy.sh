#!/usr/bin/env bash
# Bunker example — spawn → exec → destroy agent lifecycle.
#
# Demonstrates the full agent workflow against a running bunkerd:
#   1. connect     — register the server and token in ~/.bunker/config.yaml
#   2. spawn       — create an agent, capture its ID from the output
#   3. exec        — run a command inside the agent
#   4. destroy     — tear the agent down
#
# Usage:
#   bunkerd --config examples/dev-noauth.yaml &   # start a local daemon (no auth)
#   ./examples/spawn-exec-destroy.sh http://127.0.0.1:9090
#
# With an auth-enabled daemon, export BUNKER_TOKEN first:
#   BUNKER_TOKEN=your-master-token-here ./examples/spawn-exec-destroy.sh https://host:9443
#
# Set SERVER_NAME to name the server entry (default: derived from the host).
set -euo pipefail

SERVER_URL="${1:-http://127.0.0.1:9090}"
SERVER_NAME="${SERVER_NAME:-demo}"

echo "==> connect: $SERVER_URL"
bunker connect "$SERVER_URL" --name "$SERVER_NAME"

echo "==> spawn"
SPAWN_OUT="$(bunker spawn)"
echo "$SPAWN_OUT"
# The spawn output includes a line like:  Agent created: <agent-id>
AGENT_ID="$(printf '%s\n' "$SPAWN_OUT" | sed -n 's/^Agent created: //p' | head -n1)"
if [[ -z "$AGENT_ID" ]]; then
  echo "ERROR: could not parse agent ID from spawn output" >&2
  exit 1
fi
echo "==> agent id: $AGENT_ID"

echo "==> exec"
bunker exec "$AGENT_ID" -- uname -a

echo "==> destroy"
bunker destroy "$AGENT_ID"

echo "==> done: agent $AGENT_ID created, exec'd, and destroyed"
