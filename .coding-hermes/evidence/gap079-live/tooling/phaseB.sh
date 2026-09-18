#!/usr/bin/env bash
# GAP-079-LIVE — Phase B: deploy HEAD (6a6ad20) to bunker-mvp.
# The tar: back up the installed binaries, reset /opt/bunker to origin/main,
# rebuild, install both binaries, restart the systemd unit, certify.
set -u
cd /opt/bunker || exit 1
export PATH=/usr/local/go/bin:$PATH
echo "PHASE_B_START_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo
echo "########## BACK UP THE INSTALLED BINARIES ##########"
cp -a /opt/bunker/bunker  /root/gap079-live/backup/bunker.pre-gap079live
cp -a /opt/bunker/bunkerd /root/gap079-live/backup/bunkerd.pre-gap079live
sha256sum /root/gap079-live/backup/bunker.pre-gap079live /root/gap079-live/backup/bunkerd.pre-gap079live
/root/gap079-live/backup/bunker.pre-gap079live version | head -3
echo
echo "########## CHECKOUT STATE BEFORE ##########"
git status --porcelain | head -10
echo "head_before=$(git rev-parse HEAD)"
echo
echo "########## FETCH + RESET TO origin/main ##########"
git fetch origin -q
git reset --hard origin/main -q
git log --oneline -1
echo "head_after=$(git rev-parse HEAD)"
echo
echo "########## BUILD ##########"
go version
make build 2>&1 | tail -4
echo "build_rc=$?"
echo
echo "########## INSTALL (both binaries, /usr/local/bin) ##########"
make install 2>&1 | tail -4
echo "install_rc=$?"
echo
echo "########## RESTART THE DAEMON ##########"
systemctl restart bunkerd
sleep 4
echo "is_active=$(systemctl is-active bunkerd)"
systemctl show bunkerd -p MainPID -p NRestarts -p ExecMainStartTimestamp
echo "exe=$(readlink -f /proc/"$(systemctl show bunkerd -p MainPID --value)"/exe)"
echo "healthz=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/healthz)"
echo "serverinfo_unauth=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:18080/bunker.v1.Bunkerd/ServerInfo)"
echo
echo "########## DEPLOYED IDENTITY (all four paths must read 6a6ad20) ##########"
for b in /opt/bunker/bunkerd /opt/bunker/bunker /usr/local/bin/bunkerd /usr/local/bin/bunker; do
    echo "--- $b"
    "$b" --version 2>/dev/null | head -3
    sha256sum "$b"
done
echo
echo "########## BINARY CERTIFICATION (--bin-report) ##########"
bash e2e-full-battery.sh --bin-report
echo "bin_report_rc=$?"
echo
echo "########## RESOLVED RUN PLAN (--show-plan) ##########"
bash e2e-full-battery.sh --show-plan
echo "show_plan_rc=$?"
echo "PHASE_B_END_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
