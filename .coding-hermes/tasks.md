# Bunker — Coding Hermes Task Queue

## Active Sprint: MVP

### Phase 1: Core bunkerd daemon
- [x] **WI-001**: Protobuf code generation — `buf generate` for gRPC + REST gateway
- [ ] **WI-002**: bunkerd server skeleton — gRPC server on :9090 with TLS + token auth
- [ ] **WI-003**: Agent spawn lifecycle — `useradd` → generate SSH keypair → start dockerd via systemd-run
- [ ] **WI-004**: Agent destroy lifecycle — stop dockerd → `userdel -r` → free port range
- [ ] **WI-005**: Resource tracking — capacity management, cgroup CPU/memory limits
- [ ] **WI-006**: Port range allocator — assign/free per-agent port ranges

### Phase 2: Networking
- [ ] **WI-007**: SSH transport — `DOCKER_HOST=ssh://` support with per-agent SSH keys
- [ ] **WI-008**: TryCloudflare anonymous tunnels — per-agent public URL
- [ ] **WI-009**: Cloudflare named tunnel support — custom domain routing
- [ ] **WI-010**: Tailscale integration — per-agent tailnet IP

### Phase 3: CLI
- [ ] **WI-011**: `bunker connect` — register a bunkerd server
- [ ] **WI-012**: `bunker spawn` — create agent, return connection bundle
- [ ] **WI-013**: `bunker list` — list all agents across servers
- [ ] **WI-014**: `bunker destroy` — cleanup agent
- [ ] **WI-015**: `bunker metrics` — live agent resource usage
- [ ] **WI-016**: `bunker exec` — execute command in agent context

### Phase 4: REST API
- [ ] **WI-017**: REST gateway — gRPC-Gateway or connect-go HTTP handlers
- [ ] **WI-018**: API key management — top-level static key + per-agent sub-keys
- [ ] **WI-019**: mTLS support — certificate-based auth

### Phase 5: Integration
- [ ] **WI-020**: Coding-Hermes skill integration — spawn/destroy in agent lifecycle
- [ ] **WI-021**: Hilo test harness — end-to-end tests exercising Hilo
- [ ] **WI-022**: GitReins Tier 1 + Tier 2 config — secrets, lint, tests, eval

---

## Task States
- `[ ]` — pending
- `[~]` — in progress
- `[x]` — complete

## Model
- Primary: Kimi K2.7 (`kimi-for-coding/kimi-for-coding`)
- Backup: ollama-cloud
- Orchestrator: DeepSeek V4 Pro (Hermes)
