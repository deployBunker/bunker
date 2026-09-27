# BFS-049 — two observers in one server disagreed about which changes exist: ONE identity, and the numbers

**Row:** BFS-049 (P1) · **Author:** Hermes (bunker worker, `wt/BFS-049`) · **Date:** 2026-09-27
**Base:** `4879343` — the driver's base for this row. Both production blobs are sha256-pinned inside
`BFS-049-arms.sh`, and every run below checks the blob it is about to measure against those hashes, so
nothing here is measured against moving `main`.
**Product code changed:** `internal/server/webdav/tree.go`, `internal/server/webdav/events.go`.
**Not changed:** the client (`internal/fsclient/**`), the spec, the board, `.gitreins/tasks.yaml`, and the
events path's rules (never-quiet, resume rule, frame bound).
**Depends on:** SPEC-watcher-capability §2.4 blind spot 2, §7.1 D4, §7.2 R-V2/R-V6 · BFS-004 §3 E-6 and
§6.1 step 5 · BFS-009 F1 · BFS-026 (the ledger's ctime) · BFS-048 (the fast-path cost baseline) ·
BFS-062 / BFS-063 (the events bounds and the resume rule — untouched, re-run green)

---

## 0. Verdict in one screen

| # | what the row asked | result | where |
|---|---|---|---|
| 1 | **the DECISION** | **ONE shared identity definition.** The hash cache keys on the identity the event ledger observes — one type, one producer, no second copy of the tuple. The ledger was the right observer and **nothing on the events path was weakened**; the cache adopted the ledger's definition, not the reverse. On top of that, the class this row closes is **counted in production** rather than left as a historical anecdote. | §1 |
| 2 | **a RED that constructs the divergence, both verdicts printed** | **MEASURED**, on the filed blob: for **one** edit (same size, mtime restored to the nanosecond) observer D4 answers `moved=false` and observer D5 answers `invalidate [src/util.go] moved=true`. The two lines are printed by the cell. | §2 |
| 3 | **the GREEN** | the same cell passes; **7 arms green** on the fixed tree, plus the whole package. | §3 |
| 4 | **a negative control that turns the cell red** | **TWO, both sha256-verified.** The filed tree (both production blobs swapped for the base commit's) → the RED cell and the isolating arm FAIL, and the attribution arm PASSES. A one-line neuter of the shared projection on the fixed tree → the same two FAIL and the same attribution arm PASSES. | §4 |
| 5 | **blast radius, as a NUMBER** | **0 added syscalls**: 520 vs 520 stat-family syscalls over 500 cache lookups (strace, two test binaries differing only in `tree.go`). **+16 bytes** per cache entry (32 → 48). The one-call snapshot on BFS-048's 10 106-entry fixture: **113.4 / 120.8 ms**, inside the **7.4–18.9 ms noise floor** the same protocol produced on *code-identical* paths — and the no-hash snapshot never calls `hashFile` at all. | §5 |
| 6 | **QA-BUNKER-36's two arms** | **Both PASS at HEAD, and both also PASS at the commit QA named** (a control worktree at `86c8a95`, 10 repeats + `-race`). Arm 1's exact failure text is reproduced from **one named mechanism** — a ctime that exists but does not move — which is a platform property, not a code state. Reported, not closed. | §6 |

---

## 1. The decision

### 1.1 What the two observers are, and which one is authoritative

| | D4 — the hash cache (`tree.go`) | D5 — the event ledger (`events.go`) |
|---|---|---|
| question it answers | *"must the content be re-sent?"* | *"must a client drop what it holds?"* |
| consumer | GET/HEAD's `ETag` + `X-Bunker-Hash` + the `304` decision (`handler.go:455`), PROPFIND's `b:hash`/`D:getetag` (`props.go:355`), the snapshot's `include_hash` form (`ops.go:276`), the **early** PUT precondition (`handler.go:556`) | the `X-Bunker-Op: events` poll (`handleEvents`) |
| keyed on, before this row | `(size, mtime)` — a private pair | `(size, mtime, ctime)` + kind |
| keyed on, after | **the same `identity` D5 observes**, via `sameContentKey` | unchanged |

**The authoritative observer for the class in question is the ledger, and it is the one that did not
change.** That is the whole decision: the fix direction is the cache adopting the stricter definition,
never the stricter observer being blinded to match the weaker one (which the row forbids, and which
would have traded a reported divergence for a silent one).

The two consumers are not interchangeable, which is why "which one is right" has a per-class answer:

- **"Did this path change?"** (change notification) — the **ledger** is authoritative. It observes the
  tree and holds an identity per path, kind included: a file replaced by a collection with byte-identical
  metadata is still a change it must report (`sameIdentity`).
- **"Must these bytes be re-sent?"** (content identity) — the **cache** is authoritative for that
  question, and it now answers it with the ledger's definition. A `(size, mtime)` key could answer
  `304` for bytes that had moved; `(size, mtime, ctime)` cannot, for any edit the filesystem reports.
- **A byte-level change that no metadata observer can see** (a rewriter that restores size, **mtime and
  ctime**) — **neither** metadata observer is authoritative, and neither claims to be: that class is the
  write path's content-hash re-validation (BFS-004 §6.1 step 5, `freshEntry`), which reads the bytes and
  is untouched by this row. `events.go` already states this in its own header, and it stays true.

### 1.2 Why one identity, and not "add ctime to the cache"

The row's own reading is the right one, and it is what the code now does. `hashEntry` does not hold a
`size`/`mtime` pair plus a bolted-on ctime: it holds **the `identity` the ledger observes**, produced by
the **same** `identityOf(fi)`, and the cache decides "unchanged" with a predicate defined **on that one
type**:

```go
type hashEntry struct {
	id   identity   // the observation the event ledger makes — one definition, two consumers
	hash string
}
...
if ok && e.id.sameContentKey(id) { return e.hash, nil }
```

and, beside it in `events.go`:

```go
func (id identity) sameIdentity(other identity) bool    { return id == other }               // the ledger
func (id identity) sameContentKey(other identity) bool { return id.Size == other.Size &&
	id.Mtime == other.Mtime && id.Ctime == other.Ctime }                                     // the cache
func (id identity) sameMetadataKey(other identity) bool { return id.Size == other.Size &&
	id.Mtime == other.Mtime }                                                                // the OLD key
```

Three projections of **one** identity, each named and each carrying its own justification, instead of a
second copy of the tuple at the second call site. `sameMetadataKey` decides nothing; it exists to *name*
the class this row closes (next section), so a future observer cannot re-introduce the divergence
silently — a copied constant in two places is exactly how it would come back, and there is no second
constant now.

**The recurrence sweep was run, not assumed.** `grep -rn 'ModTime()\.UnixNano()' --include='*.go'
internal/` (excluding tests) returns exactly **two** sites in the whole server after this row:
`events.go:140` — `identityOf`, the one definition both observers now use — and `ops.go:269`,
`MtimeUnixMS`, which is a **wire presentation field** of a snapshot entry (unix milliseconds, for a
client's display) and decides nothing about whether anything changed. `watch.go` — the watcher that
owns D4's `forget(abs)` rule — compares no metadata at all; it acts on the filesystem's own events. So
the server has one metadata-keyed identity definition, not two, and not three.

The spec's own words for this are R-V6: *"The derived artifacts must agree about what a change is."* The
spec's planned mechanism was the watcher closing the gap with `forget(abs)` (D4's row in §7.1); that
still holds and is untouched (`watch.go:930,968`). This row makes the two observers agree **whether or
not** a watcher is established, which is the state R-V6's second sentence demands a build without a
watcher not to get wrong.

### 1.3 The closed class is REPORTED, not silent

Sharing the identity makes *today's* two observers agree. No test of today's pair can catch a *future*
observer that brings its own tuple back: it would simply be a third definition. So the class is counted
where it is classified — in `changed()`, a path whose observed identity moved while the fields the old
`(size, mtime)` key compared are **equal**:

```
IdentityDivergenceCounters() -> (metadata-key-blind moves, last such path)
```

That is deliberately *not* a "stale bytes served" metric, and it must not be read as one: a `chmod`
lands in this class and changes no bytes at all. What it measures is exactly what it says — the paths
whose move the pre-BFS-049 key **could not see**, i.e. the class in which D4 and D5 would have
disagreed. A zero count is the honest "no observation of this process produced one". It is process-wide
for the same reason `frameOverBoundCounters` is, and it is exercised in both directions (`§3`, and the
neutered control shows it keeps counting while the cache is blind again).

---

## 2. The RED on the filed tree — ONE edit, BOTH verdicts, printed

`sh docs/evidence/BFS-049-arms.sh unfixed` → `docs/evidence/BFS-049-red.txt`. The script swaps in
`4879343:internal/server/webdav/tree.go` (sha256 `3c4e9561…`) and
`4879343:internal/server/webdav/events.go` (sha256 `61e4bdf1…`), checks each against the pinned hash,
sets aside the one test file that asserts this row's ADDED surface (the filed tree has no
`IdentityDivergenceCounters` to compile against), and runs the cells.

The cell prints, for **one** stimulus:

```
BFS-049: ONE edit on the tree under test — same size (29 bytes), mtime restored to the nanosecond.
  the bytes on disk:    before=sha256:c42441ab60cd91a4af4005ce39e237d1f9b5b40b9719a5b41d3dc587db846ec1
                        after =sha256:5b309a8e2d3ff22ea4eafc66e8cd450c24fc50888d5b4324bece4f346ddef5cf   (the served bytes did move)
  observer D4 (hash cache -> whether content is RE-SENT, a GET's ETag/304): before=sha256:c42441ab…ec1 after=sha256:c42441ab…ec1 moved=false
  observer D5 (event ledger -> whether a client is told to DROP):            event="invalidate" paths=[src/util.go] moved=true
```

and then refuses the state it has just constructed:

```
DIVERGENCE: the hash cache still answers sha256:c42441ab…ec1 for a path whose bytes are now
sha256:5b309a8e…f5cf, while the same server reported ["src/util.go"] to a polling client — a GET can
answer 304 with the ETag of bytes that are no longer on disk
--- FAIL: TestBFS049TheTwoObserversCannotDisagreeOnASameSizeMtimePreservedEdit
```

**The trap is asserted, not assumed**: the cell re-stats after the edit and fails if the size or the
mtime differs (`the trap did not hold`), so the two `moved=` verdicts above are about an edit that really
did preserve both.

The second RED arm isolates the *mechanism* rather than the edit shape — a **mode change**, which moves
ctime and nothing else (size and mtime asserted unchanged inside the stimulus), with a planted sentinel
in the cache entry so a HIT and a MISS are directly observable instead of inferred:

```
observer D4 (hash cache): cache HIT — it answered the sentinel it was planted with, i.e. it judged the path current
observer D5 (event ledger): event="invalidate" paths=[src/util.go] moved=true
--- FAIL: TestBFS049AMetadataOnlyMoveIsNotACacheHit
```

Attribution arms in the same unfixed run — the ones that must stay green, so a red arm above cannot be
"the cache was switched off":

```
--- PASS: TestBFS049AnUnchangedReadIsStillACacheHit
--- PASS: TestBFS049TheSharperIdentityCostsNoExtraSyscall
```

---

## 3. The GREEN

`sh docs/evidence/BFS-049-arms.sh green` → `docs/evidence/BFS-049-green.txt`. All seven arms pass on the
fixed tree (`go test ./internal/server/webdav -run TestBFS049 -count=1 -v`: `ok … 7.970s`), with the
verdict pair now agreeing:

```
observer D4 (hash cache …): before=sha256:c42441ab…ec1 after=sha256:5b309a8e…f5cf moved=true
observer D5 (event ledger …): event="invalidate" paths=[src/util.go] moved=true
```

The two arms QA-BUNKER-36 named, at HEAD (`4879343` base, `58df47c` + this row's tree):

```
--- PASS: TestEventsOpSeesAnEditThatPreservesSizeAndMtime (0.00s)
--- PASS: TestEventsOpStaleCursorGetsTheRetainedEvents (0.00s)
```

The neighbours this row must not have weakened, re-run green with the package (`go test ./... -count=1`
→ `ok internal/server/webdav 72.261s`; `docs/evidence/BFS-049-suites.txt`) and under `-race`
(`ok … 152.144s`, `docs/evidence/BFS-049-race.txt`):

- the never-quiet / resume rule: `TestEventsOpFirstPollDeclaresThePriorRangeLost`,
  `TestEventsOpSnapshotSeedMakesTheFirstPollHonest`, `TestEventsOpStaleCursorGetsTheRetainedEvents`,
  `TestEventsOpCursorAheadOfTheLedgerIsDeclaredLost`
- the frame bound (BFS-062): `TestBFS062…` — untouched, same package run
- the write path's content re-validation (§6.1 step 5): `TestExpectedHashIsRevalidatedInsideTheCommit`,
  `TestCreateOnlyRuleIsRevalidatedInsideTheCommit`, `TestCommitSectionIsExclusive`,
  `TestConcurrentConditionalWritesCannotBothLand`
- the cost baseline of BFS-048 (`rev_fastpath_cost_test.go`) — run as part of the arms, §5

### 3.2 Whole-repo suites

`go build ./...` rc=0 · `go vet ./...` rc=0 · `gofmt -l` clean · whole-repo `go test ./... -count=1`
rc=0 (every package `ok`), and the package under `-race` `ok … 152.144s`:
`docs/evidence/BFS-049-suites.txt`, `docs/evidence/BFS-049-race.txt`. Both transcripts print the sha256 of
every blob they measured and the host loadavg at the time. Tier-1 guard on the two code commits:
**PASS** (secrets + go_build + go_lint + go_tests, `test_mode: full`).

---

## 4. The negative controls

Both are in the arms script and both restore byte-identically, with the pre- and post-swap hashes
printed in the transcript.

**4.1 The filed tree** (§2, `unfixed`): the defect as filed → RED cell FAIL, isolating arm FAIL,
attribution arms PASS. Restores verified:
`tree.go sha256=7f835740…` / `events.go sha256=8ca1c752…` / the set-aside test file moved back.

**4.2 The shared identity neutered** (`sh docs/evidence/BFS-049-arms.sh neutered` →
`BFS-049-control-neutered.txt`). One substitution, on text this row added — the shared projection drops
ctime again, exactly as filed, while the ledger, the never-quiet rule and the frame bound stay in place.
The script audits the mutation in both directions (mutant text present, pre-image gone), checks
`gofmt`, and **builds** it before running anything, so a mutation that does not compile can never be
mistaken for a passing arm:

```
mutation audit (neutered): mutant text present, pre-image gone, gofmt clean, package builds
--- FAIL: TestBFS049TheTwoObserversCannotDisagreeOnASameSizeMtimePreservedEdit
--- FAIL: TestBFS049AMetadataOnlyMoveIsNotACacheHit
--- PASS: TestBFS049AnUnchangedReadIsStillACacheHit                      <- attribution: the cache still hits when nothing moves
--- PASS: TestBFS049TheFormerDivergenceClassIsReportedNotSilent          <- attribution: the report is not fix-dependent
--- PASS: TestBFS049TheReportNamesTheClassAndNotEveryChange
```

The two attribution arms are the point of the control: the cache is *not* disabled in the mutant (it
still hits on an unchanged read) and the ledger's classification is *not* changed (the report still
counts), so the two failures are attributable to the shared identity being bypassed and nothing else.

---

## 5. The blast radius, as a number

`sh docs/evidence/BFS-049-arms.sh cost` → `docs/evidence/BFS-049-cost.txt`. This host was at **loadavg
61–74** throughout (`host load at measurement time` is printed in the transcript by the script), which
decides how the numbers below may be read.

### 5.1 The number that is exact: **zero added syscalls**

The arms script builds two test binaries that differ **only** in `tree.go`, and runs the same cell (500
`hashFile` cache hits, nothing else) under `strace -f -c` over the stat family:

| tree | newfstatat | fstat | total stat-family calls | lookups | calls per lookup |
|---|---|---|---|---|---|
| fixed (`7f835740…`) | 511 | 9 | **520** | 500 | 1.02 |
| filed (`3c4e9561…`) | 511 | 9 | **520** | 500 | 1.02 |

**Δ = 0.** A key that had to `stat` each path a second time for ctime would have added **+500** here —
this is the measurement R-V2 asks for ("it must not cost a stat"), and it is the reason this row is
about the *identity* rather than about a new syscall. ctime is a field of the same `syscall.Stat_t` the
existing `os.Stat` already returns, so the added work per lookup is reading two more fields and
comparing one more `int64`.

### 5.2 Memory: +16 bytes per cache entry

`unsafe.Sizeof(hashEntry{})`: **32 → 48 bytes**. The cache is capped at `hashCacheLimit = 4096` entries,
so the worst case is **+64 KiB per tree**, and a tree with a few hundred live entries pays a few KiB.

### 5.3 Per-lookup cost

`hashFile` cache-hit minimum per call (min over 5 × 200 in-process calls, the estimator BFS-048 uses):
fixed **2.71 µs**, filed **2.13–2.49 µs**. Both are dominated by the one `stat` inside them (the same
one, §5.1); the difference is at or below this protocol's resolution on a box at loadavg 74, which is
why §5.1 and not this line is the evidence for the cost.

### 5.4 The one-call snapshot — the reason this project exists

The script runs the cost arm **three times per tree, interleaved** (fixed→filed, filed→fixed, …) on the
**same fixture BFS-048 used** (1 000 and 10 000 files ⇒ 1 106 / 10 106 entries), and reports the minimum
per metric per tree:

| metric (min of 3 interleaved rounds) | fixed tree | filed tree | delta |
|---|---|---|---|
| snapshot, no hashes, 1 106 entries | 8.17 ms | 9.59 ms | +1.42 ms |
| snapshot, no hashes, 10 106 entries | **120.83 ms** | **113.40 ms** | −7.43 ms |
| paired no-hash, 10 101 entries *(code-identical in both trees)* | 88.04 ms | 106.93 ms | **+18.89 ms** |
| paired include_hash, 6 021 entries | 359.00 ms | 365.51 ms | +6.51 ms |

**Read the last two rows together, because that is the honest reading.** The `paired no-hash` row is
*byte-identical code* in both trees — it never calls `hashFile` — and it moved by **18.89 ms** between
them. That is this protocol's noise floor at 10 000 files on this host, and it is larger than every delta
attributable to this row (including the `include_hash` row, which is the only snapshot form that calls
`hashFile` per path). **So the timings do not resolve this change at all, and no delta above should be
quoted as its cost.** What the timing rows do show is the required thing and only that: the fast path is
**unregressed** — same code path, same band, and BFS-048's published figure for the same fixture
(98.9–167.4 ms at 10 106 entries) sits inside the same spread.

Two structural facts complete it, and both are checkable in the source rather than measured:

1. the whole-tree snapshot **without** `include_hash` never calls `hashFile`, so it cannot be affected;
2. the `include_hash` form calls `hashFile` once per path, and §5.1 measures that call at **one `stat`** in
   both trees.

---

## 6. QA-BUNKER-36 — both arms accounted for

The finding (P2, `internal/server/webdav`): *"TestEventsOpSeesAnEditThatPreservesSizeAndMtime FAIL
(events_test.go:283 … not reported: []) and TestEventsOpStaleCursorGetsTheRetainedEvents FAIL
(events_test.go:352 … answered with silence) … Relates to BFS-049 (hash cache lacks ctime) but is its own
failing-test defect at HEAD 86c8a95."* **Not closed and not edited by this row — reported only.**

**6.1 State at HEAD:** both tests **PASS** (§3), and on the FULL package run, and under `-race` (§3.2).

**6.2 State at the commit QA named.** A detached control worktree at `86c8a95`
(`git worktree add --detach /tmp/bunker-qa86 86c8a95`; nothing in this row's worktree or in `main` was
touched):

| run at `86c8a95` | result |
|---|---|
| the two named tests, `-v` | PASS |
| the two named tests, `-count=10` ×3 | PASS (all three runs) |
| the two named tests, `-race -count=3` | PASS |
| the **whole package**, `-count=1` (the battery's own invocation) | `ok … 4.866s` — every `TestEventsOp*` PASS |

So the failure does **not** reproduce on a named tree at the commit the row cites. One thing that is easy
to get wrong here and is worth stating: the commit message of `86c8a95` says it closes BFS-063, but its
`events.go` does **not** contain the resume rule (`resumePoint`, `Cursor == 0 && l.base > 1`) — that
landed later, in `d4046ab`. The board row was closed ahead of the code.

**6.3 Arm 1's mechanism, named and reproduced.** The one cause that produces arm 1's *exact* text at its
*exact* line is a **ctime that exists but does not move** between the ledger's baseline and the edit —
what a filesystem with coarse ctime resolution gives you (and precisely the hazard my own isolating arm
guards against by retrying the move and then skipping with a stated reason). On the `86c8a95` control
worktree, with `ctimeUnixNano()` left intact (so the arm's own platform guard does not skip) and only the
observed identity's ctime member zeroed:

```
events_test.go:283: a same-size, mtime-preserved edit was not reported: []
--- FAIL: TestEventsOpSeesAnEditThatPreservesSizeAndMtime
--- PASS: TestEventsOpStaleCursorGetsTheRetainedEvents      <- same mutant: arm 2 is NOT this mechanism
```

`86c8a95`'s ledger already carried ctime (added by `58bcee5`, BFS-026, an ancestor of `86c8a95`), so arm 1
cannot be a "the code at 86c8a95 lacks ctime" defect either. And a platform with **no** ctime at all
cannot produce this failure: the arm skips with a reason in that case.

**6.4 Arm 2.** Its failure does not reproduce at `86c8a95` (unmutated, 10×, under `-race`, or in the full
package run) nor in the arm-1 mutant, so it is **not** the same mechanism and I do not claim to have
identified its cause. What is verified: the rule it pins (the seeded-vs-unseeded resume decision) is the
one BFS-063 rewrote after `86c8a95`, it is intact at HEAD, and **this row neither weakened it nor relies
on it**. Its accounting therefore belongs to BFS-063's row, whose fix is in the tree; the arm as filed is
green wherever I can measure it.

**6.5 What the QA lane's finding got right, and the one part of its premise that does not hold.** The
defect class it names — *a metadata-keyed observer answering "current" for a change another observer
reports* — is real, is this row's subject, and is exactly what §2 constructs on the cache side. Its
premise that the **events poll** missed the edit *because* the hash cache keys on `(size, mtime)` does
not hold: the ledger and the cache are independent observers, the ledger never consults the cache, and at
the commit cited the ledger had ctime already. Two independent paths did reach one class; they did not
reach the same mechanism, and the file that failed them was not reproducible. Suggested follow-up for
that lane (not filed here): record the tree's sha and the full `go test` output with the battery, so a
failing arm is anchored to a named artifact — the working directory's HEAD was `86c8a95`, whose tests
pass.

---

## 7. What this row does not do (residuals, named rather than implied)

1. **It does not close a byte-level change that restores size, mtime AND ctime.** No metadata observer
   can see that; the write path's content-hash re-validation (§6.1 step 5) is the cover, unchanged.
2. **A ctime-only move now costs one rehash.** A `chmod`/`chown` with unchanged bytes makes the next read
   of that path re-hash it: one extra read of one file, once. That is the price of the cache being able
   to see what the ledger sees, and it is bounded by the number of such moves, not by the size of the tree.
3. **On a platform without ctime** (`filetime_other.go`) both observers are `(size, mtime)`: they remain
   **equal**, and equally blind to that one class. The divergence is gone; a shared, stated weakness
   remains, and the write path still covers it. The `0` in the identity is the honest spelling of that.
4. **`identityDivergenceCounters` is exported at the Go level for the host**, in the same shape as
   `frameOverBoundCounters` (BFS-062); like it, it is not yet wired to an HTTP surface. It is the
   backstop against a *future* second tuple, and it is pinned by a cell — it is not a claim that
   divergence is impossible.
5. **The watcher's own D4 rule (`forget(abs)` on a watched change) is untouched.** This row makes the two
   observers agree with or without a watcher; it does not replace the watcher, and it does not make the
   cache an observer of anything the filesystem does not report.

## 8. Reproduce every claim

```sh
cd <worktree>                                   # wt/BFS-049
sh docs/evidence/BFS-049-arms.sh green           # the seven arms on the fixed tree
sh docs/evidence/BFS-049-arms.sh unfixed         # the RED on the filed blobs (sha256-checked, restored)
sh docs/evidence/BFS-049-arms.sh neutered        # the shared identity neutered, one substitution (audited)
sh docs/evidence/BFS-049-arms.sh cost            # strace counts + the interleaved timing rounds
sh docs/evidence/BFS-049-arms.sh race            # the package under -race
```

Every mode prints the blob hash it is about to measure, and every restore re-checks the sha256 of the
file it wrote back; a restore that does not match aborts the run instead of leaving a mutated tree.
