# Bunker — Agent Lifecycle Specification

Version: 1.0.0
Status: Stable
Last Updated: 2026-07-19

## Overview

Each agent is an isolated Linux user with a rootless Docker daemon, private SSH keypair, dedicated port range, and optional network ingress. This spec covers the full lifecycle from spawn through runtime to destroy.

## Spawn: Step-by-Step

### 1. Request Validation

```
SpawnAgentRequest → validate limits, TTL format, agent_id uniqueness
```

- If `agent_id` is empty, generate `bunker-<8-char-random>`
- Validate `limits` against server capacity
- Validate `ttl` format: `\d+[hmd]` (e.g., "6h", "24h", "7d")
- Check agent_id doesn't exist in resource tracker

### 2. Port Allocation

```
PortAllocator.Allocate(port_range_start, port_range_end) → (start, end)
```

- If request specifies a range, validate it's available
- If unspecified, allocate next available block from pool
- Default block size: 100 ports
- Record allocation in tracker

### 3. User Creation

```bash
useradd -m -s /bin/bash bunker-<id>
```

- Creates `/home/bunker-<id>/`
- Assigns UID from system range
- No password set (SSH key only)

### 4. SSH Keypair

```
ssh-keygen -t ed25519 -f /tmp/bunker-<id>-key -N "" -C "bunker-<id>"
mkdir -p /home/bunker-<id>/.ssh
cp /tmp/bunker-<id>-key.pub /home/bunker-<id>/.ssh/authorized_keys
chown -R bunker-<id>:bunker-<id> /home/bunker-<id>/.ssh
chmod 700 /home/bunker-<id>/.ssh
chmod 600 /home/bunker-<id>/.ssh/authorized_keys
```

- Private key returned in response (not stored on server)
- If `ssh_public_key` provided in request, append to authorized_keys instead
- `environment="DOCKER_HOST=unix:///run/user/<UID>/docker.sock"` prepended for auto socket discovery

### 5. Resource Limits Enforcement

```
systemd-run --user --machine=bunker-<id>@ \
  --property=CPUQuota=<pct> \
  --property=MemoryMax=<bytes> \
  --property=TasksMax=<max_procs> \
  --property=LimitNOFILE=<max_fds> \
  --property=LimitFSIZE=<disk_bytes> \
  ...
```

- `CPUQuota`: Percentage of one CPU core (100% per core, so 200% = 2 cores)
- `MemoryMax`: Absolute byte limit
- `TasksMax`: Process count limit (default: 256)
- `LimitNOFILE`: Open file limit (default: 65536)
- `LimitFSIZE`: Disk quota per file (default: 20 GiB)

### 6. User Manager Enablement

```bash
loginctl enable-linger bunker-<id>
```

Enables persistent systemd user manager so dockerd survives SSH session termination.

### 7. Rootless Docker Installation

```
export XDG_RUNTIME_DIR=/run/user/<UID>
export DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<UID>/bus

# Ensure ~/bin exists and is owned by agent
mkdir -p /home/bunker-<id>/bin
chown bunker-<id>:bunker-<id> /home/bunker-<id>/bin

# Run the rootless installer
dockerd-rootless-setuptool.sh install
```

- `dockerd-rootless-setuptool.sh` must be available on the host
- Ubuntu 24.04 requires an AppArmor profile: `/etc/apparmor.d/home.bunker-<id>.bin.rootlesskit`
- Install process creates rootlesskit + dockerd under `~/bin/`

### 8. Docker Daemon Start

```bash
systemctl --user --machine=bunker-<id>@ start docker
```

Environment variables:
- `DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false` (required for rootlesskit v1.1.1 compatibility)
- `PATH=/home/bunker-<id>/bin:$PATH`
- `XDG_RUNTIME_DIR=/run/user/<UID>`
- `DOCKER_HOST=unix:///run/user/<UID>/docker.sock`

### 9. Da Readiness Wait

```
waitForDockerd(agentID, UID, 5s timeout):
  poll every 250ms:
    1. Check for dockerd process owned by agent UID
    2. Check /run/user/<UID>/docker.sock exists
  on success: create symlink /run/bunker/<id>/docker.sock → /run/user/<UID>/docker.sock
  on failure: capture systemctl status + journalctl logs, trigger cleanup
```

### 10. Runtime Directory Setup

```bash
mkdir -p /run/bunker/<id>/tmp
chown bunker-<id>:bunker-<id> /run/bunker/<id>/tmp
touch /run/bunker/<id>/env
```

- `tmp/`: Private TMPDIR, prevents `/tmp` collisions
- `env`: Environment variables sourced by exec and docker compose

### 11. Optional Network Ingress

**TryCloudflare Tunnel:**
```bash
cloudflared tunnel --url http://localhost:<port> --no-autoupdate &
```
- Disown background process, no stdout capture needed
- Public URL scraped from cloudflared startup output
- Process killed on destroy via pkill

**Tailscale:**
```bash
tailscale up --authkey=<key> --hostname=bunker-<id>
```
- Auth key from server config (`tailscale.auth_key`)
- `tailscale down` on destroy

### 12. API Key Generation (if auth enabled)

```
apikey.Generate(agentID) → (keyID, plaintext, hash)
```

- Opaque random token stored as bcrypt hash
- Plaintext returned once in SpawnAgentResponse
- Stored in `Config.Auth.APIKeys` list in memory
- Scoped: only allows Agent service RPCs

### 13. Response Assembly

```
SpawnAgentResponse {
  agent_id, docker_host_ssh, docker_host_tunnel, sshfs_mount,
  public_url, port_range_start, port_range_end,
  ssh_private_key, limits, expires_at, tailnet_ip, api_key
}
```

## Destroy: Step-by-Step

### 1. Agent Lookup

```
tracker.Get(agentID) → Agent record
```
- Returns error if agent not found
- `force=true` skips safety checks

### 2. Network Teardown

- **cloudflared**: `pkill -f "cloudflared.*bunker-<id>"` or process group kill
- **tailscale**: `tailscale down --hostname=bunker-<id>` (best-effort)
- **named tunnel**: `cloudflared tunnel cleanup <domain>`

### 3. Docker Shutdown

```bash
systemctl --user --machine=bunker-<id>@ stop docker
systemctl --user --machine=bunker-<id>@ disable docker
```

- Grace period: 10 seconds for container shutdown
- Force kill dockerd process if unresponsive

### 4. User Removal

```bash
userdel -r bunker-<id>
```
- `-r`: Remove home directory and mail spool
- Cleans up `/home/bunker-<id>/`, subuid/subgid entries

### 5. Runtime Cleanup

```bash
rm -rf /run/bunker/<id>/
```

### 6. Port Reclamation

```
PortAllocator.Free(port_range_start, port_range_end)
```

### 7. Tracker Removal

```
tracker.Delete(agentID)
```

## Runtime Operations

### Exec (SSH-based)

```
bunker exec <agent-id> -- <command>
```

1. CLI loads agent's SSH private key from local state
2. Opens SSH connection: `ssh -i <key> -o StrictHostKeyChecking=no bunker-<id>@<host>`
3. Forwards `DOCKER_HOST` and `TMPDIR` via environment
4. Sends command: `sh -c '<command>'` (or raw exec if `--raw`)
5. Streams stdout/stderr back

Docker commands flow path:
```
CLI → SSH → agent shell → DOCKER_HOST socket → dockerd → containerd → container
```

### TMPDIR Isolation

Each agent's exec context sets `TMPDIR=/run/bunker/<id>/tmp`. This prevents collisions between:
- Agent processes and root cron jobs writing to `/tmp`
- Multiple agents sharing the global `/tmp`

### Env Injection

```
bunker env set <agent-id> DATABASE_URL=postgres://...
```

Writes to `/run/bunker/<id>/env`, sourced before each exec:
```bash
export $(grep -v '^#' /run/bunker/<id>/env | xargs)
```

### Heartbeat (TTL Extension)

```
bunker heartbeat <agent-id>
```

1. CLI calls HeartbeatAgent RPC
2. Server extends `ExpiresAt = now + TTL`
3. Returns new expiry timestamp

### Run (Detached Commands)

```
bunker run <agent-id> --detach -- docker compose up
```

1. Creates systemd transient unit: `bunkerd-<agent-id>-<name>.service`
2. Unit type: `oneshot` with `RemainAfterExit=yes`
3. Survives exec session termination
4. Managed via `systemctl --user --machine=bunker-<id>@`

## State Machine

```
       ┌─────────┐
       │ pending  │  Spawn request accepted
       └────┬─────┘
            │ useradd, dockerd install, waitForDockerd
            ▼
       ┌─────────┐
       │ starting │  Dockerd starting, socket not ready
       └────┬─────┘
            │ socket reachable, docker run hello-world succeeds
            ▼
       ┌─────────┐
  ┌───►│ running  │  Fully operational
  │    └────┬─────┘
  │         │ destroy requested
  │         ▼
  │    ┌─────────┐
  │    │stopping  │  Tunnel kill, dockerd stop
  │    └────┬─────┘
  │         │ userdel, runtime cleanup, port free
  │         ▼
  │    ┌─────────┐
  │    │ stopped  │  Fully cleaned up
  │    └─────────┘
  │
  └──── heartbeat (TTL extended)
  
       ┌─────────┐
       │ failed   │  Spawn error or runtime crash
       └─────────┘
```

### State Transitions

| From | To | Trigger |
|------|----|---------|
| - | pending | SpawnAgent RPC received |
| pending | starting | User creation + systemd unit created |
| starting | running | waitForDockerd succeeds |
| starting | failed | waitForDockerd timeout or dockerd crash |
| running | stopping | DestroyAgent RPC received |
| running | failed | dockerd crash, OOM kill, or disk full |
| stopping | stopped | Cleanup complete |
| running | running | HeartbeatAgent extends TTL |
| any | stopped | TTL expiry (auto-destroy) |

## Error Recovery

### Failed Spawn Cleanup

If any spawn step fails after user creation:
1. Roll back: destroy partially-created agent
2. `userdel -r` if user was created
3. Free port range if allocated
4. Return error to caller

### Runtime Crash Detection

- Health check polls `/run/bunker/<id>/docker.sock` periodically
- Missing socket → agent marked `failed`
- Resource tracker notified

### Zombie Process Reaping

- systemd cgroup handles child process cleanup
- `KillMode=control-group` on transient units
- Orphaned containers stopped by dockerd on daemon restart
