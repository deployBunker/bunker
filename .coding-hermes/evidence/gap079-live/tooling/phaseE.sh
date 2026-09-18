#!/usr/bin/env bash
# GAP-079-LIVE — Phase E: the full E2E battery, standalone mode, documented
# invocation (deployed daemon on the production ports, real token from
# /etc/bunkerd/config.yaml, BUNKER_STRICT_BIN=1 so a MISMATCH is fatal before
# any host mutation).
set -u
cd /opt/bunker || exit 1
export BUNKER_TOKEN="$(python3 /root/gap079-live/bin/token.py)"
export BUNKER_STRICT_BIN=1
echo "BATTERY_START_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "cwd=$(pwd)"
echo "repo HEAD=$(git rev-parse HEAD)"
echo "BUNKER_BIN=${BUNKER_BIN:-<default /usr/local/bin/bunker>}"
echo "BUNKERD_BIN=${BUNKERD_BIN:-<default /usr/local/bin/bunkerd>}"
echo "BUNKER_STRICT_BIN=$BUNKER_STRICT_BIN"
echo "BUNKERD_COEXIST=${BUNKERD_COEXIST:-<unset - standalone mode>}"
echo "token_len=${#BUNKER_TOKEN} (value never printed)"
echo
bash e2e-full-battery.sh
echo "BATTERY_EXIT_CODE=$?"
echo "BATTERY_END_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
