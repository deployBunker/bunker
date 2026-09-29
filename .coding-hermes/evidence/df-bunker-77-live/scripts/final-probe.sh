#!/usr/bin/env bash
# DF-BUNKER-77-LIVE: final mechanism probe — is the exec context inside a user
# namespace (which would explain host-uid-1071-owned paths reading as root)?
set -u
BASE=/root/df77-deploy
RUN=$BASE/live
export BUNKER_HOME="$RUN/cli-home"
export HOME="$BUNKER_HOME"
X=/opt/bunker/bunker
A=df77live1
S=df77live

echo "===== FINAL MECHANISM PROBE  $(date -u) ====="
"$X" exec "$A" --server "$S" -- sh -lc '
echo "--- effective ids ---"; id -u; id -g
echo "--- /proc/self/uid_map ---"; cat /proc/self/uid_map
echo "--- /proc/self/gid_map ---"; cat /proc/self/gid_map
echo "--- ownership as seen from the HOST-context path (same paths, host exec) ---"
echo "--- inside-container stats ---"
stat -c "%u:%g %a %n" /run/bunker/'"$A"' /run/bunker/'"$A"'/docker.sock /home/bunker-'"$A"' /home/bunker-'"$A"'/bin 2>&1
echo "--- can we write the agent HOME (yes expected: -v home:home, same uid) ---"
touch /home/bunker-'"$A"'/.writeprobe_df77 2>&1 && echo HOME-WRITABLE && rm -f /home/bunker-'"$A"'/.writeprobe_df77 || echo HOME-NOT-WRITABLE
echo "--- can we read the agent runtime env file if it existed (parent dir perms) ---"
ls -ld /run/bunker 2>&1
echo "--- group 110 / 65534 membership for our process ---"
cat /proc/self/status | grep -E "^(Uid|Gid|Groups):"
' 2>&1
echo
echo "===== and the HOST-context view of the same paths (no image agent) ====="
echo "(captured separately in the evidence doc from the spawn-time listing)"
echo "===== DONE ====="
