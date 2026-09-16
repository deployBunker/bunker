# Package: `internal/resource`

## Public API

- `PortAllocator` — allocates per-agent port sub-ranges from a configured pool.
- `NewPortAllocator(start, end, rangeSize)` — creates an allocator with a contiguous port range divided into fixed-size sub-ranges.
- `(*PortAllocator) Allocate(agentID)` — assigns the next free sub-range; returns `(rangeStart, rangeEnd, error)`. Duplicate IDs and an exhausted pool are errors.
- `(*PortAllocator) Free(agentID)` — releases a sub-range back to the free pool (no-op for an unknown ID).
- `(*PortAllocator) Reserve(agentID, start, end)` — claims an EXACT sub-range, validating it against the pool geometry (inside bounds, aligned to `rangeSize`, exact width) and against the free list; idempotent for the same agent + same range; fails when another agent holds it (`ErrRangeUnavailable` wrapped, GAP-070/ca01d28).
- `(*PortAllocator) Restore(agentID, start, end)` — the replay/adopt-facing form of `Reserve` (identical validation and idempotency): a reconciled/adopted agent keeps the exact ports it had so a later `Allocate` cannot hand them to a second agent (GAP-070/ca01d28).
- `(*PortAllocator) ValidateRange(start, end)` — exported pre-check of the same geometry rule, for callers restoring persisted metadata.
- `(*PortAllocator) AllocatedRange(agentID)` — the exact sub-range currently held (start, end, ok); `Has(agentID)` / `Available()` / `Allocated()` / `MaxRanges()` — pool introspection.
- `Tracker` — in-memory agent state registry with capacity enforcement.
- `AgentRecord` — tracked state: AgentID, Status, Limits, timestamps, port range, public URL, SSH key path, tailnet IP, tunnel commands, per-agent disk usage, and `Image` — the ref of the per-agent customized image built from the spawn's image spec (GAP-064). When non-empty, exec must run inside a container of that image through the agent's own rootless dockerd instead of the bare host user context (GAP-069, 82e4140); empty for agents spawned without an image spec.
- `NewTracker(maxAgents, logger)` — creates a tracker with a hard capacity ceiling.
- `(*Tracker) Register(rec)` / `Unregister(agentID)` — add/remove agents; enforces capacity.
- `(*Tracker) UpdateStatus(agentID, status)` — changes an agent's status in place.
- `(*Tracker) Get(agentID)` / `List()` / `Count()` / `MaxAgents()` / `HasCapacity(n)` — query methods.
- `(*AgentRecord) ToAgentSummary()` — converts to the proto `AgentSummary`; includes `DiskUsedBytes` (per-agent disk usage, MONITOR-001).
- `CgroupManager` — applies CPU/memory cgroup limits to agent processes.
- `CPUSampler` in `cpu_sampler.go` (DOGFOOD-006) — real CPU usage measurement:
  - `NewCPUSampler()` + `(*CPUSampler) Percent() float64` — delta of `cpu.stat` `usage_usec` between samples, mutex-guarded, baseline taken on first call (first sample reads ~0%, never a hardcoded 0).
- Cgroup/metrics reading (DOGFOOD-006) — used by the server for live status/metrics:
  - `ReadCgroupMetrics() (*CgroupMetrics, error)` — HOST-level cgroup v2 CPU+memory; falls back to `/proc/meminfo` (`MemTotal`→limit, `MemTotal−MemAvailable`→used) when `/sys/fs/cgroup/memory.current`/`memory.max` are unreadable (absent on cgroup2fs/systemd 255 hosts).
  - `parseUsageUsec(cpuStat string) (uint64, bool)` / `parseMeminfo(meminfo string) (totalBytes, availableBytes uint64)` / `readMeminfo()` — raw parsers, table-tested.
  - `ReadAgentCgroupLimits(uid, agentID) (*CgroupMetrics, error)` — per-agent cgroup LIMITS read best-effort from the systemd user unit path `user@<uid>.service/bunker-docker-<agentID>.service` (cpu.max, memory.max); zero-valued, never an error, when the files are unreadable — the authoritative limits remain the systemd unit properties.
  - `ReadAgentCgroupMetrics(uid, agentID) (*CgroupMetrics, error)` (DOGFOOD-011: 5836a85 + ef334f7; GAP-060: 61dafd2) — the PER-AGENT memory/usage read. It reads the agent USER SLICE, `/sys/fs/cgroup/user.slice/user-<uid>.slice` — NOT the dockerd unit path, and NOT the host root cgroup: all of an agent's processes (the systemd-run rootless dockerd unit AND every SSH session scope, i.e. every `bunker exec`) live under that slice, and its `memory.max` is the agent's real `--memory` limit. Reads `memory.current` (used), `memory.max` (`max` = unlimited, left for fallback), and `cpu.max` (quota/period). MANDATORY degradation: when the agent cgroup is absent (stopped/destroyed agent, deleted user) or any memory field is unreadable, the missing fields fall back to the HOST-level `ReadCgroupMetrics` (which itself falls back to /proc/meminfo), `CgroupMetrics.HostLevelFallback` is set TRUE, and the function still returns a nil error — callers can always render a coherent response. When both reads fail the result is a zero-valued struct. `HostLevelFallback` is surfaced as the proto wire field `host_level_fallback` (field 10 of `AgentMetricsResponse`, proto/bunker/v1/bunker.proto, 61dafd2); `internal/server`'s AgentMetrics sets it (and flags it directly when the agent user cannot be resolved at all — it never reads user-0.slice as if it were an agent), and `bunker metrics <id>` prints an explicit NOTE that the values are HOST values, not agent values.
- `CgroupMetrics` — CPUUsagePercent (always 0 from these readers; callers hold a `CPUSampler` for real percentages), MemoryUsedBytes, MemoryLimitBytes, CPUQuota, and `HostLevelFallback` (true when any memory field came from the host-level fallback — surface it so host values are never mistaken for agent values).
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
- `gap070_portalloc_test.go` (ca01d28): `Reserve`/`Restore` semantics — exact-range claim + idempotency for the same agent+range, geometry validation (misaligned/out-of-pool/wrong-width rejected), other-agent conflicts surface as `ErrRangeUnavailable`, and restore-matches-reserve equivalence (the replay/adopt contract).
- `cgroup_test.go`: CPU/memory limit parsing, cgroup path construction, `parseMeminfo` missing-field handling, meminfo-fallback vs cgroup-preferred precedence (`TestReadCgroupMetrics_MeminfoFallback`, `TestReadCgroupMetrics_CgroupPreferredOverMeminfo`), `TestParseUsageUsec_Valid/Missing`, `TestReadCgroupMetrics_NoError`, and the per-agent reader battery (`TestReadAgentCgroupMetrics_AgentPathWins`, `_FallbackToHost` with `HostLevelFallback` asserted, `_ZeroWhenBothUnreadable`, `_MaxLimitHybrid`, plus `ReadAgentCgroupLimits` parse/graceful cases). All tests override the `cgroupBaseDir`/`meminfoFile`/path-fn variables instead of touching `/sys`.
- `tracker_test.go`: register/unregister, capacity enforcement, duplicate detection, list ordering.

## Pitfalls

1. **`PortAllocator.Free` does not validate ownership.** Any caller can free any agent's port range. The caller (agent manager) must ensure correct pairing.
2. **Tracker is in-memory only.** Agent state is lost on process restart unless the GAP-070 durable registry is enabled (default ON since ca01d28 — `internal/registry` replays the JSONL log at startup and `Restore` re-claims each agent's exact port sub-range; a disabled registry restores the old in-memory behaviour).
3. **Cgroup paths are Linux-specific.** Tests that construct cgroup paths will fail on macOS/Windows. Use build tags or `runtime.GOOS` guards.
4. **`ToAgentSummary` formats timestamps with RFC3339.** If the proto definition changes format, this method must be updated to match.
5. **`CPUSampler` is delta-based.** `Percent()` returns 0 on the first sample (baseline); a single sample is meaningless — always sample twice for a real reading (DOGFOOD-006).
6. **cgroup v2 memory files are not guaranteed to exist.** `/sys/fs/cgroup/memory.current` + `memory.max` are absent on some systemd 255 hosts — callers must tolerate the `/proc/meminfo` fallback instead of treating it as an error.
7. **`CgroupCPUSharesPath`/`CgroupMemoryPath` return legacy paths that no cgroup writer creates** (`/sys/fs/cgroup/bunker-<id>.slice`); the real per-agent files live under `user.slice/user-<uid>.slice` (the user-slice reader, `ReadAgentCgroupMetrics`) and the unit path (`agentCgroupBase`, used for read-back verification only). Don't "read" the legacy paths and expect data.
8. **A per-agent read that silently degraded to HOST values is not an agent reading.** `ReadAgentCgroupMetrics` never errors; the only signal that the numbers are host-level (or zero) is `HostLevelFallback`. Every caller must surface it — the server maps it to the `host_level_fallback` wire field and the CLI prints a NOTE (GAP-060, 61dafd2); dropping the flag is how DOGFOOD-011's "1 GB agent reports 2.4 GB" bug looked fixed when it wasn't.
