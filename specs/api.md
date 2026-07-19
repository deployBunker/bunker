# Bunker — API Specification

Version: 1.0.0
Based on: proto/bunker/v1/bunker.proto
Last Updated: 2026-07-19

## Protocol

Bunker uses Protocol Buffers with connect-go, providing both gRPC and REST (JSON+Protobuf codecs) on a single port.

- gRPC: `:9090` (h2c or TLS)
- REST: `:18080` (HTTP/1.1 + HTTP/2)

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
  - `disk_max_bytes` (uint64): Disk quota
  - `max_docker_containers` (uint32): Container cap
- `network` (NetworkConfig, optional): Ingress configuration
  - `mode`: CLOUDFLARE_TUNNEL, TAILSCALE, or DIRECT
  - `domain`: Custom Cloudflare domain (named tunnel)
  - `trycloudflare`: Use anonymous TryCloudflare tunnel
  - `port_range_start` / `port_range_end`: Per-agent port isolation
- `ttl` (string): Duration like "6h", "24h", "7d"
- `ssh_public_key` (bytes, optional): Push existing key
- `labels` (map<string,string>): Metadata key-value pairs

Response:
- `agent_id` (string): Assigned agent ID
- `docker_host_ssh` (string): `DOCKER_HOST=ssh://bunker-<id>@host`
- `docker_host_tunnel` (string): `ssh -L 2376:...` local tunnel command
- `sshfs_mount` (string): `sshfs bunker-<id>@host:/home/...` command
- `public_url` (string): Cloudflare tunnel URL (if enabled)
- `port_range_start` / `port_range_end` (uint32): Allocated ports
- `ssh_private_key` (string): Generated key (if not pushed)
- `limits` (ResourceLimits): Enforced resource caps
- `expires_at` (string): ISO 8601 expiry timestamp
- `tailnet_ip` (string): Tailscale IP (if enabled)
- `api_key` (string): Agent-scoped API sub-key (if auth enabled)

Error codes:
- `CodeResourceExhausted`: No capacity for requested limits
- `CodeAlreadyExists`: agent_id collision
- `CodeInvalidArgument`: Bad limits or TTL format

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
- `disk_used_bytes`, `disk_limit_bytes` (uint64)
- `docker_containers` (uint32): Running containers
- `uptime` (string): Human-readable uptime

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
| disk_max_bytes | uint64 | Disk quota in bytes |
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

### NetworkConfig

| Field | Type | Values |
|-------|------|--------|
| mode | enum | CLOUDFLARE_TUNNEL, TAILSCALE, DIRECT |
| domain | string | Custom domain (named tunnel) |
| trycloudflare | bool | Anonymous tunnel |
| port_range_start | uint32 | Agent port range start |
| port_range_end | uint32 | Agent port range end |

## Auth Headers

All requests require one of:

```
Authorization: Bearer <master-token>
Authorization: Bearer <jwt>
X-API-Key: <agent-sub-key>
```

Agent-scoped tokens are rejected on Bunkerd service RPCs (master-only).

## REST Mapping

connect-go maps proto RPCs to REST paths:

| RPC | Method | Path |
|-----|--------|------|
| ServerInfo | GET | /bunker.v1.Bunkerd/ServerInfo |
| SpawnAgent | POST | /bunker.v1.Bunkerd/SpawnAgent |
| ListAgents | GET | /bunker.v1.Bunkerd/ListAgents |
| GetAgent | GET | /bunker.v1.Bunkerd/GetAgent |
| DestroyAgent | POST | /bunker.v1.Bunkerd/DestroyAgent |
| ExecAgent | POST | /bunker.v1.Bunkerd/ExecAgent |
| RunAgent | POST | /bunker.v1.Bunkerd/RunAgent |
| ServerMetrics | GET | /bunker.v1.Bunkerd/ServerMetrics |
| AgentMetrics | GET | /bunker.v1.Bunkerd/AgentMetrics |
| HeartbeatAgent | POST | /bunker.v1.Bunkerd/HeartbeatAgent |

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
- `CodeInvalidArgument` (3): Bad request parameters
- `CodeInternal` (13): Server-side failure
