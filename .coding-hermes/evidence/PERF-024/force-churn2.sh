#!/usr/bin/env bash
# PERF-024 forcing round 3: fresh scratch daemon + destroy->spawn churn with
# uid recycling (the e2e battery late-section shape that failed 3/3 in CI).
set -u
BIN=/root/perf024-bins
EV=/root/perf024-evidence
WORK=/root/perf024-churn-daemon
mkdir -p "$EV" "$WORK"
export BUNKER="$BIN/bunker"

cat > "$WORK/daemon.yaml" <<'EOF'
server:
  grpc_addr: ":39093"
  rest_addr: ":38083"
auth:
  enabled: false
tls:
  insecure_dev: true
agent:
  ssh_dir: /root/perf024-churn-daemon/ssh
  max_agents: 10
  port_range_start: 32000
  port_range_end: 32999
  port_range_per_agent: 100
  registry:
    enabled: false
EOF

pkill -f "perf024-churn-daemon/daemon.yaml" 2>/dev/null
sleep 1
nohup "$BIN/bunkerd" -c "$WORK/daemon.yaml" > "$EV/churn-daemon.log" 2>&1 &
DPID=$!
sleep 2
kill -0 $DPID 2>/dev/null || { echo "daemon died at startup"; tail -5 "$EV/churn-daemon.log"; exit 42; }
echo "churn daemon up pid $DPID"

$BUNKER connect http://127.0.0.1:38083 --name p24churn --token churn >/dev/null 2>&1

# journal capture
nohup journalctl -f -n 0 --since now 2>/dev/null | grep --line-buffered -E "Reloading requested|Reloading finished|user-1[0-9]{3}\.slice|slice\.d.*Unknown|useradd\[|userdel\[" > "$EV/churn2-journal.txt" 2>/dev/null &
JPID=$!

echo "CHURN2-START $(date -u +%FT%TZ)" >> "$EV/forcing-timeline.txt"
for i in 1 2 3; do
  echo "--- cycle $i: spawn p24c$i $(date -u +%H:%M:%S.%3N)" >> "$EV/forcing-timeline.txt"
  timeout 300 "$BUNKER" spawn --server p24churn --agent-id "p24c$i" --preset standard > "$EV/p24c$i-spawn.log" 2>&1
  RC=$?
  echo "spawn p24c$i rc=$RC at $(date -u +%H:%M:%S.%3N)" >> "$EV/forcing-timeline.txt"
  if [ $RC -ne 0 ]; then
    echo "SPAWN FAILED cycle $i:" >> "$EV/forcing-timeline.txt"
    tail -6 "$EV/p24c$i-spawn.log" >> "$EV/forcing-timeline.txt"
    # failed spawn: assert residue BEFORE anything else touches it
    NEWUID=$(id -u "bunker-p24c$i" 2>/dev/null || true)
    {
      echo "== cycle $i residue check (uid $NEWUID) at $(date -u +%H:%M:%S.%3N)"
      ls -la "/etc/systemd/system/user-$NEWUID.slice.d" 2>/dev/null || echo "  (no dir)"
      cat "/etc/systemd/system/user-$NEWUID.slice.d/50-bunker.conf" 2>/dev/null || echo "  (no conf)"
    } >> "$EV/churn-residue.txt"
    # the failed agent's user was rolled back; the residue dir should still be there (pre-fix)
  else
    echo "--- destroy p24c$i $(date -u +%H:%M:%S.%3N)" >> "$EV/forcing-timeline.txt"
    "$BUNKER" destroy --server p24churn "p24c$i" --force >> "$EV/churn2-destroy.log" 2>&1
    echo "destroy p24c$i rc=$? at $(date -u +%H:%M:%S.%3N)" >> "$EV/forcing-timeline.txt"
  fi
done
echo "CHURN2-DONE $(date -u +%FT%TZ)" >> "$EV/forcing-timeline.txt"
kill $JPID 2>/dev/null
kill $DPID 2>/dev/null
grep -E "landing|slice-limits|p24c" "$EV/churn-daemon.log" > "$EV/churn2-daemon-lines.txt" || true
echo DONE > "$EV/churn2-done"
