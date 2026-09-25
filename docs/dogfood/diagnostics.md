# Bunker Diagnostics — how it's built, the errors hit, the right way

This is the diagnostic trail for Bunker (a multi-agent hosting platform): how the system is put together, why, the errors encountered during real use (2026-08-03 dogfood run), and the right way to do things. Written so a future agent can answer "does this work / how do I use it" without re-deriving everything.

> **STATUS (2026-08-06, verified live):** DOGFOOD-001, DOGFOOD-002, DOGFOOD-003 are **FIXED and live-verified** — see the per-section notes below for commits. A remote-client re-verification of the full env/cp/deploy/tunnel battery ran 2026-08-06 (GAP-009, tick #231): all PASS, including a follow-up fix (IdentitiesOnly=yes for client ssh/scp, `bunker env *`, `bunker cp`, `bunker deploy`, `bunker tunnel` all work from a remote client with a loaded ssh-agent). The sections below keep the original failure descriptions for history — the "landmine" wording describes the state BEFORE the fixes.

## 1. What Bunker is and how it's built

**Architecture in one paragraph:** `bunkerd` (daemon, runs as root on a Linux host) exposes `Bunkerd`/`Agent` gRPC+REST services via connect-go on two ports (default REST :8080, gRPC :9090 per `config.go` DefaultConfig; the live MVP instance, bunker-mvp, exposes REST :18080 / gRPC :19090). `bunker` (cobra CLI) talks to it over HTTP/2. Each agent = a real Linux user (`bunker-<id>`) with its own home, SSH keypair, a rootless dockerd (via `dockerd-rootless-setuptool.sh`), a port range (e.g. 10000-10099), and cgroup limits enforced through a systemd user-slice drop-in (`/etc/systemd/system/user-<uid>.slice.d/50-bunker.conf`). `bunker exec` runs commands via SSH as that user; env vars live in `/run/bunker/<id>/env` and are sourced at the start of every exec.

**Data/state:** agent records tracked in-memory by the server's tracker; SSH keys persisted under `/etc/bunkerd/ssh/` (server) and `~/.bunker/keys/` (client, at spawn). Port allocation is a server-side allocator over `port_range_start..end`.

**Why it's built this way:** rootless Docker per Linux user gives hard isolation (usernamespaces + cgroups) with zero shared daemon — a compromised agent can't reach other agents' containers. The trade-off: **everything is a shell-out** (useradd, systemd-run, ssh, scp, dockerd-rootless-setuptool) — which is exactly where the sharp edges live (see below).

## 2. The request path (how an exec actually flows) — read this before touching exec/env

```
bunker exec <id> -- <cmd> <args...>
  → CLI: ExecAgent RPC { AgentId, Command: <cmd>, Args: [<args>] }
  → server (internal/server/service.go ExecAgent):
      rec := tracker.Get(agentID)                    // nil → CodeNotFound
      buildAgentExecCommand(agentID, home, cmd, args):
          remoteCmd = cmd + " " + strings.Join(args, " ")
          ". <envFile> 2>/dev/null; env PATH=... DOCKER_HOST=unix://<sock> TMPDIR=... <remoteCmd>"
      buildExecSSHCommand:  ssh ... "sh -c '<wrappedCmd>'"
  → ssh as bunker-<id>@host → dash parses → executes
```

**THE known landmine (DOGFOOD-001):** ~~`remoteCmd` is joined **without quoting**. If the CLI sends `Command:"sh", Args:["-c", "<snippet>"]` (which `bunker env *` does), the remote dash parses `... sh -c if [ -f '...' ]; then ...` — `if` becomes an argument to the inner `sh -c`, `then` is orphaned → `Syntax error: "then" unexpected`. Same mechanism produces `-f: 1: [: missing ]` (from `sh -c '['`), and the gawk usage error on `env get`.~~ **FIXED (tick #192, commits 9896c99 / 5151025 / b96f696 / 956d307 / e6879ab):** the server now (1) shellQuoteSingle-quotes every arg, (2) wraps the joined command in `sh -c '<joined>'`, (3) guards the env-file source with `[ -f ]`, (4) uses `set -a` so injected vars reach the child shell. Round-trip unit tests execute the built command through sh; live E2E on bunker-mvp passed 10/10 (env set/get/list/unset, compound snippets, if/then, awk, &&). Re-verified from a remote client 2026-08-06 (GAP-009).

**How to test without a full server:** the failure reproduces with any compound snippet through exec:
`bunker exec <id> -- "if [ -f /etc/hostname ]; then cat /etc/hostname; fi"` → fails. `"[ -f /etc/hostname ] && cat /etc/hostname"` → works.

## 3. The SSH-command landmine (DOGFOOD-002)

~~`cp`/`deploy`/`tunnel`/printed-bundle commands all assume (a) the server's self-reported hostname resolves on the client, and (b) the server-side key path `/etc/bunkerd/ssh/<id>` exists on the client. On the server host itself both are true → the E2E battery passes. From any real client they fail with `Could not resolve hostname` / `Identity file ... not accessible`.~~ **FIXED in two parts:** (1) tick #193 (commit fd282b1) — the client now derives the SSH host from the configured server URL (IP) with a `--ssh-host` override, and uses the client-side key `~/.bunker/keys/<id>` that spawn saves (`internal/cli/sshhost.go`); (2) tick #231 (GAP-009 follow-up) — client ssh/scp/sshfs/tunnel commands now pass `-o IdentitiesOnly=yes` so a loaded ssh-agent (multiple keys) can't exhaust the server's MaxAuthTries before the correct `-i` key is offered (commits: buildSCPArgs + chown paths in `internal/cli/cp.go`/`deploy.go`, sshfs in `mount.go`, tunnel rewrite in `sshhost.go`, server bundle in `internal/agent/manager_spawn.go`). **Live remote-client verification 2026-08-06:** `bunker cp` + `bunker deploy` copied files onto the agent; `bunker tunnel` forwarded the agent's docker socket and `docker run --rm alpine echo TUNNEL-DOCKER-PASS` ran through it (GAP-009 evidence).

## 4. Validation gap (DOGFOOD-003)

~~`SpawnAgent` TTL: invalid durations fall through to the default instead of `CodeInvalidArgument` (spec: specs/api.md says bad TTL → CodeInvalidArgument). TTL parsing lives server-side; `time.ParseDuration` should be called and errors mapped to `connect.CodeInvalidArgument`.~~ **FIXED (tick #194, commit bf1e556):** new `internal/agent/ttl.go` parser accepts `\d+[hmd]` (incl. days), rejects garbage/zero/overflow with `CodeInvalidArgument` at Spawn step 1a; API-key TTL block reuses the parser so agent+key expiry agree. 28-case table test. Live E2E: `--ttl banana` → invalid_argument + 0 agents; `--ttl 7d` → expires exactly +7d.

## 5. What a failed spawn leaves behind (verified — it's clean)

Spawn order: allocate port range → create user → start dockerd → write keys. If `useradd` fails (e.g. non-root): port range is freed (next agent got the next range), no tracker record persists, no /run/bunker dir. Restart of bunkerd after failed spawns shows 0 agents. Destroy order (from specs/api.md): kill tunnels → stop dockerd unit → userdel -r → rm /run/bunker/<id> → free ports.

## 6. Environment facts gathered during the run (for future runs)

- Live MVP: 78.46.173.180, REST :18080, gRPC :19090, SSH :22, **auth enforced** (Bearer token required — 401 without a token, per GAP-014/README), hostname `bunker-mvp`, max 50 agents, disk ~6-8% used.
- Server-side E2E: `bash e2e-full-battery.sh` on the server (needs root; creates/destroys `bunker-e2e-*` users). Token in that script is the test token.
- CLI config: `~/.bunker/config.yaml` (servers map, active_server). Keys in `~/.bunker/keys/<id>`.
- cgroup limits are on `user.slice/user-<uid>.slice/` — read `memory.max`, `cpu.max`, `pids.max` there; the drop-in is `user-<uid>.slice.d/50-bunker.conf`.
- `bunker status` reports live CPU/Memory/Disk/Uptime (DOGFOOD-006 shipped real metrics, tick #197) — CPU% is a delta sample, so the first call shows the baseline and the second call shows the real figure; `bunker metrics` gives the detailed per-agent view.
- Deployed server binary version is not verifiable (`bunker version` shows commit: unknown) — if behavior differs from HEAD, check the server's build timestamp via `ServerInfo` before assuming HEAD is broken.

## 7. How to validate a fix for the dogfood findings (the real-user way)

All three are now FIXED and re-verified live 2026-08-06 (GAP-009, tick #231). The checklist below is the standing regression battery for future changes:

1. **DOGFOOD-001:** from a client machine (NOT the server host): `bunker env set <id> A=B`, `env list`, `env get`, `env unset`, plus `bunker exec <id> -- "if true; then echo ok; fi"` → all must work.
2. **DOGFOOD-002:** from a client machine: `bunker cp`, `bunker deploy`, `bunker tunnel` against the live server must connect via the configured URL host + client key (and must work even with a loaded ssh-agent — IdentitiesOnly fix, tick #231).
3. **DOGFOOD-003:** `bunker spawn --ttl banana` must error with CodeInvalidArgument, not create an agent.
4. Always destroy scratch agents afterwards (`bunker destroy <id> --force`) and confirm `bunker list` is back to 0/expected.

## 8. Where the important code lives

| Concern | File |
|---|---|
| Exec command construction (THE bug) | `internal/server/service.go` — `buildAgentExecCommand` / `buildExecSSHCommand` (~L372-441) |
| CLI exec RPC payload | `internal/cli/exec.go` |
| env command snippets (client side, correctly quoted — server side mangles them) | `internal/cli/env.go` |
| cp/deploy SCP | `internal/cli/cp.go` / `deploy.go` |
| Spawn/destroy lifecycle, cgroups, rootless docker | `internal/agent/` |
| TTL handling | `internal/agent/` (spawn) |
| REST/gRPC handlers | `internal/server/service.go` |
| Specs | `specs/api.md` (RPC contract), `specs/architecture.md`, `specs/agent-lifecycle.md` |

## 9. Dogfood run 2026-08-18 — current-state snapshot (the right way, verified live)

This section records what a 2026-08-18 real-use run proved, so future readers
don't trust stale claims (including this file's own older sections and the
stale `skills/bunker-usage/SKILL.md` — see DOGFOOD-007). Verified against the
auth-enforced demo server (78.46.173.180) with a real token.

### How the system actually works today

- **Transport:** connect-go dual protocol. CLI talks gRPC to :19090 (demo) /
  :9090 (default). REST is the SAME service at `POST /bunker.v1.Bunkerd/<Method>`
  on :18080 (demo) / :8080 (default), `Content-Type: application/json`,
  `Authorization: Bearer <master-token>`. **The service path prefix is
  `bunker.v1` — guessing `bunkerd.v1` 404s.**
- **Auth:** enforced by default (GAP-011/014). No config → bunkerd refuses to
  start; explicit `auth.enabled: false` prints a warning. Missing/wrong token
  → 401. Client tokens live in `~/.bunker/config.yaml` (server aliases), keys
  in `~/.bunker/keys/<agent-id>`.
- **Agent lifecycle:** spawn creates a Linux user `bunker-<id>`, SSH keypair,
  rootless dockerd, cgroup limits (user slice drop-in), port range; bundle
  printed by CLI. Exec runs `sh -c '<quoted args>'` server-side with the env
  file sourced (`/run/bunker/<id>/env`). Destroy removes user + keys + run dir.
- **Env:** `bunker env set <id> KEY=VALUE` (ONE argument), `env get <id> KEY`,
  `env list <id>`, `env unset <id> KEY`. Persists across exec/run until unset
  or destroy. (Was completely broken 2026-08-03; fixed tick #192.)
- **Detached runs:** `bunker run <id> --detach -- <cmd>` → systemd transient
  unit `bunker-run-<id>-<uuid>`; survives the SSH session.
- **Errors:** connect codes — `invalid_argument` (bad TTL), `not_found`
  (destroy unknown → CLI exits 0 with `Agent <id> not found.`), exec exit
  codes propagate (exit 7 → CLI exit 7).

### Errors hit this run and the right way

1. REST 404 on `bunkerd.v1` → correct path is `bunker.v1.Bunkerd`. Read
   docs/integration.md §3 before REST probing.
2. `env set <id> KEY VALUE` → error; correct is `env set <id> KEY=VALUE`.
3. `spawn --ttl 1h demo-agent` silently ignores the name → use `--agent-id`
   (DOGFOOD-008).
4. README env docs missing → DOGFOOD-009.
5. Demo server v0.1.1 vs HEAD v0.1.2 → DOGFOOD-010.

### The 2026-08-03 "known-broken" list is now ALL FIXED (verify, don't assume)

env (DOGFOOD-001, tick #192), cp/deploy/tunnel (DOGFOOD-002, tick #193),
TTL validation (DOGFOOD-003, tick #194), root docs (DOGFOOD-004), destroy UX
+ exit codes (DOGFOOD-005, tick #196), status metrics (DOGFOOD-006, tick #197),
auth enforcement (GAP-014, tick #236), `go install @latest` (GAP-027, tick #254),
`--version` parity (GAP-045). Anything in the repo claiming these are broken
predates those ticks.

## 10. Dogfood run 2026-08-29 — audit trail + metrics deep-dive (read this before touching metrics/audit)

### How the audit trail works (GAP-047/049/050, all shipped since the 08-18 run)

- `internal/audit/audit.go` — append-only JSONL (`O_APPEND`, mode 0600) with
  per-record SHA-256 hash chaining (each record's `hash` = sha256 of its line
  bytes, `prev_hash` = previous record's hash; chain spans rotated backups
  `.1`–`.3`). Rotation: 5 MB × 3 backups.
- `internal/audit/interceptor.go` — a connect interceptor composed INSIDE the
  auth interceptor (auth outermost): unauthenticated requests are rejected
  before audit sees them, guaranteeing "one record per authenticated RPC".
  Caller identity comes from the Claims the auth interceptor injected into the
  context — the raw token NEVER touches the audit log (verified: master token
  grep = 0).
- Records carry: ts (UTC RFC3339Nano), caller (master / agent:<id> / key:<id>),
  method (full connect procedure path), agent_id (target), remote_addr
  (peer), duration_ms, outcome (ok or connect code), summary, hash, prev_hash.
- `bunker audit list/export` (GAP-050) query the trail LOCALLY (--path) or
  REMOTELY (--server → QueryAudit RPC). Filters: --agent/--method/--since/
  --until/--limit; ANDed. `list` = human table, `export` = raw JSONL.
  Pitfall from SKILL.md: --limit returns newest-N matches but prints
  oldest-first; unparseable ts rows are skipped only under time filters.

### THE GAP (DOGFOOD-012): streaming RPCs are invisible to the interceptor

`ExecAgent` is declared `rpc ExecAgent(ExecAgentRequest) returns (stream
ExecAgentResponse)` (proto/bunker/v1/bunker.proto:22). connect-go interceptors
CANNOT see the request message of a server-streaming RPC — the interceptor's
`WrapStreamingHandler` receives `msg == nil`. `targetAgentID()` then falls
back to `claims.AgentID`, which is EMPTY for master-token callers. Result,
verified live on bunker-las-03:

```
{"ts":"...","caller":"master","method":"/bunker.v1.Bunkerd/ExecAgent",
 "remote_addr":"","agent_id":"","duration_ms":184,"outcome":"ok",...}
```

`GetAgent`/`DestroyAgent`/`HeartbeatAgent`/`RunAgent` (all unary) carry
agent_id fine; `SpawnAgent` carries none (expected — id assigned later) and
`ServerInfo`/`ListAgents`/`QueryAudit` need none. But exec — the single most
forensically valuable RPC — records with an empty agent_id AND empty
remote_addr (the stream conn's peer is available; the interceptor just never
reads it on the stream path). `bunker audit list --agent <id>` therefore
misses every exec on that agent. The right fix: stamp agent_id into the
context in the ExecAgent service handler (service layer sees req.Msg),
read it back in the interceptor; also populate remote_addr in the stream
wrapper. Regression: spawn → exec → destroy, then assert exec records carry
agent_id + non-empty remote_addr.

### THE OTHER GAP (DOGFOOD-011): `bunker metrics` reads the HOST cgroup

- `internal/resource/cgroup.go` `ReadCgroupMetrics()` reads
  `/sys/fs/cgroup/memory.current` + `memory.max` — the HOST ROOT cgroup (or
  falls back to `/proc/meminfo`: MemTotal as limit, MemTotal−MemAvailable as
  used). It never reads `user.slice/user-<uid>.slice/*` for a specific agent.
- `internal/server/service.go:215` `AgentMetrics` fills
  `MemoryUsedBytes` from that host-level read while `MemoryLimitBytes` comes
  from the agent record (`rec.Limits`). Mixed sources → live evidence:
  agent with `--memory 1073741824` reported "Memory Used: 2.4 GB / Memory
  Limit: 1.0 GB" — used > limit, on a healthy idle agent; a 480 MB in-agent
  allocation changed nothing. The CPU percent was 0 despite the load
  (point-in-time cpu.stat can't express percent without deltas — known,
  documented in cgroup.go).
- Right way: per-agent cgroup path is `user.slice/user-<uid>.slice`
  (confirm inside agent: `/proc/self/cgroup` → `0::/user.slice/user-1009.slice/
  session-790.scope`); read `memory.current`/`memory.max` and `cpu.stat`
  deltas there, keyed off the agent record's UID. Keep meminfo as last resort
  only when per-agent files are unreadable.

### Verified-clean list (things that DID work — don't re-litigate)

- Named spawn (`--agent-id` and positional), 300s spawn deadline (49.8s fresh
  spawn was comfortably under; the old 30s would have killed it —
  SPAWN-TIMEOUT-001 was a real bug).
- Rootless Docker 29.7.2 in-agent (`docker run alpine` real workload),
  env visible in exec, cp byte round-trip, run --detach systemd transient
  units, heartbeat TTL extension, destroy 1.96s + idempotent + not_found.
- Audit writes for all unary RPCs with hash chain intact, 0600 mode, master
  token never logged; remote list/export/query surfaces work.
- REST: 401 no-auth / 404 wrong path / 200 auth — consistent across the fleet.

### Errors hit this run (all user-side, all understood)

1. `bunker audit verify --server X` → unknown flag (DOGFOOD-013; verify is
   intentionally local-only — the group help over-promises).
2. `expr $(cat)` inside docker alpine → "non-numeric argument" — my shell
   quoting, not a product bug (used `$((6*7))` → 42 instead).
3. `/sys/fs/cgroup/memory.current` does not exist inside the agent namespace
   — confirms the daemon-side host-cgroup read in metrics is the real source
   (and why DOGFOOD-011's fix must read the user slice from the daemon, not
   inside the agent).

## 11. Dogfood run 2026-09-16 — live-fleet run against las-04 at HEAD 66d4150

**Why this run looks different:** the standard ephemeral host (las-bunker-03) was down
(ssh + daemon both timing out), so the install leg ran inside a throwaway agent created
with bunker itself (spawn --image-spec git+curl → clone → build → smoke → destroy). That
makes this the first run where the product was used to verify its own installability —
and where the CLI/daemon version split was directly observable.

**How the version split shaped the findings.** The CLI was built at HEAD (66d4150) but
las-04's daemon is v0.1.3 (no tag contains the GAP-070/073/075 feature line: durable
registry, audit hardening, per-agent /tmp isolation). Consequences, in order of discovery:

1. `bunker --server X exec <id>` failed with `agent "--server" not found` — NOT a daemon
   issue: exec is the one command with `DisableFlagParsing: true` plus a hand-rolled flag
   peeler that only scans AFTER args[0] (internal/cli/exec.go:201). The global flag
   position every other command accepts becomes the agent-id, and the server returns a
   misleading not_found for a token that never was an id. `--timeout` in the same
   position fails identically. Filed DF-BUNKER-8 (P1). Workaround: `bunker use`.
2. `exec -- ls /tmp` showed the HOST's /tmp (colord/fwupd/polkit systemd-private dirs) —
   README's "Private /tmp per agent" is simply not implemented in any tagged daemon; the
   feature (GAP-075) exists only at HEAD. `run --detach` units use PrivateTmp, so the
   three tmp views (exec / detach / promised per-agent) are all different on this daemon.
   Filed DF-BUNKER-9 (P1): either ship a tag containing 9703082 or make daemons advertise
   their isolation level so the README promise is checkable.
3. Destroy was clean — the systemctl WARN noise class (DF-BUNKER-5) landed at HEAD and
   the new CLI suppressed it; the daemon side was unchanged, proving the CLI-side
   classifier works. The rework attempt for DF-BUNKER-5 landed in-tree DURING this run
   (358a10c appeared mid-session), which is why HEAD moved between build and findings.
4. Audit forensics got one false alarm worth remembering: `audit list --since` silently
   returned "no records" for a window I had computed wrong (future clock skew), which
   looked exactly like "exec records are dropped again". The proof cycle that settles it:
   run a marked spawn→exec→destroy, then immediately audit-grep the marker window. All
   three RPCs were attributed — DOGFOOD-012 stays closed.
5. mount failed 2/2 with a raw `read: Connection reset by peer` while plain ssh and the
   sftp subsystem both succeeded against the same agent — pointing at sshd
   parallel-session limiting on a many-connection agent host, and at a missing
   retry/backoff + actionable-error layer in the mount command (DF-BUNKER-11).
6. The install rehearsal produced the run's cheapest finding: README Prerequisites omit
   `make`, so the documented `make build` path dies with `make: not found` on a minimal
   Go-only host, while the two-command `go build` path (documented only under
   Development) works (DF-BUNKER-12).

**Right way recap for future agents:** build at HEAD, `bunker use` before multi-command
sessions, put exec flags after the agent-id, write detached-job output to $HOME (never
/tmp), verify audit claims with a marked live cycle rather than window arithmetic, and
remember the deployed daemons can lag the docs by a major version — check
`bunker status` Version before interpreting behavior differences as bugs.

## 12. Dogfood run 2026-09-18 — REST-integrator run (read this before writing a client)

**Focus:** the *protocol* surface, not the CLI. A stdlib-only Python client was
written from `docs/integration.md` and drove a full lifecycle against
`bunker-las-02` (v0.1.4, HEAD `967c331`). Findings DF-BUNKER-21..26.

1. **Why this run exists.** Every previous run drove the CLI; the CLI hides the
   protocol. A client that speaks HTTP directly is the actual product promise
   ("gRPC + REST … single binary"), and it is where the docs gaps are visible.
   The unary surface is in good shape (post-DF-BUNKER-19 rewrite of
   `integration.md`): field naming, string-encoded 64-bit numbers, error
   envelopes, auth mapping and pre-side-effect TTL validation all matched the
   doc on the first try.
2. **The streaming wall (DF-BUNKER-21).** `ExecAgent` is server-streaming, and
   the doc's own §2 recipe (`Content-Type: application/json`) answers
   `415` with an **empty** body — no `{"code","message"}` envelope, directly
   contradicting §2's error-promise paragraph. The working shape is connect
   streaming: `application/connect+json`, one length-prefixed envelope per
   message (`[flags:1][len:4 BE][payload]`), chunked response, base64
   `stdout`/`stderr`, `exitCode` present only when non-zero, and a final
   `flags=0x02` trailer. Both spec-shaped end-of-stream endings fail
   (`incomplete envelope: unexpected EOF` / `unmarshal end stream message:
   unexpected end of JSON input`) — omitting the EOS and letting
   Content-Length end the request is the only form that works. A client
   checking only HTTP status sees `200` on those failures.
3. **Unknown fields are invisible (DF-BUNKER-22).** `SpawnAgent {"name": "..."}`
   returns `200` with a random `agentId`; the field is not in
   `SpawnAgentRequest` at all. §5 still advertises `name` (and env vars) — a
   doc-faithful client loses the handle on the agent it just created and only
   discovers it via `404 not_found` on itself. The correct field is
   `agent_id`, and it works (`df0918-named` round-tripped).
4. **Key hygiene is asymmetric (DF-BUNKER-23).** `DestroyAgent` removes its own
   key (`ls /etc/bunkerd/ssh/<id>` gone after the CLI destroy), but the hosts
   carry keys for users that no longer exist: **139/139 on las-03**, 4/7 on
   las-02, 1/1 on the demo host (each checked with `id bunker-<key>`). This is
   the key-material half of the same residue family as GAP-080 (orphan homes +
   linger units): agents that die by TTL expiry or by the daemon's startup
   reconcile leave their private key behind. Re-spawning a *known* id reuses
   the same key name, so the residue is not just litter.
5. **Install leg, fresh host, real numbers.** Ephemeral agent on
   `bunker-las-03` (bare Debian 13): clone 2s, `make build` →
   `sh: 1: go: not found` / `Error 127` (make present, Go absent), then a Go
   1.26.5 tarball install (which prints the `GOPATH and GOROOT are the same
   directory` warning if extracted into `$HOME`) → `make build` **49s** →
   `./bunker --version` = `0.1.4 / 967c331`. Install works; the first step
   fails for anyone without Go, and the repo ships no binaries and no Go
   install instructions (DF-BUNKER-24).
6. **Verifications that matter for future runs.** `AgentMetrics` is
   agent-scoped over REST (401 MB / 8 GB) — DOGFOOD-011 stays closed;
   `QueryAudit` attributes `ExecAgent` with `agentId` + `remoteAddr` including
   REST-streaming calls — DOGFOOD-012 stays closed; the CLI destroy now prints
   `Removed local SSH key` — DOGFOOD-014 stays closed; `DestroyAgent` on a
   *never-known* id is `404` while a *just-destroyed* id is `200 destroyed` —
   the tombstone nuance is undocumented (DF-BUNKER-26).

7. **The spawn-failure chain (DF-BUNKER-21) — the run's most serious finding.**
   A `SpawnAgent` landed on UID 1012, previously used by a deleted agent whose
   `user-1012.slice` was still ACTIVE (3 days) with 145 lingering `bunker-*`
   entries in `/var/lib/systemd/linger` and stale `/run/user/<uid>` dirs around.
   The daemon logged `resetting user manager runtime` → `lazily unmounted stale
   runtime mount` → **5 minutes of silence** → `rolling back: removing user` →
   `rollback userdel failed: context deadline exceeded` → `spawn agent failed:
   ... user manager did not start ...: context deadline exceeded` → HTTP 500,
   154B, in `5m0.001s`. A client on the documented 300s timeout sees only a
   socket timeout. Residue: the `/etc/passwd` entry, `/home/bunker-df0918-ttl`,
   and a fresh linger entry — all invisible to `GetAgent` (404) and to
   `bunker list`. Control: after deleting that residue at the operator level, a
   spawn landing on the **same** UID 1012 succeeded in 21.8s. When a spawn looks
   dead, read `journalctl -u bunkerd` on the daemon host before anything else.

8. **The durability surface (2026-09-19 run) — how the trust promises actually behave.**
   - *TTL reaper mechanics:* a 1-minute ticker in `internal/agent/manager.go`
     scans the tracker for `ExpiresAt` in the past and destroys the agent
     (user, home, keys, ports, registry destroy event). Verified live: `--ttl 2m`
     agent expired on schedule and was reaped on the next tick (~40s later). A
     short-TTL spawn is the cheapest way to prove the whole destroy path
     without touching anything you care about.
   - *Crash replay:* the registry (`agents.jsonl`, append-only, fsync'd appends)
     is replayed before listeners open; `restoreAgent` reinstates each live
     record with its EXACT persisted port range (a record without a range fails
     closed and is force-destroyed — that is `failClosedRestore`). Verified
     with a `kill -9` + restart: `restored:2`, port ranges identical, exec
     working immediately. **The right way to test durability is kill -9, not
     systemctl restart** — restart masks unflushed-state bugs that SIGKILL
     exposes (here: none; the appends are already on disk).
   - *Audit chain internals:* each record hashes its canonical bytes with the
     previous record's hash; `lastHash` lives in the AuditLog struct (memory).
     Rotation keeps the chain (seal record carries the head) but a PROCESS
     RESTART starts a fresh chain with `prev_hash:""` — `bunker audit verify`
     then reports "tamper detected" at the first post-restart record
     (`internal/audit/audit.go`: no chain-head recovery at New). Until fixed,
     `audit verify` output is only meaningful on a daemon that has not
     restarted since the log was created.
   - *Reconcile decision matrix (startup):* registry-live + user present →
     restore; registry-live + user gone → purge (+ scoped key removal);
     user present + registry-unknown → FOREIGN check first (owner marker /
     out-of-pool ports — DF-BUNKER-18), then per mode: destroy → destroy;
     adopt → adopt ONLY if `/home/bunker-<id>/.bunker/ports` is readable and
     the range is in-pool, else WARN + destroy. The adopt precondition is
     the sharp edge: a hand-made `useradd bunker-x` orphan will always be
     destroyed even in adopt mode. To construct an adoptable orphan you must
     write a plausible `.bunker/ports` file first.
   - *Scratch-daemon pattern (the right way to dogfood this project on a
     shared host):* private config in /tmp (own ports, own registry/audit
     paths, own SSH dir), `BUNKER_HOME` pointed at a private CLI home, and
     reconciliation `adopt` for the first restart if foreign `bunker-*`
     users exist on the host (defense against destroying another lane's
     agents by mistake). Verify cleanup by exact-name probes (`pgrep -x
     bunkerd`, `getent passwd`), never `pgrep -f` — the full-command match
     hits your own checking shell and lies twice in one session.
   - *Non-root daemon behavior:* spawn proceeds (ports, dirs) until
     `useradd` needs root, then fails `exit 1` with the rollback path —
     correct, but easy to misread as a port/permission bug if you forgot the
     daemon needs root.

**Right way recap for future agents:** build at HEAD; for anything that needs
command output over REST use `application/connect+json` with envelope framing
and **no** end-of-stream envelope; de-chunk and base64-decode before parsing;
spawn with `agent_id` (never `name`); check `id bunker-<agent>` and
`/etc/bunkerd/ssh/<agent>` on the daemon host after experiments — destroy is
clean, TTL expiry may not be; and treat a `200` on the streaming path as
"transport OK", not "command OK", because protocol errors arrive inside the
stream. For durability claims: prove TTL with a `--ttl 2m` agent, prove crash
safety with kill -9 (not restart), and read `audit verify` with the
restart-breaks-chain caveat in mind. On a shared host, run your scratch
daemon on private ports with a private BUNKER_HOME and `pgrep -x`/`getent`
for cleanup checks.

---

## 13. Dogfood run 2026-09-20 — the isolation boundary + the mount command (read this before touching mount or the scratch point)

**Headline: `bunker mount` has never worked at HEAD, and no test can see it.** Two findings here are worth
internalising as *patterns*, not just bugs.

### 13.1 The double-start trap — a helper that can never succeed, protected by its own test seam

`internal/cli/mount_preflight.go` `runWithTimeout` does `cmd.Start()` and then hands the SAME `*exec.Cmd` to
`cmd.CombinedOutput()` in a goroutine. `CombinedOutput` calls `Start()` internally, so it returns
`exec: already started` synchronously; the goroutine result carries an empty capture and the timer branch is
never reached. The caller reads the empty capture, takes the `details == ""` branch, and emits a **confident,
specific, wrong diagnosis**: "remote path ... does not exist or is not a directory".

**How to spot this class again:** an error message that names a cause must be checkpointed against the bytes
that would prove it. Here the message names a filesystem fact while the function's own capture is empty — the
absence of evidence was rendered as evidence of absence. (The `internal/cli/SKILL.md` pitfall #14 states this
rule; the preflight violates it.) If you are debugging a "does not exist" for something you can see with your
own eyes, log the helper's raw capture before believing the message.

**Why coverage did not catch it — the generalisable part.** The only non-trivial behaviour in this function is
"shell out to ssh and parse the output", and the package stubs exactly that away:

* `internal/cli/mount_test.go:230` — `remotePathCheck = func(...)` stub, with the comment "the mount preflight
  shells out to a real host; stub it here so every caller exercises the mount path rather than failing at
  preflight". The stub is reasonable; the gap is that nothing anywhere executes the real function.
* `internal/cli/proc_lifecycle_test.go:120` — sets `BUNKER_SKIP_MOUNT_PREFLIGHT=1` for the subprocess CLI.

So the *seam* is tested and the *implementation* is not. **The fix pattern:** this function's only external
dependency is the `ssh` binary name, so a test can put a fake `ssh` on `PATH` (a two-line shell script) and
exercise the real code path end-to-end. When a seam exists purely because something "shells out to a real
host", ask what a fake executable on `PATH` would cost — usually less than the seam.

**Diagnostic tool that found it (reuse it):** put a logging shim named `ssh` early on `PATH`
(`/tmp/df1017/shim/ssh` in this run) that records `"$@"`, a copy of stdin, and the exit status, then `exec`s
`/usr/bin/ssh`. The CLI's own invocation appears with an EMPTY stdin while the same command run by hand
carries the probe script — that single observation collapses "maybe ssh is failing" into "the script is never
written". Then reproduce the helper in a ~20-line Go program before touching the repo.

### 13.2 The shared scratch exchange point is provisioned by a command nobody is required to run

`/srv/bunker-share` has **two** halves with different owners:

| Half | Who creates it | Correct shape |
|---|---|---|
| the exchange ROOT | `hostsetup.Options.EnsureSharedScratch`, whose only caller is `internal/hostsetup/status.go:219` → `bunker host-provision --apply` | `2750 root:bunker-agents`, verified by a stat-back that is a hard error |
| one agent's directory | `hostsetup.Options.EnsureAgentScratch` → `internal/agent/isolation.go:209` → **every spawn** | `2770 <agent>:bunker-agents`, tmpfs `size=<cap>` |

The spawn path assumes the root is already right. It is not on a host that never ran `host-provision --apply`,
and the failure is invisible from the daemon's side: `EnsureAgentScratch` logs `"shared scratch ready"`, the
per-agent tmpfs mounts, the cap is enforceable — and the agent still cannot `ls /srv/bunker-share`, because
the *parent* is `750 root:root` and not traversable.

**The rule this violates:** "ready" must mean *usable by the consumer*, not *the last step I ran returned nil*.
A capability whose provisioning is a manual host step, but whose advertisement is automatic, produces exactly
this green-and-broken state.

**How to check any host in one line:**

```bash
ssh <host>-root 'stat -c "%n %a %U:%G" /srv/bunker-share'
# want: /srv/bunker-share 2750 root:bunker-agents
```

Measured 2026-09-20: `bunker-mvp` correct (2750 root:bunker-agents); `bunker-las-01` `750 root:root`;
`bunker-las-02` and `bunker-las-04` absent. So treat a green `shared scratch ready` as unproven until this
stat says 2750.

### 13.3 The isolation boundary itself WORKS — the verified-clean list (do not re-litigate)

Measured live at HEAD `93d7a53` against `bunker-las-03` (daemon 0.1.4 `6a6ad20`, `isolation-grant`):

* **G3 (detached-unit `/tmp`) — confirmed for the first time, and it is the sharpest demo of the design.**
  An `exec` session and a `run --detach` unit on the SAME agent see DIFFERENT `/tmp`s:
  `exec -- sh -c 'echo m > /tmp/shell-marker; cat /tmp/host-marker.txt'` reads the host's `/tmp` (the host
  marker is visible, `ls /tmp | wc -l` ≈ 1219); the detached unit sees `ls /tmp | wc -l` = **1**, cannot see
  `shell-marker`, and cannot see the host marker. Two namespaces, one agent, no shared tmp. Note this is the
  *unprovisioned-host* behaviour — private per-session `/tmp` (pam_namespace) had not been applied on las-03,
  so the session path was HOST-SHARED while the unit path was already private.
* Agent-to-agent `/tmp` isolation is real: agent B gets `Permission denied` reading agent A's `0600` file and
  cannot create over the same path; A's value survives B's attempt unchanged.
* The per-agent scratch **cap** is a real kernel bound: a 300 MiB write into the 256 MiB directory stops at
  exactly `268435456` bytes with `df` at 100%.
* `metrics`/`info`/`heartbeat` on a never-spawned id all return `not_found` — the DF-BUNKER-28 fabricated-record
  defect is genuinely fixed at HEAD.
* `audit list --server` carries `Caller` + `Agent` per record and its `--agent` filter resolves; `not_found`
  RPCs are recorded too.

### 13.4 Where to point the next run

The mount chain is now the highest-value surface: `DF-BUNKER-38` (P0) fixed, GAP-112's live proof (phantom
transport, reconnect, empty-tree refusal) becomes meaningful — and it has never passed. The scratch root is a
one-line host check per box (§13.2) and should be part of the fleet inventory rather than discovered by a
dogfood run.

## 14. Dogfood run 2026-09-22/23 — the key lifecycle and the three-instance auth split (read this before touching auth)

This run picked up the GAP-132 key-lifecycle surface the day after it landed, because
GAP-139b asked for live proof on the demo daemon. The run's value is less "does it work"
than "what is the auth model, actually" — because the surface forced the question.

14.1 How auth is really wired. server.go builds three independent auth objects from the
same config values: the service struct's `s.jwtAuth` (line ~227), the Bunkerd-service
master-only interceptor (~228), and the Agent-service interceptor (~229). Nothing shares
mutable state between them by design — the interceptors are plain values built at boot.
That is a fine architecture for stateless validation, and it is exactly why GAP-132's
"rotate the secret live" feature cannot work as shipped: `RotateJWTSecret` mutates the
one instance that never validates a request. The lesson is general: whenever a feature
mutates security state at runtime, the FIRST review question is "which object does the
enforcement path actually read, and is it the same object?".

14.2 Why the tests could not see it. gap132_test.go drives the service methods directly
and checks the audit log; the mock servers in cli tests echo the RPCs. No test sends a
post-rotate request through `bunkerv1connect` handlers with the real interceptor chain.
The green suite was honest about what it tested — the integration was simply never
tested. The cheap missing test: build the real handler stack (NewBunkerdHandler +
NewAgentHandler with their interceptors), rotate via the RPC, send one request signed
with the old secret and one with the new, assert 200/401 at both endpoints. That test
fails today and would have caught this before deploy.

14.3 The three credential classes and where each breaks. (1) The static master token is
a bearer token that bypasses JWT machinery entirely — rotate does not touch it, and its
validity confused two consecutive verification passes into thinking overlap worked.
Probing `key list` after a rotate proves nothing about JWTs. (2) Agent sub-keys are
opaque tokens validated by the apikey manager ONLY on the Agent service; on Bunkerd
endpoints the master-only interceptor denies them with "agent-scoped tokens are not
allowed" — a 401 that is BY DESIGN even for a perfectly live key. Two runs (this one and
the interrupted 09-22 session) initially misread that 401. The discriminator is the
message body: scope-denial = key live, "invalid token" = key revoked/unknown. (3) JWTs
are what rotate governs, and the only way to observe the rotation today is to mint one
yourself with the candidate secret — which is precisely how the P0 was proven.

14.4 The error trail worth keeping. The CLI's rotate output says "restart bunkerd to
load it", which reads as if the operator must persist the secret for anything to change;
in reality the service instance switches immediately, the interceptors never do, and a
restart loads whatever is in /etc/bunkerd/jwt_secret (rotate does not write it). A
dogfood run that pasted the output into its CLI config locked itself out mid-run and
recovered from a config backup — the recovery path (the old config backup, or
`auth: token:` on the daemon host) is the one to document, because the failure mode will
recur for every operator who reads the output the natural way. Also: the boot secret
file is one rotation behind on purpose (rotate is not persistent) — do not "fix" that by
writing the rotated secret from the RPC handler without deciding the restart story,
because a crash-looping daemon would then burn through secrets irreversibly.

14.5 Right way to verify a change on this surface. Two ports of call, one port number:
the Bunkerd service and the Agent service live behind different interceptors on the same
listener. Any auth-matrix test needs both. And when a probe returns 401, capture the
the BODY before concluding — this run found three distinct 401s (missing header, invalid
signature, agent-scoped-denied) that mean completely different things.

## 15. Remote Dev Workflow (2026-09-23)

### 15.1 How the exec/run path works

`bunker exec` and `bunker run` both resolve the agent's SSH connection details from the
bunkerd API (user `bunker-<id>`, host from server URL, key from `~/.bunker/keys/<id>`)
and execute the command via SSH. On las-03 (no container mode), exec runs directly as
the agent user via SSH — `/tmp` persists across calls, the home directory persists, and
the only ephemerality is the agent's TTL. On image-spec agents (dedi-2), exec runs
`docker run --rm` per call, so `/tmp` and installed tools evaporate (SURF-012).

### 15.2 The env file and the PATH override

`bunker env set` writes KEY=VALUE lines to `/run/bunker/<id>/env` on the agent host.
Every `bunker exec` and `bunker run --detach` sources this file before executing the
command. GOPATH, GOCACHE, APP_MODE and other vars correctly persist across calls.

PATH does NOT persist as set — the SSH session's PAM/profile setup unconditionally
sets PATH to `/home/bunker-<id>/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`,
overriding whatever the env file contains. This is because the env file is sourced
BEFORE the profile runs, or the profile's PATH assignment is unconditional. The fix
is to source the env file AFTER the profile, or to prepend the user's PATH.

### 15.3 The mount preflight and the CLI version gap

The mount preflight (`remotePathExists` in mount_preflight.go) sends a shell script
via `ssh ... sh -s` to verify the remote path exists. The runWithTimeout function
was fixed in DF-BUNKER-38 (it used to call Start+CombinedOutput on the same Cmd).
The default-path resolution was fixed in DF-BUNKER-39 (653d763).

Both fixes are in HEAD (945cd5c) but NOT in the released CLI binary (00c3555,
2026-09-20). The released CLI's mount command fails with "no remote path to check"
(default form) or "does not exist or is not a directory" (with --path). Building
from HEAD and using the fresh binary fixes both issues immediately.

### 15.4 The detached run that isn't a systemd unit

`bunker run --detach` starts a process via SSH that gets orphaned to PID 1. The CLI
prints a "Unit: bunker-run-<id>-<uuid>" name, suggesting it's a systemd transient
unit, but `systemctl --user list-units` shows no such unit. The process runs in a
session scope, not a service unit. The unit name is cosmetic — there's no systemd
management possible.

### 15.5 The Docker tunnel: the right way

`bunker tunnel <agent>` opens an SSH tunnel from local port 2376 to the agent's
rootless Docker socket at `/run/bunker/<id>/docker.sock`. Once the tunnel is up,
`DOCKER_HOST=tcp://localhost:2376 docker <cmd>` works exactly like a local Docker.
Tested: version, ps, run hello-world — all succeed. The tunnel is a foreground
process (Ctrl-C to stop).

## 16. Dogfood run 2026-09-24 — agent-tools delivery + lifecycle (read this before touching tool delivery or stop/start/restart)

**How tool delivery actually works (and why the refusal exists).** `bunker
agent-tools <id>` probes the AGENT (not the client) for toolsd/rg/git/jq/gopls
and prints a table — missing tools are DATA, exit 0. `--install` ships the
vendored toolsd onto the agent's `$HOME/bin` (already on the exec PATH) and
re-probes; it REFUSES a dynamically linked binary, because it would depend on
the control host's libc and die on the agent. That refusal is correct and its
error names the fix: build static via `make dist` (CGO_ENABLED=0) in the
toolkit repo. Proven live: the refusal fired on the dev toolsd, the dist
static build delivered clean (21.9s), the agent ran the delivered binary, and
a file written pre-stop survived stop→start→restart. The gap is coverage, not
design: rg and gopls are named REQUIRED by the same probe that cannot install
them on SSH-based agent classes (no image-spec path there) — DF-BUNKER-57.

**The lifecycle commands mean what they say.** stop = SIGSTOP-equivalent state
preservation (files intact, exec fails with `failed_precondition: agent_stopped`
naming the resume command, exit 1); start resumes; restart ALSO grants a full
default TTL, not the agent's remaining TTL — a 2h agent came back with ~4h
(measured 16:37→20:38 -07). If you meant "just cycle it", budget for the extra
lifetime. `restart` output prints only the new expiry (not the agent table).

**Host maintenance is deliberately local-only and dry-run-first.** `homes` and
`linger` inspect THIS host, not the wire protocol (root only on the daemon
host; they are control-host tools when the control host owns /home). Both
classify stale-vs-kept by user existence, refuse to prune anything inconclusive,
and demand `--dry-run` first by convention (prune --dry-run works and prints the
would-remove set). On this control host: 1 stale home (bunker-media-hermes),
2 stale linger entries — left unpruned (dogfood does not run destructive
commands; the tool surfaced them correctly).

**The error I hit and the right way around it.** First `--install` failed
rc=1 in 0.03s: "refusing to deliver ... DYNAMICALLY linked ... Build it
statically (`make dist` in the toolkit repo sets CGO_ENABLED=0)". The wrong
next step is hunting for a static binary on PATH (the dev toolsd is dynamic);
the right one is the dist/ directory of the tools repo
(~/coding-hermes-tools/dist/toolsd-linux-amd64), which exists precisely for
this. A release asset carrying that static build would make the whole flow
one command (DF-BUNKER-57's fix direction covers it).

**Verify-only scripting note (this run's own trap).** `bunker registry` (and
its unknown subcommands) print help and exit 0 — never script against a bare
`registry` invocation as a success signal. See DF-BUNKER-58.

Evidence: docs/dogfood/2026-09-24-agenttools-lifecycle.md (live command/time
table), board rows DF-BUNKER-57/58, .coding-hermes/dogfood-log.md.

## 17. Dogfood run 2026-09-25 — the raw-REST integrator surface (read this before writing a non-CLI client)

**How to talk to bunkerd without the CLI.** Unary RPCs: `POST
/bunker.v1.Bunkerd/<Method>` with `Content-Type: application/json`, proto
snake_case field names IN, protojson camelCase OUT, int64 fields as JSON
STRINGS (`uptimeSeconds:"71420"`), 32-bit/floats as numbers, zero-valued
scalars omitted. Errors are `{"code","message"}` envelopes with the connect
code in string form; an unknown path is a plain-text router 404 (not JSON);
a GET is 405 before auth runs. The only streaming RPC is ExecAgent: it needs
`application/connect+json` with `[flag:1][len:4 BE][protojson]` envelopes in
AND out (responses arrive HTTP-chunked, so de-chunk before walking envelopes),
stdout/stderr base64, exit code absent when 0, 0x02 trailer ends the stream,
and RPC errors arrive INSIDE the 200 — checking the HTTP status alone lies.
Both recipes were validated live against bunker-mvp and left as runnable
scripts in docs/dogfood/2026-09-25-rest-probes/.

**How the CLI gets the agent SSH key (GAP-128) — and how it broke.** Spawn
responses carry NO key material; the CLI follows up with GetAgentKey (master
gated) and writes ~/.bunker/keys/<id>. On 09-25 that follow-up sent NO
Authorization header (internal/cli/spawn.go sets the header only on the
SpawnAgent request), so every spawn on an auth-enforced daemon warned
"unauthenticated: missing Authorization header" and saved no key — while raw
REST with the same token got 200 and the 411-byte key. Proven with a
standalone Go repro (WITH header → 200, WITHOUT → 401) and filed as
DF-BUNKER-59. Lesson: when a CLI composes TWO RPCs for one user operation,
each request must carry its own credential — connect does not inherit headers
across requests on the same client. The operator workaround until fixed:
`POST /bunker.v1.Bunkerd/GetAgentKey {"agent_id":...}` with the master token,
write the returned sshPrivateKey to ~/.bunker/keys/<id>, chmod 600.

**Fresh-machine reality check (the run's inversion).** The INSTALL path was
fine — real public clone 2s, scripts/install.sh 3s, make build ~60s on a bare
agent — but `make test-short` was RED: two host-provision uninstall tests
write /etc/pam.d/sshd.bunker-tmp for real and fail as non-root
(DF-BUNKER-60). The suite that "proves fresh machines" only passes as root.
Second inversion: `bunker destroy` deadline_exceeded 3× on an agent that had
run a build, while raw REST destroyed the same agent in seconds
(DF-BUNKER-61) — the CLI deadline is the constraint, not the daemon.

Evidence: docs/dogfood/2026-09-25-rest-credential-surface.md (full report +
transcripts), docs/dogfood/2026-09-25-rest-probes/ (runnable scripts), board
rows DF-BUNKER-59/60/61 + PERF-002 (events 752-755), .coding-hermes/dogfood-log.md.

## 18. Dogfood run 2026-09-25b — the TLS trust surface (read this before touching TLS, TOFU pinning, or the insecure knob)

**How this surface is built (the why):** GAP-127 replaced `--tls-insecure` with
SSH-style trust on first use. The CLI observes the daemon's leaf certificate on
a real TLS handshake, prints its sha256 fingerprint loudly, and stores it as
`cert_pin` on the server entry (`~/.bunker/config.yaml`); every later command
re-verifies the leaf against that pin. GAP-141 then made `tls_insecure` a
**two-key act**: the knob alone is refused; `BUNKER_ALLOW_TLS_INSECURE=1` must
be in the environment for EVERY invocation (connect and each later command), a
pinned entry refuses insecure dialing outright (ack or not — the pin wins), and
the CLI declares itself with `X-Bunker-TLS-Unverified: 1` so the daemon stamps
`[TLS-UNVERIFIED]` on the RPC record AND the correlated `/command` record. The
reasoning is deliberate: a version number cannot prove a capability, and a
silent fallback to skipping verification is the one mistake this surface must
never make. Client side: `internal/cli` TLS dial + pin checks; server side:
`internal/config` (tls.self_signed generation at first boot, cert paths),
`internal/audit/interceptor.go` (header → record summary stamping).

**How it was tested (the nine-arm battery, reproducible):** run a scratch
daemon from a HEAD build on private ports with the README's inline config
verbatim (`tls.enabled: true`, `tls.self_signed: true`), then: (1) fresh-home
TOFU connect — fingerprint printed, pin stored; (2) wrong token — auth fails
(but note the banner still CLAIMS the pin was stored — DF-BUNKER-64a); (3)
re-key the daemon by pointing `tls.cert_file` at a NEW empty dir and restarting
(the daemon generates a fresh self-signed pair — no cert deletion needed) —
every command refuses naming BOTH fingerprints; (4) re-pin deliberately with
`--accept-cert`; (5) `--tls-insecure` without the env — refused; (6) with the
env — INSECURE banner, and the audit log gains `[TLS-UNVERIFIED]` on the RPC
and /command records; (7) hand-set `tls_insecure: true` on a pinned entry —
refused both with and without the ack; (8) mint an already-expired cert with a
10-line Python script (cryptography is enough; no faketime needed) and point
the daemon at it — first-use and TOFU both refuse with notAfter + remedy, and
no pin is stored on refusal; (9) plain http against the TLS port — refused.
Close with `bunker audit verify` over the mixed log (chain OK).

**Errors hit and their fixes (the right way):**
- "kill the daemon" tripped a root-delete approval gate → kill by PID, never
  `pkill`+`rm` under sudo; re-point cert paths instead of deleting certs.
- The bash -lic background wrapper means `kill $!` kills the wrapper, not
  bunkerd — resolve the real PID via `pgrep -x bunkerd` or the listen port.
- Hyperfine on a scratch CLI home failed with "server not found" — the scratch
  home does not know other servers; pass the home explicitly per invocation.
- An expired-cert arm silently "passed" once because the entry still carried
  `tls_insecure: true` from the previous arm — the contradiction refusal fired
  first. Reset the entry between arms; one arm per config state.
- ${PIPESTATUS[0]} after pipes, again (third run in a row this bit the harness).

**What this surface still owes (rows filed this run):** the uid-collision /
reaper-loop interplay (DF-BUNKER-63 — spawn hands out a uid a host-docker
container already runs as; destroy's live-process gate then blocks userdel
forever and the TTL reaper retries unbounded; `--force` does NOT bypass), the
README quick start vs fail-closed target binding (DF-BUNKER-62 — pass
`--server`/`BUNKER_SESSION_TARGET` on every mutating call, `bunker use` does
NOT help), and the refusal-UX edges (DF-BUNKER-64 — status exit-0 on refusals,
banner persistence claim on auth failure, stream-error class for a config
refusal, scheme-mismatch message).

**The one-liner:** TLS TOFU does what the README says, verbatim, in all nine
arms — pin it and forget `--tls-insecure` exists. The new risks this run found
live one layer down, in uid allocation and lifecycle reaping, not in TLS.

## 19. Dogfood run 2026-09-25c — the renewal/identity surface (read this before touching renew, spawn containment, or the destroy gate)

**What this surface IS.** `bunker renew` = destroy + re-spawn on the SAME id,
so every stored path (`/home/bunker-<id>`, the uid, fleet.toml entries,
systemd units) stays valid across a refresh (docs/renewal.md). Three layers
have to agree: the client's pre-flight drift report (RPC), the daemon's
destroy live-process gate, and the re-spawn's containment landing.

**How it works, and why the pieces sit where they sit.** The drift report
scans the CURRENT home (path extracted from the stored sshfs mount line,
`internal/cli/renew.go:25`) for references to the same path — report-only,
never rewrites. The destroy gate reads `/proc/<pid>/status` for the uid so
`userdel -rf` can never orphan a live scheduler daemon (the DF-BUNKER-34
failure that started this). The landing gate re-reads the agent uid's cgroup
after the slice drop-in write+reload because systemd consumes drop-ins
slightly asynchronously (INT-CI-37).

**Errors hit and their fixes (the right way):**
- Renew against a DEPLOYED (older) daemon silently loses both safety layers:
  the drift RPC 404s ("unimplemented") and degrades to one warn line, and the
  destroy gate doesn't exist — the agent gets destroyed WITH live processes.
  Version-check before renewing anything real.
- After ANY successful renew the client's `~/.bunker/keys/<id>` is stale
  (renew never re-fetches the key; spawn does at spawn.go:228). RPC verbs
  work; ssh/cp/mount are dead. Workaround until fixed: refetch via
  GetAgentKey (master-token RPC) and overwrite the key file.
- Scratch HEAD daemon (run 19's pattern) needs `server.grpc_addr/rest_addr`,
  `agent.base_data_dir/ssh_dir/port_range_*`, `auth.token` (not
  master_token), and `BUNKER_ALLOW_TLS_INSECURE=1` on every client call.
  NEVER scratch-spawn on the control host: a local container already runs as
  uid 1001 (the DF-BUNKER-63 collision precondition).
- The destroy gate's remedy ("stop those processes … then retry") is
  unsatisfiable from inside the agent for the rootless-docker session's own
  services (dbus-daemon/pipewire): exec sessions reap their children,
  detached stop/pkill get respawned by user systemd. The gate is only
  passable with host-side root — plan renewals for a maintenance window.
- Standard/hardened spawns on bunker-las-03 currently fail the containment
  landing gate: the code allows 125ms (5×25ms, isolation.go:790) for
  MemorySwapMax=0 to land, systemd 257.13 on this box takes ~125-349ms
  (measured). If spawns fail with "memory.swap.max = \"max\"", it is this,
  not the daemon's config. `--preset open` has no swap bar and spawns fine.

**The one-liner:** the stable-identity idea is real and the drift report
proves itself on first use — but as shipped, renew kills the client's SSH
access, cannot pass the destroy gate on any agent that has ever run
rootless-docker, and on this fleet box cannot even re-spawn a standard agent
because the containment gate's convergence budget is smaller than systemd's
actual landing latency.


