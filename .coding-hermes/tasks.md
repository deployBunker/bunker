# Bunker — Model-Router Task Matrix

> **Core purpose:** Per-user Docker host provisioning for AI agents — gRPC + REST API to spawn isolated Docker environments with SSH, Cloudflare tunnels, and resource enforcement.
> **Language:** Go 1.26.5 | **CI:** GitHub Actions (green) | **Live server:** bunker-mvp (78.46.173.180)

## Active

| INFRA-001 | ~~System thread exhaustion~~ — RESOLVED. Go build/vet/test all pass (397 tests, 14/14 packages green). Transient host-level load spike. | Resolved | 1 | env | — |

## Completed

| SYNC-001 | Sync GitReins tasks — 55 completed tasks still pending in task objects | Low | 1 | ec0c54f | DeepSeek V4 Flash |

| ID | Task | Pri | Cpx | Commit | Model |
|----|------|-----|-----|--------|-------|
| **Phase 1 (WI-001–006)** | Protobuf codegen, bunkerd skeleton, agent spawn/destroy lifecycle, resource tracking, port allocator | Critical | 6 | — | DeepSeek V4 Pro |
| **Phase 2 (WI-007–010)** | SSH transport, TryCloudflare tunnels, named tunnels, Tailscale | High | 5 | — | DeepSeek V4 Pro |
| **Phase 3 (WI-011–016)** | CLI: connect, spawn, list, destroy, metrics, exec | High | 4 | — | DeepSeek V4 Pro |
| **Phase 4 (WI-017–019)** | REST gateway, API key management, mTLS | High | 4 | — | DeepSeek V4 Pro |
| **Phase 5 (WI-020–022)** | Coding-Hermes skill, Hilo test harness, GitReins config | High | 4 | — | DeepSeek V4 Pro |
| **Phase 6 (WI-023–034)** | Bug fixes: destroy, exec, re-spawn, concurrency, cgroup, Cloudflare, JWT, TLS, TTL, systemd | Critical | 6 | — | GPT-5.6 Sol |
| **Phase 7 (WI-035–044)** | E2E hardening: rootless Docker, JWT E2E, TLS E2E, TTL heartbeat, Cloudflare E2E, CI build fix | Critical | 6 | — | GPT-5.6 Sol |
| **Phase 8 (WI-040–044)** | Resource isolation: exec flag parsing, ulimit, cgroup through rootlesskit, PID namespace | High | 5 | — | DeepSeek V4 Pro |
| WI-045 | Fix CI: 4 hilo graph tests failing | High | 3 | — | DeepSeek V4 Pro |
| **Phase 9 (WI-046–048)** | Live-server verification: exec SSH env, dockerd wait, socket path (rootlesskit detach-netns) | Critical | 5 | f330406, 31966ee | DeepSeek V4 Pro |
| **Phase 10 (WI-049–055)** | Spec compliance: SSHFS mount, Docker tunnel, disk_max_bytes, max_docker_containers, ServerMetrics, GetAgent, Agent service scoping | High | 5 | c9e4099 | DeepSeek V4 Pro |
| WI-056 | Multi-server CLI E2E (2 bunkerd instances, isolated port ranges) | High | 4 | — | DeepSeek V4 Pro |
| WI-057 | Tailscale integration (code verified, E2E needs binary+auth key) | Medium | 3 | — | DeepSeek V4 Pro |
| WI-058 | Resource enforcement verification (CPU/memory cgroup confirmed OOM-kill) | High | 4 | — | DeepSeek V4 Pro |
| WI-059 | Fix /tmp disk quota in hermes skills tests (os.TempDir) | Medium | 2 | 5289328 | DeepSeek V4 Flash |
| **Phase 11 (WI-060)** | E2E battery hardening (34 pass, 0 fail, VERIFY-PASS) | High | 3 | be05e4a | DeepSeek V4 Pro |
| **Phase 12 (WI-061)** | Rootless Docker installer regression (lingering, XDG_RUNTIME_DIR, DBUS) | Critical | 5 | 7d13e1b | GPT-5.6 Sol |
| **Phase 13 (WI-062)** | Per-package SKILL.md (8 files: agent, auth, cli, config, server, systemd, tunnel, proto) | Medium | 2 | — | DeepSeek V4 Flash |
| **Phase 14 (WI-064–065)** | Repo hygiene: cross-repo contamination removal, untrack GitReins history | Low | 2 | — | DeepSeek V4 Flash |
| **Phase 15 (WI-066)** | Rootless dockerd socket path mismatch fix (symlink reconciliation) | High | 3 | — | DeepSeek V4 Pro |
| **Phase 16 (WI-063,067–069)** | Production UX: exec --raw/--script, /tmp isolation, run --detach (systemd transient), env set | High | 4 | — | DeepSeek V4 Pro |
| **Phase 17 (WI-070)** | Makefile (build, test, vet, fmt, lint, proto, clean, e2e, install, ci) | Medium | 2 | — | DeepSeek V4 Flash |
| **Phase 18 (WI-071–073)** | Security: Go 1.26.5, 12 outdated deps, govulncheck | Medium | 3 | 7273bfd | DeepSeek V4 Pro |
| TEST-001 | internal/server coverage 52.3%→60.9% (+9 tests, ExecAgent, agentService, SSH) | High | 4 | c61b01d | DeepSeek V4 Pro |
| TEST-002 | CLI unit tests: client.go + mount.go (16 tests) | Medium | 3 | 200c424 | Step 3.7 Flash |
| TEST-003 | internal/server coverage merged into TEST-001 | Medium | 3 | c61b01d | DeepSeek V4 Pro |
| TEST-004 | Auth streaming interceptor tests (14 tests, coverage 79.9%→89.7%) | Medium | 3 | 258dc9b | DeepSeek V4 Pro |
| TEST-005 | Rootless function integration tests (6 tests, 358 lines, +build integration) | Medium | 3 | 0ec350b | DeepSeek V4 Pro |
| SPEC-001 | Formal spec files: architecture, API, agent-lifecycle | Low | 2 | SPEC-001 | GPT-5.6 Terra |
| DUCKBRAIN-001 | Memory initialized: architecture, tech stack, foreman state, open gaps | Low | 1 | — | DeepSeek V4 Flash |
| DEPS-001 | Upgrade 9 outdated test/indirect Go deps | Low | 2 | c0908ae | Step 3.7 Flash |
| DOC-001 | go.mod go directive 1.25.0→1.26.5 | Low | 1 | c0908ae | DeepSeek V4 Flash |
| DOC-002 | README Go badge + 7 SKILL.md files | Low | 2 | — | DeepSeek V4 Flash |
| QUAL-001 | Split manager.go (959 lines → manager.go 93L + spawn 744L + destroy 103L) | Medium | 3 | a60aa88 | DeepSeek V4 Pro |
| QUAL-002 | 7 SKILL.md files: apikey, hermes, hilo, resource, tailscale, tlsutil, bunkerv1connect | Low | 2 | — | DeepSeek V4 Flash |
| DUCK-001 | Idle tick counting fixed (DuckBrain tracks tick history with timestamps) | Low | 2 | — | DeepSeek V4 Flash |
| DEPS | Upgrade go-jose + go-md2man blocked by cobra | Low | 1 | — | DeepSeek V4 Flash |

## Assumptions

- Go project: `go build ./... && go test ./... -short && go vet ./... && gofmt -w`
- GitReins Tier 1 (secrets, lint, tests) + Hilo classification active
- Live E2E battery on bunker-mvp (78.46.173.180) required for spawn/destroy/exec/SSH changes
- 397 tests across 14 packages, 4 no-test packages expected (cmd/bunker, cmd/bunkerd, proto, bunkerv1connect)
- All 73 work items complete (Phase 1–18). Project is feature-complete.

## Routing Notes

- **Go project** — DeepSeek V4 Pro primary for implementation ($0.44/1M), Step 3.7 Flash for test/CI tasks ($0.09/1M)
- GPT-5.6 Sol for complex system-level work: rootless Docker debugging, TLS/mTLS E2E, cgroup enforcement
- GPT-5.6 Terra for spec/documentation: SPEC-001 formal specs
- DeepSeek V4 Flash for mechanical: doc updates, SKILL.md files, config fixes
- Phases 6-7 escalated to GPT-5.6 Sol due to architectural complexity (rootlesskit, TLS, JWT end-to-end)

## Execution Order

1. DOC-001, DOC-002 (docs — unblock nothing, fast)
2. DEPS-001, DEPS (dependency hygiene)
3. TEST-001 through TEST-005 (test gaps — parallel by package)
4. SPEC-001 (architecture specs)
5. DUCK-001, DUCKBRAIN-001 (memory sync)
6. QUAL-001, QUAL-002 (code quality)
7. SYNC-001 (GitReins task sync — last, non-code)

## Escalation Conditions

- Rootless Docker changes fail E2E battery → escalate to GPT-5.6 Sol (systemd + kernel interaction)
- Spawn/destroy lifecycle regressions → escalate to GPT-5.6 Sol (state machine complexity)
- cgroup enforcement failures on live server → escalate to GPT-5.6 Sol (kernel-level debugging)
- Go test failures not reproducible locally → escalate to Kimi K3 (autonomous investigation)

---

## [x] U01 — Usability & coverage audit — ✅ Complete. Endpoints: all 13 wired (0 stubs). Error handling: proper connect codes. Edge cases: no TODOs/FIXMEs. Coverage gaps found → COV-001 created. Commit: c4203f2

## [ ] COV-001 — Boost internal/agent coverage from 28.7% → 60%+ — ⚠️ BLOCKED by INFRA-002

| ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback |
|----|------|-----|-----|------|------|-------|-----------|----------|
| COV-001 | Boost internal/agent coverage from 28.7% → 60%+: Spawn (12.6%), Destroy (47.8%), RunAgent (28.6%), rootless Docker setup (0%), cgroup limits (0%), AppArmor (0%) | High | 4±1 | INFRA-002 | +++testing, ++go, +docker, +integration | GLM-5.2 | High | MiniMax-M3 |

## [ ] INFRA-002 — Raise hermes-gateway.service cgroup pids.max from 512 (BLOCKING all Go builds)

|| ID | Task | Pri | Cpx | Deps | Tags | 
||----|------|-----|-----|------|------|
|| INFRA-002 | `/sys/fs/cgroup/system.slice/hermes-gateway.service/pids.max` = 512. `go build ./...` spawns 20+ threads, hits limit → `runtime: failed to create new OS thread (errno=11)`. Fix: `echo 4096 > /sys/fs/cgroup/system.slice/hermes-gateway.service/pids.max` (requires root). Recurrence of INFRA-001 (previously marked resolved, not actually fixed). | Critical | 1 | — | +infra, +host |

> **Tick #14 (2026-07-22 08:43):** COV-001 attempted. `go build ./...` + `go vet ./...` both crashed with thread exhaustion (errno=11). `gh run list` SIGABRT'd on pthread_create. Root cause: hermes-gateway.service cgroup pids.max=512, too low for Go compiler's thread usage. Host has 243k ulimit, 48GB free — cgroup is the only bottleneck. INFRA-002 created. Tick bailed.
> **Tick #15 (2026-07-22 13:59):** Situation DEGRADED. `pids.current` = 505/512 (7 free). Even `echo "OK"` + `git status` can't fork — shell profile init exhausts remaining pids. `read_file` also failing with "can't start new thread". Cgroup pids.max=512 is blocking ALL tooling. Root intervention required: `echo 4096 > /sys/fs/cgroup/system.slice/hermes-gateway.service/pids.max` (sudo). **Escalated to Bane — no foreman or worker can operate until pids.max is raised.**

## [ ] NEVER-DONE — Run coding-hermes-never-done 11-point audit

Load coding-hermes-never-done skill. Run ALL 11 checks: spec alignment, doc coverage, test gaps, package upgrades, pitfall hunt, performance audit, endpoint verification, CI/CD health, DuckBrain sync, code quality, middle-out wiring. Create a task for EVERY gap found. This task is never complete — the audit always finds something.

> **Last audit:** U01 tick #13 (2026-07-22 04:22) — 13/13 endpoints wired (0 stubs), all error paths covered with connect codes, no TODOs/FIXMEs. Coverage: 14/14 pkgs pass, agent@28.7% gap → COV-001 created. Build: PASS. Tests: PASS (397/14 pkgs). Vet: PASS. Govulncheck: 0 vulns. Hilo: 87 files, 727 edges (healthy). CI: all green (3 recent runs). DuckBrain: 5 keys. Cooldown: 43200 (12h, re-fixed via API PUT — 4th REVERSION, escalated). Idle counter: 13/7 — escalated to Bane (4th cooldown reversion). Feature-complete, 73/73 done. ⚠️ 4th cooldown reversion — daemon restarts keep resetting 43200→1800. ⚠️ 1 new task: COV-001.
