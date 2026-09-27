# BFS-036 — the pushed `watch` form

**Row:** BFS-036 (P1, complexity 3) · **Repo:** `/home/kara/bunker` · **Worktree:** `wt/BFS-036`
**Landed:** `6769b88` (the stream, the client's switch, the poll unchanged), `750344f` (the commit
flush, found by the `no-fanout` arm) + the evidence commit
**Authority:** `docs/prd/SPEC-push-channel.md` (BFS-041), `docs/prd/PRD-bunker-invalidation.md` R3/§2.7/§5.

---

## 0. The decision, and the numbers

**The channel is a STREAM, served by the `watch` op the surface already had, and the poll is
untouched beside it.** The wire form is the spec's, not a new one; nothing about the choice of a
stream over a bounded long-poll is re-litigated here.

| what | value | where it comes from |
|---|---|---|
| op | `X-Bunker-Op: watch` | already the invalidation op's carrier (BFS-026) |
| response | `200`, `Content-Type: application/x-ndjson`, `Cache-Control: no-store` | SPEC §2 |
| one line | `{"seq":N,"event":"…","paths":[…],"rev":"…","tree":"…"}` | `eventLine`, the E-4 record BFS-060/061 already fixed |
| scope | `whole_tree` (`X-Bunker-Subscription: whole_tree`) | R-5: one ledger, no per-subscriber filtering (W-3) |
| resume | body `{"paths":[],"since_seq":N}` | SPEC §3.2 |
| heartbeat | **30 000 ms** declared, one line per period | `invalidation.DefaultWatchHeartbeatMS` |
| write deadline | **10 000 ms** declared, armed per write | `DefaultPushWriteDeadlineMS` (B-6: strictly < heartbeat) |
| buffer | **4 MiB / 256 events** per subscriber, drop-oldest | `DefaultPushBufferBytes`, `DefaultPushBufferEvents` |
| subscribers | **8** per target, a retryable `429` beyond | `DefaultPushMaxSubscribers` |
| event bound | **1 MiB** per frame | `DefaultPushMaxEventBytes` (BFS-062's negotiated bound) |
| refusals | `501 capability_unavailable` where push is not served (byte-identical to the pre-row refusal); `429` + `Retry-After` at the cap, with **no** `X-Bunker-Verdict` | B-13/B-14 |

A real transcript, from the runnable cell rather than from a hand-written sample
(`docs/evidence/BFS-036-green-server.txt`; the fixture's bounds are compressed so a cell does not wait
thirty seconds for a heartbeat):

```
    push_test.go:301: wire: {"seq":1,"event":"heartbeat","paths":[],"rev":"rev:0@0","tree":"tree:8abb78f35b838603"}
    push_test.go:301: wire: {"seq":2,"event":"invalidate","paths":["src/main.go"],"rev":"rev:0@1","tree":"tree:8abb78f35b838603"}
    push_test.go:303: channel: subscription=whole_tree heartbeat_ms=300 buffer_bytes=4194304 buffer_events=256 max_subscribers=8 write_deadline_ms=100 counters={SubscribersActive:1 SubscribersActiveMax:1 DropsTotal:0 GapsTotal:0 DisconnectsTotal:0 DisconnectsCtxTotal:0 DisconnectsDeadlineTotal:0 BufferHighWaterBytes:101 WriteDeadlineRefusedTotal:0}
--- PASS: TestPushCell01AChangeIsPushedWithTheMechanismOnTheWire (0.02s)
```

The change that landed was made by a plain file write — no WebDAV request, no in-process call — and
`rev:0@0 → rev:0@1` is the tree's own revision token moving because of it. The `seq`-1
`heartbeat` is the ESTABLISH line: the channel commits by speaking, pushed through the ledger's
funnel so it is journaled and broadcast like every other line rather than being a private token
invented for one subscriber (R-5).

A second subscriber on the same quiet tree, from the heartbeat cell — the declared period honoured,
each line carrying the cursor the client advances over:

```
    push_test.go:659: wire: {"seq":1,"event":"heartbeat","paths":[],"rev":"rev:0@0","tree":"tree:8dfc0c2a02c1fb5b"}
    ...
--- PASS: TestPushCell06TheDeclaredHeartbeatBoundIsHonoured (2.15s)
```

---

## 1. The server endpoint

`internal/server/webdav/push.go` (new): the hub, the per-subscriber buffer, the writer, the counters
and the capability block. The op is served by the SAME handler that already served the invalidation
ops, so the placement property holds by construction:

* `ServeHTTP` dispatches `X-Bunker-Op` **before** anything reaches the chi router
  (`internal/server/webdav_mount_test.go: TestWebDAVOpIsAnsweredAheadOfTheRPCTimeout`, landed with
  BFS-006), and P-9 re-proves the property for the STREAM specifically: a subscriber is served past a
  150 ms deadline that already answers a routed handler `504`. The stream therefore inherits the
  300 s RPC timeout exemption the placement exists for, rather than repeating the defect a long-poll
  behind that layer would have.
* Push is **per-target**: the op list, the mode, the degradations and the stream's answer all derive
  from **one** watcher snapshot (`pushServedFrom` — "the ONE definition of 'the push is served'"), so
  a document can never advertise a mode the stream will not serve.
* Where no event source exists the `watch` refusal is produced by the **same code with the same
  fields** as before this row (`watchRefusal`: `capability`, `scope: target`, `mode: poll`, the
  status's `reason`, the configured-vs-observed pair, the detail — checked against `6769b88^`), and
  P-7 asserts the status and the verdict on a handler with no watcher. The ONE refusal this row
  removes is the build-scope branch that said *"the push wire form is not in this build"* — it is
  unreachable now because that is exactly the fact this row changes, and keeping it would be an
  absence that is no longer absent.

**Bounds are enforced, not just published.** `Handler.eventMaxBytes` and the push cadences come from
the same resolved invalidation surface the watcher obeys (`handler.go`), so the numbers in the
capability document are the numbers the server applies.

**The channel is committed before any line is waited for** — headers, then a flush, then the resumed
plan, then the loop. That is not decoration: see §6.

---

## 2. The client's switch (RED → GREEN)

The pre-row client learned about the channel **by asking**: `Handshake` probed the `watch` op inside
the bind budget. With `watch` now a stream that probe is a *subscribe*, so it would burn the whole
bind deadline and then report the watcher unavailable — on every bind, on every mount, on every host
that offers push.

**RED on the tree before this row's client change** (the reproduction is the `probe-not-declared`
mutation in the arms script, which puts the client back on the pre-row code path —
`docs/evidence/BFS-036-c1-red.txt`):

```
--- FAIL: TestPushClientSwitchReadsTheDeclarationNotAStreamProbe (2.01s)
    push_test.go:100: the document declares mode=push and the handshake still reports the watcher
    unavailable (source="probe_refused"): the client asked by opening a stream instead of reading the offer
FAIL	github.com/deployBunker/bunker/internal/fsclient	2.009s
```

**GREEN with the fix:** `Handshake` reads the declaration (`capabilities` document → `mode: push`),
sets `WatcherAvailable` from it, and does **not** open a stream at bind time — 0.01 s against the same
endpoint whose bind budget is 2 s. The delivery cell proves the other end of the switch: against a
real `davserve` endpoint with a real watcher, a plain file write to the served tree arrives as a
pushed line, the mount applies it, and the status record says so
(`docs/evidence/BFS-036-green-client.txt`):

```
    push_test.go:156: the delivered record: mode=push available=true mechanism=watch seq=3 events=2 dropped=1 gaps=0 resyncs=0 stream_ends=0 reconnects=0 reason=""
--- PASS: TestPushClientAppliesAPushedChangeAndReportsTheWatchMechanism (0.06s)
```

---

## 3. Mechanism honesty

The record already carried the mechanism (BFS-048's rev-kind, BFS-060's liveness); this row must not
simplify it, and does not. Two independent carriers say `watch`:

* the transport: `X-Bunker-Op: watch` + `Content-Type: application/x-ndjson` (the poll answers
  `X-Bunker-Op: events` + `application/json`), and the client sets its mechanism from the **op that
  answered**, not from what it hoped for;
* the status record: `mechanism=watch` vs `mechanism=events` vs a resync.

**The control that matters** (`TestPushClientNeverReportsWatchWhenThePollDelivered`): the *same*
push-capable endpoint, with the invalidator pinned to `ModePoll`, must report `mechanism=events` and
mode `poll`; the same endpoint with the mode left automatic must report `watch`. One endpoint, two
records, two different answers — so the field follows the mechanism in force and cannot be made to
say "push" when the poll delivered it. Under the `mechanism-lie` mutation this cell goes RED in 10s
while the push cell stays green.

---

## 4. A dead client cannot hold a stream

Two independent detectors, both armed by the handler, both counted separately:

| the client | what finds it | counter | cell |
|---|---|---|---|
| **disconnects** (FIN/RST, or the request context ends) | `r.Context()` in the select | `disconnects_ctx` | P-2 |
| **stops reading, stays connected** | the per-write deadline (`DefaultPushWriteDeadlineMS`) | `disconnects_deadline` | P-3 |

Both cells assert three things, not one: the counter moves, `SubscribersActive` returns to **0**, and
the released slot is immediately reusable — a bound that releases the connection but leaks the slot
is not a bound. The write deadline is *probed* rather than assumed: a transport that cannot carry one
(a hypothetical HTTP/3 writer) reports itself in `counters.write_deadline_refused` and in the
capability document's `write_deadline_supported`, rather than claiming a detector it does not have.

The stalled-reader cell drives a `net.Pipe` whose reader never reads, so the write genuinely blocks —
the arm proves the bound is honoured by *reaching* the bound, not by sleeping and hoping. The two
triggers are separable in the record, measured rather than asserted
(`docs/evidence/BFS-036-green-server.txt`):

```
    push_test.go:379: the dead client's release: counters={SubscribersActive:0 ... DisconnectsTotal:1 DisconnectsCtxTotal:1 DisconnectsDeadlineTotal:0 ...}
    push_test.go:431: the stalled reader's release: counters={SubscribersActive:0 ... DisconnectsTotal:1 DisconnectsCtxTotal:0 DisconnectsDeadlineTotal:1 ...}
```

and the slot comes back:

```
    push_test.go:394: the slot came back and was released again: counters={SubscribersActive:0 ... DisconnectsTotal:2 DisconnectsCtxTotal:2 DisconnectsDeadlineTotal:0 ...}
```

---

## 5. Resume from cursor — or told to resync, never a silent gap

The stream replays from the ledger; it does not observe the tree. Four cases, all asserted in P-8
against the real journal, and the third one is the whole point:

| presented cursor | answer |
|---|---|
| inside the retained journal | the lines after it, in seq order |
| at head | nothing (the client is current) |
| **behind the journal's base** | an `overflow` line **FIRST**, at a seq above the client's own duplicate rule, before any other line |
| ahead of head | nothing + the `overflow` (the "I did not see what it says it sent" case) |
| **no cursor at all** (`since_seq` absent) | the whole interval the ledger cannot vouch for — never a quiet tail |
| cursor `0` against a ledger that has observed nothing | the same unvouched interval |

The client side is BFS-061/063's, unchanged in shape: a line whose seq is not the expected next seq
is a gap, the cursor advances only over lines it has actually applied (§3.3 R-1), and the
presented `since_seq` is the strongest cursor the client can back — its own observation where it
holds one, otherwise the highest seq it has applied, otherwise `0`.

**BFS-036's client change, and the collision it caused.** The pre-row stream presented `i.seq`, which
is `0` for a mount that bound with a snapshot and has not yet applied a line: against a rotated
journal such a client was answered with a `watch_overflow` resync on every reconnect — a real but
narrow cost. The stream now presents the **observation cursor** where the client holds one. That
change collided with four landed BFS-060/061/062 cells, whose fixture identifies a *subscription* by
"the body carries `since_seq`" (the bind-time probe must not be counted as one). A client that
presents **no** cursor makes that discriminator false. The resolution keeps both properties: the
observation cursor wins where it exists, and a client holding nothing presents `0` (**"my view starts
at the beginning"** — a cursor the server can vouch for, and the honest claim) rather than a cursor
it does not hold. Omitting `since_seq` entirely was the first attempt and was reverted for this
reason; `C-4` now pins both halves (the observation cursor where held, `0` where nothing is held).

---

## 6. Backpressure, with a COUNTED gap

Drop-oldest within the subscriber's own buffer, and the drop is **counted and declared**: the
dropping subscriber owes exactly one `overflow` line, emitted before the next line it forwards, so a
client can never see a boundary it cannot recognise. Then, and only then, the buffer's earliest
entries go. Per §5.4 the counter is the count *in flight*, not a lifetime total.

Nine lines pushed into an eight-event buffer that nobody drains, then drained
(`docs/evidence/BFS-036-green-server.txt`):

```
    push_test.go:558: the counted gap: pushed=9 written=5 counters={SubscribersActive:1 ... DropsTotal:5 GapsTotal:1 ... BufferHighWaterBytes:396 ...}
```

five lines written for nine pushed — four lost plus the ONE marker that declares them, and the
high-water figure that makes the bound reportable rather than merely enforced (§2.7).

Every line in the buffer keeps the path list it was pushed with — a trimmed list would be the
truncated-list shape §6.2 B-10 forbids, and grouping two events into one marker would understate the
drop.

---

## 7. The arms: nine mutations, nine REDs, nine sha256-verified restores

`docs/evidence/BFS-036-arms.sh` (BFS-046's shape: mutation → the claiming cell RED → an attribution
control that must stay GREEN → restore → hash census). Transcripts:
`BFS-036-arms-all.txt` (green + all nine arms + the census, `RC=0`),
`BFS-036-green-server.txt`, `BFS-036-green-client.txt`, `BFS-036-c1-red.txt`, and
`BFS-036-green-bfs06x.txt` (the landed BFS-060..063 cells, re-run).

| mutation | the claim it neuters | RED | control that stayed GREEN |
|---|---|---|---|
| `probe-not-declared` | the client reads the declared mode | C-1 | C-2 (a pushed change is applied) |
| `no-fanout` | the ledger's funnel broadcasts | P-1 | P-8 (the ledger itself) |
| `no-heartbeat` | the declared heartbeat bound is honoured | P-6 | P-1 |
| `no-gap-marker` | a drop is declared, not silent | P-4 | P-1 |
| `no-write-deadline` | the deadline is *honoured*, not merely armed | P-3 | P-2 (context detection) |
| `cap-is-degradation` | the cap is retryable, not a degradation | P-5 | P-1 |
| `silent-gap-resume` | a cursor behind the journal is told to resync | P-8 | P-1 |
| `no-commit-flush` | the channel is committed before any line | P-10 | P-1 |
| `mechanism-lie` | the record names the mechanism that delivered | C-3 | C-2 |

Three of the script's own rules are load-bearing, and all three were learned by running it:

1. **A non-zero exit is not a RED.** The `no-fanout` arm did not fail: with every line suppressed,
   the cell BLOCKED on a header that was never flushed, the timeout killed it, and a naive
   "exit != 0" would have recorded that hang as a red. `must_fail` now requires a real `--- FAIL` in
   the log — BFS-017's law, one repository over, and the same shape as this row's own subject matter.
2. **A SKIP is not a GREEN.** The attribution control for `probe-not-declared` first read the
   function under test (`WatchPushDeclared`) to decide whether to run, so the mutation made it skip —
   a control that passes by not running. Every cell's guard now reads the **declared mode**.
3. **Every exit path restores.** An arm that fails its own assertion left the mutation in the tree,
   and the next run's GREEN then failed against a mutated tree, which reads as a broken row rather
   than a broken script. `trap restore EXIT INT TERM` is now installed.

A mutation that does not land, or does not compile, aborts the arm: a `sed` whose substitution is a
no-op would otherwise be evidence of nothing, and a mutation that breaks the build would produce a
"red" that is a build failure.

---

## 8. The poll still works, and push is never a requirement

* P-7 asserts the poll's envelope, `count`, `head_seq`, `scanned`, content type and
  `X-Bunker-Op: events` **while a stream is attached to the same tree** — heartbeats are journaled,
  a polling client may carry them, and the envelope must not change because another client is
  subscribed.
* The same cell asserts the refusal on a handler with **no watcher**: `501 capability_unavailable`,
  the same verdict and the same poll form served. Every host without an event source behaves exactly
  as it did before this row.
* A stock HTTP/1.1 client that never sends `X-Bunker-Op: watch` is not on this path at all: the
  stream is a new response shape behind an op header no existing client sets, and the poll op is
  unchanged code.

---

## 9. What this row deliberately did NOT do

* **No hot-file cache** (BFS-037) and **no DC validation harness** (BFS-047) — both depend on this.
* **No weakening of BFS-061/063's gap logic or BFS-062's frame bound**: `resumePoint`,
  `frameLimitMismatch` and the client's `apply` are used, not rewritten, and the landed BFS-060..063
  cells are re-run in the GREEN transcript (`BFS-036-green-bfs06x.txt`, `ok 7.274s`). Every mutation
  targets one mechanism and names a control that must survive it (§7).
* **Nothing new is required of an unwatched host**: no watcher means the same 501 refusal and the
  same poll form, asserted as bytes in P-7.
* **`write_deadline_supported` is reported, not assumed**: it is probed per connection and counted.

### Residuals (named, not hidden)

1. **A journaled heartbeat per subscription.** The establish line is pushed through the ledger's
   funnel so it is broadcast and journaled like every other line (R-5), which means a reconnect loop
   advances the ledger's seq and its journal. Bounded (`eventsJournalEvents`) and cheap, but a
   rapidly reconnecting client does write journal lines.
2. **A client whose view was dropped by a resync whose re-observation failed** still presents its
   pre-drop cursor (the pre-existing stream behaviour, deliberately preserved here because BFS-063
   kept it). The server answers from the ledger, so the cost is bounded by the journal; the
   alternative (omitting the cursor) was measured against the landed cells and rejected in §5.
3. **The `X-Bunker-Rev` line field** is the tree's token at the moment the line is written, not at
   the moment the change happened; `seq` is the ordering authority, and the rev kind (`git`,
   `git+watch`) is BFS-048's.

---

## 10. Reproduction

```sh
sh docs/evidence/BFS-036-arms.sh green   # every cell, on the committed tree
sh docs/evidence/BFS-036-arms.sh all     # green + nine mutations + the hash census
```

The script refuses to mutate a file that is not at its committed hash, refuses to count a non-zero
exit as a RED, refuses a SKIP as an attribution control, and restores on every exit path including a
failed assertion — four rules, each of which this row learned by running it.
