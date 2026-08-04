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
- `(*Tracker) UpdateStatus(agentID, status)` — changes an agent's status in place.
- `(*Tracker) Get(agentID)` / `List()` / `Count()` / `MaxAgents()` / `HasCapacity(n)` — query methods.
- `(*AgentRecord) ToAgentSummary()` — converts to the proto `AgentSummary`; includes `DiskUsedBytes` (per-agent disk usage, MONITOR-001).
- `CgroupManager` — applies CPU/memory cgroup limits to agent processes.
- `CPUSampler` in `cpu_sampler.go` (DOGFOOD-006) — real CPU usage measurement:
  - `NewCPUSampler()` + `(*CPUSampler) Percent() float64` — delta of `cpu.stat` `usage_usec` between samples, mutex-guarded, baseline taken on first call (first sample reads ~0%, never a hardcoded 0).
- Cgroup/metrics reading (DOGFOOD-006) — used by the server for live status/metrics:
  - `ReadCgroupMetrics() (*CgroupMetrics, error)` — reads host cgroup v2 CPU+memory; falls back to `/proc/meminfo` (`MemTotal`→limit, `MemTotal−MemAvailable`→used) when `/sys/fs/cgroup/memory.current`/`memory.max` are unreadable (absent on cgroup2fs/systemd 255 hosts).
  - `parseUsageUsec(cpuStat string) (uint64, bool)` / `parseMeminfo(meminfo string) (totalBytes, availableBytes uint64)` / `readMeminfo()` — raw parsers, table-tested.
  - `ReadAgentCgroupLimits(uid, agentID) (*CgroupMetrics, error)` — per-agent cgroup limits.
- Cgroup path helpers (Linux-specific): `agentCgroupBase(uid, agentID)`, `CgroupCPUSharesPath(agentID)`, `CgroupMemoryPath(agentID)`, `CgroupCPUPath(uid, agentID)`, `CgroupMemoryLimitPath(uid, agentID)`.

## Conventions

- Port sub-ranges are allocated from a free stack (LIFO) for locality.
- Agent statuses: `running`, `stopped`, `failed`.
- `Register` fails if capacity is full or agent ID already exists.
- `Unregister` is idempotent — no error if the agent doesn't exist.
- Capacity check is atomic (under write lock).
- Metrics prefer cgroup v2 files when present; `/proc/meminfo` is the documented fallback, never an error path.

## Dependencies

- `proto/bunker/v1` — `ResourceLimits`, `AgentSummary` proto types.
- Standard library: `bufio`, `fmt`, `log/slog`, `os`, `strconv`, `strings`, `sync`, `time`.

## Test Patterns

- `portalloc_test.go`: allocation, exhaustion, free+reuse, invalid range (start >= end, zero range size).
- `cgroup_test.go`: CPU/memory limit parsing, cgroup path construction, `parseMeminfo` missing-field handling, meminfo-fallback vs cgroup-preferred precedence (`TestReadCgroupMetrics_MeminfoFallback`, `TestReadCgroupMetrics_CgroupPreferredOverMeminfo`), `TestParseUsageUsec_Valid/Missing`, `TestReadCgroupMetrics_NoError`.
- `tracker_test.go`: register/unregister, capacity enforcement, duplicate detection, list ordering.

## Pitfalls

1. **`PortAllocator.Free` does not validate ownership.** Any caller can free any agent's port range. The caller (agent manager) must ensure correct pairing.
2. **Tracker is in-memory only.** Agent state is lost on process restart. There is no persistence or recovery.
3. **Cgroup paths are Linux-specific.** Tests that construct cgroup paths will fail on macOS/Windows. Use build tags or `runtime.GOOS` guards.
4. **`ToAgentSummary` formats timestamps with RFC3339.** If the proto definition changes format, this method must be updated to match.
5. **`CPUSampler` is delta-based.** `Percent()` returns 0 on the first sample (baseline); a single sample is meaningless — always sample twice for a real reading (DOGFOOD-006).
6. **cgroup v2 memory files are not guaranteed to exist.** `/sys/fs/cgroup/memory.current` + `memory.max` are absent on some systemd 255 hosts — callers must tolerate the `/proc/meminfo` fallback instead of treating it as an error.
