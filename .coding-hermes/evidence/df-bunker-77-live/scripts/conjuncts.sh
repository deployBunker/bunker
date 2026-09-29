#!/usr/bin/env bash
# DF-BUNKER-77-LIVE step 6: the FOUR live conjuncts, verbatim, against the
# image-spec agent spawned in the up phase.
set -u
BASE=/root/df77-deploy
RUN=$BASE/live
BUNKER=/opt/bunker/bunker
export BUNKER_HOME="$RUN/cli-home"
export HOME="$BUNKER_HOME"
AGENT="${1:-df77live1}"
SRV=df77live

echo "########## DF-BUNKER-77-LIVE CONJUNCTS  agent=$AGENT  server=$SRV  $(date -u) ##########"
echo
echo "===== CONJUNCT A: bunker env set <id> K=V -> rc 0 ====="
"$BUNKER" env set "$AGENT" --server "$SRV" DF77_LIVE=1 2>&1
echo "conjunct_a_rc=$?"
echo "--- read it back ---"
"$BUNKER" env get "$AGENT" --server "$SRV" DF77_LIVE 2>&1
echo "env_get_rc=$?"
echo
echo "===== CONJUNCT B: bunker exec <id> -- id -u (must NOT be 0) ====="
"$BUNKER" exec "$AGENT" --server "$SRV" -- id -u 2>&1
echo "conjunct_b_rc=$?"
echo "--- also: whoami / uname (context) ---"
"$BUNKER" exec "$AGENT" --server "$SRV" -- sh -lc 'id; uname -a' 2>&1
echo
echo "===== CONJUNCT C: bunker exec <id> -- docker info (rootless 29.x) ====="
"$BUNKER" exec "$AGENT" --server "$SRV" -- docker info 2>&1 | tee "$RUN/docker-info.txt" | head -40
echo "conjunct_c_rc=${PIPESTATUS[0]}"
echo "--- docker info: rootless / server version lines ---"
grep -iE "server version|rootless|security options|cgroup|storage driver|docker root dir" "$RUN/docker-info.txt" 2>/dev/null | head -20
echo
echo "===== CONJUNCT D: agent-tools probe -> git PRESENT (real agent userland) ====="
"$BUNKER" agent-tools "$AGENT" --server "$SRV" 2>&1 | tee "$RUN/agent-tools.txt"
echo "conjunct_d_rc=${PIPESTATUS[0]}"
echo
echo "===== SPEC-SPECIFIC BONUS: the image's own package + stock userland INSIDE the exec context ====="
echo "--- which rg / rg --version (proves the apt package from the image spec is reachable) ---"
"$BUNKER" exec "$AGENT" --server "$SRV" -- sh -lc 'command -v rg && rg --version' 2>&1
echo "--- which git / git --version / docker --version / python3 (proves stock userland survived, DF-BUNKER-80) ---"
"$BUNKER" exec "$AGENT" --server "$SRV" -- sh -lc 'command -v git && git --version; command -v docker && docker --version; command -v python3 && python3 --version' 2>&1
echo "--- the recorded image ref on the agent record (why exec is container-wrapped) ---"
"$BUNKER" info "$AGENT" --server "$SRV" 2>&1 | grep -iE "image|container|id|status" | head -15
echo
echo "########## CONJUNCTS DONE ##########"
