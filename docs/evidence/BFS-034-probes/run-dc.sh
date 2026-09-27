#!/usr/bin/env bash
# BFS-034 — run the WAN battery against a REAL datacentre.
#
# What this script owns, on a host that also runs a LIVE bunkerd:
#   * a spare high port (never 18080/18090), verified free before use;
#   * a fixture tree created ON THE DC and a davserve of that tree, detached with
#     setsid and a pidfile, so teardown is `kill <pid>` and never a pattern kill;
#   * an ssh tunnel instead of opening a firewall port when the DC's ufw is DROP.
# What it does NOT do: deploy to, restart, or reconfigure the live bunkerd; touch
# 18080; or leave anything behind (teardown is verified, not asserted).
#
#   bash docs/evidence/BFS-034-probes/run-dc.sh <ssh-host> <public-ip-or-127.0.0.1> \
#        <remote-port> <local-port-or-0> [extra battery args...]
#   local-port 0 => connect to the DC's public IP directly (no tunnel)
set -uo pipefail

HOST="${1:?usage: run-dc.sh <ssh-host> <public-host> <remote-port> <local-port|0> [extra...]}"
PUB="${2:?public host}"
RPORT="${3:?remote port}"
LPORT="${4:-0}"
shift 4 || true
EXTRA=("$@")

WT="${WT:-/home/kara/worktrees/bunker-BFS-034}"
TS="$(date +%Y%m%d%H%M%S)"
RDIR="/tmp/bunker-wan-$TS"
CDIR="${CDIR:-/tmp/bfs034-dc-$TS}"
SSH="ssh -o BatchMode=yes -o LogLevel=ERROR -o ConnectTimeout=15 $HOST"

mkdir -p "$CDIR/bin" "$CDIR/mnt"
cd "$WT" || exit 1

echo "== pre-flight: what is live on $HOST (must stay untouched) =="
$SSH 'uname -m; echo "--- bunkerd ---"; systemctl is-active bunkerd 2>/dev/null; echo "--- listeners 18080/19090 ---"; ss -ltn 2>/dev/null | grep -E ":(18080|19090)\b" || echo none; echo "--- spare port $RPORT ---"; ss -ltn 2>/dev/null | grep -c ":$RPORT\b"'

echo
echo "== build + push the probe server (linux/amd64) =="
ARCH="$($SSH 'uname -m')"
case "$ARCH" in
  x86_64|amd64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) echo "unsupported remote arch: $ARCH"; exit 1 ;;
esac
GOOS=linux GOARCH="$GOARCH" go build -o "$CDIR/bin/davserve" ./probes/davserve || exit 1
go build -o "$CDIR/bin/bunker" ./cmd/bunker || exit 1
$SSH "mkdir -p $RDIR/bin $RDIR/logs"
tar -C "$CDIR/bin" -cf - davserve | $SSH "tar -C $RDIR/bin -xf -" || exit 1
echo "  davserve pushed: local md5 $(md5sum "$CDIR/bin/davserve" | cut -d' ' -f1) / remote md5 $($SSH "md5sum $RDIR/bin/davserve" | cut -d' ' -f1)"

cat > "$CDIR/remote-serve.sh" <<'EOS'
#!/bin/sh
# remote-serve.sh RDIR PORT TREE — pidfile first, then exec (pid is preserved),
# so teardown is `kill <pid>` rather than a pattern kill.
RD="$1"; PORT="$2"; TREE="$3"
echo $$ > "$RD/pid"
exec "$RD/bin/davserve" --root "$TREE" --addr "$4:$PORT"
EOS
tar -C "$CDIR" -cf - remote-serve.sh | $SSH "tar -C $RDIR -xf -"

echo
echo "== create the fixture ON THE DC (the tree nobody here mirrors) =="
bash probes/bunker-fs-battery-wan.sh \
  --url "http://$PUB:$RPORT/dav" --ssh "$HOST" --tree "$RDIR/tree" \
  --mnt "$CDIR/mnt" --bin "$CDIR/bin/bunker" --fixture-only 2>&1 | sed 's/^/  /'

echo
echo "== start the probe server on the DC (spare port $RPORT) =="
BIND=0.0.0.0
TUNNEL_PID=""
if [ "$LPORT" != "0" ]; then BIND=127.0.0.1; fi
$SSH "cd $RDIR && setsid --fork sh $RDIR/remote-serve.sh $RDIR $RPORT $RDIR/tree $BIND </dev/null >$RDIR/logs/srv.log 2>&1 & sleep 2; echo -n 'pid='; cat $RDIR/pid 2>/dev/null; echo; echo -n 'listening='; ss -ltn | grep -c \":$RPORT\"; echo '--- log ---'; head -3 $RDIR/logs/srv.log" 

if [ "$LPORT" != "0" ]; then
  echo "  opening an ssh tunnel 127.0.0.1:$LPORT -> $HOST:127.0.0.1:$RPORT (the DC keeps ufw DROP;"
  echo "   no firewall change is made on a live host). Tunnel pid is captured, never pattern-killed."
  ssh -N -o BatchMode=yes -o ExitOnForwardFailure=yes \
      -L "127.0.0.1:$LPORT:127.0.0.1:$RPORT" "$HOST" </dev/null >"$CDIR/tunnel.log" 2>&1 &
  TUNNEL_PID=$!
  URL="http://127.0.0.1:$LPORT/dav"
else
  URL="http://$PUB:$RPORT/dav"
fi

echo
echo "== reachability from the CLIENT side ($URL) =="
for i in $(seq 1 20); do
  if curl -sS -m 10 -o /dev/null -w '%{http_code} %{time_total}s\n' "$URL/go.mod" 2>/dev/null; then break; fi
  sleep 1
done
curl -sS -m 10 -o /dev/null -w '  bind preflight via the client: %{http_code} in %{time_total}s\n' "$URL/" || true

echo
echo "== THE BATTERY (every write verified on the DC side, over ssh) =="
STOP="ssh -o BatchMode=yes $HOST 'kill \$(cat $RDIR/pid)'"
bash probes/bunker-fs-battery-wan.sh \
  --url "$URL" --ssh "$HOST" --tree "$RDIR/tree" \
  --mnt "$CDIR/mnt" --bin "$CDIR/bin/bunker" \
  --control --stop-endpoint "$STOP" \
  --csv "$CDIR/battery-dc.csv" "${EXTRA[@]}"
RC=$?
echo "battery rc=$RC"

echo
echo "== teardown (verified, not asserted) =="
$SSH "kill \$(cat $RDIR/pid) 2>/dev/null; sleep 1; echo -n 'probe port listeners now: '; ss -ltn | grep -c \":$RPORT\"; echo -n 'probe pid alive: '; (kill -0 \$(cat $RDIR/pid) 2>/dev/null && echo yes || echo no)"
$SSH "rm -rf -- $RDIR"
echo -n "  remote dir removed: "; $SSH "test -e $RDIR && echo NO || echo yes"
echo -n "  live bunkerd still: "; $SSH 'systemctl is-active bunkerd 2>/dev/null'
echo -n "  live listeners 18080/19090: "; $SSH 'ss -ltn 2>/dev/null | grep -cE ":(18080|19090)\b"'
if [ "$LPORT" != "0" ]; then
  kill "$TUNNEL_PID" 2>/dev/null
  sleep 0.5
  echo "  tunnel pid $TUNNEL_PID alive: $(kill -0 "$TUNNEL_PID" 2>/dev/null && echo yes || echo no)"
fi
echo "  local mounts left: $(awk -v m="$CDIR/mnt" '$2 ~ m {n++} END{print n+0}' /proc/mounts)"
echo "artifacts: $CDIR"
exit $RC
