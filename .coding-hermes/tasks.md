<!--
  ⚠️  BOARD FORMAT — coding-hermes-model-router v1.3 (2026-07-24)
  All tasks MUST use matrix format: | ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback |
  Before editing this file, load the skill: skill_view(name='coding-hermes-model-router')
  Validate: python3 ~/.hermes/scripts/validate-board-format.py .coding-hermes/tasks.md
-->

# Bunker — Task Board

**Core purpose:** Multi-agent hosting platform — `bunkerd` daemon + `bunker` CLI. Spawns isolated Linux users with rootless Docker, resource limits (cgroups), SSHFS mounts, Docker tunnels, TTL expiry. Go 1.26, connect-go (gRPC+REST), cobra CLI.

## Dogfood Findings (2026-08-03)

Real-use run by coding-hermes-dogfood (cron): built CLI from source, drove the live bunker-mvp server (78.46.173.180) through the full lifecycle — spawn → exec → docker run → metrics → heartbeat → destroy. Core lifecycle WORKS (9s spawn, rootless Docker 29.1.3, cgroup limits verified enforced). Full report: `docs/dogfood/2026-08-03-integration.md`.

| ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback |
|----|------|-----|-----|------|------|-------|-----------|----------|
| DOGFOOD-001 | 🔴 `bunker env set/get/list/unset` ALL broken on live server + compound exec snippets fail — `sh: 1: Syntax error: "then" unexpected`, `-f: 1: [: missing ]`, awk usage error. Root cause: `internal/server/service.go` `buildAgentExecCommand` (line ~372-383) joins Command+Args UNQUOTED into the remote line; CLI sends `sh -c <snippet>` so the remote shell parses `if`/`[` as args to inner `sh -c`, orphaning `then`. Reproduced: `bunker exec <id> -- "if [ -f /etc/hostname ]; then cat /etc/hostname; fi"` fails identically; `&&` chains work. Fix direction: server must quote the snippet (`sh -c '<snippet>'`) or CLI must not self-wrap — send Command+Args separately. | P0 | 3 | — | ++bugfix, ++shell | deepseek-v4-flash | Reproduce via `bunker env set` on bunker-mvp, fix arg join in buildAgentExecCommand, add unit test for compound-snippet exec | GLM-5.2 |
| DOGFOOD-002 | 🔴 `bunker cp`, `bunker deploy`, `bunker tunnel` unusable from remote clients — SSH target is the server's SELF-REPORTED hostname (`bunker-mvp`) which does not resolve from client machines (`ssh: Could not resolve hostname bunker-mvp`); `tunnel` additionally uses server-side key path `/etc/bunkerd/ssh/<id>` (Permission denied) instead of `~/.bunker/keys/<id>`. Spawn output prints SSHFS/tunnel commands with the same broken hostname+key path. No `--ssh-host` override flag exists. | P1 | 3 | — | ++networking, ++cli | deepseek-v4-flash | Resolve SSH host from server config URL (or ServerInfo hostname fallback + --ssh-host flag); use client-local key path; fix printed bundle commands | GLM-5.2 |
| DOGFOOD-003 | 🟡 Invalid `--ttl` silently accepted — `bunker spawn --ttl banana` created an agent with default 6h TTL (expiry +6h) instead of erroring. API spec (specs/api.md) promises `CodeInvalidArgument` for bad TTL format. Silent fallback = users typo TTL and get agents lasting 6h. | P1 | 1 | — | ++validation, ++api | deepseek-v4-flash | Parse TTL server-side, return CodeInvalidArgument on bad format | deepseek-v4-flash |
| DOGFOOD-004 | 🟡 README quick start never states `bunkerd` must run as root — non-root run serves ServerInfo/list fine but spawn fails with `useradd: Permission denied`. User following the quick start hits a wall at first spawn. Document root requirement (or preflight-check and warn at startup). | P2 | 1 | — | ++docs | deepseek-v4-flash | Add root requirement + preflight warning to README/quick start | deepseek-v4-flash |
| DOGFOOD-005 | 🟡 Error UX — `bunker destroy <unknown-id>` leaks raw `userdel: user ... does not exist` (exit 6) instead of clean not_found; CLI prints `bunker: exit code 2` but process exits 1 (inconsistent remote exit-code propagation); `bunker exec <unknown>` correctly returns `not_found` — inconsistent across commands. | P2 | 1 | — | ++ux | deepseek-v4-flash | Map userdel failure to CodeNotFound; align CLI exit codes | deepseek-v4-flash |
| DOGFOOD-006 | 🟡 Status/metrics gaps — `bunker status` shows `CPU: 0.0%`, `Memory: 0 B / 0 B`, `Uptime: unknown` on live server (metrics exist via `bunker metrics`); `bunker version` shows `commit: unknown`/`built: unknown` (no ldflags injection) making deployed-vs-HEAD skew undetectable. | P2 | 2 | — | ++monitoring, ++ux | deepseek-v4-flash | Wire server metrics into status; add -ldflags version injection | deepseek-v4-flash |

**Assumptions:** Go project — `go build ./... && go test ./... -short && go vet ./... && gofmt -w`. Live E2E battery on bunker-mvp (78.46.173.180). GitReins Tier 1 + Hilo active. Dogfood run date 2026-08-03; agent cleanup verified (server back to 0 agents).

**Routing Notes:** Go project — deepseek-v4-flash primary. DOGFOOD-001/002 are the top priorities (documented features 100% broken from remote client). Verify fixes against live server, not just unit tests — env/cp/tunnel need a real client outside the server host.
