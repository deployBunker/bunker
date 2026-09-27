# BFS-035 — the server-side watcher: acceptance evidence

Row: **BFS-035 (P0)** — the server-side watcher (`internal/server/webdav/watch.go`).
Design authority: `docs/prd/SPEC-watcher-capability.md` (§4, §5, §7, §8, §11).
Recovery context: the previous worker was killed mid-refactor by a state-database write
collision. Turn 1 of this pass landed the recovered WIP compiling
(`cf745ac`); turn 2 landed the acceptance work and the three defects the cells found
(`c229377`). Nothing was pushed; the work is on `wt/BFS-035`.

Raw artifacts, all in this directory:

| file | what it is |
|---|---|
| `BFS-035-red.txt` | the RED: every acceptance cell run against the recovered WIP **before** any fix |
| `BFS-035-mutation-A-red.txt` | the required negative control: the rescan neutered ⇒ the overflow arm goes red |
| `BFS-035-negative-control-A.patch` | that mutation, verbatim, and nothing else |
| `BFS-035-negative-controls.patch` | the two additional controls (quiet drain; header carrier removed) |
| `BFS-035-mutation-red.txt` | their RED |
| `BFS-035-evidence-run.txt` | the GREEN run with the citable numbers logged by the cells themselves |

Every number below was produced by the run recorded in those files on this host.

---

## 1. The RED (written first, run against the recovered WIP)

`go test -run TestWatch -short -count=1 -v ./internal/server/webdav/` → **13 failing cells**
(4 test functions, 9 sub-tests). They are not one failure wearing twelve hats; they are
three distinct defects, and the accounting is what identified them.

**8 cells — §3.1's third carrier was missing.** Every probe-matrix case whose reason was
correct in the envelope and in the document failed on the header:

```
watch_test.go:501: X-Bunker-Capability carried reason="", want "watch_unsupported_platform"
                   (header "watch;scope=target;mode=poll")
watch_test.go:541: ... want "watch_target_netbacked"   (header "watch;scope=target;mode=poll")
watch_test.go:579: ... want "watch_limit_exhausted"
watch_test.go:621: ... want "watch_limit_exhausted"
watch_test.go:643: ... want "watch_limit_exhausted"
watch_test.go:665: ... want "watch_install_failed"
watch_test.go:704: ... want "watch_partial_coverage"
watch_test.go:784: ... want "watch_lost"
```

The reason reached two of the three carriers, so a client that reads the header (the one
carrier a non-JSON client has) could not tell W-3 from W-1 — which is the whole reason the
matrix has seven names.

**3 cells — the served revision never moved for an out-of-band edit.**

```
watch_test.go:984:  the revision did not move after a full rescan that saw .../src/out-of-band.txt
watch_test.go:1211: timed out waiting for the served revision to move for the out-of-band edit
watch_test.go:820:  timed out waiting for the event from the re-installed backend to move the revision
```

**2 more** — the stall detector's interval was never repaired (`unvouched_total did not move
across the stall (1 -> 1)`) and `W-6 transient` timed out, which is the re-install path.

### The three defects, and what fixed them

1. **The header carrier** (`verdict.go`, `ops.go`). `capabilityHeaderValue` rendered
   `name;scope;phase;mode` and nothing else. It now takes the reason and appends
   `reason=<value>` as the fourth part (§3.1's own example), **only when a probed reason
   exists** — so every other refusal's header stays byte-identical (A-2) and "no reason was
   probed" can never be spelled as an empty one. `failure` carries the field too, so the
   envelope, the XML refusals and the header cannot disagree.

2. **D1/D2 were never wired** (`watch.go`). `noteWatchedChanges` advanced the *watcher's* own
   counter and never the tree's, so `revToken()`'s watcher half (`git:<head>@<n>`) was pinned
   at `@0` forever: an out-of-band change moved the watcher's books and nothing a client
   could see — precisely the R2 blind spot R-V1 exists to close. The count is now published
   to the tree (two atomic loads on a read path, no stat: R-V2).

3. **A re-install left the watcher reading dead channels** (`watch.go`). The event loop did
   `onBackendClosed(); return` and the drain captured its channel once, so after a
   *successful* re-install the watcher reported `watching` and read nothing — the silent
   shape O-1/O-2 exist to prevent, arriving through the repair path. The loop now continues
   on the new backend's channels (only a *failed* re-install is an absence), and
   `startDrain()` runs once per backend **generation**.

A fourth, found by the same cells: the rescan journaled its `overflow` line **before**
aligning the tree, so the line's `rev` disagreed with the revision the tree went on to serve
(the cell caught it: `line carries rev ...@0 but the tree now serves ...@2`). The alignment
now happens inside the ledger's critical section before the push — the ordering the poll
path already has.

## 2. The GREEN

```
go build ./...                                   ok
go vet ./internal/server/webdav/                 clean
gofmt -l internal/server/webdav/                 (empty)
go test ./internal/server/webdav/ -count=1       ok  25.7s
go test -race -run TestWatch -short -count=1     ok  21.8s
go test -run TestWatch -count=1 (kernel arm on)  ok  22.0s
```

The pre-commit guard ran on both commits and passed all four lanes with `test_mode: full`,
`test_targets: all (full mode)`, `guards: 4 (0 failed, 0 skipped)`, `overall: PASS`
(`.gitreins/logs/guard-20260927T13*.log`). No `--no-verify` was used at any point.

## 3. The probe matrix — one NAMED reason per case

Each case asserts `reason` + `scope` + `mode` + `blocks_push` **on all three carriers**, and
that the detail names the observed evidence rather than the category. The full per-case
assertions are in `TestWatchProbeMatrixNamesOneReasonPerCase`.

| case | injected fact | asserted verdict |
|---|---|---|
| control | local `ext4` root | `state=watching`, `reason` null, `coverage.complete`, `headroom=512`, refusal `scope=build` (the push wire form is BFS-036) |
| W-1 | `backend=none` | `watch_unsupported_platform`, `backend:"none"`, `blocks_push=true`, detail names `backend=none` |
| W-3 | root is `fuse.sshfs` | `watch_target_netbacked`, `mount_type=fuse.sshfs`, `mount_point` = the longest-prefix match, `network_backed=true`, **and no backend was created** (nothing was probed about the kernel) |
| W-2 (pre-flight) | ceiling 3 vs 6 directories + headroom | `watch_limit_exhausted`, `limits.watching` reports `limit_name`, `configured=3`, `desired≥4`, `headroom=512` |
| W-2 (from the add) | add returns `ENOSPC` at #n | `watch_limit_exhausted`, `errno=ENOSPC`, `watches_held_by_this_process=n`, **every added watch removed and the backend closed** (the partial set is torn down) |
| W-2 (instance) | `newBackend` returns `ENOSPC` | `watch_limit_exhausted` naming `fs.inotify.max_user_instances` |
| W-5 | add returns `ENOENT` | `watch_install_failed`, detail carries the errno **and the path**, incomplete set torn down |
| W-4 | add returns `EACCES` | `watch_partial_coverage`, `missing_count=1`, `missing[]` names the directory, `complete=false`, **the set is kept** |
| W-4 (bound) | 4100 directories, all refused | `missing[]` **empty**, `missing_count=4100` — never a truncated list presented as complete; control: a 3-item list is verbatim |
| W-6 (lost) | events channel closes, re-install fails | `watch_lost`, detail names the instant and the failed re-install, 2 backend attempts |
| W-6 (transient) | events channel closes, re-install succeeds | **not** an absence: back to `watching`, `unvouched_total` moved across the gap, and an event published on the **new** backend moves the revision |
| W-7 | root is `overlay` | `boundary_split=true`, **no** refusal reason, watcher still established, a `degradations[]` entry with `blocks_push=false` and the upperdir consequence spelled out |
| A-1 control | a deployment with no watcher | `state=absent`, `reason` **null** and no `reason=` header part: a build that ran no probe reports no reason rather than a default |

## 4. The overflow guarantee (A-4) and its control

`TestWatchOverflowForcesCountedFullRescanAndIsNeverQuiet`. The loop is parked while the
interval is observed, so the unvouched state is asserted **before** it is repaired instead of
racing it:

* before the release: `state=overflow`, `vouched=false`, `overflows_total=1`,
  `unvouched_total=1`, `unvouched_reason=kernel_queue_overflow`, `rescans_total=0`,
  `overflow_dropped_events=null` **with** `overflow_dropped_reason` set;
* the revision has **not** moved (R-V1 aligns it with a *vouched* change only);
* after the release: `rescans_total=1`, the ledger holds an `overflow` line with an **empty
  `paths[]`** (never a diff of the part that fit), and the revision moves because the rescan
  really re-observed the change nobody reported on the channel.

### The negative controls (sha256-verified)

| control | mutation | result |
|---|---|---|
| **A — the rescan is neutered** (the required one) | `requestRescan` drops the request | `watch_test.go:967: timed out after 5s waiting for the forced full rescan` → **FAIL**, at exactly the rescan assertion | 
| B — the quiet implementation | the drain receives and discards the classification | `watch_test.go:926: timed out waiting for the overflow to be counted` → **FAIL** |
| C — one carrier removed (§3.1 / A-3) | the header part is passed as `""` | `X-Bunker-Capability carried reason="", want "watch_target_netbacked"` → **FAIL** |

`watch.go` sha256 `aa310ab0b581a1839934a57776ce55858c5ab7f7d0b9018392ec9dbeaa31e816` under control A,
restored to `04aa62945dc5f3b6cdbe4a8eafbe1475d7add22aa1d2062fa86f93fb6711b822`, which is the HEAD
blob's own sha256 (`git show HEAD:internal/server/webdav/watch.go | sha256sum`), working tree
`git status` clean, and the same cell green again. The three mutations are in
`BFS-035-negative-controls.patch`; control A alone is in `BFS-035-negative-control-A.patch`.

## 5. O-1, the drain — and the trap it exists for

* `TestWatchOverflowNegativeControlDisablesTheDrain` (A-5): the **same** overflow with the
  drain disabled is not observed at all — counters flat, state still `watching`. This is what
  makes the compliant cell able to fail, and it is the shape the row demands.
* `TestWatchOverflowRealKernelReproducer` — §5.4's trap through the **real** library:

```
BFS-035 kernel reproducer: max_queued_events=16384, 16384 files created,
overflows_total=1, unvouched_total=1, rescans_total=0
  drain arm     PASS (1.41s)  the notice was received
  no-drain arm  PASS (16.25s) SILENCE: no overflow counted, state still `watching`
```

One measurement corrected the spec's reading while building this arm, and BFS-046 should
inherit it: **the overflow record is queued behind the events it replaced**, so a *fully*
parked consumer never even reaches the blocked `sendError` — the notice is read only as the
queue drains. The trap therefore bites a **slow** consumer, exactly as §5.4's chain says
("slow consumer → library reader blocks → queue fills"), and the first version of this arm
(timeout while parked) was wrong for that reason. The no-drain arm then reproduces the
silence for real: the reader reaches the notice, `sendError` has nobody to hand it to, and
the watcher reports a healthy quiet channel indefinitely.

## 6. O-2, liveness — the heartbeat, and a stalled reader

`TestWatchLivenessIsTheHeartbeatNeverSilence`:

* a healthy **idle** tree (no events at all): `heartbeats_total` and `event_loop_ticks`
  advance, `last_event_age_ms` stays **null**, `stalled=false`, and the document's
  `liveness_source` says the absence of events is not the source. Liveness comes from the
  watcher's own clock, which is the only admissible reading.
* a genuinely parked loop: after two consecutive beats `stalled=true`, the state is **never**
  `watching`, `vouched=false`, `unvouched_reason=reader_stalled` (not the kernel's fault),
  and releasing the loop repairs it through the rescan the stall itself requested
  (`unvouched_total` and `rescans_total` both move).

## 7. The served revision (A-10 / D1–D6)

`TestWatchOutOfBandEditMovesServedRevision`:

* a write that is not ours (the file is written directly, the event pushed as the kernel
  would) moves `X-Bunker-Rev` **and** the served bytes of the next `GET` (D4's cache was
  forgotten);
* the capability document declares `extensions.rev.kind = "git+watch"` on the wire (R-V3:
  a composite token must be declared under its own kind), while a build with no watcher
  keeps `git`;
* the change is journaled in the tree's own ledger, so a *poller* sees it too (D5);
* **control**: with the loop parked (an unvouched interval) the same edit does **not** move
  the revision (R-V4).

## 8. The snapshot fast-path cost, as a NUMBER (R-V2)

`TestWatchSnapshotFastPathCostWithWatcherLive`, 4000-file git work tree, minimum over 5
batches so a single scheduling hiccup cannot move the figures; the cell logs them:

```
BFS-035 fast path (watcher absent): files=4000 token_memo_hit=46ns   token_refresh=21.265µs one_call_snapshot=19.046012ms
BFS-035 fast path (watcher live):   files=4000 token_memo_hit=175ns  token_refresh=21.778µs one_call_snapshot=18.725047ms
```

Read as: the one-call snapshot — the reason this project exists — is **unchanged** with the
watcher live (18.73 ms vs 19.05 ms on this box, i.e. inside run-to-run noise; a second run
measured 20.00 ms vs 17.25 ms). The token stays two loads and a format. A stat-per-file
implementation would show the whole-tree read here, which is the 19 ms line itself: the
guarantee is that the token path **never** looks like it, at any tree size
(`TestRevisionTokenCostIsIndependentOfTreeSize`, BFS-048, still green).

**Residual, measured and reported rather than smoothed over:** the token's memo-hit arm
costs 175 ns with a watcher live against 46 ns without. The composite is built with
`fmt.Sprintf("git:%s@%d")`; the delta is the formatter, not the watcher (an atomic load is
~1 ns) and it does not grow with the tree. A `strconv.FormatUint` build would recover it.
Not changed here: it is not a regression of the snapshot, and the row's rule (R-V2) is about
not paying a stat per read, which holds. Named so the next pass can decide.

## 9. What this pass did NOT do

* **No push, no merge, no board/`.gitreins` edit.** The row is reported, not closed:
  `BFS-049`, `BFS-051`, `BFS-054` and the hot-file work were left alone.
* **The push wire form is BFS-036's.** The `watch` op still refuses with
  `capability_unavailable`, and with a watcher established the refusal is now
  `scope=build` and says so, rather than denying the watcher it has.
* **BFS-046 owns the test program.** These cells are the row's own acceptance; the pinned
  reproducer is handed over in §5 with the correction above.
* A sibling cell's timing fragility was **fixed**, not skipped:
  `TestSameSurfaceOverHTTP1AndHTTP2` compared two bodies' `duration_ms` and decided content
  by the clock — attributed against a pristine `HEAD` control (3/3 pass there, 3/3 FAIL with
  the larger watch document, 3/3 pass after blanking a per-request measurement the spec never
  requires to match). That is in commit `cf745ac` with the attribution.
