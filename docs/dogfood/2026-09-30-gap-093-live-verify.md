# GAP-093 LIVE HALF — fail-closed session-scoped target binding verified on the live bunker-mvp demo (2026-09-30)

**VERIFY-GAP093-LIVE-PASS**

The CODE half of GAP-093 is `41d27c8` (41d27c8 2026-09-20 feat(binding): fail-closed target binding for every mutating command (GAP-093)), already an ancestor of `main`. This
document is the **LIVE half**: the same S2 contract re-proved against real `bunkerd` daemons on
`bunker-mvp`, by the tick that owns GitReins task `GAP-093-LIVE`.

* evidence assembled `2026-09-30T02:44:17Z` (UTC); repo HEAD `2a23f88ad2060a0122c49356dc5077367a0e16b2` (origin/main `2a23f88ad2060a0122c49356dc5077367a0e16b2`)
* CLI under test: **built from HEAD** (`make build` → `./bunker`, `bunker 0.1.4`, commit
  `2a23f88`) — every command below ran that binary, never a deployed one
* live demo daemon: `bunker-mvp` `78.46.173.180`, REST `:18080`, gRPC `:19090`, auth enforced
  (deployed build `45d7d44`, 66 commits behind HEAD: the CLI carries the binding logic, the
  daemon is only the peer it talks to)
* raw logs (control host `karaHermes`, `/tmp/gap093/`):
  * `testA` sha256 `5166e65fc1a3867979c270f06cfa1c3982afd2469db44eb3a9c0c59d86391e6a` (13535 bytes)
  * `testB` sha256 `0adfc0f65aed524f4b38236beca7faa836c42b58dc4bfdd6e22a3620442b6041` (37123 bytes)
  * `testB.stdout` sha256 `1e1ee0f27805ef94d0853b276f70fae10e525d700185433bf43dfc155b282313` (958 bytes)
  * `testC` sha256 `101e869c4ba1eec4c96310658a1e7e05f3361eb3064e29316d70e1df8dc0db34` (10523 bytes)
  * `gate` sha256 `964c5bb3af76dca893138cb60dd357cfea87ffece07861ece03baf4d20d6aabc` (2361 bytes)
  * `bindingtests` sha256 `1f01163d2d49cedcbc51ad2c21150e15c71092d92865da5acd08aa5e96440fb5` (711 bytes)

## The contract (`internal/cli/binding.go`)

```
--server flag  >  BUNKER_SESSION_TARGET env  >  REFUSE   (mutating)
--server flag  >  BUNKER_SESSION_TARGET env  >  shared active_server, ANNOUNCED   (read-only)
```

The shared `active_server` — one global written by `bunker use` into one config file — is
never consulted by a mutating command; that global is exactly how one session used to
re-target another. The refusal is a typed error (`ErrNoTarget`), exits non-zero, and names both
remedies.

| # | GAP-093-LIVE criterion | evidence | result |
|---|------------------------|----------|--------|
| 1 | no binding → every mutating verb refuses with the named message + non-zero exit | Test A: 21 verbs, each exit 1, zero connections to the live spy listener | **PASS** |
| 2 | two sessions with different explicit targets touch different servers, zero cross-writes | Test B: 4/4 visibility cells, raw-API + per-daemon audit trail | **PASS** |
| 3 | read-only output names its resolved target | Test C: 8/8 | **PASS** |
| 4 | full suite + guard green; evidence doc committed | `go build`/`go vet`/`go test -short` all rc=0; this commit | **PASS** |

## Test A — the named refusal, with the shared `active_server` pointed at a LIVE spy listener

Method: a throwaway CLI config (`--config`; the operator's `~/.bunker/config.yaml` is never
touched) whose `active_server` is a server named `spy` → `http://127.0.0.1:18999`, where a
Python listener logs **every accepted connection**. That is the strong form of "does NOT touch
the default server": a fallback would have produced both a `CONNECT` line and a non-refusal
error. Every case runs with `BUNKER_SESSION_TARGET` explicitly unset and no `--server`.

Controls at the end of the log prove the spy observes connections at all: with an explicit
binding (`--server spy` and `BUNKER_SESSION_TARGET=spy`) the same `destroy` command connects to
the spy (2 and 2 connections) and fails with the 404 from the fake daemon instead of refusing.

Result: **26 PASS / 0 FAIL** (21 mutating refusal cases + 3 explicit-binding
controls + 2 read-only naming cells).

| kind | case | exit | verdict |
|------|------|------|---------|
| CASE | spawn | 1 | PASS |
| CASE | destroy | 1 | PASS |
| CASE | exec | 1 | PASS |
| CASE | cp | 1 | PASS |
| CASE | deploy | 1 | PASS |
| CASE | tunnel | 1 | PASS |
| CASE | mount | 1 | PASS |
| CASE | start | 1 | PASS |
| CASE | stop | 1 | PASS |
| CASE | restart | 1 | PASS |
| CASE | renew | 1 | PASS |
| CASE | ssh | 1 | PASS |
| CASE | run | 1 | PASS |
| CASE | surface install | 1 | PASS |
| CASE | surface remove | 1 | PASS |
| CASE | heartbeat | 1 | PASS |
| CASE | key rotate | 1 | PASS |
| CASE | env set (BUNKER_HOME) | 1 | PASS |
| CASE | env unset (BUNKER_HOME) | 1 | PASS |
| CASE | destroy (no active_server) | 1 | PASS |
| CASE | exec (no active_server) | 1 | PASS |
| CONTROL | --server spy | 1 | PASS |
| CONTROL | BUNKER_SESSION_TARGET=spy | 1 | PASS |
| CONTROL | env set --server spy (BUNKER_HOME) | 1 | PASS |

Full raw transcript:

```
### GAP-093 Test A — named refusal, no fallback to the shared active_server
binary:   bunker 0.1.4  (/home/kara/bunker/bunker)
built:    2a23f88
config A: /tmp/gap093/config.yaml  (active_server: spy -> http://127.0.0.1:18999, LIVE listener that logs every connect)
config B: /tmp/gap093/config-no-active.yaml  (same server, NO active_server key at all)
config C: $BUNKER_HOME=/tmp/gap093/home/config.yaml  (for the flag-parsing-disabled 'env' verb)
env BUNKER_SESSION_TARGET: '<unset>' (explicitly unset for every case)

spy connections at start: 0

## Part 1 — mutating verbs, no --server, no BUNKER_SESSION_TARGET, shared default = live spy
## (active_server on the spy is the exact hazard: a fallback WOULD be observable here)

── CASE: spawn
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml spawn gap093-absent --ttl 1h
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: destroy
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml destroy gap093-absent
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: exec
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml exec gap093-absent -- echo hi
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: cp
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml cp /etc/hostname gap093-absent:/tmp/x
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: deploy
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml deploy /tmp/gap093 gap093-absent:/tmp
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: tunnel
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml tunnel gap093-absent
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: mount
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml mount gap093-absent /tmp/gap093-mnt
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: start
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml start gap093-absent
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: stop
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml stop gap093-absent
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: restart
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml restart gap093-absent
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: renew
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml renew --agent-id gap093-absent
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: ssh
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml ssh gap093-absent
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: run
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml run gap093-absent -- echo hi
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: surface install
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml surface install gap093-absent
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: surface remove
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml surface remove gap093-absent
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: heartbeat
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml heartbeat gap093-absent
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: key rotate
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config.yaml key rotate
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: env set (BUNKER_HOME)
   cmd:  env BUNKER_HOME=/tmp/gap093/home /home/kara/bunker/bunker env set gap093-absent FOO=bar
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: env unset (BUNKER_HOME)
   cmd:  env BUNKER_HOME=/tmp/gap093/home /home/kara/bunker/bunker env unset gap093-absent FOO
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

## Part 2 — same verbs with NO active_server key in the config at all
## (proves the refusal is not an artefact of a missing/blank shared default)

── CASE: destroy (no active_server)
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config-no-active.yaml destroy gap093-absent
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

── CASE: exec (no active_server)
   cmd:  /home/kara/bunker/bunker --config /tmp/gap093/config-no-active.yaml exec gap093-absent -- echo hi
   exit: 1
   output:
     | bunker: no target bound: pass --server/--agent or set BUNKER_SESSION_TARGET (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)
   spy connections during this case: 0
   VERDICT: PASS (named refusal + non-zero exit + zero spy connections)

## Part 3 — CONTROLS: an explicit binding MUST reach the spy (the spy observes connections)

── CONTROL: --server spy
   cmd:  env -u BUNKER_SESSION_TARGET /home/kara/bunker/bunker --config /tmp/gap093/config.yaml --server spy destroy gap093-absent
   exit: 1
   output:
     | Destroying agent gap093-absent (home size unknown; archiving before delete, deadline 12m0s)…
     | bunker: destroy agent: unimplemented: 404 Not Found
   spy connections during this control: 2
   VERDICT: PASS (binding honored — spy contacted, no refusal; the spy DOES observe connections)

── CONTROL: BUNKER_SESSION_TARGET=spy
   cmd:  env BUNKER_SESSION_TARGET=spy /home/kara/bunker/bunker --config /tmp/gap093/config.yaml destroy gap093-absent
   exit: 1
   output:
     | Destroying agent gap093-absent (home size unknown; archiving before delete, deadline 12m0s)…
     | bunker: destroy agent: unimplemented: 404 Not Found
   spy connections during this control: 2
   VERDICT: PASS (binding honored — spy contacted, no refusal; the spy DOES observe connections)

── CONTROL: env set --server spy (BUNKER_HOME)
   cmd:  env BUNKER_HOME=/tmp/gap093/home /home/kara/bunker/bunker env set --server spy gap093-absent FOO=bar
   exit: 1
   output:
     | bunker: stream error: unimplemented: HTTP status 404 Not Found
   spy connections during this control: 1
   VERDICT: PASS (binding honored — spy contacted, no refusal; the spy DOES observe connections)

## Part 4 — read-only verbs: convenience default KEPT, target PRINTED (Test C, same config)

── READ-ONLY: status
   exit: 0
     | bunker: reading server "spy"
     | ── spy ──
     |   URL:      http://127.0.0.1:18999
     |   Status:   OFFLINE
     |   Error:    server info: unimplemented: 404 Not Found
   VERDICT: PASS (announced 'bunker: reading server "spy"')

── READ-ONLY: list
   exit: 1
     | bunker: reading server "spy"
     | bunker: list agents: unimplemented: 404 Not Found
   VERDICT: PASS (announced 'bunker: reading server "spy"')

## Totals
   spy connections: start=0 end=5 (the 3 controls account for all of them)
   PASS=26 FAIL=0

## spy.log (raw)
  CONNECT 2026-09-29T21:08:59 peer=127.0.0.1:53200
    REQUEST-LINE b'POST /bunker.v1.Bunkerd/AgentMetrics HTTP/1.1'
  CONNECT 2026-09-29T21:08:59 peer=127.0.0.1:53214
    REQUEST-LINE b'POST /bunker.v1.Bunkerd/DestroyAgent HTTP/1.1'
  CONNECT 2026-09-29T21:08:59 peer=127.0.0.1:53222
    REQUEST-LINE b'POST /bunker.v1.Bunkerd/AgentMetrics HTTP/1.1'
  CONNECT 2026-09-29T21:08:59 peer=127.0.0.1:53228
    REQUEST-LINE b'POST /bunker.v1.Bunkerd/DestroyAgent HTTP/1.1'
  CONNECT 2026-09-29T21:09:00 peer=127.0.0.1:53242
    REQUEST-LINE b'POST /bunker.v1.Bunkerd/ExecAgent HTTP/1.1'
  CONNECT 2026-09-29T21:09:00 peer=127.0.0.1:53254
    REQUEST-LINE b'POST /bunker.v1.Bunkerd/ServerInfo HTTP/1.1'
  CONNECT 2026-09-29T21:09:00 peer=127.0.0.1:53266
    REQUEST-LINE b'POST /bunker.v1.Bunkerd/ListAgents HTTP/1.1'
```

## Test B — two sessions, two live servers on the same host, zero cross-writes

Two **shell processes** with different `BUNKER_SESSION_TARGET` values, against two daemons
running concurrently on `bunker-mvp`:

|  | session A | session B |
|--|-----------|-----------|
| binding | `BUNKER_SESSION_TARGET=bunker-mvp` | `BUNKER_SESSION_TARGET=gap093-scratch` |
| server | live demo daemon `127.0.0.1:18080` (auth enforced, token A) | scratch daemon `127.0.0.1:28070` (own token B) |
| agent | `gap093-iso-a` | `gap093-iso-b` |
| spawn port range | `10300-10399` (live pool `10000-19999`) | `21000-21099` (scratch pool `21000-21999`) |

Shared test config `active_server: gap093-scratch` — i.e. the "some other session last ran
`bunker use`" state, deliberately NOT either session's binding.

Server B is a HEAD-built `bunkerd` (`2a23f88`) started on the same host with **its own
`agent.base_data_dir` (`/var/lib/bunkerd-gap093`), its own disjoint port pool (`21000-21999`),
its own durable registry (`/var/lib/bunkerd-gap093/agents.jsonl`), its own audit trail
(`/tmp/gap093/audit.log`) and unique ports (`:28070/:29070`)** — the DF-BUNKER-13/18 hazard
surface (a second daemon on a shared host must never adopt/destroy the first daemon's agents)
is contained by construction, and the live daemon was re-listed after the scratch boot to prove
it. The driver refuses to continue unless the unit's `ExecMainPID` is the process actually
holding `:28070` — without that assert another daemon squatting the port can silently stand in
for server B (it did during this session's first attempt: CI's regression suite defaults to
`:28080/:29090` and its daemon answered instead, which is why the run was thrown away and
re-executed on free ports). The session shells run on the host because the scratch port is not
in the host firewall's allow-list (a control-host probe to `:28080` timed out while `:18080`
answered); the binding contract under test is identical either way.

Visibility matrix (per-session CLI, on the host):

```
session(A) bound to bunker-mvp     lists gap093-iso-a  : YES
session(B) bound to bunker-mvp     lists gap093-iso-b  : no
session(A) bound to gap093-scratch lists gap093-iso-a  : no
session(B) bound to gap093-scratch lists gap093-iso-b  : YES
```

Independent of the CLI — raw connectrpc `ListAgents` against each port:

```
live     daemon (:18080) knows gap093-iso-a  : YES
live     daemon (:18080) knows gap093-iso-b  : no
scratch  daemon (:28070) knows gap093-iso-a  : no
scratch  daemon (:28070) knows gap093-iso-b  : YES
```

Credential isolation (each daemon authenticates its own token only):

```
LIVE token    -> SCRATCH daemon : http=401
SCRATCH token -> LIVE daemon    : http=401
no token      -> LIVE daemon    : http=401
no token      -> SCRATCH daemon : http=401
```

**Zero cross-writes, proved from each daemon's own append-only audit trail.** Records written
before the pre-run watermark are excluded, so only this window is attributed. The window is
read by *method + agent id*: `DestroyAgent` records carry the id in `agent_id` and in
`summary` (a `SpawnAgent` record carries `agent_id: ""`, so the spawn attribution rests on the
spawn port range plus each daemon's own registry — both printed below):

```
--- AUDIT-TRAIL cross-write check (records appended during this test window only) ---
baseline line counts: live=9550 scratch=0

LIVE daemon, window records by method:
  gap093-iso-a -> AgentMetrics, DestroyAgent, ExecAgent, ExecAgent/command
  gap093-iso-b -> ExecAgent, ExecAgent/command      (the cross-exec ATTEMPT only: not_found)
SCRATCH daemon, records by method:
  gap093-iso-b -> SpawnAgent, GetAgentKey, AgentMetrics, ExecAgent, ExecAgent/command, DestroyAgent
  gap093-iso-a -> ExecAgent, ExecAgent/command      (the cross-exec ATTEMPT only: not_found)

=> the ONLY daemon that destroyed gap093-iso-a is the live daemon; the ONLY daemon that
   destroyed gap093-iso-b is the scratch daemon. Neither daemon ever spawned or destroyed the
   other's agent.
```

Post-destroy cells and leak census:

```
bunker-mvp lists gap093-iso-a: no
gap093-scratch lists gap093-iso-a: no
bunker-mvp lists gap093-iso-b: no
gap093-scratch lists gap093-iso-b: no
```

Full raw transcript:

```

======== 0. CLEAN UP residue from earlier attempts (idempotent re-runs) ========

======== 1. STAGE the HEAD CLI binary, both configs and the host helper scripts ========

======== 2. BASELINE + audit-log line-count watermark ========

======== 3. START the scratch daemon (server B) and ASSERT it is the process serving 28070 ========

======== 4. SESSION A shell on the host (binding: BUNKER_SESSION_TARGET=bunker-mvp) ========

======== 5. SESSION B shell on the host (binding: BUNKER_SESSION_TARGET=gap093-scratch) ========

======== 6. CROSS-VISIBILITY MATRIX (per-session CLI on the host) ========

======== 7. RAW REST probes + registry + token isolation + window-scoped AUDIT cross-write check ========

======== 8. DESTROY each agent from its OWN session shell ========

======== 9. POST-DESTROY verification + host leak census ========

======== 10. TEARDOWN: stop the scratch daemon, remove every staged artifact ========
TESTB4_DONE
TESTB4_RC=0

inactive
-rwxr-xr-x 1 root root 24276714 Sep 30 02:32 /tmp/gap093/bunkerd
cleaned
total 42928
drwxr-xr-x    3 root root     4096 Sep 30 02:36 .
drwxrwxrwt 5295 root root   561152 Sep 30 02:36 ..
-rw-r--r--    1 root root        5 Sep 30 02:33 audit-base-live.txt
-rw-r--r--    1 root root        2 Sep 30 02:33 audit-base-scr.txt
-rwxr-xr-x    1 root root 19063144 Sep 30 02:36 bunker-cli
-rwxr-xr-x    1 root root 24276714 Sep 30 02:32 bunkerd
-rwxr-xr-x    1 root root      321 Sep 30 02:36 host-census.sh
-rwxr-xr-x    1 root root      810 Sep 30 02:36 host-postcheck.sh
-rw-------    1 root root      475 Sep 30 02:36 host-two.yaml
drwx------    2 root root     4096 Sep 30 02:24 keys
-rwxr-xr-x    1 root root     4563 Sep 30 02:36 rest-probe.sh
-rw-------    1 root root     1607 Sep 30 02:36 scratch.yaml
-rwxr-xr-x    1 root root     1694 Sep 30 02:36 sessionA.sh
-rwxr-xr-x    1 root root     1590 Sep 30 02:36 sessionB.sh
--- staged host CLI identity (must equal the control-host build) ---
bunker 0.1.4
  commit:     2a23f88
  built:      2026-09-30T02:06:16Z
--- staged scratch config (pool / registry / ports) ---
  grpc_addr: ":29070"
  rest_addr: ":28070"
  enabled: true
  enabled: true
  path: /tmp/gap093/audit.log
  enabled: false
  # OWN base_data_dir => a DIFFERENT daemon instance id => every other daemon's
  # agent carries a foreign ownership marker (DF-BUNKER-18 skip path).
  base_data_dir: /var/lib/bunkerd-gap093
  max_agents: 5
  port_range_start: 21000
  port_range_end: 21999
  port_range_per_agent: 100
    enabled: true
    path: /var/lib/bunkerd-gap093/agents.jsonl
--- live daemon agent list (read-only, control host) ---
  Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
  ────────       ──────     ─────────      ─────────────  ───────                   ──────────
  da5b7b3c       destroy-refused:live_processes 5.0 GB         20.0 GB        2026-09-29T20:08:04Z      (no URL)
  54c8bf55       destroy-refused:live_processes 5.4 GB         20.0 GB        2026-09-29T02:53:38Z      (no URL)
  95f3325a       destroy-refused:live_processes 6.2 GB         20.0 GB        2026-09-27T14:15:46Z      (no URL)
  9b4fe54d       destroy-refused:live_processes 6.5 GB         20.0 GB        2026-09-28T02:51:47Z      (no URL)
  fe5ee3ac       destroy-refused:live_processes 9.9 GB         20.0 GB        2026-09-29T12:15:57Z      (no URL)
  b88c12af       destroy-refused:live_processes 5.2 GB         20.0 GB        2026-09-29T12:57:26Z      (no URL)
  718bf3d4       destroy-refused:live_processes 5.8 GB         20.0 GB        2026-09-27T13:39:31Z      (no URL)
  7dfbd5ea       destroy-refused:live_processes 8.5 GB         20.0 GB        2026-09-27T13:50:08Z      (no URL)
  a9ebf454       destroy-refused:live_processes 5.7 GB         20.0 GB        2026-09-27T13:39:59Z      (no URL)

Total: 9 agents (server: bunker-mvp)
Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
rc=0
--- live daemon service state ---
NRestarts=0
ExecMainPID=2016609
ActiveState=active
ActiveEnterTimestamp=Tue 2026-09-29 06:11:32 UTC
--- live agent census on the host (user / owner-marker / persisted pool) ---
bunker-718bf3d4                    240748fc931ceaf44b387dd4309ed1cf | 10500-10599
bunker-a9ebf454                    240748fc931ceaf44b387dd4309ed1cf | 11100-11199
bunker-7dfbd5ea                    240748fc931ceaf44b387dd4309ed1cf | 10900-10999
bunker-95f3325a                    240748fc931ceaf44b387dd4309ed1cf | 10200-10299
bunker-9b4fe54d                    240748fc931ceaf44b387dd4309ed1cf | 10000-10099
bunker-54c8bf55                    240748fc931ceaf44b387dd4309ed1cf | 11200-11299
bunker-fe5ee3ac                    240748fc931ceaf44b387dd4309ed1cf | 10400-10499
bunker-b88c12af                    240748fc931ceaf44b387dd4309ed1cf | 10600-10699
bunker-da5b7b3c                    240748fc931ceaf44b387dd4309ed1cf | 10100-10199
bunker-e2e-imgspec                 240748fc931ceaf44b387dd4309ed1cf | 30300-30399
/tmp/gap093/host-census.sh: line 5: /home/bunker-e2e-imgspec-b/.bunker/ports: No such file or directory
bunker-e2e-imgspec-b                | 
live audit watermark: 9550 lines
--- ports 28070/29070 must be free ---
ports free
Running as unit: gap093-scratch.service; invocation ID: e9331dd485e84f4f8c1fa5b4932d3d7b
   attempt 1: ExecMainPID=485593  port :28070 held by pid=485593
--- scratch unit state ---
ExecMainPID=485593
ActiveState=active
SubState=running
LISTEN 0      4096                             *:29070       *:* users:(("bunkerd",pid=485593,fd=8))                       
LISTEN 0      4096                             *:28070       *:* users:(("bunkerd",pid=485593,fd=7))                       
--- readiness ASSERTED: gap093-scratch.service (pid 485593) owns :28070 ---
--- scratch daemon boot log ---
{"time":"2026-09-30T02:37:40.339546076Z","level":"WARN","msg":"bunkerd: *** INSECURE: serving plaintext on non-loopback listener :29070, :28070 (tls.enabled is false and tls.insecure_dev is true). Any client that can reach this address controls the RPC plane; every audit record is marked [INSECURE-PLAINTEXT]. Disable tls.insecure_dev and enable TLS for anything reachable beyond this host. ***"}
bunkerd: *** INSECURE: serving plaintext on non-loopback listener :29070, :28070 (tls.enabled is false and tls.insecure_dev is true). Any client that can reach this address controls the RPC plane; every audit record is marked [INSECURE-PLAINTEXT]. Disable tls.insecure_dev and enable TLS for anything reachable beyond this host. ***
{"time":"2026-09-30T02:37:40.339792766Z","level":"INFO","msg":"daemon instance identity","instance_id":"0bb82a1b048c5791f76057892e336687"}
{"time":"2026-09-30T02:37:40.3398419Z","level":"INFO","msg":"agent registry replayed","path":"/var/lib/bunkerd-gap093/agents.jsonl","files":1,"events":0,"live":0,"known":0,"malformed":0,"partial_tail":false,"created_this_boot":true}
{"time":"2026-09-30T02:37:40.339872218Z","level":"INFO","msg":"agent reconciliation sweep guard armed","guard":"orphan_sweep_guard","enabled":true,"mode":"destroy","unproven_orphan_limit":3,"refuses":"an unproven sweep (empty replayed live set, or a registry file created by this boot) that would destroy more than the limit"}
{"time":"2026-09-30T02:37:40.339923976Z","level":"INFO","msg":"agent registry reconciliation dispatched","mode":"destroy","replayed_live":0,"replayed_known":0,"restored":0,"restored_foreign_pool":0,"purged":0,"orphans_async":true}
{"time":"2026-09-30T02:37:40.340092317Z","level":"ERROR","msg":"registry reconcile: REFUSING to destroy unproven orphans — NOTHING was destroyed","action":"refuse","guard":"orphan_sweep_guard","refused_orphans":11,"unproven_orphan_limit":3,"reason":"the durable registry cannot vouch for this host (replayed live=0, registry file created by this boot=true) while the orphan walk found 11 unknown bunker-* users, above the limit of 3","mode":"destroy","registry_path":"/var/lib/bunkerd-gap093/agents.jsonl","remedy":"NOTHING was destroyed. First check whether the durable registry was lost: if it was, restore /var/lib/bunkerd-gap093/agents.jsonl (or its .1/.2/.3 backups) so bunkerd can recognise its own agents — do NOT raise the limit, that path deletes live agents. If instead these are leftover test/battery users and the registry is intact, restart bunkerd with agent.reconciliation.unproven_orphan_limit raised above 11 (env BUNKERD_AGENT_RECONCILIATION_UNPROVEN_ORPHAN_LIMIT), or accept the whole population being removed by setting agent.reconciliation.orphan_sweep_guard_disabled: true."}
{"time":"2026-09-30T02:37:40.340154355Z","level":"INFO","msg":"agent registry reconciliation complete (async orphan walk)","mode":"destroy","adopted":0,"destroyed":0,"foreign":0,"unproven":0,"refused":11}
{"time":"2026-09-30T02:37:40.340544338Z","level":"INFO","msg":"bunkerd REST listening","addr":":28070","tls":false}
{"time":"2026-09-30T02:37:40.340718698Z","level":"INFO","msg":"bunkerd gRPC listening","addr":":29070","tls":false}
--- LIVE daemon after the scratch boot (must be untouched: same agents, same pid) ---
  da5b7b3c       destroy-refused:live_processes 5.0 GB         20.0 GB        2026-09-29T20:08:04Z      (no URL)

Total: 9 agents (server: bunker-mvp)
Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
rc=0
NRestarts=0
ExecMainPID=2016609
ActiveState=active
### session A (bash pid 486313) — BUNKER_SESSION_TARGET=bunker-mvp  phase=up
### cli=/tmp/gap093/bunker-cli  config=/tmp/gap093/host-two.yaml  (own shell process, own exported binding)
── A1: list (what can THIS session see?)
bunker: reading server "bunker-mvp"

══════════ Agents ══════════

  Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
  ────────       ──────     ─────────      ─────────────  ───────                   ──────────
  54c8bf55       destroy-refused:live_processes 5.4 GB         20.0 GB        2026-09-29T02:53:38Z      (no URL)
  95f3325a       destroy-refused:live_processes 6.2 GB         20.0 GB        2026-09-27T14:15:46Z      (no URL)
  9b4fe54d       destroy-refused:live_processes 6.5 GB         20.0 GB        2026-09-28T02:51:47Z      (no URL)
  fe5ee3ac       destroy-refused:live_processes 9.9 GB         20.0 GB        2026-09-29T12:15:57Z      (no URL)
  b88c12af       destroy-refused:live_processes 5.2 GB         20.0 GB        2026-09-29T12:57:26Z      (no URL)
  718bf3d4       destroy-refused:live_processes 5.8 GB         20.0 GB        2026-09-27T13:39:31Z      (no URL)
  7dfbd5ea       destroy-refused:live_processes 8.5 GB         20.0 GB        2026-09-27T13:50:08Z      (no URL)
  a9ebf454       destroy-refused:live_processes 5.7 GB         20.0 GB        2026-09-27T13:39:59Z      (no URL)
  da5b7b3c       destroy-refused:live_processes 5.0 GB         20.0 GB        2026-09-29T20:08:04Z      (no URL)

Total: 9 agents (server: bunker-mvp)
Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
   rc=0
── A2: spawn gap093-iso-a on the bound server
Creating agent...
Agent created: gap093-iso-a

══════════ Connection Bundle ══════════

  Docker SSH:   DOCKER_HOST=ssh://bunker-gap093-iso-a@127.0.0.1
  SSH Key:      (saved to ~/.bunker/keys/)
                /tmp/gap093/keys/gap093-iso-a
  Port Range:   10300-10399
  Expires:      2026-09-30T03:08:33Z
  API Key:      [REDACTED — ephemeral per-agent key; its agent was destroyed in this run]
  SSHFS Mount:  sshfs -o IdentityFile=/tmp/gap093/keys/gap093-iso-a -o idmap=user -o allow_other bunker-gap093-iso-a@127.0.0.1:/home/bunker-gap093-iso-a /mnt/bunker/gap093-iso-a
  Docker Tunnel: ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -i /tmp/gap093/keys/gap093-iso-a -L 2376:/run/bunker/gap093-iso-a/docker.sock bunker-gap093-iso-a@127.0.0.1 -N

═ Use `bunker exec` to run commands in this agent ═
   rc=0
── A3: list after the spawn
bunker: reading server "bunker-mvp"

══════════ Agents ══════════

  Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
  ────────       ──────     ─────────      ─────────────  ───────                   ──────────
  54c8bf55       destroy-refused:live_processes 5.4 GB         20.0 GB        2026-09-29T02:53:38Z      (no URL)
  95f3325a       destroy-refused:live_processes 6.2 GB         20.0 GB        2026-09-27T14:15:46Z      (no URL)
  9b4fe54d       destroy-refused:live_processes 6.5 GB         20.0 GB        2026-09-28T02:51:47Z      (no URL)
  fe5ee3ac       destroy-refused:live_processes 9.9 GB         20.0 GB        2026-09-29T12:15:57Z      (no URL)
  b88c12af       destroy-refused:live_processes 5.2 GB         20.0 GB        2026-09-29T12:57:26Z      (no URL)
  718bf3d4       destroy-refused:live_processes 5.8 GB         20.0 GB        2026-09-27T13:39:31Z      (no URL)
  7dfbd5ea       destroy-refused:live_processes 8.5 GB         20.0 GB        2026-09-27T13:50:08Z      (no URL)
  a9ebf454       destroy-refused:live_processes 5.7 GB         20.0 GB        2026-09-27T13:39:59Z      (no URL)
  gap093-iso-a   running    246.7 MB       20.0 GB        2026-09-30T02:38:33Z      (no URL)
  da5b7b3c       destroy-refused:live_processes 5.0 GB         20.0 GB        2026-09-29T20:08:04Z      (no URL)

Total: 10 agents (server: bunker-mvp)
Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
   rc=0
── A4: exec inside A's own agent (agent-side identity + its own marker file)
bunker-gap093-iso-a
bunker-mvp
iso-a-marker
   rc=0
── A5: CROSS-EXEC attempt: reach B's agent from session A (must NOT be reachable)
bunker: stream error: not_found: agent "gap093-iso-b" not found
   rc=1
SESSION-A-up-DONE
### session B (bash pid 487977) — BUNKER_SESSION_TARGET=gap093-scratch  phase=up
### cli=/tmp/gap093/bunker-cli  config=/tmp/gap093/host-two.yaml  (own shell process, own exported binding)
── B1: list (what can THIS session see?)
bunker: reading server "gap093-scratch"
No agents found.
   rc=0
── B2: spawn gap093-iso-b on the bound server
Creating agent...
Agent created: gap093-iso-b

══════════ Connection Bundle ══════════

  Docker SSH:   DOCKER_HOST=ssh://bunker-gap093-iso-b@127.0.0.1
  SSH Key:      (saved to ~/.bunker/keys/)
                /tmp/gap093/keys/gap093-iso-b
  Port Range:   21000-21099
  Expires:      2026-09-30T03:08:59Z
  API Key:      [REDACTED — ephemeral per-agent key; its agent was destroyed in this run]
  SSHFS Mount:  sshfs -o IdentityFile=/tmp/gap093/keys/gap093-iso-b -o idmap=user -o allow_other bunker-gap093-iso-b@127.0.0.1:/home/bunker-gap093-iso-b /mnt/bunker/gap093-iso-b
  Docker Tunnel: ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -i /tmp/gap093/keys/gap093-iso-b -L 2376:/run/bunker/gap093-iso-b/docker.sock bunker-gap093-iso-b@127.0.0.1 -N

═ Use `bunker exec` to run commands in this agent ═
   rc=0
── B3: list after the spawn
bunker: reading server "gap093-scratch"

══════════ Agents ══════════

  Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
  ────────       ──────     ─────────      ─────────────  ───────                   ──────────
  gap093-iso-b   running    0 B            20.0 GB        2026-09-30T02:38:59Z      (no URL)

Total: 1 agents (server: gap093-scratch)
Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
   rc=0
── B4: exec inside B's own agent (agent-side identity + its own marker file)
bunker-gap093-iso-b
bunker-mvp
iso-b-marker
   rc=0
── B5: CROSS-EXEC attempt: reach A's agent from session B (must NOT be reachable)
bunker: stream error: not_found: agent "gap093-iso-a" not found
   rc=1
SESSION-B-up-DONE
   session(A) bound to bunker-mvp     lists gap093-iso-a  : YES
   session(B) bound to bunker-mvp     lists gap093-iso-b  : no
   session(A) bound to gap093-scratch lists gap093-iso-a  : no
   session(B) bound to gap093-scratch lists gap093-iso-b  : YES
--- raw lists, so the row counts are auditable ---
   [session A -> bunker-mvp]
      bunker: reading server "bunker-mvp"
      
      ══════════ Agents ══════════
      
        Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
        ────────       ──────     ─────────      ─────────────  ───────                   ──────────
        9b4fe54d       destroy-refused:live_processes 6.5 GB         20.0 GB        2026-09-28T02:51:47Z      (no URL)
        fe5ee3ac       destroy-refused:live_processes 9.9 GB         20.0 GB        2026-09-29T12:15:57Z      (no URL)
        b88c12af       destroy-refused:live_processes 5.2 GB         20.0 GB        2026-09-29T12:57:26Z      (no URL)
        718bf3d4       destroy-refused:live_processes 5.8 GB         20.0 GB        2026-09-27T13:39:31Z      (no URL)
        7dfbd5ea       destroy-refused:live_processes 8.5 GB         20.0 GB        2026-09-27T13:50:08Z      (no URL)
        a9ebf454       destroy-refused:live_processes 5.7 GB         20.0 GB        2026-09-27T13:39:59Z      (no URL)
        gap093-iso-a   running    246.7 MB       20.0 GB        2026-09-30T02:38:33Z      (no URL)
        da5b7b3c       destroy-refused:live_processes 5.0 GB         20.0 GB        2026-09-29T20:08:04Z      (no URL)
        54c8bf55       destroy-refused:live_processes 5.4 GB         20.0 GB        2026-09-29T02:53:38Z      (no URL)
        95f3325a       destroy-refused:live_processes 6.2 GB         20.0 GB        2026-09-27T14:15:46Z      (no URL)
      
      Total: 10 agents (server: bunker-mvp)
      Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
   [session B -> gap093-scratch]
      bunker: reading server "gap093-scratch"
      
      ══════════ Agents ══════════
      
        Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
        ────────       ──────     ─────────      ─────────────  ───────                   ──────────
        gap093-iso-b   running    246.7 MB       20.0 GB        2026-09-30T02:38:59Z      (no URL)
      
      Total: 1 agents (server: gap093-scratch)
      Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
--- spawn port ranges prove WHICH daemon served each spawn ---
/tmp/gap093/sessionA.log:  Port Range:   10300-10399
/tmp/gap093/sessionB.log:  Port Range:   21000-21099
--- ListAgents on the LIVE daemon http://127.0.0.1:18080 ---
{"agents":[{"agentId":"a9ebf454","status":"destroy-refused:live_processes","limits":{"cpuQuota":2,"memoryMaxBytes":"4294967296","diskMaxBytes":"21474836480","maxDockerContainers":10},"createdAt":"2026-09-27T13:39:59Z","expiresAt":"2026-09-27T17:39:59Z","sshfsMount":"sshfs -o IdentityFile=/etc/bunkerd/ssh/a9ebf454 -o idmap=user -o allow_other bunker-a9ebf454@bunker-mvp:/home/bunker-a9ebf454 /mnt/bunker/a9ebf454","dockerHostTunnel":"ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -i /etc/bunkerd/ssh/a9ebf454 -L 2376:/run/bunker/a9ebf454/docker.sock bunker-a9ebf454@bunker-mvp -N","portRangeStart":11100,"portRangeEnd":11199,"diskUsedBytes":"6139388912","safetyPreset":"standard","systemdProperties":[{"name":"CPUQuota","value":"200%"},{"name":"MemoryMax","value":"4294967296"},{"name":"LimitFSIZE","value":"21474836480"},{"name":"TasksMax",
--- ListAgents on the SCRATCH daemon http://127.0.0.1:28070 ---
{"agents":[{"agentId":"gap093-iso-b", "status":"running", "limits":{"cpuQuota":2, "memoryMaxBytes":"4294967296", "diskMaxBytes":"21474836480", "maxDockerContainers":10}, "createdAt":"2026-09-30T02:38:59Z", "expiresAt":"2026-09-30T03:08:59Z", "sshfsMount":"sshfs -o IdentityFile=/etc/bunkerd/ssh/gap093-iso-b -o idmap=user -o allow_other bunker-gap093-iso-b@bunker-mvp:/home/bunker-gap093-iso-b /mnt/bunker/gap093-iso-b", "dockerHostTunnel":"ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -i /etc/bunkerd/ssh/gap093-iso-b -L 2376:/run/bunker/gap093-iso-b/docker.sock bunker-gap093-iso-b@bunker-mvp -N", "portRangeStart":21000, "portRangeEnd":21099, "diskUsedBytes":"258666431", "safetyPreset":"standard", "systemdProperties":[{"name":"CPUQuota", "value":"200%"}, {"name":"MemoryMax", "value":"4294967296"}, {"name":"LimitFSIZE", "value":"214748
--- which daemon knows which agent (raw API, no CLI) ---
   live     daemon (:18080) knows gap093-iso-a  : YES
   live     daemon (:18080) knows gap093-iso-b  : no
   scratch  daemon (:28070) knows gap093-iso-a  : no
   scratch  daemon (:28070) knows gap093-iso-b  : YES
--- token isolation (each credential is refused by the OTHER daemon) ---
   LIVE token    -> SCRATCH daemon : http=401
   SCRATCH token -> LIVE daemon    : http=401
   no token      -> LIVE daemon    : http=401
   no token      -> SCRATCH daemon : http=401
--- scratch daemon registry contents (its OWN durable state) ---
   registry file: -rw------- 1 root root 1347 Sep 30 02:38 /var/lib/bunkerd-gap093/agents.jsonl
"agent_id":"gap093-iso-b" 
--- AUDIT-TRAIL cross-write check (records appended during this test window only) ---
   baseline line counts: live=9550 scratch=0
   LIVE audit.log new lines naming gap093 agents, with method:
      1 "method":"/bunker.v1.Bunkerd/ExecAgent"	"agent_id":"gap093-iso-a"
      1 "method":"/bunker.v1.Bunkerd/ExecAgent"	"agent_id":"gap093-iso-b"
      1 "method":"/bunker.v1.Bunkerd/ExecAgent/command"	"agent_id":"gap093-iso-a"
      1 "method":"/bunker.v1.Bunkerd/ExecAgent/command"	"agent_id":"gap093-iso-b"
   SCRATCH audit.log new lines naming gap093 agents, with method:
      1 "method":"/bunker.v1.Bunkerd/ExecAgent"	"agent_id":"gap093-iso-a"
      1 "method":"/bunker.v1.Bunkerd/ExecAgent"	"agent_id":"gap093-iso-b"
      1 "method":"/bunker.v1.Bunkerd/ExecAgent/command"	"agent_id":"gap093-iso-a"
      1 "method":"/bunker.v1.Bunkerd/ExecAgent/command"	"agent_id":"gap093-iso-b"
   SpawnAgent/DestroyAgent records naming gap093-iso-a (new lines only):
     live    : 0
     scratch : 0
   SpawnAgent/DestroyAgent records naming gap093-iso-b (new lines only):
     live    : 0
     scratch : 0
   daemon instance ids: live=240748fc931ceaf44b387dd4309ed1cf scratch=0bb82a1b048c5791f76057892e336687
### session A (bash pid 486313) — BUNKER_SESSION_TARGET=bunker-mvp  phase=up
### cli=/tmp/gap093/bunker-cli  config=/tmp/gap093/host-two.yaml  (own shell process, own exported binding)
── A1: list (what can THIS session see?)
bunker: reading server "bunker-mvp"

══════════ Agents ══════════

  Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
  ────────       ──────     ─────────      ─────────────  ───────                   ──────────
  54c8bf55       destroy-refused:live_processes 5.4 GB         20.0 GB        2026-09-29T02:53:38Z      (no URL)
  95f3325a       destroy-refused:live_processes 6.2 GB         20.0 GB        2026-09-27T14:15:46Z      (no URL)
  9b4fe54d       destroy-refused:live_processes 6.5 GB         20.0 GB        2026-09-28T02:51:47Z      (no URL)
  fe5ee3ac       destroy-refused:live_processes 9.9 GB         20.0 GB        2026-09-29T12:15:57Z      (no URL)
  b88c12af       destroy-refused:live_processes 5.2 GB         20.0 GB        2026-09-29T12:57:26Z      (no URL)
  718bf3d4       destroy-refused:live_processes 5.8 GB         20.0 GB        2026-09-27T13:39:31Z      (no URL)
  7dfbd5ea       destroy-refused:live_processes 8.5 GB         20.0 GB        2026-09-27T13:50:08Z      (no URL)
  a9ebf454       destroy-refused:live_processes 5.7 GB         20.0 GB        2026-09-27T13:39:59Z      (no URL)
  da5b7b3c       destroy-refused:live_processes 5.0 GB         20.0 GB        2026-09-29T20:08:04Z      (no URL)

Total: 9 agents (server: bunker-mvp)
Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
   rc=0
── A2: spawn gap093-iso-a on the bound server
Creating agent...
Agent created: gap093-iso-a

══════════ Connection Bundle ══════════

  Docker SSH:   DOCKER_HOST=ssh://bunker-gap093-iso-a@127.0.0.1
  SSH Key:      (saved to ~/.bunker/keys/)
                /tmp/gap093/keys/gap093-iso-a
  Port Range:   10300-10399
  Expires:      2026-09-30T03:08:33Z
  API Key:      [REDACTED — ephemeral per-agent key; its agent was destroyed in this run]
  SSHFS Mount:  sshfs -o IdentityFile=/tmp/gap093/keys/gap093-iso-a -o idmap=user -o allow_other bunker-gap093-iso-a@127.0.0.1:/home/bunker-gap093-iso-a /mnt/bunker/gap093-iso-a
  Docker Tunnel: ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -i /tmp/gap093/keys/gap093-iso-a -L 2376:/run/bunker/gap093-iso-a/docker.sock bunker-gap093-iso-a@127.0.0.1 -N

═ Use `bunker exec` to run commands in this agent ═
   rc=0
── A3: list after the spawn
bunker: reading server "bunker-mvp"

══════════ Agents ══════════

  Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
  ────────       ──────     ─────────      ─────────────  ───────                   ──────────
  54c8bf55       destroy-refused:live_processes 5.4 GB         20.0 GB        2026-09-29T02:53:38Z      (no URL)
  95f3325a       destroy-refused:live_processes 6.2 GB         20.0 GB        2026-09-27T14:15:46Z      (no URL)
  9b4fe54d       destroy-refused:live_processes 6.5 GB         20.0 GB        2026-09-28T02:51:47Z      (no URL)
  fe5ee3ac       destroy-refused:live_processes 9.9 GB         20.0 GB        2026-09-29T12:15:57Z      (no URL)
  b88c12af       destroy-refused:live_processes 5.2 GB         20.0 GB        2026-09-29T12:57:26Z      (no URL)
  718bf3d4       destroy-refused:live_processes 5.8 GB         20.0 GB        2026-09-27T13:39:31Z      (no URL)
  7dfbd5ea       destroy-refused:live_processes 8.5 GB         20.0 GB        2026-09-27T13:50:08Z      (no URL)
  a9ebf454       destroy-refused:live_processes 5.7 GB         20.0 GB        2026-09-27T13:39:59Z      (no URL)
  gap093-iso-a   running    246.7 MB       20.0 GB        2026-09-30T02:38:33Z      (no URL)
  da5b7b3c       destroy-refused:live_processes 5.0 GB         20.0 GB        2026-09-29T20:08:04Z      (no URL)

Total: 10 agents (server: bunker-mvp)
Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
   rc=0
── A4: exec inside A's own agent (agent-side identity + its own marker file)
bunker-gap093-iso-a
bunker-mvp
iso-a-marker
   rc=0
── A5: CROSS-EXEC attempt: reach B's agent from session A (must NOT be reachable)
bunker: stream error: not_found: agent "gap093-iso-b" not found
   rc=1
SESSION-A-up-DONE
### session A (bash pid 491490) — BUNKER_SESSION_TARGET=bunker-mvp  phase=down
### cli=/tmp/gap093/bunker-cli  config=/tmp/gap093/host-two.yaml  (own shell process, own exported binding)
── A6: destroy gap093-iso-a from its own session
Destroying agent gap093-iso-a (home 246.7 MB; archiving before delete, deadline 12m0s)…
Agent gap093-iso-a destroyed.
Removed local SSH key /tmp/gap093/keys/gap093-iso-a
   rc=0
── A7: list after the destroy
bunker: reading server "bunker-mvp"

══════════ Agents ══════════

  Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
  ────────       ──────     ─────────      ─────────────  ───────                   ──────────
  54c8bf55       destroy-refused:live_processes 5.4 GB         20.0 GB        2026-09-29T02:53:38Z      (no URL)
  95f3325a       destroy-refused:live_processes 6.2 GB         20.0 GB        2026-09-27T14:15:46Z      (no URL)
  9b4fe54d       destroy-refused:live_processes 6.5 GB         20.0 GB        2026-09-28T02:51:47Z      (no URL)
  fe5ee3ac       destroy-refused:live_processes 9.9 GB         20.0 GB        2026-09-29T12:15:57Z      (no URL)
  b88c12af       destroy-refused:live_processes 5.2 GB         20.0 GB        2026-09-29T12:57:26Z      (no URL)
  718bf3d4       destroy-refused:live_processes 5.8 GB         20.0 GB        2026-09-27T13:39:31Z      (no URL)
  7dfbd5ea       destroy-refused:live_processes 8.5 GB         20.0 GB        2026-09-27T13:50:08Z      (no URL)
  a9ebf454       destroy-refused:live_processes 5.7 GB         20.0 GB        2026-09-27T13:39:59Z      (no URL)
  da5b7b3c       destroy-refused:live_processes 5.0 GB         20.0 GB        2026-09-29T20:08:04Z      (no URL)

Total: 9 agents (server: bunker-mvp)
Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
   rc=0
SESSION-A-down-DONE
### session B (bash pid 487977) — BUNKER_SESSION_TARGET=gap093-scratch  phase=up
### cli=/tmp/gap093/bunker-cli  config=/tmp/gap093/host-two.yaml  (own shell process, own exported binding)
── B1: list (what can THIS session see?)
bunker: reading server "gap093-scratch"
No agents found.
   rc=0
── B2: spawn gap093-iso-b on the bound server
Creating agent...
Agent created: gap093-iso-b

══════════ Connection Bundle ══════════

  Docker SSH:   DOCKER_HOST=ssh://bunker-gap093-iso-b@127.0.0.1
  SSH Key:      (saved to ~/.bunker/keys/)
                /tmp/gap093/keys/gap093-iso-b
  Port Range:   21000-21099
  Expires:      2026-09-30T03:08:59Z
  API Key:      [REDACTED — ephemeral per-agent key; its agent was destroyed in this run]
  SSHFS Mount:  sshfs -o IdentityFile=/tmp/gap093/keys/gap093-iso-b -o idmap=user -o allow_other bunker-gap093-iso-b@127.0.0.1:/home/bunker-gap093-iso-b /mnt/bunker/gap093-iso-b
  Docker Tunnel: ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -i /tmp/gap093/keys/gap093-iso-b -L 2376:/run/bunker/gap093-iso-b/docker.sock bunker-gap093-iso-b@127.0.0.1 -N

═ Use `bunker exec` to run commands in this agent ═
   rc=0
── B3: list after the spawn
bunker: reading server "gap093-scratch"

══════════ Agents ══════════

  Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
  ────────       ──────     ─────────      ─────────────  ───────                   ──────────
  gap093-iso-b   running    0 B            20.0 GB        2026-09-30T02:38:59Z      (no URL)

Total: 1 agents (server: gap093-scratch)
Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
   rc=0
── B4: exec inside B's own agent (agent-side identity + its own marker file)
bunker-gap093-iso-b
bunker-mvp
iso-b-marker
   rc=0
── B5: CROSS-EXEC attempt: reach A's agent from session B (must NOT be reachable)
bunker: stream error: not_found: agent "gap093-iso-a" not found
   rc=1
SESSION-B-up-DONE
### session B (bash pid 493977) — BUNKER_SESSION_TARGET=gap093-scratch  phase=down
### cli=/tmp/gap093/bunker-cli  config=/tmp/gap093/host-two.yaml  (own shell process, own exported binding)
── B6: destroy gap093-iso-b from its own session
Destroying agent gap093-iso-b (home 246.7 MB; archiving before delete, deadline 12m0s)…
Agent gap093-iso-b destroyed.
Removed local SSH key /tmp/gap093/keys/gap093-iso-b
   rc=0
── B7: list after the destroy
bunker: reading server "gap093-scratch"
No agents found.
   rc=0
SESSION-B-down-DONE
   bunker-mvp lists gap093-iso-a: no
   gap093-scratch lists gap093-iso-a: no
   bunker-mvp lists gap093-iso-b: no
   gap093-scratch lists gap093-iso-b: no
bunker-gap093-iso-a: absent
bunker-gap093-iso-b: absent
no gap093 ssh keys left
no gap093 agent homes left
--- scratch unit + ports ---
active
scratch ports 28080/29090 closed
--- live daemon service state ---
NRestarts=0
ExecMainPID=2016609
ActiveState=active
SubState=running
ActiveEnterTimestamp=Tue 2026-09-29 06:11:32 UTC
--- scratch daemon log tail ---
{"time":"2026-09-30T02:40:06.430619504Z","level":"INFO","msg":"archiving agent home before delete","agent_id":"gap093-iso-b","policy":"archive","home":"/home/bunker-gap093-iso-b","archive_dir":"/var/backups/bunker","home_bytes":258256734,"archive_budget":"10m0s","excluded":".local/share/docker"}
{"time":"2026-09-30T02:40:21.047494424Z","level":"INFO","msg":"agent home archived and verified","agent_id":"gap093-iso-b","policy":"archive","archive_path":"/var/backups/bunker/bunker-gap093-iso-b-20260930T024006Z.tar.gz"}
{"time":"2026-09-30T02:40:21.047772754Z","level":"INFO","msg":"archive prune: removing old archive beyond keep limit","dir":"/var/backups/bunker","file":"bunker-gap075-a-20260930T022255Z.tar.gz","keep":20}
{"time":"2026-09-30T02:40:21.06348255Z","level":"INFO","msg":"archive prune: removing old archive beyond keep limit","dir":"/var/backups/bunker","file":"bunker-probeok-88364-20260930T022126Z.tar.gz","keep":20}
{"time":"2026-09-30T02:40:21.082299722Z","level":"INFO","msg":"archive prune removed old archives","archive_dir":"/var/backups/bunker","removed":2}
{"time":"2026-09-30T02:40:21.199447542Z","level":"INFO","msg":"agent unregistered","agent_id":"gap093-iso-b","total":0}
{"time":"2026-09-30T02:40:21.199487017Z","level":"INFO","msg":"freed port range","agent_id":"gap093-iso-b"}
{"time":"2026-09-30T02:40:21.201466637Z","level":"INFO","msg":"agent destroyed","agent_id":"gap093-iso-b"}
2026/09/30 02:40:21 [bunker-mvp/PU477ogx0L-000016] "POST http://127.0.0.1:28070/bunker.v1.Bunkerd/DestroyAgent HTTP/1.1" from 127.0.0.1:53316 - 200 49B in 20.924884673s
2026/09/30 02:40:21 [bunker-mvp/PU477ogx0L-000017] "POST http://127.0.0.1:28070/bunker.v1.Bunkerd/ListAgents HTTP/1.1" from 127.0.0.1:60204 - 200 23B in 879.455µs
2026/09/30 02:40:29 [bunker-mvp/PU477ogx0L-000018] "POST http://127.0.0.1:28070/bunker.v1.Bunkerd/ListAgents HTTP/1.1" from 127.0.0.1:40222 - 200 23B in 1.681441ms
2026/09/30 02:40:34 [bunker-mvp/PU477ogx0L-000019] "POST http://127.0.0.1:28070/bunker.v1.Bunkerd/ListAgents HTTP/1.1" from 127.0.0.1:40230 - 200 23B in 902.708µs
scratch ports closed
inactive
audit-base-live.txt
audit-base-scr.txt
audit.log
audit-scratch-final.log
host-census.sh
host-postcheck.sh
host-two.yaml
keys
registry-scratch-final.jsonl
rest-probe.sh
scratch-daemon.log
scratch.yaml
sessionA.log
sessionB.log
--- live daemon FINAL state (control host) ---
  Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
  ────────       ──────     ─────────      ─────────────  ───────                   ──────────
  718bf3d4       destroy-refused:live_processes 5.8 GB         20.0 GB        2026-09-27T13:39:31Z      (no URL)
  7dfbd5ea       destroy-refused:live_processes 8.5 GB         20.0 GB        2026-09-27T13:50:08Z      (no URL)
  a9ebf454       destroy-refused:live_processes 5.7 GB         20.0 GB        2026-09-27T13:39:59Z      (no URL)
  da5b7b3c       destroy-refused:live_processes 5.0 GB         20.0 GB        2026-09-29T20:08:04Z      (no URL)
  54c8bf55       destroy-refused:live_processes 5.4 GB         20.0 GB        2026-09-29T02:53:38Z      (no URL)
  95f3325a       destroy-refused:live_processes 6.2 GB         20.0 GB        2026-09-27T14:15:46Z      (no URL)
  9b4fe54d       destroy-refused:live_processes 6.5 GB         20.0 GB        2026-09-28T02:51:47Z      (no URL)
  fe5ee3ac       destroy-refused:live_processes 9.9 GB         20.0 GB        2026-09-29T12:15:57Z      (no URL)
  b88c12af       destroy-refused:live_processes 5.2 GB         20.0 GB        2026-09-29T12:57:26Z      (no URL)

Total: 9 agents (server: bunker-mvp)
Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
rc=0
NRestarts=0
ExecMainPID=2016609
ActiveState=active
ActiveEnterTimestamp=Tue 2026-09-29 06:11:32 UTC
--- final host user census ---
bunker-718bf3d4                    240748fc931ceaf44b387dd4309ed1cf | 10500-10599
bunker-a9ebf454                    240748fc931ceaf44b387dd4309ed1cf | 11100-11199
bunker-7dfbd5ea                    240748fc931ceaf44b387dd4309ed1cf | 10900-10999
bunker-95f3325a                    240748fc931ceaf44b387dd4309ed1cf | 10200-10299
bunker-9b4fe54d                    240748fc931ceaf44b387dd4309ed1cf | 10000-10099
bunker-54c8bf55                    240748fc931ceaf44b387dd4309ed1cf | 11200-11299
bunker-fe5ee3ac                    240748fc931ceaf44b387dd4309ed1cf | 10400-10499
bunker-b88c12af                    240748fc931ceaf44b387dd4309ed1cf | 10600-10699
bunker-da5b7b3c                    240748fc931ceaf44b387dd4309ed1cf | 10100-10199

TEST-B-COMPLETE 2026-09-30T02:40:56Z
```

## Test C — read-only verbs keep the convenience default and NAME the target

Result: **7 PASS / 0 FAIL**.

```
### GAP-093 Test C — read-only verbs name the server they read
binary: bunker 0.1.4
shared config: /home/kara/.bunker/config.yaml
shared active_server: karahermes-mde-7840hs
live demo server entry: http://78.46.173.180:18080

## (a) NO binding: the shared 'bunker use' default is KEPT (convenience) and announced

── READ-ONLY: status (no --server)
   cmd:  bunker status
   exit: 0
     | bunker: reading server "karahermes-mde-7840hs"
     | ── karaHermes-mde-7840hs ──
     |   Hostname: karaHermes-mde-7840hs
     |   URL:      http://localhost:10001
     |   Version:  0.1.4
     |   Status:   ONLINE
     |   Uptime:   5d 17h 26m
     |   Agents:   0/20
     |   /tmp:     HOST-SHARED — agent sessions see the host /tmp
     | 
     |   ╔══════════════════════════════════════════════════════════╗
     |   ║  ⚠  WARNING: Private /tmp is NOT active on this host.           ║
     |   ║  Agent exec sessions share the host /tmp; the README's        ║
     |   ║  'Private /tmp per agent' promise does not hold here.         ║
     |   ╚══════════════════════════════════════════════════════════╝
     |   Reason:   the pam_namespace drop-in configuration is missing
     |   Residue:  0 orphan users, 4 orphan homes, 13 orphan keys, 2 stale linger entries (0 registered agents)
     |             residue present: this host holds agent users/homes/keys/linger entries with no registered agent behind them
     |   CPU:      663.9%
     |   Memory:   23.0 GB / 59.6 GB
     |   Disk:     48% (878.4 GB/1.8 TB)
   VERDICT: PASS (announced: bunker: reading server "karahermes-mde-7840hs")

── READ-ONLY: list   (no --server)
   cmd:  bunker list
   exit: 0
     | bunker: reading server "karahermes-mde-7840hs"
     | No agents found.
   VERDICT: PASS (announced: bunker: reading server "karahermes-mde-7840hs")

## (b) BUNKER_SESSION_TARGET binding on the live demo server

── READ-ONLY: list (BUNKER_SESSION_TARGET=bunker-mvp)
   cmd:  BUNKER_SESSION_TARGET=bunker-mvp bunker list
   exit: 0
     | bunker: reading server "bunker-mvp"
     | 
     | ══════════ Agents ══════════
     | 
     |   Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
     |   ────────       ──────     ─────────      ─────────────  ───────                   ──────────
     |   95f3325a       destroy-refused:live_processes 6.2 GB         20.0 GB        2026-09-27T14:15:46Z      (no URL)
     |   9b4fe54d       destroy-refused:live_processes 6.5 GB         20.0 GB        2026-09-28T02:51:47Z      (no URL)
     |   fe5ee3ac       destroy-refused:live_processes 9.9 GB         20.0 GB        2026-09-29T12:15:57Z      (no URL)
     |   b88c12af       destroy-refused:live_processes 5.2 GB         20.0 GB        2026-09-29T12:57:26Z      (no URL)
     |   718bf3d4       destroy-refused:live_processes 5.8 GB         20.0 GB        2026-09-27T13:39:31Z      (no URL)
     |   7dfbd5ea       destroy-refused:live_processes 8.5 GB         20.0 GB        2026-09-27T13:50:08Z      (no URL)
     |   a9ebf454       destroy-refused:live_processes 5.7 GB         20.0 GB        2026-09-27T13:39:59Z      (no URL)
     |   da5b7b3c       destroy-refused:live_processes 5.0 GB         20.0 GB        2026-09-29T20:08:04Z      (no URL)
     |   54c8bf55       destroy-refused:live_processes 5.4 GB         20.0 GB        2026-09-29T02:53:38Z      (no URL)
     | 
     | Total: 9 agents (server: bunker-mvp)
     | Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
   VERDICT: PASS (announced: bunker: reading server "bunker-mvp")

── READ-ONLY: status (BUNKER_SESSION_TARGET=bunker-mvp)
   cmd:  BUNKER_SESSION_TARGET=bunker-mvp bunker status
   exit: 0
     | bunker: reading server "bunker-mvp"
     | ── bunker-mvp ──
     |   Hostname: bunker-mvp
     |   URL:      http://78.46.173.180:18080
     |   Version:  0.1.4
     |   Status:   ONLINE
     |   Uptime:   19h 58m 42s
     |   Agents:   9/50
     |   /tmp:     private (per-session pam_namespace instance)
     |   Residue:  1 orphan user, 1066 orphan homes, 1 orphan key, 756 stale linger entries (9 registered agents)
     |             residue present: this host holds agent users/homes/keys/linger entries with no registered agent behind them
     |   CPU:      117.2%
     |   Memory:   1.9 GB / 15.2 GB
     |   Disk:     55% (82.9 GB/149.9 GB)
     |   Docker:   4 containers
   VERDICT: PASS (announced: bunker: reading server "bunker-mvp")

## (c) explicit --server against the live demo server (operator-stated target)

── READ-ONLY: status --server bunker-mvp
   cmd:  bunker --server bunker-mvp status
   exit: 0
     | ── bunker-mvp ──
     |   Hostname: bunker-mvp
     |   URL:      http://78.46.173.180:18080
     |   Version:  0.1.4
     |   Status:   ONLINE
     |   Uptime:   19h 58m 43s
     |   Agents:   9/50
     |   /tmp:     private (per-session pam_namespace instance)
     |   Residue:  1 orphan user, 1066 orphan homes, 1 orphan key, 756 stale linger entries (9 registered agents)
     |             residue present: this host holds agent users/homes/keys/linger entries with no registered agent behind them
     |   CPU:      117.0%
     |   Memory:   1.9 GB / 15.2 GB
     |   Disk:     55% (82.9 GB/149.9 GB)
     |   Docker:   4 containers
   VERDICT: N/A (--server given explicitly; announcement is the implicit-default guard)

── READ-ONLY: list   --server bunker-mvp
   cmd:  bunker --server bunker-mvp list
   exit: 0
     | bunker: reading server "bunker-mvp"
     | 
     | ══════════ Agents ══════════
     | 
     |   Agent ID       Status     Disk Used      Max File Size  Created                   Public URL
     |   ────────       ──────     ─────────      ─────────────  ───────                   ──────────
     |   9b4fe54d       destroy-refused:live_processes 6.5 GB         20.0 GB        2026-09-28T02:51:47Z      (no URL)
     |   fe5ee3ac       destroy-refused:live_processes 9.9 GB         20.0 GB        2026-09-29T12:15:57Z      (no URL)
     |   b88c12af       destroy-refused:live_processes 5.2 GB         20.0 GB        2026-09-29T12:57:26Z      (no URL)
     |   718bf3d4       destroy-refused:live_processes 5.8 GB         20.0 GB        2026-09-27T13:39:31Z      (no URL)
     |   7dfbd5ea       destroy-refused:live_processes 8.5 GB         20.0 GB        2026-09-27T13:50:08Z      (no URL)
     |   a9ebf454       destroy-refused:live_processes 5.7 GB         20.0 GB        2026-09-27T13:39:59Z      (no URL)
     |   da5b7b3c       destroy-refused:live_processes 5.0 GB         20.0 GB        2026-09-29T20:08:04Z      (no URL)
     |   54c8bf55       destroy-refused:live_processes 5.4 GB         20.0 GB        2026-09-29T02:53:38Z      (no URL)
     |   95f3325a       destroy-refused:live_processes 6.2 GB         20.0 GB        2026-09-27T14:15:46Z      (no URL)
     | 
     | Total: 9 agents (server: bunker-mvp)
     | Disk Used is the agent's measured usage; Max File Size (per-file cap — not a total-disk quota)
   VERDICT: PASS (explicit --server and also announced: bunker: reading server "bunker-mvp")

── READ-ONLY: metrics --server bunker-mvp
   cmd:  bunker --server bunker-mvp metrics
   exit: 0
     | bunker: reading server "bunker-mvp"
     | 
     | ══════════ Server Metrics ══════════
     | 
     |   CPU Usage:      131.84%
     |   Memory Used:    1.8 GB / 15.2 GB
     |   Disk Used:      82.9 GB / 149.9 GB
     |   Docker Containers: 4
     | 
     |   Agent ID       Status     CPU %      Memory
     |   ────────       ──────     ────       ──────
     |   95f3325a       destroy-refused:live_processes 2.0        4.0 GB
     |   9b4fe54d       destroy-refused:live_processes 2.0        4.0 GB
     |   fe5ee3ac       destroy-refused:live_processes 2.0        4.0 GB
     |   b88c12af       destroy-refused:live_processes 2.0        4.0 GB
     |   718bf3d4       destroy-refused:live_processes 2.0        4.0 GB
     |   7dfbd5ea       destroy-refused:live_processes 2.0        4.0 GB
     |   a9ebf454       destroy-refused:live_processes 2.0        4.0 GB
     |   da5b7b3c       destroy-refused:live_processes 2.0        4.0 GB
     |   54c8bf55       destroy-refused:live_processes 2.0        4.0 GB
     | 
     | Total: 9 agents
   VERDICT: PASS (explicit --server and also announced: bunker: reading server "bunker-mvp")

── READ-ONLY: info --server bunker-mvp 95f3325a
   cmd:  bunker --server bunker-mvp info 95f3325a
   exit: 0
     | bunker: reading server "bunker-mvp"
     | 
     | ══════════ Agent: 95f3325a ══════════
     | 
     |   Status:           destroy-refused:live_processes
     |   Created At:       2026-09-27T14:15:46Z
     |   Expires At:       2026-09-27T18:15:46Z
     |   Port Range:       10200-10299
     |   Docker Tunnel:    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -i /home/kara/.bunker/keys/95f3325a -L 2376:/run/bunker/95f3325a/docker.sock bunker-95f3325a@78.46.173.180 -N
     |   SSHFS Mount:      sshfs -o IdentityFile=/home/kara/.bunker/keys/95f3325a -o idmap=user -o allow_other bunker-95f3325a@78.46.173.180:/home/bunker-95f3325a /mnt/bunker/95f3325a
     |   Limits:
     |     CPU Quota:      2.0 cores
     |     Memory Limit:   4.0 GB
     |     Max File Size:   20.0 GB (per-file cap — not a total-disk quota)
     |     Max Containers: 10
     |   Safety Preset:    standard
     |   Safety Knobs:
     |     CPUQuota:       200%
     |     MemoryMax:      4294967296
     |     LimitFSIZE:     21474836480
     |     TasksMax:       4096
     |     LimitNOFILE:    65536:65536
   VERDICT: PASS (explicit --server and also announced: bunker: reading server "bunker-mvp")

## Totals: PASS=8 FAIL=0  (live agent used for the info probe: 95f3325a)
```

The transcript shows the documented asymmetry: the announcement is emitted when the target is
resolved **implicitly** (shared default or `BUNKER_SESSION_TARGET`) — `status` with no binding
prints `bunker: reading server "karahermes-mde-7840hs"` (the shared default, kept) while
`list`/`info`/`metrics` print the line for explicit `--server` too. Explicit `--server` cases are
recorded as `N/A`/PASS rather than failed.

## Local gate (no code change was required)

```
build_rc=0
vet_rc=0
test_rc=0      # 29 packages ok, 0 FAIL lines, 36 package lines
```

Targeted keystone tests:

```
=== RUN   TestSessionScopedTarget_FlagWins
--- PASS: TestSessionScopedTarget_FlagWins (0.00s)
=== RUN   TestSessionScopedTarget_EnvSecond
--- PASS: TestSessionScopedTarget_EnvSecond (0.00s)
=== RUN   TestSessionScopedTarget_NeverFallsBackToSharedDefault
--- PASS: TestSessionScopedTarget_NeverFallsBackToSharedDefault (0.00s)
=== RUN   TestReadOnlyTarget_KeepsConvenienceDefault
--- PASS: TestReadOnlyTarget_KeepsConvenienceDefault (0.00s)
=== RUN   TestReadOnlyTarget_EnvBeatsShared
--- PASS: TestReadOnlyTarget_EnvBeatsShared (0.00s)
=== RUN   TestMutatingCommandsCannotReadActiveServer
--- PASS: TestMutatingCommandsCannotReadActiveServer (0.00s)
PASS
ok  	github.com/deployBunker/bunker/internal/cli	0.114s
```

## Cleanup / leak verification

```
NRestarts=0
ExecMainPID=2016609
ActiveState=active
attempt 1: ExecMainPID=485593  port :28070 held by pid=485593
ExecMainPID=485593
ActiveState=active
NRestarts=0
ExecMainPID=2016609
ActiveState=active
scratch ports 28080/29090 closed
NRestarts=0
ExecMainPID=2016609
ActiveState=active
NRestarts=0
ExecMainPID=2016609
ActiveState=active
bunker-gap093-iso-a: absent
bunker-gap093-iso-b: absent
no gap093 ssh keys left
no gap093 agent homes left
```

The scratch daemon was stopped, `gap093-scratch.service` reset, `/var/lib/bunkerd-gap093`,
`/.config/bunkerd` (the scratch daemon's relative secret dir), the staged `bunkerd`/`bunker-cli`
binaries, both session scripts and the token-bearing staged configs removed; the two test agents'
own rollback archives (`/var/backups/bunker/bunker-gap093-iso-{a,b}-*.tar.gz`, 98 MB each) were
removed too. The live daemon kept its `ExecMainPID` and `NRestarts` across the whole run: the run
did not restart, stop or reconfigure the demo daemon, and left no `gap093` agent, user, home,
SSH key or linger entry behind.

## Residual observations (no code change made)

1. **`env` disables flag parsing** (`DisableFlagParsing: true`; only `--server`/`--timeout` are
   accepted): `bunker --config X env set ...` is rejected by design with `env takes no flags
   (got "--config")`. The `env` cases therefore point at the same spy config through
   `BUNKER_HOME`. Both `env set` and `env unset` refused correctly. Recorded so a later reader
   does not mistake the rejected invocation form for a binding failure.
2. **The static keystone's file list is 14 files**
   (`TestMutatingCommandsCannotReadActiveServer`: cp, deploy, destroy, env, exec, heartbeat,
   mount, restart, run, spawn, ssh, start, stop, tunnel) while `SessionScopedTarget(` is called
   from 17 files — `keys.go`, `renew.go` and `surface.go` are not named by the guard. Test A
   exercises those verbs live (`renew`, `key rotate`, `surface install`, `surface remove`) and all
   four refused, so the live half covers what the static list omits; the drift itself is worth a
   follow-up row rather than an in-tick change.
3. **Pre-existing audit-chain break on the live host**:
   `audit chain-head recovery skipped error="/var/log/bunkerd/audit.log.3: record 5: prev_hash
   does not chain (tampered)"` — the record is from **2026-09-27T08:36:57Z** in a rotated file,
   i.e. it predates this test's scratch daemon (2026-09-30T02:12Z) and is not caused by this run.
   The scratch daemon was given its own `audit.path` precisely so this test could not touch the
   live hash chain; the live `audit.log` received session A's records only.
4. **CI/root-suite overlap, disclosed**: the self-hosted runner on `bunker-mvp` was executing
   `go test -count=1 -run TestSpawn|TestCgroup|TestConcurrency ./...` for the whole window and
   leaking its own `bunker-*` users (visible in the census: `bunker-gap075-a`,
   `bunker-imgspec-none`, `bunker-{f945c2c9,tmpdir-env-*}`). No Go test in this repo sweeps host
   users (grep-verified), the scratch daemon's pool (`21000-21999`), ports (`:28080/:29090`),
   audit path and base data dir are disjoint from both the live daemon (`10000-19999`) and the
   coexist battery (`30000-30999`, `:28081/:29091`), and the live daemon was re-listed after the
   scratch boot. Those leaked users are the pre-existing CI/test-spawn class and were NOT
   cleaned (another actor's live users are out of scope for this row).
5. **The scratch daemon shared one host-level directory it did not have to**: destroy archives
   go to `agent.destroy_archive_dir` (default `/var/backups/bunker`), which the scratch config
   did not override, so its `destroy` wrote there and the keep-20 prune removed two OLDER
   archives belonging to other actors' test agents
   (`bunker-gap075-a-20260930T022255Z.tar.gz`, `bunker-probeok-88364-20260930T022126Z.tar.gz`).
   Both were disposable rollback tarballs of already-destroyed test agents and the prune is the
   documented keep-limit behaviour, but a scratch/coexist daemon on a shared host should set its
   own `destroy_archive_dir` — worth a row for the coexist battery recipe.
6. **A port collision can silently substitute the wrong daemon for the thing you are testing.**
   This session's first isolation attempt bound `:28080/:29090`, which is exactly
   `regression-tests.sh`'s default (`BUNKERD_REST_ADDR=:28080`, `BUNKERD_GRPC_ADDR=:29090`) while
   a CI regression run was live on the host: the scratch unit failed to start, CI's daemon held
   the port, and the session-B leg was therefore testing the WRONG daemon (it showed the
   registry-less host-wide agent enumeration and a spawn port range from CI's `20000-20999`
   pool). The run was discarded and re-executed on free ports with a readiness assert
   (`ExecMainPID` must equal the pid holding the port). Same class as the fleet's documented
   concurrent-battery contention: assert the identity of the process under test, do not infer it
   from a successful bind.

## One-command re-verification

```sh
cd <repo> && make build && ./bunker version                       # commit: 2a23f88
git merge-base --is-ancestor 41d27c8 HEAD && echo CODE-HALF-IN-MAIN
go build ./... && go vet ./... && go test ./... -short            # all rc=0
```

## Marker

**VERIFY-GAP093-LIVE-PASS** — criteria 1-4 re-proved against live daemons on `bunker-mvp` with a
CLI built from HEAD; zero leaked agents/users left behind by this run.
