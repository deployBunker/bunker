# Safety presets — operator guide

Status: **shipped plumbing (GAP-116) + shipped tier naming (GAP-117)**; the
GAP-118 DoS-containment table is live on the slice drop-in. Tier differentiation
(GAP-118 `open`/`hardened` cells, GAP-119 unit sandboxing, GAP-120 atomic
teardown, GAP-121 observability, GAP-122 battery) remains open.
Last Updated: 2026-09-29
Source of truth: `internal/agent/isolation.go`, `internal/config/config.go`.

> **Authoritative measured semantics live in
> [`knob-safety-matrix.md`](knob-safety-matrix.md)** (GAP-114, measured on this
> host 2026-09-22). Every tier cell in this guide cites that matrix; where this
> guide and the matrix disagree, the matrix wins and this guide is the bug.
> Design authority: [../../specs/safety-presets.md](../../specs/safety-presets.md).

---

## 1. What a preset is

A safety preset is a **named bundle of containment settings** resolved once per
spawn and applied at the spawn's enforcement points:

1. the **user slice drop-in** (`user-<uid>.slice`, applying to *every* process the
   agent user owns — direct `bunker exec` commands as well as containers), and
2. the **rootless-dockerd transient unit** (`systemd-run` argv).

One point alone is insufficient: the dockerd unit constrains only dockerd, so a
`bunker exec … -- stress` would otherwise run entirely unconstrained. Both points
resolve from the **same** tier lookup, so a tier can never narrow one surface and
leave the other wide (`internal/agent/manager_spawn.go`, `KnobsForPreset`).

The preset selects the **bundle** — it does not duplicate the arithmetic. The
five baseline numbers come from the `agent.default_*` config keys, so an operator
who tunes a default tunes every tier that ships that knob
(`internal/agent/isolation.go:102`).

## 2. The tiers

| Tier | Status today | Resolves to |
|---|---|---|
| `standard` | **shipped / built-in default** (GAP-117) | the five-knob baseline + `MemorySwapMax=0` + `MemoryHigh` = 90% of `MemoryMax` |
| `open` | valid name, accepted | **exactly the same knob set as `standard` today** (GAP-118 will differentiate) |
| `hardened` | valid name, accepted | **exactly the same knob set as `standard` today** (GAP-118/119 will differentiate) |

Nothing outside the vocabulary is ever accepted: an unknown name fails **loudly**
at config load and at spawn — never a silent fallback to a weaker (or stronger)
set. A deployment that already says `safety.preset: open` keeps working
unchanged; the name is honoured even though its cells are not yet distinct.

## 3. Precedence — the only resolution order

```
--preset <tier>                      (bunker spawn --preset, or
 │                                    SpawnAgentRequest.safety_preset /
 │                                    RunAgentRequest.safety_preset over RPC)
 ▼  if set and valid → wins
BUNKERD_SAFETY_PRESET                (environment)
 ▼  if set and valid → wins
safety.preset                        (/etc/bunkerd/config.yaml)
 ▼  if set and valid → wins
standard                             (built-in default — SafetyPresetDefault)
```

In one line: `--preset` (per spawn) > `BUNKERD_SAFETY_PRESET` (env) >
`safety.preset` (config) > `standard` (built-in default).

- **First WINS, not last.** A valid value at a higher level short-circuits the
  chain: a lower source holding an *invalid* name is never even consulted, exactly
  like the `*_FILE` secret chain.
- **Empty means "defer"** at every level — an empty flag/env/config does not name a
  preset, it steps down to the next source.
- **An unknown name is a hard error, never a fallback**, and the error names the
  source that held it: `--preset: unknown safety preset "ultra" (valid: [standard
  open hardened])`, `BUNKERD_SAFETY_PRESET: …`, `safety.preset: …`.
  - In the config file it **refuses to start** (validation runs before any
    listener binds).
  - Over RPC the spawn/run is refused with `CodeInvalidArgument` — never
    `CodeInternal`.
  - The CLI validates `--preset` locally before the RPC, so a typo fails fast.
- Every spawn-shaped path (spawn, detached run) resolves through the **single**
  resolver `config.ResolveSafetyPreset`, so the sources can never disagree. The
  resolver's output is what the tier table consumes; an untabled name after
  resolution is a programming error and panics rather than degrading quietly.

## 4. What each tier enforces today

### 4.1 The five-knob baseline — every tier, both enforcement points

| systemd property | Value today | Comes from |
|---|---|---|
| `CPUQuota` | `200%` | `agent.default_cpu_quota: 2.0` |
| `MemoryMax` | `4294967296` (4 GiB) | `agent.default_memory_bytes` |
| `TasksMax` | `4096` | `agent.default_max_processes` |
| `LimitNOFILE` | `65536:65536` | `agent.default_max_open_files` |
| `LimitFSIZE` | `21474836480` (20 GiB) | `agent.default_disk_bytes` |

`LimitFSIZE` is a **per-file** cap (`RLIMIT_FSIZE`), **not a disk quota** — no
mechanism counts an agent's aggregate usage, and total-disk enforcement is
GAP-161, not implemented (DF-BUNKER-54). A finite value also crash-loops .NET
apps that `ftruncate` a large sparse file at first boot, which is why
`agent.default_disk_bytes: 0` is the good configuration for mixed workloads.

### 4.2 The GAP-118 containment knobs — the slice drop-in, landing-checked

These ride the **slice drop-in** (they are not in the dockerd unit argv) and each
emitted property is read back from the live cgroup: a requested knob that did not
land fails the spawn at `StageSliceLimits` (the R2 no-silent-no-op rule). A knob a
tier does **not** request emits no property at all, so it can never trip a landing
check.

| Knob | `open` | `standard` (shipped) | `hardened` | Matrix evidence |
|---|---|---|---|---|
| `MemorySwapMax` | not emitted (host default) | `0` — **swap barred** | `0` | With swap barred an at-limit workload dies loudly at the cap; with swap allowed the same under-sizing is masked by paging. Bar-swap must NEVER reach `open`: it converts a would-have-completed run into a hard OOM. |
| `MemoryHigh` | not emitted (off) | 90% of the agent's `MemoryMax` | 90% | Measured **graceful**: throttle + swap spill, zero kills; the peak pins exactly at the cap. (`MemoryHigh=` only — docker `--memory-reservation` measured a cgroup-v2 no-op.) |
| `MemoryOOMGroup` | off | off | off | **UNMEASURED-here**: the systemd 259 user manager refuses the property and delegated cgroupfs writes `EACCES`. Blocked from default-on; `agent.default_memory_oom_group: true` is refused by validation. |
| `IOWeight` | off | off | off | Measured **inert** on uncontended NVMe (900 vs 100 weights → identical throughput). Config opt-in only. |
| `IOWriteBandwidthMax` | off | opt-in via config | opt-in via config | Measured effective and exact. `standard` stays opt-in so a 1.4 GB `docker load` keeps today's throughput. An emitted bound must resolve to the **whole disk device** — `io.max` rejects partitions (measured `ENODEV`); if the device cannot be resolved the spawn fails loudly rather than dropping the property. |

The tier-name mapping note from the matrix: it names four tiers
(`open`/`standard`/`guarded`/`hostile`); this code's vocabulary is
`open`/`standard`/`hardened`, and `hardened` is the guarded-and-above reading —
the containment cells are identical for the upper tiers, so no measured value
changes.

### 4.3 Not yet differentiated

`open` and `hardened` are **accepted names that resolve to `standard`'s knob set**
until GAP-118/119 differentiate them. Read a preset as "the name the operator
asked for plus the set actually enforced" — `bunker info` shows both (§6).

## 5. Admin overrides (and the `-1` convention)

The tier table is the design authority; the daemon config is the
**admin-override seam**, applied in the documented order *tier table → daemon
config → per-spawn request*. Configure a knob under **either** the flat
`agent.default_*` field **or** the nested `agent.containment.*` block — a value in
both is refused as ambiguous (`containment.* wins` for single-shape config, but
both-set is an error).

| Value | Meaning |
|---|---|
| `0` | **The tier table decides** (nothing overridden). |
| `-1` | **Release the tier knob back to the host default** — the swap convention. For `memory_swap_max_bytes` this opts OUT of the tier's bar-swap; it is the measured-UNSAFE direction and is never "infinity". `-1` is the only accepted negative anywhere, and for `memory_swap_max_bytes` it is the *only* accepted non-zero value. |
| positive | A byte override. The IO write bound has a **floor of 20 MiB/s** (the lowest measured-enforced bound, `MinContainmentIOWriteBps = 20971520`) and a 2 GiB/s sanity ceiling; `io_weight` must be in the kernel's `1..10000` range. |

The full envelope and the refusal messages are in
[../../specs/configuration.md](../../specs/configuration.md) §7.

## 6. Reading back the resolved preset

The resolved preset and the **effective knob set** ride the agent record, so a
client can always see what was actually enforced:

| Wire field | Where | Meaning |
|---|---|---|
| `safety_preset` (`AgentSummary.safety_preset = 13`) | `GetAgent`, `ListAgents`, `ServerMetrics.agents[]` | the resolved preset name the agent was spawned under |
| `systemd_properties` (`AgentSummary.systemd_properties = 14`, repeated `SystemdProperty{name,value}`) | same | the knob set as written into the unit argv / slice drop-in, e.g. `CPUQuota=200%`, `MemoryMax=4294967296`, `MemorySwapMax=0` |

Both are **additive** fields: an older client ignores them, and an agent record
that predates GAP-116 reports an **empty** preset — displayed as the built-in
default name with **no** knob block (honest absence, never a fabricated set).

The spawn response itself carries **no** preset field — read the resolved preset
back immediately after spawning:

```bash
bunker spawn build-1 --ttl 2h --preset hardened
bunker info build-1
#   Safety Preset:    hardened
#   Safety Knobs:
#     CPUQuota:       200%
#     MemoryMax:      4294967296
#     TasksMax:       4096
#     LimitNOFILE:    65536:65536
#     LimitFSIZE:     21474836480
#     MemorySwapMax:  0
#     MemoryHigh:     3865470480
```

(That knob list is illustrative — it is the default `agent.default_*` baseline
with the `standard`/`hardened` containment additions; `MemoryHigh` renders
exactly 90% of the agent's resolved `MemoryMax`.)

`RunAgentRequest.safety_preset` carries the same field with the same grammar and
precedence for the detached-run path.

## 7. Operator recipes

```bash
# 1. Per spawn (highest precedence) — the flag beats everything below it.
bunker spawn build-1 --ttl 2h --preset hardened

# 2. Daemon-wide via the environment (beats the config file).
#    In a systemd unit: Environment=BUNKERD_SAFETY_PRESET=hardened
BUNKERD_SAFETY_PRESET=hardened bunkerd --config /etc/bunkerd/config.yaml

# 3. Daemon-wide via the config file (lowest explicit source).
#    /etc/bunkerd/config.yaml
#      safety:
#        preset: hardened

# 4. Over REST (proto snake_case in, protojson camelCase out).
curl -s http://127.0.0.1:8080/bunker.v1.Bunkerd/SpawnAgent \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <master-token>' \
  -d '{"agent_id":"build-1","ttl":"2h","safety_preset":"hardened"}'
```

An empty value at any level means "not set — defer to the next source".

## 8. Failure modes you will see

| You wrote | What happens |
|---|---|
| `safety.preset: ultra` in the config file | daemon **refuses to start**: `safety.preset must be one of [standard open hardened], got "ultra"` |
| `BUNKERD_SAFETY_PRESET=ultra` (no flag/config preset) | spawn refused: `BUNKERD_SAFETY_PRESET: unknown safety preset "ultra" (valid: […])` |
| `--preset ultra` / `"safety_preset":"ultra"` over REST | refused with `CodeInvalidArgument` (HTTP 400); the CLI catches it locally before the RPC |
| `agent.containment.io_write_bps: 1048576` (1 MiB/s, below the floor) | config load refused: the value is outside the measured envelope |
| `agent.default_io_weight` **and** `agent.containment.io_weight` both set | config load refused: `agent.default_io_weight and agent.containment.* disagree; set only one` |
| `agent.default_memory_oom_group: true` | config load refused: the knob is UNMEASURED on this host and blocked from default-on |
| an IO write bound on a host whose whole-disk device cannot be resolved | **spawn fails** at `StageSliceLimits` naming the knob (never a silently omitted property) |

## 9. Where the code lives

| Surface | File |
|---|---|
| Vocabulary, defaults, precedence resolver, override envelope | `internal/config/config.go` |
| Tier → knob set, GAP-118 tier table, override merge, landing check | `internal/agent/isolation.go` |
| Spawn wiring (both enforcement points, recorded preset) | `internal/agent/manager_spawn.go` |
| Detached-run wiring | `internal/agent/run.go` |
| `--preset` flag | `internal/cli/spawn.go` |
| Read-back rendering (`bunker info`) | `internal/cli/info.go` |
| Measured evidence | [`knob-safety-matrix.md`](knob-safety-matrix.md) |
