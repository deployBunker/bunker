#!/usr/bin/env bash
# GAP-079-LIVE — Phase E2: second full battery run, invoked as a FILE (not piped
# into `bash -s`) so the wrapper's post-battery lines cannot be lost when a child
# process reads the caller's stdin. Battery output is tee'd to a host-local file
# as well as this script's stdout.
set -u
mkdir -p /root/gap079-live
cd /opt/bunker || exit 1
export BUNKER_TOKEN="$(python3 /root/gap079-live/bin/token.py)"
export BUNKER_STRICT_BIN=1
LOG=/root/gap079-live/battery2.log
: > "$LOG"
echo "BATTERY2_START_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "cwd=$(pwd)"
echo "repo HEAD=$(git rev-parse HEAD)"
echo "BUNKER_STRICT_BIN=$BUNKER_STRICT_BIN"
echo "BUNKERD_COEXIST=${BUNKERD_COEXIST:-<unset - standalone mode>}"
echo "token_len=${#BUNKER_TOKEN} (value never printed)"
echo
bash e2e-full-battery.sh 2>&1 | tee "$LOG"
rc=${PIPESTATUS[0]}
echo
echo "BATTERY_EXIT_CODE=$rc"
echo "VERIFY_PASS_LINES=$(grep -c 'VERIFY-PASS' "$LOG")"
echo "VERIFY_FAIL_LINES=$(grep -c 'VERIFY-FAIL' "$LOG")"
echo "STATUS_LINE=$(grep 'STATUS:' "$LOG" | tail -1)"
echo "PASS_FAIL_NOTES=$(grep -E 'Pass:|Fail:|Notes:' "$LOG" | tr -d ' ' | tr '\n' ' ')"
echo "TUNNEL_REAP_LINES=$(grep -c 'tunnel reap check' "$LOG")"
echo "BATTERY2_END_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo
echo "########## POST-RUN RESIDUE (same checks as phase F) ##########"
bash /root/gap079-live/bin/phaseF.sh
echo "PHASE_E2_DONE_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
