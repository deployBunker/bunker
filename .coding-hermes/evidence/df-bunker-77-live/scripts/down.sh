#!/usr/bin/env bash
# DF-BUNKER-77-LIVE step 7: destroy the scratch agent, stop the isolated
# daemon, and prove the host carries no residue from THIS run.
set -u
BASE=/root/df77-deploy
RUN=$BASE/live
BUNKER=/opt/bunker/bunker
export BUNKER_HOME="$RUN/cli-home"
export HOME="$BUNKER_HOME"
AGENT="${1:-df77live1}"
SRV=df77live

echo "########## DF-BUNKER-77-LIVE TEARDOWN  $(date -u) ##########"
echo "-- bunker-* users BEFORE destroy: $(getent passwd | grep -c '^bunker-')"
echo "-- docker containers BEFORE: $(docker ps -q | wc -l)"
echo
echo "===== destroy scratch agent ====="
timeout 600 "$BUNKER" destroy "$AGENT" --server "$SRV" --force 2>&1 | tail -20
echo "destroy_rc=${PIPESTATUS[0]}"
echo
echo "===== stop the isolated daemon ====="
if [ -f "$RUN/bunkerd.pid" ]; then
  DPID="$(cat "$RUN/bunkerd.pid")"
  kill "$DPID" 2>/dev/null || true
  sleep 3
  if kill -0 "$DPID" 2>/dev/null; then kill -9 "$DPID" 2>/dev/null || true; sleep 1; fi
  echo "stopped pid $DPID (running=$(kill -0 "$DPID" 2>/dev/null && echo yes || echo no))"
fi
echo
echo "===== post-teardown host state ====="
echo "-- bunker-* users AFTER: $(getent passwd | grep -c '^bunker-')"
echo "-- bunker-df77live1 user present? $(getent passwd bunker-df77live1 >/dev/null 2>&1 && echo YES || echo no)"
echo "-- docker containers AFTER: $(docker ps -q | wc -l)"
echo "-- any container referencing df77live:"
docker ps -a --format '{{.Names}} {{.Image}}' 2>/dev/null | grep -i df77 | head -5 || echo "   (none)"
echo "-- my isolated daemon listening ports (must be empty):"
ss -ltn | grep -E ':28083|:29093' || echo "   (none)"
echo "-- agent home dir: $(ls -d /home/bunker-df77live1 2>/dev/null || echo 'removed/absent')"
echo
echo "===== PRODUCTION DAEMON — health check (must be untouched) ====="
echo "-- pid: $(systemctl show bunkerd -p MainPID --value)  status: $(systemctl is-active bunkerd)"
echo "-- running binary sha256: $(sha256sum /proc/$(systemctl show bunkerd -p MainPID --value)/exe 2>/dev/null | cut -c1-32)"
/opt/bunker/bunker status 2>&1 | head -18
echo "########## TEARDOWN DONE ##########"
