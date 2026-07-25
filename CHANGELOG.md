# Changelog

## 1.0.0 (2026-07-25)

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
