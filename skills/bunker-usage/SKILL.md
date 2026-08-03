---
name: bunker-usage
description: >-
  How to actually USE the Bunker platform (bunkerd + bunker CLI) — the real
  workflows, the working commands, the known-broken features, and the
  pitfalls that tests don't catch. Load this before doing anything with
  bunker/bunkerd, writing E2E scripts, or triaging agent lifecycle bugs.
  Written from the 2026-08-03 dogfood run (docs/dogfood/2026-08-03-integration.md).
version: 1.0.0
category: software-development
---

# Bunker Usage — How to Drive This System For Real

Bunker = multi-agent hosting platform. `bunkerd` (daemon, needs **root**, runs on a Linux host) + `bunker` (CLI, runs anywhere). Each agent is an isolated Linux user with its own rootless Docker daemon, cgroup limits, and port range. Control everything through the CLI or the connect-go gRPC/REST API.

## Entry points

- **Live server (MVP):** `78.46.173.180` — REST :18080, gRPC :19090, SSH :22. **Auth is disabled** (empty token works). Hostname it reports: `bunker-mvp` (only resolves on the server itself!).
- **Binaries:** `go build -o bunker ./cmd/bunker`, `go build -o bunkerd ./cmd/bunkerd`.
- **CLI config:** `~/.bunker/config.yaml` (server aliases + active server), keys in `~/.bunker/keys/<agent-id>`.
- **Test config for local daemon:** `test-config.yaml` (auth off, /tmp/bunkerd-test, port 9095) — daemon runs as non-root but spawn will fail at `useradd` (root required, undocumented in README).

## The working workflow (verified 2026-08-03)

```bash
bunker connect http://<server-ip>:19090 --name prod     # register server
bunker status                                           # ONLINE / agents / disk
bunker spawn --agent-id myagent --ttl 30m --cpu 1.0 --memory 1073741824
bunker list --status all
bunker info myagent
bunker exec myagent whoami                              # → bunker-myagent
bunker exec myagent -- docker run --rm alpine echo hi   # rootless Docker works
bunker metrics [myagent]                                # server or agent metrics
bunker heartbeat myagent                                # extend TTL
bunker destroy myagent --force                          # full cleanup, verified
```

Spawn ≈ 9s. Limits are real — check them at `/sys/fs/cgroup/user.slice/user-<uid>.slice/{memory.max,cpu.max,pids.max}` (NOT `/sys/fs/cgroup/memory.max`), drop-in at `/etc/systemd/system/user-<uid>.slice.d/50-bunker.conf`.

## Known-broken (as of 2026-08-03, tasks DOGFOOD-001..006)

| Feature | Symptom | Workaround |
|---|---|---|
| `bunker env set/get/list/unset` | ALL fail: `Syntax error: "then" unexpected`, `-f: 1: [: missing ]` | none — avoid; set env inline in exec: `bunker exec <id> -- env FOO=bar <cmd>` |
| `bunker exec <id> -- sh -c '<compound>'` | same syntax error (root cause: server joins args unquoted) | pass simple commands/`&&` chains; no `if`/`for`/`while` at statement start |
| `bunker cp` / `bunker deploy` | `Could not resolve hostname bunker-mvp` | none from remote client — run scp manually with the IP + `~/.bunker/keys/<id>` |
| `bunker tunnel` | hostname + wrong key path `/etc/bunkerd/ssh/<id>` | use the printed command after fixing host→IP and key→`~/.bunker/keys/<id>` |
| spawn `--ttl banana` | silently creates 6h agent | always validate TTL yourself (`6h`, `30m`, `7d`) |
| `bunker destroy <unknown>` | raw `userdel` error | treat as already-gone; check `bunker list` first |
| `bunker status` CPU/Memory/Uptime | zeros/unknown | use `bunker metrics` for real values |

## Pitfalls

- **Never run the E2E battery from a client** (`e2e-full-battery.sh` needs root + on-server paths; it creates/deletes `bunker-e2e-*` users).
- **Server-reported hostname ≠ reachable host.** Any SSH/SCP/SSHFS command printed by spawn uses `bunker-mvp` and server paths — substitute the real IP and client key.
- **Don't trust "CI green" for SSH features** — the battery runs on the server where hostname+keys resolve. Remote-client behavior is the real test.
- **Scratch agents:** always `bunker destroy <id> --force` after a run; TTL auto-destroys but don't rely on it. `bunker list --status all` to confirm zero.
- **Auth:** token is sent as `Authorization: Bearer <token>`; the MVP has auth disabled. Never commit real tokens — the test token `test-regression-token` (in e2e scripts) is fine for the MVP.
- **REST API:** connect-go JSON — `POST http://<ip>:18080/bunker.v1.Bunkerd/<Rpc>` with `Content-Type: application/json`; errors are connect codes (`CodeNotFound`, `CodeInvalidArgument`, `CodeResourceExhausted`).

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
- Full dogfood evidence + diagnostics → `docs/dogfood/2026-08-03-integration.md`, `docs/dogfood/diagnostics.md`
