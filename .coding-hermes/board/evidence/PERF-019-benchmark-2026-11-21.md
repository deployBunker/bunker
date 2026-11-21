# PERF-019 — ListAgents cached-path component breakdown

- Row: PERF-019 (P1) — "ListAgents cached path still costs 11-24 ms server-side"
  (board: `.coding-hermes/board/tasks.jsonl`)
- Tree under test: worktree `/home/kara/worktrees/bunker-PERF-019`, branch `wt/PERF-019`,
  base `8f5d0925` (== `origin/main` at session start)
- Captured: 2026-11-21 (UTC), foreman host `kara`
- Raw output: `.coding-hermes/board/evidence/PERF-019-bench-raw-2026-11-21.txt`

## Verdict

**5 ms is NOT reachable at this registry size (2931 records) with the current
per-request cost structure. Declared floor below.**

The handler decomposes into a registry-independent half and a registry-scaled
half, and the registry-independent half is ~74% of the call:

| component | median | share of full handler |
|---|---|---|
| `/proc` ownership sweep (ONE per call) | 27.62 ms | 73.8 % |
| registry-scaled work (assembly + orphan metadata) | 7.8–9.8 ms | 21–26 % |
| — of which per-record proto assembly (2931 × `ToAgentSummary`) | 2.39–3.70 ms | 6.4–9.9 % |
| — of which orphan metadata (validation + user lookups + uid maps) | 4.14 ms | 11.1 % |
| `tracker.List()` copy | 0.115 ms | 0.31 % |
| disk-usage TTL cache reads (2931 × `usageFor`) | 0.149 ms | 0.40 % |
| registry read (`/var/lib/bunkerd/agents.jsonl`) | **0 ms — none exists in this path** | 0 % |
| full cached handler (2931 live records) | **37.43 ms** | 100 % |
| proto encode (connect, AFTER the interceptor's `duration_ms` is stamped) | 2.09 ms | outside |
| protojson encode (same, JSON codec) | 15.89 ms | outside |

Zeroing *every* registry-scaled component would leave 27.6 ms of sweep on this
host, and the sweep is what the daemon's own `duration_ms` measured on
bunker-mvp (11–24 ms, median 14 ms, PERF-005 evidence). **The 5 ms target is
not a registry problem; it is a per-request `/proc` sweep problem.**

## What "the cached path" is — code map

`ListAgents` (`internal/server/service.go:679`) does, in order:

1. `s.tracker.List()` — in-memory tracker snapshot, one RLock + one slice copy
   (`internal/resource/tracker.go`). **No registry file read.**
2. `liveIDs` set + `agentDiskUsageCache.pruneLive(liveIDs)` — PERF-001's TTL
   disk-usage cache housekeeping.
3. per record: `rec.ToAgentSummary()` (three `time.Format(time.RFC3339)` calls)
   and `agentDiskUsageCache.usageFor(id)` (mutex + map hit; no walk while warm —
   that is the "cached" in the row's wording).
4. `s.orphanUIDBatchSummarizer(ids)` → `(*AgentManager).OrphanUIDSummaries`
   (`internal/agent/orphan_uid.go:626`) → `checkOrphanUIDBatch` (:586) →
   - `resolveOrphanUIDMetadata` per agent (:536): `validAgentID` regex, then
     `lookupUser(username)` and `resolveUsernameUID(username)` (a **second**
     `lookupUser` on the healthy path), building `byUID`;
   - `want` = `byUID`'s key set, then `listUserProcessesByUIDWant(want)` (:364)
     → `scanProcOwners` (:291): ONE `/proc/<pid>/status` read per live pid on
     the host, PIDs sorted per uid, then `renderProcCommands` for the wanted
     uids only (PERF-021).
5. response construction (`&v1.ListAgentsResponse{...}`).

`internal/audit/interceptor.go` stamps `DurationMS`/`duration_ms` at handler
return, so connect's marshal is **not** in the 11–24 ms figure (measured here
anyway, last two rows of the table above, for completeness).

The durable registry (`internal/registry`, `/var/lib/bunkerd/agents.jsonl`,
2931 records in production) is a **boot-time input**: event rows are appended
on lifecycle transitions and replayed at manager start. Nothing under
`internal/server/` calls a registry read on this path — `grep -rn "registry\."`
across the package returns exactly two hits, both unrelated to ListAgents: a
Prometheus `GaugeFunc` in `internal/server/metrics.go:155` reading a cached
event counter, and a doc comment about a WebDAV "subscriber registry"
(`internal/server/webdav/push.go:179`). So the row's "registry read cost"
component is **structurally 0 ms**, not small-but-unmeasured: the 2931 records
enter the handler already in memory, and the 2931-agent benchmarks below pay
zero file I/O for them.

## Environment

```
host                 Linux 7.0.0-43-generic, AMD Ryzen 7 7840HS, 16 CPUs
go                   go1.26.11 linux/amd64
live pid dirs        ~770-790 during the runs (/proc/[0-9]*)
/etc/passwd          56 entries (the bench's agent users resolve through the seam)
loadavg              25.3 -> 36.5 across the blocks (shared box, fleet siblings)
```

The host was **loaded throughout** (load 25–36 on 16 cores). Absolute numbers
are therefore pessimistic and block-to-block drift is ±10–30 %; the comparison
that matters — sweep vs registry work, and the kernel floor probes — is
consistent across every block and is quoted as medians with min/max.

## Measurements

All arms live in `internal/server/perf019_components_bench_test.go` (new,
committed with this row) and run against a **2931-record** tracker seeded with
production-shaped `resource.AgentRecord`s, the user-lookup seam resolving every
agent, and the disk-usage TTL cache **warm** (frozen clock + pre-published
entries), i.e. exactly the row's "cached path".

### Registry-scaled components — `-benchtime 100x -count 9`

```
$ go test ./internal/server -run '^$' -bench \
    'BenchmarkP019_(TrackerList|ToAgentSummary|DiskUsageCacheRead|Assembly|HandlerNoOrphan|OrphanMetadata)$' \
    -benchtime 100x -count 9
```

| benchmark | runs | min ns | median ns | max ns |
|---|---|---|---|---|
| `P019_TrackerList` (List copy only) | 9 | 52 484 | **115 454** | 192 448 |
| `P019_ToAgentSummary` (2931 summaries) | 9 | 2 151 737 | **2 391 433** | 2 952 113 |
| `P019_DiskUsageCacheRead` (2931 cache reads) | 9 | 138 760 | **148 822** | 225 444 |
| `P019_Assembly` (handler minus orphan check) | 9 | 3 169 196 | **3 696 648** | 4 240 525 |
| `P019_HandlerNoOrphan` (real handler, orphan unwired) | 9 | 3 295 312 | **3 785 658** | 4 778 470 |
| `P019_OrphanMetadata` (batch classifier, sweep stubbed) | 9 | 3 592 613 | **4 142 742** | 4 709 835 |

Per-record cost of the assembly: `ToAgentSummary` 2.39 ms / 2931 = **816 ns**
per record (≈ 4 allocations/record: 3 RFC3339 strings + the summary), the
dominant part being the three `time.Format` calls.

`OrphanMetadata` at 4.14 ms / 2931 = **1.41 µs** per agent where the user
lookups are a stub (i.e. that arm already *excludes* the user database's own
cost — see "user-database cost" below).

### Differential cross-check — same handler at 4 / 41 / 2931 agents

PERF-021's own harness, same tree, same block (`-benchtime 30x -count 9`):

| arm | runs | min ns | median ns | max ns |
|---|---|---|---|---|
| `P019_ListAgents_Full/agents=4` | 9 | 34 102 142 | **36 742 354** | 40 102 923 |
| `P019_ListAgents_Full/agents=41` | 9 | 33 506 109 | **34 755 327** | 35 176 717 |
| `P019_ListAgents_Full/agents=2931` | 9 | 34 665 208 | **37 426 845** | 39 620 798 |

The three arms agree within the block's noise (±3 ms at load 36): the marginal
cost of 2927 extra live records is **~0.7 ms in this block, 2.6 ms in the
quieter block captured in the raw file** — i.e. below the noise floor of a
loaded host. The isolated component arms above (`ToAgentSummary` 2.39 ms,
`OrphanMetadata` 4.14 ms) are the lower-variance attribution of that same
work; the full-handler residual `37.43 − 27.62 = 9.80 ms` brackets them.

### The sweep shapes (registry-independent) — `-benchtime 20x -count 9`

| benchmark | runs | min ns | median ns | max ns | procs |
|---|---|---|---|---|---|
| `P019_SweepOwnershipScan` (production `scanProcOwners`, no cmdline render) | 9 | 26 401 871 | **27 623 392** | 34 854 882 | 773 |
| `P019_SweepWantAllHost` (same scan + cmdline render for every host uid) | 9 | 45 627 271 | **49 208 418** | 53 052 623 | 773 |

The delta (21.6 ms) is the command-rendering share **when the want set covers
every process on the host** — which is what a registry whose agents are all
resident looks like. PERF-021's own `P019_ProcSweepFiltered` arm reports
`wanted_pids 6.0 / wanted_uids 4.0` on this host (the bench fleet's synthetic
uids own nothing), so it measures the *lower* bound of the filter's benefit
(24.0–31.4 ms vs 37.8–42.7 ms full). PERF-021 recovered the render for uids
nobody asked about; it did not and cannot remove the ownership scan, which is
the remaining floor.

### Kernel floor probes — how cheap could the scan possibly be?

Same tree, same block, `-benchtime 20x -count 9`, ~773 pids:

| per-pid source | runs | min ns | median ns | max ns | µs/pid |
|---|---|---|---|---|---|
| `P019_ProcDirWalk` — `ReadDir` only (getdents64) | 9 | 1 230 162 | **1 340 865** | 1 505 518 | 1.7 |
| `P019_ProcDirStat` — walk + `lstat` (cheapest uid source) | 9 | 3 017 999 | **3 193 028** | 3 558 427 | 4.1 |
| `P019_ProcDirStatus` — walk + one `status` read (production rule) | 9 | 19 642 515 | **20 947 832** | 21 703 884 | 27.1 |

Reading `/proc/<pid>/status` costs **6.6×** the cheapest uid-bearing
alternative. The production scan (`27.62 ms`) sits 6.7 ms above the raw status
reads (`20.95 ms`) — that 24 % is parse/allocation overhead in
`statusOwnerUIDs` (`strings.Split` + `Fields` + `ParseUint` per pid), the only
part of the scan that is even theoretically removable:
a scratch lean-parser probe (byte-scan for `\nUid:`, reused buffer, no
`Split`/`Fields`) measured 15.3–15.8 ms vs 18.9–19.9 ms for the current
implementation on the same host at the same pid count — a ~19 % saving, still
two orders of magnitude away from the 5 ms target.

Why `lstat` cannot replace the status read: a `/proc/<pid>` directory carries
**one** uid, while `statusFileOwnedByUID` (`internal/agent/orphan_uid.go:204`)
deliberately matches **real or saved-set** uid — the exact credentials `kill(2)`
accepts for a same-uid signal. Switching to the dir owner would silently drop
the saved-set arm (and any non-dumpable process the kernel re-attributes),
i.e. it would make the detector miss orphans. That is a product semantics
change, not a perf tweak, and it is not made here.

### Encode cost (outside the handler's own `duration_ms`)

| benchmark | median | note |
|---|---|---|
| `P019_EncodeProto` (2931 summaries, binary) | 2.09 ms | 454 283 wire bytes |
| `P019_EncodeJSON` (protojson, JSON codec) | 15.89 ms | 6.0 MB allocated, 26 428 allocs |

The row's 11–24 ms is handler-only, so neither is in it; a JSON-codec client
pays the 15.9 ms on top of whatever the handler costs.

### User-database cost (why `OrphanMetadata` is a lower bound)

The orphan metadata arm stubs `lookupUser`. On this host (56-entry passwd) a
real `user.Lookup` costs **76 ns on a cached hit** and **270 µs on a miss**
(average of 2000 misses) — libc walks the NSS sources before failing. The
classifier performs **two** lookups per healthy agent (`lookupUser` at
`orphan_uid.go:538`, then `resolveUsernameUID` → `lookupUser` again at :543),
so a host whose user database is slow, or a fleet whose users are gone (the
orphan case itself), pays that twice per agent. At 2931 agents that is
2 × 2931 × 270 µs = 1.58 s in the pathological all-missing case, which is
plainly not what bunker-mvp sees (its whole handler is 11–24 ms), but it is
the one registry-scaled component whose cost is *host-shaped* rather than
fixed, and it is exactly halved by resolving the uid from the first lookup's
`*user.User` instead of looking the same name up twice.

## Why 5 ms is unreachable — the arithmetic

To land the handler at 5 ms on a host shaped like bunker-mvp, the sweep would
have to fall from its measured 11–24 ms (mvp, PERF-005 arm 3, n=10, median 14)
to under ~1 ms (since it is 74–99 % of the call). That is a **14–24×
reduction** of a loop whose per-pid cost is one `open`+`read`+`close` on
`/proc/<pid>/status`:

- measured here: 27.1 µs/pid via `status`, 4.1 µs/pid via `lstat` (6.6×),
  1.7 µs/pid for the bare directory walk;
- even the *semantically weaker* `lstat` variant would cost ~3.2 ms at 773
  pids on this host, i.e. it only just fits under 5 ms while missing orphans,
  and it grows linearly with the host's process count (mvp already pays
  11–24 ms with `status`, implying a comparable or larger pid count);
- the only variant that removes the per-pid syscall entirely is one that stops
  sweeping **per request** (cache/coalesce the scan across calls with a bounded
  TTL or single-flight). That reverses the freshness contract PERF-008 states
  in `service.go:711-712` ("every response reflects the process table as of
  this call, nothing is cached across calls") and PERF-021's AC-5. It is a
  product decision, not a worker optimisation, so it is **not** taken here.

Therefore: **at a 2931-record registry the cached ListAgents path cannot reach
5 ms while it keeps a per-request `/proc` ownership sweep with real+saved-uid
semantics.** The floor is the scan: ~11–24 ms on bunker-mvp, ~21–28 ms on this
host, of which ~76 % is irreducible kernel work and ~24 % is parse overhead
(a ~19 % saving is available there, nowhere near enough).

## What IS removable (identified, measured, deliberately NOT landed here)

Neither item changes the verdict, and both touch behaviour the row did not ask
to change, so they are reported for a follow-up row rather than slipped in:

1. **Duplicate user lookup** — `resolveOrphanUIDMetadata` looks the same name
   up twice on the healthy path (`orphan_uid.go:538` + :543 via
   `resolveUsernameUID`). Resolving the uid from the first `*user.User` halves
   the user-database work (76 ns/agent on a cache hit here; 270 µs/agent per
   miss on this host). Worth 0.1–0.2 ms on *this* host, potentially far more
   on a host with slow/missing NSS.
2. **Per-record `time.Format`** — 3 RFC3339 strings per summary = 6.4 % of the
   call (2.39 ms at 2931 records). Removable only by changing the wire shape
   (e.g. emitting Unix timestamps), which is an API change, not a perf patch.
3. **Scan parse overhead** — 6.7 ms of the 27.6 ms scan (24 %) is
   `strings.Split`/`Fields`/`ParseUint` per pid; a byte-scanning parser keeps
   semantics identical and was measured at ~19 % faster (scratch probe).

## Reproduction

```
cd /home/kara/worktrees/bunker-PERF-019
go test ./internal/server -run '^$' \
  -bench 'BenchmarkP019_(TrackerList|ToAgentSummary|DiskUsageCacheRead|Assembly|HandlerNoOrphan|OrphanMetadata)$' \
  -benchtime 100x -count 9
go test ./internal/server -run '^$' \
  -bench 'BenchmarkP019_(SweepOwnershipScan|SweepWantAllHost|ProcDirWalk|ProcDirStat|ProcDirStatus)$' \
  -benchtime 20x -count 9
go test ./internal/server -run '^$' -bench 'BenchmarkP019_ListAgents_Full' -benchtime 30x -count 9
go test ./internal/server -run '^$' -bench 'BenchmarkP019_(HandlerFull|ProcSweep|ProcSweepFiltered)$' -benchtime 20x -count 5
```

`HandlerFull`, `ProcSweep` and `ProcSweepFiltered` are PERF-021's arms in
`orphan_uid_bench_test.go` / `orphan_uid_bench_filtered_test.go`; they skip
when `/proc` is unreadable.

## Caveats (stated, not papered over)

- **The measurement host is loaded** (load 25–36 on 16 cores, fleet siblings
  running). Absolute medians are pessimistic; every conclusion above rests on
  ratios measured inside the same block or on the kernel floor probes, which
  moved together.
- **bunker-mvp was not re-measured.** Its binary is the Sep-23 build
  (`3f89359d` per PERF-005 evidence) and its 11–24 ms predates PERF-021; the
  live re-battery follows the PERF-005/006 redeploy. Nothing was deployed from
  this row.
- **2931 live records is an upper bound.** The production registry holds 2931
  records, not necessarily 2931 *live* agents; every bench record is live, so
  the registry-scaled numbers over-estimate production. The sweep does not:
  it is registry-independent and is the reason 5 ms fails either way.
- **The `WantAllHost` arm is a production shape, not a production measurement**:
  it asks the sweep to render for every uid that owns a process here, which
  bounds PERF-021's filter benefit from above (mvp's registry presumably covers
  most resident processes too).
