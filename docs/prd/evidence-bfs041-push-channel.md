# EVIDENCE — BFS-041, the push channel wire form

**Spec delivered:** `docs/prd/SPEC-push-channel.md` (this row's only deliverable; no product code changed).
**Row:** BFS-041 · **Date:** 2026-09-27 · **Worktree:** `wt/BFS-041` · **Short note, per the brief's four asks.**

---

## (a) The choice, and the measurement it rests on

**The choice is the STREAM, and it is not a preference.** The measurement that decides it is not on the server
side at all — it is that **the client already speaks the stream wire form and nothing else**:

- `internal/fsclient/invalidate.go:350–420` sends `POST` + `X-Bunker-Op: watch` + `Content-Type: application/json`
  + `Accept: application/x-ndjson` with body `{"paths":[…],"since_seq":N}`, and decodes the body as **one JSON
  object per line**. That is E-6's declared shape, landed on the consumer.
- The stream deliberately escapes both client-side bounds — it uses `c.hc.Do(req)` directly, so it takes neither
  the 30 s `OpTimeout` nor one of the 25 in-flight semaphore slots (`invalidate.go:391–398` vs
  `client.go:276–300`, `:164`). Every other op holds a slot and dies at 30 s.

A bounded long-poll therefore produces exactly two behaviours on the consumer that exists, and both are silent
faults rather than errors:

1. an **envelope-shaped** answer unmarshals into the client's event type with unknown fields ignored
   (`Event:""`, `Seq:0`) and is **ignored** by `apply()` — the client reports `mode=push`,
   `mechanism=watch`, `channel_available=true` and never invalidates anything; and
2. an **answer-and-close** is read at the scanner's clean EOF as `nil`, which `Run` treats as *"clean close: the
   stream ended because we closed it"* and **returns** (`invalidate.go:282–293`, `:430–438`) — invalidation stops
   for the life of the mount, still reporting a healthy push channel.

So the long-poll is not merely a weaker mechanism here; on this consumer it is a **silent-corruption shape**, which
is the failure class the whole invalidation release exists to remove. The third possibility — hold and then stream
lines on one response — *is* the stream.

**Placement, verified rather than asserted.** The `/dav` subtree (and therefore every `X-Bunker-Op` handler) is
mounted **ahead of the chi router**, which is where `middleware.Timeout(server.request_timeout)` (300 s default)
lives (`server.go:153`, `:394–398`), and neither listener installs a socket deadline
(`server.go:469–478`, `:487–497`). I ran the landed control: **`TestWebDAVOpIsAnsweredAheadOfTheRPCTimeout`
PASS (0.15 s)** on this branch, whose harness drives a real router whose 50 ms deadline demonstrably refuses an
overrunning handler — so BFS-006's structural fact is still true and still testable.

The row asked for the timeout to be stated as an implementation **constraint**, and it is
(`SPEC-push-channel.md` §1.4):

- **C-1:** no deadline may govern this response — moving the channel behind the router, adding a listener
  `WriteTimeout`/`IdleTimeout` to the `/dav` listener, or implementing it as a connect RPC reintroduces the 300 s
  timeout BFS-006 removed, and it does so silently (the client sees a transport fault and reconnects, so the symptom
  is a reconnect every 300 s).
- **C-2:** the **client's own** deadline binds first — 30 s `OpTimeout` (`client.go:39–40`) — so a hold must be
  strictly shorter than it or it is a fault generator, not a slow mechanism.
- **C-3:** on h3 the connection's idle budget is quic-go's **30 s default** and nothing configures it
  (`h3.go:73–87`; `quic-go@v0.63.0/internal/protocol/params.go:94`; `http3/server_conn.go:59–63`), while
  `heartbeat_ms` is also 30 s: the connection can be reaped at the instant its first heartbeat is due. Either set
  `KeepAlivePeriod` or keep a strict margin.

---

## (b) The exact clause that forbids a silent gap

Two clauses, and they are the load-bearing sentences of the document (§3.3):

> **R-3 — no answer may stand in for a gap.** An empty tail is a statement that the interval the client asked about
> was observed and nothing moved. When that statement is not true, the server MUST NOT make it: it MUST answer
> `overflow` (a full resync, `paths: []`) and MUST NOT answer with an empty tail, a partial tail, or a tail whose
> first `seq` the client has no way to recognise as a gap.

> **R-4 — an `overflow` notice may never be emitted at a `seq` the presenting client's own rules can discard.**
> A client is correct to discard any line whose `seq` ≤ the cursor it presented. So when a presented cursor is
> **ahead** of the ledger's own `seq` — the restarted-server case — the ledger MUST advance its counter **past**
> the presented cursor **before** pushing the `overflow`. Emitting it at the ledger's own low `seq` produces an
> `overflow` the client silently drops, and a dropped `overflow` is a silent gap **the server believes it
> reported**.

R-4 is not invented here: it is exactly what the landed poll does (`events.go:294–304`, including the comment's
reasoning), which is the consistency the row asked for between the poll and the stream. What this row adds is that
the stream inherits it **by clause rather than by accident** — BFS-040's forced-resync table names the
client-behind-the-journal case (§5.2, §5.3) but not the ahead case, so an implementer working from BFS-040's prose
would not derive it.

The 4-case resume table (§3.2) is the operational form: inside the journal ⇒ the missing lines; exactly at head ⇒ a
heartbeat and then the new lines; **behind** ⇒ `overflow` **then** the retained tail (never the tail alone);
**ahead** ⇒ advance-then-`overflow` (R-4).

**And a case the design had not considered, which is why the "behind" row does not just copy the poll (H-10).** The
landed `events` op answers a cursor older than the retained journal with the retained tail alone, relying on the
client to notice the gap from `seq` monotonicity — but that check is guarded on a non-zero cursor
(`ev.Seq > i.seq+1 && i.seq != 0`, `invalidate.go:241–246`). A client whose cursor is `0` that receives a tail
starting at `seq 300` therefore sees **no gap** and treats the tail as complete coverage: a silent gap in the poll
form as landed, in the very case the row asked me to keep consistent. The stream is protected by an unconditional
marker (R-3), and the poll's own case is **reported, not fixed** — it belongs to BFS-026's owner, and this row owns
the channel's wire form rather than the poll op. That is hole H-10; the full list is ten (H-1…H-10, §10.2).

---

## (c) What the long-poll costs that the stream does not, and vice versa

| | stream | bounded long-poll |
|---|---|---|
| **client connection** | "occupies the single connection" on HTTP/1.1 — the mount needs one extra TCP connection, paid once per session (declared, `BFS-004` §4.3: 1589 ms unmultiplexed vs 278 ms multiplexed); on h2/h3 one stream among many | one of the client's **25** in-flight slots held for the whole hold (`client.go:33`, `:164`) — and it is the same pool file traffic uses, so idle mounts starve a busy one |
| **client deadline** | none applies (bypassed deliberately) | the client's own **30 s** `OpTimeout` caps the hold, so its bound is a slower poll rather than a hold |
| **round trips** | 1 per session + 1 per 30 s | 1 per bound, forever, on every idle mount (vs 1 per 2 s for the plain poll) |
| **server memory** | one bounded per-subscriber buffer (bytes **and** events, declared, counted) | the same waiter state, plus a parked timer per held request |
| **reconnect storms** | a stream end is a reconnect — controlled by §8's jittered backoff; the server must never end a stream on a schedule | every expiry is a client-side timeout, i.e. a transport verdict, so a mis-sized bound *is* the reconnect loop |
| **proxy / CDN behaviour** | survives any intermediary that delivers the response incrementally (headers + first line commit immediately, then a line per ≤30 s); **a buffering proxy defeats it** → the client's 90 s rule fires → declared degradation to poll, not an error | a hold is precisely "no response bytes", which is what a proxy's response timeout measures: Cloudflare's published behaviour answers `524` after ~100 s without a response (external, cited) |
| **the middlebox that kills idle connections** | answered head-on by the heartbeat: silence is never longer than the declared bound, so the deployment can be checked against it (rule D-2) | the hold *creates* silence the middlebox must be configured to tolerate — a requirement unverifiable from inside the process |

**Does the choice differ by deployment?** Kind: no. Margins: yes, and the rule is stated as **D-1/D-2** (§1.6): the
stream is used wherever the response reaches the client incrementally; a deployment that cannot make that true for a
client has no use for a long-poll either — it has a use for the **poll**, reached through the declared 90 s
degradation, with the mode change **reported**. Every deployment must be able to name the smallest idle/response
timeout on the client's path and keep `heartbeat_ms` below it. This is not hypothetical here: `internal/tunnel`
runs TryCloudflare/named `cloudflared` tunnels in front of agent ports, while the default mount path is loopback
(`http://127.0.0.1:18481/dav`) — so the untunnelled case is unconstrained and the tunnelled one is a declaration.

---

## (d) The boundary I did not cross

**Carried, never respecified — BFS-040** (`SPEC-watcher-capability.md`): I read it first and used it by reference
only. The seven `reason` values, the four `state` values, the refusal bytes, the probe matrix, the overflow
guarantee, §5.2's forced-resync table, §5.3's "resync ≠ quiet" and §5.4's O-1/O-2 are **quoted by section, never
restated, never extended**. Three additions are declared as additions, each to a *field* or a *lifecycle*, none
touching a `reason`, a `state`, an event name or a refusal code: §3.4's one-ledger/one-cursor rules (which
BFS-040 §6.7 explicitly hands to this row: "BFS-041 owns the consumer-side cursor rules"), §7.2's
`max_event_bytes` (an additive field in the block BFS-040 §8.2 fixes — no existing field redefined,
`document_version` stays 1), and §5's release paths for a dead subscriber.

**Named and not entered:**

- **BFS-042** (hot-file policy) — only the statement that the per-subscriber buffer and the hot-file tracker/queue
  are **separate bounds**, and that a refresh is never a correctness dependency. No scoring, sizes, queue-stop
  semantics or pool shares.
- **BFS-043** (server config) — I fixed the **relations** the knobs must satisfy (heartbeat vs the path's smallest
  idle timeout, heartbeat vs the QUIC idle budget, write deadline < heartbeat, the client read cap vs
  `max_event_bytes`) and asserted a few defaults; flag/knob **names, defaults, validation messages and bindings**
  are explicitly handed over.
- **BFS-045** (observability) — I named the counters this row's policies require (drops, gaps, disconnects by
  trigger, buffer high-water, active subscribers) and fixed the closed two-value release reason, then stopped:
  **record layout, naming style, aggregation, retention are BFS-045's**, and §11.1 says explicitly that what the push
  path adds to the client's existing mode/mechanism record is *at most* a cursor and the reconnect/idle counters.
- **BFS-046** (tests) — §12 lists **properties with the control that proves each can fail** as an interface, and
  states plainly that the harness, the program, the cells, the floor and the coverage target are not decided here.
- **BFS-026** (the landed poll) — untouched; F-1 says so and §3.4(3) is careful to describe what the *shared ledger*
  can put in a poll answer rather than proposing a change to the op.
- Also out of scope by the brief and by choice: the watcher implementation, the running deployment, any board row,
  `.gitreins/tasks.yaml`, and any code change.

**Concurrent work unaffected:** the row touched exactly one new file in its own worktree
(`docs/prd/SPEC-push-channel.md`) plus this note; nothing reformatted, no sibling's file edited, no board/state
file touched, no push. The only test executed was a landed, read-only unit test in this worktree.
