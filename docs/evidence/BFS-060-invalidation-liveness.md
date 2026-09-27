# BFS-060 — the invalidation channel: DEAD is reported, and a quiet tree is not

**Row:** BFS-060 (P0) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-27
**Depends on:** `docs/prd/SPEC-push-channel.md` §3 (resume and liveness), §8.1 (the idle row), §8.2 (R-6),
§8.3 (the fallback set), §11.1 (the record); `docs/prd/SPEC-watcher-capability.md` §5 (the overflow
guarantee, normative), §5.3, §5.4 (O-2); BFS-005 §4.4; BFS-040 §4.6 (`watch_lost` ends the stream).
**Product code changed:** `internal/fsclient/invalidate.go` (+307/−44), `internal/fsclient/errors.go` (+18).
**Tests added:** `internal/fsclient/invalidate_bfs060_test.go` (595 lines, the three behaviour arms),
`internal/fsclient/invalidate_bfs060_rule_test.go` (118 lines, the GREEN-only reporting arm).
**Evidence:** `docs/evidence/BFS-060-{green,red,control-eof,control-idle,race}.txt`,
`docs/evidence/BFS-060-arms.sh`, `docs/evidence/BFS-060-negative-control-{eof,idle}.patch`.

---

## 0. Verdict in one screen

| # | what the row asked | result | where |
|---|---|---|---|
| 1 | **RED first, on the UNFIXED tree, reproducing BOTH failures** | **REPRODUCED — both, from the FINAL test files.** The filed blobs of `5baef51` are swapped in for the two product files and sha256-checked (`d54bbde1…` / `e47d9700…`), so the RED is a named artifact rather than a memory. (a) the clean EOF: `Run returned (err=<nil>) and no reconnect was attempted, so nothing can invalidate again for the life of the mount. The record left standing was mode="push" mechanism="watch" channel_available=true`. (b) the stall: quiet and stalled produce the **same** claim, printed side by side. | §3 |
| 2 | **the GREEN: DEAD is reported dead, QUIET is reported healthy-and-quiet** | **DELIVERED.** Quiet: one claim for the whole hold (`push`/`watch`/`available`, empty reason). Stalled: three — `push/watch/available` → **`poll/events/`NOT available** + the stall named → the poll answers and the channel is live again by the declared mechanism. On the filed tree those two sequences are the same single claim. | §4 |
| 3 | **a negative control for EACH, sha256-verified restore** | **BOTH PROVEN.** Control EOF: the EOF arm goes RED, the stall arm still PASSES. Control IDLE: the stall arm goes RED, the EOF arm still PASSES. Each restore is re-hashed and printed. The arms are also mutual controls: neither fix can be carrying the other's arm. | §5 |
| 4 | **which mechanism is AUTHORITATIVE for liveness, stated** | **The heartbeat's own receipt is the SIGNAL; the client's idle deadline is the DECISION.** Neither alone can do the job: an unmeasured receipt cannot be observed to be *missing*, and a deadline that nothing resets is not a liveness signal — it is a guess. Both are implemented, on the relation the server itself declares. | §1 |
| 5 | **no unbounded wait anywhere; every path bounded and named** | Six failure paths, each with a named cause, a named recovery and a bound (§6). The read loop gained a `ctx.Done` arm, so the landed "cancel does not interrupt a stalled read" shape is gone as well. | §6 |
| 6 | **must not change what a working channel does** | **No server change at all** (one op's client half only). The poll path is untouched apart from `Run`'s exit marking. The `watch` refusal path, the `stale_tree` re-bind path and the 501/400 degradation ladder behave as before — `TestInvalidatorStaysHonestWhenThePollFormIsRefused` and `TestInvalidatorDeliversThePollFormOfTheChannel` re-run green, unchanged. | §7 |
| 7 | **must not "fix" BFS-061/062/063** | Untouched, each with the shape of its own row preserved: the heartbeat's cursor bookkeeping, the 8 MiB token cap and its transport classification, the poll's guarded gap check. My liveness signal is deliberately independent of the cursor so that BFS-061 can land beside it. | §7.4 |
| 8 | **must not make the resync path less safe** | No resync rule changed (`overflow`, gap, tree change, zero-path invalidate, over-cap list all still `Resync`). The new `stream_ended` path RECONNECTS with the cursor and never re-requests a missing range; a resync is still always correct, only expensive. | §7.3 |

`hilo graph impact internal/fsclient/invalidate.go` → `No dependents found for 'internal/fsclient/invalidate.go'.`
(the row's own quality gate, run before the commit).

---

## 1. The decision: what is authoritative for liveness, and why the other one is not needed *instead*

**Answer: both, and they are not alternatives — the receipt is the evidence, the deadline is the
measurement. Liveness is authoritative at the RECEIPT OF A LINE.**

1. **The heartbeat's own receipt is the SIGNAL.** BFS-040 §5.4 O-2 makes liveness the *watcher's*
   obligation: "liveness must be produced by the watcher's own loop (a heartbeat it emits on a period
   it controls), and it must be distinguishable from the absence of changes". §5.1 is the same
   statement as a rule: a quiet answer means *"I observed, and nothing moved"*. Nothing this client can
   do invents liveness; it can only be told.
2. **The client's idle deadline is the DECISION.** A receipt that is never measured cannot be observed
   to be *missing* — which is exactly the filed defect: `DefaultIdleTimeout` existed, nothing read it,
   and `scanner.Scan()` has no timer, so the channel could stop being a channel with no observable
   difference from a healthy idle tree. The deadline is what turns evidence-absence into a named
   verdict (`stream_stalled` → declared poll fallback).
3. **Why the heartbeat alone is not enough:** without a deadline, a heartbeat is unfalsifiable. A
   client that only *records* heartbeats can say "the last line was 4 minutes ago" and nothing follows
   from it — the mount still reports `available=true` and still never invalidates. That is the state
   the row was filed against.
4. **Why the deadline alone would be wrong:** a deadline that fires on "no line" is only valid if the
   source is *obliged* to write one. It is — `extensions.watch.heartbeat_ms`, ≤30 s (BFS-004 §3 E-6
   decl. 1) — so the client's rule is the spec's own relation, **no line for 3 × heartbeat_ms**, and
   the deadline is derived from the server's declaration rather than from a number of ours
   (`idleFromHeartbeat`; `Capabilities.Heartbeat()` already resolves "not declared" to the ≤30 s the
   surface pins, which is where `DefaultIdleTimeout = 90 s` comes from). A server that *never* writes
   its liveness line is reported dead, correctly: its silence vouches for nothing.
5. **What the deadline keys on, and what it deliberately does not.** It resets on **any line**
   received — a heartbeat, an `invalidate`, an unknown event name, a line this client cannot even
   parse. Not on the *absence of events* (O-2 forbids that inference), and **not on the cursor**: the
   loop never writes `seq`, so a heartbeat is liveness here whether or not `apply()` also records it as
   a `seq`. That is the BFS-061 boundary: its fix (cursor bookkeeping on every line) and this fix
   (liveness on every line) are independent, and either can land without the other.

**The second decision the row asked for implicitly: what a clean EOF *is*.** §8.2 R-6: a live-context
EOF is a channel END, so it takes the reconnect path *with the cursor* — the same recovery as a
transport fault, because the cursor is the resume and a blip does not mean events stopped being
generated. It is nevertheless a **named** fault (`stream_ended`) rather than an anonymous transport
error, because "it closed on us" and "we closed it" are different facts: the context is the only thing
that can distinguish them, and it is available at the call site.

---

## 2. What was implemented

| # | change | where | what it closes |
|---|---|---|---|
| 1 | The read loop runs in its own goroutine and the consumer `select`s on **the line / the idle deadline / `ctx.Done()`**. A line resets the deadline *before* it is interpreted; `ctx.Done` is now honoured mid-stream (the landed loop could not be cancelled while a read was blocked). | `invalidate.go` `watchOnce`, `readStream`, `resetTimer` | H-3 (the timer) + the unbounded-read shape |
| 2 | `streamRead` is a **three-state** value (line / clean EOF / read error) instead of a line-or-nothing `Scan()`. A clean EOF on a live context returns `CauseStreamEnded` (`stream_ended`), counted as `stream_ends_total`. | `invalidate.go` `streamRead`, `watchOnce` | H-1 |
| 3 | `CauseStreamStalled` (`stream_stalled`) is the idle rule's verdict; `stallToPoll` counts it (`idle_fallbacks_total`), reports the channel dead, switches to the declared poll and **says so in `reason`** — the poll's own answer is what flips `available` back (O-1's ordering). | `invalidate.go` `stallToPoll`, `Run`, `reconnectLoop` | H-3 (§8.3's fifth fallback condition) |
| 4 | `watchSession` wraps every attempt at the stream (the first one and every reconnect) and marks the channel **unavailable while no stream is established**, whatever the failure — the landed code left `available=true` through a clean EOF, a stall and every reconnect gap. `Run` also marks the channel stopped on any return. | `invalidate.go` `watchSession`, `markUnavailable`, `markStopped` | H-1 (the honesty half: a channel that has stopped being one must not report itself as one) |
| 5 | The rule is the **relation**, not the constant: `effectiveIdle()` = the mount's own value, else 3 × the server's declared heartbeat, else `DefaultIdleTimeout`. `idle_timeout_ms` is reported whenever the push rule is or has been in force. | `invalidate.go` `effectiveIdle`, `idleFromHeartbeat`, `State` | H-3's "and say so", PRD §2.8 (a bound you cannot see is not a bound) |
| 6 | One shared predicate for §8.3's declared-degradation set, asked by **both** entry points (the first attempt and the retry after a fault). | `invalidate.go` `isDeclaredDegradation` | drift risk between `Run` and `reconnectLoop` (§8.4 R-7: reconnect forever against a server that will never serve the op) |

**One adjacent gap closed while inside the function, disclosed rather than smuggled:** `Run` already
treated four verdicts as "this build does not serve the op — take the poll"
(`capability_unavailable`/501/`op_unknown`/`extension_op_missing`), but the *reconnect* path only
matched the first two. A build answering `400 op_unknown` after a blip therefore reconnected forever,
which is the same "bounded and named for every failure path" requirement this row carries. #6 makes the
two paths share one set. It does not close any other row, and no other row's acceptance depends on it.

**Reported state, additive only** (`§11.1` gives the push path the right to a cursor + reconnect/idle
counters; BFS-045 owns names, layout and drill-down): `idle_timeout_ms`, `stream_ends_total`,
`reconnects_total`, `idle_fallbacks_total`. No existing field changed meaning. A client that never
calls the channel sees nothing: **no server-side byte changed in this row.**

---

## 3. The RED, on the tree as filed

`sh docs/evidence/BFS-060-arms.sh unfixed` — the two product files are replaced by the blobs of
`5baef51` and sha256-checked; the GREEN-only reporting arm is set aside (it asserts fields that do not
exist on that tree, and a compile error would not have been evidence about behaviour). Transcript:
`docs/evidence/BFS-060-red.txt`, verbatim:

```
pre-mutation: internal/fsclient/invalidate.go sha256=985858df5e7b59314c30cb227006e9b9444169cde57f4997109cb8dd897e6cc3
swapped in 5baef51:internal/fsclient/invalidate.go sha256=d54bbde1f2d7407c89c629feaa1c8badc8be6739661a00b5cc4787ef6def07b8
swapped in 5baef51:internal/fsclient/errors.go sha256=e47d970008be14604027727111644c53e341862ed12aab73abc32f70e91866b4
set aside: internal/fsclient/invalidate_bfs060_rule_test.go (GREEN-only arm: it asserts fields that do not exist on the filed tree)
--- RED (tree as filed): go test ./internal/fsclient -run TestBFS060 -count=1 -v -timeout 120s
=== RUN   TestBFS060CleanEOFIsAChannelEndNotASuccess
    invalidate_bfs060_test.go:365: CLEAN EOF ENDED THE CHANNEL: Run returned (err=<nil>) and no reconnect was attempted, so nothing can invalidate again for the life of the mount. The record left standing was mode="push" mechanism="watch" channel_available=true, events_total=1, drops=1 (1 watch stream(s) served)
--- FAIL: TestBFS060CleanEOFIsAChannelEndNotASuccess (3.03s)
=== RUN   TestBFS060StalledChannelIsNotAQuietTree
    invalidate_bfs060_test.go:515: quiet tree (900ms hold): [{mode:push mechanism:watch channel_available:true reason:"" dropped:0 resyncs:0 gaps:0}]
    invalidate_bfs060_test.go:516: stalled channel (900ms hold): [{mode:push mechanism:watch channel_available:true reason:"" dropped:0 resyncs:0 gaps:0}]
    invalidate_bfs060_test.go:520: A STALLED CHANNEL AND A QUIET TREE ARE THE SAME OBSERVABLE: both ended the 900ms hold as {mode:push mechanism:watch channel_available:true reason:"" dropped:0 resyncs:0 gaps:0} — the client cannot tell a dead channel from a healthy idle tree, which is exactly the obligation BFS-040 §5.4 O-2 makes and the fourth fallback condition of SPEC-push-channel §8.3 (idle rule = 3 × 40 ms = 120ms)
--- FAIL: TestBFS060StalledChannelIsNotAQuietTree (2.35s)
=== RUN   TestBFS060CancellationIsTheOneCleanClose
    invalidate_bfs060_test.go:593: the record still claims a live channel after Run returned: {Mode:push Mechanism:watch Seq:1 LastEventAge:10.255218ms LastEventAgeMS:0x294982482530 PollIntervalMS:<nil> Gaps:0 Resyncs:0 Events:1 DroppedPaths:1 Reason: Available:true}
--- FAIL: TestBFS060CancellationIsTheOneCleanClose (0.02s)
FAIL
--- RED (tree as filed) exit=1
```

Read as the two failures the row names, plus the honesty half of the first:

- **(a) the clean EOF.** The server ends the stream (the designed `watch_lost` of BFS-040 §4.6); `Run`
  returns `nil`; **no second watch request is ever made** (the arm waits a bounded 3 s for one and gets
  none), so a change made afterwards can never be invalidated for the life of the mount — and the
  record left standing still says `mode="push" mechanism="watch" channel_available=true`.
- **(b) the stall is the same observable as quiet.** The two arms' fixtures differ in exactly one way
  (one of them keeps writing its declared liveness line, the other goes silent after one line) and the
  record is **identical**, printed field by field. This is the measurement the row asked for: not "the
  timer is missing" but "the two states are one state from outside".
- **(c) the honesty half, on the cancellation path.** `Run` returns, and the record still claims
  `Available:true` — the same defect's second face, and the one that makes the first dangerous rather
  than merely useless.

**Bounds, stated:** the RED holds for 900 ms per arm against a rule of 120 ms (7.5 × the rule), waits
3 s max for a stream or a return, and completes in 5.4 s. No wait anywhere is unbounded.

---

## 4. The GREEN

`sh docs/evidence/BFS-060-arms.sh green` → `docs/evidence/BFS-060-green.txt`. All four tests pass
(3 behaviour + 1 reporting with 3 subtests), 3.85 s.

The measurement the row asked for, verbatim from the arm's own log:

```
quiet tree (900ms hold): [{mode:push mechanism:watch channel_available:true reason:"" dropped:0 resyncs:0 gaps:0}]
stalled channel (900ms hold): [{mode:push mechanism:watch channel_available:true reason:"" dropped:0 resyncs:0 gaps:0}
 {mode:poll mechanism:events channel_available:false reason:"the channel stalled: no line for 120ms (the idle rule: 3 × the declared heartbeat): the channel is stalled, not quiet — declared poll fallback" dropped:0 resyncs:0 gaps:0}
 {mode:poll mechanism:events channel_available:true reason:"the channel stalled: no line for 120ms (the idle rule: 3 × the declared heartbeat): the channel is stalled, not quiet — declared poll fallback" dropped:0 resyncs:0 gaps:0}]
```

Three claims, in order, and each is the honest one:

1. `push`/`watch`/available — the channel was established (the arm cannot say anything about a channel
   that never worked, so this is asserted first);
2. `poll`/`events`/**not available**, with the idle deadline that fired named in `reason` — **DEAD is
   reported dead**, and the bound is visible;
3. `poll`/`events`/available — the declared mechanism the client fell back to has answered, so the
   client is a working channel again *and the reason still says why it moved*.

The quiet tree never moves at all (`len(claims) == 1` over 900 ms, i.e. 7.5 × the rule): **healthy and
quiet is reported as healthy and quiet, and nothing was inferred from the absence of changes.**

The clean EOF arm (§4 of the test) asserts all four halves of R-6 in order: the channel was live, the
EOF happened, the reconnect presented `since_seq=1` (the cursor), and **a change made after the EOF was
delivered** — the acceptance "invalidation is not over for the life of the mount". The reporting arm
pins the counters (`stream_ends_total=1`, `reconnects_total≥1`, `idle_fallbacks_total=1`), the derived
rule (`idle_timeout_ms=120` for a declared 40 ms heartbeat; `90000` when the document names none) and
the reason text.

**The whole package under `-race`: `ok github.com/deployBunker/bunker/internal/fsclient 5.396s`**
(`docs/evidence/BFS-060-race.txt`) — the read goroutine is new code, so the race detector is part of the
acceptance, and it caught a real defect in my first test fixture (a write to the `ResponseWriter` from
the test goroutine; the serving goroutine now owns every write).

---

## 5. The negative controls (both arms must be able to fail)

Each control is one `git apply` of a patch kept in the repository, one bounded test run, and a restore
whose sha256 is checked and printed. Neither patch is committed; both are applied to
`internal/fsclient/invalidate.go` — the file this row owns.

| control | mutation | this arm | other arm |
|---|---|---|---|
| **EOF** (`BFS-060-negative-control-eof.patch`) — the EOF detection returns `nil` again, i.e. "we closed it" | `if rd.eof { … return nil }` | `TestBFS060CleanEOFIsAChannelEndNotASuccess` → **RED**: *"Run returned (err=<nil>) and no reconnect was attempted, so nothing can invalidate again for the life of the mount"* | `TestBFS060StalledChannelIsNotAQuietTree` → **PASS** (2.58 s) |
| **IDLE** (`BFS-060-negative-control-idle.patch`) — the deadline is armed and not acted upon, i.e. the declared rule has no implementation | the `case <-timer.C:` arm removed | `TestBFS060StalledChannelIsNotAQuietTree` → **RED**: *"A STALLED CHANNEL AND A QUIET TREE ARE THE SAME OBSERVABLE"*, both claims printed and equal | `TestBFS060CleanEOFIsAChannelEndNotASuccess` → **PASS** (0.54 s) |

Restores, verbatim from the transcripts:

```
restore: internal/fsclient/invalidate.go sha256=985858df5e7b59314c30cb227006e9b9444169cde57f4997109cb8dd897e6cc3 (verified against the pre-mutation hash)
```

The two arms are **mutual controls**: each fix disables only its own arm, which is the attribution
statement — neither arm can be passing because of the other fix, and neither fix can be believed on the
strength of the other's test. Building the RED the same way (the filed blobs, sha256-checked) is the
third leg: the arm was RED on the content it claims to have been RED on.

---

## 6. Every failure path, named and bounded

| the ending | cause named in the record | reported as | recovery | the bound |
|---|---|---|---|---|
| no line for 3 × the declared heartbeat | `stream_stalled` | `available=false` + `mode=poll` + the deadline in `reason` + `idle_fallbacks_total` | the declared poll (`events`, else `rev`) | the deadline itself: `idle_timeout_ms` (declared or derived) |
| clean EOF on a live context | `stream_ended` | `available=false` during the gap, `mode` stays `push`, `stream_ends_total` | reconnect **with the cursor**; a declared degradation → poll | one attempt per backoff step, capped at 10 s |
| transport fault / reset | the transport cause (unchanged vocabulary) | same as above plus `reconnects_total` | reconnect with the cursor | delay 500 ms ×2, cap 10 s |
| declared degradation (501 / 400 `op_unknown` / 400 `extension_op_missing`) | the server's own verdict + `capability_unavailable on watch (scope=… mode=…)` | `mode=poll`, mechanism switched before the first poll answer (O-1) | the declared poll | one attempt |
| stale tree identity | `stale_tree` / `stale_identity` | **not** a downgrade: re-bind required | `ErrDeletedTree` to the mount | one attempt |
| the context ended (we closed it) | — (the one clean close) | `Run` returns `nil`, `available=false`, **no reconnect** | nothing: the caller is gone | prompt: `ctx.Done` is selected in the read loop, so a stalled read can no longer outlive cancellation |
| `--invalidation=push` and any of the above | the fault, verbatim | a **loud error**, not a silent downgrade | the mount's own declaration | one attempt |

The read loop itself waits on exactly three things (`lines`, `timer.C`, `ctx.Done`) — there is no wait
in this row without either a deadline or a context.

---

## 7. What this row did NOT change

1. **The server.** No byte of `internal/server/**` moved. The push channel is still refused with the
   declared `mode=poll` on this build (BFS-036 has not landed), which is *why* the arms drive a stub
   that serves the channel's own wire form (`POST X-Bunker-Op: watch`, `application/x-ndjson`, one JSON
   object per line, `{"paths":[],"since_seq":N}` — §2/§3 of the push spec). The stub also answers the
   bind-time probe and the `events` op, so the capability document, the declared heartbeat and the fallback
   mechanism in the arms are the real ones.
2. **The poll form.** `pollLoop`, `pollEventsOnce`, `pollRevOnce` are untouched (F-1). The only
   interaction is `available` going false while no stream is established and true again when a poll answers.
3. **The resync path.** Every resync rule is as it was; the EOF path reconnects and never re-requests a
   missing range, exactly as §3.2 requires. A resync is still always correct and only expensive.
4. **BFS-061, BFS-062, BFS-063 — untouched, and each given room to land.**
   - **BFS-061 (the heartbeat's `seq` manufactures a false gap):** no change to `apply()`'s cursor
     bookkeeping at all. My liveness does not read or write `seq`; it resets a timer on the receipt of a
     line in the read loop, which is a different fact from the cursor. Evidence of the boundary: the
     arms' reconnect carries `since_seq=1`, the last *applied* seq, with heartbeats present in the stream
     and no gap counted (`gaps:0`) — i.e. this row neither consumes nor advances the cursor.
   - **BFS-062 (a legal 4096-path frame vs the 8 MiB token cap):** the reader keeps the same 16 KiB
     initial / 8 MiB maximum token and the same `classifyTransport` classification, so a too-long line
     still surfaces as a transport fault exactly as before. The goroutine and the channel carry lines
     unmodified.
   - **BFS-063 (the poll's off-the-end resume guarded on a non-zero cursor):** untouched, in `events.go`
     and in `apply()`'s `i.seq != 0` guard.
5. **The honesty of the refusal path.** `TestInvalidatorStaysHonestWhenThePollFormIsRefused` (a server
   that advertises the poll form and serves neither mechanism) still passes unchanged: the record stays
   `channel_available=false` with the server's own refusal in `reason`, and now also says the channel is
   not available rather than relying on a never-established stream to imply it.

---

## 8. Residuals, and what I did not prove

- **R1 — the mount never cancels the invalidator's context.** `internal/fsmount/fs_linux.go` runs
  `m.inv.Run(context.Background())`, so in production the read loop ends only when the process does. My
  `ctx.Done` arm therefore matters to library callers and tests more than to the shipped mount; wiring
  `Unmount` to a cancellable context is a one-line change in a file this row does not own, and is
  reported rather than taken. (Not a leak with a consequence today: the mount exits.)
- **R2 — a buffering intermediary is still undetectable in-process** (§10.1 item 4 of the push spec).
  This row does not claim otherwise: the *symptom* the spec names is the 90 s degradation, and that
  symptom is now real, reported and counted. If a proxy buffers heartbeats, the client will declare the
  channel dead and fall back — correct by the letter and honest by the record, at the cost of a
  needless downgrade.
- **R3 — the reconnect backoff carries no jitter** (H-8). Not this row's hole, and not fixed here: N
  mounts stranded by one restart still return within the same millisecond.
- **R4 — `events_total` counts heartbeats** (landed behaviour, unchanged). A quiet tree's counter grows
  because liveness lines arrive. That is the pre-existing reading of the field and BFS-045 owns the
  layout; I did not repurpose it, which is why the arms compare claims rather than counters.
- **R5 — a line that cannot be parsed still resyncs once per line** and counts as liveness. Bounded
  (one resync, one line) and unchanged from the landed behaviour, but a server writing garbage forever
  keeps the channel "alive" while forcing a resync per line. Named here, not fixed: the remedy is a
  protocol-fault policy that belongs to whoever owns the wire-form conformance rules.
- **R6 — the arms drive a stub, not a live server.** The server half of the push channel does not exist
  on this build (BFS-036), so no live deployment can serve a stream today. What the arms *can* prove —
  and do — is the client's own contract against the channel's own wire form, which is where both filed
  defects live. The one thing they cannot show is a live FUSE mount's caches actually being dropped on
  the reconnect, and they do not claim it.
- **R7 — one allocation per line.** The line now travels from the reading goroutine to the consumer, so
  it is copied out of the scanner's buffer (`append([]byte(nil), …)`) instead of being read in place.
  That is one small allocation per event on a path that carries O(changes) + one line per heartbeat
  period; it is the price of having a deadline at all, and it is spent deliberately rather than
  discovered later.

---

## 9. Reproducing this

```sh
cd <worktree>                       # repo branch wt/BFS-060, no push
sh docs/evidence/BFS-060-arms.sh green      # all arms PASS
sh docs/evidence/BFS-060-arms.sh unfixed    # the tree as filed: all three behaviour arms FAIL
sh docs/evidence/BFS-060-arms.sh noeof      # control EOF:   EOF arm RED, stall arm PASS
sh docs/evidence/BFS-060-arms.sh noidle     # control IDLE:  stall arm RED, EOF arm PASS
sh docs/evidence/BFS-060-arms.sh race       # whole package under -race
go test ./... -count=1 -timeout 600s        # the row's regression floor
```

`unfixed` reconstructs the filed content from `5baef51` and refuses to run if the reconstructed blob
does not hash to `d54bbde1…`/`e47d9700…`; the two controls reconstruct from the patches in this
directory. Every mode ends by re-hashing the file it mutated and printing the verification line, so a
failed restore is loud rather than silent.
