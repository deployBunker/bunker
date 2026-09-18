#!/usr/bin/env bash
# GAP-079-LIVE — Phase A (clean single run): control with the PRE-FIX CLI.
# The binary is commit acfe850, built from the revision immediately before the
# GAP-079 fix (1295b86 reap / cc61695 Pdeathsig). Same host, same agent id, same
# probe as phase C — only the binary differs.
set -u
S=/root/gap079-live/state
B=/root/gap079-live/bin/bunker-prefix
export BUNKER_TOKEN="$(python3 /root/gap079-live/bin/token.py)"
echo "BUNKER_TOKEN_LEN=${#BUNKER_TOKEN} (value never printed)"
echo "PHASE_A_START_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo
echo "########## PRE-FIX CLIENT IDENTITY ##########"
echo "binary: $B"
sha256sum "$B"
"$B" version
echo
echo "########## CLEAN SLATE: destroy the agent left by the aborted first attempt ##########"
HOME="$S" BUNKER_HOME="$S" "$B" destroy gap079live --force 2>&1 || true
sleep 3
echo
echo "########## BEFORE STATE (after clean slate) ##########"
bash /root/gap079-live/bin/host_state.sh
echo
echo "########## CONNECT (isolated state dir $S) ##########"
mkdir -p "$S"
HOME="$S" BUNKER_HOME="$S" "$B" connect http://localhost:18080
echo
echo "########## SPAWN agent gap079live (pre-fix CLI) ##########"
HOME="$S" BUNKER_HOME="$S" "$B" spawn --agent-id gap079live 2>&1
echo
echo "########## READINESS POLL (rootless dockerd + docker.sock) ##########"
ready=0
for i in $(seq 1 60); do
    if [ -S /run/bunker/gap079live/docker.sock ] && pgrep -u bunker-gap079live dockerd >/dev/null 2>&1; then
        echo "agent_ready after $((i*2))s"
        ready=1
        break
    fi
    sleep 2
done
echo "ready=$ready"
pgrep -u bunker-gap079live dockerd | sed 's/^/agent_dockerd_pid=/'
ls -la /run/bunker/gap079live/docker.sock 2>&1
ls -la "$S/keys/" 2>&1
echo
echo "########## PROBE 1: SIGTERM, PRE-FIX CLI (--docker) ##########"
python3 /root/gap079-live/bin/tunnel_probe.py conf-prefix-TERM "$B" "$S" gap079live TERM --docker
echo "probe_TERM_rc=$?"
echo
echo "########## PROBE 2: SIGKILL, PRE-FIX CLI ##########"
python3 /root/gap079-live/bin/tunnel_probe.py conf-prefix-KILL "$B" "$S" gap079live KILL
echo "probe_KILL_rc=$?"
echo
echo "########## POST-PHASE-A FORWARDS ##########"
bash /root/gap079-live/bin/host_state.sh | grep -E "docker_sock_forward_count|orphan_forward_count|forward pid|sshd_processes"
echo "PHASE_A_END_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
