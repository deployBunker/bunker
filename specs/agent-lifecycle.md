# Bunker — Agent Lifecycle Specification

Version: 1.2.0
Status: Stable
Last Updated: 2026-09-12

## Overview

Each agent is an isolated Linux user with a rootless Docker daemon, private SSH keypair, dedicated port range, and optional network ingress. This spec covers the full lifecycle from spawn through runtime to destroy.

## Spawn: Step-by-Step

> **Canonical order in `internal/agent/manager_spawn.go`:** validate TTL + image
> spec → allocate ports → `useradd` → write `authorized_keys`/`.profile` →
> persist the server-side SSH key → `configureSubIDs` → rootlesskit AppArmor
> profile → install rootless Docker (which enables linger first) → create the
> `systemd-run --system` dockerd unit → `waitForDockerd` → runtime dir →
> tunnels → agent API key → response. The numbered sections below describe each
> piece; where a number differs from that order, the code is authoritative.

### 1. Request Validation

```
SpawnAgentRequest → validate limits, TTL format, agent_id uniqueness
```

- If `agent_id` is empty, generate `bunker-<8-char-random>`
- Validate `limits` against server capacity
- Validate `ttl` format: `\d+[hmd]` (e.g., "6h", "24h", "7d") — an empty TTL
  falls back to `agent.default_ttl` (default 6h)
- Validate a supplied `image_spec` (GAP-064) before ANY side effect: base image
  must be in the server allowlist, package directives limited to apt/go/npm;
  a rejected spec returns `CodeInvalidArgument` and builds nothing
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

- The private key is returned in the response AND persisted server-side at
  `<agent.ssh_dir>/<agent-id>` (default `/etc/bunkerd/ssh/<agent-id>`, mode
  0600) so `ExecAgent` / `RunAgent` can SSH in. `DestroyAgent` removes it.
- If `ssh_public_key` provided in request, append to authorized_keys instead
- `environment="DOCKER_HOST=unix:///run/user/<UID>/docker.sock"` prepended for auto socket discovery

### 5. Resource Limits Enforcement

```
systemd-run --system --unit=bunker-docker-<id> --uid=<uid> --gid=<gid> \
  --property=PAMName=login \
  --property=CPUQuota=<pct>% \
  --property=MemoryMax=<bytes> \
  --property=LimitFSIZE=<disk_bytes> \
  --property=TasksMax=<max_procs> \
  --property=LimitNOFILE=<max_fds>:<max_fds> \
  --setenv=... <dockerd-rootless.sh>
```

- The unit is a **system** unit run as the agent user (`--system --uid`), not
  `systemd-run --user --machine=...` — it does not require a running user
  manager/D-Bus for a freshly created user.
- `CPUQuota`: Percentage of one CPU core (100%=1 core, 200%=2 cores); the code
  passes `int(cpuQuota*100)%`
- `MemoryMax`: Absolute byte limit (default: 4 GiB)
- `LimitFSIZE`: Per-file size cap, the pragmatic disk enforcement (default: 20 GiB)
- `TasksMax`: Process count limit (default: **4096**, `agent.default_max_processes`)
- `LimitNOFILE`: Open file limit, passed as `N:N` (default: 65536)

### 6. User Manager Enablement

```bash
loginctl enable-linger bunker-<id>
```

Called inside `installRootlessDocker` **before** the rootless installer runs —
the installer needs the systemd user manager (`systemctl --user`) to exist, and
linger makes the user manager persist so dockerd survives session termination.

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

- On a fresh host this downloads the official `docker-ce-rootless-extras`
  installer and installs rootlesskit + dockerd under `~/bin/` — a **~93 MB
  download taking 60-90s+**. That is why a first spawn on a host is slow;
  later spawns reuse the cached tooling (~10s).
- Ubuntu 24.04 requires an AppArmor profile: `/etc/apparmor.d/home.bunker-<id>.bin.rootlesskit`
  (`ensureRootlesskitAppArmor` runs before the installer)

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
  poll every 200ms:
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
  ssh_private_key, limits, expires_at, tailnet_ip, api_key, image
}
```

`image` is set to the customized image ref (e.g. `bunkerd-imagespec-<key>:latest`)
when `image_spec` was supplied (GAP-064); `expires_at` is RFC3339 in the server's
local timezone.

## Destroy: Step-by-Step

Order per `internal/agent/manager_destroy.go`:

### 0. Agent ID Validation

An invalid `agent_id` still frees the allocator range (idempotent) and returns
`status: "error"`.

### 1. Agent Container Cleanup (step 0.4)

`cleanupAgentContainers` stops/removes the agent's own container through the
agent's rootless socket, **before** the daemon is stopped, so no container leaks
past it. Best-effort.

### 2. User Slice Limits (step 0.5)

`removeUserSliceLimits` deletes the cgroup drop-in
(`/etc/systemd/system/user-<UID>.slice.d/50-bunker.conf`) so stale limits don't
accumulate.

### 3. Docker Shutdown (steps 1-2b)

- `stopDockerdDirect` (SIGTERM, then kill) with a `systemctl --user stop`
  fallback
- `systemctl --user disable bunker-docker-<id>`
- `waitAgentProcessesExit` polls up to 10s, SIGKILLing lingering
  `dockerd`/`rootlesskit` processes, so `userdel` can succeed

### 4. User Removal (step 3)

```bash
userdel -rf bunker-<id>
```
- `-rf`: Remove home directory (and force). Home is deleted — container-mode's
  "home survives destroy" default is a planned deviation, not current behavior.
- Cleans up `/home/bunker-<id>/`, subuid/subgid entries
- A non-force `userdel` failure returns `not_found` (and still frees the port
  range + tracker slot)

### 5. Runtime Cleanup (step 4)

```bash
rm -rf /run/bunker/<id>/
rm /run/user/<UID>/docker.sock      # the real rootless socket
```

### 6. Persisted SSH Key (step 4.5)

```bash
rm <agent.ssh_dir>/<agent-id>       # default /etc/bunkerd/ssh/<agent-id>
```

### 7. Network Teardown

- **cloudflared** tunnel stop (best-effort)
- **tailscale** stop (best-effort)

### 8. Tracker / Port Reclamation

```
tracker.Unregister(agentID)
PortAllocator.Free(agentID)          # unconditional + idempotent
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
2. Server sets `ExpiresAt = now + agent.default_ttl` (default 6h) and **never
   shrinks** an existing longer expiry
3. Returns new expiry timestamp (RFC3339, server-local timezone) and
   `acknowledged: true`

### Run (Detached Commands)

```
bunker run <agent-id> --detach -- docker compose up
```

1. Creates systemd transient unit: `bunkerd-<agent-id>-<name>.service`
2. Unit type: `oneshot` with `RemainAfterExit=yes`
3. Survives exec session termination
4. Managed via `systemctl --user --machine=bunker-<id>@`

## Durable Registry (GAP-070)

Agent lifecycle state is persisted in an append-only JSONL log that the daemon
replays at startup, so agent state survives a `bunkerd` restart. Before GAP-070
the tracker was in-memory only: a restart forgot every agent while its Linux
user, home directory, and containers kept running.

### Log format

Default path `/var/lib/bunkerd/agents.jsonl`, mode `0600`, one JSON object per
line (a torn final line is ignored on replay; a corrupt complete line is logged
and skipped, and every earlier valid event is retained):

```json
{"ts":"2026-09-12T10:00:00Z","kind":"spawn","agent_id":"abc","status":"running","created_at":"...","expires_at":"...","port_start":10000,"port_end":10099,"limits":{...},"ssh_key_path":"/etc/bunkerd/ssh/abc"}
{"ts":"2026-09-12T10:05:00Z","kind":"heartbeat","agent_id":"abc","expires_at":"...","status":"running"}
{"ts":"2026-09-12T11:00:00Z","kind":"destroy","agent_id":"abc"}
{"ts":"...","kind":"known","known_ids":["abc","def"]}
```

Replay folds events in file order (later wins): `spawn` upserts a live record
with the full current state needed to restore the agent, `heartbeat` extends its
expiry (never shrinks), `destroy` removes it from the live set and remembers the
ID, and `known` seeds the bounded destroyed-ID index written by compaction.

### Retention and rotation

The active file is size-capped at `agent.registry.max_bytes` (default 5 MiB) and
rotated through `agent.registry.max_backups` files (default 3:
`agents.jsonl.1` … `.3`). Replay reads the oldest rotation first. Rotation is a
**retention window, not a state transition** — an event that rotates out is
gone, so keep the log small with `bunker registry compact` (a fleet's event
volume is kilobytes; the 20 MiB window is deliberate headroom).

### Compaction

```
bunker registry compact [--path /var/lib/bunkerd/agents.jsonl] [--dry-run]
```

Rewrites the active log to **exactly one current-state record per live agent**,
dropping stale lifecycle events, and prints before/after counts. Consequences:

* destroyed agents disappear except for their IDs, which live in a single
  bounded `known` index record (`agent.registry.known_id_cap`, default 10000,
  newest kept) — that index is what keeps a repeated `destroy` idempotent after
  compaction;
* rotated backups are removed (they are superseded, and re-folding them would
  resurrect stale events);
* the rewrite is atomic (temp file + rename, file and parent directory
  fsync'd), so a crash mid-compaction cannot truncate the registry;
* the daemon and the CLI share an advisory `flock` on `agents.jsonl.lock`, so
  compaction is serialized against spawn/destroy/heartbeat appends and is safe
  with the daemon running (and safe offline).

### Startup reconciliation

`server.Run` replays and reconciles **before** serving traffic and **before**
the TTL reaper ticks. Each agent produces exactly one startup log line:

| Registry | System user | Action |
|---|---|---|
| live record | exists | **restored** into the tracker with its exact port reservation (an unrestorable record is **destroyed** instead — see Failure semantics) |
| live record | missing | **purged** (stale; ID remembered as known) |
| no record | exists | orphan → **destroyed** (default) or **adopted** |
| no record | missing | nothing |

`agent.reconciliation.mode` is `destroy` (default) or `adopt`. Adoption
re-registers the agent and restores its **exact** persisted port sub-range from
`<home>/.bunker/ports` (written at spawn time) via `PortAllocator.Restore`, so a
post-relaunch spawn can never double-allocate the same ports. Adopted agents
carry no durable TTL (zero expiry) and are therefore never reaped.

Adoption is **exact-port or nothing**. Missing or malformed metadata, a
persisted range that is not a legal pool sub-range, or a range another agent
already holds all fail adoption *before* a tracker or registry record can
exist; reconciliation then force-destroys the orphan (one `destroy` action
line, no `adopt` line). An agent whose ports the next spawn could
double-allocate must not be served — a leftover user is cheaper to recreate
than a port collision.

Restoration is held to the same contract: ports are reserved **before** the
tracker record is registered (and released again if registration fails), so an
agent is never half-managed — visible to the RPC surface without the exact
reservation that makes it safe.

A failed system probe (e.g. unreadable `/etc/passwd`) aborts reconciliation
rather than being read as "no agents exist" — otherwise one broken probe would
destroy every orphan and purge every live record.

### Idempotent destroy

`destroy` of an ID that the registry knew **and** whose system user is already
gone succeeds (`destroyed`) instead of returning `not_found`; a never-seen ID
still returns `not_found`. That distinction is durable across replay and
compaction via the bounded known-ID index. Any non-force failure path still
releases the tracker slot and the port range.

### Failure semantics

* A spawn whose registry append fails **rolls back** (user removed, key
  removed, port range released, tracker slot freed) and returns an error — the
  daemon never reports durable success for state it could not persist.
* **Unsafe adoption or restoration falls back to destroy.** An agent is only
  ever served with its exact isolation metadata. If a replayed live record's
  reservation cannot be re-established — range missing, invalid, outside the
  pool, or already held by another agent — reconciliation fails closed for that
  agent: any tracker record and port reservation it holds are dropped, the
  unsafe live registry record is removed (so a restart cannot resurrect it),
  the system agent is force-destroyed through the destroy path, and exactly one
  `destroy` action line is logged. An orphan that cannot be adopted with its
  exact persisted range is destroyed the same way. Running an agent whose ports
  the next spawn could double-allocate is never an option.
* A destroy failure in that fail-closed path is logged as an error and the
  agent is **still** left with no tracker record, no port reservation and no
  live registry record — the following restart re-detects the surviving system
  user as an orphan.
* A daemon that cannot open the registry refuses to start
  (`agent registry unavailable`).
* A failed heartbeat append is logged and tolerated: the in-memory TTL
  extension stands and the next heartbeat retries.

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
