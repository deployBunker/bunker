# QA-BUNKER-10 — GAP-073 audit ship/seal live-server battery pass (bunker-mvp)

Executed 2026-10-15 by bunker foreman tick bunker-foreman-2026-10-15-15-47-19.
Host: bunker-mvp (78.46.173.180), daemon bunkerd systemd service, config
/etc/bunkerd/config.yaml, audit log /var/log/bunkerd/audit.log.

## Setup

Binaries built from repo HEAD 14f0ca5c (`go build ./cmd/bunker ./cmd/bunkerd`),
pushed to bunker-mvp, daemon restarted (`systemctl is-active bunkerd` → active,
`bunker --version` reports commit 14f0ca5). Ship endpoint: local python3 HTTP
catcher on 127.0.0.1:19099 (loopback-only, not reachable off-host).

Daemon config applied for the run (backed up first at
/etc/bunkerd/config.yaml.bak-qa10, restored after the run):

```yaml
audit:
  enabled: true
  ship_to: "http://localhost:19099/audit-segments"
  seal_key: "<jwt_secret value from /etc/bunkerd/jwt_secret>"
```

## 1. Natural rotation → remote ship (live)

The live log was 4.4 MB; ~700 authenticated RPCs (`bunker list` / `bunker
status` loops) drove it over the 5 MiB MaxSize threshold. At 15:58:56 UTC the
daemon rotated and shipped — no code change, no force flag, the production
path:

- endpoint received: `2026-10-15T15:58:56Z
  head=40e9636c02e2a463f18a846ade3594d068d17bcdbea20cf579d24c75eae634d9
  len=5243185 path=/audit-segments` (POST body = the full rotated segment,
  5,243,185 bytes, header X-Bunker-Chain-Head set)
- shipstate file written: `/var/log/bunkerd/audit.log.shipstate` →
  `{"ship_to":"http://localhost:19099/audit-segments","last_attempt":
  "2026-10-15T15:58:56Z","last_result":"ok","last_success":
  "2026-10-15T15:58:56Z","queue_depth":0}`
- `bunker audit status` after rotation:
  - `shipping: enabled (http://localhost:19099/audit-segments)`
  - `last ship result: ok`, `last ship success: 2026-10-15T15:58:56Z`
  - `ship retry queue: 0 segment(s)`
  - `off-host anchor: ... last shipped 2026-10-15T15:58:56Z`

## 2. Rotation seal (live)

The fresh log's first record is a chained SEAL record:
`{"caller":"bunkerd","method":"/internal/audit/seal","summary":
"[INSECURE-PLAINTEXT] rotation seal","prev_hash":"40e9636c...","seal":
"bc798d6d004ed57a81c3b347aa29c7bafcfc6de667b5737b525b374fdf0567c3"}`.

Independently recomputed on the host:
`HMAC-SHA256(key=<configured audit.seal_key>, msg=<chain head 40e9636c...>)`
== `bc798d6d004e...67c3` — matches. The shipped copy + seal key prove a
truncated or replaced local log (GAP-073's contract).

## 3. e2e-full-battery.sh with the knobs ENABLED (live, production daemon)

`sudo BUNKER_TOKEN=<token> bash /opt/bunker/e2e-full-battery.sh`
(transcript: /tmp/qa10-battery.log on bunker-mvp). Result:
**116 pass / 2 fail / 7 notes**. The shipper/seal code path ran under every
spawn/destroy/exec cell (every RPC is an audited record; rotation fired
mid-battery from section-4+ volume). The 2 failures are NOT shipper
interference:

1. Section 3 "list (empty)" — the standalone battery expects an empty agent
   list, but host bunker-mvp carries a pre-existing live agent (crier-lab,
   created 2026-10-14). Pre-existing condition, unrelated to GAP-073.
2. Section 12 nested regression suite — first run failed 13 spawn cells from
   port-pool exhaustion right after its own teardown (the known INT-CI-020
   boot-slow pattern); a clean re-run of regression-tests.sh passed
   33/34 with one spawn-output race ("Creating agent..." captured before the
   final line). Neither failure touches the audit path.

## 4. Fault/edge notes observed live

- The pre-existing audit.log.3 has a genuinely broken record 1 (old tamper on
  this host, predates this run) → chain status BROKEN, seal status reported
  false via absence-of-seal-in-retained-window, and "audit chain-head
  recovery skipped" warns at restart. Honest reporting path verified too.
- A dead endpoint path is unit-covered (ship_test.go, MaxShipAttempts ~15s
  before drop); this run's endpoint was alive and the success path is what
  the deferred leg asked to prove.

## 5. Teardown

Config restored from backup, daemon restarted healthy, ship endpoint and
loopback forward killed, `/root/audit-ship-evidence-segment.ndjson` retained
on bunker-mvp as a copy of the shipped segment.
