# Dogfood Integration — 2026-09-19 — Durability & Trust Surface (bunker-releng lane)

**Angle:** runs 1-12 covered the CLI lifecycle, the REST protocol, and fresh-machine
install. This run took the README's *trust promises* — the claims a cautious user
actually buys: TTL auto-destroy, restart survival (registry replay +
reconciliation), stop/start, agent-scoped sub-keys, and the audit hash chain —
and exercised each one for real against a scratch daemon built at HEAD.

**Method:** scratch `bunkerd` at HEAD `e344088` on a dedicated port pair
(REST 127.0.0.1:18093 / gRPC 19092), private config/registry/audit paths under
`/tmp/df0919/`, its own CLI home (`BUNKER_HOME=/tmp/df0919/clihome`), master
token + `jwt_secret` set so the sub-key surface is live, reconciliation
`adopt` first, port pool 20000-20999. The fleet daemon on this host
(`/opt/bunker/bunkerd`, different ports/pool) was untouched. Every scratch
agent was destroyed and every synthetic user removed; host verified clean.

## Promise statement

"A user can trust that agents die when their TTL expires, survive a daemon
crash via the durable registry, can be stopped and started, expose only a
scoped sub-key to third parties, and leave an audit trail whose hash chain
proves tampering."

## What was done, in order (real use, not test scripts)

1. **Spawn** `dfdf-a` (`--ttl 90m`) — 15s, progress line present (GAP-023 fix
   live), bundle lists Docker SSH, key path, port range 20000-20099, expiry,
   **API Key** (the sub-key — minted because `jwt_secret` is set),
   SSHFS and tunnel command lines.
2. **Exec** `whoami` → `bunker-dfdf-a`. `/tmp` probe: file created inside the
   agent is visible in the agent (host-shared /tmp disclosed loudly by
   `bunker status` on an unprovisioned host — the honest-degradation path
   works as documented).
3. **Sub-key scoping matrix** (raw curl against the connect-RPC endpoints):
   - Sub-key → `Bunkerd/ListAgents`: **401** `agent-scoped tokens are not
     allowed for this endpoint` ✅
   - Sub-key → `Agent/GetInfo` with a FOREIGN agent id: returns the sub-key's
     OWN agent record (clamped) ✅
   - Sub-key → `Agent/Metrics` with a foreign id: **returns the foreign
     agent's data** ❌ (DF-BUNKER-28)
   - Sub-key → `Agent/Metrics` with a never-spawned id: **`status:running` +
     host memory**, no error ❌ (same finding)
   - Sub-key → `Agent/Heartbeat` foreign id: **404 not_found** ✅ (the
     correct behavior Metrics lacks)
   - Master token → `Agent/Metrics` nonexistent id: same fabricated
     `running` record ❌
4. **TTL auto-destroy**: `dfdf-b` spawned `--ttl 2m`, expiry 23:19:23.
   Reaper ticked 23:20:03 → `TTL expired, destroying agent` → user removed,
   registry `destroy` event appended, `list` drops it. **Promise holds.**
5. **Crash durability**: spawned `dfdf-c` (ports 20100-20199), `kill -9` on
   the daemon, restart. Replay log: `replayed_live:2 … restored:2` — both
   agents back, **dfdf-c's exact port reservation preserved**, exec works
   immediately (`ALIVE-A`). Zero orphans, zero purges. **Promise holds.**
6. **Stop/start**: `stop dfdf-c` → status `stopped`; `start` → exec works
   (`BACK-ONLINE-C`). **Promise holds.**
7. **Audit chain**: `audit list` renders 32 records with caller attribution
   (`master` / `agent:<id> key:<fp>`) including the requested agent id of
   sub-key calls. `audit verify` → **"tamper detected at record 4"** — on a
   log nobody tampered with. Record 4 is the first record of the restarted
   daemon and carries an empty `prev_hash`: the chain head lives only in
   process memory, so **every restart breaks the chain** (DF-BUNKER-29).
8. **Reconciliation / adopt**: synthetic orphan `bunker-dforph-a1`
   (bare `useradd`, no `.bunker/ports`) + daemon restart in `mode: adopt`
   → `adopt failed … no readable port metadata` → orphan destroyed, WARN
   logged, home removed. Correct fail-closed behavior, but the
   config.example.yaml comment does not mention the metadata precondition
   (DF-BUNKER-30).
9. **Mount** (`bunker mount dfdf-a <mnt>`): **works at HEAD** — agent home
   visible live, clean `fusermount -u`. First recorded mount success; the
   09-16 run's 2/2 connection-reset failures appear fixed.
10. **Docker-in-agent**: `docker run --rm alpine:latest echo …` inside
    `dfdf-a` → pull + `DOCKER-IN-AGENT-PASS`. Rootless path healthy.
11. **Cleanup**: `destroy dfdf-a/dfdf-c` (keys removed, users gone), scratch
    daemon stopped, port pair released, host verified (no `bunker-dfdf-*` /
    `bunker-dforph-*` users or homes left; `:18093` refuses connections).

## Verdict context

Core durability promises — TTL reap, crash replay, exact port restore,
stop/start — all held at HEAD. Two trust defects surfaced: the Agent-service
Metrics RPC is effectively unscoped (DF-BUNKER-28, P1) and the audit chain
self-breaks on every restart, making `audit verify` cry wolf (DF-BUNKER-29,
P1). Both are filed on the board; neither blocks the CLI lifecycle promises
prior runs verified.

## Frictions (run total: 6)

1. Metrics scoping hole (DF-BUNKER-28) — security, P1.
2. Audit chain restart break → `audit verify` false positive (DF-BUNKER-29) — P1.
3. Adopt precondition undocumented (DF-BUNKER-30) — docs, P2.
4. Verb grammar inconsistency: `exec` rejects global flags before the
   agent-id (`exec takes no flags before <agent-id>` — the DF-BUNKER-8
   peeler), while `mount` accepted `--config` after its args. One consistent
   peeler would remove the guesswork (DF-BUNKER-31) — P2.
5. Non-root daemon: spawn proceeds until `useradd` fails (`exit 1`), which
   is the correct failure but the README's "Run it locally" section never
   says the daemon half needs root (it does say "as root" for the daemon
   start — easy to skim past). Diagnostics note, no row.
6. `pgrep -f 'df0919/bunkerd'` self-matched the checking shell twice during
   cleanup (tooling lesson recorded in diagnostics §13, not a bunker bug).

## Working example (reproducible)

```bash
# scratch daemon on private ports (config: see docs/dogfood/diagnostics.md §13)
sudo /path/bunkerd --config /tmp/scratch/config.yaml &
bunker connect http://127.0.0.1:18093 --token <master-token>
BUNKER_HOME=$HOME/.bunker-scratch bunker spawn --ttl 90m demo-a
BUNKER_HOME=$HOME/.bunker-scratch bunker exec demo-a -- sh -c 'echo hi'
# kill -9 the daemon, restart it: agents return with exact ports (verified)
BUNKER_HOME=$HOME/.bunker-scratch bunker audit verify --daemon-config /tmp/scratch/config.yaml
```

Placeholder token shown; never commit real tokens (per skill rules this run's
scratch token was single-purpose and the config never left /tmp).
