---
name: bunker-usage
description: >-
  How to actually USE the Bunker platform (bunkerd + bunker CLI) — the real
  workflows, the working commands, the previously-broken-now-verified features,
  and the pitfalls that tests don't catch. Load this before doing anything with
  bunker/bunkerd, writing E2E scripts, or triaging agent lifecycle bugs.
  Verified against the live auth-enforced MVP on 2026-08-18
  (docs/dogfood/2026-08-18-integration.md); audit CLI + fleet notes added
  2026-08-29 (docs/dogfood/2026-08-29-integration.md).
version: 1.2.0
category: software-development
---

# Bunker Usage — How to Drive This System For Real

Bunker = multi-agent hosting platform. `bunkerd` (daemon, needs **root**, runs on a Linux host) + `bunker` (CLI, runs anywhere). Each agent is an isolated Linux user with its own rootless Docker daemon, cgroup limits, and port range. Control everything through the CLI or the connect-go gRPC/REST API.

## Entry points

- **Live servers (fleet, 2026-08-29):** public MVP `78.46.173.180` (REST :18080, gRPC :19090, SSH :22; v0.1.3) + Tailscale boxes `bunker-las-01..04`, `cube-las-00`, local `bunker-7840hs` (127.0.0.1:10002). **Auth is ENFORCED everywhere** (since GAP-014): empty/missing token → 401; every call needs `Authorization: Bearer <token>` (stored in `~/.bunker/config.yaml` by `bunker connect`). Switch boxes with `bunker --server <name> status|list|audit`; spawn always targets the ACTIVE server (`active_server:` in config) — check `bunker status` first. Server-reported hostnames only resolve on the server itself.
- **Binaries:** `make build` or `go build -o bunker ./cmd/bunker`, `go build -o bunkerd ./cmd/bunkerd`; `go install github.com/deployBunker/bunker/cmd/bunker@latest` serves the newest TAG (may lag HEAD — compare `bunker version` commit vs `git rev-parse HEAD`, README GAP-058 note).
- **CLI config:** `~/.bunker/config.yaml` (server aliases + active server, incl. token), keys in `~/.bunker/keys/<agent-id>`.
- **Test config for local daemon:** `test-config.yaml` (auth off, /tmp/bunkerd-test, port 9095) — daemon runs as non-root but spawn will fail at `useradd` (root required, undocumented in README).

## The working workflow (verified 2026-08-18)

```bash
bunker connect http://78.46.173.180:18080 --token <token>   # writes ~/.bunker/config.yaml
bunker status                                               # ONLINE, version, agents, real CPU/mem/disk — 0.56s
bunker list                                                 # current agents
bunker spawn --ttl 1h                                       # ~10s → connection bundle (key, ports, TTL, sshfs/tunnel cmds); use --agent-id <name> for a named agent
bunker info <id>                                            # status, expires, limits (CPU 2.0, mem 4 GB, disk 20 GB, 10 containers)
bunker exec <id> -- uname -a                                # isolated user: bunker-<id>
bunker exec <id> -- docker run --rm alpine echo hi          # rootless Docker works (3.4s first pull)
bunker env set <id> FOO=bar                                 # NOTE: KEY=VALUE as ONE arg
bunker env list <id>                                        # FOO=bar
bunker env get <id> FOO                                     # bar (get needs the KEY arg)
bunker exec <id> -- sh -c 'echo $FOO'                       # env visible in exec (sourced at start)
bunker cp /local/file <id>:/tmp/file                        # scp round-trip, byte-identical
bunker metrics <id>                                         # real memory/disk usage
bunker heartbeat <id>                                       # TTL extended (acknowledged + new expiry)
bunker run <id> --detach --name job -- sh -c 'sleep 3; echo done >> /tmp/j.log'   # systemd transient unit, survives disconnect
bunker destroy <id>                                         # 1.9s; server back to prior agent count
```

REST (same surface, JSON over HTTP): `POST http://<ip>:18080/bunker.v1.Bunkerd/<Rpc>` with `Authorization: Bearer <token>` and `Content-Type: application/json` — e.g. `.../ListAgents`, `.../ServerInfo`.

**NEW in v0.1.3 — audit trail (GAP-047/049/050, verified live 2026-08-29):**

```bash
bunker audit list --server bunker-las-03                    # table: ts/caller/method/agent/outcome/summary
bunker audit list --server <box> --agent <id>               # per-agent RPCs (⚠️ misses exec, see pitfalls)
bunker audit list --server <box> --since 2026-08-29T00:00:00Z
bunker audit export --server <box> --since ...              # raw JSONL incl. hash+prev_hash chain
bunker audit verify --path /var/log/bunkerd/audit.log       # LOCAL ONLY (daemon host), no --server
```

Every authenticated RPC is appended (JSONL, 0600) with caller/agent/duration/
outcome; token values are NEVER logged. `verify` checks the SHA-256 chain
across rotated backups (.1–.3).

Spawn ≈ 10s. Limits are real — check them at `/sys/fs/cgroup/user.slice/user-<uid>.slice/{memory.max,cpu.max,pids.max}` (NOT `/sys/fs/cgroup/memory.max`), drop-in at `/etc/systemd/system/user-<uid>.slice.d/50-bunker.conf`.

## Previously broken — now verified (2026-08-18)

All features broken as of 2026-08-03 (tasks DOGFOOD-001..006) are fixed and verified live against the auth-enforced MVP:

| Feature (was broken ≤ 2026-08-03) | Status 2026-08-18 | Notes |
|---|---|---|
| `bunker env set/get/list/unset` | ✅ all work | `env set <id> KEY=VALUE` as ONE argument (no space between KEY and VALUE); `env get <id> KEY` — get/list fail with a clear usage error if the KEY arg is missing |
| `bunker exec <id> -- sh -c '<compound>'` | ✅ works | compound commands, `if`/`for`/`while` no longer syntax-error (DOGFOOD-001 fix holds) |
| `bunker cp` / `bunker deploy` / `bunker tunnel` | ✅ work from a client | hostname/key-path issues fixed; no manual IP/`~/.bunker/keys/<id>` substitution needed anymore |
| `bunker spawn --ttl banana` | ✅ rejected cleanly | `invalid_argument: invalid ttl "banana": must match "[0-9]+[hmd]"` — no more silent 6h agent |
| `bunker destroy <unknown-id>` | ✅ idempotent exit 0 | `Agent <id> not found.` and exit 0 (DOGFOOD-005 fix holds) |
| `bunker status` CPU/Memory/Uptime | ✅ real values | 0.56s; no more zeros/unknown |

## Pitfalls

- **Never run the E2E battery from a client** (`e2e-full-battery.sh` needs root + on-server paths; it creates/deletes `bunker-e2e-*` users).
- **Server-reported hostname ≠ reachable host.** Any raw SSH/SCP/SSHFS command printed by spawn uses `bunker-mvp` and server paths — substitute the real IP and client key. (The `bunker cp` / `deploy` / `tunnel` CLI subcommands are fixed and work from a client directly.)
- **Don't trust "CI green" for SSH features** — the battery runs on the server where hostname+keys resolve. Remote-client behavior is the real test.
- **Auth is REQUIRED** — the MVP has enforced auth since GAP-014; an empty/missing token is rejected (unauthenticated → connect error; no Content-Type → 415; wrong service path → 404). Never commit real tokens — the test token `test-regression-token` (in e2e scripts) is fine for the MVP.
- **REST path is `bunker.v1.Bunkerd`, not `bunkerd.v1.Bunkerd`** — `POST http://<ip>:18080/bunker.v1.Bunkerd/<Rpc>` with `Content-Type: application/json`; a guessed service name 404s, errors are connect codes (`CodeNotFound`, `CodeInvalidArgument`, `CodeUnauthenticated`, `CodeResourceExhausted`).
- **Scratch agents:** always `bunker destroy <id>` after a run (1.9s, idempotent); TTL auto-destroys but don't rely on it. `bunker list` to confirm zero.
- **spawn positional name arg works (fixed DOGFOOD-008)** — `bunker spawn --ttl 30m my-agent` names the agent `my-agent`; `--agent-id` also accepted; ids match `^[a-z0-9-]{1,64}$`.
- **`bunker metrics <id>` memory is HOST-level, not per-agent (DOGFOOD-011, unfixed)** — verified 2026-08-29: 1.0 GB-limit agent showed "Memory Used: 2.4 GB"; a 480 MB in-agent alloc changed nothing. Root cause: `internal/resource/cgroup.go` reads the host root cgroup/meminfo, per-agent limit comes from the agent record. Disk numbers are per-agent; check real limits via `bunker info` or on-host `user.slice/user-<uid>.slice/*`.
- **Audit `--agent <id>` filter misses exec records (DOGFOOD-012, unfixed)** — ExecAgent is a server-streaming RPC; the interceptor never sees its request message, so exec records carry `agent_id:""` and `remote_addr:""`. Filter by `--since` timestamps + `bunker audit export` until fixed.
- **`bunker audit verify` is local-only** — runs on the daemon host (`--path /var/log/bunkerd/audit.log`); `--server` → `unknown flag` (DOGFOOD-013; group help over-promises).

## Right way to validate changes

1. `go build ./... && go vet ./... && go test ./... -short`
2. For exec/env changes: from a CLIENT machine, `bunker exec <id> -- "if true; then echo ok; fi"` and all 4 env subcommands.
3. For cp/deploy/tunnel: from a client machine against the live MVP (IP-based).
4. For spawn/destroy/cgroups: on a root host with the battery, PLUS a client-side lifecycle check.
5. Update `.coding-hermes/tasks.md` + `.gitreins/tasks.yaml`; run `gitreins guard` before commit.

## Where to look when something breaks

- Exec/env weirdness → `internal/server/service.go` (`buildAgentExecCommand` L~372, `buildExecSSHCommand` L~437), `internal/cli/env.go`
- Audit trail gaps → `internal/audit/interceptor.go` (`WrapStreamingHandler` = the msg==nil streaming hole, DOGFOOD-012), `internal/audit/audit.go` (chain/rotation), `internal/cli/audit.go`
- Metrics wrong numbers → `internal/resource/cgroup.go` (`ReadCgroupMetrics` host-cgroup read, DOGFOOD-011), `internal/server/service.go:215` `AgentMetrics`
- SCP/tunnel host/key issues → `internal/cli/cp.go`, `internal/cli/deploy.go`, `internal/cli/tunnel.go`
- Key hygiene after destroy → `internal/cli/destroy.go` (no local key cleanup, DOGFOOD-014)
- Spawn/destroy/TTL/cgroups → `internal/agent/`
- API contract → `specs/api.md`; architecture → `specs/architecture.md`
- Full dogfood evidence + diagnostics → `docs/dogfood/2026-08-29-integration.md` (current, verified), `docs/dogfood/2026-08-18-integration.md` (prior run), `docs/dogfood/diagnostics.md`
