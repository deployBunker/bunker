# BFS-046 — the invalidation test program and its controls

**Row:** BFS-046 (P1) · **Repo:** bunker · **Worktree:** `wt/BFS-046` · **Author:** Hermes
**Status:** the gate. BFS-036/037/039/047 depend on this row, which is why it was filed *ahead* of the
builds: the cells exist before the features they judge.
**Normative inputs (read, not summarised):** `docs/prd/SPEC-watcher-capability.md` §4/§5/§9/§11 and App. A.4 ·
`docs/prd/SPEC-push-channel.md` §3/§5/§6/§8/§12 · `docs/prd/SPEC-hot-file-policy.md` §0.1/§4/§5.5/§7.2/§10 ·
`docs/prd/PRD-bunker-invalidation.md` §2.
**Code under test (landed):** `internal/server/webdav/watch.go` (BFS-035) · `internal/fsclient/invalidate.go`
(BFS-026/048/060/063) · `internal/fsclient/hotpolicy.go` (BFS-044) · `internal/fsclient/cache.go` (BFS-038) ·
`internal/invalidation/` + `internal/config/invalidation.go` (BFS-043) · `internal/fsclient/status.go` (BFS-045).

**Deliverables of this row:**
`internal/server/webdav/bfs046_program_test.go` · `internal/fsclient/bfs046_program_test.go` ·
`docs/evidence/BFS-046-arms.sh` + seven `.patch` mutations · `docs/evidence/BFS-046-coverage.sh` · this document.

---

## 0. Verdict in one screen

| # | cell | the DEFECT it catches | the mutation that turns it red | status |
|---|---|---|---|---|
| 1 | SMOKE | the channel and the watcher never come up together — a watcher claiming `watching` with no watch set, a channel op refused, or a change that reaches neither the served revision nor the ledger | `--nochannel`: the channel's poll form (`X-Bunker-Op: events`) is refused | **LIVE** — RED under the mutation |
| 2 | INTEGRATION (real non-WebDAV edit) | an invalidation path wired to the **request handler** instead of the filesystem, i.e. blind to `git checkout`, an editor, a deploy (PRD R1) | `--nowatchpublish`: the watcher observes and never publishes to the served revision | **LIVE** — RED; writes are `/bin/sh` and real `git checkout` |
| 3 | OVERFLOW | an overflow counted but **not repaired** — a `rescans_total` that moves while nothing is re-observed | `--norescan`: the rescan request is dropped | **LIVE** — RED |
| 4 | HALF-FILE | a refresh implemented as a **mutation of the cached copy**, so a concurrent reader reads a torn file | `--mutate-in-place`: `Cache.Stage` reuses the blob the path already names | **LIVE** — RED, measured `partial(49152 bytes)` |
| 5 | STAMPEDE | an average that looks fine while **a single foreground read stalls** — the requirement is foreground latency under a burst, not aggregate throughput | (pending) | **PENDING-UNTIL-BFS-037** |
| 6 | STOP | a stop that silently lifts: **`drain-then-idle`** and **`let-in-flight-finish`**, the two readings §5.5 refuses | (pending) | **PENDING-UNTIL-BFS-037** |
| 7 | PROMOTION | a promotion that **double-fetches** — N concurrent readers producing N GETs where the invariant is exactly 1 | (pending) | **PENDING-UNTIL-BFS-037** |
| 8a | CANCEL — killed reader | correctness that depends on the reader's **lifetime**: a persisted pin/lock poisoned by SIGKILL | `--leaked-lock`: the pin becomes a persisted in-use marker | **LIVE** — RED |
| 8b | CANCEL — killed refresher | an abandoned refresh leaving **unbounded disk growth** (BFS-031's class) | `--no-sweep`: the orphan sweep removes nothing | **LIVE** — RED, residue `.stage-2532603744` |
| 8c | CANCEL — deliberate (FUSE interrupt, `EINTR` vs `EIO`) | a cancel that is indistinguishable from a failure | `--cancel-class`: the cancel is not attributed to the caller | **LIVE** — RED; completed by BFS-039 |
| 9a | SIZE RULE at the boundary | an **exclusive** comparison at 8 MiB — a one-byte difference no coverage number can see — or a refusal that does not name both numbers | `--exclusive-size`: `>` becomes `>=` at the bound | **LIVE** — RED |
| 9b | SIZE RULE skip census | a skip that happens **without a reason counter** (the BFS-032 shape), or a reason with no reachable trigger | (pending) | **PENDING-UNTIL-BFS-037** |
| 10 | COVERAGE FLOOR | coverage drifting below the invalidation surface's measured floor | (a regression; enforced by `BFS-046-coverage.sh`) | **LIVE** — MERGED **75.4%** (floor 75.0) |

Every cell that claims to catch a defect **has** its mutation, and every mutation is proven in
`docs/evidence/BFS-046-arms.txt` with a **sha256-verified restore**. Cells waiting on 036/037/039 are marked
`PENDING-UNTIL-…` and are **gated**, not weakened (§3).

**The one-line result of the program as it stands:** `green` PASSES; `all` reports seven mutations, seven RED
cells, seven attribution cells unmoved, seven verified restores, and the coverage floor held.

---

## 1. What the program is, and the rule it is built on

> **A TEST THAT CANNOT FAIL PROVES NOTHING.**

This repository has paid for that rule three times already (BFS-031's bound that did not bound, BFS-032's
counter that could never move, BFS-034's old verification passing a broken write), so the program is written as
**cells with a named defect each**, and every claiming cell carries the **source mutation** that turns it red.
A cell with no mutation is a cell with no evidence, and is listed as pending rather than counted.

Two instruments are used, and the choice is deliberate:

- **A REAL backend** (inotify via fsnotify) for cells 1–3. Cell 2's defect class — a channel wired to the
  request handler — is **invisible to a fake backend**: a fake publishes only what the test hands it, so a
  fake-based cell cannot distinguish "the watcher saw the filesystem change" from "the test said so".
- **A REAL killed process** (a re-executed test binary, `SIGKILL`) for cell 8. A goroutine cannot be killed,
  and "no cancel ever arrives" is the whole point of PRD §2.8.

---

## 2. The cells, and what each one measurably catches

### Cell 1 — SMOKE · `webdav/TestBFS046Cell01SmokeWatcherAndChannelComeUp`

The channel and the watcher come up **together**, and a change made by a real shell is seen. It asserts the
watcher's state, backend and **coverage completeness** (`directories_watched == directories_desired` — a short
set is W-4 and must never be reported as `watching`), that the poll form answers an `events` array, that the
capability document reports `state:"watching"`, that the served revision moves for the write, that the change
reaches the ledger the channel serves, and that `revKind()` is the **declared** composite `git+watch` (R-V3).

RED under `--nochannel` (the channel's poll form refused, the watcher untouched):

```
bfs046_program_test.go:148: the channel's poll form answered 501: {"ok":false,"op":"events",
    "verdict":"capability_unavailable", … "error":{"capability":"events","scope":"target",
    "detail":"BFS-046 red-proof mutation: the poll form is refused"}}
--- FAIL: TestBFS046Cell01SmokeWatcherAndChannelComeUp
```

and under `--nowatchpublish` (the watcher up, the change not published):

```
bfs046_program_test.go:165: timed out after 5s waiting for the served revision to move for a real shell write
--- FAIL: TestBFS046Cell01SmokeWatcherAndChannelComeUp (5.02s)
```

### Cell 2 — INTEGRATION over a REAL NON-WebDAV edit · `webdav/TestBFS046Cell02OutOfBandEditOverRealFilesystem`

**This is the cell that matters most.** The writer is **not** WebDAV, and the test issues **no request that
mutates the tree**:

- `/bin/sh -c 'printf "%s" "$1" > "$2"' sh <body> <abs>` — a shell write on the target;
- a real `git init` repo, a dirty working tree, then **`git checkout -- tracked.go`** — git rewriting tracked
  bytes in place, exactly as a branch switch does.

Both arms are asserted through the *served* path: the revision moves, and a subsequent `GET` returns the new
bytes (D4's forget + D5's ledger, R2's second half).

**THE CONTROL that makes it non-vacuous.** The same tree, no watcher, the identical write: nothing moves,
and `revKind()` is the un-extended `git`. Without it, movement could be an artefact of something else.

RED under `--nowatchpublish` (both arms):

```
bfs046_program_test.go:217: timed out after 5s waiting for the watcher to vouch for the shell write
bfs046_program_test.go:272: timed out after 5s waiting for the dirty write to be vouched for
--- FAIL: TestBFS046Cell02OutOfBandEditOverRealFilesystem (10.04s)
```

### Cell 3 — THE OVERFLOW CELL · `webdav/TestBFS046Cell03OverflowIsACountedFullRescanAndNeverQuiet`

The interval the kernel cannot vouch for is **never reported as quiet**, and the overflow is **always a
counted full rescan**. The deterministic arm parks the event loop, publishes `IN_Q_OVERFLOW` on the **error**
channel, and asserts: the overflow is *counted while the loop is parked* (O-1 — the drain is a goroutine of its
own), the state is `overflow` and `vouched` is false, `rescans_total` is still 0 (the rescan is the loop's work,
not the drain's), then after release `rescans_total == 1`, exactly **one** `overflow` line with an **empty**
path list, the tree actually re-observed (the revision moved), and `unvouched_total == 1` (intervals, not reads).

RED under `--norescan` — the brief's own requirement, *"with the rescan neutered the cell goes red"*:

```
bfs046_program_test.go:357: timed out after 5s waiting for the forced full rescan, counted
--- FAIL: TestBFS046Cell03OverflowIsACountedFullRescanAndNeverQuiet (5.01s)
```

**The BFS-035 arms this cell stands on, re-run on this tree** (`docs/evidence/BFS-046-overflow-reproducer.txt`):

- **The real-kernel reproducer** (`TestWatchOverflowRealKernelReproducer`, App. A.4's measured trap, driven
  through the real library):

  ```
  BFS-035 kernel reproducer: max_queued_events=16384, 16384 files created,
      overflows_total=1, unvouched_total=1, rescans_total=0
  --- PASS: TestWatchOverflowRealKernelReproducer (16.74s)
      --- PASS: .../the_drain_arm_observes_the_kernel's_overflow_as_the_backlog_drains (0.93s)
      --- PASS: .../the_no-drain_arm_gets_SILENCE_(the_trap_R-6_names) (15.81s)
  ```

  The drain arm receives the marker; the **no-drain arm gets silence** — the R-6 trap reproduced rather than
  asserted.
- **The `disableDrain` control** (`TestWatchOverflowNegativeControlDisablesTheDrain`): with the drain off the
  overflow is *invisible* — counters flat, state `watching`, `vouched` true. It is the attribution cell for the
  `--nowatchpublish` mutation below, i.e. it proves the two mutations land in different places.

### Cell 4 — THE HALF-FILE CELL · `fsclient/TestBFS046Cell04HalfFileCellIsLoadBearing`

The cell that proves BFS-038's representation is **load-bearing**, not decorative. The landed cell
(`TestAtomicRefreshHalfFileCell`) runs the shipped representation beside **two mutate-in-place implementations
written in the test** (`mutateOldBlobRefresh`, `earlyPublishRefresh`), graded by one shared judgement, and
fails if either control stops being caught ("NEGATIVE CONTROL IS BLIND"). This row adds what was missing: the
**source mutation**.

RED under `--mutate-in-place` (`Cache.Stage` opens the blob the path already names instead of a fresh
unpublished one) — **the shipped cell fails first**, then the program's cell:

```
cache_staged_test.go:340: a reader observed "partial(196608 bytes)" during the refresh: a refreshed file was
    visible half-written
--- FAIL: TestAtomicRefreshHalfFileCell
bfs046_program_test.go:104: a reader observed "partial(49152 bytes)" with a stage in flight: a refreshed file
    was visible half-written
--- FAIL: TestBFS046Cell04HalfFileCellIsLoadBearing
```

A concurrent reader was handed **49152 bytes of a 98304-byte file**. The cell catches the bug it exists for.

### Cell 8 — CANCEL: a deliberate cancel and a KILLED reader · `fsclient/TestBFS046Cell08*`

`TestBFS046HelperProcess` is re-executed as a **child process** and **SIGKILLed** mid-operation; a goroutine
cannot be killed and a killed process sends no cancel.

- **Killed reader** (pinned and reading): the blob is **byte-identical** afterwards (a reader's death is not a
  write), the path is reachable from a **fresh handle**, no staged residue is left, and a second reader can
  still pin the same blob.
- **Killed refresher** (bytes on disk, nothing published): nothing is published — the old content keeps being
  served and the index never moves — the residue is **swept** on reopen, `used_bytes` is unchanged, and
  `reserved_bytes` returns to `used_bytes` (no phantom reservation, which would eventually refuse every refresh).

RED, both arms:

```
--- leaked-lock ---
bfs046_program_test.go:332: the blob could not be pinned after a killed reader: the pin is being held on the
    dead process's behalf
--- FAIL: TestBFS046Cell08KilledReaderLeavesTheBlobAndThePathIntact

--- no-sweep ---
bfs046_program_test.go:381: the staged residue survived the reopen: [.stage-2532603744] (an unpublished blob
    left behind is unbounded disk growth)
--- FAIL: TestBFS046Cell08KilledRefresherNeverPublishesAndIsSwept
```

> **A finding from building these arms, recorded because it changed a patch rather than a cell.** The first
  `--leaked-lock` mutation put the pin marker **inside `blobs/`**, and the cell still PASSED: the orphan sweep
  on the next `OpenCache` removed it, so the "defect" healed itself across a reopen. A defect that heals itself
  is not the defect cell 8 exists to catch, so the mutation was **re-targeted at a persisted marker outside the
  swept directory** — and then the cell goes red. The cell was right; the mutation was wrong.

### Cell 9 — THE SIZE RULE at the boundary · `fsclient/TestBFS046Cell09SizeRuleBoundaryIsInclusiveAndCountedByReason`

The pinned number is asserted against the **literal** (8388608), the **inclusive** rule is read back off the
running policy (`effective.size_rule_inclusive`, not prose in a spec), and **both** mount-time refusals are
asserted **at the edge**: exactly at the bound is **accepted**, one byte over is **refused naming both numbers**
(S-10 against the per-entry cap, S-9 against the cache bound — the S-9 edge on an environment that raises the
per-entry cap so the edge under test is S-9's alone). While the hot path is **disarmed**, the same edge is not a
refusal but a **reported** relation failure naming both numbers (S-11) — a silent `false` for an unmeasurable
fact is forbidden.

RED under `--exclusive-size` (`>` becomes `>=` at the bound):

```
bfs046_program_test.go:449: a size rule EXACTLY at the per-entry cap was refused (bunker-fs: --hot.max-file-bytes
    (67108864) exceeds the cache's per-entry cap (--cache-max-entry-bytes 67108864); … S-10 …): the comparison at
    the bound is exclusive, which is the off-by-one this cell exists to catch (S-10 is `>`, not `>=`)
--- FAIL: TestBFS046Cell09SizeRuleBoundaryIsInclusiveAndCountedByReason
```

### Cell 10 — THE COVERAGE FLOOR, as a NUMBER · `docs/evidence/BFS-046-coverage.sh`

Enforced (`docs/evidence/BFS-046-coverage.txt`, `FLOOR-EXIT=0`):

| package | measured | floor |
|---|---|---|
| `internal/invalidation` | **82.3%** | 82.0% |
| `internal/fsclient` | **69.6%** | 69.0% |
| `internal/server/webdav` | **81.3%** | 81.0% |
| **MERGED (all three)** | **75.4%** | **75.0%** |

The floors sit just below the measurements so a regression trips them and ordinary churn does not; the
merged figure is the number this row states for the invalidation packages.

---

## 3. The cells that are WAITING — gated, never weakened

Cells 5, 6, 7 and the two remaining halves of 8 and 9 cannot be written against features that are not in the
build: **BFS-036 does not serve the push form** (`ops.go` still answers the `watch` op with
`501 capability_unavailable` — the refusal is correct and is not a bug), **BFS-037 has no refresh** (only the
policy surface, `hotpolicy.go`, is landed and `DefaultHotEnabled` is `false`), and **BFS-039's cancel-IO has
LANDED** — it completed cell 8c, so the `EINTR` half of the cancel cell is no longer waiting and the clause
that guarded it has been retired (see §3's note below). Rather than weaken them into "the vocabulary is listed",
the claims are recorded here in the form they must take, and they are **gated**:

`fsclient/TestBFS046PendingUntilLandingGate` **fails** the moment BFS-037's vocabulary appears in the package
(probed by the spec's own identifiers — `singleflight`, `hot.json`, `hot_queue`, … — rather than by "a new file
appeared", so a sibling landing an unrelated file cannot trip it), and again the moment BFS-039's `EINTR`
vocabulary appears. A `t.Skip` that outlives its feature is a green that means nothing, which is the class this
row closes.

**FULFILLED, 2026-09-27 — the BFS-039 clause has FIRED and been retired.** The gate failed the moment BFS-039
landed the cancel vocabulary, exactly as designed, and
`…Cell08DeliberateCancelIsDistinguishableFromFailure` was completed in the same change: it now drives the
deliberate cancel against the landed surface and asserts `EINTR`/`cancelled` for the cancel against
`EIO`/`server_error` for a 5xx, that the cancelled path does not exist and its neighbour is byte-identical,
that the retry lands once, and that a conditional write on the SAME path proceeds afterwards (the per-path
commit lock is free, latency reported). The gate's BFS-039 clause is replaced by its inverse — the namespace
must still contain the vocabulary, or the completed cell would silently stop exercising anything — and it is
proven red under the `cancel-class` mutation in `docs/evidence/BFS-039-arms.sh`.

**The pending cells are REAL test functions**, not prose: `TestBFS046Cell05StampedeForegroundLatencyUnderABurst`,
`…Cell06StopInFullRefusesTheTwoWrongReadings`, `…Cell07PromotionIsSingleFlightExactlyOneFetch`,
`…Cell08DeliberateCancelIsDistinguishableFromFailure` and `…Cell09SkipCensusByReasonIsDrivable` each exist, each
names the defect it catches in its own doc comment, and each **SKIPs with the exact claim it will carry** — and
each converts that skip into a `t.Fatal("PENDING CELL NOW DUE: …")` the instant its dependency lands, so the
skip cannot survive the feature.

| pending cell | the exact claim it will carry when its dependency lands |
|---|---|
| **5 STAMPEDE** | With a foreground stream against a throttled server, the foreground's own **latency distribution** is unchanged versus a hot-path-disabled build (AC-9). The observable is the foreground's numbers, **not** a refresh starved/un-starved count: an average that looks fine while a single foreground read stalls **is the failure**. |
| **6 STOP** | STOP IN FULL does exactly §5.5: new work refused **and counted while stopped** (level-triggered); `queue_depth == 0` within `hot_stop_deadline`, each dropped item counted by reason `stopped`; an in-flight refresh abandoned with **no publish** (the path's cached hash, `used_bytes` and the reader's bytes bit-identical, `in_flight_bytes` back to 0, no temp blob, no slot): and it must **assert the two REFUSED readings** — a `drain-then-idle` stop fails the depth clause, a `let-in-flight-finish` stop fails the deadline clause; a **promoted** fetch **survives** the stop (AC-8 i–iv). |
| **7 PROMOTION** | 32 concurrent readers of one cold tracked path with a queued refresh produce **EXACTLY 1 GET on the wire**, asserted **at the server's own request count** (not a client counter), with 1 leader + 31 joins and `singleflight_entries` back to **0 at rest**. "At most one" is **not** the claim; the control arm is the map disabled, which must observe 32 GETs (AC-11/AC-12). |
| **8c DELIBERATE CANCEL** | A deliberate FUSE-interrupt cancel is distinguishable from a failure in the error returned to the kernel (`EINTR` vs `EIO`), releases the per-path commit lock, and leaves the previous content intact (PRD §2.8); the absent-cancel case is already covered by 8a/8b above. |
| **9b SKIP CENSUS** | One cell per reason in S-12 (`oversize_pre`, `oversize_after_head`, `untracked`, `disarmed`, `stopped`, `cache_disabled`, `no_room`, `pinned_eviction`, `queue_full`, `resync`, `tree_mismatch`, `not_found`, `replaced`) and P-13, each **driving the live mount** (a real read, a real invalidation, a real stop) and asserting the counter moved — with the **BFS-032 control**: insert a pre-filter above any counter and the affected cell must fail. The refresh-side file-size boundary (S-3/S-4, `size <= hot_max_file_bytes` inclusive) belongs here too. |

**Cell 9 as landed says which half it is.** It asserts the pinned number, the declared inclusive rule, the two
mount-time refusals **at the edge**, and **logs** that the census half is pending — it does not pretend to
assert the census (see the transcript line quoting exactly that).

---

## 4. The mutation table (the instructions, not the summary)

`docs/evidence/BFS-046-arms.sh` — one script, one mode per mutation, each with an **attribution** arm that must
stay unmoved (so a mutation is attributed to the defect it names and not to a general breakage), and each with
a **sha256-verified restore**.

| mode | file | mutation | RED cell (must fail) | attribution (must pass) |
|---|---|---|---|---|
| `nochannel` | `ops.go` | the `events` op answers `501 capability_unavailable` | cell 1 | cell 2 (it drives the ledger directly, never the op) |
| `nowatchpublish` | `watch.go` | `noteWatchedChanges` stops publishing to the served revision | cell 1, cell 2 | `TestWatchOverflowNegativeControlDisablesTheDrain` |
| `norescan` | `watch.go` | `requestRescan` drops the request | cell 3 | cell 2 |
| `mutate-in-place` | `cache.go` | `Cache.Stage` reuses the named blob | cell 4 **and the shipped `TestAtomicRefreshHalfFileCell`** | cell 9 |
| `leaked-lock` | `cache.go` | the pin becomes a persisted in-use marker | cell 8 (killed reader) | cell 8 (killed refresher) |
| `no-sweep` | `cache.go` | the orphan sweep removes nothing | cell 8 (killed refresher) | cell 8 (killed reader) |
| `exclusive-size` | `hotpolicy.go` | `>` becomes `>=` at the size-rule bound | cell 9 | `TestHotPolicyDefaultsAreThePinnedNumbers` |

The full transcript is `docs/evidence/BFS-046-arms.txt` (`ARMS-EXIT=` at the end). **Seven mutations, seven RED
cells, seven attribution cells unmoved, seven restores verified:**

```
restore: internal/server/webdav/watch.go   sha256=74211882400483020c2231f9f5197470eb3ee186f50fb793a684fd291396d138 (verified)
restore: internal/server/webdav/ops.go     sha256=5dc7f4fe8367fe260ac3ea6b1f95a27c018f592c77ff5999764339af1afc0687 (verified)
restore: internal/fsclient/cache.go        sha256=ff928ac73d7d219232ba49525e66e928b8635bb676316496ed33438e79eabd4e (verified)
restore: internal/fsclient/hotpolicy.go    sha256=0b7538b0a3fba4fcba05a715061bcf3dc704b297b4e286b2c03c4956e16d1e01 (verified)
```

---

## 5. The one-call snapshot and the read path — the fast-path number

The row must not regress the one-call snapshot or the read path, so the number is given rather than asserted in
prose (`docs/evidence/BFS-046-fastpath.txt`, `TestWatchSnapshotFastPathCostWithWatcherLive`, a 4000-file tree):

| measurement | watcher absent | watcher live | delta |
|---|---|---|---|
| `revToken()` memo hit | **58 ns** | **93 ns** | +35 ns, sub-microsecond |
| `revToken()` refreshed | **17.391 µs** | **16.662 µs** | unchanged (two file reads) |
| **one-call snapshot** | **16.102 ms** | **16.525 ms** | **+2.6%** |

A stat-per-file operation on a 4000-file tree would be milliseconds, not microseconds, so the token path is
still **two atomic loads** and the one-call snapshot is still one call — no stat-per-file walk was introduced
(R-V2, PRD §5).

---

## 6. What this row did NOT do

- **No push endpoint (BFS-036), no hot-file refresh (BFS-037), no cancel-IO (BFS-039).** This row tests them;
  where a feature does not exist the cell is written and marked pending, and §3 names it. (The BFS-039 clause
  has since FIRED: that row landed the cancel vocabulary and completed cell 8c — see §3's FULFILLED note. This
  section is about what BFS-046 itself did, and it did none of the three.)
- **No cell weakened to make it pass.** Where an assertion could not hold against the landed surface it was
  either re-derived against the surface that *is* landed (cell 9's S-9 edge needed an environment that raises
  the per-entry cap so the edge under test is S-9's alone) or recorded as pending.
- **No board rows, no `.gitreins/tasks.yaml`, no `.coding-hermes/board/**`.** This row reports.
- **No `--no-verify` on any commit**, and the Tier-1 guard (secrets, build, lint, tests) was allowed to run on
  both commits.

## 7. Findings and residuals (reported, not smoothed over)

| # | finding | disposition |
|---|---|---|
| **F-1** | **The overflow's drop count is unknowable, and the tree says so.** The real-kernel arm records `overflows_total=1` with `overflow_dropped_events == nil` and a reason; the count is never invented (§3.3 rule 2 of the watcher contract). The cell asserts the null **and** its reason, so an implementation that starts reporting a number fails here. | Reported; the cell is the enforcement. |
| **F-2** | **Cell 4's mutate-in-place red reached BFS-038's own delivered cell.** The `--mutate-in-place` mutation turns red not only this row's cell but the shipped `TestAtomicRefreshHalfFileCell` — i.e. the representation's own cell was already able to fail, and now that is a **measured** fact rather than a claim in its header. | Reported; no code change. |
| **F-3** | **The first `--leaked-lock` mutation healed itself** (pin marker inside `blobs/`, cleared by the reopen sweep), so the cell stayed green. | Fixed in the *mutation*, not the cell; recorded in §2 so the next reader does not repeat it. |
| **F-4** | **`Cache.Stage`/`Commit`/`Abort` have no production caller yet** (`grep` over `internal/` shows only `GetPinned` on the read path). The representation BFS-038 landed is consumed by BFS-037, so cells 4 and 8b currently prove the representation **as driven directly** — which is the strongest statement available before 037 lands, and no weaker than that. | Reported; cell 4's `TestBFS046Cell04…` says so in its own comment. |
| **F-5** | **The push form's refusal is correct and remains BFS-036's.** Cell 1's "channel" is the landed poll form (`X-Bunker-Op: events`) plus the watcher's state in the capability document; when 036 lands, the smoke cell grows the stream arm and the pending gate is the reminder. | Reported. |

## 8. Reproducing this document

```sh
cd <worktree>
sh docs/evidence/BFS-046-arms.sh green       # every BFS-046 cell must PASS
sh docs/evidence/BFS-046-arms.sh all         # green + seven mutations + the floor + the pending inventory
sh docs/evidence/BFS-046-arms.sh floor       # the coverage floor alone, as a number
sh docs/evidence/BFS-046-arms.sh pending     # which cells are LIVE and which are waiting
```

`green` runs `./internal/server/webdav` (cells 1–3) and `./internal/fsclient` (cells 4–9 and the pending gate).
A mutation that does not turn its cell red, an attribution cell that moves, or a restore whose sha256 does not
match aborts the run with a non-zero exit.

The regression transcript for this tree is `docs/evidence/BFS-046-suites.txt` (`gofmt-exit=0`, `vet-exit=0`,
`test-exit=0` for the full `go test ./...` on `wt/BFS-046`), and the arms transcript is
`docs/evidence/BFS-046-arms.txt`.
