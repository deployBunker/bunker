#!/usr/bin/env bash
# GAP-079-LIVE — Phase C2: destroy the probe agent with the DEPLOYED CLI and
# prove no agent/user/host residue from this run is left behind.
set -u
S=/root/gap079-live/state
B=/usr/local/bin/bunker
export BUNKER_TOKEN="$(python3 /root/gap079-live/bin/token.py)"
echo "PHASE_C2_START_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo
echo "########## BEFORE DESTROY ##########"
id bunker-gap079live 2>&1 | head -1
ls -d /home/bunker-gap079live /run/bunker/gap079live 2>&1
ls /etc/bunkerd/ssh/ 2>&1
echo "linger=$(ls /var/lib/systemd/linger 2>/dev/null | tr '\n' ' ')"
echo
echo "########## DESTROY gap079live (deployed CLI 6a6ad20) ##########"
HOME="$S" BUNKER_HOME="$S" "$B" destroy gap079live --force 2>&1
echo "destroy_rc=$?"
sleep 4
echo
echo "########## AFTER DESTROY ##########"
id bunker-gap079live 2>&1 | head -1
ls -d /home/bunker-gap079live 2>&1
ls -d /run/bunker/gap079live 2>&1
echo "ssh_keys_present=$(ls /etc/bunkerd/ssh/ 2>/dev/null | tr '\n' ' ')"
echo "linger=$(ls /var/lib/systemd/linger 2>/dev/null | tr '\n' ' ')"
echo
echo "########## DAEMON-SIDE VIEW ##########"
HOME="$S" BUNKER_HOME="$S" "$B" list --status all 2>&1 | head -10
echo "--- bunker status (residue surface) ---"
HOME="$S" BUNKER_HOME="$S" "$B" status 2>&1 | head -20
echo
echo "########## HOST FINGERPRINT ##########"
bash /root/gap079-live/bin/host_state.sh | grep -E "docker_sock_forward_count|orphan_forward_count|bunker_users|bunker_homes|bunker_keys|ssh_key_names|linger_entries|linger_names|run_bunker_dirs|run_bunker_names|operator_config"
echo "PHASE_C2_END_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
