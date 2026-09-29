#!/usr/bin/env bash
# DF-BUNKER-77-LIVE control + identity probes.
#  A) compact in-container identity (how the runtime dir/socket are owned
#     inside the exec context),
#  B) the daemon's own audit record of the exec argv,
#  C) the DISCRIMINATING CONTROL: the same four probes on a PLAIN agent
#     (no image spec) on the same daemon — if docker/env-set work there and
#     fail on the image agent, the failure is the image-exec path, not the host.
set -u
BASE=/root/df77-deploy
RUN=$BASE/live
BUNKER=/opt/bunker/bunker
export BUNKER_HOME="$RUN/cli-home"
export HOME="$BUNKER_HOME"
IMG=df77live1
PLAIN=df77plain1
S=df77live
X=$BUNKER

echo "########## CONTROL + IDENTITY PROBES  $(date -u) ##########"
echo
echo "===== A) in-container identity of the image-spec agent ====="
"$X" exec "$IMG" --server "$S" -- sh -lc 'echo "--- id ---"; id; echo "--- stat runtime dir + socket (uid:gid mode name, INSIDE the container) ---"; stat -c "%u:%g %a %n" /run/bunker/'"$IMG"' /run/bunker/'"$IMG"'/docker.sock /run/bunker/'"$IMG"'/env 2>&1; echo "--- does the image know uid 1071? ---"; (getent passwd 1071 || echo "no passwd entry for 1071"); echo "--- passwd size ---"; wc -l < /etc/passwd; echo "--- socket group names ---"; (getent group 110 || echo "no group 110")' 2>&1
echo
echo "===== B) daemon audit record of the exec (verbatim argv, if logged) ====="
grep -h "df77live1" /var/log/bunkerd/audit.log 2>/dev/null | grep -i "exec\|docker run" | tail -3 || echo "   (no audit records with an exec argv)"
echo "   -- audit log tail (last 3 lines, any agent) --"
tail -3 /var/log/bunkerd/audit.log 2>/dev/null || echo "   (no audit log)"
echo
echo "===== C) DISCRIMINATING CONTROL: spawn a PLAIN agent (no image spec) ====="
timeout 600 "$X" spawn --server "$S" --agent-id "$PLAIN" --ttl 2h 2>&1 | tail -12
echo "spawn_plain_rc=$?"
echo
echo "--- plain agent: id -u ---"
"$X" exec "$PLAIN" --server "$S" -- id -u 2>&1
echo "--- plain agent: container env has DOCKER_HOST? (expect host-context: full env) ---"
"$X" exec "$PLAIN" --server "$S" -- sh -lc 'echo "DOCKER_HOST=${DOCKER_HOST:-<unset>}"; echo "TMPDIR=${TMPDIR:-<unset>}"' 2>&1
echo "--- plain agent: docker info (the SAME command that fails on the image agent) ---"
"$X" exec "$PLAIN" --server "$S" -- docker info 2>&1 | grep -iE "^ *(Server Version|Storage Driver|Rootless|Security Options|Cgroup)|error|denied|Version:" | head -12
echo "plain_docker_info_rc=$?"
echo "--- plain agent: env set (the SAME command that fails on the image agent) ---"
"$X" env set "$PLAIN" --server "$S" DF77_CONTROL=ok 2>&1; echo "plain_env_set_rc=$?"
"$X" env get "$PLAIN" --server "$S" DF77_CONTROL 2>&1; echo "plain_env_get_rc=$?"
echo "--- plain agent: agent-tools (expect rg ABSENT: rg comes only from the image spec) ---"
"$X" agent-tools "$PLAIN" --server "$S" 2>&1 | head -14
echo
echo "===== D) destroy the control agent ====="
timeout 400 "$X" destroy "$PLAIN" --server "$S" --force 2>&1 | tail -6
echo "destroy_plain_rc=$?"
echo "########## CONTROL PROBES DONE ##########"
