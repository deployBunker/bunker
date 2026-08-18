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
