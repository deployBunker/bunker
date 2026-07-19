# Package: `internal/resource`

## Public API

- `PortAllocator` — allocates per-agent port sub-ranges from a configured pool.
- `NewPortAllocator(start, end, rangeSize)` — creates an allocator with a contiguous port range divided into fixed-size sub-ranges.
- `(*PortAllocator) Allocate(agentID)` — assigns the next free sub-range; returns start port.
- `(*PortAllocator) Free(agentID)` — releases a sub-range back to the free pool.
- `(*PortAllocator) Get(agentID)` — returns the currently assigned sub-range start (or 0).
- `Tracker` — in-memory agent state registry with capacity enforcement.
- `AgentRecord` — tracked state: AgentID, Status, Limits, timestamps, port range, public URL, SSH key path, tailnet IP, tunnel commands.
- `NewTracker(maxAgents, logger)` — creates a tracker with a hard capacity ceiling.
- `(*Tracker) Register(rec)` / `Unregister(agentID)` — add/remove agents; enforces capacity.
- `(*Tracker) Get(agentID)` / `List()` / `Count()` / `MaxAgents()` / `HasCapacity(n)` — query methods.
- `(*AgentRecord) ToAgentSummary()` — converts to the proto `AgentSummary`.
- `CgroupManager` — applies CPU/memory cgroup limits to agent processes.

## Conventions

- Port sub-ranges are allocated from a free stack (LIFO) for locality.
- Agent statuses: `running`, `stopped`, `failed`.
- `Register` fails if capacity is full or agent ID already exists.
- `Unregister` is idempotent — no error if the agent doesn't exist.
- Capacity check is atomic (under write lock).

## Dependencies

- `proto/bunker/v1` — `ResourceLimits`, `AgentSummary` proto types.
- Standard library: `fmt`, `log/slog`, `sync`, `time`.

## Test Patterns

- `portalloc_test.go`: allocation, exhaustion, free+reuse, invalid range (start >= end, zero range size).
- `cgroup_test.go`: CPU/memory limit parsing, cgroup path construction.
- `tracker_test.go`: register/unregister, capacity enforcement, duplicate detection, list ordering.

## Pitfalls

1. **`PortAllocator.Free` does not validate ownership.** Any caller can free any agent's port range. The caller (agent manager) must ensure correct pairing.
2. **Tracker is in-memory only.** Agent state is lost on process restart. There is no persistence or recovery.
3. **Cgroup paths are Linux-specific.** Tests that construct cgroup paths will fail on macOS/Windows. Use build tags or `runtime.GOOS` guards.
4. **`ToAgentSummary` formats timestamps with RFC3339.** If the proto definition changes format, this method must be updated to match.
