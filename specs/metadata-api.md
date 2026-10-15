# Bunker — Metadata API Specification (operator-facing fleet surface)

Version: 0.1.0 (Draft)
Status: Draft — design authority for GAP-169 (implementation); nothing in §4 is implemented yet
Last Updated: 2026-10-15
Related: specs/api.md (the wire contract this extends), specs/architecture.md, specs/agent-lifecycle.md, specs/network-isolation.md (§5.2 reporting law), specs/safety-presets.md

## 0. Overview & Repo Reality

The bunker metadata surface is how an operator answers "what does the fleet
IS" questions without SSH-ing to the host: what agents exist, where they
live (URLs, ports, mounts), what state they are in, what resources they
consume and are bounded by, and what residue they left behind. Today those
answers are spread across `ServerInfo`, `ServerMetrics`, `ListAgents`,
`GetAgent` and `AgentMetrics`, and several declared fields are never
populated while several pieces of tracked state have no wire projection at
all.

This spec:

1. inventories the CURRENT surface from source (§2), verified live against
   the demo daemon (§2.1);
2. names the operator questions the current surface cannot answer and the
   daemon state that would back each answer (§3);
3. proposes the expansion as additive fields on the EXISTING messages — no
   new RPC, no semantic change to any existing field (§4);
4. fixes the compatibility law every GAP-169 change must follow (§5).

Everything here is documentation. GAP-169 implements against §4's tables.
This spec deliberately does NOT depend on any open GAP-125 decision
(threat-model scope, egress default, compliance level, shared-scratch
default, doc ownership); where a proposed row would be shaped by one, it is
marked `DECISION-PENDING(GAP-125)` and excluded from GAP-169's scope until
that row closes.

Additive framing throughout: extend, never replace. The three-state
reporting law of specs/network-isolation.md §5.2 ("a bound that is not
reported is not a bound"; empty = predates, unknown = unverifiable,
explicit value = the only affirmative statement) is the template for every
new field.

## 1. Evidence & provenance

All file:line pointers in this spec are relative to worktree HEAD
`302a73be` (branch `wt/GAP-167`). The live verification was performed on
**2026-10-15** against the demo daemon **bunker-mvp**
(`http://100.115.67.116:19090`, REST/connect; daemon reports version
`0.2.0`, uptime ~5h35m, 4/50 agents) with:

- `bunker status`
- `bunker list --server bunker-mvp --status all --page-size 100`
- `bunker info ce5140db` and `bunker info 99449dee`
- `bunker metrics --server bunker-mvp` (server-wide)
- `bunker metrics 99449dee --server bunker-mvp` (per-agent)
- raw connect POSTs (`ServerInfo`, `ListAgents {statusFilter:all}`,
  `AgentMetrics {agentId:99449dee}`) to read the wire shape directly

All calls were read-only. Nothing was spawned, destroyed, or written. No
tokens or secrets were recorded.

Note on client skew: the installed CLI (`bunker version` → commit
`9b924f0`, built 2026-10-04) predates the CLI-side egress render
(internal/cli/egress_render.go, landed 2026-10-11) and the
`bunker resource-events` command (internal/cli/resource_events.go, landed
2026-10-13). CLI output alone therefore under-reports the current daemon
surface; the wire probes above are the authority for what the DAEMON
populates today.

## 2. Current surface inventory (verified from source + live)

### 2.1 `ServerInfo` — `ServerInfoResponse` (proto/bunker/v1/bunker.proto:125-167)

Populated by internal/server/service.go:134-182.

| Field (proto #:name) | Type | Source of truth (code) | Live 2026-10-15 |
|---|---|---|---|
| 1 hostname | string | `os.Hostname()` (service.go:135) | `bunker-mvp` |
| 2 version | string | `version.Version` (service.go:138; internal/version/version.go:25 default `0.2.0`, stamped via -ldflags) | `0.2.0` |
| 3 uptime_seconds | uint64 | `time.Since(serverStartTime)` (service.go:139; `serverStartTime` at service.go:42, process start) | set (~5h35m rendered by `bunker status`) |
| 4 agent_count | uint32 | `tracker.Count()` (service.go:140; internal/resource/tracker.go:136) | 4 |
| 5 max_agents | uint32 | `tracker.MaxAgents()` (service.go:141; tracker.go:143) | 50 |
| 6 total_resources | ResourceLimits | **DECLARED, NEVER POPULATED** — the only non-test references are generated code (proto/bunker/v1/bunker.pb.go:318,423); no handler writes it | absent on the wire |
| 7 available_resources | ResourceLimits | **DECLARED, NEVER POPULATED** — same (bunker.pb.go:319,430) | absent on the wire |
| 8 tmp_isolation | string | cached pam_namespace probe (service.go:147 → 303-325; states at service.go:217-221) | `private` |
| 9 tmp_isolation_detail | string | same probe (service.go:147) | set (per-session pam_namespace instance) |
| 10 residue | ResidueInventory | host probe via `agentMgr.ResidueInventory()` (service.go:153 → 188-202; internal/agent/inventory.go:103) | `{orphan_users:0, orphan_homes:0, orphan_keys:19, stale_linger:0, registered_agents:6, status:"ok"}` |
| 11 default_network_mode | string | `cfg.ResolveNetworkMode("")` + degrade-to-unknown (service.go:161-170) | `shared` |
| 12 default_network_boundary | string | `netmode.BoundaryFor(mode)` (service.go:165) | set (host network namespace line) |
| 13 egress_modes_supported | repeated string | `config.ValidEgressModes()` — re-export of the internal/egress vocabulary, never restated (service.go:180) | 3 values (`open`, `allowlist`, `none`) |

`ResidueInventory` sub-fields (proto:172-195): orphan_users, orphan_homes,
orphan_keys, stale_linger_entries, registered_agents, status
(`ok`/`partial`/`unavailable`), detail — all mapped at service.go:193-201,
counts clamped non-negative by `maxInt` (service.go:207-212).

Observable live divergence: `residue.registered_agents` = 6 while
`agent_count` = 4 — the durable registry and the in-memory tracker DISAGREE
on the demo host today, and no field pairs them so an operator can see the
gap (see gap G-10).

### 2.2 `AgentSummary` (proto:211-252) — the List/Get agent shape

Projected by `AgentRecord.ToAgentSummary()`
(internal/resource/tracker.go:155-190); served by `ListAgents`
(service.go:604-656) and `GetAgent` (service.go:659-682).

| Field (proto #:name) | Type | Source of truth (code) | Live 2026-10-15 |
|---|---|---|---|
| 1 agent_id | string | tracker record (tracker.go:157) | set |
| 2 status | string | tracker record; `GetAgent` may adjust it via the docker-health adjuster (service.go:676-678, DF-BUNKER-72) | `running (docker degraded)` observed via `bunker info` |
| 3 limits | ResourceLimits | tracker record (tracker.go:159) | CPU 2.0 / mem 4 GB / file-cap 20 GB / containers 10 |
| 4 created_at | string | record, RFC3339 (tracker.go:160) | set |
| 5 expires_at | string | record, RFC3339 (tracker.go:161) | set (created + 4h observed) |
| 6 sshfs_mount | string | record (tracker.go:166) | set (full sshfs command) |
| 7 docker_host_tunnel | string | record (tracker.go:167) | set (ssh -L command) |
| 8 public_url | string | record (tracker.go:162) | empty (no tunnel configured on these agents) |
| 9/10 port_range_start/end | uint32 | record (tracker.go:163-164) | 10100-10199 / 10000-10099 |
| 11 tailnet_ip | string | record (tracker.go:165) | empty (no tailscale) |
| 12 disk_used_bytes | uint64 | TTL snapshot cache, ListAgents only (service.go:623, PERF-001) | 4.7 GB / 9.7 GB / … |
| 13 safety_preset | string | record (tracker.go:179; GAP-116) | `standard` |
| 14 systemd_properties | repeated SystemdProperty | record **UnitProperties only** (tracker.go:180) | 5 knobs (CPUQuota/MemoryMax/LimitFSIZE/TasksMax/LimitNOFILE) |
| 15 mount_spec | MountSpec | derived (tracker.go:173, `newMountSpecForSummary`) | driver + command set |
| 16 orphan_uid_detail | string | per-request /proc batch verdict (service.go:638-651, DF-BUNKER-34) | empty (healthy) |
| 17 network_isolation | NetworkIsolation | record (tracker.go:184; proto:510-518 three-state vocabulary) | mode `shared` + boundary set |
| 18 egress_mode | string | record (tracker.go:188; GAP-134) | set |

Record state with NO summary projection (tracked, invisible on the wire):

- `Image` — the per-agent customized image ref (tracker.go:27-32, GAP-064/069)
- `SliceProperties` — the slice drop-in's knob set (tracker.go:46)
- `SliceDropIn` / `SliceDropInState` — exact drop-in content + `written`/`failed` (tracker.go:47-48)
- `MountDriver` is projected only inside `mount_spec.driver` (tracker.go:173)

Spawn-request state with NO persistence at all:
`SpawnAgentRequest.labels` (proto:268) is accepted and dropped — the
`AgentRecord` has no labels field and nothing stores it.

### 2.3 `ServerMetrics` — `ServerMetricsResponse` (proto:199-207)

Populated by internal/server/service.go:328-362.

| Field | Type | Source of truth (code) | Live 2026-10-15 |
|---|---|---|---|
| cpu_usage_percent | double | delta-sampling `cpuSampler.Percent()` (service.go:341-343; first call on a fresh sampler is a 0 baseline) | 408.5–451% |
| memory_used/total_bytes | uint64 | cgroup v2 read, /proc/meminfo fallback (service.go:347-350) | 5.7–5.8 / 15.2 GB |
| disk_used/total_bytes | uint64 | `readDiskStats()` (service.go:353-356) | 56.5–57.1 / 149.9 GB (38%) |
| docker_containers_total | uint32 | `countDockerSockets()` — **a docker.sock-file count under /run/bunker, a PROXY for running daemons, not a container count** (service.go:359 → 2140-2155) | 48 |
| agents | repeated AgentSummary | every tracker record via `ToAgentSummary()` (service.go:330-333) — note: this path does NOT fill disk_used_bytes from the TTL cache (that happens only in ListAgents, service.go:623) | 4 summaries; disk_used_bytes empty here |

### 2.4 `AgentMetrics` — `AgentMetricsResponse` (proto:532-552)

Populated by internal/server/service.go:723-801.

| Field | Type | Source of truth (code) | Live 2026-10-15 (agent 99449dee) |
|---|---|---|---|
| agent_id, status | string | tracker record (service.go:729-732) | set |
| cpu_usage_percent | double | agent cgroup read when the user resolves (service.go:770-774) | not populated on this probe (degraded read/baseline) |
| memory_used_bytes | uint64 | agent cgroup (service.go:772) | 956.8 MB |
| memory_limit_bytes | uint64 | `rec.Limits.MemoryMaxBytes` echo (service.go:734) | 4.0 GB |
| disk_used_bytes | uint64 | TTL cache (service.go:788) | 4.9 GB |
| disk_limit_bytes | uint64 | `rec.Limits.DiskMaxBytes` echo — a PER-FILE cap, never a ratio denominator (service.go:745; DF-BUNKER-54) | 20.0 GB |
| docker_containers | uint32 | **DECLARED, NEVER POPULATED** by the daemon (no non-test writer) | absent |
| uptime | string | **DECLARED, NEVER POPULATED** by the daemon (no non-test writer) | absent |
| host_level_fallback | bool | set when the agent user/cgroup is unreadable and HOST values are returned instead (service.go:773, 782) | false (absent) |
| resource_events | ResourceEvents | GAP-121 cgroup events + peaks, `fillAgentResourceEvents` (service.go:798; proto:569-638; peaks carry their source: `cgroup`/`daemon`/empty) | populated, available=true, cgroup_path set |

### 2.5 Agent service (scoped sub-key surface)

`GetInfo` (service.go:1955-1986): returns agent_id, status, public_url,
expires_at, limits from the tracker record. `docker_host` (proto:714) is
**DECLARED, NEVER POPULATED** — no handler writes it. `Metrics` and
`Heartbeat` reuse the Bunkerd handlers (service.go:1989, 2074).

### 2.6 What the operator can actually see today (CLI rendering)

`bunker status` renders: hostname, version, uptime, agents N/max, /tmp
isolation, default net mode + boundary, residue counts line (with a
"residue present" warning when counts are non-zero), CPU, memory, disk,
docker (socket-proxy count). `bunker list` renders per agent: id, status,
disk used, per-file cap, created, public URL, net-isolation line. `bunker
info` adds: port range, tunnel + mount commands, limits, safety preset +
knobs, network isolation, egress policy (egress render only on CLIs ≥
2026-10-11). `bunker metrics` renders server-wide or per-agent usage;
`bunker resource-events <id>` (CLI ≥ 2026-10-13) renders the GAP-121 view.

## 3. Gap analysis — questions the current surface cannot answer

Each gap names the operator question, why the current surface fails it, and
the daemon state that would back the answer. Gap IDs are referenced by §4.

- **G-1 Build identity.** "Is this daemon the build I think it is?" The
  version string is a bare semver; two 0.2.0 daemons can be different
  builds (the CLI already stamps commit + build date — internal/version
  ldflags mechanism exists). Source: the same stamped vars in the daemon
  binary.
- **G-2 Capacity arithmetic.** "How much headroom is left?" proto fields 6-7
  (`total_resources`/`available_resources`) are declared but never populated
  (§2.1); spawn admission (capacity check, tracker.go:86-98, 148-152) is
  invisible. Source: tracker counters + the config defaults spawn resolves.
- **G-3 State census.** "How many agents are running vs stopped vs failed?"
  requires listing every agent and counting client-side, and
  `ServerMetrics.agents` ships N full summaries per poll to do it. Source:
  `tracker.List()` classification.
- **G-4 Expiry/heartbeat hygiene.** "What expires soon? Is this agent
  actually alive or merely not yet expired?" Only per-agent `expires_at`
  exists; heartbeat recency is NOT recorded anywhere (HeartbeatAgent,
  service.go:1281, only extends the expiry), and stopped agents expire
  silently to the reaper. Source: tracker records + a new last-heartbeat
  timestamp on the record.
- **G-5 Residue detail.** "WHICH 19 orphan keys?" Counts only cross the wire
  (§2.1); the probe planes already walk named entities (internal/agent/
  inventory.go) and the host-local `bunker homes`/`bunker linger` CLIs
  render names — but only on the daemon host.
- **G-6 Labels.** "Which agents belong to project X?" Spawn labels are
  dropped (§2.2). Source: new record field populated from
  SpawnAgentRequest.labels.
- **G-7 Image identity.** "Is this agent containerized, and on what image?"
  `record.Image` exists (tracker.go:27-32) but has no summary projection —
  GAP-064 agents are indistinguishable in list/info.
- **G-8 Slice enforcement visibility.** "What does the slice drop-in
  enforce?" safety-presets.md fixes TWO enforcement points; only the unit
  knobs cross the wire (`UnitProperties`). `SliceProperties` and
  `SliceDropInState` (`written`/`failed`) are tracked but unprojected.
- **G-9 Docker-degraded detail.** Status says `running (docker degraded)`
  (observed live) but nothing says WHY or since when — the adjuster's
  failure reason (service.go:676-678) is discarded.
- **G-10 Tracker vs registry divergence.** Live: `agent_count` 4 vs
  `residue.registered_agents` 6. The durable registry count exists
  (agent-lifecycle.md) but is only visible disguised as a residue
  denominator. A paired field makes adopt/replay drift legible.
- **G-11 Egress allowlist visibility.** An agent reports `egress_mode:
  allowlist`, but the configured accept-list (agent.egress.allowlist,
  GAP-134) is not surfaced — the operator cannot see what "allowlist"
  permits without host access.
- **G-12 Declared-but-dead fields.** `total_resources`,
  `available_resources`, `AgentMetrics.uptime`,
  `AgentMetrics.docker_containers`, `GetInfo.docker_host` are declared and
  never populated (§2). Clients cannot distinguish "the daemon did not
  tell" from "there is nothing to tell" — the exact ambiguity the §5.2 law
  exists to prevent.

## 4. Proposed expansion (additive fields on existing messages)

No new RPC. No change to any existing field number or semantics. Every new
field follows §5. Field numbers continue from each message's current
maximum (ServerInfoResponse next = 14; AgentSummary next = 19;
AgentMetricsResponse next = 12; GetInfoResponse next = 7).

Column "live-provable" = the backing state can be demonstrated to exist
today (with the current daemon or on the daemon host) without new
instrumentation.

### 4.1 `ServerInfoResponse` additions

| # | Name | Type | Source of truth | Live-provable | GAP-125 dep | Notes |
|---|------|------|-----------------|---------------|-------------|-------|
| 14 | build_commit | string | ldflags-stamped version var (internal/version; same mechanism `bunker version` already prints) | y | n | empty = build predates stamping (three-state law) |
| 15 | build_date | string (RFC3339) | same stamp | y | n | empty = predates |
| 16 | registry_agent_count | uint32 | durable registry reader (internal/agent registry JSONL, agent-lifecycle.md) | y | n | pairs with agent_count (4); divergence = adopt/replay drift signal (G-10) |
| 17 | agents_by_status | repeated StatusCount {status, count} | classification over tracker.List() (G-3) | y | n | repeated message, not map, so the status vocabulary can evolve |
| 18 | default_egress_mode | string | `cfg.ResolveEgressMode("")` re-export — same shape as fields 11-12; the VALUE it reports is set by config/GAP-125, not by this field | y (config exists, GAP-134) | **DECISION-PENDING(GAP-125)** for the default VALUE it will report (egress default). The field itself (honest reporting of whatever is configured) is not blocked | empty = predates; never render as "open" |
| 19 | default_egress_boundary | string | `egress.BoundaryFor(mode)` — owning package re-export, never restated | y | same as #18 | |
| 20 | residue_detail | repeated ResidueItem {kind, name, detail} | the same inventory probe planes that produce the counts (internal/agent/inventory.go) — names, not new probing | y | n | additive to residue counts (field 10 stays); kind ∈ {orphan_user, orphan_home, orphan_key, stale_linger} |
| 21 | scratch_surface | ScratchInfo {present, mode, per_agent_cap_bytes} | hostsetup scratch probe (/srv/bunker-share, GAP-075) | y (host-provision status shows the state host-side) | **DECISION-PENDING(GAP-125)** (shared-scratch default shapes what "expected" means; reporting the OBSERVED state does not) | omit from GAP-169 unless the GAP-125 row lands first; degrade-to-unknown when unreadable |
| — | posture/compliance summary | — | — | — | **DECISION-PENDING(GAP-125)** (threat-model scope + compliance level) | deliberately NOT designed here; see §5 non-goals |

### 4.2 `AgentSummary` additions

| # | Name | Type | Source of truth | Live-provable | GAP-125 dep | Notes |
|---|------|------|-----------------|---------------|-------------|-------|
| 19 | image | string | `record.Image` (tracker.go:27-32) — pure projection | y | n | empty = agent spawned without an image spec (existing record semantics, distinct from a GAP-064 agent) |
| 20 | slice_properties | repeated SystemdProperty | `record.SliceProperties` (tracker.go:46) — pure projection | y | n | completes the two-enforcement-points view (safety-presets.md; G-8) |
| 21 | slice_drop_in_state | string | `record.SliceDropInState` (tracker.go:48): `written` / `failed` / empty | y | n | empty = pre-GAP-116 record or step not run; never render as success or failure |
| 22 | last_heartbeat_at | string (RFC3339) | NEW record field written by HeartbeatAgent (service.go:1281) and RestartAgent expiry reset | **n** — heartbeats leave no trace today (only expires_at moves) | n | empty = never heartbeated / predates; makes "alive" vs "not yet expired" distinguishable (G-4) |
| 23 | labels | map<string,string> | NEW record field populated from SpawnAgentRequest.labels (proto:268, currently dropped) | **n** — dropped today | n | replayed/adopted records report empty; client-side filtering can build on it later (G-6) |
| 24 | docker_health_detail | string | the docker-status adjuster's failure reason (service.go:676-678) | **n** — the adjuster returns only the adjusted status today | n | empty + plain `running` = healthy; empty + `docker degraded` = predates the detail surface (§5.2 law; G-9) |

### 4.3 Declared-but-unpopulated fields: populate or deprecate

GAP-169 must resolve each of these one way or the other; leaving them
silently dead is the status quo defect (G-12). Additive rule: populate with
a named source, or mark `[deprecated = true]` — never delete, never rename.

| Message.field | Decision proposed | Source of truth if populated |
|---|---|---|
| ServerInfoResponse.total_resources (6) | populate | configured default limits × max_agents (the values spawn admission actually grants against) |
| ServerInfoResponse.available_resources (7) | populate | the same defaults minus live tracker allocation |
| AgentMetricsResponse.uptime (9) | populate (or add uptime_seconds and deprecate the free-text form) | the agent's systemd session unit / cgroup start time |
| AgentMetricsResponse.docker_containers (8) | populate | the agent's own rootless dockerd (`docker ps -q` through /run/bunker/<id>/docker.sock), NOT the socket count |
| GetInfoResponse.docker_host (3) | deprecate (the CLI resolves connection strings from GetAgent today) | — |

### 4.4 Explicitly out of scope for GAP-169

- Any new RPC (fleet census, expiry feed): derivable from List + §4.1 #17.
- Cross-daemon aggregation ("fleet of hosts"): one daemon per surface.
- Time-series/history: peaks already live in resource_events (GAP-121);
  retention is a different problem.
- Label-based server-side filtering: needs labels (§4.2 #23) to land and
  age first; additive request field later, never a breaking change.
- Mutation of any kind: the metadata surface stays strictly read-only.
- The `DECISION-PENDING(GAP-125)` rows in §4.1 (#18/#19 default VALUE,
  #21, posture summary): implemented only after the corresponding GAP-125
  decision closes, never speculatively.

## 5. Versioning & compatibility law (binding on GAP-169)

1. **Additive only.** New field numbers on existing messages (§4 numbering);
   no renames, no re-used numbers, no semantic change to existing fields.
   Precedent: every GAP-116/134/MOUNT-006/NET-BUNKER-010 field.
2. **Empty/absent = "predates the surface", never an affirmative claim.**
   Every new string/vocabulary field inherits the §5.2 three-state law
   (specs/network-isolation.md; proto fields 8, 11, 13 are the models).
   A client MUST NOT render an empty egress/default/health field as any
   specific value — internal/cli/egress_render.go:21-25 is the render
   precedent.
3. **Semantic corrections live in comments/labels, not renames.**
   disk_limit_bytes (DF-BUNKER-54) keeps its number; so does anything
   GAP-169 touches.
4. **Counts are clamped non-negative** before the uint32 conversion
   (maxInt, service.go:207-212) — a probe may never wrap into ~4 billion.
5. **Probes never fail the RPC.** A failed residue/scratch/heartbeat read
   degrades to empty/unknown with a detail string
   (tmp_isolation precedent, service.go:303-325); ServerInfo stays a
   hot-path cheap call — new probes cache like tmpIsolationOnce where they
   stat the host.
6. **Vocabularies are re-exported from their owning package**, never
   restated in the server (egress_modes_supported precedent,
   service.go:171-180).
7. **Declared-but-dead is a defect.** Any field this spec lists in §4.3
   ends up either populated from a named source or `[deprecated = true]`
   after GAP-169 — "declared and silently empty" is not an acceptable end
   state.
8. **No secret material.** The expansion adds no key paths, tokens, or
   host-credential material; residue_detail names residue ENTITIES (user/
   home/key names), which the daemon host CLIs already print, and key
   PRIVACY remains: names only, never key content or key file paths.
9. **Acceptance evidence.** For every §4 row marked live-provable = y,
   GAP-169 shows before/after wire evidence; rows marked n (new state)
   land with a unit test pinning the new record field plus the
   empty-means-predates render.
