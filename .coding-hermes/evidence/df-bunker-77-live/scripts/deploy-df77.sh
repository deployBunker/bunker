#!/usr/bin/env bash
# DF-BUNKER-77-LIVE — deploy the HEAD build to bunker-mvp WITHOUT restarting
# the production demo daemon (a restart's registry reconcile would destroy the
# live CI-runner agents on this shared host; see evidence doc).
set -u
TS="$(date -u +%Y%m%d-%H%M%S)"
OUT=/root/df77-deploy
SRC=/root/df77-deploy          # scp'd candidate binaries
OLD_SHORT="$(/opt/bunker/bunkerd version 2>/dev/null | awk 'NR==2{print $2}')"
NEW_SHORT="$(/root/df77-deploy/bunkerd version 2>/dev/null | awk 'NR==2{print $2}')"

echo "########## DF-BUNKER-77-LIVE DEPLOY  ts=${TS}  old=${OLD_SHORT}  new=${NEW_SHORT} ##########"

echo
echo "===== BEFORE: host census ====="
echo "-- bunker-* users: $(getent passwd | grep -c '^bunker-')"
echo "-- docker running containers: $(docker ps -q | wc -l)"
echo "-- production daemon pid: $(systemctl show bunkerd -p MainPID --value)"
echo "-- production daemon exe: $(readlink /proc/$(systemctl show bunkerd -p MainPID --value)/exe)"
echo "-- production daemon started: $(ps -o lstart= -p $(systemctl show bunkerd -p MainPID --value))"
echo "-- registry lines: $(wc -l < /var/lib/bunkerd/agents.jsonl)"
echo "-- /home bunker-* dirs: $(ls /home | grep -c '^bunker-')"
echo "-- /opt/bunker HEAD: $(git -C /opt/bunker rev-parse HEAD)"
echo "-- /opt/bunker/bunkerd version: $(/opt/bunker/bunkerd version | awk 'NR==2{print $2}')"
echo "-- /usr/local/bin/bunkerd version: $(/usr/local/bin/bunkerd version | awk 'NR==2{print $2}')"
echo "-- tenants:"; /opt/bunker/bunker list 2>&1 | sed -n '/Agent ID/,$p' | head -12

echo
echo "===== STEP 1: back up the deployed binaries (repo convention: .bak-<sha>-<ts>) ====="
for d in /opt/bunker /usr/local/bin; do
  for b in bunker bunkerd; do
    if [ -f "$d/$b" ]; then
      if [ ! -f "$d/$b.bak-${OLD_SHORT}-${TS}" ]; then
        cp -p "$d/$b" "$d/$b.bak-${OLD_SHORT}-${TS}"
      fi
      echo "  backed up $d/$b -> $d/$b.bak-${OLD_SHORT}-${TS}  (sha256 $(sha256sum "$d/$b" | cut -c1-16))"
    fi
  done
done

echo
echo "===== STEP 2: install the HEAD build (both binaries, both locations) ====="
for d in /opt/bunker /usr/local/bin; do
  install -m 0755 "$SRC/bunker"  "$d/bunker"
  install -m 0755 "$SRC/bunkerd" "$d/bunkerd"
  echo "  installed $d/{bunker,bunkerd}  sha256 $(sha256sum "$d/bunkerd" | cut -c1-16)"
done

echo
echo "===== STEP 3: update the /opt/bunker checkout to the deployed commit ====="
git -C /opt/bunker fetch --quiet origin 2>&1 | head -5
git -C /opt/bunker merge --ff-only origin/main 2>&1 | head -5
echo "-- /opt/bunker HEAD now: $(git -C /opt/bunker rev-parse HEAD)"
echo "-- /opt/bunker branch:    $(git -C /opt/bunker rev-parse --abbrev-ref HEAD)"

echo
echo "===== STEP 4: bin-report certification (AGENTS.md live gate) ====="
cd /opt/bunker || exit 1
bash e2e-full-battery.sh --bin-report
echo "bin_report_rc=$?"

echo
echo "===== AFTER: production daemon UNTOUCHED (no restart performed) ====="
echo "-- production daemon pid: $(systemctl show bunkerd -p MainPID --value)"
echo "-- production daemon status: $(systemctl is-active bunkerd)"
echo "-- production daemon running-binary sha256: $(sha256sum /proc/$(systemctl show bunkerd -p MainPID --value)/exe 2>/dev/null | cut -c1-32)"
echo "-- on-disk /opt/bunker/bunkerd sha256:      $(sha256sum /opt/bunker/bunkerd | cut -c1-32)"
echo "-- bunker-* users: $(getent passwd | grep -c '^bunker-')"
echo "-- docker running containers: $(docker ps -q | wc -l)"
echo "########## DEPLOY DONE ##########"
