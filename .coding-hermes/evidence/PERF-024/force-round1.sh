#!/usr/bin/env bash
# PERF-024 forcing round: concurrent standard spawns against the live battery
# daemon, with a cgroup poller capturing the landing window.
set -u
BIN=/root/perf024-bins
EV=/root/perf024-evidence
mkdir -p "$EV"
export BUNKER="$BIN/bunker"

$BUNKER connect http://127.0.0.1:38081 --name p24tgt --token preset-battery-token >/dev/null 2>&1

cat > /root/perf024-poll2.sh <<'EOF2'
#!/usr/bin/env bash
END=$((SECONDS+600))
while [ $SECONDS -lt $END ]; do
  ts=$(date -u +%H:%M:%S.%3N)
  for d in /sys/fs/cgroup/user.slice/user-17*.slice /sys/fs/cgroup/user.slice/user-18*.slice; do
    [ -d "$d" ] || continue
    u=$(basename "$d"); u=${u#user-}; u=${u%.slice}
    sw=$(cat "$d/memory.swap.max" 2>/dev/null) || sw=ABSENT
    hi=$(cat "$d/memory.high" 2>/dev/null) || hi=ABSENT
    props=$(systemctl show "user-$u.slice" -p ActiveState -p MemorySwapMax -p MemoryHigh 2>/dev/null | tr '\n' ' ')
    echo "$ts uid=$u cgroup.swap.max=$sw cgroup.high=$hi | $props"
  done
  sleep 0.1
done
EOF2
chmod +x /root/perf024-poll2.sh
pkill -f perf024-poll2.sh 2>/dev/null
nohup /root/perf024-poll2.sh > "$EV/cgroup-poll2.log" 2>&1 &
echo "poll2 pid $!"

echo "FORCED-START $(date -u +%FT%TZ)" >> "$EV/forcing-timeline.txt"
nohup "$BUNKER" spawn --server p24tgt --agent-id p24-force1 --preset standard > "$EV/force1.log" 2>&1 &
F1=$!
nohup "$BUNKER" spawn --server p24tgt --agent-id p24-force2 --preset standard > "$EV/force2.log" 2>&1 &
F2=$!
echo "f1=$F1 f2=$F2 at $(date -u +%H:%M:%S)" >> "$EV/forcing-timeline.txt"
wait $F1 $F2
echo "FORCED-DONE $(date -u +%FT%TZ)" >> "$EV/forcing-timeline.txt"
echo "--- force1:"; tail -3 "$EV/force1.log"
echo "--- force2:"; tail -3 "$EV/force2.log"
pkill -f perf024-poll2.sh 2>/dev/null
# capture the daemon's landing lines for the forced spawns
grep -E "p24-force|user-17[0-9]{2}\.slice" /tmp/preset-battery-bunkerd.log | grep -E "landing|drop-in|reload|slice-limits|failed" | tail -20 > "$EV/forced-daemon-lines.txt"
