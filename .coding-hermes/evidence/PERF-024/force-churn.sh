#!/usr/bin/env bash
# PERF-024 forcing round 2: destroy->spawn churn on the live battery daemon,
# mimicking the e2e battery's late sections (spawn after destroy, uid recycling).
set -u
BIN=/root/perf024-bins
EV=/root/perf024-evidence
mkdir -p "$EV"
export BUNKER="$BIN/bunker"

# journal capture in parallel
nohup journalctl -f -n 0 --since now 2>/dev/null | grep --line-buffered -E "Reloading requested|Reloading finished|user-17[0-9]{2}\.slice|slice\.d|useradd\[|userdel\[" > "$EV/churn-journal.txt" 2>/dev/null &
JPID=$!

echo "CHURN-START $(date -u +%FT%TZ)" >> "$EV/forcing-timeline.txt"
for i in 1 2 3; do
  echo "--- cycle $i: spawn p24-churn$i $(date -u +%H:%M:%S.%3N)" >> "$EV/forcing-timeline.txt"
  timeout 300 "$BUNKER" spawn --server p24tgt --agent-id "p24-churn$i" --preset standard > "$EV/churn$i-spawn.log" 2>&1
  RC=$?
  echo "spawn p24-churn$i rc=$RC at $(date -u +%H:%M:%S.%3N)" >> "$EV/forcing-timeline.txt"
  if [ $RC -ne 0 ]; then
    echo "SPAWN FAILED cycle $i:" >> "$EV/forcing-timeline.txt"
    tail -5 "$EV/churn$i-spawn.log" >> "$EV/forcing-timeline.txt"
  fi
  echo "--- destroy p24-churn$i $(date -u +%H:%M:%S.%3N)" >> "$EV/forcing-timeline.txt"
  "$BUNKER" destroy --server p24tgt "p24-churn$i" --force >> "$EV/churn-destroy.log" 2>&1
  echo "destroy p24-churn$i rc=$? at $(date -u +%H:%M:%S.%3N)" >> "$EV/forcing-timeline.txt"
done
echo "CHURN-DONE $(date -u +%FT%TZ)" >> "$EV/forcing-timeline.txt"
kill $JPID 2>/dev/null

grep -E "p24-churn|landing|slice-limits" /tmp/preset-battery-bunkerd.log | tail -40 > "$EV/churn-daemon-lines.txt"
echo DONE > "$EV/churn-done"
