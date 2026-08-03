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
| DOGFOOD-001 | ✅ FIXED (tick #192) — `bunker env set/get/list/unset` + compound exec snippets. Root cause: `buildAgentExecCommand` joined Command+Args unquoted; CLI sends `sh -c <snippet>` so remote shell orphaned `then`/`[`. Fixed server-side: (1) shellQuoteSingle every arg, (2) wrap joined command in `sh -c '<joined>'` (fixes snippets as COMMAND token too), (3) `[ -f ]` guard on env-file source (fresh agent has no env file → dash exit 2 killed ALL exec — live-E2E-only regression), (4) `set -a` allexport so injected vars reach child shell. Round-trip unit tests execute built command through sh. Live E2E on bunker-mvp: 10/10 PASS (VERIFY-PASS, if/then, env set/get/list/unset, FOO visible in exec, awk, &&). Commits 9896c99/5151025/b96f696/956d307/e6879ab. | P0 | 3 | — | ++bugfix, ++shell | deepseek-v4-flash | ✅ COMPLETE — verified by gitreins judge (6/6 PASS, 744e04a9) + live E2E battery | GLM-5.2 |
| DOGFOOD-002 | ✅ FIXED (tick #193) — `bunker cp`/`deploy`/`tunnel` usable from remote clients. Root cause: server baked its SELF-REPORTED hostname (`bunker-mvp`, unresolvable off-host) + server-local key path `/etc/bunkerd/ssh/<id>` into sshfs/tunnel/docker-host strings. Fixed client-side: new `internal/cli/sshhost.go` resolves SSH host by precedence (--ssh-host flag > server-entry URL hostname > server-provided hostname), rewrites tunnel `-i` to `~/.bunker/keys/<id>`, rewrites spawn/info bundle display (host + key). Live E2E from kara vs bunker-mvp: spawn→cp→deploy→tunnel (docker 29.1.3 through forwarded socket)→destroy, server back to 0 agents. Bonus: fixed pre-existing mount_test.go HOME-isolation bug that clobbered real ~/.bunker/config.yaml on every `go test ./...`. Commit fd282b1. Judge INCOMPLETE x2 (evaluator 2.1M token cap, same structural issue as DOGFOOD-001) — verified by guard Tier 1 PASS + unit tests + live E2E instead. | P1 | 3 | — | ++networking, ++cli | deepseek-v4-flash | ✅ COMPLETE — guard Tier 1 PASS + live E2E from remote client (commit fd282b1) | GLM-5.2 |
| DOGFOOD-003 | 🟡 Invalid `--ttl` silently accepted — `bunker spawn --ttl banana` created an agent with default 6h TTL (expiry +6h) instead of erroring. API spec (specs/api.md) promises `CodeInvalidArgument` for bad TTL format. Silent fallback = users typo TTL and get agents lasting 6h. | P1 | 1 | — | ++validation, ++api | deepseek-v4-flash | Parse TTL server-side, return CodeInvalidArgument on bad format | deepseek-v4-flash |
| DOGFOOD-004 | 🟡 README quick start never states `bunkerd` must run as root — non-root run serves ServerInfo/list fine but spawn fails with `useradd: Permission denied`. User following the quick start hits a wall at first spawn. Document root requirement (or preflight-check and warn at startup). | P2 | 1 | — | ++docs | deepseek-v4-flash | Add root requirement + preflight warning to README/quick start | deepseek-v4-flash |
| DOGFOOD-005 | 🟡 Error UX — `bunker destroy <unknown-id>` leaks raw `userdel: user ... does not exist` (exit 6) instead of clean not_found; CLI prints `bunker: exit code 2` but process exits 1 (inconsistent remote exit-code propagation); `bunker exec <unknown>` correctly returns `not_found` — inconsistent across commands. | P2 | 1 | — | ++ux | deepseek-v4-flash | Map userdel failure to CodeNotFound; align CLI exit codes | deepseek-v4-flash |
| DOGFOOD-006 | 🟡 Status/metrics gaps — `bunker status` shows `CPU: 0.0%`, `Memory: 0 B / 0 B`, `Uptime: unknown` on live server (metrics exist via `bunker metrics`); `bunker version` shows `commit: unknown`/`built: unknown` (no ldflags injection) making deployed-vs-HEAD skew undetectable. | P2 | 2 | — | ++monitoring, ++ux | deepseek-v4-flash | Wire server metrics into status; add -ldflags version injection | deepseek-v4-flash |

**Assumptions:** Go project — `go build ./... && go test ./... -short && go vet ./... && gofmt -w`. Live E2E battery on bunker-mvp (78.46.173.180). GitReins Tier 1 + Hilo active. Dogfood run date 2026-08-03; agent cleanup verified (server back to 0 agents).

**Routing Notes:** Go project — deepseek-v4-flash primary. DOGFOOD-003 is the top remaining priority (P1: invalid --ttl silently accepted — API spec promises CodeInvalidArgument). Verify fixes against live server, not just unit tests.
