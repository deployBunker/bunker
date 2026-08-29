# Bunker — Real-Use Integration Report (2026-08-29)

Dogfood field test by coding-hermes-dogfood (cron). Verdict: **✅ SHIPPABLE**.
Third dogfood run (2026-08-03 🟡 → 2026-08-18 ✅ → 2026-08-29 ✅). This run
exercised the NEW surface shipped since 2026-08-18: the **audit trail**
(GAP-047 append-only JSONL + GAP-049 rotation/hash-chain + GAP-050
`bunker audit list/export` CLI), the named-agent spawn binding (DOGFOOD-008),
and the 300s spawn deadline (SPAWN-TIMEOUT-001). It ran against the live
fleet (a private multi-server deployment), not just the public demo.

## 1. Environment

- **Client:** kara's workstation (Linux, go 1.26.5), CLI binary built from
  HEAD: `./bunker version` → `bunker 0.1.3, commit 4af949d` (make build).
- **Server:** `bunker-las-03` (bunker-las-03:10002, REST port; auth-enforced
  Bearer token from `~/.bunker/config.yaml`; server binary v0.1.3). Also
  probed `bunker-mvp` (78.46.173.180:18080, v0.1.3) and `bunker-las-01..04`
  — all ONLINE, all 401 without a token.
- **Fleet status check:** `bunker status` (active server) + `bunker --server
  <name> status` per box — multi-server switching (MULTI-003) verified across
  6 servers in one sweep.
- **Scope:** two full agent lifecycles on `bunker-las-03` (scratch agents
  `dogfood-0829` and `pos-test-0829`), REST surface probes, audit-trail
  forensics of the run. Server left with **0 agents**; no leaked users/keys
  on the daemon host.

## 2. The verified workflow (copy-paste runnable — 2026-08-29 state)

```bash
make build                                   # HEAD binaries; ./bunker version → 0.1.3 / commit <HEAD>

# --- fleet awareness ---
bunker status                                # active server: version, uptime, agents, CPU/mem/disk
bunker --server bunker-mvp status            # multi-server: per-box status
bunker list

# --- lifecycle 1: named agent with explicit limits ---
bunker spawn --agent-id dogfood-0829 --ttl 2h --cpu 1.0 --memory 1073741824
                                             # ~21s → connection bundle (key, ports, TTL, sshfs/tunnel cmds)
bunker info dogfood-0829                     # status, expires, limits (CPU 1.0, mem 1.0 GB, disk 64 GB, 16 containers)
bunker exec dogfood-0829 -- whoami           # bunker-dogfood-0829 (isolated Linux user)
bunker exec dogfood-0829 -- docker info --format '{{.ServerVersion}} {{.SecurityOptions}}'
                                             # 29.7.2 rootless=[seccomp cgroupns rootless] — rootless docker works
bunker exec dogfood-0829 -- docker run --rm alpine sh -c 'echo $((6*7))'
                                             # 42 — real workload inside the agent's own daemon
bunker env set dogfood-0829 DOGFOOD_MARKER=hunter42    # KEY=VALUE as ONE arg
bunker env get dogfood-0829 DOGFOOD_MARKER             # hunter42
bunker exec dogfood-0829 -- sh -c 'echo "env: $DOGFOOD_MARKER"'   # env: hunter42 (sourced into exec)
echo "MARKER-$(date +%s)" > /tmp/src.txt
bunker cp /tmp/src.txt dogfood-0829:/tmp/cp-rt.txt     # byte round-trip
bunker run dogfood-0829 --detach --name df-job -- sh -c 'sleep 3; echo done > /tmp/j.log'
                                             # Run ID + transient systemd unit; survives disconnect
bunker heartbeat dogfood-0829                # TTL extended, new expiry echoed
bunker destroy dogfood-0829                  # 1.96s; `bunker list` → No agents found

# --- lifecycle 2: README quick-start form (positional name now WORKS) ---
bunker spawn --ttl 30m pos-test-0829         # agent named pos-test-0829 (DOGFOOD-008 fix, 49.8s)
bunker destroy pos-test-0829

# --- NEW: audit trail, queried remotely (GAP-050) ---
bunker audit list --server bunker-las-03 --agent dogfood-0829   # RPCs targeting that agent
bunker audit list --server bunker-las-03 --since 2026-08-29T00:00:00Z
bunker audit export --server bunker-las-03 --since 2026-08-29T16:00:00Z   # raw JSONL incl. hash chain
bunker audit verify --path /var/log/bunkerd/audit.log          # LOCAL-ONLY (daemon host), no --server

# --- REST (connect-go, same surface as gRPC) ---
curl -s -X POST http://bunker-las-03:10002/bunker.v1.Bunkerd/ListAgents \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{}'
# no token → 401; wrong service path (/bunkerd.v1.Bunkerd/...) → 404
```

## 3. What held up (promises kept, verified live)

- **Named spawn:** `--agent-id` and the positional form both create the agent
  you asked for. DOGFOOD-008 fix holds.
- **Rootless Docker:** 29.7.2 with rootless/ seccomp/ cgroupns security
  options; first `docker run` pulls alpine inside the agent's own daemon.
  Egress works (`wget example.com` OK).
- **Isolation:** `whoami` = `bunker-<id>`; limits enforced (info shows
  CPU/mem/disk/containers; heartbeat extends TTL; expiry honored).
- **env / exec compound / cp / run --detach:** all previously-broken features
  (DOGFOOD-001..006 era) remain fixed and verified.
- **Audit trail writes:** every authenticated RPC lands in the audit log —
  spawn/exec/env/cp/run/heartbeat/metrics/destroy/queryaudit all present with
  caller=master, outcome=ok, duration_ms real, and the SHA-256 chain intact
  (export shows hash → prev_hash links).
- **Auth surface:** 401 no-token, 404 wrong path, 200 with Bearer.
- **Cleanup:** destroy 1.96s, idempotent; server back to 0 agents; daemon-side
  users/keys cleaned (only client-side `~/.bunker/keys/` lingers — DOGFOOD-014).

## 4. Errors hit and what they mean

1. `bunker audit verify --server bunker-las-03` → `bunker: unknown flag:
   --server`. **verify is local-only** (checks the hash chain on the daemon
   host). The group help (`bunker audit --help`) says "--server is given"
   for subcommands generally — misleading. Filed as DOGFOOD-013.
2. `bunker metrics dogfood-0829` → "Memory Used: 2.4 GB / Memory Limit:
   1.0 GB" on a healthy 1.0 GB agent. Not a transient — 480 MB in-agent
   allocation changed nothing. Root cause: metrics reads the HOST cgroup,
   not the agent's (`internal/resource/cgroup.go`). Filed as DOGFOOD-011.
3. `bunker audit list --agent dogfood-0829` returned 7 records while the run
   produced ~20 audited RPCs targeting that agent — every ExecAgent record
   carries `agent_id:""` and `remote_addr:""` because ExecAgent is a
   server-streaming RPC and connect-go interceptors cannot see stream request
   messages. Filed as DOGFOOD-012. Workaround while unfixed: filter by
   `--since` + eyeball, or correlate by timestamp from
   `bunker audit export`.
4. Spawn of a fresh agent took 49.8s on the second run (rootless dockerd
   bring-up, package install) — the 300s client deadline (SPAWN-TIMEOUT-001)
   absorbed it comfortably; the old 30s deadline would have killed it. This
   is exactly the race the fix addressed.

## 5. Integration depth notes (for future consumers)

- **The audit CLI is the fastest way to learn what a box has been doing:**
  `bunker audit list --server <box> --since ...` before touching anything.
  Caveats: `--agent` filter incomplete for exec (DOGFOOD-012); `verify` is
  host-side only (DOGFOOD-013); `--limit` returns newest-N but prints
  oldest-first (documented pitfall in internal/cli/SKILL.md).
- **Metrics:** until DOGFOOD-011 lands, treat `bunker metrics` memory as
  host-level; the disk numbers and per-agent limit ARE agent-specific.
- **Multi-server:** `--server <name>` flag works on status/list/audit; spawn
  goes to the ACTIVE server from `~/.bunker/config.yaml`
  (`active_server:` key) — check `bunker status` first if you have a fleet.
- **Client key hygiene:** destroyed agents leave `~/.bunker/keys/<id>`
  behind (DOGFOOD-014) — delete manually until fixed.

## 6. What I'd tell the maintainer (1h budget)

1. Fix the metrics agent-scoping (DOGFOOD-011) — observability that lies is
   worse than none.
2. Fix the streaming-exec audit gap (DOGFOOD-012) — the audit trail's top
   reason to exist is "who exec'd what where", and that's the one column set
   that's empty.
3. Sweep `~/.bunker/keys/` cleanup (DOGFOOD-014) — 66 stale private keys.
4. 10 minutes: fix the audit group help wording (DOGFOOD-013).
