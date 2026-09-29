# QA-BUNKER-34 — bunker-las-03 recovered (live evidence, 2026-09-29)

## Row claim (2026-09-27 12Z)

bunker-las-03 down ~1d: SSH connect timeout, ICMP 100% loss, REST HTTP=000, tailscale
"offline, last seen 1d". Designated bunker-qa battery server unusable; QA/dogfood lanes
skipped install legs.

## Re-probe from the control host (2026-09-29 ~07:40 local, tick bunker-2026-09-29-07-30-15)

| Probe | Result |
|---|---|
| `ssh -o ConnectTimeout=6 bunker3 'hostname'` | UP, `bunker-las-03` |
| `ping -c1 100.69.3.13` | 1 received, rtt 137.9 ms |
| `curl http://100.69.3.13:10001/health` | HTTP 404 (daemon answering on :10001 — route answer, not connection failure; original outage probe read HTTP=000) |
| on-host: `systemctl is-active bunkerd` | active (pid 2889, `/opt/bunker/bunkerd --config /etc/bunkerd/config.yaml`) |
| on-host: `ss -tlnp` | LISTEN `*:10001` |
| on-host: `docker info` | docker-ok 26.1.5+dfsg1 |
| on-host: `uptime` | up 1 day 10h, load 0.78 — host rebooted ~2026-09-27 evening, consistent with the row's "one event ~Sep 26 took both las hosts" |

## Conclusion

All three probe classes named in the row (SSH / ICMP / REST) now answer; the battery
server is usable again. `uptime` places recovery at the host's own reboot ~1d10h ago —
the outage self-resolved host-side; no fleet action was taken or needed. Residual note:
the running bunkerd build version was not re-verified against HEAD this tick; deploy-freshness
for las hosts is tracked by RELEASE-003 / RELEASE-008 / DF-BUNKER-10, not here.

No code change accompanies this closure (verified-and-closed pattern; zero diff).
