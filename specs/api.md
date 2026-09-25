# Bunker — API Specification

Version: 1.1.0
Based on: proto/bunker/v1/bunker.proto
Last Updated: 2026-09-12

## Protocol

Bunker uses Protocol Buffers with connect-go, providing both gRPC and REST (JSON+Protobuf codecs) on a single port.

- gRPC: `:9090` (h2c or TLS)
- REST: `:8080` (HTTP/1.1 + HTTP/2) — the daemon's configured default (see config.example.yaml); the public demo instance (bunker-mvp, 78.46.173.180) exposes REST on `:18080` and gRPC on `:19090` — demo-instance ports, not defaults.

Both transports serve the same handlers. Auth is transport-agnostic (JWT or static token in `Authorization` header).

## Service: Bunkerd

Master-level authentication required. Manages agent lifecycle and server state.

### ServerInfo

Returns server identity and capacity information.

```
rpc ServerInfo(ServerInfoRequest) returns (ServerInfoResponse)
```

Request: Empty
Response:
- `hostname` (string): Server hostname
- `version` (string): bunkerd semver
- `uptime_seconds` (uint64): Process uptime
- `agent_count` (uint32): Currently running agents
- `max_agents` (uint32): Hard capacity limit
- `total_resources` (ResourceLimits): Total provisionable
- `available_resources` (ResourceLimits): Remaining capacity

### ServerMetrics

Aggregate resource usage across all agents.

```
rpc ServerMetrics(ServerMetricsRequest) returns (ServerMetricsResponse)
```

Request: Empty
Response:
- `cpu_usage_percent` (double)
- `memory_used_bytes` (uint64)
- `memory_total_bytes` (uint64)
- `disk_used_bytes` (uint64)
- `disk_total_bytes` (uint64)
- `docker_containers_total` (uint32)
- `agents` (repeated AgentSummary): Per-agent details

### SpawnAgent

Creates a new isolated agent. This is the core provisioning RPC.

```
rpc SpawnAgent(SpawnAgentRequest) returns (SpawnAgentResponse)
```

Request:
- `agent_id` (string, optional): Auto-generated `bunker-<random>` if empty
- `limits` (ResourceLimits, optional): Falls back to server defaults
  - `cpu_quota` (double): CPU cores (e.g., 2.0)
  - `memory_max_bytes` (uint64): RAM limit
  - `disk_max_bytes` (uint64): Per-file size cap (applied as `LimitFSIZE`/`RLIMIT_FSIZE`), not a total-disk quota (DF-BUNKER-54)
  - `max_docker_containers` (uint32): Container cap
- `network` (NetworkConfig, optional): Ingress configuration
  - `mode`: CLOUDFLARE_TUNNEL, TAILSCALE, or DIRECT
  - `domain`: Custom Cloudflare domain (named tunnel)
  - `trycloudflare`: Use anonymous TryCloudflare tunnel
  - `port_range_start` / `port_range_end`: Per-agent port isolation
- `ttl` (string): Duration like "6h", "24h", "7d"
- `return_ssh_private_key` (bool, optional): GAP-128 opt-in flag. When set,
  the spawn response carries the generated private key (old behavior);
  when unset (the default) the response is key-free and the key stays
  persisted server-side, retrievable via `GetAgentKey`
- `ssh_public_key` (bytes, optional): Push existing key
- `labels` (map<string,string>): Metadata key-value pairs
- `image_spec` (ImageSpec, optional): Secure per-agent image customization
  (GAP-064). A small declarative grammar — NOT Dockerfile text:
  - `base` (string, optional): base image; must be one of the server's
    allowed bases (default: `docker.io/library/ubuntu:24.04`; also
    ubuntu:22.04, debian:12, debian:11)
  - `packages` (repeated PackageAdd, optional): package-add directives;
    `manager` is one of the registered managers — `apt` (apt-get install),
    `go` (go install), `npm` (npm install -g), `pip` (pip install),
    `cargo` (cargo install), `gem` (gem install), `composer`
    (composer global require); each directive lists package names with an
    optional manager-specific version (`name=version` for apt,
    `name@version` for go/npm, a PEP 440 specifier for pip,
    `crate@requirement` for cargo, `name@requirement` for gem,
    `vendor/package:constraint` for composer). Max 16 directives,
    16 packages each. Every token is limited to letters, digits and
    `. + - _ : / @ =` (no slashes for apt) PLUS the version-grammar
    characters that manager's own row declares (pip: `, < > [ ] ! ~`;
    cargo: `, < > ^ ~`; gem/composer: `, < > ^ ~ !`), and every token is
    rendered SINGLE-QUOTED into the RUN line, so shell chaining,
    substitution, redirection, `curl|sh`, mounts/sockets, and any
    non-package command are structurally impossible. Whitespace,
    `; & | $ ` \ ' "` and newlines are refused for EVERY manager.
    Unknown fields (FROM/USER/EXPOSE/VOLUME/ENV/RUN
    lookalikes) are rejected. Validation happens BEFORE any side effect.

Response:
- `agent_id` (string): Assigned agent ID
- `docker_host_ssh` (string): `DOCKER_HOST=ssh://bunker-<id>@host`
- `docker_host_tunnel` (string): `ssh -L 2376:...` local tunnel command
- `sshfs_mount` (string): `sshfs bunker-<id>@host:/home/...` command
- `public_url` (string): Cloudflare tunnel URL (if enabled)
- `port_range_start` / `port_range_end` (uint32): Allocated ports
- `ssh_private_key` (string): **Deprecated (GAP-128).** Empty by default —
  the spawn response carries NO private key material unless the request set
  `return_ssh_private_key: true`. Keys are persisted server-side; fetch one
  explicitly via `GetAgentKey` (master credentials only)
- `limits` (ResourceLimits): Enforced resource caps
- `expires_at` (string): ISO 8601 expiry timestamp
- `tailnet_ip` (string): Tailscale IP (if enabled)
- `api_key` (string): Agent-scoped API sub-key (if auth enabled)
- `image` (string): Customized image ref (e.g.
  `bunkerd-imagespec-<key>:latest`) when `image_spec` was supplied

Error codes:
- `CodeResourceExhausted`: No capacity for requested limits
- `CodeAlreadyExists`: agent_id collision
- `CodeInvalidArgument`: Bad limits, TTL format, or rejected image spec
  (rejected specs build NOTHING — no user, ports, dockerd, or image)

### DestroyAgent

Removes an agent and reclaims all resources.

```
rpc DestroyAgent(DestroyAgentRequest) returns (DestroyAgentResponse)
```

Request:
- `agent_id` (string): Agent to destroy
- `force` (bool): Kill running processes if true

Response:
- `agent_id` (string): Destroyed agent
- `status` (string): "destroyed", "not_found", "error"

Cleanup steps (in order):
1. Kill tunnel processes (cloudflared, tailscale)
2. Stop dockerd via systemd unit
3. Disable systemd unit
4. `userdel -r bunker-<id>`
5. Remove `/run/bunker/<id>/`
6. Free port range in allocator
7. Remove from resource tracker

### StopAgent

Pauses an agent **without destroying it** (GAP-071): the agent's session units
and processes are stopped so it stops consuming CPU, while the Linux user, home
directory, container and allocated port range are all KEPT, and the resource
tracker keeps the record with status `stopped`.

```
rpc StopAgent(StopAgentRequest) returns (StopAgentResponse)
```

Request:
- `agent_id` (string): Agent to stop

Response:
- `agent_id` (string): Stopped agent
- `status` (string): `"stopped"`, `"already_stopped"`, `"not_found"`, `"error"`

Stop steps (in order), each best-effort — a leg that is already down does not
fail the stop:

1. Container-mode agent (one spawned with an image spec): `docker stop -t 5`
   against the agent's container through the **agent's own** rootless socket.
   The container is stopped, never removed.
2. `systemctl --user stop bunker-docker-<id>` and, when discoverable,
   `systemctl --user stop` for the agent's own `bunker-run-<id>-*` transient
   units.
3. The agent's `dockerd`/`rootlesskit` processes are signalled (SIGTERM →
   SIGKILL) and awaited, exactly as the destroy path does.

NOT done by stop (these are Destroy's steps): `userdel`, port range release,
`/run/bunker/<id>` removal, home-directory removal, SSH-key removal.
Stopping an already-stopped agent reports `already_stopped` and issues no host
command (idempotent). An unknown id reports `not_found` with
`CodeNotFound` — never a panic.

### StartAgent

Re-arms a stopped agent (GAP-071). The user and home were kept, so the session
is restored: the agent's unit is started again (and a container-mode agent's
kept container is started through the agent's own socket), and the status
returns to `running`. The existing heartbeat expiry is left as it is —
`RestartAgent` is the call that resets the TTL.

```
rpc StartAgent(StartAgentRequest) returns (StartAgentResponse)
```

Request:
- `agent_id` (string): Agent to start

Response:
- `agent_id` (string): Started agent
- `status` (string): `"started"`, `"already_running"`, `"not_found"`, `"error"`

An agent that is not stopped reports `already_running` and issues no host
command. An unknown id reports `not_found` with `CodeNotFound`.

### RestartAgent

Stops and starts an agent in one call and **resets the heartbeat expiry** — the
recovery path for a wedged session (GAP-071). Nothing is destroyed.

```
rpc RestartAgent(RestartAgentRequest) returns (RestartAgentResponse)
```

Request:
- `agent_id` (string): Agent to restart

Response:
- `agent_id` (string): Restarted agent
- `status` (string): `"restarted"`, `"not_found"`, `"error"`
- `expires_at` (string): Heartbeat expiry after the reset (RFC3339,
  server-local timezone)

The stop legs run unconditionally (a wedged session may be half-dead while the
tracker says nothing useful), then the start legs, then the expiry is **set**
to `now + agent.default_ttl` (6h unless configured). This is deliberately NOT
the heartbeat rule: a heartbeat never shrinks a longer expiry, while a restart
resets the TTL clock — an agent whose expiry was further out ends up at
`now + default_ttl`.

### Stopped agents: the `agent_stopped` error

ExecAgent, RunAgent and HeartbeatAgent against a **stopped** agent fail with
`CodeFailedPrecondition` and a message containing the stable token
`agent_stopped`, e.g.:

```
agent_stopped: agent "abc12345" is stopped; run 'bunker start abc12345'
```

This is a distinct signal, not `CodeNotFound`: the agent exists, so a client
should start or restart it instead of concluding it is gone. Unknown ids keep
returning `CodeNotFound`, and behaviour for running agents is unchanged.

A stopped agent is still subject to TTL expiry like a running one (its record
keeps its `ExpiresAt`), so a pause longer than the remaining TTL is destroyed
by the reaper — heartbeat a stopped agent is rejected rather than silently
extending it.

### ListAgents

Paginated agent listing with optional status filter.

```
rpc ListAgents(ListAgentsRequest) returns (ListAgentsResponse)
```

Request:
- `status_filter` (string, optional): "running", "stopped", "all"
- `page_size` (uint32): Results per page
- `page_token` (string): Pagination cursor

Response:
- `agents` (repeated AgentSummary): Page of agents
- `next_page_token` (string): Cursor for next page
- `total_count` (uint32): Total matching agents

### GetAgent

Single agent lookup by ID.

```
rpc GetAgent(GetAgentRequest) returns (GetAgentResponse)
```

Request:
- `agent_id` (string)

Response:
- `agent` (AgentSummary): Full agent details

Error codes:
- `CodeNotFound`: agent_id does not exist

### AgentMetrics

Per-agent resource usage snapshot.

```
rpc AgentMetrics(AgentMetricsRequest) returns (AgentMetricsResponse)
```

Request:
- `agent_id` (string)

Response:
- `agent_id`, `status`
- `cpu_usage_percent` (double)
- `memory_used_bytes`, `memory_limit_bytes` (uint64)
- `disk_used_bytes`, `disk_limit_bytes` (uint64) — `disk_limit_bytes` is the
  agent's PER-FILE size cap (`LimitFSIZE`/`RLIMIT_FSIZE`), **not** a total-disk
  limit; see `ResourceLimits.disk_max_bytes` (DF-BUNKER-54)
- `docker_containers` (uint32): Running containers
- `uptime` (string): Human-readable uptime
- `host_level_fallback` (bool): true when the memory values came from the host
  cgroup (`/proc/meminfo`), i.e. the agent's own cgroup slice was unreadable —
  the numbers are host figures, not the agent's. `bunker metrics <id>` prints an
  explicit `NOTE: host-level fallback` line in that case (GAP-060).

### ExecAgent

Execute a command inside an agent and stream output.

```
rpc ExecAgent(ExecAgentRequest) returns (stream ExecAgentResponse)
```

Request:
- `agent_id` (string)
- `command` (string): Binary to execute
- `args` (repeated string): Command arguments
- `timeout_seconds` (uint32): Execution timeout
- `raw` (bool): If true, exec directly (no shell interpretation)
- `script_content` (string, optional): Upload + execute script file

Response (streamed):
- `stdout` (bytes): Standard output chunk
- `stderr` (bytes): Standard error chunk
- `exit_code` (int32): Command exit code (only in final message)

Implementation: SSH into agent via private key, run `DOCKER_HOST=unix:///run/bunker/<id>/docker.sock <command>`.

### RunAgent

Execute a command with optional persistence (systemd transient unit).

```
rpc RunAgent(RunAgentRequest) returns (RunAgentResponse)
```

Request:
- `agent_id` (string)
- `command`, `args`: Command to run
- `env` (map<string,string>): Environment variables
- `detach` (bool): Start as persistent background unit
- `timeout_seconds` (uint32)
- `name` (string, optional): Suffix for systemd unit name

Response:
- `run_id` (string): Unique run identifier
- `status` (string): "running", "completed", "failed"
- `exit_code` (int32): -1 for detached (still running)
- `unit_name` (string): systemd unit name for detached runs

Detached runs create a systemd transient unit that survives the exec session. Use `bunker run <agent> --detach -- docker compose up` for persistent services.

### HeartbeatAgent

Extend agent TTL. Does not change resource limits.

```
rpc HeartbeatAgent(HeartbeatAgentRequest) returns (HeartbeatAgentResponse)
```

Request:
- `agent_id` (string)

Response:
- `agent_id` (string)
- `expires_at` (string): New expiry timestamp
- `acknowledged` (bool): Always true on success

### GetAgentKey

Returns the agent's persisted SSH private key (GAP-128: `SpawnAgent` no longer
carries key material by default — set `return_ssh_private_key` there to keep
the old inline behavior, or call this after spawn). Master-credential gated
exactly like exec/destroy: agent-scoped sub-keys are rejected before the
handler runs.

```
rpc GetAgentKey(GetAgentKeyRequest) returns (GetAgentKeyResponse)
```

Request:
- `agent_id` (string)

Response:
- `agent_id` (string)
- `ssh_private_key` (string): PEM private key (the server-side persisted copy)

Error codes:
- `CodeNotFound`: agent not found, or the agent has no persisted SSH private key

### RenewalDriftReport

Read-only pre-renewal scan (DF-BUNKER-34): reports every reference to a given
(old) home path across the artifact classes a renewal goes stale in — systemd
`--user` units, cron entries, shell/env and config files. It reports, it never
rewrites. `bunker renew` runs this scan as its pre-flight and prints the
summary verbatim.

```
rpc RenewalDriftReport(RenewalDriftRequest) returns (RenewalDriftResponse)
```

Request:
- `agent_id` (string): the agent whose home is scanned
- `old_home` (string, required): the path searched for (the previous home
  path) — a scan with no needle would read as "clean" without proving anything
- `home` (string, optional): the directory to scan; empty = the agent's own
  home (`/home/bunker-<agent_id>`)

Response:
- `agent_id` (string)
- `home` (string): the home that was scanned
- `old_home` (string): the path searched for
- `files_scanned` (uint32)
- `hits` (repeated RenewalDriftHit): `file` (path relative to the home), `line`
  (1-based), `text` (the trimmed line content)
- `unreadable` (repeated string): files that exist but could not be read
- `summary` (string): the operator-facing one-line-per-hit rendering (what the
  CLI prints verbatim)

### RotateJWTSecret

Rotates the HS256 JWT **signing secret** without downtime (GAP-132). The new
secret signs immediately — no restart needed — while the retired secret keeps
validating existing tokens for a bounded dual-accept overlap window, after
which they are rejected. The response carries the new secret exactly once; the
daemon never echoes it again. This is NOT a bearer token: never paste the
signing secret into a server entry's `token:` field (the static `auth.token`
from `/etc/bunkerd/config.yaml` is unchanged by rotation, and a generated
secret that would collide with it is refused).

```
rpc RotateJWTSecret(RotateJWTSecretRequest) returns (RotateJWTSecretResponse)
```

Request:
- `overlap_seconds` (uint32, optional): dual-accept window in seconds; 0 =
  default (10 minutes), values beyond 1 hour are clamped to 1 hour

Response:
- `jwt_secret` (string): the NEW signing secret — shown exactly once
- `rotated_at` (string): RFC3339 timestamp
- `overlap_seconds` (uint32): the window actually applied
- `previous_fingerprint` (string): sha256 fingerprint (first 12 hex chars) of
  the retired secret — fingerprint only, never the value

Error codes:
- `CodeFailedPrecondition`: JWT auth is not configured on this server
- `CodeInvalidArgument`: the generated secret collides with the static
  `auth.token` (credential classes must stay disjoint; retry the rotation)

### RevokeKey

Revokes an API sub-key by key ID. Immediate — the credential stops validating
before this response is sent — and durable: the revocation marker is persisted
so it survives a daemon restart. Credentials issued under a revoked key stop
working with it.

```
rpc RevokeKey(RevokeKeyRequest) returns (RevokeKeyResponse)
```

Request:
- `key_id` (string, required)

Response:
- `key_id` (string)
- `status` (string): `"revoked"` on success

Error codes:
- `CodeInvalidArgument`: empty `key_id`
- `CodeNotFound`: unknown `key_id`

### KeyList

Lists active API sub-keys — metadata only, no secret material (GAP-132).

```
rpc KeyList(KeyListRequest) returns (KeyListResponse)
```

Request:
- `agent_id` (string, optional): exact filter; empty = all keys

Response:
- `keys` (repeated KeyInfo): each with `key_id`, `agent_id`, `created_at`
  (RFC3339), `expires_at` (RFC3339), `revoked` (revoked keys are listed with
  `revoked=true` until they expire out)

## Service: Agent

Agent-scoped authentication. Only accessible with a scoped API key or JWT containing the agent's `agent_id`.

### GetInfo

Returns the agent's own details.

```
rpc GetInfo(GetInfoRequest) returns (GetInfoResponse)
```

Response:
- `agent_id`, `status`, `docker_host`, `public_url`
- `limits` (ResourceLimits)
- `expires_at` (string)

### Metrics

Same schema as Bunkerd.AgentMetrics, scoped to the calling agent.

### Heartbeat

Same schema as Bunkerd.HeartbeatAgent, scoped to the calling agent.

## Common Types

### ResourceLimits

| Field | Type | Description |
|-------|------|------------|
| cpu_quota | double | CPU cores, e.g. 2.0 |
| memory_max_bytes | uint64 | Memory limit in bytes |
| `disk_max_bytes` | uint64 | Per-file size cap in bytes (applied as `LimitFSIZE`/`RLIMIT_FSIZE`) — **not** a total-disk quota (DF-BUNKER-54) |
| max_docker_containers | uint32 | Max concurrent containers |

### AgentSummary

| Field | Type | Description |
|-------|------|------------|
| agent_id | string | Unique identifier |
| status | string | pending/starting/running/stopping/stopped/failed |
| limits | ResourceLimits | Enforced resource caps |
| created_at | string | ISO 8601 creation timestamp |
| expires_at | string | ISO 8601 expiry timestamp |
| sshfs_mount | string | sshfs mount command |
| docker_host_tunnel | string | SSH tunnel command for local Docker access |
| public_url | string | Cloudflare tunnel URL |
| port_range_start | uint32 | First port in agent's allocation |
| port_range_end | uint32 | Last port in agent's allocation |
| tailnet_ip | string | Tailscale IP address |
| disk_used_bytes | uint64 | Per-agent disk usage in bytes |

### NetworkConfig

| Field | Type | Values |
|-------|------|--------|
| mode | enum | CLOUDFLARE_TUNNEL, TAILSCALE, DIRECT |
| domain | string | Custom domain (named tunnel) |
| trycloudflare | bool | Anonymous tunnel |
| port_range_start | uint32 | Agent port range start |
| port_range_end | uint32 | Agent port range end |

### QueryAudit

Read-only query over the daemon's audit trail (master auth). Returns matching
records oldest-first from the live log plus rotated backups (`.1`-`.3`).

```
rpc QueryAudit(QueryAuditRequest) returns (QueryAuditResponse)
```

Request (all filters optional and ANDed; empty matches everything):
- `agent_id` (string): exact match
- `method` (string): substring match on the full procedure, e.g. `SpawnAgent`
- `since` / `until` (string): RFC3339 bounds, inclusive
- `limit` (uint32): max records, keeping the NEWEST matches; 0 = no limit

Response:
- `records` (repeated AuditRecord): `ts`, `caller`, `method`, `remote_addr`,
  `agent_id`, `duration_ms`, `outcome`, `summary`, `hash`, `prev_hash` — the same
  fields the JSONL log stores.

Error codes:
- `CodeInvalidArgument`: unparseable `since`/`until`
- `CodeUnavailable`: audit logging is disabled on the daemon

See [docs/audit.md](../docs/audit.md) for the record format, hash chain, and the
`bunker audit list/export` client surface.

## Auth Headers

All requests require one of:

```
Authorization: Bearer <master-token>
Authorization: Bearer <jwt>
X-API-Key: <agent-sub-key>
```

Agent-scoped tokens are rejected on Bunkerd service RPCs (master-only).

## REST Mapping

connect-go maps proto RPCs to REST paths. The handler is created without the
`WithHTTPGet` option, so every RPC is served over **POST only** — there are no
read-style HTTP endpoints.

### Bunkerd service (master token)

| RPC | Method | Path |
|-----|--------|------|
| ServerInfo | POST | /bunker.v1.Bunkerd/ServerInfo |
| ServerMetrics | POST | /bunker.v1.Bunkerd/ServerMetrics |
| SpawnAgent | POST | /bunker.v1.Bunkerd/SpawnAgent |
| DestroyAgent | POST | /bunker.v1.Bunkerd/DestroyAgent |
| StopAgent | POST | /bunker.v1.Bunkerd/StopAgent |
| StartAgent | POST | /bunker.v1.Bunkerd/StartAgent |
| RestartAgent | POST | /bunker.v1.Bunkerd/RestartAgent |
| ListAgents | POST | /bunker.v1.Bunkerd/ListAgents |
| GetAgent | POST | /bunker.v1.Bunkerd/GetAgent |
| AgentMetrics | POST | /bunker.v1.Bunkerd/AgentMetrics |
| ExecAgent | POST | /bunker.v1.Bunkerd/ExecAgent |
| RunAgent | POST | /bunker.v1.Bunkerd/RunAgent |
| HeartbeatAgent | POST | /bunker.v1.Bunkerd/HeartbeatAgent |
| QueryAudit | POST | /bunker.v1.Bunkerd/QueryAudit |
| GetAgentKey | POST | /bunker.v1.Bunkerd/GetAgentKey |
| RenewalDriftReport | POST | /bunker.v1.Bunkerd/RenewalDriftReport |
| RotateJWTSecret | POST | /bunker.v1.Bunkerd/RotateJWTSecret |
| RevokeKey | POST | /bunker.v1.Bunkerd/RevokeKey |
| KeyList | POST | /bunker.v1.Bunkerd/KeyList |

### Agent service (scoped sub-key)

| RPC | Method | Path |
|-----|--------|------|
| GetInfo | POST | /bunker.v1.Agent/GetInfo |
| Metrics | POST | /bunker.v1.Agent/Metrics |
| Heartbeat | POST | /bunker.v1.Agent/Heartbeat |

> **POST-only:** connect-go serves all RPCs over POST (HTTP/1.1 and HTTP/2);
> no route accepts any other method. Sending any non-POST request to one of
> the paths above returns `405 Method Not Allowed`. The auth interceptor runs
> on the POST path only, so an unauthenticated request receives `401
> Unauthenticated` only when sent as POST — a non-POST request returns 405
> instead of 401 because it never reaches the interceptor. curl users must
> send `-X POST` with a JSON body (`Content-Type: application/json`) and the
> `Authorization: Bearer <token>` header.

Content-Type: `application/json` or `application/proto`.

## Error Model

All RPCs return connect-go errors with:

- `code`: Standard gRPC status code
- `message`: Human-readable error description
- `details` (optional): Machine-readable error details

Common error codes:
- `CodeUnauthenticated` (16): Missing or invalid auth
- `CodePermissionDenied` (7): Agent-scoped token on master endpoint
- `CodeNotFound` (5): Agent not found
- `CodeAlreadyExists` (6): Agent ID collision
- `CodeResourceExhausted` (8): No capacity
- `CodeFailedPrecondition` (9): The agent exists but is not runnable — the
  stopped-agent case. The message carries the stable token `agent_stopped`,
  e.g. `agent_stopped: agent "abc12345" is stopped; run 'bunker start abc12345'`
  (GAP-071; only ExecAgent / RunAgent / HeartbeatAgent return it)
- `CodeInvalidArgument` (3): Bad request parameters
- `CodeInternal` (13): Server-side failure
