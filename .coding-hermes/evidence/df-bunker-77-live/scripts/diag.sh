#!/usr/bin/env bash
# DF-BUNKER-77-LIVE diagnostic: inside an image-spec agent's exec context,
# is DOCKER_HOST/agent-env injected into the CONTAINER, and is the agent's
# rootless socket reachable when the variable IS supplied?
set -u
BASE=/root/df77-deploy
RUN=$BASE/live
BUNKER=/opt/bunker/bunker
export BUNKER_HOME="$RUN/cli-home"
export HOME="$BUNKER_HOME"
A="${1:-df77live1}"
S=df77live
X="$(command -v "$BUNKER" || echo /opt/bunker/bunker)"

echo "===== PROBE 1: container env — which of PATH/DOCKER_HOST/TMPDIR reached it? ====="
"$X" exec "$A" --server "$S" -- sh -lc 'env | sort; echo "--- uid ---"; id -u; echo "--- hostname (container id) ---"; hostname' 2>&1
echo
echo "===== PROBE 2: is the agent runtime dir + rootless socket visible inside the container? ====="
"$X" exec "$A" --server "$S" -- sh -lc 'ls -la /run/bunker/'"$A"'/ 2>&1; echo "--- socket? ---"; ls -la /run/bunker/'"$A"'/docker.sock 2>&1; echo "--- /var/run/docker.sock? ---"; ls -la /var/run/docker.sock 2>&1' 2>&1
echo
echo "===== PROBE 3: can the container write the agent runtime dir (what env set needs)? ====="
"$X" exec "$A" --server "$S" -- sh -lc 'touch /run/bunker/'"$A"'/writeprobe 2>&1 && echo WRITABLE && rm -f /run/bunker/'"$A"'/writeprobe || echo NOT-WRITABLE; touch /tmp/writeprobe 2>&1 && echo TMP-WRITABLE && rm -f /tmp/writeprobe; touch "$HOME/writeprobe" 2>&1 && echo HOME-WRITABLE && rm -f "$HOME/writeprobe"' 2>&1
echo
echo "===== PROBE 4: same docker info, but with DOCKER_HOST supplied explicitly ====="
"$X" exec "$A" --server "$S" -- sh -lc 'DOCKER_HOST=unix:///run/bunker/'"$A"'/docker.sock docker info 2>&1 | sed -n "1,12p;/Server Version/p;/Rootless/p"' 2>&1
echo
echo "===== PROBE 5: does the HOST-context path (no image) have both? (agent env file + DOCKER_HOST) ====="
"$X" exec "$A" --server "$S" --raw -- sh -c 'echo "raw mode: env file is deliberately not sourced"' 2>&1 | head -3
echo
echo "===== PROBE 6: env set with a raised-privilege target — /tmp and \$HOME instead ====="
"$X" env set "$A" --server "$S" DF77_PROBE=ok 2>&1; echo "env_set_again_rc=$?"
echo
echo "===== PROBE 7: does exec see an env var set earlier (had A succeeded)? ====="
"$X" exec "$A" --server "$S" -- sh -lc 'echo "DF77_LIVE=[${DF77_LIVE:-<unset>}]"' 2>&1
echo "===== DIAG DONE ====="
