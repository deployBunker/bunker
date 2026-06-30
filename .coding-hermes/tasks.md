# Bunker — Coding Hermes Task Queue

## Active Sprint: MVP

### Phase 1: Core bunkerd daemon
- [x] **WI-001**: Protobuf code generation — `buf generate` for gRPC + REST gateway
- [x] **WI-002**: bunkerd server skeleton — gRPC server on :9090 with TLS + token auth
- [x] **WI-003**: Agent spawn lifecycle — `useradd` → generate SSH keypair → start dockerd via systemd-run
- [x] **WI-004**: Agent destroy lifecycle — stop dockerd → `userdel -r` → free port range
- [x] **WI-005**: Resource tracking — capacity management, cgroup CPU/memory limits
- [x] **WI-006**: Port range allocator — assign/free per-agent port ranges

### Phase 2: Networking
- [x] **WI-007**: SSH transport — `DOCKER_HOST=ssh://` support with per-agent SSH keys
- [x] **WI-008**: TryCloudflare anonymous tunnels — per-agent public URL
- [x] **WI-009**: Cloudflare named tunnel support — custom domain routing
- [x] **WI-010**: Tailscale integration — per-agent tailnet IP

### Phase 3: CLI
- [x] **WI-011**: `bunker connect` — register a bunkerd server
- [x] **WI-012**: `bunker spawn` — create agent, return connection bundle
- [x] **WI-013**: `bunker list` — list all agents across servers
- [x] **WI-014**: `bunker destroy` — cleanup agent
- [x] **WI-015**: `bunker metrics` — live agent resource usage (2026-06-28)
- [x] **WI-016**: `bunker exec` — execute command in agent context

### Phase 4: REST API
- [x] **WI-017**: REST gateway — connect-go HTTP handlers (single port, same handlers, JSON+Protobuf codecs)
- [x] **WI-018**: API key management — top-level static key + per-agent sub-keys
- [x] **WI-019**: mTLS support — certificate-based auth

### Phase 5: Integration
- [x] **WI-020**: Coding-Hermes skill integration — spawn/destroy in agent lifecycle
- [x] **WI-021**: Hilo test harness — end-to-end tests exercising Hilo
- [x] **WI-022**: GitReins Tier 1 + Tier 2 config — secrets, lint, tests, eval (2026-06-29)

### Phase 6: Bug fixes + hardening (regression findings 2026-06-29)
- [x] **WI-023**: Fix destroy — systemctl user-instance mismatch — `systemctl --user stop` targets root, but dockerd runs under agent user. Stop dockerd via `systemctl --user --machine=bunker-<id>@ stop` or kill the process directly.
- [x] **WI-024**: Fix exec DOCKER_HOST propagation — authorized_keys `environment=` sets DOCKER_HOST correctly but the SSH session may not pick it up. Verify with `docker ps` inside exec.
- [x] **WI-025**: Fix systemd-run stale unit on re-spawn — when spawn reuses an agent_id from a previous incomplete destroy, `systemd-run --unit=` fails with "already loaded". Stop/disable stale unit before re-creating (partial fix in manager.go, needs verification).
- [x] **WI-026**: Multi-agent concurrency test — spawn 5+ agents simultaneously, verify each has isolated dockerd, unique port range, independent home directories.
- [x] **WI-027**: cgroup enforcement test — verify CPU/memory limits constrain dockerd. Spawn agent with 0.5 CPU / 256MB, run stress test, verify killed by OOM or throttled.
- [x] **WI-028**: Cloudflare TryCloudflare tunnels — per-agent public URL via cloudflared. Install cloudflared on server, test public URL reachability.
- [x] **WI-029**: JWT auth + agent-scoped sub-keys — replace static token with JWT (HS256), generate per-agent sub-keys, test auth rejection.
- [x] **WI-030**: TLS/mTLS — certmagic Let's Encrypt (or self-signed for test), mutual TLS between CLI and bunkerd.
- [x] **WI-031**: TTL expiry — agents auto-destroy after default_ttl (6h). Verify timer fires and cleanup runs.
- [x] **WI-032**: bunkerd systemd service — install bunkerd as a systemd service unit so it survives reboots and logrotates.
- [x] **WI-033**: coding-hermes full integration test — added internal/hermes/integration_test.go with 5 safe CI integration tests covering skill lifecycle, task queue format, core skills, tracker integration, and cleanup idempotency. (2026-06-29)
- [x] **WI-034**: Regression suite CI — wire regression-tests.sh into GitHub Actions or cron, run on every push to main.

### Phase 7: E2E hardening (2026-06-29 findings)
- [x] **WI-035**: Rootless Docker for agents — WI-003's dockerd start never actually worked for unprivileged users. dockerd requires root; agents run as non-root. Fix: update `manager.go` spawn to use `dockerd-rootless-setuptool.sh` (download from get.docker.com/rootless), configure subuid/subgid per user, add AppArmor profile for rootlesskit on Ubuntu 24.04 (`/etc/apparmor.d/home.bunker-<id>.bin.rootlesskit`). Verify with `docker run hello-world` via `bunker exec`. Server deps: rootlesskit + uidmap already installed on bunker-mvp. Also fixed `installRootlessDocker` to chown `~/bin` to the agent user before running the installer, updated AppArmor profile to Docker-suggested format, and moved profile creation before installer so rootless docker can actually start (2026-06-30).
- [x] **WI-036**: JWT auth end-to-end — added `api_key` field to `SpawnAgentResponse`, service generates agent-scoped opaque sub-key when JWT auth is enabled, CLI prints it, and unit tests verify rejection without token and acceptance with valid sub-key. Added static-token fallback in JWT auth for migration. (2026-06-30)
- [x] **WI-037**: TLS/mTLS end-to-end — added `tls.self_signed` config, `internal/tlsutil` cert generation helper, refactored CLI to shared `newBunkerdClient`/`resolveToken`, added `e2e-tls.sh` and `internal/server/tls_e2e_test.go`. Verified local self-signed TLS start + CLI `--tls-insecure` connect/list. mTLS verified via unit tests and REST curl with client cert. (2026-06-30)
- [x] **WI-038**: TTL expiry end-to-end — added `bunker heartbeat` CLI command (`internal/cli/heartbeat.go`), wired into `cmd/bunker/main.go`, added unit tests for CLI and service TTL extension (`internal/server/ttl_e2e_test.go`). HeartbeatAgent extends ExpiresAt by configured default TTL. (2026-06-30)
- [x] **WI-039**: Cloudflare tunnel end-to-end — install `cloudflared` on server, spawn agent with `--trycloudflare`, verify public URL reachable via curl. (Code exists from WI-028, needs server-side binary. Fixed tunnel process lifetime regression: cloudflared now runs under background context and stdout is drained. Also hardened destroy cleanup and E2E battery.)
- [x] **WI-044**: Fix CI: Build fails — proto package not found. `github.com/deployBunker/bunker/proto/bunker/v1` missing because `proto/**/*.pb.go` is gitignored. CI needs `buf generate` step before `go build`. (2026-06-30)

### Phase 8: Resource isolation (2026-06-30 findings)
- [x] **WI-040**: Fix `bunker exec` flag parsing — docker flags (`--format`, `--rm`, `-d`, `--name`) are intercepted by cobra instead of forwarded to the exec command. `bunker exec e2e-a docker run --rm hello-world` fails with `unknown flag: --rm`. Fix: add `--` separator support to stop cobra flag parsing before the command args, or use `DisableFlagParsing` + manual parsing. Verify with `bunker exec <agent> -- docker run --rm hello-world`. (2026-06-30)
- [x] **WI-041**: Wire ulimit controls into agent spawn — agents currently inherit system defaults: ulimit -u 62303 (processes), ulimit -n 1024 (open files). Fix: add `TasksMax` (process limit) and `LimitNOFILE` (open file limit) to the systemd-run spawn command in `manager.go`, sourced from agent config defaults (`agent.default_max_processes`, `agent.default_max_open_files`) with per-spawn overrides. Verify with `bunker exec <agent> -- ulimit -u` and `ulimit -n`.
- [x] **WI-042**: Verify + fix cgroup enforcement through rootlesskit — rootless docker creates cgroups under `/sys/fs/cgroup/user.slice/user-<UID>.slice/`, not the standard systemd unit path. Current CPU/memory limits set via systemd-run properties may not propagate correctly through rootlesskit. Fix: verify `cpu.max` and `memory.max` in the correct cgroup path, add cgroup readback test in `rootless_test.go`, document the cgroup verification command in agent metrics. Verify with actual stress test inside agent: `bunker exec <agent> -- sh -c 'stress --cpu 4 --timeout 5s &'` then check cgroup stats.
- [x] **WI-043**: PID namespace isolation — all agents see the same system-wide process list (16 processes visible). Rootlesskit supports `--pidns` for PID namespace isolation. Fix: add `--pidns` flag to rootlesskit launch in `rootless.go`, verify each agent sees only their own processes. Verify with `bunker exec <agent> -- ps aux | wc -l` showing only the agent's own processes (~5), not system-wide (~200). (2026-06-30)

## [x] WI-045: Fix CI: Unit tests — 4 hilo graph tests failing
- **Priority:** high
- **CI Run:** https://github.com/deployBunker/bunker/actions/runs/28455593259
- **Error:** `internal/hilo` package: TestGraph_BlastRadius, TestGraph_BlastRadius_MaxDepth, TestGraph_Stats, TestGraph_ProjectDirResolution fail. Needs hilo binary in CI or environment fix.
- **Fix:** Rewrote `internal/hilo/hilo_test.go` to create a synthetic `.vfs/graph/edges.jsonl` fixture in a temp directory via `newTestGraph`, removing the dependency on the real Hilo CLI or `.vfs/` directory. All graph tests now pass when `.vfs/` is absent. Verified with `.vfs` renamed, full suite passed, and E2E battery passed on bunker-mvp (28 pass, 0 fail, 4 documented notes).

> ⚠ **WI-003 post-mortem**: Spawn was marked complete but dockerd never ran under unprivileged users. Root cause: no rootless docker config in spawn code, and no E2E test that actually ran `docker run` inside an agent. WI-035 is the fix; WI-033 (integration test) should also be extended to include a `docker run` smoke assertion once rootless docker works.

---

## Tech Stack (researched & locked)
- **gRPC+REST**: connect-go (v1.20) — single binary, net/http native
- **Router**: chi (v5) — stdlib-compatible
- **Auth**: golang-jwt (v5.3) — HS256/RS256/Ed25519
- **TLS**: certmagic (v0.25) — auto Let's Encrypt, self-signed, mTLS
- **CLI**: cobra (v1.10) + viper (v1.21)
- **Config**: YAML at /etc/bunkerd/config.yaml
- **TryCloudflare**: shell out to cloudflared binary

## Quality Gates (run EVERY commit)
- **GitReins Tier 1**: `gitreins guard run` — secrets, lint, tests, format
- **GitReins Tier 2**: `gitreins judge evaluate <id>` — LLM code review per task
- **Hilo**: `hilo classify` + `hilo graph` — auto-classify files, dependency analysis, metadata woven into codebase
- **Build**: `go build ./... && go vet ./...` before every commit

## Task States
- `[ ]` — pending
- `[~]` — in progress
- `[x]` — complete

## Model
- Primary: Kimi K2.7 (`kimi-for-coding/kimi-for-coding`)
- Backup: ollama-cloud
- Orchestrator: DeepSeek V4 Pro (Hermes)
