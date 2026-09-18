#!/usr/bin/env bash
# GAP-079-LIVE host-state fingerprint. Read-only; prints a copy-pasteable block.
set -uo pipefail
echo "snapshot_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "hostname=$(hostname)"
echo "--- /opt/bunker checkout ---"
echo "checkout_HEAD=$(git -C /opt/bunker rev-parse HEAD 2>/dev/null)"
echo "checkout_origin_main=$(git -C /opt/bunker rev-parse origin/main 2>/dev/null)"
echo "--- deployed binaries (commit line) ---"
for b in /opt/bunker/bunkerd /opt/bunker/bunker /usr/local/bin/bunkerd /usr/local/bin/bunker; do
    printf '%s: ' "$b"
    "$b" --version 2>/dev/null | awk '/commit:/{print $2}' || true
done
echo "--- systemd ---"
echo "bunkerd_is_active=$(systemctl is-active bunkerd 2>/dev/null)"
echo "bunkerd_MainPID=$(systemctl show bunkerd -p MainPID --value 2>/dev/null)"
echo "bunkerd_NRestarts=$(systemctl show bunkerd -p NRestarts --value 2>/dev/null)"
echo "bunkerd_ExecMainStart=$(systemctl show bunkerd -p ExecMainStartTimestamp --value 2>/dev/null)"
echo "bunkerd_exe=$(readlink -f "/proc/$(systemctl show bunkerd -p MainPID --value 2>/dev/null)/exe" 2>/dev/null)"
echo "healthz=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/healthz 2>/dev/null)"
echo "--- docker ---"
echo "docker_ps_count=$(docker ps -q 2>/dev/null | wc -l)"
echo "--- process table facts ---"
python3 - <<'PY'
import os
PROC='/proc'
rows=[]
for pid in os.listdir(PROC):
    if not pid.isdigit(): continue
    try:
        stat=open('%s/%s/stat'%(PROC,pid)).read()
        ppid=int(stat[stat.rindex(')')+2:].split()[1])
        args=[a for a in open('%s/%s/cmdline'%(PROC,pid),'rb').read().decode('utf8','replace').split('\0') if a]
    except Exception: continue
    rows.append((int(pid),ppid,args))
fw=[]
for pid,ppid,args in rows:
    specs=[]
    for i,a in enumerate(args):
        if a=='-L' and i+1<len(args): specs.append(args[i+1])
        elif a.startswith('-L') and len(a)>2: specs.append(a[2:])
    for s in specs:
        if s.endswith('docker.sock'): fw.append((pid,ppid,s,' '.join(args)))
print('docker_sock_forward_count=%d'%len(fw))
for pid,ppid,s,a in fw:
    print('  forward pid=%d ppid=%d spec=%s'%(pid,ppid,s))
print('orphan_forward_count=%d'%len([f for f in fw if f[1]==1]))
ssh=[r for r in rows if r[2] and r[2][0].endswith('sshd')]
print('sshd_processes=%d'%len(ssh))
for pid,ppid,args in ssh:
    print('  sshd pid=%d ppid=%d %s'%(pid,ppid,' '.join(args)[:90]))
d=( [r for r in rows if any('dockerd' in a for a in r[2]) and r[2][0].startswith('/usr/bin/dockerd')] )
print('agent_dockerd_count_free_text=%d'%len([r for r in rows if '--user' in ' '.join(r[2]) and 'dockerd' in ' '.join(r[2])]))
print('battery_procs=%d'%len([r for r in rows if any('e2e-full-battery' in a for a in r[2]) or any('regression-tests.sh' in a for a in r[2])]))
PY
echo "--- users / homes / keys / linger ---"
echo "bunker_users=$(awk -F: '/^bunker-/ {print $1}' /etc/passwd 2>/dev/null | wc -l)"
echo "bunker_homes=$(ls -d /home/bunker-* 2>/dev/null | wc -l)"
echo "bunker_keys=$(ls /etc/bunkerd/ssh 2>/dev/null | wc -l)"
echo "ssh_key_names=$(ls /etc/bunkerd/ssh 2>/dev/null | tr '\n' ' ')"
echo "linger_entries=$(ls /var/lib/systemd/linger 2>/dev/null | wc -l)"
echo "linger_names=$(ls /var/lib/systemd/linger 2>/dev/null | tr '\n' ' ')"
echo "run_bunker_dirs=$(ls -d /run/bunker/* 2>/dev/null | wc -l)"
echo "run_bunker_names=$(ls -d /run/bunker/* 2>/dev/null | tr '\n' ' ')"
echo "--- operator CLI config (fingerprint only) ---"
echo "operator_config_sha256=$(sha256sum /root/.bunker/config.yaml 2>/dev/null | awk '{print $1}')"
echo "operator_config_mtime=$(stat -c %y /root/.bunker/config.yaml 2>/dev/null)"
echo "--- battery scratch ---"
echo "battery_cli_state_dirs=$(ls -d /tmp/bunker-battery-cli-* 2>/dev/null | wc -l)"
echo "bunker_tunnel_logs=$(ls /tmp/bunker-tunnel-* 2>/dev/null | wc -l)"
echo "scratch_dir=/root/gap079-live"
