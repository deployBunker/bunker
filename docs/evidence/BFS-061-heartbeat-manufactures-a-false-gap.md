# BFS-061 — every HEARTBEAT manufactured a FALSE GAP (H-2)

* **Row:** BFS-061 (P1, defect in shipped code) — found while writing `docs/prd/SPEC-push-channel.md` (hole H-2).
* **Worktree:** `/home/kara/worktrees/bunker-BFS-061`, branch `wt/BFS-061`, base `064b39f`, fix commit `788a588`.
* **Files changed:** `internal/fsclient/invalidate.go` (the fix), `internal/fsclient/invalidate_bfs061_test.go` (the three arms), `internal/fsclient/status_census_test.go` (a fixture the fix corrects — §7).
* **Nothing else:** `internal/server/webdav/events.go` (BFS-062's sibling) untouched; no push endpoint (BFS-036); no hot-file cache (BFS-037); no new counter.

---

## 1. THE DECISION — **(a)**, and the numbers that justify it

> **(a) the cursor is advanced for EVERY frame that carries a `seq`, including heartbeats.**

| | before (tree as filed) | after |
|---|---|---|
| 5 heartbeats then **one** change | `resyncs_from_gap 0→1`, `resyncs_total 0→1`, **1 full-view drop**, `seq=7`, reason `"sequence gap at seq=7: the missing range is never re-requested"` | `resyncs_from_gap 0`, `resyncs_total 0`, **0 full-view drops**, `seq=7`, and the change applied as a precise path drop |
| a heartbeat carrying a jump over a **real** gap (cursor 1, heartbeat at 3) | `resyncs_from_gap 0`, `resyncs_total 0`, `seq=1` — the interval `[2]` is never re-requested | `resyncs_from_gap 1`, `resyncs_total 1`, 1 full-view drop, `seq=3`, reason names `seq=3` |

The two rows are the same mechanism seen from both sides: the heartbeat's `seq` was **consumed and never
recorded**, so the client's cursor was one short per keep-alive. It made a change look like a gap (a
whole-tree resync per heartbeat) *and* it made a real gap invisible when a heartbeat stood over it.

### Why (a) and not (b)

**(b) taken client-side IS the shipped defect.** "Heartbeats do not carry a consumable `seq`" can only
mean one of two things, and neither is a client-side fix:

1. *The client keeps discarding a seq the ledger has already spent.* That is the code as filed — the
   ledger counts the line (one counter per tree, §2 W-2/R-5), the client does not. Nothing changes; the
   defect stands. The row's own sentence says exactly this: *"the client consumes a seq it never
   records"*.
2. *The server stops numbering heartbeats.* That is a **server** change: `events.go`, which this row's
   boundary forbids ("stay client-side" — BFS-062's sibling is in that file), and which §3.4 rules on
   directly: heartbeats are lines in a monotone stream (**BFS-004 §10.5** numbers one), and R-5 pins one
   counter per tree shared by the poll and the stream. A server that emits them outside the counter
   (`seq: 0`) is the case §3.4(1) is written about.

### Argued against §3.3 (R-3 / R-4) and §3.2, which is what decides it

The cursor's whole job in §3.3 is to be a **truthful, monotone statement of the highest `seq` this client
has observed**: §3.2's four resume cases are indexed on it, and R-4's argument ("a client is correct to
discard any line whose `seq` is ≤ the cursor it presented") is only sound while that is true — §10.1
item 1 states the dependency outright: *"the client's duplicate rule (`seq <= cursor` ⇒ ignore) is the
whole defence, and it is sound only because R-1 makes the cursor advance on every line."*

Under (b) the client **holds lines above its own declared cursor**, so the number it presents no longer
means what the server reads it as:

* the ledger keeps numbering heartbeats and **retains** them (§3.4(2), H-9), so `held.base` climbs over
  keep-alives alone. A client that is *in fact current* is then classified as **§3.2 row 3, "behind the
  retained journal"**, and answered the ~resync it was supposed to stop paying. The cost is not removed,
  it is **relocated**: from a per-heartbeat event the client can count to a journal-occupancy event the
  client cannot see, bound or predict. That is the same mechanism-defeat the row exists to remove, minus
  the ability to measure it.
* an answer consisting only of retained heartbeats is a tail the client cannot recognise as covering the
  interval it asked about — R-3's exact prohibition ("a tail whose first `seq` the client has no way to
  recognise as a gap").

**(a) is also what the spec already decided** — §3.1 R-1, and §10's disposition line for H-2 ("Fixed by
R-1: the cursor advances on every line, regardless of `event`"). This row implements the normative clause
rather than a preference.

### The failure mode accepted under (a)

**A line lost on the wire is a REAL gap and resyncs, heartbeat or not.** The client cannot distinguish a
lost keep-alive from a lost change from the line sequence alone (that is §3.4(2)'s ambiguity, and its only
safe reading is a resync), and a resync is always safe. So a manifest that loses a heartbeat pays one
resync; a server that numbers heartbeats outside the ledger — the §3.4(1) case — now surfaces as a resync
rather than as a cursor that quietly understates what the client has seen. Both are the conservative
direction, and both are *countable*: R-1 was chosen over (b) precisely because a cost the client can
count is a cost the owner can fix, and (b)'s cost cannot even be observed from the client.

---

## 2. THE MECHANISM — what the shipped code did

`apply()` handled a heartbeat first and returned before the cursor bookkeeping:

* filed blob (`064b39f:internal/fsclient/invalidate.go`, sha256
  `14617c65…6db65`): `if ev.Event == EventHeartbeat { …; i.mu.Unlock(); return }` at `:823–834`, and the
  cursor block (`if ev.Seq > 0`, the duplicate branch, `ev.Seq > i.seq+1 && i.seq != 0` ⇒ `i.gaps++`,
  then `i.seq = ev.Seq`) at `:843–854`. The spec's appendix A.12 cites the same two ranges as
  `invalidate.go:217–253`.
* the **read loop is not involved and was not touched** — it keys liveness on the *receipt of a line*
  and never writes `seq` (its own comment says so; BFS-060's rule), which is why the fix is entirely
  inside `apply()`/one new helper.

Consequence, exactly as the row states: heartbeat at `seq 43` leaves the cursor at 42; the next
`invalidate` at `seq 44` satisfies `ev.Seq > i.seq+1`, so the gap check fires and the client resyncs —
every time, forever. The safety property never broke (a resync is safe), which is why this was P1.

---

## 3. THE FIX

* `advanceCursorLocked(seq int64) (dup, gap bool)` (`invalidate.go:836`) — the seq bookkeeping in ONE
  place, so it cannot drift between the two paths that need it: a `seq ≤ 0` is not a cursor claim at all
  (neither advanced over nor read as a gap), `seq ≤ i.seq` is a duplicate, `seq > i.seq+1 && i.seq != 0`
  is a real gap **and** still advances the cursor (the missing range is never re-requested, BFS-005 §4.1),
  and anything else advances it.
* the heartbeat branch calls it (`:878`) **after** recording the heartbeat as liveness and its counters,
  and resyncs when it reports a gap;
* the main path calls the same helper (`:892`) and keeps its duplicate early-return.

### The half that is NOT "move the early return"

Advancing the cursor over a heartbeat's jump and staying quiet would vouch for a range nobody observed —
the silent-staleness P0 this P1 must not be traded for. So the **gap check runs on a heartbeat too**, and
that is what arm C measures. `neutered` (§5) is the control that shows arm C is not decoration: mutating
the heartbeat's argument back to `0` fails A *and* C.

### Nothing else moved

* BFS-063's non-zero-cursor guard is preserved verbatim (`i.seq != 0` and the `seq ≤ 0` skip).
* BFS-060's liveness is untouched: `lastLine`, `heartbeats`, `events` and the evidence label are recorded
  **before** any gap decision, so a heartbeat that also resyncs still counts as the line it was; the read
  loop still never writes `seq`. BFS-060's own arms are green in the full-package run.
* No counter was added: the arms assert on the **landed** BFS-045 counters (`resyncs_from_gap` →
  `InvalidationState.Gaps`, `resyncs_total` → `.Resyncs`; documented at
  `docs/evidence/BFS-045-observability.md:75`, classified `kindMoves` at
  `internal/fsclient/status_census_test.go:916–917`) plus `liveness.heartbeats_total`, which is what
  proves the heartbeats were actually received.

---

## 4. THE ARMS (`internal/fsclient/invalidate_bfs061_test.go`)

| arm | what it claims | on the filed tree | on the fixed tree |
|---|---|---|---|
| **A** `TestBFS061HeartbeatsDoNotManufactureAGap` | 5 heartbeats then ONE change ⇒ no gap, no resync, change applied precisely, cursor == the heartbeat's seq | **RED** (`resyncs_from_gap 0→1`, 1 full-view drop) | **GREEN** |
| **B** `TestBFS061ARealGapIsStillDetected` | a real gap (seq 2 skipped) still resyncs, still drops the whole view, the post-gap line is never applied, the reason names `seq=3`, the cursor still advances past it | GREEN (the invariant) | **GREEN** |
| **C** `TestBFS061AHeartbeatCannotMaskARealGap` | the same real gap with a HEARTBEAT as the only surviving line is still detected | **RED** (`seq=1`, no gap, nothing will re-request `[2]`) | **GREEN** |

The arms drive the REAL path: an `httptest` stub serving the `X-Bunker-Op: watch` NDJSON stream (the
BFS-060 fixture, reused rather than duplicated), a real `Client`, a real `Invalidator.Run`, and the real
status record. Arm A waits for the heartbeats to be *received* before sending the change, so the
interleaving under test is the scripted one and not a race. All three are green under `-race`
(`BFS-061-arms-race-clean.txt`).

---

## 5. THE TWO NEGATIVE CONTROLS (each claim, one mutation, sha256-verified restore)

Both mutations are two-line substitutions on text **this row added** (so they survive a rebase of
somebody else's hunks), and the script greps **both directions** afterwards — mutant text present,
pre-image text gone — and builds the package before running the arm, so a mutation that did not compile
would be reported as such rather than silently measuring the fixed tree.

**Control 1 — `neutered`** (claim: *a heartbeat no longer manufactures a gap*). The heartbeat calls
`advanceCursorLocked(0)` again: **one argument**, the defect restored, with the gap check left armed for
every other line. Mutant sha256 `36968a3c…`.
* **arm A FAILS again** — `resyncs_from_gap 0→1`, 1 full-view drop. The cell catches the defect returning.
* arm B still PASSES (attribution: it does not depend on the heartbeat's `seq`).
* arm C fails too — same mutation, same mechanism (C probes the heartbeat's own `seq`), recorded as such
  rather than excused.

**Control 2 — `loosegap`** (claim: *a real gap is still a real gap* — the one that matters). The gap
classification is removed (`i.gaps++` and `gap = true` deleted) while the heartbeat still advances the
cursor: i.e. **the cheap wrong fix**, which would trade this P1 for a silent-staleness P0. Mutant sha256
`6968efac…`.
* **arm B FAILS**: `resyncs_from_gap 0`, `resyncs_total 0`, `paths_dropped_total 2` — the client applied
  the post-gap change as a precise drop as though `[2]` had been covered.
* **arm C FAILS**: same, with a heartbeat as the surviving line.
* **arm A PASSES** in this mutant — which is the entire point of the control: the mutant *looks* like a
  fix, and the two claims separate it from one.

Both modes end with `restore: internal/fsclient/invalidate.go sha256=69ff511f…c5d7c (verified
byte-identical against the pre-mutation hash)`, and the modes were re-run for these transcripts against
the final script text.

---

## 6. An earlier census arm had encoded the bug as an assumption (corrected fixture, not expectation)

`TestEveryFigureInTheStatusRecordMovesOrIsExplained` (BFS-045's census) failed after the fix, and the
reason is worth stating rather than smoothing over: its scripted poll answered a **tail starting at
`seq 10` to a client holding `seq 1`**, with no `overflow` marker. That answer is contract-violating
(R-2/R-3: the poll the repo landed emits the marker — BFS-063), and it only passed before **because of
this defect**: the stream's own heartbeat at `seq 1` went unrecorded, so the client's cursor was 0 and the
non-zero-cursor guard swallowed the hole. With the cursor recorded, the hole is a gap and the client
resyncs — the behaviour arm C exists to require.

So the fixture's **seqs** were corrected (`10,11,12` → `2,3,4`, contiguous with the cursor the client
holds, which is exactly what a real server answers for `since_seq: 1`), with the reasoning in the code.
The arm's **expectation** was not touched (`paths_dropped_total ≥ 3`, the deliberate gap at 99, the
overflow at 100). And the correction is not fix-dependent: on the **filed** tree, with the filed
`invalidate.go`, the corrected census arm **PASSES** (`BFS-061-red.txt`, last block, exit 0).

---

## 7. WHAT THIS ROW DID NOT DO — named, not implied

1. **The `-race` lane is red for a reason that predates this row and does not involve it.**
   `TestRevPollFiresOnlyWhenTheServedRevisionMoves` (BFS-048's file, landed `9ec3bb9`, untouched here)
   has a **data race in test code**: the stub handler goroutine reads test-local variables
   (`revision_poll_contract_test.go:113`, `:68`) while the test goroutine writes them (`:128`, `:132`,
   `:135`). Proven pre-existing by the pristine-clone control — `git clone --shared` of the shared repo
   checked out at the filed commit with the **filed** `invalidate.go` (`sha256 14617c65…`), same test,
   same race, **twice**, with none of this row's change present (`BFS-061-race-base-control.txt`,
   `BFS-061-race-base-control.sh`). It is another row's file and its own fix; it is reported here, not
   reached into. This row's own arms are race-clean (`BFS-061-arms-race-clean.txt`).
2. **The server half of H-6/H-9 is untouched.** Whether heartbeats consume a journal slot, and what that
   costs a reconnect's retained window, is `events.go`'s (§3.4(2), §3.4(3)) — that file is BFS-062's
   sibling and out of bounds here. This row only makes the **client** consistent with R-1/R-3, and the
   only requirement it places on the server is the one §3.4(1) already states: a heartbeat that is not
   numbered in the ledger is not a cursor observation, and this client treats `seq ≤ 0` as exactly that
   (no advance, no gap) while its gap detection on change lines is unaffected.
3. **The remaining holes of §10 stay as filed** — H-1 (clean EOF, BFS-060/062's client half), H-3 (idle
   rule — landed by BFS-060), H-4 (frame bounded by count), H-5 (`heartbeat_ms` vs the QUIC idle
   timeout), H-7 (capacity refusal), H-8 (jitterless reconnect), H-10 (closed by BFS-063).
4. **No new counter, no new field, no board or `.gitreins/tasks.yaml` edit**, and nothing pushed.
5. **Nothing ran against a live deployment.** All evidence is local: the stub stream, the real client,
   the real record.

### Residual worth an owner's eye (reported, not filed)

Arm C now resyncs on a heartbeat that jumps. That is correct (§3.3 R-3), but it means a **server that
numbers heartbeats in a counter other than the tree ledger's** — the shape §3.4(1) forbids — will make
every mount resync on each keep-alive instead of being *silently* understating. That is the safe
direction, and it is what §3.4(1) makes a contract violation rather than a client concern.

---

## 8. HOW TO RE-RUN EVERYTHING

```
cd /home/kara/worktrees/bunker-BFS-061
sh docs/evidence/BFS-061-arms.sh green    064b39f   # arms pass
sh docs/evidence/BFS-061-arms.sh unfixed  064b39f   # the RED, on the sha256-pinned filed blob
sh docs/evidence/BFS-061-arms.sh neutered 064b39f   # control 1: the resync returns
sh docs/evidence/BFS-061-arms.sh loosegap 064b39f   # control 2: a real gap is no longer a gap
sh docs/evidence/BFS-061-arms.sh race     064b39f   # package under -race (see §7.1)
sh docs/evidence/BFS-061-race-base-control.sh       # the -race attribution control
```

Every mode restores `internal/fsclient/invalidate.go` from a byte copy and re-checks its sha256, or aborts.

## 9. THE SUITES (`BFS-061-suites.txt`)

* `gofmt -l internal/ cmd/` — no output.
* `go build ./...` — rc 0.
* `go vet ./...` — rc 0.
* `go test ./... -count=1` — **29/29 packages ok** (incl. `fsclient` 6.8 s, `fsmount`, `cli`, `server`,
  `server/webdav`, `probes/webdav-battery`); one complete run, in the transcript, at the commit that
  carries this bundle.
* The **MUST-NOT-REGRESS** arms, named rather than implied: `TestBFS060*` (the channel is still reported
  dead when it is dead, and a quiet tree is not) and `TestBFS063*` (the resume declaration) all PASS in
  the same transcript — §3 asserted this from the code; the transcript is the measurement.
* `go test ./internal/fsclient -count=1 -race -run TestBFS061` — ok.

## 10. PROVENANCE

| artifact | sha256 |
|---|---|
| `internal/fsclient/invalidate.go` — the filed blob (`064b39f:…`) the RED is measured on | `14617c6518e69c3335a29ae1ac7cc6aff1c41e67bf24f25aac72e8339056db65` |
| `internal/fsclient/invalidate.go` — fixed | `69ff511fdc043176224426598601f3c3cc378bde261a4bc7c9812e6b1e7c5d7c` |
| mutant `neutered` | `36968a3c5dfb6be7bf70a1a83ecf77eab29d3340643e34ddd4235333e3c95724` |
| mutant `loosegap` | `6968eface967c3238b8a62f07f1b2d945f36e74f2f066b25f7cdc27d35f8cdb9` |
| `internal/fsclient/invalidate_bfs061_test.go` | `ee1394ed1a406a01bf5249814122652281a9397cbe7f0242a266acb2485a6028` |

Fix commit `788a588` on `wt/BFS-061`; Tier-1 guard PASS on that commit (secrets, go_build, go_lint,
go_tests). Not pushed.

## 11. TRANSCRIPTS IN THIS BUNDLE

| file | what it is |
|---|---|
| `BFS-061-arms.sh` | the arms script: five modes, the sha256-pinned filed blob, both mutations, both restores |
| `BFS-061-red.txt` | `unfixed` — the RED with the counters, plus the corrected census arm passing on the filed tree |
| `BFS-061-green.txt` | `green` — all three arms pass on the fixed tree |
| `BFS-061-control-neutered.txt` | control 1 — arm A fails again, arm B still passes, arm C fails with the same mechanism |
| `BFS-061-control-loosegap.txt` | control 2 — arms B and C fail, arm A passes: the cheap fix that looks like a fix |
| `BFS-061-race.txt` | `race` — the package under `-race` (fails on the pre-existing rev-poll race, §7.1) |
| `BFS-061-race-base-control.sh` / `.txt` | the attribution: the same race on a pristine base clone with the filed blob |
| `BFS-061-arms-race-clean.txt` | this row's arms under `-race` |
| `BFS-061-suites.txt` | gofmt / build / vet / `go test ./...` |
