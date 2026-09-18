#!/usr/bin/env bash
# GAP-079-LIVE — token-leak scan + scratch-leftover provenance, run on the host
# over the EXACT transcript files that will be committed.
set -u
echo "LEAKSCAN_START_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "scan_root=/root/gap079-live"
echo
echo "########## files under the scratch dir ##########"
find /root/gap079-live -type f | sort | sed 's/^/  /'
echo
echo "########## sha256 of the transcript copies ##########"
sha256sum /root/gap079-live/scan/*.log 2>/dev/null | sed 's#/root/gap079-live/scan/##'
echo
echo "########## token-bytes scan (the real token is read here, never printed) ##########"
python3 - <<'PY'
import os
import re

tok = None
for line in open('/etc/bunkerd/config.yaml'):
    m = re.match(r'\s*token:\s*(.+?)\s*$', line)
    if m:
        tok = m.group(1).strip().strip('"').strip("'")
        break
if not tok:
    raise SystemExit('no token found')

needle = tok.encode()
hits = []
scanned = 0
for root, _dirs, files in os.walk('/root/gap079-live'):
    for name in files:
        path = os.path.join(root, name)
        try:
            data = open(path, 'rb').read()
        except OSError:
            continue
        scanned += 1
        if needle in data:
            hits.append(path)
print('token_len=%d scanned_files=%d token_hits=%d' % (len(tok), scanned, len(hits)))
for h in hits:
    print('  TOKEN LEAK: %s' % h)
# also scan the repo copy of the transcripts if it happens to be present
for extra in ('/root/gap079-live/scan',):
    for name in sorted(os.listdir(extra)) if os.path.isdir(extra) else []:
        p = os.path.join(extra, name)
        if needle in open(p, 'rb').read():
            print('  TOKEN LEAK: %s' % p)
print('leakscan_clean=%s' % ('yes' if not hits else 'NO'))
PY
echo
echo "########## /tmp/bunkerd-battery-* provenance (nested-suite temp configs) ##########"
echo "count=$(ls /tmp/bunkerd-battery-*.yaml 2>/dev/null | wc -l)"
echo "newest_5:"
ls -lt /tmp/bunkerd-battery-*.yaml 2>/dev/null | head -5 | sed 's/^/  /'
echo "created_in_this_window_2151_2156=$(find /tmp/bunkerd-battery-*.yaml -newermt '2026-09-18 21:46' 2>/dev/null | wc -l)"
echo
echo "########## /tmp/bunker-tunnel-e2e-main.log ##########"
ls -la /tmp/bunker-tunnel-e2e-main.log 2>/dev/null
echo "LEAKSCAN_END_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
