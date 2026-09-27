# BFS-039 — cancel-IO correctness, deliberate and accidental

**Row:** BFS-039 (P1) · **Repo:** bunker · **Worktree:** `wt/BFS-039` · **Author:** Hermes
**Status:** the cancel is a cancellation. Three defects found by measurement, each with a RED on the tree that
landed BFS-046, a GREEN, and a source mutation that turns the cell red again (`docs/evidence/BFS-039-arms.sh`,
7 mutations, 16/16 declared outcomes, every restore sha256-verified byte-identical).
**Normative inputs (read, not summarised):** `docs/prd/PRD-bunker-invalidation.md` **R10** and **§2.8** ·
`docs/evidence/BFS-046-program.md` (cell 8, and the seven-mutation shape this row extends rather than
duplicating) · BFS-030 (`>` emptied a file and then failed — the same class, landed first) · BFS-038 (immutable
blob + one pointer swap) · BFS-015 (the striped per-path commit lock) · BFS-021 (append loses bytes, open).
**Code changed:** `internal/fsclient/{errno,errors,client,status,snapshot,delegate}.go` ·
`internal/fsmount/fs_linux.go` · `internal/server/webdav/handler.go` (one predicate in §6.1's precondition
table).
**Deliverables:** `internal/fsclient/bfs039_cancel_test.go` · `internal/fsmount/bfs039_cancel_test.go` · the
completed `internal/fsclient/bfs046_program_test.go` cell 8 deliberate half · `docs/evidence/BFS-039-arms.sh` +
seven `.patch` mutations · `docs/evidence/BFS-039-red.txt` · `docs/evidence/BFS-039-arms.txt` · this document.

---

## 0. Verdict in one screen

| # | what | RED on the pre-fix tree (measured) | GREEN | the mutation that turns it red again |
|---|---|---|---|---|
| 1 | a DELIBERATE cancel is reported to the caller | `ENOTCONN(107)` / `unreachable_reset` — the answer a connection that died under us gets | `EINTR(4)` / `cancelled` | `cancel-class`: the cancellation is not attributed to the caller |
| 2 | a cancelled publication keeps the caller's buffer readable | the retry was refused `EIO` — **5 runs in 12** (a closed file, see §3) | the retry sends all 45 buffered bytes; the server sees exactly 2 attempts | `closable-body`: the PUT body is the buffer file again |
| 3 | a KILLED writer leaves nothing in the cache directory | `writebuf-835548445` (and `-2148493228` on re-run), **1 MiB**, unswept, bounded by nothing | 0 named buffer files; the path is usable in **26.8 ms** | `named-buffer`: the buffer keeps its name |
| 4 | a retried write does not apply twice | the retry re-wrote identical bytes and **moved the mtime** | the wrapper holds the bytes once; the surface reports `identical_content` and the mtime does not move | `removed-noop-rule`, and `non-positional-write` (the non-idempotent control) |
| 5 | the per-path commit lock is released | — (it never was held by a dead process: nothing is persisted) | the next conditional write on the SAME path proceeds; **0** lock artifacts at rest; latency in §5 | `blind-lock-sensor`: the sensor stops looking for lock files |

**The successful path is unregressed, as a number:** a whole publication through the real surface
(Create → Write → Release → ONE conditional PUT → the server's stage+rename) measured **min 5.192 ms /
median 18.644 ms / max 29.235 ms** on a box at load average **68** (§7).

---

## 1. THE DECISION, and the three defects it came out of

The row's own thesis is that a cancel is **two events** and the second one is the dangerous one: a deliberate
cancel arrives as a FUSE interrupt, an accidental one may never arrive at all. So the deliverable is not "handle
the cancel message" — it is that **every mutation is atomic and idempotent**, so that the ABSENCE of a cancel is
never corrupting. Anything that needs to be told about the interruption to stay correct is the wrong design, and
the row forbids it explicitly.

Three defects stood between the tree and that statement. All three were found by measurement; none was inferred.

1. **A deliberate cancel was classified as a transport fault.** `classifyTransport` read `context.Canceled` as
   `unreachable_reset`/`ENOTCONN`. So a caller's own Ctrl-C was indistinguishable from a connection that died
   under it, and — because the two have different recoveries (retry vs back off) — the caller could not retry
   correctly. Fixed by asking the one question the transport table cannot: *was the CALLER's context cancelled?*
   (`classifyRequest`, on the parent context, before `do` wraps it with the operation deadline).
2. **A cancelled publication CLOSED the caller's buffer.** net/http closes a request body on every failed round
   trip (`c._send() always closes req.Body`), and the handle handed the transport its own `*os.File`. The retry
   the EINTR asks for then found a closed file and was refused with `EIO`. Fixed with a read-only view
   (`noCloseReader`) — and, separately, a cancel no longer records a permanent failure on the handle, so the
   retry is possible at all.
3. **A killed writer left an orphan in the cache directory.** `writebuf-<n>`, holding up to a whole write
   buffer, unswept by the reopen (the sweep owns `blobs/`, not this name) and bounded by no reported figure —
   BFS-031's class arriving through the write path. Fixed by making the buffer **anonymous**: the file is
   unlinked the moment it exists, so the open descriptor is its only reference and the kernel reclaims the inode
   with the last descriptor. A SIGKILL then leaves **nothing**, which is the only form of the guarantee that
   needs no cancel and no sweep.

---

## 2. THE DELIBERATE CANCEL: `EINTR` vs `EIO`, both shown

The requirement is that "cancelled" be distinguishable from "failed" in the error returned to the kernel. Three
arms, one client, one measurement each — the cancel is the only one that may be retried on the strength of the
errno, so it is the only one that must not look like the others:

| arm | what it is | before | after | retry? |
|---|---|---|---|---|
| DELIBERATE CANCEL | the caller's context is cancelled mid-request (what go-fuse does when the caller goes away) | `ENOTCONN(107)` `unreachable_reset` | **`EINTR(4)` `cancelled`** | **yes** — nothing was refused |
| FAILURE (5xx) | a real server failure | `EIO(5)` `server_error` | `EIO(5)` `server_error` (unchanged) | no — the server gave a verdict |
| TRANSPORT RESET | the far end vanished mid-request | `ENOTCONN(107)` `unreachable_connect` | `ENOTCONN(107)` `unreachable_connect` (unchanged) | no — back off |

Verbatim from the RED transcript (`docs/evidence/BFS-039-red.txt`, the tree at `e47abec`):

```
BFS039-MEASURE arm=deliberate-cancel errno=ENOTCONN(107) cause="unreachable_reset" detail=""
BFS039-MEASURE arm=server-500       errno=EIO(5)       cause="server_error"
BFS039-MEASURE arm=transport-reset  errno=ENOTCONN(107) cause="unreachable_connect"
--- FAIL: the deliberate cancel reported errno=ENOTCONN (107), want EINTR (4)
```

and after the fix (`TestBFS039DeliberateCancelIsDistinguishableFromFailure`,
`internal/fsclient/bfs039_cancel_test.go`):

```
BFS039-MEASURE arm=deliberate-cancel errno=EINTR(4) cause="cancelled" detail="the operation was cancelled by the caller (EINTR): no verdict was given, so retrying it is safe"
BFS039-MEASURE arm=server-500       errno=EIO(5)       cause="server_error"
BFS039-MEASURE arm=transport-reset  errno=ENOTCONN(107) cause="unreachable_connect"
```

`EINTR` is 4 and `EIO` is 5 on Linux; the cell also asserts `ErrnoEINTR != ErrnoEIO` directly, so a future
edit that collapses the two is a red cell rather than a review comment. The cell's assertions and the
`classifyRequest` condition are written so that **the same test text is red on the pre-fix tree and green on the
fixed one** — the RED is a measurement, not a compile error.

**Where the classification now lives.** `classifyRequest(parent, err, op, path)` asks whether the PARENT context
— the one the call chain was given, kept before `do` applies the operation deadline — is done with
`context.Canceled`. Everything else falls through to `classifyTransport` unchanged, including our own teardown
(the response-body cancel), because that request really did end without an answer and the caller cancelled
nothing. The same treatment is applied at every caller-facing read on the request path (GET's and PROPFIND's
body reads, the delegated op read, the tree walk's final `ctx.Err()`), so "a cancel is a cancel" holds across the
surface rather than at one seam. A mount whose caller is interrupt-happy now also says so:
`transport.cancels_total` in the status document (§2.7 — an event the owner cannot see is not one).

---

## 3. THE ACCIDENTAL CANCEL: the reader dies, the mount is signalled, nothing arrives

The row's RED requirement is a **SIGKILL mid-mutation**, and the honest form of it is a real child process. Two
defects came out of it, and one non-defect is reported as such.

### 3a. The killed WRITER leaves an orphan (RED → GREEN)

`internal/fsmount/bfs039_cancel_test.go` spawns a real writer (the test binary re-executed, `Create` → 1 MiB
into the unpublished buffer), waits for it to report its buffered byte count, and **SIGKILLs it**. The cell
asserts the premise first (buffered bytes > 0) so it cannot pass on a build that buffers nothing.

```
# RED (pre-fix tree; reproduced by `sh docs/evidence/BFS-039-arms.sh named-buffer`)
BFS039-MEASURE while the writer is live: 1048576 buffered bytes, 1 named buffer file(s) in the cache dir [writebuf-2148493228]
a SIGKILLed writer left [writebuf-2148493228] in the cache directory (1 file(s), 1048576 buffered bytes):
the bytes have no reader and no bound, and they survive the reopen sweep because the sweep owns blobs/ and not this name

# GREEN (fixed tree)
BFS039-MEASURE while the writer is live: 1048576 buffered bytes, 0 named buffer file(s) in the cache dir []
BFS039-MEASURE first successful write after the kill: 26.783ms (no stale lock: the operation proceeded)
```

Why this is corruption and not untidiness: the bytes are on disk with a name no reader can reach and no figure
bounds. `OpenCache`'s sweep removes unreferenced **blobs**; `measureDirLocked` sees the file only as the
catch-all `other` class (BFS-045's vocabulary, so it is not *silent* — but "visible as other" is not a bound).
One orphan per killed writer, forever. The fix removes the category rather than sweeping it: **an unnamed file
cannot be left behind**, because the kernel reclaims an inode with its last descriptor, and a SIGKILL is exactly
"the last descriptor went away".

The other two assertions of the same cell: the target keeps its previous content byte-for-byte (sha256 before
and after), and a fresh handle — which is what a restarting mount process is — lands a write on the SAME path
(§5 for the lock reading and the latency).

### 3b. The killed READER: reported, not manufactured

The client-side cache was **already** proof against a killed reader and a killed refresher: BFS-046's cells 8a
and 8b drive a real SIGKILL and both PASS on the pre-fix tree (`go test ./internal/fsclient -run
TestBFS046Cell08`). They are BFS-038's representation doing its job — the path index is the only way to a blob,
so an interrupted refresh is unreachable rather than half-visible, and the reopen sweeps the staged residue.
**No red was manufactured here**, and this row's contribution to that half is the deliberate-cancel cell the
landed test program's own gate was waiting for (§6).

### 3c. The subtle half: a cancelled publication closed the buffer (RED → GREEN)

Running the deliberate-cancel arm against the mount produced an INTERMITTENT failure — `5 runs in 12` — and the
intermittency is the finding. Diagnostic output from the pre-fix tree:

```
BFS039-DIAG retry failure=<nil> base={IfNoneMatchStar:true Source:absent} hasBase=true flushed=true size=33
the retry after a cancel was refused with errno=input/output error (EIO)
```

`flushed=true` with `failure=<nil>` means the SECOND attempt took a different branch than the code suggested:
the retry's `Seek` failed. It failed because the FIRST (cancelled) attempt had handed the buffer `*os.File` to
net/http, which closes a request body on every failed round trip — so the cancel closed the file the retry had
to read. Whether it fired depended on whether the request had started before the cancel: an intermittent,
byte-shaped defect in a write path, which is the worst kind to leave in.

The deterministic arm (`TestBFS039CancelledPublicationDoesNotCloseTheBuffer`) removes the race: a server that
holds the PUT open and signals arrival, so the cancel provably lands mid-request.

```
BFS039-MEASURE mid-request cancel: 2 PUT attempts, retry carried all 45 buffered bytes intact
```

Two attempts — the cancelled one and the retry — and the retry carried **the caller's bytes, byte for byte**.
Under the `closable-body` mutation this cell fails; under the pre-fix tree it failed 5 runs in 12.

---

## 4. THE IDEMPOTENCE ARM, and the cell that was BLIND until the arms caught it

"A retried mutation must not double-apply. A retried write must not append twice." The buffer's writes are
positional (`WriteAt`), which is what makes a retry at an offset it already sent a no-op rather than an append.
The arms script contains the control the row asks for — `non-positional-write`, `WriteAt` → `Write` — and it
**caught this row's own first cell**:

```
CONTROL non-positional-write (the mutation MUST turn this cell red) PASSED but the mutation declares it must FAIL
```

The first version of `TestBFS039RetriedWriteDoesNotApplyTwice` retried from a NEW handle, and a new handle
starts from a fresh buffer, so an appending buffer cannot double-apply there: the mutation passed and the cell
was blind. It was rewritten with the two arms that discriminate, and re-checked against the same mutation:

```
--- FAIL: TestBFS039RetriedWriteDoesNotApplyTwice
    ARM 1 — the handle's buffer holds 26 byte(s) "exactly once\nexactly once\n" after the SAME write at the
    SAME offset twice, want exactly "exactly once\n" (13 byte(s)): the mutation was applied twice
```

- **ARM 1 — the same mutation twice at the same offset on ONE handle.** The retry the kernel actually issues.
  Asserted on the buffer's own bytes (an appending buffer has already doubled there) *and* on the target.
- **ARM 2 — a misordered pair** (a late chunk then an early one). The buffer exists because a chunk stream is
  not guaranteed to be sequential (BFS-005 §5.4's stated deviation), so under an appending buffer the bytes
  land in ARRIVAL order and the server holds something that is not the file the kernel acknowledged — the
  user-visible shape of the same defect. Expected content is the kernel's own: early bytes at 0, the unwritten
  gap as NULs, late bytes at their offset.
- **ARM 3 — the retry from a new handle**, asserted against the surface's own no-op: identical bytes are already
  there, so the answer is `identical_content`, **nothing is written and the mtime does not move**. Without that
  half the cell would pass on an implementation that rewrote the same bytes a second time and bumped the served
  revision for a change that did not happen.

That last point is defect 4 of the verdict table, and it is a server-side rule: **a PUT whose bytes ARE the
current content is now the reported no-op when the precondition SUCCEEDS too**, not only when the base was stale
(§6.1's D3 arm). Before it, a retry with a correct base re-wrote identical bytes, moved the mtime and bumped
`X-Bunker-Rev` — a spurious invalidation every other client acts on. The create-only rule (`If-None-Match: *`)
is deliberately EXEMPT and keeps refusing, so BFS-015's rule-2 arm is untouched.

### The `EINTR` retry itself

`TestBFS039DeliberateCancelReturnsEINTRAndTheRetryLandsOnce` (mount level): cancel → `EINTR` → **the handle is
still publishable** → the retry lands the bytes **exactly once** (asserted as `len(got)==len(body)`, not only as
equality, so a doubled body cannot pass) → a **duplicate/late cancel on an already-published handle is a no-op
rather than an error**. The pre-fix tree set `flushed` before the PUT and recorded the cancel as a permanent
failure, so the retry returned the cancel forever: "interruptible and careful" instead of atomic and idempotent.
The `sticky-flush` mutation restores exactly that and the cell goes red.

---

## 5. THE LOCK STORY, MEASURED

The per-path commit lock is BFS-015's striped array of in-process mutexes on the server. **It is not persisted
anywhere**, so a dead process cannot hold it — the claim is not that a sweep recovers it, it is that the failure
mode does not exist. That is a structural argument, so it is reported with measurements that would expose the
alternative:

| measurement | value |
|---|---|
| lock artifacts in the client's directory at rest (lock/pid/in-use names, walked) | **0** |
| lock artifacts in the cache directory after a SIGKILLed writer | **0** |
| a conditional write on the SAME path after a cancel | **proceeds** (no refusal, no timeout) |
| cancel → retry, end to end through the real surface | **14.4 – 58.1 ms** (5 samples: 14.4, 16.4, 22.5, 45.1, 58.1) |
| the next successful operation after the cancel | **8.1 – 83.6 ms** |

The sensor is exercised rather than trusted: `TestBFS039LockSensorIsNotBlind` injects a stale
`commit.lock` and asserts the sensor SEES it, so a green reading above means "there is none" and not "this
cannot see one"; the `blind-lock-sensor` mutation (the sensor stops looking for lock names) turns that control
red.

The same shape is what BFS-030 established for the agent-side subuid lock, one surface over: a lock whose holder
died must resolve immediately rather than block. Here it resolves because there is nothing on disk to resolve.

---

## 6. BFS-046's cell 8 deliberate half — the landed gate fired, and was retired

`TestBFS046PendingUntilLandingGate` FAILED the moment this row's `EINTR` vocabulary existed, exactly as
designed, and `TestBFS046Cell08DeliberateCancelIsDistinguishableFromFailure` was completed in the same change
rather than left skipping. It drives the landed surface and carries the claims its gate named:

- a deliberate cancel is `EINTR`/`cancelled`, and a 5xx failure is `EIO`/`server_error` — distinguishable, both
  shown;
- the cancelled path **does not exist** on the server and its neighbour is byte-identical (nothing was refused,
  so nothing may exist);
- the retry lands once; **a conditional write on the SAME path then proceeds** (the lock, from the client's
  side) with both latencies reported;
- a **duplicate abandonment and a late abandonment** of a cache stage are no-ops rather than errors, and a late
  abandonment after a `Commit` does not unpublish the entry.

The gate's BFS-039 clause is replaced by its inverse (the vocabulary must still be present, or the completed
cell would silently stop exercising anything), which the `cancel-class` mutation proves is not vacuous.
`docs/evidence/BFS-046-program.md` §0/§3/§6 are updated to record the firing rather than left claiming a pendency
that no longer exists.

---

## 7. THE SUCCESSFUL PATH: unregressed, as a number

Five whole publications through the real surface, in the idempotence cell:

```
BFS039-MEASURE a successful publication through the real surface: min=5.192ms median=18.644ms max=29.235ms
```

(host load average **68** at the time; the spread is the host, not the code.)

What this row adds to that path, in full: one **unlink** per write handle (once per open file, replacing
`discard`'s later unlink), one **interface hop per body read** (`noCloseReader`), one **header comparison per
PUT** (the widened no-op rule), and — on the FAILURE path only — one `errors.Is` in `classifyRequest`.
`internal/fsclient/cache.go` has **no diff at all** in this change: the cache read and publish paths the
benchmarks exercise are byte-identical to the pre-fix tree.

Micro-benchmarks, control tree (`e47abec`) vs fixed tree, `-benchtime=300x -count=3`
(`docs/evidence/BFS-039-bench-control.txt`, `-bench-fixed.txt`):

| bench | control | fixed |
|---|---|---|
| `BenchmarkGetAndPin8MiB` | 20.33 / 8.28 / 11.39 ms | 7.82 / 6.14 / 6.54 ms |
| `BenchmarkWriteIntentOn` | 55.6 / 59.0 / 54.0 ns | 44.3 / 52.5 / 55.8 ns |

Reported honestly: the control tree's OWN spread on `GetAndPin8MiB` is **2.4×** (20.33 vs 8.28 ms) on a box at
load average 68, so these figures cannot resolve a difference of the size this change could produce, and the
code under them is unchanged. **The numbers are reported as noise-dominated, not as an improvement or a
regression.** `BenchmarkStagedRefreshPublish8MiB` FAILS on BOTH trees, identically
(`Stage: fsclient: no room for a staged refresh: 256 entries at the entry bound 256`) — a pre-existing bench
defect, unrelated to this row, named here rather than left to look like a regression.

---

## 8. THE NEGATIVE CONTROLS

`sh docs/evidence/BFS-039-arms.sh all` — full transcript in `docs/evidence/BFS-039-arms.txt`. Seven mutations,
each asserted to turn ITS cell red AND to leave an attribution cell unmoved, each restored from a byte copy and
re-checked by sha256 (a restore that does not land aborts the run):

| mode | mutation | cell that must go red | attribution cell that must stay green | result |
|---|---|---|---|---|
| `cancel-class` | the caller's cancellation is not attributed to the caller | client deliberate-cancel | client killed-reader (8a) | RED as declared |
| `sticky-flush` | a cancelled publication recorded as a verdict | mount cancel → retry lands once | mount retried-write | RED as declared |
| `named-buffer` | the buffer keeps its name | killed writer leaves no residue | lock sensor control | RED as declared |
| `closable-body` | the PUT body is the buffer file again | mid-request cancel keeps the buffer | lock sensor control | RED as declared |
| `non-positional-write` | `WriteAt` → `Write` (**the non-idempotent control**) | retried write does not apply twice | lock sensor control | RED as declared |
| `removed-noop-rule` | the identical-content no-op is gone | retried write does not apply twice | mid-request cancel | RED as declared |
| `blind-lock-sensor` | the lock sensor stops looking for lock names | lock sensor is not blind | killed-writer cell | RED as declared |

**16/16 declared outcomes matched, 0 mismatches, 7/7 byte-identical restores verified.**
The `non-positional-write` mutation is the control the row demanded specifically — the one proving the cell can
catch a **non-idempotent** mutation — and it is the one that found this row's own blind cell (§4).

Building the controls also produced a lesson worth recording: generating a mutation patch by applying it with
`perl` and restoring with `git checkout -- <file>` **silently discards uncommitted work in that file** — it ate
the very arm the patch run was supposed to leave alone, because the working copy was ahead of HEAD. The
generator now restores from a byte copy and verifies the hash, and the arms script does the same.

---

## 9. WHAT THIS ROW DID NOT DO, and the residuals it names

- **No cancel MESSAGE as a correctness dependency.** Nothing in the design needs to be told about an
  interruption: the cache publishes by one pointer swap (BFS-038), the write buffer is anonymous, the write
  handle keeps a cancelled publication publishable, and a retried whole-file PUT is the surface's reported
  no-op. A cancel that never arrives changes nothing; a cancel that arrives late is a no-op.
- **No push endpoint (BFS-036), no hot-file cache (BFS-037).** Not touched.
- **BFS-030's and BFS-038's landed guarantees are untouched**: the write-shape refusal is unchanged, the
  atomic swap is unchanged, and the one server-side predicate added is the identical-content no-op, which
  cannot lose an update (there is nothing to write) and is exempt for the create-only rule so BFS-015's
  rule-2 arm still refuses.
- **BFS-021 (append loses bytes) is ADJACENT and IN SCOPE only as the buffer's positional write**: "a retried
  write must not append twice" is satisfied for every write shape this surface accepts, and the append shape
  itself is still not reachable through the mount, so a retried append cannot double-apply today. BFS-021 owns
  making `>>` work or refusing it by name; this row does not change that surface, and the idempotence arms here
  are the regression net it will land under.
- **RESIDUAL, named rather than fixed:** the INVALIDATOR's own watch stream (`internal/fsclient/invalidate.go`
  line ~1166 and `framelimit.go` line ~162) still classifies a cancelled context as a transport fault, because
  that loop's cancellation is the MOUNT shutting down rather than a caller cancelling an operation. A sibling
  worker held `invalidate.go` during this row, and the edit was kept out of it deliberately. The same treatment
  applies there when someone owns that file; nobody's read or write is affected today.
- **RESIDUAL:** the write buffer is no longer visible on disk for debugging (that IS the fix), while
  `write_buffer_bytes` still reports the same figure from the mount's own bookkeeping — a reader of that figure
  sees no change, an operator wanting the bytes on disk no longer can. Stated because the trade is real.
- **RESIDUAL, pre-existing, not mine:** `BenchmarkStagedRefreshPublish8MiB` fails on both trees (§7), and
  `measureDirLocked`'s `other` class is what would have shown the killed writer's orphan had anyone looked.
- **Board and `.gitreins/tasks.yaml` untouched; no `--no-verify`;** the Tier-1 guard ran on both commits.

---

## 10. Re-running the evidence

```sh
# the cells, on the tree as committed
go test ./internal/fsclient -run 'TestBFS039|TestBFS046Cell08|TestBFS046PendingUntilLandingGate' -count=1 -v
go test ./internal/fsmount  -run 'TestBFS039' -count=1 -v

# the controls: the tree green, then every mutation and its attribution cell
sh docs/evidence/BFS-039-arms.sh green
sh docs/evidence/BFS-039-arms.sh all        # 7 mutations, 16 declared outcomes, restores sha256-verified
```

`docs/evidence/BFS-039-red.txt` is the pre-fix capture used for the RED numbers above; each of its entries is
reproduced by the matching arms mode, so the RED is re-runnable rather than a transcript of a vanished tree.
