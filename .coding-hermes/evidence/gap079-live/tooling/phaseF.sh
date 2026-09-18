#!/usr/bin/env bash
# GAP-079-LIVE — Phase F: post-run residue / orphan inspection on bunker-mvp.
set -u
S=/root/gap079-live/state
B=/usr/local/bin/bunker
export BUNKER_TOKEN="$(python3 /root/gap079-live/bin/token.py)"
echo "PHASE_F_START_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo
echo "########## (1) docker-sock SSH forwards: '-L <port>:...docker.sock' ##########"
python3 - <<'PY'
import os
PROC = '/proc'
rows = []
for pid in os.listdir(PROC):
    if not pid.isdigit():
        continue
    try:
        stat = open('%s/%s/stat' % (PROC, pid)).read()
        ppid = int(stat[stat.rindex(')') + 2:].split()[1])
        args = [a for a in open('%s/%s/cmdline' % (PROC, pid), 'rb').read().decode('utf8', 'replace').split('\0') if a]
    except Exception:
        continue
    rows.append((int(pid), ppid, args))
fw = []
for pid, ppid, args in rows:
    specs = []
    for i, a in enumerate(args):
        if a == '-L' and i + 1 < len(args):
            specs.append(args[i + 1])
        elif a.startswith('-L') and len(a) > 2:
            specs.append(a[2:])
    for s in specs:
        if s.endswith('docker.sock'):
            fw.append((pid, ppid, s, ' '.join(args)))
print('docker_sock_forward_count=%d' % len(fw))
for pid, ppid, s, a in fw:
    print('  forward pid=%d ppid=%d spec=%s' % (pid, ppid, s))
print('orphan_forward_count=%d' % len([f for f in fw if f[1] == 1]))
ssh = [r for r in rows if r[2] and r[2][0].endswith('sshd')]
print('sshd_processes=%d' % len(ssh))
for pid, ppid, args in ssh:
    print('  sshd pid=%d ppid=%d %s' % (pid, ppid, ' '.join(args)[:90]))
PY
echo
echo "########## (2) any ssh client process at all ##########"
ps -eo pid,ppid,user,etime,args | awk 'NR==1 || $0 ~ /[s]sh /' | head -5
echo
echo "########## (3) bunkerd service state ##########"
echo "is_active=$(systemctl is-active bunkerd)"
systemctl show bunkerd -p MainPID -p NRestarts -p ExecMainStartTimestamp
echo "healthz=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/healthz)"
echo
echo "########## (4) agents registered (daemon view) ##########"
HOME="$S" BUNKER_HOME="$S" "$B" list --status all 2>&1 | head -5
echo "--- bunker status ---"
HOME="$S" BUNKER_HOME="$S" "$B" status 2>&1 | grep -E "Agents|Residue|Uptime|Version|Status"
echo
echo "########## (5) docker containers / agent dockerd ##########"
echo "docker_ps_count=$(docker ps -q 2>/dev/null | wc -l)"
echo "agent_dockerd_count=$(ps -eo args | grep -c '^/usr/bin/dockerd.*--user')"
echo
echo "########## (6) users / homes / keys / linger / run dirs ##########"
bash /root/gap079-live/bin/host_state.sh | grep -E "bunker_users|bunker_homes|bunker_keys|ssh_key_names|linger_entries|linger_names|run_bunker_dirs|run_bunker_names|operator_config|battery_cli_state_dirs|bunker_tunnel_logs"
echo
echo "########## (7) battery scratch leftovers ##########"
echo "battery_cli_state_dirs=$(ls -d /tmp/bunker-battery-cli-* 2>/dev/null | wc -l)"
echo "bunkerd_battery_configs=$(ls /tmp/bunkerd-battery-* 2>/dev/null | wc -l)"
echo "tunnel_logs=$(ls /tmp/bunker-tunnel-* 2>/dev/null | tr '\n' ' ')"
echo "battery_run_dirs=$(ls -d /run/bunker/e2e-* 2>/dev/null | wc -l)"
echo
echo "########## (8) deferred agent ids from the battery (should be gone) ##########"
for a in e2e-main e2e-agent-2 e2e-agent-3 e2e-agent-4 e2e-agent-5 e2e-imgspec e2e-imgspec-b gap070-idem gap075-a gap075-b; do
    if id "bunker-$a" >/dev/null 2>&1; then echo "  LEFTOVER user bunker-$a"; fi
    if [ -d "/home/bunker-$a" ]; then echo "  LEFTOVER home /home/bunker-$a"; fi
done
echo "leftover_scan_done=1"
echo
echo "########## (9) the battery's own reap check, re-run post-run ##########"
bash /root/gap079-live/bin/reap_check.sh
echo "PHASE_F_END_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
