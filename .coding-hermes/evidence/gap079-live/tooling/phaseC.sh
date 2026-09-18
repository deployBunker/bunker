#!/usr/bin/env bash
# GAP-079-LIVE — Phase C: the deployed HEAD CLI on the same live agent.
# Same probe, same agent, same state dir as phase A; only the binary differs.
set -u
S=/root/gap079-live/state
B=/usr/local/bin/bunker
export BUNKER_TOKEN="$(python3 /root/gap079-live/bin/token.py)"
echo "BUNKER_TOKEN_LEN=${#BUNKER_TOKEN} (value never printed)"
echo "PHASE_C_START_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo
echo "########## DEPLOYED CLIENT IDENTITY ##########"
echo "binary: $B"
sha256sum "$B"
"$B" version
echo "repo HEAD (checkout): $(git -C /opt/bunker rev-parse HEAD)"
echo
echo "########## PROBE 3: SIGTERM, DEPLOYED HEAD CLI (--docker) ##########"
python3 /root/gap079-live/bin/tunnel_probe.py post-fix-TERM "$B" "$S" gap079live TERM --docker
echo "probe_TERM_rc=$?"
echo
echo "########## PROBE 4: SIGKILL, DEPLOYED HEAD CLI ##########"
python3 /root/gap079-live/bin/tunnel_probe.py post-fix-KILL "$B" "$S" gap079live KILL
echo "probe_KILL_rc=$?"
echo
echo "########## POST-PHASE-C FORWARDS ##########"
bash /root/gap079-live/bin/host_state.sh | grep -E "docker_sock_forward|orphan_forward|forward pid|local"
echo "PHASE_C_END_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
