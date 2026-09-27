# BFS-031 — the bound bounds the directory it names, and the state it does not name has its own

Row: BFS-031 (P1) · worktree `wt/BFS-031` · base `4879343` · repo `/home/kara/bunker`
Design authority: `docs/spec/BFS-005-client-cache-and-diff.md` §3 (the cache and its
bounds), `docs/prd/SPEC-hot-file-policy.md` F-2/Q-9 (a second new file must not
reproduce this defect), and BFS-045's law: every bound is counted and visible, a
reported figure must agree with an independent measurement, a null carries a reason.

## THE DECISION, FIRST

**The bound `--cache-max-size` names a directory, and that directory now holds the
cache and nothing else. The two files that are not the cache — `status.json` and
`conflicts.jsonl` — are NOT deleted, NOT truncated away and NOT squeezed: they move
beside the cache directory, into the mount directory, and get their own declared,
enforced, reported bound.**

```
<mount dir>/                  the mount's own state: status.json, conflicts.jsonl,
                              and the write path's per-handle buffer spills.
                              Bounded by StateMaxBytes() = 4 MiB (log) + 64 KiB (doc).
<mount dir>/cache/            THE CACHE DIRECTORY. `--cache-max-size` bounds THIS,
                              in full, including the index's temp copy.
```

**Why relocation rather than "bound them inside the cache bound".** Two numbers decide
it, both measured: the status document is a **7,243 B** fixed-schema document on the
live route (4,354 B on BFS-045's run, because the schema grows with the figures the
record publishes), and the refusal log **grows with EDIT VOLUME, not with tree size**
(BFS-012 measured 27,836 B of log from 40 refused writes). A 1 KiB directory bound
cannot hold either: there is no version of a 7 KB document that fits under 1 KiB, and
a log has no size of its own to fit — only a bound. The alternative, adding the two
caps to the cache's bound and reporting the sum, would have been the row's own
prohibition: it *raises the bound* the owner set (`--cache-max-size 1024` would have
become "1024 + 4 MiB") and leaves the number he set describing something other than
what he set it for. So the cache directory holds cache bytes, the bound the owner sets
is TRUE at any size including 1 KiB, and everything else is named, bounded and
reported in its own right — which is what the row's first requirement allows
("inside the accounting, or explicitly excluded by a REPORTED policy").

**And the bound now bounds the directory's PEAK, not just its rest.** `du` of the
cache directory is ≤ `max_bytes` at every instant, not only once the temps are gone:
the enforcement reasons about `dirPeakLocked` — published blobs, the staged refreshes'
booked bytes, the index **twice** (the document plus the `index.json.tmp` that
`flushLocked` writes before its rename), and whatever bytes a walk found in the
directory that this cache did not write. The added term is the reason the claim needs
no tolerance: at a 1 KiB bound with a 609 B index the temp copy alone is 609 B, which
is why the old arithmetic let the directory pass the bound (mutation M2, below).

## THE NUMBERS, FIRST

**The RED, on the unfixed tree, at a 1 KiB bound** (`docs/evidence/BFS-031-red-smallbound.txt`,
the row's own reproducer verbatim):

| figure | value |
|---|---|
| reported `used_bytes` | 26 B — "inside" the 1,024 B bound |
| `du -sb` of the cache directory | **7,965 B = 7.78x the bound** |
| of which `status.json` | 7,243 B (outside `used_bytes`) |
| of which `conflicts.jsonl` | 696 B (outside `used_bytes`) |
| the row's recorded worst case (BFS-012 §F-C) | **30,689 B = 29.97x** (40 log lines then; the refusal-hold rule records one line per refused path today) |

**The GREEN, the same bound and the same instrument** (`docs/evidence/BFS-031-live-mount.txt`,
a real FUSE mount, `du` as the outside witness):

| figure | 1 KiB bound | default bound (nothing passed) |
|---|---|---|
| `du -sb` of the cache directory | **201 B** | **376 B** |
| `max_bytes` | 1,024 B | 268,435,456 B |
| reported `dir_bytes` | 201 B (**delta 0**) | 376 B (**delta 0**) |
| peak the bound is enforced against (`dir_peak_bytes`) | 374 B | 697 B |
| read path: entries / blobs cached | 2 / 99 B | 3 / 126 B |
| the mount's state (`state.bytes`/`state.max_bytes`) | 8,666 / 4,259,840 B | 8,482 / 4,259,840 B |
| the refusal log recorded | 696 B, 1 line | 696 B, 1 line |
| footprint (`state.footprint_bytes`/`…max…`) | 8,867 / 4,260,864 B | 8,858 / 272,695,296 B |

The row's reproducer runs *inside each arm* and returns PASS on the fixed tree
(its own checks: the directory is inside the bound in every phase, the reported
figure agrees with `du` within 4,096 B — worst delta 99 B, the walk's own age — and
the state is inside its bound).

**The workload that produced the 29.97x, run again at the same bound** (ARM C of the
live probe: 40 DISTINCT refused paths — one log line each, because BFS-033's hold
refuses repeats on one path, which is why today's single-path reproducer measures
7.78x and not 29.97x):

| figure | measured now | on the unfixed tree, the same bytes |
|---|---|---|
| the refusal log | 27,023 B (the row's fixture reached 27,836 B) | inside the directory the bound named |
| `status.json` | 7,956 B | inside it too |
| `du` of the directory the bound names | **26 B** (bound 1,024 B) | **35,005 B = 34.2x the bound** (`du` of the mount directory, which IS the old cache directory) |
| where the growth is now | the state: 34,981 B of a 4,259,840 B bound, and the footprint 35,007 B of 4,260,864 B — bounded AND visible | unaccounted, at 34.2x the bound |

That is the number to read twice: the burst that produced the row's headline multiple
still produces it (a little larger, because the status document has grown from
2,217 B to 7,956 B since), and the directory the owner's number names now holds 26 B
of it.

**The negative control** (`docs/evidence/BFS-031-mutation-red.txt`): six mutations,
each removing the one thing that makes the property hold. Every owning cell went RED
with its own message, and every restore was sha256-verified byte-identical.

| group | what was neutered | the cell's own RED message |
|---|---|---|
| M1 | the layout split (`MountCacheDir` returns the mount dir) | "the observability files are inside the cache directory: status=4456 conflicts=99" |
| M2 | the peak (`2*index` → `index`) | "the index's temp copy is not reserved: used_bytes=865 + index_bytes=609 > the 1,024 B bound" |
| M3 | the log's rotation | "after 3 appends the refusal log holds 2367 B, over its 2048 B bound" |
| M4 | the document's cap | "the status document on disk is 64355 B, over its declared cap 8192" |
| M5 | the log's measured bytes | "conflict bytes = 0, want the log's 242" |
| M6 | the durable drop count | "the bound dropped entries and the count says 0" |

## 1. What changed, mechanism by mechanism

| # | mechanism | where | why it is the fix and not a patch |
|---|---|---|---|
| 1 | the cache is `<mount dir>/cache`; the state is `<mount dir>` | `internal/fsclient/layout.go` | the directory the bound names holds only bytes the bound can reason about |
| 2 | a pre-BFS-031 cache is **migrated** (rename) into it | `MigrateMountLayout` | an upgrade must not throw the owner's cache away, and the state files are left byte-identical |
| 3 | the bound is enforced against `dirPeakLocked` | `cache.go` `makeRoomLocked`, `admitStagedLocked`, `reserveStagedLocked` | `used_bytes` is the occupancy at rest; the bound names a directory, so the temp copy and the foreign bytes are reserved |
| 4 | the refusal log is capped and rotated; the dropped count is durable | `status.go` `AppendConflict` → `rotateConflictLog`, `state.go` `addConflictsDropped` | the edit-volume grower has no size of its own to fit — only a bound, and a bounded log that hid the loss would be the same defect one file over |
| 5 | one entry's `detail` is capped first | `capConflictDetail` | a single server response must never produce a line larger than the whole log's cap |
| 6 | the status document is capped where it is written, and says so | `status.go` `WriteStatus` + `state.go` `capStatusStrings`/`dropOptionalBlock` | a declared bound that nothing enforces is the row's defect with the client's own report as its subject |
| 7 | `state.*` measures the state and reports its bound; the footprint is the sum | `state.go` `MeasureState`, `fsmount.Status()` | the owner sees one number for local storage AND the two bounds it obeys |
| 8 | the state block is CENSUSED like the cache's | `status_census_test.go` (105 figures, 105 classified) | BFS-045's law: a figure nobody can move must not ship quietly |

**The peak, stated exactly.** `dirPeakLocked(extraBlob, extraIndex) = blobs + stagedBooked + extraBlob + 2*(index + extraIndex) + foreignMeasured`. `foreignMeasured` is the orphan + other bytes the last walk found: bytes in the directory the cache did not write are still bytes the directory holds, so they consume the bound (a stale sample therefore reserves bytes that are gone — erring toward the bound holding, with the sample's age reported beside the figure).

**Cost, as numbers.** The extra term is ONE index copy, and the index is measured once per call site and used twice (no extra marshal):

* capacity: 4,793 B of index against a 2 MiB-filled arm = **0.0018 %** of the bound, and **~1.1 %** at the 16,384-entry bound (≈2.9 MiB index against 256 MiB) — measured in `TestTheBoundBoundsTheCacheDirectoryAtTheDefaultBound` (32 × 64 KiB stored, **0 evictions**);
* the cache directory's walk: unchanged in kind, and now one file smaller (status.json is no longer in it);
* the state walk: 2–4 `stat` calls per status publish (status.json, conflicts.jsonl, conflicts.dropped, any spill), measured fresh every publish — no memo, so no stale figure;
* the log's rotation: only when an append would pass the cap; one read + one rewrite of ≤4 MiB, and the durable count is one small write;
* the document's cap: one length check per write; the reduction only fires when the document is over 64 KiB (never in the live run).

**The paths that must not regress.** The read path served the whole tree in both live arms (126 B in 2 requests) and the cache held what it read (2 entries/99 B at 1 KiB, 3/126 B at the default); the eviction path still evicts when it must: in the Go 1 KiB arm, 64 entries went in, 1 refused once nothing was evictable (pinned), `bypass_reasons.no_room=1`; at the default-shaped bound 2 MiB went in with 0 evictions. The two existing arms whose fixtures were tuned to the OLD arithmetic were adjusted and the reason is in the code (a 700 B bound could no longer store even one 512 B entry, because the entry now costs its blob plus two index copies).

## 2. The agreement, as a TEST (BFS-045's arm, reused and extended)

`internal/fsclient/status_census_test.go::TestTheReportedDirectoryFigureAgreesWithAnIndependentWalk`
already walked the directory for itself and asserted `dir_bytes`, the class split, the
sum and the unaccounted delta. It now also asserts THE LAYOUT AND THE BOUND, in the same
arm, on the same counters — no parallel figures were added:

* `cs.DirBytes == ` an independent walk of the CACHE directory (unchanged);
* the classes sum to the total; `dir_unaccounted_bytes == dir_bytes − used_bytes`;
* **`dir_bytes <= max_bytes`** and **`dir_peak_bytes <= max_bytes`** (the row's claim);
* **`used_bytes + index_bytes <= max_bytes`** — the temp copy is reserved, stated sharply enough that a cache filled to the old arithmetic fails it;
* **`dir_bytes_by_class.status == 0 && …conflicts == 0`** while both files exist in the mount directory beside it (the decision, asserted);
* the sample's age is still reported beside the value.

New arms (numbered by the property they own):

| arm | what it proves |
|---|---|
| `bound_directory_bfs031_test.go::TestTheBoundBoundsTheCacheDirectoryAtATinyBound` | at 1 KiB: every byte the directory holds is inside the bound after EVERY insert (stored=64, refused=1, `dir_bytes=445` = an independent walk), the peak is inside it, and the refusals are counted by reason |
| `…::TestTheBoundBoundsTheCacheDirectoryAtTheDefaultBound` | the same at the real default (32 × 64 KiB stored, 0 evictions, walk == reported, reservation cost 0.0018 %) |
| `…::TestAForeignFileInTheCacheDirectoryConsumesTheBound` | a 600 B file this cache did not write is refused a 512 B blob under a 1 KiB bound, attributed to the `other` class, and named in the delta |
| `…::TestAPreBFS031CacheIsMigratedIntoTheCacheDirectory` | the migration moves the index and the blobs, removes a stale index temp, returns nothing to do on a second run, and leaves `status.json`/`conflicts.jsonl` byte-identical |
| `state_bound_bfs031_test.go::TestTheRefusalLogIsBoundedAndTheDropIsCounted` | an 8 KiB cap over 200 appends: the file never passes the cap, present + dropped == appended (3 + 197 == 200), the newest entry survives, one entry's detail is capped |
| `…::TestTheStatusDocumentIsBoundedByItsOwnCap` | a 64,355 B document lands at 5,479 B under an 8 KiB cap, is NAMED as reduced with a vocabulary-class reason, carries the truncation marker, and an ordinary document is NOT reduced |
| `…::TestTheStateMeasurementNamesEveryResidentAndItsBound` | every resident is measured (status/log/spill), the footprint is cache + state, both bounds are reported, and an unreadable directory is absent WITH A REASON |
| `…::TestTheStateBoundHoldsAcrossASession` | 60 rounds of insert + refuse + status write: the cache directory, the state and the footprint stay inside their bounds, the log drops 48 entries under an 8 KiB cap, and the footprint equals an independent walk of the mount directory |

The live probe `probes/bfs031-bound-live.sh` is the same claim on the real route, with
`du` as the witness, at **both** bounds, waiting for a FRESH directory measurement
(the reported figure carries its own age and is reused for up to 30 s by design, so the
probe waits rather than widening a tolerance). Its stated tolerance is 4,096 B and the
observed delta is 0 B.

## 3. The null rule, and where it had to be extended

`state.reason` is in the absence-reason vocabulary set (a mount directory that cannot
be read is `unknown: …`, never a zero that reads as "the state is empty"), and the
document's own reduction carries `reduced_reason` opening with a class. The
`state.*` reason field is `omitempty`, so a measured state does not publish an empty
string that the census would have to call a null without a reason.

One dependency is stated rather than hidden: `state.footprint_bytes` is the cache
directory's measured bytes (`cache.dir_bytes`) plus the state's, so when the CACHE's
walk fails the footprint is short by that figure — and the reason is visible beside it
in `cache.dir_bytes_reason`, in the same document.

## 4. What this row did NOT do — the residuals, named

* **No new CLI flag for the state's caps.** `StatusMaxBytes`/`ConflictsMaxBytes` are
  declared, enforced and reported, but they are package bounds (variables, so a test
  can drive a small cap), not flags: the flag surface is BFS-044's to extend. The
  figures are reported today, so the owner can see them without a flag.
* **The write path's per-handle spill** (`writebuf-*`) is bounded per handle by
  `--write-buffer-max-bytes` (a package constant today; the flag is validated against
  it) and its aggregate is the number of open buffered handles, which the record
  reports (`write_handles_buffered`). The footprint's bound is computed from that
  count, which is why the bound moves with it.
* **The measurement is reused for up to `DirMeasureTTL` (30 s)** for the blob half; the
  top level is re-stat'ed on every publish and the age of the sample is reported.
  Exposing the knob is BFS-044's surface (BFS-045 §5).
* **The server's own passthrough strings** are the one input to the status document
  this client cannot bound at the source; the document's own cap catches them at write
  time (per-field cap → block omission → the minimal document). The capability
  document that carries them is itself bounded by the negotiated frame bound
  (BFS-062).
* **The hot-file tracker (BFS-037) is not in this build.** When it lands, it belongs
  INSIDE `cache/` if it is to be inside the admission reservation — which is exactly
  what `SPEC-hot-file-policy` F-2 asks BFS-031 to decide, and this is the decision:
  a file the cache writes is cache bytes, and cache bytes live in the cache directory.
* **One refusal line per refused PATH**: the live arm records a single log line for
  20 refused writes, because the refusal-hold rule (BFS-033) refuses repeats on a path
  that still holds. That is BFS-033's behaviour, not this row's, and it is why the
  live log is 696 B rather than the 27,836 B BFS-012 measured.

## 5. How to re-run every claim

```sh
cd <worktree>

# the RED numbers are in the transcript, produced by the row's own reproducer on the
# unfixed tree; the GREEN is the same reproducer inside the probe:
bash probes/bfs031-bound-live.sh

# the negative control: six mutations, each cell RED then sha256-verified GREEN
bash probes/bfs031-mutation-red.sh "$(pwd)"

# the agreement arm, the bound arms and the state arms
go test ./internal/fsclient/ -run 'TestTheReportedDirectoryFigureAgreesWithAnIndependentWalk|TestTheBoundBounds|TestAForeignFile|TestAPreBFS031|TestTheRefusalLog|TestTheStatusDocumentIsBounded|TestTheState' -count=1 -v

# the census (every figure moves or is explained)
go test ./internal/fsclient/ -run TestEveryFigureInTheStatusRecordMovesOrIsExplained -count=1 -v

# everything the change touches, and the whole repo
go vet ./... && gofmt -l internal cmd probes tools && go test ./... -count=1
```

Transcripts in this directory:

| file | what it is |
|---|---|
| `BFS-031-red-smallbound.txt` | the RED: the row's reproducer on the unfixed tree at a 1 KiB bound (7,965 B = 7.78x the bound) |
| `BFS-031-live-mount.txt` | the GREEN: the live probe at 1 KiB and at the default, with `du`, and the row's reproducer run inside each arm |
| `BFS-031-mutation-red.txt` | the negative control: six mutations, each owning cell RED, every restore sha256-verified |
| `BFS-031-suites.txt` | `gofmt -l`, `go vet ./...` and `go test ./... -count=1` on the commit this document describes |

## 6. The docs this changes

* `docs/spec/BFS-005-client-cache-and-diff.md` — the on-disk layout table (§ cache dir)
  now records the mount directory / cache directory split and the state's own bound.
* `docs/prd/SPEC-hot-file-policy.md` F-2 — the decision F-2 asks BFS-031 for: a file
  the cache writes is cache bytes and belongs in the cache directory, which is what
  puts it inside the admission reservation.
