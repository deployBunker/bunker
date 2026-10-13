# Per-agent resource events (GAP-121)

`bunker metrics <agent-id>` answers **"how much is this agent using right
now?"**. It cannot answer the question an operator asks *after* a runaway
agent:

> Did it actually hit its memory limit? Did the kernel kill anything?

Those facts are recorded by the kernel itself, per cgroup, in `memory.events`
and `pids.events`. This page documents the surface that exposes them.

```bash
bunker resource-events <agent-id>            # flags: --server
```

`bunker metrics <agent-id>` also prints a one-line `Ceiling events:` summary,
so the hit is visible while reading usage.

## Where the numbers come from

Each agent is its own Linux user (`bunker-<id>`) whose processes all live under
one cgroup v2 slice:

```
/sys/fs/cgroup/user.slice/user-<uid>.slice
```

That slice is the per-agent view — the rootless-dockerd unit and every
`bunker exec` session scope are inside it — and it is the same layout the
existing metrics readers use. The daemon runs as root and reads these files
directly:

| File | Meaning |
|------|---------|
| `memory.events` | `max`, `high`, `low`, `oom`, `oom_kill`, `oom_group_kill` counters |
| `pids.events` | `max` counter |
| `memory.current` | live memory usage |
| `pids.current` | live process/thread count |
| `memory.peak` | kernel high-water mark (kernel ≥ 5.19) |
| `pids.peak` | kernel high-water mark (kernel ≥ 6.1) |

## Reading the counters

Every counter is **cumulative since the cgroup was created** — an event count,
not a rate — so a non-zero value is a durable fact about the agent's whole
life, not a statement about the present moment:

| Counter | Non-zero means |
|---------|----------------|
| `memory.events max` | usage reached `memory.max` and reclaim was attempted |
| `memory.events high` | usage exceeded `memory.high` (throttle threshold) |
| `memory.events low` | usage dropped below `memory.low` |
| `memory.events oom` | the cgroup entered an OOM state |
| `memory.events oom_kill` | the kernel killed a process inside the agent to satisfy a memory limit |
| `memory.events oom_group_kill` | the whole cgroup was killed at once (`memory.oom.group`) |
| `pids.events max` | a fork/clone was refused because `pids.max` was already reached |

Because they are counters, they cannot tell you *when* a hit happened; pair
them with the daemon audit trail and the agent's own logs when you need a
timeline.

## Peaks, and why the source matters

Peaks are reported **with their source**, because the two sources are not
interchangeable:

| Source | Where it comes from | What it means |
|--------|---------------------|---------------|
| `cgroup` | `memory.peak` / `pids.peak` | The kernel's own high-water mark: covers every moment of the agent's life and survives a daemon restart. Authoritative. |
| `daemon` | the daemon's periodic sampler (default every 30s) | A **lower bound**: usage between two samples is invisible, and the value is lost when the daemon restarts. |
| *(empty)* | — | No peak available from either source. The CLI prints `no peak available` rather than a `0`. |

Sampling feeds both the reply to `bunker resource-events` and every metrics
read, so a peak accumulates even on a kernel too old to expose `memory.peak` /
`pids.peak`. Peaks are kept only for **running** agents: a destroyed (or
stopped) agent's peak is dropped on the next sampler pass, so a stale
high-water mark can never be mistaken for a live one.

## Unavailable is not clean

The failure modes here are all silent-zero traps, so the surface states them
explicitly:

* **No cgroup to read** — a stopped/destroyed agent, or a deleted agent user.
  The command prints `RESOURCE EVENTS UNAVAILABLE` and repeats that the
  counters were *not measured*; it never prints a clean verdict.
* **No `resource_events` on the wire at all** — a daemon built before GAP-121.
  The command prints `NOT REPORTED by this daemon` and says the ceiling events
  are not visible from here.
* **A partially readable cgroup** — the response carries per-block
  `available` flags plus a `detail` string naming exactly which files could
  not be read. A missing `memory.peak` on an older kernel is *not* a failure
  (it only means "no kernel peak, use the daemon's"); an unreadable
  `memory.events` is. One readable counter set never vouches for the other:
  if only `pids.events` could be read, the memory block is marked unavailable
  and the verdict line names `memory.events` as unmeasured.

Read the `available` flag before treating any zero as evidence. Zero ceilings
on an **available** reading means "this agent never hit its limits"; zero on an
unavailable one means "nobody looked".

## Wire surface

`ResourceEvents` rides on `AgentMetricsResponse` (field 11) of both the
`Bunkerd` and `Agent` services, so an agent-scoped credential can read its own
events (the `Agent.Metrics` handler already enforces `claims.AgentID == target`)
as well as the master credential reading any agent.

```
ResourceEvents {
  MemoryEvents memory = 1;   // max, high, low, oom, oom_kill, oom_group_kill, available
  PidsEvents   pids   = 2;   // max, available
  uint64 memory_current_bytes = 3;
  uint64 pids_current         = 4;
  uint64 memory_peak_bytes    = 5;
  uint64 pids_peak            = 6;
  string memory_peak_source   = 7;   // "cgroup" | "daemon" | ""
  string pids_peak_source     = 8;
  string cgroup_path          = 9;
  bool   available            = 10;  // the cgroup was located and at least one event file was read
  string detail               = 11;
}
```

Each `MemoryEvents`/`PidsEvents` block carries its own `available` flag, and
that is the one to check per counter set. The top-level `available` only says
the agent's cgroup was found and read at all.

## Example session

```bash
$ bunker resource-events abc12345

══════════ Agent Resource Events: abc12345 ══════════

  Status:   running
  Cgroup:   /sys/fs/cgroup/user.slice/user-1001.slice
  MEMORY
    limit            1.0 GB
    current          512.0 MB
    peak             768.0 MB   [cgroup memory.peak]

    memory.events (cumulative since the cgroup was created)
      max                     3   ceiling reached; reclaim attempted
      high                    4   usage exceeded memory.high (throttle threshold)
      low                     0   usage dropped below memory.low
      oom                     1   cgroup entered an OOM state
      oom_kill                1   process killed to satisfy a memory limit
      oom_group_kill          0   whole cgroup killed (memory.oom.group)

  PIDS
    current                23
    peak                   99   [daemon-observed, a LOWER BOUND: usage between samples is not seen]

    pids.events (cumulative since the cgroup was created)
      max                     7   fork refused: pids.max already reached

  Ceiling events recorded: memory.max reached ×3, OOM ×1, OOM kill ×1, pids.max reached ×7.
  ⚠  The kernel killed process(es) inside this agent to satisfy a memory limit.
```

## See also

* README → "Resource limits" (how the limits themselves are applied)
* [docs/threat-model.md](threat-model.md) — why agents are separated at all
* [specs/agent-lifecycle.md](../specs/agent-lifecycle.md) — the agent lifecycle
  the slice belongs to
