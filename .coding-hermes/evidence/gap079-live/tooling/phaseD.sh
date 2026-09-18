#!/usr/bin/env bash
# GAP-079-LIVE — Phase D: prove the battery's GAP-079 reap check is not vacuous.
# The function is extracted BYTE-IDENTICALLY from the deployed battery script and
# run with stub assert()/fail() (no host mutation, no battery section).
set -u
echo "PHASE_D_START_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo
echo "########## EXTRACT session_tunnel_leak_check FROM THE DEPLOYED BATTERY ##########"
python3 - <<'PY'
import hashlib
src = open('/opt/bunker/e2e-full-battery.sh').read()
start = src.index('session_tunnel_leak_check() {')
end = src.index('\n}\n', start) + 3
body = src[start:end]
open('/root/gap079-live/extracted_function.sh', 'w').write(body)
print('extracted_bytes=%d extracted_sha256=%s' % (len(body), hashlib.sha256(body.encode()).hexdigest()))
hdr = ('#!/usr/bin/env bash\nPASS=0; FAIL=0\n'
       'assert(){ echo "  OK: $1"; PASS=$((PASS+1)); }\n'
       'fail(){ echo "  FAIL: $1"; FAIL=$((FAIL+1)); }\n')
open('/root/gap079-live/bin/reap_check.sh', 'w').write(
    hdr + body + '\nsession_tunnel_leak_check\nrc=$?\n'
    'echo "reap_check_rc=$rc pass=$PASS fail=$FAIL"\n')
PY
echo "battery_source_sha256=$(sha256sum /opt/bunker/e2e-full-battery.sh | awk '{print $1}')"
echo "extracted_sha256=$(sha256sum /root/gap079-live/extracted_function.sh | awk '{print $1}')"
echo
echo "########## 1) BASELINE, NO DECOY (expect 0) ##########"
bash /root/gap079-live/bin/reap_check.sh
echo
echo "########## 2) PLANT THE DECOY (orphan-shaped -L .../docker.sock) ##########"
python3 /root/gap079-live/bin/decoy.py start
sleep 1
python3 /root/gap079-live/bin/decoy.py status
echo "--- decoy rows in the process table ---"
ps -ww -eo pid=,ppid=,stat=,args= | grep -F "decoy-live" | grep -v "grep" || echo "(none)"
echo
echo "########## 3) REAP CHECK WITH THE DECOY PLANTED (expect FAIL > 0) ##########"
bash /root/gap079-live/bin/reap_check.sh
echo
echo "########## 4) KILL THE DECOY (explicit pid, no pkill -f) ##########"
python3 /root/gap079-live/bin/decoy.py stop
echo
echo "########## 5) REAP CHECK AFTER CLEANUP (expect 0) ##########"
bash /root/gap079-live/bin/reap_check.sh
echo "PHASE_D_END_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
