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
- [ ] **WI-019**: mTLS support — certificate-based auth

### Phase 5: Integration
- [ ] **WI-020**: Coding-Hermes skill integration — spawn/destroy in agent lifecycle
- [ ] **WI-021**: Hilo test harness — end-to-end tests exercising Hilo
- [ ] **WI-022**: GitReins Tier 1 + Tier 2 config — secrets, lint, tests, eval

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
