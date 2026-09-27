# BFS-038 — a refreshed file is never visible half-written

**Row.** BFS-038 (P0, ordered first). Land the representation the hot-file refresh (BFS-037) sits
on: a refreshed file must never be observable half-written.

**Owner documents read as design authority.** `docs/prd/PRD-bunker-invalidation.md` §2.5 *including its
2026-09-27 amendment* (the two-account split), §2.6, §2.7, §2.8, §4 (ordering);
`docs/prd/SPEC-hot-file-policy.md` §5.4 and its finding **F-1**, §5.6 (abandonment obligations),
§9.1/§9.2 (the bound census), §10 AC-15, §12 F-1/F-2, Appendix A.5/A.12.

---

## 1. The decision, and the numbers

**The refresh does not take a lock. It gets a place to put bytes that no reader can reach, and one
pointer swap to publish them.**

1. **A staged refresh writes a NEW immutable file inside `blobs/`.** The path index (`cache.entries`,
   under `cache.mu`) is the *only* way a reader reaches a blob, so an unindexed blob is unreachable —
   not in part, not at all. `Commit` renames the file to its content address (still unreferenced) and
   then performs **one** map store. Between "byte written" and "pointer swapped" there is no state a
   reader can observe; there is no third state.
2. **Two accounts, not one** (the amended rule): `used_bytes` is published blobs + index — what
   eviction reasons about and what is `du`-comparable at rest; `reserved_bytes` is
   `used + in_flight + the index growth the in-flight refreshes will publish` — the directory's real
   peak and what admission enforces. Both are separate fields in the status record. Admission
   **refuses** (`no_room`) rather than evicting: an in-flight refresh may never cause the eviction of a
   published blob (Q-8).
3. **The bound is entries as well as bytes**, both reported, because a byte bound alone does not bound a
   directory (BFS-031's lesson, which this row inherits and does not repair).

| what | measured | source |
|---|---|---|
| half-file cell, staged arm | reader observes `old` (complete) at the checkpoint, `new` (complete) after | `BFS-038-red-green.txt` |
| half-file cell, mutate-in-place control | reader observes `partial(196608 bytes)` | same |
| half-file cell, publish-then-stream control | reader observes `partial(196608 bytes)` | same |
| free-running measurement, staged arm | `old:469–911  new:104–190  miss:48–51  partial:0` | same |
| free-running measurement, in-place control | `partial:2028–6776` | same |
| F-1 over-bound arithmetic | published `461272`, in flight `579112`, reserved `1040536`, bound `1048576`; a published-only check would admit a second refresh and the directory would reach `1619496` (**54.4% over**) while every reported figure reads "inside" | `BFS-038-accounting.txt` |
| the same, under control 2 (measured on a real directory) | **`the directory reached 1619644 bytes against max_bytes=1048576`** | `BFS-038-mutation-red.txt` |
| entry bound | `entries=8/8`, `used_bytes=9401` of `8388608` (**0.11%**), `evictions_total=92` | `BFS-038-accounting.txt` |
| the same, under its mutation | `the directory holds 100 entries against an entry bound of 8` | `BFS-038-mutation-red.txt` |
| successful-path cost | **5.9–6.2 ms per 8 MiB publish** (1349–1419 MB/s); test-side 9.5 ms warm / 29 ms cold | `BFS-038-bench.txt`, `BFS-038-accounting.txt` |
| read path | `GetPinned` 1.25–3.35 ms vs `Get`+`Pin` 1.01–4.36 ms for 8 MiB (ratio 0.69–1.24, page-cache bound); one critical section instead of two | `BFS-038-accounting.txt`, `BFS-038-bench.txt` |
| read-side cells | `internal/fsclient` + `internal/fsmount`: **70 cells + 38 subtests, all green**; whole repo `go test ./...` exit 0 | `BFS-038-read-side.txt` |
| mutation battery | **5 of 5 cells' negative controls proven**, `cache.go` restored byte-identical (`sha256=508089d6…0fb0c`) | `BFS-038-mutation-red.txt` |

---

## 2. The RED, as a measurement — and what the current representation actually does

The row allows two answers here and asks for the honest one. Both were measured.

**(a) The current representation's `Insert` cannot leak a half file — measured, not asserted.**
`Insert(path, hash, data)` takes the *complete* body as one `[]byte`, holds `cache.mu` for the whole
operation, and publishes the blob with `CreateTemp` + `Rename` (`cache.go` `writeBlob`); the index is
published the same way. In the free-running measurement (8 readers hammering the public read path
`Lookup`+`Get` through 12 refreshes, every observation classified) the **shipped arm recorded 0 partial
reads**: `map[miss:48 new:151 old:576]`. There is no interleaving *of the existing API* that serves a
partial file.

**(b) But the representation has no way to hold an unpublished version at all** — and that is what the
refresh needs. There is no stage, no reservation, no in-flight census, no second pointer. So a refresh
built on what exists today has exactly two shapes:

* fetch the whole body into memory, then `Insert` — atomic, but it cannot be cancelled inside the
  window, cannot hold in-flight bytes, and therefore enforces a bound that is true only of the published
  state (F-1's defect, one layer up); or
* write the bytes where the cache already exposes them — the **mutate-in-place** shape — which *does*
  serve a partial file.

**The measured RED.** The mutate-in-place shape, run against the current representation:

* controlled schedule (the observation is taken at the refresher's declared checkpoint, so it is
  deterministic rather than a race): a concurrent reader observes **`partial(196608 bytes)`** — half of a
  384 KiB refresh — and the file named by the OLD content address no longer holds the old content, so the
  cache's own address stops describing its content;
* free-running: **2028–6776 partial observations** across runs, over the same 12-refresh schedule that
  produced 0 for a correct representation.

**A correction recorded rather than smoothed over.** The measurement harness's first draft classified a
cache **miss** together with a partial file. A miss is not a partial: a reader whose `Lookup` raced a
pointer swap reads nothing from the cache and falls through to the server — that is the read path's
existing behaviour and is correct. The bucket was split (`observeCounted`, four verdicts) and the
attribution was confirmed by a temporary in-package diagnostic that captured, for every partial, the
index hash, the index size, the file's on-disk size and whether the bytes were a prefix of either
version: **0 genuine partials**, and the whole count landed in the miss bucket. The diagnostic was
deleted; the four-way classifier is what ships.

---

## 3. The GREEN

The same cell, the same observation code, the same judgement function (`halfFileViolation`), against the
staged representation:

* at the refresher's checkpoint the concurrent reader observes the **complete OLD** content;
* after the refresh returns it observes the **complete NEW** content;
* free-running: **partial:0** (`old:469–911  new:104–190  miss:48–51`), and the measurement asserts it is
  non-vacuous (both sides were observed);
* the OLD content address still describes the OLD bytes afterwards.

**A reader that started before the swap still reads the complete OLD content.** `GetPinned(path, hash)`
returns the bytes and takes the reference on the blob in **one** critical section; after a commit the
reader's bytes are unchanged, the old blob file is still on disk (kept alive by that reader's own
reference), and only `Unpin` reclaims it (`BFS-038-cells.txt`, and `TestEvictionRespectsPinnedInFlightReaders`
drives real eviction pressure over it). Landing this cell required `GetPinned`: the previous pairing
(`Get` then `Pin`) has a window in which a commit's swap removes the entry and, if that was the last
reference, the file — a reference taken after that is a reference to nothing. The mount's read handle now
uses the single-section pairing (`internal/fsmount/fs_linux.go`, `readHandle.load`).

---

## 4. Negative control 1 — a mutate-in-place implementation must FAIL the half-file cell

Two distinct mutate-in-place shapes are implemented **in the committed test file** and run on every
`go test`, alongside the shipped arm, through the *same* cell body and the same completeness predicate:

| arm | what it does | verdict at the checkpoint |
|---|---|---|
| `staged-new-blob-swap` (shipped) | new immutable blob, one pointer swap | `old` — complete |
| `control-mutate-old-blob` | rewrite the file the old entry names, then move the pointer | **`partial(196608 bytes)`**, and the old address is destroyed |
| `control-publish-then-stream` | publish the pointer (the hash is known from the HEAD), then stream into the destination | **`partial(196608 bytes)`** |

The cell asserts the controls are red *and* that they are real breaches (half-written bytes **or** a
destroyed address), so a control that silently stopped breaching would fail the build rather than pass
quietly. The failure mode is dated and local: `BFS-038-red-green.txt`.

**And the same claim against the shipped code**, so the cell is proven to catch a regression of the
shipped fix and not only of a test-local copy: mutation `publish_before_bytes` moves the pointer at
`Stage` time and streams the bytes into the destination blob in place. Under that mutation the shipped
arm itself reports `partial(196608 bytes)` and the cell fails:
`a reader observed "partial(196608 bytes)" during the refresh: a refreshed file was visible half-written`.

---

## 5. Negative control 2 — the accounting split is load-bearing, not asserted

`TestInFlightBytesDecideAdmission` publishes ~450 KiB into a 1 MiB cache, admits one refresh of 579112
bytes (it fits: `reserved_bytes=1040536 ≤ 1048576`), and then asks for a second refresh of the same size.
The second is **refused** with `no_room`, the refusal evicts nothing (`evictions_total` unchanged, Q-8),
`staged_no_room_total` moves, and the **measured directory** stays under the bound.

Under mutation `admission_ignores_in_flight` (one line: `reservedLocked()` returns `usedLocked()`), the
second refresh is admitted and the same cell fails with the arithmetic spelled out:

```
F-1 arithmetic: published used_bytes=461272, in_flight_bytes=579112, reserved_bytes=461272,
max_bytes=1048576; a published-only admission check would admit a second 579112-byte refresh
(published+2x = 1619496 bytes on disk, 54.4% over the bound) while the directory reads 'inside'
OVER BOUND: admission admitted a second 579112-byte refresh against used_bytes=461272 (in_flight was
579112, so the directory reached 1619644 bytes against max_bytes=1048576): admission is not accounting
for in-flight bytes
```

That is F-1's proof, executed: the reported figure reads "inside" while the directory is 54% over — and
with the split, it cannot happen.

---

## 6. Both figures, separately, in the status record

`CacheStats` (which *is* `Status.cache`) now carries, as separate fields:

```
used_bytes          published blobs + index — what eviction reasons about, du-comparable at rest
in_flight_bytes     staged bytes really on disk, unreachable by any reader
reserved_bytes      used + in-flight + the staged index growth — the peak admission enforces
entries / max_entries                the second bound, enforced and reported
staged_blobs / max_inflight          the width the reservation covers
staged_committed_total, staged_aborted_total, staged_no_room_total, staged_no_slot_total
```

`TestTwoAccountsAreReportedSeparately` asserts the figures move correctly through a stage
(`used_bytes` unchanged → published), that `reserved_bytes == used + staged reservations`, that the
in-flight bytes are really on disk while being unreachable through the index, that the handoff at the
publish is continuous (`reserved` never briefly drops below `used`), and — at the **record** level — that
the marshalled `Status` document contains both figures as separate fields with the right values.
`TestEveryStagedRefusalCanMove` gives every new counter a trigger, per BFS-032's rule.

---

## 7. The bounds: bytes **and** entries

`cache_max_entries` (`DefaultCacheMaxEntries = 16384`, ≈180 B of index per entry ≈ 2.9 MiB, 1.1% of the
256 MiB byte bound) is enforced on the insert path (evict) and on the refresh path (**refuse**, never
evict). `TestEntryBoundIsEnforcedAndReported` measures the point: **8/8 entries with `used_bytes=9401` of
`8388608` (0.11%) and 92 evictions** — the byte bound would never have bounded that directory. Under
`entry_bound_dropped` the same cell reports **100 entries against an entry bound of 8**. The refresh path
is refused at the entry bound when it would *add* an entry and admitted when it refreshes a path that is
already indexed (it adds none) — the bound is not a blanket refusal, and the refusal message names the
dimension it refused on.

---

## 8. Abandonment, the refcount, and crash residue

* **An abandoned or cancelled refresh discards its blob and never swaps.** `Abort` removes the staged
  file, releases the reservation and never touches the index; it is **idempotent** (a cancel that arrives
  twice, or never, is not corrupting — PRD §2.8) and a `Commit` after it is refused. The cell asserts
  abandonment is *unobservable*: reader verdict, entry hash and size, `used_bytes`, `entries`, `blobs` and
  the directory's byte count are all identical before and after, and no staged file remains.
* **Crash residue:** a mount killed mid-refresh leaves its staged file inside `blobs/`; `OpenCache`'s
  existing orphan sweep removes it (staged files are not in the index, so they are never `keep`), and the
  cell asserts the reopened cache reports no in-flight state and that the directory holds exactly
  `used_bytes`.
* **Eviction respects in-flight readers:** a pinned old blob survives both the swap and heavy insert
  pressure, and is reclaimed the moment its last reference is released; an *unpinned* superseded blob is
  reclaimed by the swap itself (both directions in one cell).
* **The content address is verified before the publish** — bytes that do not hash to the declared key are
  never filed under it, and the refusal counts as an abandonment with no residue and no swap.
* **A refresh that fetches identical content** (the common case) must not reclaim the blob it is about to
  point at: the swap's own reference is taken *before* the old entry is dropped. The cell asserts the
  reference count and the blob census are unchanged and the refresh costs no bytes.
* **Structural bounds on the staging path**: the per-entry cap (`oversize_after_head`) and the
  reservation itself, enforced as the bytes arrive so a caller whose declared size was wrong is refused
  rather than allowed to overshoot; a short write neither raises nor lowers a live reservation.

---

## 9. Cost, and the read path

* **Successful refresh publish:** `BenchmarkStagedRefreshPublish8MiB` → **5.9–6.2 ms per 8 MiB**
  (1349–1419 MB/s at steady state, on this box: AMD Ryzen 7 7840HS). The test-side measurement on a
  freshly-created cache (first touch of every page) is 9.5 ms warm and 29 ms cold. That is the number the
  row asks for: a complete, atomic 8 MiB refresh costs single-digit milliseconds.
* **Read cost:** `GetPinned` versus the `Get` + `Pin` pairing it replaces on an 8 MiB blob:
  1.25–3.35 ms vs 1.01–4.36 ms (ratio 0.69–1.24 — both page-cache bound, and the pairing takes one
  critical section instead of two). No regression is claimed beyond measurement; the one-call snapshot
  (`snapshot.go`) and the read path were not changed except for the atomic pairing.
* **Read-side cells green:** `internal/fsclient` + `internal/fsmount` — 70 cells + 38 subtests; whole
  repo `go test ./...` exit 0 (`BFS-038-read-side.txt`).

### The one `-race` failure in this package is not this row's, and that is attributed not asserted

`go test -race ./internal/fsclient/` fails on `TestRevPollFiresOnlyWhenTheServedRevisionMoves` — a
BFS-048 fixture whose `collectDrops` is written by the invalidator goroutine while the test reads it
without synchronisation. **Attribution by control:** a pristine `git clone --shared` of HEAD (`79eda52`,
with none of this row's changes) fails the same cell with the same **4** `WARNING: DATA RACE` reports
(`BFS-038-race-baseline-control.txt`). BFS-038's own cells run clean under `-race`
(`MINE_EXIT=0`, same file). Nothing in this row touches the invalidator or its fixtures.

---

## 10. Restores are sha256-verified

The mutation battery (`docs/evidence/BFS-038-probes/mutation-red.sh` — bash + perl, one self-contained
file, because `.gitignore` forbids `*.py` outside `tools/` and a probe that cannot be committed cannot be
re-run by anyone else) backs up `internal/fsclient/cache.go`, applies each mutation with an
exact-replacement check that **refuses unless the anchor appears exactly once**, runs the named cell
(**must fail**), restores, re-verifies the sha256, and runs the cell again (**must pass**). Every group
printed `NON-VACUITY: PROVEN`, and the run ended with `FINAL RESTORE VERIFIED: the tree is byte-identical
to the start`:

```
cache.go before : sha256=508089d6290fe789b851bc65abce752db266af88a381037cfd009dc22a70fb0c
cache.go final  : sha256=508089d6290fe789b851bc65abce752db266af88a381037cfd009dc22a70fb0c
cells with a proven negative control: 5 of 5
```

| mutation | cell it must turn red | observed failure |
|---|---|---|
| `admission_ignores_in_flight` | `TestInFlightBytesDecideAdmission` | `OVER BOUND … reached 1619644 bytes against max_bytes=1048576` |
| `read_takes_no_reference` | `TestReaderHoldingTheOldBlobReadsTheOldCompleteContent` | `the old blob was reclaimed while a reader still held it` |
| `entry_bound_dropped` | `TestEntryBoundIsEnforcedAndReported` | `the directory holds 100 entries against an entry bound of 8` |
| `abort_keeps_its_blob` | `TestAbandonedRefreshDiscardsItsBlobAndNeverSwaps` | `abandonment left residue: the directory grew from 98479 to 196783 bytes` |
| `publish_before_bytes`(+`_write`) | `TestAtomicRefreshHalfFileCell` | `a reader observed "partial(196608 bytes)" during the refresh` |

---

## 11. What this row does **not** do, and what it incidentally changes

* **It does not build the hot-file refresh.** No node, queue, tracker, promotion or fetch exists here —
  that is BFS-037, and this row is the representation it will sit on (§12).
* **It does not repair BFS-031.** `status.json` and `conflicts.jsonl` still sit in the mount directory
  outside `used_bytes`: `TestPublishedFigureNeverOverstatesTheDirectory` measures the residual
  (`used_bytes=32943`, directory `34522`, gap `1579` bytes = `status.json`) and asserts only the
  direction this row owns (`used_bytes` never overstates the directory). The general repair stays
  BFS-031's row.
* **It does not repair BFS-032.** The new counters exist only because each one has a reachable trigger
  with a cell that moves it; the counters that row named as unreachable were not touched.
* **Where this row incidentally resolves a piece of BFS-031's class:** the staged bytes of an in-flight
  refresh — a *new* file in that directory, the same shape F-2 warns about — are inside the reservation
  and therefore inside the bound (`in_flight_bytes`, included in `reserved_bytes`, both reported). That
  is measured in `TestTwoAccountsAreReportedSeparately` and `TestInFlightBytesDecideAdmission`, not
  asserted. BFS-031's row is not closed by this: the pre-existing unaccounted files remain unaccounted.
* **Residual observations, reported and not filed** (the brief forbids filing):
  * the reader path is `Lookup` then `Get`, so a refresh that swaps between the two produces a **cache
    miss** (measured: 48–51 misses per 12-refresh schedule) — correct but a wasted round trip to the
    server; removing it needs a lookup-and-read primitive the policy layer does not yet need;
  * `Statfs` reports `max_bytes - used_bytes` as free (`fs_linux.go` `node.Statfs`), which ignores the
    in-flight reservation; a writer that trusts `df` could therefore overfill during a refresh. Not
    changed here — it is a mount-surface decision, and BFS-045 owns the record's drill-down;
  * the default `max_entries` value (16384) is a reasoned default from the index's measured cost per
    entry, and **BFS-044 owns the flag** that exposes it. The enforcement and the reported figure are
    here; the knob is not.

---

## 12. What BFS-037 gets (the contract it must sit on)

| call | meaning |
|---|---|
| `Stage(path, hash, expectedBytes)` | reserve admission, open an unpublished blob. Refusals: `ErrNoRoom` → the policy's `no_room` skip (S-12); `ErrNoSlot` → the item stays **queued** (a width refusal, not a skip); `ErrCacheDisabled` → `cache_disabled`. |
| `(*StagedRefresh) Write(p)` | stream bytes; enforces the per-entry cap (`ErrOversize` → `oversize_after_head`) and the reservation (`ErrNoRoom`) as they arrive. |
| `(*StagedRefresh) Commit()` | verify the content address, then **one** pointer swap. Returns `OutcomeStored`. |
| `(*StagedRefresh) Abort()` | discard the blob, release the reservation, never swap. Idempotent — safe to call twice and safe to never call (the sweep cleans up). |
| `Bytes() / Path() / Hash()` | the figures the policy's own counters need. |

The staging window has **no cancel point by construction**: there is no state between "blob complete"
and "pointer swapped" that a caller could observe or interrupt, which is Q-17's requirement expressed as
an absence rather than a flag. A stop or a cancel may therefore abandon at any time the caller likes —
the representation cannot be corrupted by the choice.
