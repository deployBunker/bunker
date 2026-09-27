# BFS-037 — the hot-file cache: auto-refresh on invalidation, and the four bounds that make it safe

Status: implemented, cells green, arms green (each arm's negative control verified).
Tree: `wt/BFS-037` (worktree of `/home/kara/bunker`), branch not pushed (per the brief).
Evidence: this file · `BFS-037-arms.sh` (the controls) · `BFS-037-arms.txt` (the transcript, committed).

---

## 1. THE DECISION, AND THE NUMBERS

**The decision.** A refresh never takes a slot from the foreground pool. The client grows a SECOND
budget channel (`Client.rsem`) sized to the pool share (1/8 of `Concurrency`, clamped to at least 1),
and every request marked as a refresh acquires from that channel rather than from the foreground
semaphore. The share is therefore **capacity, not politeness**: there is no configuration in which a
refresh consumes an interactive slot, and a refresh that cannot get a refresh slot HANGS UP (bounded
wait, then re-queue with backoff) instead of holding one.

**The numbers measured on this tree** (`go test ./internal/fsclient -run 'TestBFS037|TestBFS046'`, and the
probes in the same package):

| what | number | where |
|---|---|---|
| foreground p95 under a 64-path invalidation burst | **7.07 ms** (control build, no hot path: 6.95 ms; p50 6.55 vs 6.79) | `TestBFS046Cell05` |
| refreshes running concurrently at the server vs the declared share | **1 / 1** (peak inflight 1, governor 1 at pool=8) | `TestBFS037Cell02` |
| refresh budget slots vs governor | **1 == 1** at pool 8; the arithmetic at pool 25 is **3 slots, 22 foreground** | `TestBFS037Cell02` |
| stop in force (queue emptied, 60 items dropped and counted, 1 refresh abandoned) | **2.2 ms** (declared deadline 2 s) | `TestBFS046Cell06` |
| queue depth at the stop / `skip:stopped` / `abandon:stopped` | **0 / 60 / 1** | `TestBFS037Cell03`, `TestBFS046Cell06` |
| published bytes moved by an abandoned refresh | **0** (`used_bytes` 4273 → 4273, 0 stage residue) | `TestBFS046Cell06` |
| 32 concurrent readers of a QUEUED path: server GETs / leaders / joins | **1 / 1 / 31**, `singleflight_entries` 0 at rest, queue 1 → 0, retries 0 | `TestBFS046Cell07` |
| size rule AT the bound (4096 B) / one byte over (4097 B) | **refreshed, 0 size skips / refused, counted, no body fetched, nothing published** | `TestBFS046Cell09` |
| mid-body gate (the fixture's hold) proven to block | peak slot wait **397 ms** with 2 waiters on a 2-slot pool; one throttled read **202 ms** for a 200 ms throttle | `TestBFS037ProbeWaitedMS` |
| skip/abandon vocabulary reachability | **13 skip + 9 abandon reasons, 23 arms**, one reason (`abandon:replaced`) structurally unreachable and reported as such | `TestBFS037Cell08` |

**The policy numbers used** (BFS-042's, re-derived where the row asked): read 1.0 / edit 8.0; decay 0.9 per
300 s; tracker 4096 entries / 1 MiB; cold start EMPTY; size rule 8 MiB **inclusive**; queue depth 256;
pool share 3 of 25; backoff 250 ms × 2ⁿ capped at 30 s. No number was re-derived downward to make a cell
pass; the only re-derivation recorded is the pool share at the fixture's own pool (1 of 8, because
`Concurrency=8`), which is the same fraction.

---

## 2. WHAT LANDED

* `internal/fsclient/hotcache.go` — the tracker (weights, decay, bounds, persistence) and the priority
  queue (256 deep, score-ordered, with `deduped` / `displaced` / `refused` / `expired` / `promoted`
  counters). Cold start is EMPTY; a path is a refresh candidate only if the tracker holds it (D-2).
* `internal/fsclient/hotrefresh.go` — the manager: reads/edits/invalidations feed the tracker and the
  queue; a ticker drains the queue through the governor; each refresh is single-flight per path; a
  refresh fetches to a NEW immutable blob and publishes with one pointer swap (BFS-038's contract, not
  re-opened); stop is level-triggered and empties the queue in full.
* `internal/fsclient/client.go` — the refresh budget channel, `acquireFor`/`releaseFor`, and the rule
  that a refresh's own cancellation is not charged against the user's cancel census.
* `internal/fsclient/cache.go` — documentation of the `StagedRefresh` contract (nothing a reader can
  observe changes until `Commit`; the content address is verified at commit).
* The four BFS-046 cells that were written and GATED for this row are now COMPLETE against the live path
  (stampede, stop, promotion, census), and the gate that used to fail when the feature landed now asserts
  the identifiers those cells drive instead — see §5.

---

## 3. REQUIREMENT → CELL → ARM

| # | requirement | cell | negative control (arms script) |
|---|---|---|---|
| 1 | size rule, **inclusive at the boundary** | `TestBFS037Cell01`, `TestBFS046Cell09SizeRuleBoundaryIsInclusiveAndCountedByReason` | `exclusive-size` — the comparison at the bound becomes `<` → both cells RED, the no-partial-read cell stays green |
| 2 | max concurrent refreshes, bounded share | `TestBFS037Cell02` | `unbounded-pool` (the request-layer channel unsized) and `unbounded-launch-governor` (the manager's governor removed) → each RED separately |
| 3 | the queue is **stoppable in full** | `TestBFS037Cell03`, `TestBFS046Cell06` | `stop-lets-in-flight-finish` — both the checkpoint's stop check AND the stop's cancel removed (one alone is a no-op: the stop is enforced twice) → RED |
| 4 | backoff for other files and interactive traffic; **hang up, do not hold a slot** | `TestBFS037Cell07` (+`07b`, `07c`), the yield path in `checkpoint` | covered by arm 2's governor arm and by `07c`'s self-stop; the yield is measured, not asserted (`WaitedMS` probe: 397 ms peak) |
| 5 | **promotion**: a directly-called queued item leaves the queue, does not sleep | `TestBFS037Cell04`, `04b`, `TestBFS046Cell07` | `promotion-noop` — the read no longer promotes → the exactly-one-fetch cell RED (queue depth stays 1, promotions 0) |
| 6 | a file under refresh **never scares a reader** | `TestBFS037Cell05`, `TestBFS046Cell06` (v) | `stream-into-published` — the refresh writes its bytes so far into the blob a reader can reach → RED at iteration 0 ("a reader got 2048 bytes under refresh, want the old COMPLETE 8192") |
| 7 | every feature OFF leaves a correct client | `TestBFS037Cell06` | `feature-off-ignored` — the manager is built even when the policy is off → RED |
| — | no drop because the queue is full; the drop is countable | `TestBFS037Cell08` (`skip_queue_full`) + the queue census (`displaced`/`refused`/`deduped`) | `counter-cannot-move` — a refusal that is no longer counted → the census arms RED, an unrelated arm stays green |
| — | do not re-fetch what did not change; never a metadata-only identity | `TestBFS037Cell08` (`not_modified`, and the hash-based decision documented in `refresh`) | BFS-049's landed rule is the guard; the refresh reads the PATH from the channel and the CONTENT from the hash |

---

## 4. THE THREE BLIND CELLS THE ARMS CAUGHT (and what was changed, not declared)

The first `all` run had four arms holding and **three arms where the cell stayed GREEN under its own
mutation**. Each one is the class the row warns about, and each was FIXED:

1. **`unbounded-pool` held.** `TestBFS037Cell02` asserted the peak concurrency *per path*
   (`stub.PathMax`), and a per-path number can only ever be 1 — a bound that cannot bind (BFS-031's
   shape). Fixed by asserting the **request-layer** budget itself: `Client.RefreshBudget()` must equal
   the governor's width and must be strictly smaller than the foreground pool. The unsized-channel
   mutation now goes RED.
2. **`stop-lets-in-flight-finish` held.** The stop is enforced twice — the manager cancels the refresh's
   context *and* the in-flight checkpoint observes `Stopped()` — so removing one check changed nothing.
   The arm now removes BOTH (one patch, two hunks), and Cell 03 goes RED ("the stop took 2.0 s to be in
   force": the deadline, i.e. the "let in-flight finish" reading).
3. **`mutate-in-place` (BFS-046's patch) held, and the reason is a property of this implementation, not
   a hole in the cell:** the refresh stages the blob only AFTER the body is complete (`GetChecked`
   accumulates, then one `Stage`/`Write`/`Commit`), so during the transfer there are no in-flight bytes
   on disk for a reader to reach — the mutation had nothing to expose mid-body. A probe
   (`TestBFS037ProbeInPlaceArmMechanism`) confirms the mutation's mechanism is real on this tree, so the
   arm was replaced with one that models the actual hazard: **the refresh writing into the published
   blob mid-body** (`stream-into-published`). That arm goes RED immediately.

---

## 5. THE BFS-046 PENDING CELLS, COMPLETED

`TestBFS046PendingUntilLandingGate` used to FAIL the moment this feature landed, so a
"pending-until-BFS-037" cell could not be forgotten. Four cells were due and are now written against the
live path:

* **cell 5 stampede** — foreground latency distribution vs a hot-path-disabled build (numbers in §1).
* **cell 6 stop** — (i) refusals counted while stopped, (ii) queue emptied and counted, (iii) the
  in-flight refresh abandoned with no publish, (iv) a PROMOTED fetch SURVIVES the stop, (v) both
  stop/promotion orderings hand the reader a complete file.
* **cell 7 promotion** — exactly one server-side fetch for 32 concurrent readers of a queued path, 1
  leader + 31 joins, map empty at rest.
* **cell 9 census** — the vocabulary is complete as keys, every reason still has an arm in the census
  source, and three reasons are driven live here (the exhaustive 23-arm drive lives in
  `TestBFS037Cell08`).

The gate is now the assertion that keeps those cells honest: it fails if the refresh disappears from the
tree or if either vocabulary empties (which would make every census arm iterate an empty set).

---

## 6. A REAL DEFECT FOUND WHILE WRITING THE CELLS

`fetches_total` was declared, reported in `Stats()` and **incremented nowhere** — the
"counter that cannot move" this project has filed twice (BFS-032). Writing cell 7's "exactly one fetch"
assertion made it visible. It is now counted at the single point where a bytes-fetching request is known
to have finished (`hotFetch.complete`, for a refresh and for a promoted reader's fetch alike).

---

## 7. RESIDUALS, STATED PLAINLY

* The starvation measurement is a **p95 margin on 40 samples** against a disabled build on one machine,
  not a benchmark. It cannot pass by the machine being fast (the bound is derived from the control arm's
  own p95) but it is not a load test.
* `abandon:replaced` is **structurally unreachable**: a path whose fetch is already in flight is refused
  before a request starts, so the reason belongs to the SKIP census. The census cell asserts the
  assignment (skip moves, abandon does not) and says so in its output rather than fabricating a move.
* The yield path is proven by measurement (`WaitedMS`, Cell 07b/07c and the probe) rather than by an arm:
  its removal is caught by Cell 02's governor arm (the bound) and Cell 07c's self-stop, both of which
  count.
* BFS-036's push form is the channel this consumes; BFS-039's cancel semantics are untouched. The
  worktree is NOT pushed, per the brief.

---

## 8. REPRODUCE

```
cd <worktree>
go test ./internal/fsclient -run 'TestBFS037|TestBFS046' -count=1           # the cells and the census
sh docs/evidence/BFS-037-arms.sh all                                        # green + every arm
sh docs/evidence/BFS-037-arms.sh <arm>                                      # one arm, with its restore check
```

Every mutation is a patch in `docs/evidence/` applied with `git apply` and restored from a byte copy whose
sha256 is re-checked against the recorded pre-mutation hash; a restore that does not match aborts the run.
