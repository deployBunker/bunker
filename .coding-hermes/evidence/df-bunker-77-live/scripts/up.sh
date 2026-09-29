#!/usr/bin/env bash
# DF-BUNKER-77-LIVE step 3/5: start an ISOLATED (coexist) daemon from the
# deployed HEAD binary and spawn a real image-spec agent (apt: ripgrep).
#
# Isolation shape = the one e2e-full-battery.sh uses in BUNKERD_COEXIST mode
# (own ports, auth off, registry DISABLED so the startup reconciliation walk
# cannot run at all) plus its own base_data_dir, so it cannot touch the
# production demo daemon's tenants or the CI runner's agents on this shared host.
set -u
BASE=/root/df77-deploy
RUN=$BASE/live
mkdir -p "$RUN"
BUNKER=/opt/bunker/bunker
CONF="$RUN/bunkerd-df77live.yaml"

export BUNKER_HOME="$RUN/cli-home"
export HOME="$BUNKER_HOME"
mkdir -p "$BUNKER_HOME"

cat > "$CONF" <<'YAML'
server:
  grpc_addr: ":29093"
  rest_addr: ":28083"
auth:
  enabled: false
tls:
  insecure_dev: true
agent:
  base_data_dir: /var/lib/bunkerd-df77live
  ssh_dir: /etc/bunkerd-df77live/ssh
  max_agents: 10
  port_range_start: 32000
  port_range_end: 32999
  port_range_per_agent: 100
  registry:
    enabled: false
image_spec:
  enabled: true
  cache_dir: /var/cache/bunkerd/imagespec-df77live
YAML
mkdir -p /etc/bunkerd-df77live/ssh /var/lib/bunkerd-df77live /var/cache/bunkerd/imagespec-df77live

cat > "$RUN/spec.json" <<'JSON'
{"packages":[{"manager":"apt","packages":["ripgrep"]}]}
JSON

if [ -f "$RUN/bunkerd.pid" ]; then
  kill "$(cat "$RUN/bunkerd.pid")" 2>/dev/null || true
  sleep 1
fi

echo "===== start isolated daemon (/opt/bunker/bunkerd, HEAD build) ====="
set -m
/opt/bunker/bunkerd -c "$CONF" > "$RUN/bunkerd.log" 2>&1 &
DPID=$!
echo "$DPID" > "$RUN/bunkerd.pid"
echo "daemon pid=$DPID"

for i in $(seq 1 40); do
  if curl -sf -o /dev/null "http://127.0.0.1:28083/healthz" 2>/dev/null; then
    echo "daemon ready after ${i}s (/healthz 200)"
    break
  fi
  sleep 1
done

echo "---- daemon binary identity (running process) ----"
echo "exe: $(readlink -f /proc/$DPID/exe)"
sha256sum /proc/$DPID/exe | sed 's#/proc/[0-9]*/exe#<running bunkerd>#'
"$BUNKER" version | sed 's/^/deployed: /'
echo
echo "---- daemon log (head 30) ----"
head -30 "$RUN/bunkerd.log"
echo
echo "===== connect CLI (isolated BUNKER_HOME=$BUNKER_HOME) ====="
"$BUNKER" connect --name df77live --token df77live-local http://127.0.0.1:28083 2>&1 | head -12
echo "connect_rc=${PIPESTATUS[0]}"

echo
echo "===== spawn image-spec agent (spec: apt ripgrep) ====="
date -u
timeout 900 "$BUNKER" spawn --server df77live --agent-id df77live1 \
  --image-spec "$RUN/spec.json" --ttl 6h 2>&1 | tee "$RUN/spawn.out"
echo "spawn_rc=${PIPESTATUS[0]}"
date -u

echo
echo "===== agent info ====="
"$BUNKER" info df77live1 --server df77live 2>&1 | head -40
echo "===== agent list ====="
"$BUNKER" list --server df77live 2>&1 | head -20
echo "===== UP PHASE DONE ====="
