#!/usr/bin/env bash
# PERF-024 repro driver — runs on bunker-mvp as root.
# Captures: pre-state residue, live cgroup/systemctl poll during spawn,
# preset-battery standard tier (workloads none), daemon log, post-state residue.
set -u
BIN=/root/perf024-bins
LOGDIR=/root/perf024-evidence
mkdir -p "$LOGDIR"

echo "=== PRE-STATE $(date -u +%FT%TZ) ===" | tee "$LOGDIR/prestate.txt"
cat /proc/loadavg | tee -a "$LOGDIR/prestate.txt"
ls -d /etc/systemd/system/user-*.slice.d 2>/dev/null | tee -a "$LOGDIR/prestate.txt" || echo "(no residue dirs)" | tee -a "$LOGDIR/prestate.txt"
pgrep -af "bunkerd" | grep -v pgrep | tee -a "$LOGDIR/prestate.txt" || true

# ── side poller: every 100ms, sample every live user-1*.slice ──
cat > /root/perf024-poll.sh <<'EOF'
#!/usr/bin/env bash
END=$((SECONDS+900))
while [ $SECONDS -lt $END ]; do
  ts=$(date -u +%H:%M:%S.%3N)
  for d in /sys/fs/cgroup/user.slice/user-1*.slice; do
    [ -d "$d" ] || continue
    u=$(basename "$d"); u=${u#user-}; u=${u%.slice}
    sw=$(cat "$d/memory.swap.max" 2>/dev/null) || sw=ABSENT
    hi=$(cat "$d/memory.high" 2>/dev/null) || hi=ABSENT
    props=$(systemctl show "user-$u.slice" -p ActiveState -p LoadState -p MemorySwapMax -p MemoryHigh 2>/dev/null | tr '\n' ' ')
    echo "$ts uid=$u cgroup.swap.max=$sw cgroup.high=$hi | $props"
  done
  sleep 0.1
done
EOF
chmod +x /root/perf024-poll.sh
pkill -f perf024-poll.sh 2>/dev/null || true
nohup /root/perf024-poll.sh > "$LOGDIR/cgroup-poll.log" 2>&1 &
POLL_PID=$!
echo "poller pid $POLL_PID" | tee -a "$LOGDIR/prestate.txt"

# ── the battery run itself ──
export BUNKER="$BIN/bunker"
export BUNKERD_BIN="$BIN/bunkerd"
export PRESET_WORKLOADS=none
export PRESET_AGENT_PREFIX=perf024-
bash /root/perf024-preset-battery.sh --tier standard > "$LOGDIR/battery-run.log" 2>&1
RC=$?
echo "battery rc=$RC" | tee -a "$LOGDIR/battery-run.log"

kill "$POLL_PID" 2>/dev/null || true
sleep 1

echo "=== POST-STATE $(date -u +%FT%TZ) ===" > "$LOGDIR/poststate.txt"
cat /proc/loadavg >> "$LOGDIR/poststate.txt"
ls -d /etc/systemd/system/user-*.slice.d 2>/dev/null >> "$LOGDIR/poststate.txt"
for d in /etc/systemd/system/user-17*.slice.d; do [ -d "$d" ] && { echo "-- $d:"; ls -la "$d"; cat "$d"/*.conf 2>/dev/null; }; done >> "$LOGDIR/poststate.txt" 2>/dev/null
# slice-limits + landing lines from the scratch daemon log
grep -n "containment landing\|daemon-reload complete\|wrote user slice drop-in\|slice-limits stage failed\|spawn failed\|slice-limits removed\|slice-limits:" /tmp/preset-battery-bunkerd.log > "$LOGDIR/daemon-landing-lines.txt" 2>/dev/null
cp /tmp/preset-battery-bunkerd.log "$LOGDIR/daemon-full.log" 2>/dev/null
# journal lines for the run window
journalctl --since "-20 min" 2>/dev/null | grep -E "perf024|user-1[0-9]{3}\.slice|useradd|userdel" | grep -v snoopy > "$LOGDIR/journal-slices.txt"
echo DONE_RC=$RC > "$LOGDIR/DONE"
