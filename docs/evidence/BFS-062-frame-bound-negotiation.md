# BFS-062 — a legal event frame must not be able to loop the consumer

**Row.** The server permits a 4096-path event while the consumer caps a decoded
line at 8 MiB, so the server can emit a frame the consumer MUST reject — and the
rejection was classified as a **transport** failure, which reconnects. A defect in
the contract **between two of our own components**, not in either one alone. A
reconnect loop on a deterministic input is an outage.

**Evidence produced by one script** (`BFS-062-arms.sh`, four modes); every claim
below is a line in one of these transcripts:

| transcript | what it is |
|---|---|
| `BFS-062-green.txt` | the fixed tree: every consumer and producer arm passes |
| `BFS-062-red.txt` | the tree **as filed** (base `064b39f`, blob sha256 asserted on every swap): the legal frame → transport misclassification → **counted** reconnect |
| `BFS-062-control-noclassify.txt` | the negative control: the classification neutered on the fixed tree turns the cell red, with a sha256-verified restore back to green |
| `BFS-062-suites.txt` | `gofmt -l`, `go build`, `go vet`, `go test ./...` on the fixed tree |

---

## 1. The decision: **(c) NEGOTIATE**, on the additive `max_event_bytes`

The row asks for one of three fixes, argued. (c) is the additive option and the
spec already declares the field — and that is **not** why it wins. It wins because
it is the only option in which the bound and the measurement live on the side that
can actually see them: the producer measures the frame it assembled, the consumer
sizes its reader from the producer's declaration, and neither assumes the other's
limit.

### The worst case is arithmetic, not an estimate

Taken from the surface's own declared limits, through the **wire encoder**
(`eventFrameBytes`, the same `encoding/json` call the wire form uses) — the arm is
`TestBFS062TheCountIsNotASize/the_worst_legal_frame_is_arithmetic,_not_an_estimate`:

| factor | value | provenance |
|---|---|---|
| paths per event | 4096 | `eventsMaxPathsPerEvent` (BFS-004 §3 E-6 decl. 2; also the ceiling of BFS-043's `flush_max_paths` knob) |
| bytes per path | 4095 | Linux `PATH_MAX` = 4096 **including** the terminating NUL |
| JSON escaping | ×6 | `encoding/json` emits `<`, `>` and `&` as `\u003c` — six bytes per input byte, the worst case for a legal filename |
| **worst legal frame** | **100 651 093 bytes = 96.0 MiB** | measured, printed by the arm |

The spec's own 16.8 MiB (`4096 × 4099`) is the *unescaped* figure; the escaped
figure is the real worst case and it is what kills option (a).

### Why not (a) — raise the consumer's per-line cap

- The number it must cover is **96 MiB**, not 16.8 MiB, and it is a function of the
  escape ratio and the count: both are the *producer's* configuration, so the
  consumer would be reserving memory against a bound it does not own.
- It fixes nothing: the producer still has no byte bound, so a frame above
  whatever cap is chosen remains legal to send — which is exactly the BFS-031
  lesson ("a size bound the producing side is allowed to exceed is not a bound").
- The consumer's cap today (8 MiB) already equals the largest bound the server's
  own knob table permits it to declare, so raising the *constant* buys nothing for
  any legal deployment — the mismatch is that the consumer never read the
  declaration, not that its constant was small.

### Why not (b) — lower `max_paths_per_event`

- A count cannot bound a frame, so the count would have to be derived from the
  byte cap and the *escape ratio*: 8 MiB ÷ (4095 × 6 + 3) ≈ **341 paths**, a 12×
  cut.
- 4096 is a declared contract number (BFS-004 §3 E-6, BFS-040 §2.2) and this row
  does not get to lower it: a 4097-path burst (an ordinary `make clean && make`)
  would begin answering `overflow` — the knowledge-loss marker — for every client,
  to avoid a bound that a measurement can carry instead.
- And it is *still* not a bound: the same count at a lower escape ratio wastes the
  budget, and at a higher one it exceeds it.

### Why (c) — and what it costs

The field is **additive** (`document_version` stays 1; a peer that predates it
ignores the key — the consumer decodes it into a zero-valued field and reports
"no bound declared" *as a fact*, not as a bound of zero). Nothing about the wire
form changes for a stock HTTP/1.1 client: the declaration rides in the capability
document the surface already serves, and the frame shape is unchanged.

The cost is honest and named: the consumer's reader is now sized from the
declaration, so a server that declares a bound and then exceeds it is refused —
with a counted, named, non-retried condition — instead of being read because the
consumer happened to have a bigger constant.

---

## 2. Which side owns the bound now

| side | owns | where |
|---|---|---|
| **server** | the byte bound: it **measures the serialized frame** and decides `overflow` on either bound being exceeded | `webdav/events.go` — `eventFrameBytes`/`frameOverBound`, applied inside the ledger's single `push` funnel, so the **journal** can never hold an over-bound frame either. Every producer of an `invalidate` goes through that funnel — the events poll *and* the watcher's own journal (`watch.go`'s `journal`), which is what keeps the two mechanisms from disagreeing about what a frame may contain |
| **server** | the declaration: the value published is the value enforced | `watchDocumentBlock` publishes `extensions.watch.max_event_bytes` from the resolved surface; the BFS-043 read-back reports that knob `applied:true` with the assembly named |
| **consumer** | the relation: `per-line cap ≥ declared + slack`, checked **before** a stream is opened | `fsclient/framelimit.go` — `frameLimitMismatch`, `readCap` |
| **consumer** | the classification: an over-limit frame is a named, counted, non-retryable condition | `fsclient/framelimit.go` — `classifyStreamRead`, `frameLimitDegradation`, asked by **both** entry points (first attempt and retry-after-a-fault) so they cannot drift |

The consumer's ceiling is **derived, not chosen**: the largest bound the surface
may declare (8 MiB — the server's own knob ceiling) plus the framing slack. A
ceiling that merely equalled the declaration would refuse a conforming server (the
slack would push it over); a larger one would reserve memory for a frame no
deployment can legally send.

---

## 3. The RED — a legal frame, a transport misclassification, a counted reconnect

`BFS-062-red.txt`, from `BFS-062-arms.sh unfixed` (product files replaced by the
base blobs, sha256 printed for every swap; the file this row added is set aside
with it, and the three GREEN-only test files are set aside — see §5):

```
--- RED (tree as filed): the frame-limit arm
=== RUN   TestBFS062ALegalOversizedFrameIsNamedCountedAndNotRetried
    legal frame: 4096 paths × 4095 bytes = 16810055 bytes on one line (16.0 MiB), against the declared 1048576-byte bound
    A LEGAL SERVER FRAME PUT THE MOUNT INTO THE RECONNECT LOOP: a 16810055-byte frame (4096 paths of PATH_MAX),
    reconnects_total=1, last_failure="watch : errno=ENOTCONN cause=unreachable_connect: bufio.Scanner: token too long",
    streams served=2. A reconnect on a deterministic input is an outage; the condition must be NAMED, COUNTED and NON-RETRYABLE.
--- FAIL: TestBFS062ALegalOversizedFrameIsNamedCountedAndNotRetried
```

Three things are in that failure, and the row asks for all three:

1. the frame is **legal** — 4096 paths, the count bound's own maximum, each path
   within `PATH_MAX`: nothing here exceeds a declared limit;
2. the failure is classified **transport** (`cause=unreachable_connect`, the
   scanner's `token too long` folded into it);
3. the **reconnect is counted**: `reconnects_total=1` and `streams served=2` — the
   second stream is the loop, measured rather than asserted.

The declaration arm fails on the same tree for its own reason — the filed consumer
reads a 200 KiB frame under a 64 KiB declaration, because its reader is a constant:

```
--- RED (tree as filed): the declaration arm
    the same frame under a smaller declaration produced no named condition: {Mode:push Mechanism:watch … Reconnects:0 …
    Failures:0 …}   (the frame was read and applied)
--- FAIL: TestBFS062TheReaderFollowsTheDeclaration
```

And the **non-vacuity control passes on the filed tree** — which is the
attribution: the two failures above are this row's defect, not a broken harness.

---

## 4. The GREEN — named, counted, non-retried, and the mount stays a mount

`BFS-062-green.txt`:

```
--- GREEN: go test ./internal/fsclient -run TestBFS062 …
--- PASS: TestBFS062ALegalOversizedFrameIsNamedCountedAndNotRetried (1.55s)
    named + non-retried: reconnects_total=0, streams served=1,
    reason="event frame over the declared bound: an event frame exceeded this mount's 1114112-byte per-line cap
            (the server declared a 1048576-byte per-frame bound, read with 65536 bytes of framing slack):
            the frame is rejected once and reported, never retried — the same bytes would arrive again
            — declared poll fallback, no reconnect (the same frame would be sent again)"
--- PASS: TestBFS062TheRecordNamesAndCountsTheFrameLimit (0.10s)
    frame_limit: declared=1048576 reader=1114112 ceiling=8454144 over_limit=true total=1 detail="…"
--- PASS: TestBFS062TheReaderFollowsTheDeclaration (1.25s)
--- PASS: TestBFS062AnUnholdableDeclarationRefusesTheChannel (0.01s)
--- PASS: TestBFS062TheClassificationDoesNotFireOnOrdinaryFrames (0.32s)
--- producer arms exit=0
    worst legal frame: 4096 paths × 4096 bytes (escaped) = 100651093 bytes = 96.0 MiB
    declared floor 65536 bytes refused a 86561-byte frame (refusals 0 -> 1); the answer is an overflow carrying 0 paths
```

The acceptance, item by item:

| acceptance | how it is shown |
|---|---|
| a legal frame the server can emit cannot put the consumer into a reconnect loop | the 16 810 055-byte frame ends in the named condition; `reconnects_total` stays **0** and the fixture serves **one** stream, measured over 1.5 s — three times the first backoff |
| an over-limit frame is a **NAMED** condition | cause `event_frame_over_limit` + a detail naming the reader cap, the declared bound and the slack; `last_failure` is asserted to contain no transport cause |
| … **COUNTED** | `frame_limit.over_limit_total = 1` (counted at the decision, not per attempt or per path) and it is also reported as a state (`frame_limit.over_limit`) — a bound the owner cannot see is not a bound |
| … **NON-RETRYABLE** | the condition never reaches `reconnectLoop`: both entry points ask one predicate, and the mechanism moves to the declared poll **once** (`mode=poll`, `mechanism=events`, `available=true` afterwards — the channel is degraded, not dead) |
| the fix is **argued from the measured worst case** | §1: 96.0 MiB measured through the wire encoder; every arm builds its frame from the declared limits |
| **which side owns the bound** | §2 |
| the **negative control turns red** | §5 |

The declaration is honoured in *both* directions, which is what "neither side
assumes the other's limit" has to mean in practice:

- the **same** ~200 KiB frame is **read and applied** under a 1 MiB declaration
  (`reader_bytes = 1114112`) and **refused, once** under a 64 KiB one
  (`reader_bytes = 131072` — the boundary follows the declaration, not a constant);
- a declaration above the surface's own ceiling (9 MiB) is refused **before a
  stream is opened** (`streams served = 0`), with `reader_bytes` **absent** rather
  than zero — no reader was allocated, and "a reader of zero bytes" is not a fact
  about anything;
- the producer side: a change whose frame measures 86 561 bytes is an `invalidate`
  under the declared default and an `overflow` carrying **zero** paths under the
  surface's declared minimum, with the refusal counted (`refusals 0 -> 1`) and the
  measured size on the counter.

---

## 5. The negative control (and why the RED survives future edits)

`BFS-062-arms.sh noclassify` neuters exactly one thing: the classification. In
`classifyStreamRead`, `bufio.ErrTooLong` falls back to the transport classifier
again — the relation check, the reader sizing and every counter are untouched.
`BFS-062-control-noclassify.txt`:

```
pre-mutation: internal/fsclient/framelimit.go sha256=a57d33bf…
mutated:      internal/fsclient/framelimit.go sha256=3e48a309… via BFS-062-negative-control-classify.patch
--- CONTROL noclassify (this arm MUST fail):
    A LEGAL SERVER FRAME PUT THE MOUNT INTO THE RECONNECT LOOP: … reconnects_total=1 … cause=unreachable_connect
--- FAIL: TestBFS062ALegalOversizedFrameIsNamedCountedAndNotRetried
--- CONTROL noclassify (attribution: this arm MUST still pass): PASS (0.32s)
restore: internal/fsclient/framelimit.go sha256=a57d33bf… (verified against the pre-mutation hash)
```

So the cell can fail, it fails **for the right reason** (the transport
classification, and nothing else), and the attribution arm stays green. The
restore is a byte copy whose sha256 is re-checked after the copy back; a
mismatch aborts the run.

The RED is also kept reproducible rather than historical: the behaviour arms are
written against surface that existed **before** this row (the state's
mode/mechanism/reason/reconnects/last_failure, the callbacks, the served-stream
count) and spell the new cause as a literal, while everything that asserts
surface this row **adds** lives in `invalidate_bfs062_rule_test.go` — set aside by
the `unfixed` mode. A RED that needed yesterday's test text to reproduce would be
a weaker claim than it looks.

---

## 6. What this row did **not** do (and why)

- **It did not build the push endpoint** (BFS-036's deliverable) or the hot-file
  cache (BFS-037). The producer-side change is the **minimal** one at the only
  assembly site that exists today: the `events` poll form's ledger push, which is
  the single funnel every `eventLine` passes through. The push form, when it
  lands, must call the same predicate — `push` already applies it to the frames it
  emits, and the journal it fills is the one both mechanisms read.
- **It did not touch the cursor or the heartbeat logic** (BFS-061's territory).
  Nothing in this row reads or writes `seq`, `IdleTimeout`, `heartbeat_ms`, or the
  idle rule.
- **It did not widen a limit to make a test pass.** The consumer's ceiling moved
  from 8 MiB to 8 MiB + slack, and only because the relation (`cap ≥ declared +
  slack`) makes a ceiling that merely equals the maximum declaration unable to
  hold a conforming server. No arm was allowed to pass by choosing a bigger
  constant: the worst-case arm *requires* the worst frame to exceed the ceiling
  (96.0 MiB > 8.06 MiB) and fails if it does not.
- **It did not make the field mandatory.** `max_event_bytes` is additive in the
  capability document; a peer that does not publish it is reported as
  "not published" (with a reason) and the consumer falls back to its own ceiling —
  and an over-cap frame is *still* a named, counted, non-retried condition on that
  path, which is the part of the fix that does not depend on the negotiation.
- **It did not file board rows** (the brief forbids it) — see the residuals below.

## 7. Residuals, reported rather than smoothed over

1. **The events poll's response is not sized by `envelopeBudget`.** `handleEvents`
   answers the retained journal (up to 256 events) without consulting
   `DefaultMaxBytes`/`AbsMaxBytes`, so a busy tree's poll answer can be large. It is
   *not* this hole — no per-line reader is involved, so nothing loops — but the
   per-frame bound added here bounds each line, not the response. Needs its own
   row. *(Recommended to the driver: `pollEvents`/`handleEvents` should honour
   `envelopeBudget` and report `truncated` as the snapshot op does.)*
2. **The push form (BFS-036) must call the same predicate.** `eventFrameBytes`
   and `frameOverBound` are exported to the package for exactly that, and the
   capability document already publishes the bound — but until the push endpoint
   assembles a frame itself, the bound is enforced on the poll form's frames only,
   and a future push implementation could forget. The arms here pin the poll form;
   P-10/P-11 of the spec's test-property table should be extended to the push form
   when it lands.
3. **`ERRNO EFBIG`** is the errno this condition carries (`ErrnoEFBIG`:
   "the write exceeds a stated local bound"). It is the closest member of the
   existing vocabulary — the *cause* (`event_frame_over_limit`) is the precise
   name — but a reader who only looks at the errno sees the family, not the fact.
   A dedicated errno is not worth a wire/vocabulary change for a consumer-local
   verdict; it is named here so the choice is visible.
