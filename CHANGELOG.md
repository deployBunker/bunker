# Changelog

## 0.1.1 (2026-08-10)

### Added
- `bunker ssh <agent-id> [command...]` — interactive session into an agent (GAP-034)
- `bunker --version` via cobra (GAP-035)
- `bunker systemd` helpers no longer hidden in `--help` (GAP-033)
- CI: CLI-surface smoke (`--version` + `ssh`/`systemd` in `--help`) and a version-authority check (latest git tag == `bunker version` == CHANGELOG top entry) on every push (GAP-036/GAP-038)

### Fixed
- `go install github.com/deployBunker/bunker/cmd/bunker@latest` works from a fresh checkout — generated protobuf code is now committed (GAP-027)
- `go install @latest` ships the full CLI again — the v0.1.1 tag was re-cut at HEAD after being cut from a pre-bump commit that built a 0.1.0 binary without `ssh`/`systemd`; CI now builds from the tag itself (GAP-041)
- `bunker version` reports a real commit and build time on `go install` builds — VCS metadata from the embedded build info (`debug.ReadBuildInfo`) is used when ldflags are absent, with the module version as a last-resort commit (GAP-042)
- Deterministic agent PATH — exec builders no longer inherit the daemon's ambient `$PATH` (GAP-030)
- Version defaults aligned across source, tags, and docs (GAP-031)
- `bunker status` exits non-zero when no servers are configured, matching `list`/`spawn`/`info` (GAP-037)

### Docs
- Agent port range, `max_agents` default, and demo port corrections across docs (GAP-028/GAP-029/GAP-032)

## 0.1.0 (2026-07-06)

### Features
- Multi-server support: `bunker use <server>` and `bunker status --all-servers`
- Rootless Docker container support with user namespace remapping
- Cloudflare tunnel auto-provisioning per container
- mTLS between CLI and server with certificate generation
- JWT-based API key authentication with scope enforcement
- cgroup resource enforcement (CPU, memory, PID limits)
- Tailscale integration for private networking
- Hermes Agent auto-provisioning inside containers
- Hilo code intelligence integration
- systemd unit generation for bunkerd
- CLI commands: spawn, destroy, list, info, exec, connect, status, use, mount, metrics, heartbeat

### Tests
- 459 test functions across 50 test files (532 test cases)
- 14 packages, all passing
- Live E2E battery on bunker-mvp

### Infrastructure
- Go 1.26.5
- ConnectRPC (gRPC-compatible)
- Docker SDK
- Cloudflare API
