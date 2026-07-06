# Package: `proto/bunker/v1`

## Public API

- `bunker.proto` — the source of truth for the Bunker API. Defines two services:
  - `Bunkerd` — server daemon API (`ServerInfo`, `ServerMetrics`, `SpawnAgent`, `DestroyAgent`, `ListAgents`, `GetAgent`, `AgentMetrics`, `ExecAgent`, `HeartbeatAgent`).
  - `Agent` — scoped sub-key API (`GetInfo`, `Metrics`, `Heartbeat`).
- Generated Go types (`bunker.pb.go`) and connect-go handler/client code (`bunkerv1connect/bunker.connect.go`).
- Key messages: `ResourceLimits`, `NetworkConfig`, `AgentSummary`, `SpawnAgentRequest`, `SpawnAgentResponse`, `DestroyAgentRequest`, `DestroyAgentResponse`, `ListAgentsRequest`, `ListAgentsResponse`, `GetAgentRequest`, `GetAgentResponse`, `AgentMetricsRequest`, `AgentMetricsResponse`, `HeartbeatAgentRequest`, `HeartbeatAgentResponse`, `ExecAgentRequest`, `ExecAgentResponse`, `ServerInfoRequest`, `ServerInfoResponse`, `ServerMetricsRequest`, `ServerMetricsResponse`, `GetInfoRequest`, `GetInfoResponse`.

## Conventions

- Field numbers are stable and must not be reused. Any schema change must be additive or use a new major version directory.
- All resource limits are bytes or counts; durations are passed as human-readable strings (`"6h"`) in `SpawnAgentRequest.Ttl` and parsed server-side.
- `AgentSummary.Status` values are strings: `pending`, `starting`, `running`, `stopping`, `stopped`, `failed`.
- `NetworkConfig.Mode` is a proto enum: `MODE_UNSPECIFIED`, `MODE_CLOUDFLARE_TUNNEL`, `MODE_TAILSCALE`, `MODE_DIRECT`.
- `SpawnAgentResponse` includes generated connection strings (`docker_host_ssh`, `docker_host_tunnel`, `sshfs_mount`) and the agent-scoped `api_key` when JWT auth is enabled.
- `ExecAgentResponse` uses a `oneof output { stdout, stderr }` so the server streams mixed output; `exit_code` is only meaningful in the final message.
- `AgentSummary` carries both limits and runtime connection metadata (`public_url`, `tailnet_ip`, `port_range_start`, `port_range_end`).

## Dependencies

- `buf` and `connect-go` plugins for code generation (`buf generate`).
- Go package: `github.com/deployBunker/bunker/proto/bunker/v1;bunkerv1`.
- `connect-go` generated code depends on `connectrpc.com/connect`.

## Test Patterns

- Proto changes are not directly unit-tested; instead, generated code must compile with `go build ./...`.
- E2E tests exercise the full RPC surface via the generated `bunkerv1connect` client.
- When adding a new field, verify both Go and JSON wire encodings (connect-go supports both Protobuf and JSON codecs).
- Keep backward compatibility: never change a field number or type; deprecate old fields rather than deleting them.

## Pitfalls

1. **`.pb.go` files are gitignored.** CI and local builds must run `buf generate` before `go build`; otherwise `github.com/deployBunker/bunker/proto/bunker/v1` will be missing and the build fails.
2. **Field numbers are frozen after the first commit.** Changing a field number breaks wire compatibility and can corrupt data across client/server versions. Always add new fields with the next available number.
3. **`oneof` fields in Go produce accessor methods with pointer returns.** `ExecAgentResponse` output methods may return nil; callers must check `msg.GetStdout() != nil` before printing.
4. **JSON codec naming follows proto JSON spec.** Field names like `docker_host_ssh` become `dockerHostSsh` in JSON unless the server explicitly configures lower-camel serialization. Test both clients if mixing JSON and Protobuf traffic.
5. **Enums default to `0` (`MODE_UNSPECIFIED`).** The server must treat `MODE_UNSPECIFIED` as the default mode (currently direct/none) rather than a valid explicit choice; otherwise clients that omit the field may accidentally trigger tunnel logic.
6. **Adding `repeated` fields with defaults requires careful zero-value handling.** For example, `ServerMetricsResponse.agents` is a slice; if `List()` returns nil, the slice is empty and JSON clients see `[]`.
