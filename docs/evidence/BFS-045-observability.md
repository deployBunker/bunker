# BFS-045 — every bound the invalidation path relies on is COUNTED and VISIBLE

Row: BFS-045 (P1) · worktree `wt/BFS-045` · base `eb2ecc4` · repo `/home/kara/bunker`
Design authority: `docs/prd/PRD-bunker-invalidation.md` §2.7 ("bounded means reported"),
`SPEC-watcher-capability.md`, `SPEC-push-channel.md`, `SPEC-hot-file-policy.md` §9.1/§9.2/AC-6.

**The decision, first.** The status record is the surface, and it was extended rather than
replaced: `client.cache.*` (the cache the invalidation path stores in) and
`client.invalidation.*` (the channel itself) now carry every bound, every queue, and every
flow figure this build can actually source. Where a figure cannot be sourced, it is **absent
with a reason from a four-word vocabulary** — never a zero, because a zero is a measurement
this build cannot make.

**The numbers, first.** Of the 88 figures the record publishes under those two blocks, **88
are classified and 47 are proven to move or appear** under one scripted workload (the rest are
declared bounds and the server's passthrough figures, each asserted by value); the reading is
enforced, not asserted: a figure the census does not classify **fails the test**
(`docs/evidence/BFS-045-census.txt`). Six source mutations were applied by hand to six
different counters; **all six turned their cell red and returned green after a sha256-verified
restore** (`docs/evidence/BFS-045-mutation-red.txt`). On the live route — a real FUSE mount,
the real read path, `du` as an outside witness — **the reported directory figure and `du`
agree to 112 bytes** and the over-cap read moves its counter
(`docs/evidence/BFS-045-live-mount.txt`).

Two figures this row is measured against, and both were defects of *reporting*, not of
mechanism:

| defect | what it was | what is reported now |
|---|---|---|
| **BFS-031** | the reported figure described something other than the thing it claimed to bound: `used_bytes` counts published blobs + the serialised index, and status.json, conflicts.jsonl, staged blobs and orphan blobs sat **outside it** | `cache.dir_bytes` — an independent walk of the whole directory, by class (index, blobs, staged, orphan, status, conflicts, other) — beside `used_bytes`, with the delta reported as a named figure `cache.dir_unaccounted_bytes`, and the sample's age beside the sample |
| **BFS-032** | a counter that exists and is displayed could never move from the live path: `fs_linux.go` pre-filtered `len(data) <= MaxEntryBytes` above `Cache.Insert`, so `oversize_bypasses` stayed 0 whatever the client did | one admission decision (`Cache.AdmitRead`) that **counts the refusal where the decision is made**, by reason (`over_entry_cap` vs `insert_over_entry_cap`), proven from the live read path and by a mutation arm |

---

## 1. The counter inventory

Every row below is a figure the record publishes. `source` is where the number comes from;
`moved by` is what the census test does to it (and the mutation battery removes the line that
makes it move). The inventory is `censusTable()` in
`internal/fsclient/status_census_test.go` — the test asserts that this table and the published
document are the same set.

### 1.1 The cache — the invalidation path's storage

| figure | source | what moves it |
|---|---|---|
| `max_bytes` | configured (`--cache-max-size`) | declared bound (asserted by value) |
| `max_entry_bytes` | configured (`--cache-max-entry-bytes`) | declared bound |
| `max_entries` | configured (entry bound, BFS-031) | declared bound |
| `max_inflight` | configured (staged window, BFS-038) | declared bound |
| `max_age_ms` | configured (`--cache-max-age`, the TTL backstop) | declared bound |
| `blobs_bytes`, `index_bytes`, `used_bytes`, `entries`, `blobs` | the cache's own accounting (published figures) | stores; index growth per entry |
| `in_flight_bytes` | the bytes staged refreshes hold unpublished | an open staged refresh |
| `reserved_bytes` | published + in-flight + the index they will publish | the same |
| `staged_blobs` | the width of the refresh window in use | an open staged refresh |
| `hits`, `misses` | the read path (`Cache.Get`) | a read it holds / does not hold |
| `evictions_total` | the eviction loop | an insert that must make room |
| `bypass_events` | the bound refusing an insert | a full, fully-pinned cache |
| `oversize_bypasses` | **the live read path's per-entry-cap refusal (BFS-032)** | an over-cap read |
| `pinned_blobs` | blobs a live reader holds | a pin |
| `staged_started_total` | a refresh admitted to the window | `Stage` |
| `staged_committed_total` / `staged_aborted_total` | a refresh that published / discarded | `Commit` / `Abort` |
| `staged_no_slot_total` | refused because every slot was taken | a second concurrent stage at `MaxInFlight=1` |
| `staged_no_room_total` | refused because the bound left no room | a stage whose declared size cannot fit |
| `bypass_reasons{cache_disabled, insert_over_entry_cap, no_room, over_entry_cap}` | the **closed** refusal census | each reason fired by its own site |
| `dir_bytes`, `dir_bytes_by_class{index, blobs, staged, orphan, status, conflicts, other}`, `dir_unaccounted_bytes`, `dir_measured_age_ms` | an independent walk of the directory | writing any file into it |

### 1.2 The invalidation channel

| figure | source | what moves it |
|---|---|---|
| `mode`, `mechanism`, `channel_available` | which mechanism actually answered | the channel answering (`false` → `true`, H-1: it must stop claiming a channel it does not have) |
| `seq`, `resume_seq` | the journal cursor consumed / presented (BFS-063) | applied events; an observation |
| `events_total`, `paths_dropped_total` | applied events / paths | an invalidate event carrying paths |
| `resyncs_total`, `resyncs_from_gap` | a gap, an overflow or a tree change | a sequence gap; an `overflow` |
| `stream_ends_total`, `reconnects_total` | a channel end; a reconnect attempt | a stream that ends; the reconnect after it |
| `idle_fallbacks_total` | the idle rule firing | a silently stalled stream |
| `requests_total`, `failures_total`, `last_failure` | every attempt to get an answer; those that produced none, named | a 500 from the surface; a stalled/ended channel |
| **`content_age.age_ms`, `.evidence_from`, `.observations_total`, `.bound_ms`, `.bound_source`, `.within_bound`** | **the new content-age bound:** how old this client's evidence for its view is, from what kind of evidence, and the window the mechanism in force implies | every observation, event, poll answer or channel line |
| `liveness.heartbeats_total`, `.last_line_age_ms`, `.declared_heartbeat_ms`, `.idle_timeout_ms`, `.stalled`, `.stalls_total` | the heartbeat/stall state of the pushed channel | a heartbeat line; the idle rule |
| `poll_interval_ms`, `idle_timeout_ms` | the declared poll period; the silence deadline | declared bounds |
| **`server.*`** (state, reason, detail, backend, blocks_push, vouched, stalled, heartbeat_ms, **max_paths_per_event**, coverage, **counters**: overflows, unvouched, rescans, install failures, **backend errors**, dropped events + reason, last event age, heartbeats, loop ticks, sampled_age_ms) | **the server's own capability document, reported verbatim** | nothing the client does — the arms assert the values the document published. A figure the document did NOT publish is `null` with the server's own reason, never `0` |
| `refresh.started_total`, `.in_flight`, `.max_inflight`, `.committed_total`, `.aborted_total`, `.refused_no_slot_total`, `.refused_no_room_total` | the staged window (BFS-038) | the staged flow above |
| `refresh.queue_depth`, `.queue_max_depth`, `.queue_refused_full_total`, `.skipped_oversize_total`, `.absent_reason` | **not in this build** (BFS-037) | nothing: JSON `null` with the reason `not_published` |

### 1.3 The content-age bound (the figure the client did not report at all)

`SPEC-hot-file-policy` §9 and PRD §2.7 both ask for the staleness window; the client reported
none. It now reports:

```
"content_age": {"age_ms": 997, "evidence_from": "observation", "observations_total": 3,
                "bound_ms": 4000, "bound_source": "poll_interval", "within_bound": true}
```

* `age_ms` is the age of the last **evidence for the view** (an observation, a change event, a
  poll answer, a channel line). It is deliberately not "age of the last change": a quiet tree
  is not a stale one, and a figure that conflated them would alarm on every idle mount.
* `evidence_from` names **which kind** of evidence holds the claim up. `observation` and
  `event` are content; `heartbeat` is **liveness** and is labelled as such (§5.4 O-2: liveness
  is never inferred from the absence of events, and it is never passed off as a content check).
* `bound_ms`/`bound_source` is the window the mechanism in force implies: the idle rule's
  deadline for a live pushed channel, two declared poll intervals for a poll (the interval plus
  one missed tick of tolerance). `within_bound` is that comparison, made once, in the record.
* **A resync CLEARS the evidence** (`Resync` sets `vouchedOK=false`): after a knowledge loss
  the client must re-observe before it can claim anything, so the figure is absent with
  `no_sample` — the pre-drop age would be a figure describing something other than what it
  claims, which is this row's whole subject.

---

## 2. The test that matters, and why it cannot pass vacuously

`TestEveryFigureInTheStatusRecordMovesOrIsExplained` (`internal/fsclient/status_census_test.go`).
A test that asserts a field EXISTS is what BFS-032's counter survived. This one is built the
other way round:

1. **It enumerates the figures from the DOCUMENT**, not from a hand-written list: every
   numeric and boolean leaf under `cache.*` and `invalidation.*` (88 at this commit, including
   the two bounds added last), read out of the marshalled status.
2. **Every enumerated figure must appear in `censusTable()`**, as `moves` (a workload must move
   it, or make it appear when it only exists once there is something to measure), `bound`
   (asserted by VALUE — a bound that starts counting fails), `passthrough` (asserted against
   the value the fake document published) or `appears`. A figure with no entry **fails the
   test with its own name**, so a counter added later with no reachable driver cannot ship
   quietly. This is the enforcement half: BFS-032's shape is now a regression arm.
3. **Every figure gets its own subtest**, so a failure names the figure, not the file.
4. **Bounds are checked by value** (`max_bytes`, `max_entry_bytes`, `max_age_ms`,
   `max_entries`, `max_inflight`, `poll_interval_ms`, `idle_timeout_ms`, `bound_ms`,
   `declared_heartbeat_ms`), which also catches a bound that moves.
5. **Passthrough figures are checked against a doctored document**: the fake capability
   document publishes distinctive values (overflows=7, backend_errors=5, heartbeats=11,
   loop_ticks=99, last_event_age_ms=1234, watched 3 of 4 directories…). A client that invented
   a plausible zero instead of copying the server's figure fails.
6. **The reason fields are classified too.** Every `*_reason` leaf must be one of:
   this client's own absence reason (**must open with a vocabulary class**), the server's own
   words (**asserted verbatim**), or the current mode's own note. An unclassified reason field
   fails, so a new null cannot arrive unexplained.

The workload behind it is one scripted channel and one real cache: a pushed stream that speaks
once and ends (channel end + reconnect), a second that stalls silently (idle rule + stalled
state), polled change events, a sequence gap, an `overflow`, a 500 (a failure that must be
counted and named), the cache's stores/hit/miss/pin/over-cap-read/eviction/refusal, and all
four staged-refresh refusals.

### 2.1 The negative control: every counter, neutered by hand

`probes/bfs045-mutation-red.sh` — for each group: sha256 the file, back it up, apply the exact
replacement (refusing unless its anchor appears exactly once), run the cell (**it must fail**),
restore, verify the sha256, run the cell again (**it must pass**). Transcript:
`docs/evidence/BFS-045-mutation-red.txt`.

| group | the line removed | cell | result |
|---|---|---|---|
| M1 | `c.CountBypass(over_entry_cap)` in `AdmitRead` (BFS-032's exact shape) | `TestAnOversizeReadThroughTheLivePathCountsTheBypassWithItsReason` | RED, then GREEN after restore |
| M2 | `c.stats.OversizeBypasses++` at the decision site | the census cell | RED, then GREEN |
| M3 | `i.failures++` / `lastFailure` in `noteAttempt` | the census cell | RED, then GREEN |
| M4 | `i.vouchedOK = true` in `noteEvidenceLocked` | the census cell | RED, then GREEN |
| M5 | `c.stats.DirBytes = c.dirBytes` in the walk | `TestTheReportedDirectoryFigureAgreesWithAnIndependentWalk` | RED, then GREEN |
| M6 | `c.stats.StagedStartedTotal++` in `Stage` | the census cell | RED, then GREEN |

One run also caught a **flaky assertion in the arm itself** (an equality on a figure the
invalidator's own goroutine keeps moving while the test runs) — recorded here because a test
that fails intermittently is how a real defect gets excused later. It was replaced with an
inequality plus a deterministic arm for the property it was actually checking
(`TestADeclaredCapabilityRefusalIsAnAnswerAndNotABackendError`: a surface that refuses an op it
cannot serve has ANSWERED — `requests_total≥1`, `failures_total==0`).

---

## 3. The independent measurement, and the defect this row found in its own figure

Two arms, because BFS-031's defect was a figure that agreed with nothing:

* **Go arm** — `TestTheReportedDirectoryFigureAgreesWithAnIndependentWalk` walks the directory
  *itself* (`filepath.WalkDir`, its own classification), asserts `dir_bytes` equals that walk,
  that the classes sum to the total, that `dir_unaccounted_bytes == dir_bytes − used_bytes`, and
  that the delta is at least the files `used_bytes` does not count (status.json is asserted
  non-zero; the conflict log's bytes are attributed).
* **Live arm** — `probes/bfs045-bounds-live.sh`: a real FUSE mount (built binary, real
  `davserve` surface, `XDG_CACHE_HOME` redirected to a temp dir so the user's cache is
  untouched), then `du -sb` of the mount's cache directory against the reported figure. On the
  first run the probe **failed**, and the failure was real:

  ```
  reported dir_bytes=0 (sample age 3000 ms)   du -sb = 4230
  ```

  The measurement was a whole-directory sample reused for 30 s; the mount writes `status.json`
  once a second, so the sample described a directory that no longer existed — BFS-031's shape,
  arriving from the timing direction. **Fixed in this row**: the top-level entries (index,
  status, conflicts, temps — a handful) are now re-stat'ed on every publish, and only the
  O(blobs) blob census is reused for at most `DirMeasureTTL`, with its age reported
  (`dir_measured_age_ms`). The live run then reports `dir_bytes=4350` against `du=4238`,
  a **112-byte** delta (status.json was rewritten between the two reads) with an 8 KiB
  tolerance named in the probe.

The class split is what makes the delta auditable rather than mysterious:
`{index: 26, status: 4324, blobs: 0, staged: 0, orphan: 0, conflicts: 0, other: 0}` — the bytes
`used_bytes` (26) does not count are named, one by one, on the live route.

---

## 4. The null rule in action

A NULL carries a REASON. The vocabulary is closed and lives in `internal/fsclient/cache.go`:
`disabled` · `unknown` · `not_published` · `no_sample`, each followed by a sentence. The four
facts they separate are genuinely different, so they are not merged:

| class | example from the record | why it is that class |
|---|---|---|
| `no_sample` | `content_age_reason: "no_sample: this mount has no evidence yet that its view is current"` | nothing has happened yet — the figure appears the moment it does |
| `unknown` | `server_reason: "unknown: no capability document has been received, so the server's watcher state is unknown rather than absent"` | the source exists but could not be read |
| `not_published` | `refresh.absent_reason: "not_published: the hot-refresh queue (BFS-037) is not in this build…"` | the source publishes no such figure (also: a document with no watch block) |
| `disabled` | `liveness_reason: "disabled: this mount never attempted the pushed channel (mode=poll)…"` | switched off / not applicable by configuration |

Two nulls are **not** the client's to explain, and are therefore reported **verbatim** with the
server's own reason beside them (the test asserts the words, not the class):

* `server.overflow_dropped_events: null` — "the kernel reports one overflow marker, not how
  many events it dropped". A count here would be a fabricated measurement.
* `server.unvouched_reason`, `server.counters_reason`, `server.coverage_reason` — the server's
  own probes.

**And the mirror case, found by reading the live rendering rather than by a red test:** on the
real surface this deployment publishes **no counters block at all** (no watcher has run), and
the first version of this record printed `overflows=0 unvouched=0 rescans=0 …` — five zeros for
figures the server never published, which read as "this never happened" instead of "this was
never measured". Every server-published counter and coverage count is now a pointer: **absent
(memory: `null`, on screen: `-`) with the server's own sentence beside it**. The
`TestTheDroppedEventCountIsNullWithAReason` arm asserts both directions — the values the
document DID publish, and the `null` for the ones it did not.

The hot-refresh queue's figures are `null` **in the JSON**, not omitted and not 0:
`"queue_depth": null` with `absent_reason`. A `0` there would read as "the queue was empty" —
a measurement this build cannot make (BFS-032's shape, one subsystem over).

---

## 5. What this row did NOT build, and the residuals it names

* **No feature was built to be counted.** BFS-035 (the watcher) landed; BFS-043/044 (config
  surfaces) are siblings; BFS-037 (the hot refresh) is not in this build, so its queue is
  reported as absent-with-reason rather than invented.
* **BFS-031's own repair is not here.** This row makes the mismatch *visible and named*
  (`dir_bytes`, `dir_unaccounted_bytes`, the classes); whether status.json/conflicts.jsonl get
  their own bounds remains BFS-031's decision. Nothing was changed about the cache's bound
  enforcement.
* **`oversize_bypasses` reachability is repaired here from the reporting side** (one admission
  decision that counts, by reason). BFS-032 owns any further change to the *mechanism*; the
  counter it filed is now movable from the live path and proven so.
* **The server's watch block is a sample taken at handshake** (`sampled_age_ms` beside it). A
  long-lived mount reports the bind-time sample until it handshakes again. A periodic refresh
  would be a new client behaviour, and this row does not add one.
* **The blob half of `dir_bytes` is reused for up to `DirMeasureTTL` (30 s).** The cost is a
  walk of the blob directory; the knob is `CacheConfig.DirMeasureInterval` (negative = walk on
  every read). Exposing it as a flag is BFS-044's surface, not this row's.
* **`events_total` counts applied events, not answers**: a poll that answers "nothing moved"
  increments `requests_total` but no event figure. That is the honest reading of a counter
  named `events_total`; the answer is counted where answers are counted.

---

## 6. How to re-run every claim in this document

```sh
cd <worktree>

# the census: the inventory, the bounds by value, the passthrough values, the reason classes
go test ./internal/fsclient/ -run TestEveryFigureInTheStatusRecordMovesOrIsExplained -v -count=1

# the negative control: six counters neutered by hand, each cell RED then GREEN
bash probes/bfs045-mutation-red.sh "$(pwd)"

# the live route: a real FUSE mount, the real read path, du as the outside witness
bash probes/bfs045-bounds-live.sh

# everything the change touches, and the whole repo
go vet ./... && gofmt -l internal/ cmd/ probes/ && go test ./... -count=1
```

Transcripts in this directory:

| file | what it is |
|---|---|
| `BFS-045-census.txt` | the counter-moves cell, verbose: the figure list, the values, the reason subtests |
| `BFS-045-mutation-red.txt` | the six source mutations, each with its RED and its sha256-verified GREEN |
| `BFS-045-live-mount.txt` | the live mount: the over-cap read through FUSE, the counters, `du` vs the reported figure, the human `bunker fs status` rendering |
| `BFS-045-suites.txt` | `go vet`, `gofmt -l`, and `go test ./...` on this commit |
