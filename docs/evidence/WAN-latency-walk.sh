#!/usr/bin/env bash
# BOTH arms, one run, one fixture, one relay - so the comparison cannot be contaminated by a
# restarted server. First attempt failed on an empty CLI arg; second failed because I had killed the
# upstream it pointed at. This one starts everything itself.
set -uo pipefail
T=$(mktemp -d /tmp/wan2-XXXXXX)
mkdir -p "$T/src" "$T/mnt-snapshot" "$T/mnt-nosnapshot"
for d in src sub1 sub2 sub3; do mkdir -p "$T/src/$d"; done
for f in $(seq 1 120); do
  printf 'content for file %s\n' "$f" > "$T/src/src/f$f.txt"
  printf 'x\n' > "$T/src/sub3/s$f.txt"
done
printf 'top\n' > "$T/src/top.txt"
N=$(find "$T/src" -type f | wc -l)
echo "  fixture: $N files"

cd /home/kara/bunker || exit 1
go build -o "$T/davserve" ./probes/davserve 2>&1 | head -2
go build -o "$T/bunker" ./cmd/bunker 2>&1 | head -3
"$T/davserve" --root "$T/src" --addr 127.0.0.1:0 > "$T/srv.log" 2>&1 &
SRV=$!
for i in $(seq 1 60); do UPORT=$(grep -oE ':[0-9]{4,5}' "$T/srv.log" 2>/dev/null | head -1 | tr -d ':'); [ -n "$UPORT" ] && break; sleep 0.25; done
echo "  davserve :${UPORT:-NOT-UP} (pid $SRV)"
[ -z "${UPORT:-}" ] && { echo "  server did not start"; cat "$T/srv.log" | head -3; exit 1; }

LPORT=39199
python3 /tmp/delayrelay.py $LPORT $UPORT 0.09262 > "$T/relay.log" 2>&1 &
RELAY=$!
sleep 1.5
head -1 "$T/relay.log" | sed 's/^/  /'
URL="http://127.0.0.1:$LPORT/dav"
echo "  --- the delay is really in the path (loopback would be ~1 ms) ---"
for i in 1 2 3; do
  /usr/bin/time -f '    one GET: %e s' curl -s -o /dev/null "$URL/top.txt" 2>&1 | tail -1
done

walk () {
  local label="$1"; local m="$T/mnt-$label"; local extra="$2"
  timeout 150 "$T/bunker" fs mount "$m" --url "$URL" --cache-dir "$T/cache-$label" $extra > "$T/mount-$label.log" 2>&1 &
  local MP=$!
  for i in $(seq 1 120); do mountpoint -q "$m" 2>/dev/null && break; sleep 0.5; done
  if ! mountpoint -q "$m"; then
    echo "  $label: MOUNT FAILED"; tail -3 "$T/mount-$label.log" | sed 's/^/      /'
    kill -KILL $MP 2>/dev/null; return 1
  fi
  local t0=$(date +%s%N)
  timeout 400 ls -lR "$m" > "$T/walk-$label.txt" 2>&1; local rc=$?
  local t1=$(date +%s%N)
  echo "  $label: ls -lR = $(( (t1-t0)/1000000 )) ms (rc=$rc, $(wc -l < "$T/walk-$label.txt") entries)"
  timeout 30 fusermount3 -uz "$m" 2>/dev/null || timeout 30 fusermount -uz "$m" 2>/dev/null
  kill -KILL $MP 2>/dev/null
  return 0
}

echo
echo "=== BOTH ARMS at ~185 ms RTT (the snapshot arm first, this time) ==="
walk "snapshot" ""
sleep 2
walk "nosnapshot" "--no-snapshot"

echo
echo "=== the comparison ==="
S=$(grep -oE 'snapshot: ls -lR = [0-9]+' "$T/../$(basename $T)" 2>/dev/null | grep -oE '[0-9]+' || true)
python3 - "$T" <<'PY' 2>/dev/null || true
PY
echo "  (figures printed above; snapshot should be ~1 round trip, no-snapshot many)"

echo
echo "=== cleanup ==="
kill -KILL $SRV 2>/dev/null; kill -KILL $RELAY 2>/dev/null
sleep 1
echo "  leftover mounts: $(grep -c "$T" /proc/mounts 2>/dev/null); bunker procs: $(pgrep -x bunker|wc -l)"
echo "  artifacts at $T"
