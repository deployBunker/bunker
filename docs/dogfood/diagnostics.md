# Bunker Diagnostics — how it's built, the errors hit, the right way

This is the diagnostic trail for Bunker (a multi-agent hosting platform): how the system is put together, why, the errors encountered during real use (2026-08-03 dogfood run), and the right way to do things. Written so a future agent can answer "does this work / how do I use it" without re-deriving everything.

## 1. What Bunker is and how it's built

**Architecture in one paragraph:** `bunkerd` (daemon, runs as root on a Linux host) exposes `Bunkerd`/`Agent` gRPC+REST services via connect-go on two ports (default REST :18080, gRPC :19090 on the live MVP; :8080/:9090 in docs — the README's ports don't match the deployed server, another small doc drift). `bunker` (cobra CLI) talks to it over HTTP/2. Each agent = a real Linux user (`bunker-<id>`) with its own home, SSH keypair, a rootless dockerd (via `dockerd-rootless-setuptool.sh`), a port range (e.g. 10000-10099), and cgroup limits enforced through a systemd user-slice drop-in (`/etc/systemd/system/user-<uid>.slice.d/50-bunker.conf`). `bunker exec` runs commands via SSH as that user; env vars live in `/run/bunker/<id>/env` and are sourced at the start of every exec.

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

**THE known landmine (DOGFOOD-001):** `remoteCmd` is joined **without quoting**. If the CLI sends `Command:"sh", Args:["-c", "<snippet>"]` (which `bunker env *` does), the remote dash parses `... sh -c if [ -f '...' ]; then ...` — `if` becomes an argument to the inner `sh -c`, `then` is orphaned → `Syntax error: "then" unexpected`. Same mechanism produces `-f: 1: [: missing ]` (from `sh -c '['`), and the gawk usage error on `env get`. **The right way:** the server must quote the snippet when joining (`sh -c '<snippet>'` with proper escaping), or the CLI must stop self-wrapping and pass args through for the server to join (a plain `bunker exec <id> -- docker run --rm alpine echo hello` works because all words are separate args and the first is a real binary).

**How to test without a full server:** the failure reproduces with any compound snippet through exec:
`bunker exec <id> -- "if [ -f /etc/hostname ]; then cat /etc/hostname; fi"` → fails. `"[ -f /etc/hostname ] && cat /etc/hostname"` → works.

## 3. The SSH-command landmine (DOGFOOD-002)

`cp`/`deploy`/`tunnel`/printed-bundle commands all assume (a) the server's self-reported hostname resolves on the client, and (b) the server-side key path `/etc/bunkerd/ssh/<id>` exists on the client. On the server host itself both are true → the E2E battery passes. From any real client they fail with `Could not resolve hostname` / `Identity file ... not accessible`. **The right way:** derive the SSH host from the configured server URL (IP) with a `--ssh-host` override, and always use the client-side key `~/.bunker/keys/<id>` that spawn saves. This cluster is why "E2E on-server green" ≠ "works for users" — the battery's blind spot is the client side of the SSH features.

## 4. Validation gap (DOGFOOD-003)

`SpawnAgent` TTL: invalid durations fall through to the default instead of `CodeInvalidArgument` (spec: specs/api.md says bad TTL → CodeInvalidArgument). TTL parsing lives server-side; `time.ParseDuration` should be called and errors mapped to `connect.CodeInvalidArgument`.

## 5. What a failed spawn leaves behind (verified — it's clean)

Spawn order: allocate port range → create user → start dockerd → write keys. If `useradd` fails (e.g. non-root): port range is freed (next agent got the next range), no tracker record persists, no /run/bunker dir. Restart of bunkerd after failed spawns shows 0 agents. Destroy order (from specs/api.md): kill tunnels → stop dockerd unit → userdel -r → rm /run/bunker/<id> → free ports.

## 6. Environment facts gathered during the run (for future runs)

- Live MVP: 78.46.173.180, REST :18080, gRPC :19090, SSH :22, **auth disabled** (empty token works), hostname `bunker-mvp`, max 50 agents, disk ~6-8% used.
- Server-side E2E: `bash e2e-full-battery.sh` on the server (needs root; creates/destroys `bunker-e2e-*` users). Token in that script is the test token.
- CLI config: `~/.bunker/config.yaml` (servers map, active_server). Keys in `~/.bunker/keys/<id>`.
- cgroup limits are on `user.slice/user-<uid>.slice/` — read `memory.max`, `cpu.max`, `pids.max` there; the drop-in is `user-<uid>.slice.d/50-bunker.conf`.
- `bunker status` metrics fields (CPU/Memory/Uptime) are not populated by the server's status path (DOGFOOD-006) — use `bunker metrics` for real numbers.
- Deployed server binary version is not verifiable (`bunker version` shows commit: unknown) — if behavior differs from HEAD, check the server's build timestamp via `ServerInfo` before assuming HEAD is broken.

## 7. How to validate a fix for the dogfood findings (the real-user way)

1. **DOGFOOD-001:** from a client machine (NOT the server host): `bunker env set <id> A=B`, `env list`, `env get`, `env unset`, plus `bunker exec <id> -- "if true; then echo ok; fi"` → all must work.
2. **DOGFOOD-002:** from a client machine: `bunker cp`, `bunker deploy`, `bunker tunnel` against the live server must connect via the configured URL host + client key.
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
