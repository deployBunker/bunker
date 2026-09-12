# Bunker — Architecture Specification

Version: 1.1.0
Status: Stable
Last Updated: 2026-09-12

## Overview

Bunker provisions isolated Docker environments ("agents") for AI coding agents. Each agent gets a non-root Linux user, a rootless Docker daemon, a dedicated port range, and optional network ingress (Cloudflare Tunnels, Tailscale). The system is designed for multi-tenant agent hosting where each agent needs its own sandboxed Docker runtime.

## System Components

```
┌─────────────────────────────────────────────────────┐
│                     CLI (bunker)                     │
│  connect  spawn  list  destroy  exec  metrics  ...  │
└──────────────────────┬──────────────────────────────┘
                       │ gRPC + REST (connect-go)
                       ▼
┌─────────────────────────────────────────────────────┐
│                 bunkerd (daemon)                     │
│  gRPC :9090  │  REST :8080                          │
│                                                     │
│  ┌──────────┐ ┌──────────┐ ┌───────────────────┐   │
│  │  Agent   │ │  Auth    │ │  Network Ingress  │   │
│  │ Manager  │ │ (JWT/Key)│ │  (Tunnel/Tailscale)│   │
│  └──────────┘ └──────────┘ └───────────────────┘   │
│                                                     │
│  ┌──────────────────────────────────────────────┐   │
│  │          Resource Tracker                     │   │
│  │  CPU/Memory/Disk caps  │  Port Allocator     │   │
│  └──────────────────────────────────────────────┘   │
└──────────────────────┬──────────────────────────────┘
                       │ systemd-run (transient units)
                       ▼
┌─────────────────────────────────────────────────────┐
│              Agent Instances                          │
│                                                     │
│  ┌──────────────────┐  ┌──────────────────┐         │
│  │  Agent: bunker-a │  │  Agent: bunker-b │  ...    │
│  │  User: bunker-a  │  │  User: bunker-b  │         │
│  │ rootless dockerd │  │ rootless dockerd │         │
│  │  Ports: 10000+   │  │  Ports: 10100+   │         │
│  │  cgroup: 1 CPU   │  │  cgroup: 2 CPU   │         │
│  │        1 GB RAM  │  │        4 GB RAM  │         │
│  │  TMPDIR: /run/   │  │  TMPDIR: /run/   │         │
│  └──────────────────┘  └──────────────────┘         │
└─────────────────────────────────────────────────────┘
```

## Directory Layout

```
/home/bunker-<id>/      Agent home directory
  bin/                  User-local binaries (rootlesskit, etc.)
  .ssh/                 SSH keypair for DOCKER_HOST=ssh://
/run/bunker/<id>/       Runtime files
  docker.sock → /run/user/<UID>/docker.sock   Symlink to actual socket
  tmp/                  Private TMPDIR
  env                   Environment variables (KEY=VALUE)
/etc/bunkerd/config.yaml   Server configuration
```

## Agent Lifecycle

### Spawn Flow

1. CLI sends `SpawnAgentRequest` to bunkerd
2. Resource tracker verifies capacity (CPU, memory, disk, port range)
3. Port allocator assigns a port range (`port_range_start`–`port_range_end`)
4. `useradd -m -s /bin/bash bunker-<id>` creates the agent Linux user
5. SSH keypair generated; public key written to `~/.ssh/authorized_keys`
6. `systemd-run --user --machine=bunker-<id>@` creates a transient unit for dockerd
7. `loginctl enable-linger bunker-<id>` enables persistent user manager
8. Rootless Docker tooling is installed into `~/bin` if absent
   (`installRootlessDocker`: a fresh host downloads ~93 MB, so the first spawn
   takes 60-90s+; later spawns reuse the cached tooling)
9. Docker daemon starts via `systemd-run --system --unit=bunker-docker-<id>`;
   `waitForDockerd` polls for socket + process
10. Symlink created: `/run/bunker/<id>/docker.sock` → `/run/user/<UID>/docker.sock`
11. Optional: cloudflared tunnel or tailscale join
12. Optional per-agent image customization (`image_spec`, GAP-064): the
    validated package-add spec is built once through the agent's rootless socket
    before the response is returned
13. Response returned with SSH connection string, keys, port range, tunnel URL

### Destroy Flow

Order per `internal/agent/manager_destroy.go`:

1. CLI sends `DestroyAgentRequest` to bunkerd
2. The agent's own containers are stopped/removed through its rootless socket
   (`cleanupAgentContainers`) — before the daemon is stopped
3. The user-slice cgroup drop-in (`user-<UID>.slice.d/50-bunker.conf`) is removed
4. rootless dockerd is stopped (direct SIGTERM/kill, then `systemctl --user
   stop`) and the transient unit disabled; destroy waits up to ~10s for the
   agent's `dockerd`/`rootlesskit` processes to exit
5. `userdel -rf bunker-<id>` removes the user + home directory
6. Runtime directory `/run/bunker/<id>/` is removed
7. The real socket `/run/user/<UID>/docker.sock` is removed
8. The server-side persisted key `<agent.ssh_dir>/<id>` is removed
9. Tunnel (cloudflared) and Tailscale teardown
10. Resource tracker unregistered; port range returned to the allocator pool

### States

| State     | Description |
|-----------|------------|
| pending   | Request accepted, spawning in progress |
| starting  | User created, dockerd starting |
| running   | dockerd socket reachable, Docker functional |
| stopping  | Destroy in progress |
| stopped   | Fully cleaned up |
| failed    | Spawn or runtime error |

### TTL Expiry

Agents have a `default_ttl` (6h default). The `HeartbeatAgent` RPC extends `ExpiresAt` by the TTL duration. Expired agents are auto-destroyed by the TTL monitor.

## Networking

### Network Modes

| Mode | Description | Port Control |
|------|------------|--------------|
| `CLOUDFLARE_TUNNEL` | Cloudflare Tunnel (TryCloudflare or named) | Cloudflare-managed |
| `TAILSCALE` | Tailscale mesh VPN with tailnet IP | Tailscale-managed |
| `DIRECT` | Direct port behind load balancer | Port range on host |

### Per-Agent Port Isolation

Each agent gets a dedicated port range (e.g., 10000–10099 for agent-a, 10100–10199 for agent-b). Ports are allocated from a pool and returned on destroy. Overlapping ranges are prevented.

### SSH Transport

`DOCKER_HOST=ssh://bunker-<id>@host` passes through the agent's SSH keypair. The `authorized_keys` file includes `environment="DOCKER_HOST=unix:///run/user/<UID>/docker.sock"` to auto-configure the Docker socket path.

## Authentication

### Two-Tier Auth Model

| Tier | Scope | Mechanism |
|------|-------|-----------|
| Master | Full server access (spawn, destroy, list all) | Static token or JWT (HS256) |
| Agent | Scoped to single agent (metrics, heartbeat, info) | Opaque API sub-key or JWT with `agent_id` claim |

### JWT Flow

1. CLI authenticates with master token → receives JWT
2. On spawn, server generates an agent-scoped opaque sub-key
3. Agent-scoped tokens can only access the `Agent` service (GetInfo, Metrics, Heartbeat)
4. Master-only endpoints reject agent-scoped tokens via `MasterOnlyJWTAuth`

### TLS/mTLS

- Self-signed certificates for local development (`tls.self_signed: true`)
- CertMagic for auto Let's Encrypt in production (`tls.auto_tls: true`)
- Mutual TLS with CA verification for server-to-server (`tls.mtls: true`, `tls.ca_file`)

## Resource Isolation

### cgroup Enforcement

CPU and memory limits are set via `systemd-run --property=CPUQuota=<pct> --property=MemoryMax=<bytes>`. These propagate through rootlesskit to Docker containers.

### Disk Quota

`LimitFSIZE=<bytes>` prevents disk exhaustion. Default: 20 GiB per agent.

### Process Isolation

`--pidns` flag on rootlesskit provides PID namespace isolation. Each agent sees only its own processes.

### TMPDIR Isolation

Each agent gets a private TMPDIR at `/run/bunker/<id>/tmp/`, preventing `/tmp` collisions between agents and root cron processes.

## gRPC Services

### Bunkerd Service (master auth)

connect-go serves every RPC over **POST only** (the handler is created without
`WithHTTPGet`); the "Kind" column is the RPC shape, not an HTTP method. Any
non-POST request to these paths returns `405`.

| RPC | Kind | Description |
|-----|------|------------|
| ServerInfo | unary | Hostname, version, capacity |
| ServerMetrics | unary | CPU, memory, disk, container totals |
| SpawnAgent | unary | Create new agent |
| DestroyAgent | unary | Remove agent |
| ListAgents | unary | List agents (filterable, paginated) |
| GetAgent | unary | Single agent details |
| AgentMetrics | unary | Per-agent resource usage |
| ExecAgent | server-streaming | Execute command, stream stdout/stderr |
| RunAgent | unary | Execute command (optionally detached) |
| HeartbeatAgent | unary | Extend TTL |
| QueryAudit | unary | Query the audit trail (see docs/audit.md) |

### Agent Service (agent-scoped auth)

| RPC | Kind | Description |
|-----|------|------------|
| GetInfo | unary | Agent's own details |
| Metrics | unary | Agent's own resource usage |
| Heartbeat | unary | Extend own TTL |

## CLI Commands

| Command | Description |
|---------|------------|
| `bunker connect <host>:<port>` | Register a bunkerd server |
| `bunker use <name>` | Select the active server |
| `bunker status` | Server status (CPU/memory/disk/uptime) |
| `bunker spawn [--cpu X] [--memory Y] [--ttl Z] [--image-spec F]` | Create agent |
| `bunker list [--server <name>]` | List agents |
| `bunker info <agent-id>` | Agent details |
| `bunker destroy <agent-id> [--keep-key]` | Remove agent (deletes the local key) |
| `bunker exec <agent-id> -- <cmd>` | Execute command via SSH |
| `bunker metrics [agent-id]` | Resource usage |
| `bunker heartbeat <agent-id>` | Extend TTL |
| `bunker tunnel <agent-id> [port]` | SSH tunnel to Docker socket |
| `bunker mount <agent-id> [path]` | SSHFS mount agent home |
| `bunker ssh <agent-id> [cmd]` | Interactive SSH session |
| `bunker cp <local> <agent-id>:<path>` | Copy a file into an agent |
| `bunker deploy <local-path> <agent-id>:<path>` | Copy + chown a path into an agent |
| `bunker run <agent-id> [--detach] -- <cmd>` | Run command (optionally persistent) |
| `bunker env set|get|list|unset <agent-id> ...` | Manage environment variables |
| `bunker systemd install|uninstall|status` | Manage the bunkerd systemd service |
| `bunker audit verify|list|export` | Inspect the audit trail |
| `bunker version` | Print version/commit/build metadata |

## Technology Stack

| Component | Technology | Version |
|-----------|-----------|---------|
| Language | Go | 1.26.5 |
| RPC Framework | connect-go | 1.20.x |
| HTTP Router | chi | 5.3.x |
| Auth | golang-jwt | 5.3.x |
| TLS | certmagic | 0.25.x |
| CLI | cobra + viper | 1.10.x / 1.21.x |
| Config | YAML | /etc/bunkerd/config.yaml |
| Process Mgmt | systemd-run | transient units |
| Container | rootless Docker | via dockerd-rootless-setuptool.sh |
| Tunnels | cloudflared | TryCloudflare + named |
| VPN | Tailscale | userspace-networking |

## Quality Gates

- Build: `go build ./...`
- Vet: `go vet ./...`
- Format: `gofmt -w` on new files
- Tests: `go test -short ./...` (unit + integration)
- E2E: `bash e2e-full-battery.sh` on bunker-mvp (78.46.173.180)
- Lint: `golangci-lint run`
- GitReins Tier 1: `gitreins guard` (secrets, build, lint, tests)
- Hilo: `hilo classify` + `hilo graph impact` before changes
