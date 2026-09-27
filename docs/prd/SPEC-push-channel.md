# SPEC: the push channel wire form

**Project:** bunker (owning) · **Row:** BFS-041 (P0, complexity 2) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-27
**Status:** proposed — spec of record for **how the change channel is carried**, and for the two contracts that
ride it: the resume cursor and the client's reconnect shape. **No product code changed by this row.**
**Deliverable:** this document, plus the wire form and the normative clauses of §3, §4, §6 and §7.
**Design authority (inherited by reference, not restated):** `PRD-bunker-invalidation.md` §1 (R3) and §2.1 — that
document is the *why*; the *event source* is `docs/prd/SPEC-watcher-capability.md` (BFS-040), which this document
**carries and does not respecify**; the *event names, the three declarations and the refusal bytes* are
`docs/spec/BFS-004-webdav-surface.md` §3 E-6, §4.2, §5.1–§5.3, §10.5.
**Event source contract (read first, owned elsewhere):** BFS-040 — §2 (what is served today), §3 (the `reason`
vocabulary), §5 (the overflow guarantee), §5.2–§5.4 (the forced-resync table, "resync ≠ quiet", and the two
obligations O-1/O-2).
**Implemented by (recommended, not decided here):** the row that lands the push form (BFS-036 or a successor) on
the server; `internal/fsclient/invalidate.go` on the client.
**Boundaries (named, not crossed — see §11):** BFS-042 (hot-file policy, queue stop semantics, pool share),
BFS-043 (server config knobs and defaults), BFS-045 (the observability record layout), BFS-046 (the test
program), and BFS-040's own surface (the watcher, its probe matrix, its vocabulary).

---

## 0. Verdict in one screen

| # | what the row asked for | the answer this document fixes | where |
|---|---|---|---|
| 1 | **the choice** — bounded long-poll or stream | **A stream.** Not as taste: the wire form is *already implemented on the client* (`POST` + `X-Bunker-Op: watch` + `Accept: application/x-ndjson` + `{paths, since_seq}`), and the two shapes a bounded long-poll would produce on that consumer are both silent faults — an envelope-shaped answer is **silently ignored** (no error, no drop, channel reported healthy), and an answer-and-close is read as a **clean close** that ends invalidation for the life of the mount (§1.2, H-1, H-2 of §10). | §1 |
| 2 | **where it sits, verified against BFS-006** | **Ahead of the chi router**, on the `/dav` subtree, because that is where the surface's op handler already lives (measured: the landed placement test passes on this branch). Consequence stated as an implementation constraint: nothing on this path may be a *request-shaped hold* that a deadline governs — the client's own `OpTimeout` is **30 s** and the router's is **300 s**, and a naive long-poll sits under both. | §1.4 |
| 3 | **what each shape costs** | Stream: one occupied connection on HTTP/1.1 (declared and measured, `BFS-004` §4.3), one stream on h2/h3, one per-subscriber buffer, and a liveness line **must** be written every ≤ 30 s. Long-poll: one of the client's **25** in-flight slots for the whole hold, a fresh round trip per expiry, and a bound smaller than the client's own 30 s op deadline — i.e. a poll with a lower cadence. | §1.5 |
| 4 | **resume, and never a silent gap** | A client presents the last `seq` it saw. Behind the retained journal ⇒ `overflow` first, then the retained tail (never the tail alone). Ahead of the ledger (restarted server / foreign cursor) ⇒ the ledger advances its own `seq` **past** the presented cursor **before** emitting `overflow`, so the client's duplicate rule cannot swallow the notice — the exact clause is §3.3. An empty answer is never a substitute for a gap. | §3 |
| 5 | **the bounded hold** | The hold is bounded by **`heartbeat_ms` (≤ 30 s, inherited)**: a line is written and flushed at the bound; it is a `heartbeat`, never an error and never a close. The channel is committed (headers + first line) on subscribe, so establishing it never waits for an event. | §4 |
| 6 | **the dead-client rule** | Two detectors, both required: `r.Context()` (cancelled when the connection closes) and a **write deadline on every line**, so the ≤ 30 s heartbeat is what *finds* a vanished client. On h3 the write deadline is **not available** through the response writer (measured) — the QUIC idle path and the context carry it instead. The release's not-hang standard applies here as it does to a FUSE op. | §5 |
| 7 | **backpressure** | A per-subscriber buffer (bytes **and** events, both declared). Full ⇒ **drop-oldest + exactly one counted `overflow` marker** at the subscriber's next write — never a coalesced or partial list. A write that cannot complete inside the write deadline ⇒ disconnect. Counters named in §6.3. | §6 |
| 8 | **max paths per event** | 4096 stays (inherited, and **aligned**: one frame, one event, one drop loop). A count does **not** bound a frame: 4096 × `PATH_MAX` ≈ 16.8 MiB exceeds the client's own 8 MiB line cap, so the wire form requires a declared **byte** bound (`max_event_bytes`). Over it ⇒ `overflow`, never a partial list. | §7 |
| 9 | **reconnect and backoff** | The client's shape is fixed: 500 ms × 2 to a 10 s cap **with full jitter** (the jitter is missing today, H-8), an implemented 90 s idle rule (declared but unwired today, H-3), **EOF is a reconnect and not a success**, and an absent endpoint is the **poll fallback** — not an error. | §8 |
| 10 | **the fallback is contractual** | `X-Bunker-Op: events` (BFS-026) stays and is unchanged; a stock HTTP/1.1 client that never sets `X-Bunker-Op` sees byte-identical behaviour; the mode/mechanism record keeps saying which mechanism delivered each change and the push path adds at most a cursor and a reconnect counter — the field list is BFS-045's. | §9, §10 |
| + | **holes found while writing this** | **Ten**, each named with evidence and an owner recommendation rather than smoothed over: a clean EOF that silently ends the channel (H-1), a heartbeat whose `seq` the client does not record and which therefore manufactures a **false gap on every change** (H-2), a declared 90 s liveness rule with **no implementation** (H-3), a frame bound by count where the consumer's is by bytes (H-4), `heartbeat_ms` equal to the QUIC default idle timeout (H-5), heartbeats versus the shared journal (H-6), a capacity refusal that must not look like a degradation (H-7), jitterless reconnect (H-8), the journal window a stream eats (H-9), and the poll's off-the-end resume being a **silent gap for a freshly-bound client** (H-10, reported to BFS-026's owner, not fixed here). | §10 |

---

## 1. The choice: a stream, and the measurements it rests on

### 1.1 What is already true (measured on this branch, not assumed)

| M | fact | value | how taken |
|---|---|---|---|
| M1 | the client already speaks the **stream** wire form | `POST` + `X-Bunker-Op: watch` + `Content-Type: application/json` + `Accept: application/x-ndjson`, body `{"paths": […], "since_seq": N}`; the response body is decoded as **one JSON object per line** | source read `internal/fsclient/invalidate.go:348–430` |
| M2 | and it deliberately escapes both client-side bounds, for the stream only | the stream uses `c.hc.Do(req)` directly instead of `c.do(...)`: no `OpTimeout` deadline, no in-flight semaphore slot | `invalidate.go:391–398` vs `client.go:276–300` |
| M3 | every other op holds one of **25** in-flight slots and is killed at **30 s** | `DefaultConcurrency = 25`; `sem = make(chan struct{}, opt.Concurrency)`; `do()` applies `context.WithTimeout(ctx, c.opt.OpTimeout)` when the caller's ctx carries no deadline; `OpTimeout` default 30 s | `client.go:33`, `:164`, `:278–284`, `:39–40` |
| M4 | the server's surface is mounted **ahead** of the router's 300 s deadline, and a live harness proves the deadline is real | `middleware.Timeout(server.request_timeout)` on the chi router (`request_timeout` default `300s`) and `rootHandler = mountWebDAV(rootHandler, …)` before it; `TestWebDAVOpIsAnsweredAheadOfTheRPCTimeout` **PASS** on this branch (0.15 s), whose control arm refuses a routed handler that outlives a 50 ms deadline with 504 | `server.go:153`, `:394–398`; test run `internal/server/webdav_mount_test.go:192–266` |
| M5 | neither listener installs a socket deadline that could reap a long response | both `&http.Server{}` literals set `Addr`, `Handler`, `TLSConfig`, `Protocols`, `DisableGeneralOptionsHandler` and **no** `ReadTimeout`/`WriteTimeout`/`IdleTimeout` (zero = no limit) | `server.go:469–478`, `:487–497` |
| M6 | the h3 side's idle budget is **30 s by default** and nothing here configures it | `http3.Server{Addr, Port, TLSConfig, Handler, Logger}` — no `IdleTimeout`, no `QuicConfig`; quic-go's default idle timeout is 30 s and its HTTP-level idle timer is armed **only when** `IdleTimeout > 0` | `h3.go:73–87`; `quic-go@v0.63.0/internal/protocol/params.go:94` (`DefaultIdleTimeout = 30 * time.Second`); `http3/server_conn.go:59–63` |
| M7 | the declared channel cadence and the client's liveness rule already exist | `heartbeat` at least every 30 s, declared as `extensions.watch.heartbeat_ms`; the client switches to poll after **90 s** of silence (three missed heartbeats) | `BFS-004` §3 E-6 decl. 1, §4.2; `BFS-005` §4.4; `invalidate.go:93–94` |
| M8 | the conn-close cancellation the dead-client rule leans on is a documented runtime fact, not a hope | "For incoming server requests, the context is canceled when the client's connection closes, the request is canceled (with HTTP/2), or when the `ServeHTTP` method returns." | `$(go env GOROOT)/src/net/http/request.go:349–351` |

### 1.2 Why not the bounded long-poll — three shapes it would produce on this consumer

A bounded long-poll is a request that the server holds open for at most *N* seconds and then answers. Run it through
the consumer that already exists (M1) and it produces one of three outcomes, and only the third is tolerable:

1. **It answers an envelope.** The landed decoder treats every line as an E-6 event object: `json.Unmarshal(line, &ev)`
   succeeds on an E-4 envelope (unknown fields are ignored), yielding `Event: ""`, `Seq: 0` — which `apply()` falls
   through as *"an unknown event name is not a resync"* and **ignores** (`invalidate.go:414–428`, `:265–270`). The
   client has `mode=push`, `mechanism=watch`, `channel_available=true`, and **never invalidates anything**. That is not
   a degraded channel; it is a healthy-looking channel that silently carries no change — the exact class this release
   exists to remove (`PRD` §2.1).
2. **It answers and closes.** `watchOnce` returns `nil` at EOF, and `Run` treats `nil` as *"clean close: the stream
   ended because we closed it"* and **returns** (`invalidate.go:292–293`, `:430–438`). Invalidation stops permanently
   for the life of the mount, with `mode=push`, `mechanism=watch`, `available=true` still reported. A bounded
   long-poll is a design whose every successful answer is this case.
3. **It holds, then streams lines on one response.** That is not a bounded long-poll; that is the stream.

A fourth variant — a *new* client mechanism that speaks a long-poll protocol — is available, and it costs the thing
this release is paying to avoid: a second consumer path over one ledger, with its own resume rules, its own failure
shapes, and no way for `BFS-005` §4.4's declared fallback ladder (`watch` → `events` → `rev`) to stay a ladder.
M1–M3 say the marginal return is small anyway: through `c.do()` the hold is capped by the client's own 30 s op
deadline and occupies one of 25 slots; through a stream-style bypass it is the stream minus the framing it needs.

**Verdict: the stream.** The bounded long-poll is not implemented, and the two mechanisms that *are* contractual
(the stream, and the bounded-return poll of BFS-026) already cover every case a third one would.

### 1.3 What the poll is for, so that "we chose the stream" is not read as "the poll is obsolete"

The poll stays, and it is not a degraded relative of the stream: it is the right mechanism for a caller that cannot
hold a connection (a one-shot CLI, a stock WebDAV peer, a shell script, `BFS-004`'s WebDAV-only client), and it is the
declared degradation for every one of BFS-040's seven `reason`s. Its measured cost is the thing the stream removes:
**7.3 ms median for 2000 paths** per poll, O(paths) per call, on a declared 2 s interval (`PRD` §0; `invalidate.go`
`DefaultPollInterval`). The stream's cost is O(changes) plus one liveness line per 30 s.

### 1.4 Where the stream sits — verified, and stated as an implementation constraint

**Verified on this branch (M4):** the `/dav` subtree, and therefore every `X-Bunker-Op` handler on it, is mounted
**ahead of the chi router**, which is where `middleware.Timeout(server.request_timeout)` lives. BFS-006 recorded this
so that the 300 s RPC timeout could not kill a stream; BFS-026 recorded that the same structural fact is what makes
the *poll* op safe; the landed test named in M4 keeps that from becoming prose — it drives a real router with a real,
demonstrably-live deadline and shows the op path never passes through it.

**Therefore the placement is already correct, and the row's real deliverable is the constraint that keeps it correct:**

> **C-1 — no deadline may govern this response.** A stream on the `/dav` op path is governed by no request
> deadline, by construction (M4) and by the absence of socket deadlines (M5). Any implementation that (a) moves the
> channel behind the chi router, (b) adds a `ReadTimeout`/`WriteTimeout`/`IdleTimeout` to the listener that serves
> `/dav`, or (c) implements the channel as an RPC on the connect surface, **reintroduces the 300 s timeout BFS-006
> removed** — and it will do so silently: the client classifies the resulting 504/timeout as a transport fault and
> reconnects (with a cursor), so the observable symptom is a reconnect every 300 s rather than an error.

Two further deadline facts a naive long-poll would collide with, both measured, both worth stating because neither is
in BFS-006's note:

> **C-2 — the client's own deadline is 30 s, and it binds before the server's 300 s** (M3). A hold placed behind
> `c.do()` is killed by the *caller*, and the client sees its own timeout, not the server's. A long-poll whose bound
> is not strictly below the client's op timeout is therefore not merely slow — it is a fault generator.
>
> **C-3 — on h3 the connection's idle budget is 30 s (M6), which is exactly the declared heartbeat period.** The
> first heartbeat is due at the instant the QUIC layer may declare the connection idle. The implementation MUST
> either set `http3.Server.QuicConfig.KeepAlivePeriod` (quic-go's own keepalive interval is `min(KeepAlivePeriod,
> idleTimeout/2)`) or emit heartbeats with a **strict margin** below the QUIC idle timeout — and if it changes
> `heartbeat_ms` it must change `extensions.watch.heartbeat_ms` with it, because that is the number the client's
> 90 s rule is derived from (M7). See H-5.

**Handed to BFS-043 (not decided here):** whether `heartbeat_ms`, the write deadline and the QUIC keepalive are
knobs, their names, and their validation. This document fixes only the **relations** they must satisfy (§4.4, §5.3).

### 1.5 What each shape costs, in the terms the row asks for

| cost axis | stream (this row's choice) | bounded long-poll | poll (BFS-026, contractual) |
|---|---|---|---|
| **per-connection memory, server** | one subscriber struct + one bounded buffer per subscriber (§6.1); the buffer bound is declared and counted, and the worst case is `subscribers × buffer` — the number BFS-045 must be able to report | the same subscriber state (the hold still needs a waiter), **plus** a goroutine parked on a timer per held request | none between calls: the ledger holds the diff baseline, the client holds the cursor |
| **connection occupancy, client** | HTTP/1.1: the stream **occupies the single connection**; the mount needs one extra TCP connection, paid once per session (declared in `BFS-004` §4.3; 1589 ms unmultiplexed vs 278 ms multiplexed). h2/h3: one stream among many | HTTP/1.1: one of the 25 in-flight slots is held for the whole hold (M3) — and it is the *same* pool the file traffic uses, so a fleet of idle mounts starves a busy one | one slot for one call, then released |
| **round trips** | 1 per session + 1 per 30 s | 1 per bound (≈ 1 per 5–25 s, depending on the bound chosen) forever, on every idle mount | 1 per 2 s |
| **reconnect storms** | a stream end is a reconnect; **the shape that must be avoided is a stream the server ends on a schedule** — §8's backoff with jitter is what keeps N mounts from returning in lockstep, and the heartbeat is what stops a middlebox from ending it | the same exposure, times the number of expiries; plus the client's `default:` branch (a transport-shaped failure) is entered on **every** timeout, so a mis-sized bound is a reconnect loop with extra steps | a failed poll does not change mode (`pollLoop` retries on the next tick) — inherently storm-resistant |
| **proxy / CDN** | works wherever the response is delivered incrementally: headers + first line commit immediately, then a line per ≤ 30 s, so a proxy that streams chunked responses is fine and its idle timers never see silence. A proxy that **buffers** (the `proxy_buffering on` shape) defeats it: the client sees no line, its 90 s rule fires, and it falls back to poll — a declared degradation, not an error (§8.4) | a hold is a request with no response bytes at all, which is precisely what a proxy's response timeout measures: Cloudflare's published proxy behaviour is to answer 524 after ~100 s without a response, and its origin idle limit reaps reused TCP connections (external; cited in App. A) | one fast request; nothing to buffer |
| **the middlebox that kills idle connections** | answered by the heartbeat: silence is never longer than the bound, and the bound is declared, so a deployment whose middlebox idles faster than 30 s is **mis-declared** rather than mysteriously broken (§4.4) | the hold *creates* silence, so every middlebox in the path must be configured to tolerate it — a requirement the deployment cannot check from inside the process | no silence at all |
| **failure to the client** | ends as EOF/reset → §8's reconnect or fallback, with the cursor preserved | ends as a timeout inside the client's own op path, i.e. a transport verdict, and (per §1.2 case 2) a correct answer looks like a **clean close** | ends as an envelope; a quiet answer is a quiet answer |

### 1.6 Does the choice differ by deployment?

**Kind: no. Margins: yes.** The stream is the mechanism; what varies is whether it can be *delivered incrementally*
and whether its liveness line arrives faster than the path's idle timeout. The rule, stated so an implementer and an
operator can both apply it:

> **D-1 — the stream is used wherever the response reaches the client incrementally.** The test is not "is there a
> proxy" but "is a line delivered before the next line is written": headers + the first line commit at subscribe, and
> a heartbeat is written every `heartbeat_ms`. A deployment that cannot make that true for a given client (an
> intermediary that buffers responses and cannot be told not to) has **not** got a use for a long-poll either — it
> has a use for the **poll**, and the client reaches it by the declared 90 s rule (§8.4) with the mode degraded and
> **reported**.
>
> **D-2 — every deployment must declare the smallest idle/response timeout on the client's path** (proxy, tunnel,
> NAT, load balancer) and must choose `heartbeat_ms` strictly below it, subject to the declared 30 s ceiling. A
> deployment that cannot answer "what is the smallest idle timeout between the mount and the surface?" cannot claim
> the push channel works over that path; it can claim the poll does.

The deployment in this repository's own tree is the reason this is not hypothetical: `internal/tunnel` starts
TryCloudflare and named `cloudflared` tunnels in front of agent ports, and a tunnelled path is exactly the case where
D-2 has to be answered rather than assumed. The default mount path is **not** that case — the surface root is a local
endpoint (`http://127.0.0.1:18481/dav` is the documented form, `internal/fsmount/options.go:54`,
`internal/cli/fs.go:76`) — so the loopback case is unconstrained and the tunnelled case is a deployment declaration.

---

## 2. The wire form (fixed here)

The request and response shapes are `BFS-004` §3 E-6 and §10.5's, already implemented on the client (M1), and this
document **pins** them rather than inventing them. Two things E-6 does not fix and this document must:

```http
POST /dav/ HTTP/1.1
X-Bunker-Op: watch
Content-Type: application/json
Accept: application/x-ndjson

{"paths":["src"],"since_seq":41}
```
```http
HTTP/1.1 200 OK
Content-Type: application/x-ndjson
X-Bunker-Verdict: ok
X-Bunker-Tree: <token>

{"seq":42,"event":"invalidate","paths":["src/main.go"],"rev":"git:9f2c1a…","tree":"<token>"}
{"seq":43,"event":"heartbeat","paths":[],"rev":"git:9f2c1a…","tree":"<token>"}
```

**W-1 — framing discipline (this surface has no streaming writer today).** The response is NDJSON and it is
*incremental or it is nothing*: the server writes the headers, then the first line, and **flushes**; every subsequent
line is written and flushed individually. `net/http` buffers a response in a `bufferBeforeChunkingSize = 2048`-byte
writer (`$(go env GOROOT)/src/net/http/server.go:341`), so a handler that never flushes can hold up to 2 KB of
lines — including every heartbeat — and the client's 90 s rule then declares a *healthy* channel dead. This is
already an obligation in `BFS-005` §4.1 ("each event is written and **flushed as its own line**"); what is new here
is the measurement that makes it non-obvious: **no file in `internal/server/webdav/` or `internal/fsclient/` calls
`Flush()` today** (`grep` over both packages: zero hits), so the stream is the first incremental response this
surface will ever produce and the discipline cannot be inherited from a neighbour.

**W-2 — commit the channel on subscribe, do not wait for an event.** The response headers and a first line are
written **immediately** (a `heartbeat` if no change event is pending). The client's establish step blocks inside
`c.hc.Do(req)` until the response headers arrive (`invalidate.go:400–404`), so a server that waits for the first
event before committing the response makes the *bind* hang on a quiet tree — and a quiet tree is the normal case.

**W-3 — the subscription's `paths` is a filter whose failure mode is asymmetric.** Over-reporting is safe
(the client's drop of a path it does not hold is a no-op); **under-reporting is a silent-corruption machine**. So a
server that applies `paths` must apply it to *every* line or to none, and the capability document must say which
(`extensions.watch.subscription: "filtered" | "whole_tree"`). A server that scopes its events but touches only part
of the scope is reporting a healthy channel that is quietly missing changes — BFS-040's W-4 arriving through a second
front door, and it must be reported there (as `watch_partial_coverage` for the *watch set*) rather than hidden here.

**W-4 — `Accept: application/x-ndjson` is not a negotiation.** A client that omits it still gets the NDJSON body,
because the client's own decoder is line-oriented and E-6's response shape is not version- or header-dependent
(`BFS-004` C-1: nothing may be refused because of the client's protocol). A server that switches to an envelope shape
because the header is absent produces §1.2 case 1 on a stock client.

---

## 3. RESUME SEMANTICS — never a silent gap

### 3.1 The cursor is the ledger's `seq`, and it advances on **every** line received

`since_seq` names the last line the client saw. It is the **same ledger and the same counter** the poll form uses
(`events.go:117–124`, `t.eventLedger()`), which is why the poll and the stream cannot disagree about what has
happened — and why the cursor must be defined identically on both: **the highest `seq` the client has observed,
whatever the line's `event` name was.** The client stores it in `Invalidator.seq`.

> **R-1 — the cursor advances on every line, heartbeat included.** The landed client returns from `apply()` for a
> `heartbeat` **before** the seq bookkeeping (`invalidate.go:219–224` vs `:241–253`), so the heartbeat's `seq` is
> never recorded. The consequence is a manufactured gap on the next real event: heartbeat at `seq 43` leaves the
> cursor at 42, the next `invalidate` at `seq 44` satisfies `ev.Seq > i.seq+1`, and the client **resyncs** — every
> time, forever. Correctness survives (a resync is always safe) and the whole point of the row does not: the mount
> re-snapshots the tree it was told not to re-walk. This is H-2 and it is the client's half of this contract.

### 3.2 The four resume cases (normative; one rule, four arrivals)

| presented cursor | what the server answers | why | what the client does |
|---|---|---|---|
| **inside the retained journal** (`held.base ≤ cursor < held.seq`) | every retained line with `seq > cursor`, oldest first | the range is vouched for by the ledger itself | apply in order; no resync unless a gap appears inside the range |
| **exactly at head** (`cursor == seq`) | a `heartbeat` (the channel's first line, §2 W-2) and then the new lines as they happen | "caught up" and "dead" must be distinguishable, which is the heartbeat's second job | nothing |
| **behind the retained journal** (`cursor < held.base`) | an **`overflow` first**, then the retained tail (whose first `seq` is a gap) | the interval between the cursor and `base` was never retained: it is *unknown*, and an unknown interval is never reported as quiet (BFS-040 §5.1) | resync once; advance past the reported `seq`; **never** re-request the missing range (BFS-005 §4.1) |
| **ahead of the ledger** (`cursor > held.seq` — a restarted server, or a cursor minted against another instance) | the ledger **advances its own `seq` to the presented cursor**, then emits `overflow` | the range was never observed here; and the notice must survive the client's own duplicate rule — see §3.3 | resync once; the `overflow` is the first line it can act on |

The poll form implements the same four cases on the same ledger (`events.go:267–311`) — including the two the row
asks to keep consistent: **the first observation is an `overflow`** (`:267–275`) and **a cursor ahead of the ledger
advances the counter first** (`:294–304`). The client's conclusion is identical in all four. The one **difference in
mechanism** is deliberate and stated so nobody "harmonises" it away:

> **R-2 — on a resume that falls off the end, the stream emits an explicit `overflow` BEFORE the retained tail,
> where the poll relies on the client's own gap detection.** The two agree for a client that has already applied an
> event; they do **not** agree for a freshly-bound client, because the landed gap rule is guarded on a non-zero
> cursor (`invalidate.go:241–246`: `ev.Seq > i.seq+1 && i.seq != 0`). A client whose cursor is `0` that receives a
> retained tail starting at `seq 300` sees **no gap**, applies the tail, and treats it as complete coverage. The
> stream must not be able to produce that answer, hence the unconditional marker. Whether the poll is changed is
> BFS-026's owner's call — it is reported here as H-10 rather than fixed by this row.

### 3.3 The clause that forbids a silent gap (normative)

> **R-3 — no answer may stand in for a gap.** On this channel, an empty tail is a statement that the interval the
> client asked about was observed and nothing moved. When that statement is not true, the server MUST NOT make it:
> it MUST answer `overflow` (a full resync, `paths: []`, per BFS-040 §5) and it MUST NOT answer with an empty tail,
> a partial tail, or a tail whose first `seq` the client has no way to recognise as a gap.
>
> **R-4 — an `overflow` notice may never be emitted at a `seq` the presenting client's own rules can discard.**
> A client is correct to discard any line whose `seq` is ≤ the cursor it presented (`invalidate.go:241–246`). So when
> a presented cursor is **ahead** of the ledger's own `seq` — the restarted-server case — the ledger MUST advance its
> counter **past** the presented cursor **before** pushing the `overflow`, so the notice's `seq` is strictly greater
> than the cursor and cannot be read as a duplicate. Emitting it at the ledger's own low `seq` produces an `overflow`
> the client silently drops, and a dropped `overflow` is a **silent gap that the server believes it reported**.

R-4 is not a new rule invented here: it is the rule the landed poll already implements (`events.go:294–304`, whose
comment states the reasoning), and it is the reason the poll cannot silently skip a range after a restart. This
document's contribution is that the stream inherits it **by clause, not by accident** — an implementer writing the
stream's resume path from BFS-040's prose alone would not derive it, because BFS-040's forced-resync table lists
"cursor older than the retained journal" (`§5.2`) and "client behind the journal" (`§5.3`) but not the ahead case.

### 3.4 The heartbeat's `seq`, the journal, and the one-ledger rule

Three consequences of the cursor being shared between two mechanisms, all of which must be decided rather than
discovered:

1. **Heartbeats consume a `seq`.** They are lines in a monotone stream (`BFS-004` §10.5 numbers one), and R-1
   requires the client to advance over them. A server that emits heartbeats **outside** the counter (e.g. `seq: 0`)
   breaks the client's gap detection, because `apply()` skips the seq logic entirely for `Seq <= 0` — the client
   would then never see the gap a real loss creates.
2. **Heartbeats are journaled, or the two mechanisms' cursors cannot be compared.** If heartbeats consume a `seq` but
   are not retained, a reconnect (or a mechanism switch) presents a cursor and receives a first line whose `seq` is
   more than one ahead, with no way to tell "heartbeats passed" from "events were lost" — and the only safe reading
   of that ambiguity is a resync. Retaining them costs journal slots (H-9, §10) and buys an unambiguous cursor. This
   document fixes **retained**, and names the cost.
3. **The poll does not *generate* heartbeats; it may *carry* them.** A heartbeat exists to keep a stream alive
   (`events.go:62–63` is right about this), so a tree with no stream subscriber produces none. But one ledger serves
   both mechanisms: while a stream is open on a tree, a **polling** client on the same tree will occasionally receive
   a `heartbeat` line in its `result.events` array. That is legal and must be stated — the client's `apply()` handles
   it identically (`:219–224`) — and it must not be "fixed" by filtering, because filtering it would break the
   client's cursor monotonicity for the next client that switches mechanism.

> **R-5 — the `seq` space is per tree, shared by both mechanisms, and every line that advances it is written to
> every attached subscriber.** An implementation that gives each subscriber its own counter, or its own heartbeat
> cadence, makes the same tree state produce different `seq`s for different clients; a client that ever changes
> mechanism then reads its own history as a gap. One counter, one cadence, broadcast.

### 3.5 What the cursor does *not* survive

- **A tree identity change.** `X-Bunker-Tree` (and the per-line `tree`) is an identity, not a counter (BFS-040
  R-V5). A cursor minted against tree *A* is void against tree *B*: the client already treats a per-line mismatch as
  a resync (`invalidate.go:227–231`), and the stream response header is checked at establish (`:406–414`).
- **A server restart, silently.** The counter restarts at 0 while `X-Bunker-Tree` is unchanged (the token is derived
  from the served root's path/dev/inode, not from the process — BFS-040 §7.1 D3). That is precisely the case R-4
  covers: the client's cursor is ahead of the new ledger, and the answer is an `overflow` it cannot drop.
- **The journal bound.** `eventsJournalEvents = 256` (inherited, BFS-040 §2.2). Anything older than the retained
  window is *unknown*, which is R-3's case, not a slow page-back.

---

## 4. THE BOUNDED HOLD

### 4.1 What the bound is

The channel is a stream, so there is no "hold and answer" bound; there is a **maximum silence** the server may
produce on an open stream:

> **B-1 — `heartbeat_ms`, ≤ 30 s (declared, inherited).** While a subscription is open, the server writes and flushes
> a line at least every `heartbeat_ms`. If no change event is pending, that line is a `heartbeat`. The bound is
> declared in the capability document (`extensions.watch.heartbeat_ms`) so a client adapts to a slower server instead
> of declaring it dead — that is `BFS-004` §3 E-6 decl. 1, and it is not restated here.

> **B-2 — the subscribe bound.** The response is committed (headers + first line) immediately (§2 W-2), so the first
> line arrives within one round trip, not within one heartbeat period. A channel that is open but has produced **no
> line at all** is not a quiet channel; it is a channel that has not started, and the client's establish step is
> still waiting.

### 4.2 What happens AT the bound

An **empty heartbeat** — one NDJSON line, `event: "heartbeat"`, `paths: []`, carrying `seq`, `rev` and `tree` like
every other line. Explicitly **not**:

- not an error (no 5xx, no verdict, no `envelope` — the response has already been committed);
- not a close (a server that closes at the bound converts every quiet tree into a reconnect; §1.5's storm axis);
- not a change claim. A heartbeat carries **no path claims** and must never be read as "nothing changed"
  (`BFS-004` §3 E-6's own words; `invalidate.go:29–31`). It bounds *liveness*, not *quietness*.

### 4.3 Why a heartbeat is required at all

Because **the client's silence is ambiguous by construction, and the ambiguity is what this release forbids**:

1. **Quiet vs dead.** With no heartbeat, a healthy idle tree and a reader stalled on the §5.4 O-1 chain produce the
   identical client-visible state: no line for N seconds. `BFS-040` §5.4 O-2 makes producing a distinguishable
   liveness signal an **obligation** on the source ("liveness must be produced by the watcher's own loop … a quiet
   channel must never be reported as healthy"), and `BFS-005` §4.4 turns it into the client's 90 s rule (three missed
   heartbeats). The declared period is the number that makes the rule sound.
2. **It is also the dead-client detector** (§5): the heartbeat is the write attempt that finds a client which
   vanished without closing.
3. **It is the middlebox keep-alive** (§1.6 D-2): the only traffic that keeps an idle path open.

> **B-3 — a heartbeat is not a substitute for a change.** A server may not use heartbeats to claim an interval was
> quiet. Only the observable "nothing moved" answer does that, and only the watcher can produce it (BFS-040 §5.1).

### 4.4 The declared relations (the numbers BFS-043 configures, the relations this document fixes)

| relation | why |
|---|---|
| `heartbeat_ms ≤ 30000` | inherited declaration; the client's 90 s rule is derived from it (M7) |
| `heartbeat_ms < the smallest idle timeout on the client's path` | otherwise the channel is reaped by infrastructure the server cannot see, and the failure looks like a flaky stream (§1.6 D-2) |
| `heartbeat_ms` strictly below the QUIC idle timeout, **or** `KeepAlivePeriod` set on the h3 server | they are both 30 s today (M6, C-3): without this, an idle h3 connection can be declared dead at the same instant its first heartbeat is due |
| `push_write_deadline < heartbeat_ms` | so a stalled subscriber is detected within one heartbeat period rather than accumulating (§5.3) |
| a changed `heartbeat_ms` changes `extensions.watch.heartbeat_ms` in the same release | a client that has read a 5 s period but receives 30 s updates declares the channel dead at 15 s |

---

## 5. THE DEAD-CLIENT RULE

### 5.1 The standard

> **B-4 — no request is held open on a client that will never read.** The release's not-hang standard — stated for a
> FUSE op in the invalidation PRD's R10/§2.8 family — applies here with the same force: a subscription whose client
> has died MUST be detected and released, and MUST NOT leave a goroutine, a buffer, or a connection allocated for the
> life of the process. "The client will eventually go away" is not a bound.

### 5.2 How a dead client is detected

Two **independent** detectors, both required, because they fail differently:

1. **The request context.** The runtime cancels the request context when the client's connection closes (M8, quoted
   from the stdlib). This catches every client that closes or resets its socket — including one that closes politely
   and one that is killed.
2. **The write deadline on every line.** A client that *vanishes without closing* (NAT drop, power loss, a suspended
   VM, a SIGKILLed process on another host) sends no FIN and no RST: the connection looks open forever, and the
   server's writes are absorbed by the TCP buffers until they are not. Detection is therefore a property of
   **writing**: each line is written under a write deadline, and a write that cannot complete within it means either
   a dead peer or a subscriber that will not drain. The heartbeat (§4) is what guarantees the write attempt happens
   within one bounded period rather than "when the next change arrives".

```
every line:  rc := http.NewResponseController(w)
             rc.SetWriteDeadline(now + push_write_deadline)
             write + flush; on timeout ⇒ dead-client path (§5.3)
```

> **B-5 — a stalled *reader* and a dead *client* are the same detection and different policies.** The write deadline
> fires in both cases; §6's buffer policy handles the slow-but-alive subscriber **before** the socket blocks, so a
> write timeout means the client is not reading. The two counters are distinct (§6.3) precisely so that this claim is
> checkable rather than asserted.

### 5.3 What the server does, and the h3 gap

- **Release everything**: cancel the subscription, write the close (a `GoAway`-free ordinary close of the
  connection/stream is enough — the client sees EOF, which §8 fixes as a *reconnect*, not a success), free the
  per-subscriber buffer, decrement the subscriber count, increment the counter.
- **Do not** queue a `bye` line for a client that is not reading; the buffer is evidence, not an obligation.
- **Log the transition**, not the byte traffic: one line per released subscriber, with the trigger
  (`ctx` vs write timeout) and the counters at release. This is the observability handoff (BFS-045 owns the record;
  this document fixes that the *release reason* must be one of a closed set of two).

> **B-6 — on HTTP/3 the response writer does not support a write deadline** (measured: quic-go `v0.63.0`'s `http3`
> package exposes `SetWriteDeadline`/`SetReadDeadline` on QUIC *streams*, but no non-test file in it implements the
> `interface{ SetWriteDeadline(time.Time) error }` that `http.ResponseController` looks for — unlike `net/http`'s h1
> and h2 writers, which do). On h3 the dead-client detection is therefore carried by the **request context** and by
> the QUIC layer's own idle/ping machinery (M6) — the QUIC idle timeout is a bounded detector, but it is the
> *connection*'s, not this line's, and its default is 30 s. An implementation that relies on `ResponseController` for
> a hard bound must not claim the same bound on h3, and BFS-046 should not write a cell that assumes it.

### 5.4 The relation to the subscriber cap

A release is not the only defence: if the server is at its subscriber cap, a new subscriber must not be admitted at
all (§6.4). The dead-client rule is what makes the cap recoverable — slots come back when dead clients are detected,
which is why "cap" cannot be an alias for "leak".

---

## 6. BACKPRESSURE

### 6.1 The per-subscriber buffer

> **B-7 — every subscriber has its own bounded buffer, bounded in BOTH bytes and events, and both bounds are
> declared and countable.** Defaults asserted here (names and configurability are BFS-043's): `subscriber_buffer_bytes
> = 4 MiB`, `subscriber_buffer_events = 256` — the event count chosen to equal the journal bound (`256`, inherited),
> because a subscriber that has fallen a whole journal behind is going to resync anyway and paying memory for events
> it can no longer deliver in order buys nothing.

The bound exists because the alternative is unbounded memory growth driven by a client, and because the *source* of
the events cannot block on a subscriber:

> **B-8 — the watcher never blocks on a subscriber.** The library's own chain is the reason (BFS-040 §5.4: a slow
> consumer blocks the fsnotify reader, the kernel queue fills, and the watcher goes silent while looking alive). A
> subscriber write is therefore never performed on the watcher's read loop and never with unbounded blocking: the
> loop appends to the buffer, and a per-subscriber writer goroutine drains it. This is a **constraint on the
> implementation**, not a hint: any design in which "deliver to subscriber N" can block the source is non-compliant
> with BFS-040's O-1 and with this section.

### 6.2 The policy when a subscriber cannot keep up

> **B-9 — drop-oldest, with one counted `overflow` marker.** When appending a line would exceed either buffer bound,
> the server discards the **oldest** buffered lines until the new line fits, and ensures that the **next line it
> writes to that subscriber is an `overflow`** (`paths: []`, the ledger's current `seq`) — which forces the client to
> re-snapshot, per BFS-040 §5. If the dropped run is still open when more lines are dropped, the server does **not**
> emit a second marker: one marker per gap run, so a subscriber that is permanently behind produces a **stuck**
> marker (one `overflow`) rather than a marker flood in the buffer that is already full.
>
> **B-10 — never coalesce, never truncate a list, never present a partial list as complete.** Merging two pending
> `invalidate` events into one path set is forbidden: the `seq` numbers it consumes are the client's only evidence
> that lines are missing, so a coalesced change is a change the client will never fetch *and* a gap it cannot see.
> Emitting a truncated `paths[]` with the surviving entries is forbidden for the same reason (it is the shape
> BFS-040 §5.1 claim 2 forbids at the source: overflow, never a partial list).

> **B-11 — a write that cannot complete inside the write deadline disconnects** (§5.3), and that is a *different*
> counter from a buffer drop. The rule of thumb an implementer needs: **the buffer policy handles "slow", the write
> deadline handles "gone"** — and if a subscriber is both (dead AND was behind), the disconnect wins and the drop
> counter retains what it counted.

### 6.3 The counters (named here; layout and drill-down are BFS-045's)

| counter | meaning | why it must exist |
|---|---|---|
| `subscriber_drops_total` | lines discarded by B-9, summed over subscribers | the "drop-oldest" half of the policy is invisible without it |
| `subscriber_gaps_total` | `overflow` markers emitted because of B-9 | the *counted gap* the policy promises: one number per delivered resync-forcing marker |
| `subscriber_disconnects_total` | subscribers released by §5.3, labelled by trigger (`ctx` vs write timeout) | separates "dead client" from "client that stalled" |
| `subscriber_buffer_high_water_bytes` | the largest buffer occupancy seen (per subscriber, maximum over subscribers) | proves the bound is near its limit *before* it is hit; a bound nobody can see is not a bound (`PRD` §2.7) |
| `subscribers_active` (+ its maximum) | currently attached subscribers | the multiplier in the memory claim: `subscribers × buffer` |

BFS-040 §8.2's counter set (server-side, in the capability document) and this table are **disjoint by design**: those
count what the *watcher* lost, these count what the *channel* dropped. A single number covering both would make
"the watcher is healthy and the channel is dropping" indistinguishable from the reverse — the failure mode this
release keeps finding.

### 6.4 The subscriber cap, and the refusal that is not a degradation

> **B-12 — a declared maximum subscriber count** (`max_subscribers`, per tree; the value is BFS-043's) with a
> declared worst-case memory (`max_subscribers × subscriber_buffer_bytes`) that BFS-045 can report. Refusing is the
> honest behaviour at the cap; silently degrading (shrinking buffers per subscriber, or dropping the oldest
> subscription) is not, because it changes a bound a client was promised.

The refusal shape is the interesting part, and it must not reuse BFS-040's vocabulary:

> **B-13 — a capacity refusal is RETRYABLE and must not look like a degradation.** It must not carry
> `capability_unavailable`, must not carry `mode=poll` in `X-Bunker-Capability`, and must not be read by the client
> as evidence that push is unavailable — a client that reacted to it by downgrading would abandon a mechanism that
> will be available in seconds. The recommendation, which fits the client's landed branch structure with **no new
> vocabulary**: answer `429` with `Retry-After` and **no** `X-Bunker-Verdict`/`X-Bunker-Capability` header, so the
> client's `default:` branch (any non-capability failure) sends it to the reconnect loop with the cursor
> (`invalidate.go:314–330`) — retry, keep push, no downgrade.
>
> **B-14 — and it must not count toward the three-strikes downgrade.** `BFS-005` §4.4 item 2 switches to poll after
> 3 consecutive non-transport failures. A capacity refusal is not evidence about the capability, so it must not be
> counted as one of the three; if the client implements that rule as "any non-transport error", the cap becomes a
> downgrade trigger — which is the opposite of what a cap means. (The rule is not implemented in code today, so this
> is an interface instruction for whoever lands it, not a defect to fix in a live path.)

**Handed to BFS-043 (not decided here):** the cap's value, whether it is per tree or per process, and whether a
capacity refusal is configurable to a `503`. **Handed to BFS-045:** the counter, not the code.

---

## 7. MAX PATHS PER EVENT, AND THE FRAME BOUND

### 7.1 4096 stays, and it is aligned

`extensions.watch.max_paths_per_event = 4096` is inherited (`BFS-004` §3 E-6 decl. 2, `events.go:76`) and this
document **does not move it**. It is aligned across all three places a path list can appear:

| where | bound | alignment |
|---|---|---|
| the poll's diff (`events.go:280–283`) | `len(changed) > 4096` ⇒ `overflow` | same constant, same consequence |
| the stream's frame | `len(paths) > 4096` ⇒ `overflow` | this document |
| the client's drop loop (`invalidate.go:266–268`) | `len(ev.Paths) > Capabilities().MaxPathsPerEvent()` ⇒ resync | the client asks the server for the number rather than assuming it |

One frame carries at most one event's list, so a burst of N changed paths does not multiply: it is capped and either
fits or becomes `overflow`.

### 7.2 A count does not bound a frame — the byte bound the wire form requires

> **B-15 — the wire form requires a declared BYTE bound per line (`max_event_bytes`), because 4096 paths is not a
> frame size.** Measured arithmetic: Linux path components are bounded by `PATH_MAX` = 4096 bytes, so a legal
> 4096-path event can be ≈ 4096 × 4099 + framing ≈ **16.8 MiB** of JSON on one line — while the landed consumer's
> read cap is `scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)` = **8 MiB** (`invalidate.go:414`). A frame the server
> is entitled to send can therefore exceed the cap the client is entitled to read. The failure is not benign: an
> oversized token surfaces as `bufio.ErrTooLong`, which the client classifies as a **transport** error
> (`invalidate.go:430–437` → `classifyTransport`) and retries in the reconnect loop — so a legal server frame
> produces a **reconnect loop, not a resync**, and the channel never converges. H-4.

The bound's carrier is additive to the block BFS-040 §8.2 fixes (a new field, no existing field redefined,
`document_version` still 1):

```json
"watch": { "max_paths_per_event": 4096, "max_event_bytes": 1048576, … }
```

| rule | value / relation |
|---|---|
| default asserted here (overridable by BFS-043, subject to the relation) | `max_event_bytes = 1048576` (1 MiB) — a 4096-path event of ordinary repository paths is ≈ 250 KiB, so the normal case fits and only the pathological case (deep paths, thousands of them) spills; which is the *right* answer, because a 16 MiB path list is not actionable by a client that must re-snapshot anyway |
| **the relation the client must satisfy** | the client's per-line read cap MUST be ≥ `max_event_bytes` + framing slack (it is not allowed to be smaller: that is H-4), and a client that cannot size its reader to the declared bound must declare the mismatch rather than reconnect-loop on it |
| the server's duty | measure the **serialized line size**, not the path count, and decide `overflow` on either bound being exceeded |
| the overflow rule | over either bound ⇒ `overflow` with `paths: []` — **never** a truncated list, never a partial list presented as complete, and per §6 the marker is counted |

### 7.3 A burst, end to end

A burst that changes 100 000 paths does not produce 25 `invalidate` frames of 4096 (that would be a legal-looking
partial list and a client that applies 100 000 drops per event boundary). It produces **one `overflow`** at the point
the bound is crossed — the same answer the poll gives (`events.go:280–283`), for the same reason: an interval the
source cannot vouch for is never reported as a bounded change. What the client does with it is BFS-040 §5.3's row 2
(drop everything, re-snapshot, advance past the reported `seq`).

---

## 8. RECONNECT AND BACKOFF (the client's shape)

The client already has a reconnect loop (`invalidate.go:314–345`); what follows is the contract that shapes it,
which is this row's, and it changes three things about the landed implementation.

### 8.1 The required shape

| parameter | value | why |
|---|---|---|
| initial delay | 500 ms | keeps a single transient blip from costing a second (already landed) |
| growth | ×2 | already landed |
| **jitter** | **full jitter on every attempt** (`delay = rand(0, min(cap, base×2^n))`) | H-8: the landed loop is deterministic, so N mounts that lost the same server return within the same millisecond. A server restart is exactly the event that produces N stranded mounts — the storm is manufactured by the server's own restart |
| cap | 10 s | already landed; must stay below the client's 90 s idle rule so the reconnect window is not itself the liveness window |
| give up | never, for a transport fault — but **only while the mechanism is still viable**; the give-up path is the declared fallback, not a retry limit | `BFS-005` §4.4: transient transport failures are not poll triggers |
| cursor | **every** reconnect presents the last `seq` observed (`{"since_seq": N}`) | a transport blip does not mean events stopped being generated |
| idle | implement the 90 s rule: no line for 3 × `heartbeat_ms` ⇒ declare the channel dead, switch to the poll **and say so** | H-3: the option and its default exist (`invalidate.go:79–82`, `:93–94`) and nothing reads them; `scanner.Scan()` has no timer, so a stalled stream is indistinguishable from a quiet tree — the one thing BFS-040 §5.4 O-2 makes an obligation |

### 8.2 EOF is a reconnect, not a success

> **R-6 — a clean EOF on a live context is a channel end.** The landed client treats a `nil` return from
> `watchOnce` as *"clean close: the stream ended because we closed it"* and **returns from `Run`**, ending
> invalidation permanently while continuing to report `mode=push`, `mechanism=watch`, `channel_available=true`
> (`invalidate.go:282–293`, `:430–438`). Two facts make that wrong: (a) the context is the only thing that can
> distinguish "we closed it" from "it closed on us" — and it is available at the call site; (b) BFS-040 §4.6
> mandates that the stream **ends** when the watcher is lost (`watch_lost`), so a server-side stream end is a
> *designed* event, and today's handling of it silently disables the channel. A live-context EOF must take the
> reconnect path with the cursor; if the reconnect is refused as a degradation, it takes the poll fallback.

### 8.3 When to fall back to poll (the exact set)

The fallback is entered on exactly these conditions — the first four already landed, the fifth new — and on nothing
else:

| condition | wire evidence | action |
|---|---|---|
| no watcher for this target | `501` + `capability_unavailable` (+ BFS-040's `reason`) | poll (`events`, else `rev`); report mode, mechanism and reason |
| the op predates this build | `400` + `op_unknown` | poll — **not an error state** |
| a bare `POST` with no op | `400` + `extension_op_missing` | poll — not an error state |
| unknown tree / re-bind needed | `409` + `stale_tree`, or a stream `X-Bunker-Tree` mismatch | **not** a fallback: re-bind (`ErrDeletedTree`) |
| the channel is silent for 3 × `heartbeat_ms` | no line at all | poll, **and report the degradation** (H-3) |

### 8.4 Reachable but absent — the case the row names explicitly

> **R-7 — "the server is reachable but the endpoint is absent" is NOT an error state; it is the poll fallback.**
> An older build answers the op with `400 op_unknown` or `501 capability_unavailable`; a build without the surface
> answers `404`/`405`; a build with the surface but no watcher answers the §4 degradation. Every one of these is a
> **declared degradation to a mechanism that works**, and the client must report it as such (`mode=poll`,
> `mechanism=events|rev`, plus the reason) rather than as a fault. The distinction the client must preserve: an
> *absent endpoint* is a **capability** verdict; a *dropped connection* is a **transport** verdict. They have
> different remedies (`BFS-005` §7.3: `unreachable` and `stale_identity` demand different recovery), and an
> implementation that maps 404 to a transport error will reconnect-loop forever against a server that will never
> serve the op.

---

## 9. THE FALLBACK IS CONTRACTUAL

> **F-1 — the poll form stays and works.** `X-Bunker-Op: events` (BFS-026, `events.go`) is unchanged by this row. Nothing in this document mandates a change to it — including the heartbeat-carrying note of §3.4(3), which describes what the *shared ledger* can put in its answer, not a change to the op.
>
> **F-2 — a client that never calls the channel behaves exactly as today.** A stock HTTP/1.1 client that sets no
> `X-Bunker-Op` sees byte-identical behaviour before and after: no new header is required, no existing response
> changes shape, and every status/body of the standard method matrix is untouched (`BFS-004` §11 A-7's stock-verb
> battery is the criterion that must keep passing).
>
> **F-3 — a build with no push still serves the poll and says so.** The three carriers of BFS-040 §3.1 are the
> client's only source of truth; the presence of the `watch` op in the capability document's op list is a
> *declaration of the vocabulary*, not a promise of the push mode (`BFS-004` §4.2 lists `watch` today, while
> `extensions.watch.mode` is `poll` — `ops.go:456–461`). A client must keep reading `mode`, never the op list.
>
> **F-4 — adding the channel adds no server-side requirement for any other consumer.** The WebDAV-only client, the
> `rev` poll, the CLI's status surfaces and the snapshot op are unaffected: this row adds one op's *response shape*,
> one declared byte bound, and a set of counters.

---

## 10. WHAT THIS DOCUMENT DOES NOT PROMISE, AND THE HOLES IT FOUND

### 10.1 Non-guarantees (each one is a property an implementer might otherwise assume)

1. **No exactly-once delivery.** A reconnect may re-deliver lines (the journal is answered by `seq > cursor`, and a
   cursor can be behind a line the client already saw if the line arrived but the client died before persisting the
   cursor). The client's duplicate rule (`seq <= cursor` ⇒ ignore) is the whole defence, and it is sound only because
   R-1 makes the cursor advance on every line.
2. **No ordering across mechanisms, and none beyond `seq`.** Within a frame, `paths[]` is sorted (the poll's rule,
   inherited) and sortedness is not causal order.
3. **No durability across a restart beyond the journal.** `eventsJournalEvents = 256` is the entire history. A
   restart is a resync for any client whose cursor the new ledger cannot reach (R-4).
4. **No liveness guarantee through a buffering intermediary.** D-1's rule is a deployment obligation, not something
   the channel can detect from inside the process. The *symptom* is the declared 90 s degradation to the poll.
5. **No content identity, no writer attribution, no file-level watches.** All three are BFS-040 §6's, carried here.
6. **No promise to a subscriber beyond the cap** (§6.4): the answer is a retryable refusal, not a slot.
7. **The heartbeat is not a change claim** and never implies an interval was observed (§4.3 B-3).
8. **The channel does not replace read-path validation.** A change notification says a path's metadata moved; the
   write path's content re-validation and the client's hash check remain the correctness mechanism (BFS-040 §6.1).
9. **`paths` scoping is not a delivery guarantee** (§2 W-3): over-reporting is legal, under-reporting is a defect.

### 10.2 Holes found while writing this (reported, not smoothed over)

| # | finding | evidence | disposition |
|---|---|---|---|
| **H-1** | **A clean EOF silently ends the channel.** `Run` treats `watchOnce == nil` as "we closed it" and returns; `mode=push`, `mechanism=watch`, `available=true` stay reported while nothing invalidates again. BFS-040 §4.6 mandates the stream *ends* on `watch_lost`, so a designed server event lands in this branch. | `invalidate.go:282–293`, `:430–438` vs BFS-040 §4.6 | Fixed at the contract level by R-6 (§8.2). The code change is the row that lands the push form (or a client follow-up); **not filed as a board row by this worker** (the brief forbids it). |
| **H-2** | **The heartbeat manufactures a false gap.** Its `seq` is not recorded because `apply()` returns before the cursor bookkeeping, so the next real event satisfies `Seq > cursor+1` and forces a resync — on every heartbeat-then-change pair. The channel works and pays the cost it exists to remove. | `invalidate.go:219–224` vs `:241–253` | Fixed by R-1 (§3.1): the cursor advances on every line, regardless of `event`. |
| **H-3** | **The declared 90 s liveness rule has no implementation.** `IdleTimeout` is an option with a `DefaultIdleTimeout = 90s` and nothing reads it; the read loop has no timer, so a stalled channel is indistinguishable from a quiet tree — the obligation BFS-040 §5.4 O-2 places on the source is unmeetable from the consumer side until this lands. | `invalidate.go:79–82`, `:93–94`, `:125–126`; zero reads of `opt.IdleTimeout` | Contract fix in §8.1 (the idle row) and §8.3 (the fifth fallback condition). |
| **H-4** | **The frame is bounded by count, not bytes.** 4096 × `PATH_MAX` ≈ 16.8 MiB exceeds the consumer's 8 MiB per-line cap; the failure classifies as a **transport** error, so a legal server frame becomes a reconnect loop instead of a resync. | `invalidate.go:414` (`8<<20`) vs Linux `PATH_MAX` 4096; `:430–437` | Fixed by B-15 (§7.2): a declared `max_event_bytes` plus the relation the client's read cap must satisfy. |
| **H-5** | **`heartbeat_ms` equals the QUIC default idle timeout.** The repo's h3 server sets no `IdleTimeout` and no `QuicConfig`, so quic-go's 30 s default governs; the declared heartbeat period is 30 s. The connection can be declared idle at the instant its first heartbeat is due. | `h3.go:73–87`; quic-go `v0.63.0` `protocol.DefaultIdleTimeout`; `http3/server_conn.go:59–63` | Constraint C-3 (§1.4) + the relation table (§4.4): set `KeepAlivePeriod` or keep a strict margin. Owner: whoever lands the push form; the knob is BFS-043's. |
| **H-6** | **Heartbeats and the poll share one journal and one counter.** If heartbeats consume a `seq` and are not retained, mechanism switches and reconnects cannot distinguish "heartbeats" from "loss" (⇒ resync on every switch); if they are retained, they occupy journal slots and a polling client can receive them. | `events.go:117–124` (one ledger per tree, both mechanisms), `:62–63` ("a heartbeat keeps a STREAM alive") | Decided in §3.4 (consumed, retained, broadcast; the poll may carry them). Consequence H-9. |
| **H-7** | **A capacity refusal must not look like a degradation.** The client's downgrade branch keys on `capability_unavailable`/`501`/`op_unknown`/`extension_op_missing`; a subscriber-cap refusal that used any of those would permanently downgrade a client whose slot frees up in seconds, and BFS-005 §4.4's three-strikes rule would do the same if a retryable refusal counted as a failure. | `invalidate.go:305–307`, `:331–336`; BFS-005 §4.4 item 2 | Fixed by B-13/B-14 (§6.4): a retryable `429`-shaped refusal with no capability header, landing in the client's existing `default:` (reconnect) branch, and exempt from the three strikes. |
| **H-8** | **The reconnect backoff has no jitter.** Deterministic 500 ms × 2 to 10 s means N mounts stranded by one server restart return together — the storm is a deterministic consequence of the server's own restart. | `invalidate.go:314–330` | Contract fix in §8.1: full jitter per attempt. |
| **H-9** | **A stream eats the journal window.** Journaling heartbeats (required by §3.4(2)) means that on a quiet tree the 256-entry journal can be almost entirely heartbeats, so the retained *change* history is much shorter than 256 — a reconnect that would have been served by the tail becomes an `overflow` instead. Honest cost, not a defect: the answer is still a resync, never a silent gap. | `events.go:78` (`eventsJournalEvents = 256`), `:232–246` (every push is journaled) | Reported as a cost of H-6's decision. If it is ever wanted cheaper, the option is a separate retained ring for change events **plus** the loss signal the split would otherwise hide — which is a design change, not a tweak, and not this row's to make. |
| **H-10** | **A resume that falls off the end is a silent gap for a freshly-bound client.** The `events` op answers a cursor older than the retained journal with the retained tail and relies on the client's monotonicity check to notice the gap — but that check is guarded on a non-zero cursor (`ev.Seq > i.seq+1 && i.seq != 0`), so a client whose cursor is `0` receives a tail starting at, say, `seq 300`, sees **no gap**, and treats it as complete coverage. The window is real on a busy tree: `eventsJournalEvents` is 256, and a client that binds and then waits one poll interval can find the window rotated. **CLOSED AS FILED, by BFS-063 (landed; see `docs/evidence/BFS-063-*`): the poll now emits the marker itself, on the rule that a retained tail is admissible only when the presenting client can recognise it as a tail — and it also accepts no cursor at all as "I hold no observation", which is answered `overflow` rather than a quiet tail. The declaration that makes the distinction possible is a server-minted cursor (`snapshot`'s `result.head_seq`), because a cursor of `0` cannot express it: the same bytes are sent by a client that bound with a snapshot and by one that has observed nothing.** | `events.go:305–311` vs `invalidate.go:241–246`; BFS-063's arms | Fixed by BFS-063 (the poll's own marker + the resume declaration). Measured: the unfixed rule answers `[]` to a client that has observed nothing while its cached bytes differ from the served ones, and the arm turns red when the clause is disabled. |

Two further cases the row's own list names, both already covered elsewhere and neither smoothed over:

- **A proxy that buffers** — §1.6 D-1: it is not detectable in-process; the symptom is the declared 90 s degradation to the poll, which is a *reported* mode change (BFS-005 §4.4) and must be visible in the status record (§11.2).
- **A client that acks after the journal rotates** — §3.2 row 3: the answer is `overflow` then the tail, and the client resyncs once and never pages back.

---

## 11. BOUNDARIES — what this document does NOT decide

| row | its surface | the interface this document hands it |
|---|---|---|
| **BFS-040** (landed, the event source) | the watcher, its probe matrix, its vocabulary, the overflow guarantee | Unchanged and carried by reference: the event names, §5.2's forced-resync table, §5.3's "resync ≠ quiet", and §5.4's O-1/O-2. This document **adds** three things it must not silently contradict (§3.4's one-ledger rules, §7.2's `max_event_bytes` field, §5's dead-client release paths) — each is additive to a field or a lifecycle, none redefines a `reason`, a `state`, a refusal verdict or an event name. |
| **BFS-042** (hot-file policy) | popularity accounting, the size rule, queue stop semantics, the pool share | Only this: the channel's per-subscriber buffer and the hot-file queue are **separate bounds** (B-7 vs the PRD §3 tracker bounds), and a hot-file refresh must never be *required* for correctness (PRD §2.4). Nothing about scoring, sizes or pool shares. |
| **BFS-043** (server config) | watcher/push knobs, defaults, validation, the unhonourable-value refusal path | §4.4's relation table, §5.3's write deadline, §6.1's buffer bounds, §6.4's cap, and §7.2's `max_event_bytes` default are the values a config surface must be able to express and validate. **Not decided here:** names, defaults (beyond the asserted ones), validation messages, env bindings. |
| **BFS-045** (observability) | the status record: every bound counted and visible | §6.3's counter table (names fixed, layout not), §5.3's closed two-value release reason, and §8's reconnect/idle counters on the client side. The *server* document's counter set stays BFS-040 §8.2's. **Not decided here:** record layout, naming style, aggregation, retention, dashboard. |
| **BFS-046** (tests) | the test program, harness, negative controls, coverage floor | §12's property list, each with the control that proves it can fail. **Not decided here:** the harness, the program, the floor, or any cell's implementation. |
| **BFS-026** (landed, the poll) | the poll form | Unchanged (F-1). |
| **BFS-044** (client config) | mount flags, defaults, validation, reported bounds | §8's reconnect/idle shape is what a flag may tune (the values, not the shape). **Not decided here:** flag names. |

**Explicitly out of scope:** the watcher's implementation, any config surface, any test, any board row (this row
reports; it does not file), and any correctness change to the write path.

### 11.1 The client's reporting surface, and what the push path adds to it

`internal/fsclient/invalidate.go`'s `InvalidationState` already reports **`mode`** (`push|poll`) and **`mechanism`**
(`watch|events|rev|none`), and it already knows *which mechanism answered* at the moment it answers
(`:158–171`; set in `watchOnce` before the first line is read at `:406–411`, in `pollEventsOnce` before events are
applied at `:495–498`, and in `pollRevOnce` at `:535–540`). The rule this document fixes is the ordering, because it
is the difference between an honest record and a lie:

> **O-1 — the mechanism is updated BEFORE the first event from that mechanism is applied.** Then no event is ever
> attributed to a mechanism that did not deliver it, and a mid-session switch (push → poll on a 90 s idle or a
> refusal) shows as a switch rather than as a retroactive relabel. The landed ordering already does this; the clause
> exists so that a refactor cannot lose it.
>
> **O-2 — never imply the watcher when the poll delivered it.** `mode` describes HOW the client is being told
> (push or asking); `mechanism` describes WHICH call answered. A client on `mode=poll`/`mechanism=events` must never
> report `watch` for changes that arrived in a poll answer, and the reverse. If the two ever disagree, the record
> must carry the reason (the existing `reason` field) rather than reconciling silently.

**What the push path adds to that record (and what it does not):** at most a **cursor** (the last `seq` applied, so a
resume is inspectable) and the **reconnect/idle counters** the §6.3/§8 rules need to be visible (reconnects, idle
fallbacks, and — with §6.3's names — the drops/gaps the channel caused). Those are the facts this row's clauses make
observable; **the field list, its naming, its layout and its drill-down are BFS-045's** and this document deliberately
does not invent them. The one hard requirement it hands over: the record must be able to distinguish
*"the watcher delivered this"* from *"the poll delivered this"* from *"the channel dropped this and we resynced"* —
three facts, three distinguishable states, because collapsing any two produces the class of defect this release is
made of.

---

## 12. What must be provable (handed to BFS-046 — the program is not written here)

Each property with the control that proves the property can fail. This is the interface, not the test program: the
harness, the cells, the floor and the coverage target are BFS-046's.

| # | property | the control that makes it non-vacuous |
|---|---|---|
| P-1 | the stream is answered **ahead of the router's deadline** | the existing placement test (M4) plus the mutation arm: mount the channel behind `middleware.Timeout` and assert the stream **dies** — turning C-1 from prose into a red arm |
| P-2 | resume inside the journal returns exactly the missed lines, once, in order | drive the burst through the poll (whose replay rule is landed) and assert the stream's answer is byte-equal to the poll's for the same cursor |
| P-3 | a cursor **ahead** of the ledger yields an `overflow` with `seq > cursor` | the restart fixture; the control is a cursor *behind* the head, which must **not** produce an `overflow` |
| P-4 | no gap is ever silent: for every resume case the answer is either vouched-for lines or an `overflow` | a subtractive control: implementation variant that returns the retained tail alone must **fail** P-4 (R-3) |
| P-5 | a quiet tree produces a heartbeat within the declared period, and the client can tell it from a dead channel | a fixture that stops the source: assert heartbeats continue and the 90 s rule fires; the control is a live source whose events also reset the rule |
| P-6 | the heartbeat does not manufacture a gap | a scripted interleaving (heartbeat between two changes) asserting gaps = 0 and a single resync-free delivery — the H-2 red arm |
| P-7 | a vanished client is released, and a killed one is released sooner | socket-kill vs `SIGKILL`-without-close fixture; assert the two `subscriber_disconnects_total` triggers; the control is a healthy reading client which must **not** be released |
| P-8 | the dead-client rule does not depend on the client ever writing | the stream is one-way after the POST: assert release with **zero** client bytes sent after the request |
| P-9 | backpressure drops oldest and marks the gap, counted | fill the buffer with a non-reading subscriber, then read: assert exactly **one** `overflow` (not one per drop), `subscriber_drops_total > 0`, and that no coalesced or partial `paths[]` was ever written |
| P-10 | a frame over either bound becomes an `overflow`, never a partial list | two arms: 4097 paths, and ≤ 4096 paths whose serialized line exceeds `max_event_bytes` (long paths) — the second is the H-4 arm and the count-only implementation must fail it |
| P-11 | an oversized frame does not put the client in a reconnect loop | the client-side arm: feed a line above the declared bound with a correctly-sized reader and assert a resync, not a transport error |
| P-12 | reconnect backs off with jitter, and N clients do not return together | run the loop against a refused endpoint and assert the delay distribution is not constant; the control is the current implementation, which must fail |
| P-13 | an absent endpoint reaches the **poll**, reports mode/mechanism/reason, and does not count as a fault | 404, 405, `400 op_unknown`, `501 capability_unavailable` — four arms, one assertion each (R-7) |
| P-14 | a live-context EOF reconnects instead of ending invalidation | the H-1 arm: close the server side, assert the client returns to the reconnect path, `mode` still `push`, and a later change is still delivered |
| P-15 | the fallback is contractual: a stock client sees no change | `BFS-004` §11 A-7's stock-verb battery, re-run unchanged, plus an assertion that no request without `X-Bunker-Op` behaves differently |
| P-16 | the record never implies the wrong mechanism | force a mid-session downgrade and apply a change **after** it: assert `mechanism` at the moment of the drop, and that no event is attributed to the previous mechanism (O-1/O-2) |

---

## Appendix A — every number and measured fact used above, and how it was taken

| # | fact | value | how taken |
|---|---|---|---|
| A.1 | the client's stream wire form | `POST`, `X-Bunker-Op: watch`, `Content-Type: application/json`, `Accept: application/x-ndjson`, body `{"paths":[…],"since_seq":N}` | source read `internal/fsclient/invalidate.go:350–420` |
| A.2 | the stream escapes the client's op deadline and semaphore | built with `i.client.hc.Do(req)`; `client.do` is the only path that takes `sem` and applies `OpTimeout` | `invalidate.go:391–398`; `client.go:276–300` |
| A.3 | the client's bounds | `DefaultConcurrency = 25`, `sem` capacity 25, `OpTimeout` default 30 s, `DefaultPollInterval = 2 s`, `DefaultIdleTimeout = 90 s` (unused) | `client.go:33`, `:164`, `:39–40`; `invalidate.go:88`, `:93–94` |
| A.4 | the client's read caps | NDJSON scanner: 64 KiB initial, **8 MiB** maximum token; per-op body reads capped at 2/4/8 KiB by call | `invalidate.go:414`; `client.go:472,508,543,…` |
| A.5 | the placement of the surface | `/dav` mounted ahead of the chi router; router carries `middleware.Timeout(server.request_timeout)`; `request_timeout` default `300s`; both `http.Server` literals set no socket timeouts; `TestWebDAVOpIsAnsweredAheadOfTheRPCTimeout` **PASS** (0.15 s) on this branch, with its live-deadline control | `server.go:153`, `:394–398`, `:469–478`, `:487–497`; `config.go:197`, `:820`; test run |
| A.6 | the runtime's response buffering and cancellation | `bufferBeforeChunkingSize = 2048`; "the context is canceled when the client's connection closes, the request is canceled (with HTTP/2), or when the `ServeHTTP` method returns" | `$(go env GOROOT)/src/net/http/server.go:341`; `…/request.go:349–351` |
| A.7 | write deadlines per version | h1 and h2 response writers implement `SetWriteDeadline` (h2 at `h2_bundle.go:6830`); **no non-test file in quic-go v0.63.0's `http3` package does** | GOROOT reads; `grep SetWriteDeadline` over `$(go env GOMODCACHE)/github.com/quic-go/quic-go@v0.63.0/http3/*.go` |
| A.8 | the h3 idle budget | `http3.Server{Addr,Port,TLSConfig,Handler,Logger}` — no `IdleTimeout`, no `QuicConfig`; quic-go `DefaultIdleTimeout = 30s`; the HTTP-level idle timer is armed only when `IdleTimeout > 0` | `h3.go:73–87`; `quic-go@v0.63.0/internal/protocol/params.go:94`, `http3/server.go:183–187`, `http3/server_conn.go:59–63` |
| A.9 | the channel's inherited declarations and bounds | heartbeat ≤ 30 s (`extensions.watch.heartbeat_ms`), ≤ 4096 paths/event, `tree` on every line; journal 256; scan limit 100 000; the refusal `501 capability_unavailable; scope=target; mode=poll` | `BFS-004` §3 E-6, §4.2, §10.5; `events.go:74–81`, `ops.go:84–90` |
| A.10 | the poll's existing resume rules (the consistency target) | first observation ⇒ `overflow`; cursor ahead ⇒ advance `seq` past the cursor, then `overflow`; else the tail with `seq > cursor` | `events.go:267–311`, with the reasoning in the comment at `:294–301` |
| A.11 | the client's existing branches and their ordering | poll on `capability_unavailable`/`501`/`op_unknown`/`extension_op_missing`; `default:` ⇒ reconnect with the cursor; 500 ms ×2 to 10 s, no jitter; `stale_tree` ⇒ re-bind | `invalidate.go:294–345` |
| A.12 | the heartbeat's effect on the client cursor (H-2) | `apply()` returns for `heartbeat` before the `seq` bookkeeping, so the cursor lags by one per heartbeat | `invalidate.go:217–253` |
| A.13 | the clean-EOF path (H-1) | `watchOnce` returns `nil` at a clean scanner EOF; `Run` returns on `nil` as "clean close" | `invalidate.go:282–293`, `:430–438` |
| A.14 | no incremental writer exists on this surface yet | zero `Flush()`/`http.Flusher` uses in `internal/server/webdav/` and `internal/fsclient/`; the only landed streaming plumbing is the connect-side `envelopeResponseWriter` (`internal/server/streaming_envelope.go:87–134`), which exists because connect *requires* `http.Flusher` for streaming RPCs | `grep -rn "http.Flusher\|Flush()" internal/` |
| A.15 | the mount path is local by default; the tunnels are the deployment's public path | `http://127.0.0.1:18481/dav` is the documented surface root; `internal/tunnel` manages TryCloudflare/named `cloudflared` tunnels for agent ports | `internal/fsmount/options.go:54`, `internal/cli/fs.go:76`; `internal/tunnel/SKILL.md` |
| A.16 | Cloudflare's published proxy behaviour (**external**, cited not measured) | ~100 s without a response is answered `524`, and reused TCP connections are reaped at the proxy idle timeout | Cloudflare Fundamentals "Connection limits" (developers.cloudflare.com/fundamentals/reference/connection-limits/); community/Stack Overflow reports of the 100 s response limit — recorded as external, and used only for D-2's *rule*, never as a measured value of this deployment |
| A.17 | `PATH_MAX` and the frame arithmetic (H-4) | 4096-byte path components ⇒ a legal 4096-path line ≈ 16.8 MiB > the consumer's 8 MiB token cap | Linux `PATH_MAX`; arithmetic over A.4 and A.9 |

Probes A.5's test run executed in this worktree; no probe in this row wrote to the repository outside the two files
this row adds, and no live deployment was touched.

## Appendix B — how this document relates to the documents it inherits

- **`docs/prd/SPEC-watcher-capability.md` (BFS-040)** — the event source. §5 is carried verbatim in intent (an
  unvouched interval is never quiet; a cursor older than the retained journal forces a resync), §5.2/§5.3 in §3.2,
  §5.4's O-1/O-2 in §4.3 and §6.1 B-8, §4's `reason` vocabulary is used, never extended, in §5/§6.4/§8.3, and §9's
  hand-off ("the transport, the cursor's wire name, the reconnect schedule, how a dead subscriber is bounded") is
  exactly this document's subject.
- **`docs/spec/BFS-004-webdav-surface.md` §3 E-6** — the event names, the three declarations and the response
  shape; §4.2's capability document (to which §7.2 adds one byte-bound field); §4.3's per-version cost table (the
  HTTP/1.1 occupancy figure); §5.1–§5.3's refusal vocabulary (no new verdict is added by this row); §10.5's bytes.
- **`docs/spec/BFS-005-client-cache-and-diff.md` §4.1/§4.4** — the resync closure and the declared fallback ladder
  this row's §8 shapes, including the flush-per-line requirement (§4.1) and the 90 s idle rule (§4.4).
- **`docs/prd/PRD-bunker-invalidation.md` §1 R3, §2.1, §2.7, §2.8** — the push requirement, the silent-loss
  prohibition, "a bound the owner cannot see is not a bound" (this row's counters), and the not-hang standard
  (§5.1 B-4).
