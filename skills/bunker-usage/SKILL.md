---
name: bunker-usage
description: >-
  How to actually USE the Bunker platform (bunkerd + bunker CLI) — the real
  workflows, the working commands, the previously-broken-now-verified features,
  and the pitfalls that tests don't catch. Load this before doing anything with
  bunker/bunkerd, writing E2E scripts, or triaging agent lifecycle bugs.
  Verified against the live auth-enforced MVP on 2026-08-18
  (docs/dogfood/2026-08-18-integration.md).
version: 1.1.0
category: software-development
---

# Bunker Usage — How to Drive This System For Real

Bunker = multi-agent hosting platform. `bunkerd` (daemon, needs **root**, runs on a Linux host) + `bunker` (CLI, runs anywhere). Each agent is an isolated Linux user with its own rootless Docker daemon, cgroup limits, and port range. Control everything through the CLI or the connect-go gRPC/REST API.

## Entry points

- **Live server (MVP):** `78.46.173.180` — REST :18080, gRPC :19090, SSH :22. **Auth is ENFORCED** (since GAP-014): an empty or missing token is rejected, every call needs `Authorization: Bearer <token>`. Connect with `bunker connect http://78.46.173.180:18080 --token <token>` (writes `~/.bunker/config.yaml`). Hostname it reports: `bunker-mvp` (only resolves on the server itself!). The demo server runs binary **v0.1.1** while repo HEAD is **v0.1.2** — no protocol breakage observed (client 0.1.2 ↔ server 0.1.1 works).
- **Binaries:** `make build` or `go build -o bunker ./cmd/bunker`, `go build -o bunkerd ./cmd/bunkerd`; `go install github.com/deployBunker/bunker/cmd/bunker@latest` also verified (GAP-027 fix).
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
- **spawn positional name arg is silently ignored** — `bunker spawn --ttl 1h demo-agent` creates an auto-id agent (`c31d8ee8`-style), not `demo-agent`. Use `--agent-id <name>` for a named agent (DOGFOOD-008).

## Right way to validate changes

1. `go build ./... && go vet ./... && go test ./... -short`
2. For exec/env changes: from a CLIENT machine, `bunker exec <id> -- "if true; then echo ok; fi"` and all 4 env subcommands.
3. For cp/deploy/tunnel: from a client machine against the live MVP (IP-based).
4. For spawn/destroy/cgroups: on a root host with the battery, PLUS a client-side lifecycle check.
5. Update `.coding-hermes/tasks.md` + `.gitreins/tasks.yaml`; run `gitreins guard` before commit.

## Where to look when something breaks

- Exec/env weirdness → `internal/server/service.go` (`buildAgentExecCommand` L~372, `buildExecSSHCommand` L~437), `internal/cli/env.go`
- SCP/tunnel host/key issues → `internal/cli/cp.go`, `internal/cli/deploy.go`, `internal/cli/tunnel.go`
- Spawn/destroy/TTL/cgroups → `internal/agent/`
- API contract → `specs/api.md`; architecture → `specs/architecture.md`
- Full dogfood evidence + diagnostics → `docs/dogfood/2026-08-18-integration.md` (current, verified), `docs/dogfood/2026-08-03-integration.md` (older run), `docs/dogfood/diagnostics.md`
