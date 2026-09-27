# BFS-026 — the invalidation channel: E-6's poll form, SERVED

**Row:** BFS-026 (P0) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-27
**Depends on:** BFS-004 §3 E-4/E-6 (the wire contract), BFS-005 §4 (the client's channel),
BFS-006 (the landed surface), BFS-009 (the measurement this row acts on), BFS-015 (the
commit-time re-validation this row must not weaken)
**Product code changed:** `internal/server/webdav/{events.go (new), ops.go, tree.go,
filetime_linux.go (new), filetime_other.go (new)}`. **No client code changed** — see §1.3.

---

## 0. Verdict in one screen

| # | what the row asked | result | where |
|---|---|---|---|
| 1 | **the decision: (a) or (b), with a reason** | **(a) — the declared poll fallback is implemented server-side**, as `X-Bunker-Op: events`, the op the client's invalidation path actually calls. §1 says why (a) beat (b), why `events` and not a `snapshot diff`, and what (b) would have bought. | §1 |
| 2 | **don't break the honesty** | The four fields keep reporting reality and are now *backed*: the pushed form is still refused with `scope=target, mode=poll`, the refusal text names the poll that exists, `mechanism` still names the call that answered, and the record's own counters are what prove delivery. Refusals quoted verbatim. | §2.4, §4, §5 |
| 3 | **a RED on the current tree, reproduced not cited** | **REPRODUCED**: `channel_available:false, events_total:0`, the 501 verbatim, and an agent-side edit not seen — a cached path served pre-edit bytes for the whole 12 s hold, while the record never moved once. | §4 |
| 4 | **the GREEN, with the mechanism named** | **DELIVERED**: `channel_available:true, mechanism:"events"` (the POLL form — never `watch`), `paths_dropped_total:2`, and the cached path served fresh **1811 ms** after an agent-side edit, **3 ms after the channel's own record reported it**. Control reads at +0 ms and +401 ms were both stale, so the fresh read is attributable to the channel. | §5 |
| 5 | **a negative control that can fail** | **PROVEN — and it caught a real defect in my own change first**: mutating the op table alone left the mechanism working (the table is a report, the handler's switch is the gate). With the gate reverted, all three arms go RED; the restore is sha256-verified. A new test now pins table↔switch agreement. | §6 |
| 6 | **don't weaken BFS-015** | Re-run green: the four commit-window tests — `TestExpectedHashIsRevalidatedInsideTheCommit` (3 subtests), `TestCreateOnlyRuleIsRevalidatedInsideTheCommit` (2), `TestCommitSectionIsExclusive`, `TestConcurrentConditionalWritesCannotBothLand`. | §7.1 |
| 7 | **no new requirement for existing clients** | 29-cell stock-verb battery, base vs new, **empty diff**; the only visible change is for a client that already sends `X-Bunker-Op`. | §7.2 |
| 8 | **HTTP/1.1 keeps working; where does the mechanism sit?** | Every arm runs over plain HTTP/1.1 against the live mount. The op is served by the WebDAV handler **ahead of the chi router**, where the 300 s `middleware.Timeout` lives — proved by a new test with two controls. | §3 |
| 9 | **bounded, and the not-hang standard** | No long-poll, no stream: a single envelope with `Content-Length`, answered in **6.2–12.3 ms** on a 2000-path tree (275× the declared interval). Observation capped, path list capped (spill proven against the real 4096), journal bounded. | §8 |
| + | the watcher (`watch`) | **Still not implemented, and still refused honestly** (`scope=target, mode=poll`). It needs a per-target inotify watcher plus a bounded stream; the poll is the mode the spec and the client already declare for a target without one. Named as a residual, not implied. | §10 R3 |

**Product code: 5 files, +448 lines** (`events.go` 345 new, `filetime_*` 45 new, `ops.go` +50/−9,
`tree.go` +8). **Tests: +17** (15 in `internal/server/webdav`, 2 in `internal/fsclient`) — the
packages go from 38→53 and 22→24 top-level tests. **`go test ./...`: 28 packages ok, 0 failures.**
**Cross-GOOS guard: PASS** (the new `!linux` fallback is what keeps the Windows targets building).

---

## 1. The decision

### 1.1 (a) or (b)

**Chosen: (a) — implement the fallback server-side.**

The criterion is whether the declared fallback can be implemented *honestly and within its bounds*
in one row. It can: one op over the tree the server already walks for `snapshot`, 345 lines of new
product code, every bound it needs (path cap, journal, scan cap) a constant. So the mechanism
exists now rather than being retracted.

Three further reasons, each from evidence rather than taste:

1. **The claim is not only the client's.** The capability document has always declared the poll
   form — `extensions.watch.modes.poll = "X-Bunker-Op: events"` (BFS-004 §4.2, and the running
   document agrees) — and `watch`'s own 501 answers `mode=poll`. Retracting would mean editing the
   *specification's* advertised mode for a missing implementation, which is the opposite of `R3`
   ("a capability this build lacks answers a structured refusal rather than disappearing").
2. **(b) moves the work instead of removing it.** The mount cannot re-fetch what it does not know
   changed; a TTL bound still needs the client to age out cached *content*, invalidate *node
   attributes*, and report the window. Same client work, worse bound: a TTL's worst case is the
   TTL, while the poll's worst case is the declared interval *and* it says which path moved.
3. **The row's P0 consequence is a stale serve.** §5 measures the channel closing it: the fresh
   read is attributable to the channel because the control reads were stale (§5.3).

**What (b) would have bought, stated honestly:** an explicit, reported content-age bound on the
client's cache. Not in this row; named as residual R6.

### 1.2 Why `events`, and not the `snapshot diff` the 501 text suggested

The old refusal text said *"poll with HEAD/ETag or an X-Bunker-Op: snapshot diff"*. Neither is a
mechanism the client's invalidation path calls:

* the path calls `i.client.Op(ctx, "events", {"since_seq": …})` (`internal/fsclient/invalidate.go`,
  `pollEventsOnce`). A server serving `snapshot diff` and not `events` would leave the channel
  exactly as dead — and reaching it would need a **client** change (a new requirement for clients,
  which requirement 4 forbids in spirit);
* `diff`/`status`/`rev-parse`/`ls-files`/`log` are the **delegated git family** (`DelegatedOps`):
  they compute a text answer on the agent and need git. The channel needs a *change signal about
  the tree's state* — what E-6's `events` is defined to carry ("the poll form of E-6", same event
  objects, same per-tree `seq`, `since_seq` to resume).

So the op implemented is the one contract and client agree on; `watch` is untouched; the other
slice-C5 ops still answer their slice-naming 501, byte-identically (§7.2).

### 1.3 No client change

`bunker fs mount` and every other client file are unmodified. The client already had the
mechanism's client half: it polls `events` each `--poll-interval`, prefers it over the last-resort
`rev` poll when the document declares it, drops the paths an `invalidate` carries, and re-snapshots
on `overflow`. What was missing was the server half.

---

## 2. What was implemented, and what it can and cannot know

### 2.1 The wire shape (unchanged from the contract)

```
POST /dav/ HTTP/1.1
X-Bunker-Op: events
Content-Type: application/json

{"since_seq": 3}

HTTP/1.1 200 OK
X-Bunker-Op: events            X-Bunker-Verdict: ok
X-Bunker-Rev: rev:0            X-Bunker-Tree: tree:…

{"ok":true,"op":"events","verdict":"ok","rev":"rev:0","tree":"tree:…","proto":"HTTP/1.1",
 "duration_ms":9,"truncated":false,
 "result":{"events":[{"seq":4,"event":"invalidate","paths":["src/main.go"],"rev":"rev:0",
                      "tree":"tree:…"}],
           "count":1,"scanned":2011,"head_seq":4},
 "error":null}
```

`result.events` is E-6's own list: `invalidate` (path claims), `overflow` (knowledge lost — drop
everything, re-snapshot), and `heartbeat` for streams only. This op never emits a heartbeat: a poll
that answers is its own liveness signal. `tree` is on every line (declaration 3) and `paths` is
always present — an empty array for `overflow`, as §10.5's bytes show — so a consumer never has to
tell "no paths" from "absent field".

### 2.2 How it knows: an observation, not a watcher

Each poll walks the served tree once (stat-only — **no file bytes are read**) and compares one
identity per path against the identity it recorded last:

```
identity = (size, mtime_ns, ctime_ns, is_dir)
```

* **ctime is in the identity on purpose.** An edit that preserves size *and* restores the mtime is
  the one shape a `(size, mtime)` observer cannot see, and this release has already recorded that
  class as a defect (BFS-009 F1). ctime moves for it, so the channel sees it —
  `TestEventsOpSeesAnEditThatPreservesSizeAndMtime` drives exactly that trap live. Where a platform
  exposes no ctime (`filetime_other.go`, `!linux`) the identity falls back to `(size, mtime)` and
  the gap is real: stated in the code, stated here, cross-compiled by the BFS-010 guard so the
  fallback cannot rot.
* **A path the filesystem reports as changed is reported as changed** — added, removed, replaced,
  or renamed (two paths: the old absent, the new present). Nothing about the change's *author*
  enters: an agent-side edit and a surface-side `PUT` are the same fact.
* **The surface's own staging files are skipped** (`isTempName`), exactly as every listing skips
  them, so a partially-written file is never reported as a path.

### 2.3 What it cannot know — stated, because the events it emits are chosen by it

| limit | consequence | the answer it gives |
|---|---|---|
| It sees the tree only **at poll time** | a change made and reverted between two polls is invisible (true of any poll-based channel) | nothing — and the interval is declared in the client's own record (`poll_interval_ms`) |
| It holds **no history before its first observation** | a client whose view predates that baseline cannot be vouched for | `overflow` — "knowledge lost, re-snapshot" — never an empty list that would claim nothing happened |
| A rewrite that restores size, mtime **and** ctime | not observable through the filesystem by anyone who does not read the bytes | invisible; the write path's content-hash re-validation (BFS-004 §6.1 step 5, BFS-015) covers that case |
| A tree bigger than the observation cap (`eventsScanLimit`, 100 000) | the diff would be partial | `overflow` **and** `truncated:true` — A-11: truncation is always reported |
| More than `eventsMaxPathsPerEvent` (4096) changed paths | a longer list would be a partial answer dressed as a complete one | one `overflow`, no path list — measured against the real cap, §8.3 |
| A client further behind than the retained journal (256 events) | the missed range is gone | the retained events, whose first `seq` is a gap the client's own rule reads as knowledge lost (BFS-005 §4.1) |
| A cursor **ahead** of the ledger (a restarted process) | the range between was never observed | `overflow` at `cursor+1`, because an event at or below a client's own `seq` is one it is right to discard as a duplicate |

Two clients polling one tree are **strictly ordered**: the ledger lock is taken before the
observation and held across it, so each diff is against what the previous observation recorded and
the baseline can never move backwards. The price is that a second client's poll waits for one walk
(~7 ms on a 2000-path tree; §8.1).

### 2.4 The honest seed

The ledger's first observation is its baseline. If a whole-tree `snapshot` has already walked the
tree in this process, that walk **is** the observation and becomes the baseline (`seedEvents`), so a
client that snapshots before it polls gets a quiet diff rather than an overflow it did not earn. A
sub-tree, a shallow listing or a truncated walk is not a baseline and is not offered as one —
`TestEventsOpSnapshotSeedMakesTheFirstPollHonest` runs both arms.

If no observation exists when the first poll arrives, the poll seeds and answers **`overflow`**: the
interval before the ledger looked is unknown rather than empty, and the client's rule for that word
is to re-establish its view. §5.2 shows a real mount taking exactly that answer once at bind, and
measures what it costs.

---

## 3. Where the mechanism sits (requirement 5)

`events` is a case in `handlePost` (`internal/server/webdav/ops.go`), served by the handler that
`bunkerd` mounts **ahead of the chi router**:

```
internal/server/server.go:153   r.Use(middleware.Timeout(s.cfg.Server.RequestTimeout))   // 300 s default
internal/server/server.go:396   rootHandler = mountWebDAV(rootHandler, webdav.Prefix, davHandler)
```

The 300 s RPC deadline is a router middleware, and the `/dav` subtree never enters the router (which
is also why `PROPFIND` reaches the surface at all). So no request deadline governs this channel —
the structural fact BFS-006 recorded for the watch stream, verified here rather than assumed.

`TestWebDAVOpIsAnsweredAheadOfTheRPCTimeout` (`internal/server/webdav_mount_test.go`) proves it with
two controls, so it is not a claim about a router that is not there:

* the op is answered by the surface and the instrumented router records **0** hits;
* control 1: a non-`/dav` path **is** routed (the router is installed);
* control 2: a handler that outlives the router's deadline **is** refused with `504` (the deadline
  in the harness really bites).

Every live arm runs over plain **HTTP/1.1** (`status.transport.proto = "HTTP/1.1"` in every
transcript), against a real `bunker fs mount`, with the client unmodified.

---

## 4. The RED, on the tree as filed

Reproduce with `channel-arm.sh` and binaries built from the tree as the row was filed (`46a5350`).
Full transcript: **`BFS-026-channel-red.txt`**.

**The client's own record, verbatim** (the `invalidation` block of `status.json`):

```json
{"channel_available": false, "events_total": 0, "last_event_age_ms": null, "mechanism": "events",
 "mode": "poll", "paths_dropped_total": 0, "poll_interval_ms": 2000,
 "reason": "poll call failed: op:events : errno=EOPNOTSUPP cause=server_error status=501 verdict=capability_unavailable: no inotify watcher on this target; poll with HEAD/ETag or an X-Bunker-Op: snapshot diff",
 "resyncs_from_gap": 0, "resyncs_total": 0, "seq": 0}
```

**The op catalogue, probed live** (every name in BFS-004 §3 E-4, plus one outside it):

```
capabilities 200  verdict=ok          snapshot     200  verdict=ok
status       501  capability_unavailable   events  501  capability_unavailable
diff         501  capability_unavailable   watch   501  capability_unavailable
rev-parse    501  capability_unavailable   frobnicate 400 op_unknown
ls-files     501  capability_unavailable
log          501  capability_unavailable
```

**The fallback runs and is refused every time.** Over a 6 s quiet window with no filesystem I/O of
ours: `requests_total 6 -> 9` — **0.5 req/s**, one POST per the declared 2000 ms interval.

**An agent-side edit is not seen** (one clock; `visibility.py`):

```
=== D. a path NEVER READ (BFS-025's shape), edited on the AGENT ===
  record before    : events=0 dropped=0 resyncs=0 seq=0 available=False mechanism=events
  agent bytes now  : 'never-version-2-EDITED-ON-THE-AGENT' (35 bytes)  [edit at +0 ms]
  read +   5 ms      : REFUSED: OSError: Stale file handle
  read + 410 ms      : 'never-version-2-EDITED-ON-THE-AGENT'   (control: still inside the declared window)
  channel reported : NONE within 12.0 s — the record never moved
  served fresh at  : +400 ms  (BEFORE the channel: repaired by the read path, not by the channel)
  (the mount's own log line:)
    bunker-fs: read bound divergence never.txt: published=16 content=35 refused=true (the metadata the kernel holds for this path was stale)

=== E. a path ALREADY READ (cached): the BFS-024 shape, with its control ===
  mount first read : cached-version-1   (so the path IS cached)
  agent bytes now  : 'cached-version-2-EDITED-ON-THE-AGENT' (36 bytes)  [edit at +0 ms]
  read +   1 ms      : 'cached-version-1\n'
  read + 401 ms      : 'cached-version-1\n'   (control: still inside the declared window)
  channel reported : NONE within 12.0 s — the record never moved
  served fresh at  : NEVER within 12.0 s
```

Read those two blocks together, because they are different failures and only one of them is the
channel's to fix (this distinction is measured, not asserted — it is what §5 turns on):

* the **never-read** path is the one BFS-025's rule refuses, and the mount then repairs it **on its
  own** — a read 0.4 s later serves the new bytes in both builds. No channel is involved, and in the
  RED there is none. That recovery belongs to the read path, and this row claims no credit for it.
* the **cached** path is the one nothing repairs: stale at +1 ms, stale at +401 ms, and stale for
  the whole 12 s hold. That is BFS-024's filed shape, reproduced.

**The delegated verb, by hand:**

```
OP REFUSED: events status=501 verdict=capability_unavailable errno=EOPNOTSUPP cause=server_error
  capability : events (scope=target phase=- mode=poll)
  detail     : no inotify watcher on this target; poll with HEAD/ETag or an X-Bunker-Op: snapshot diff
```

**Verdict lines**

```
VERDICT-LINE label=RED mode=poll mechanism=events channel_available=False events_total=0 paths_dropped_total=0 resyncs_total=0 last_event_age_ms=None
VERDICT-LINE never-read:  SUMMARY never.txt edit=0 control_fresh=True channel_ms=none fresh_ms=none
VERDICT-LINE cached-path: SUMMARY cached.txt edit=0 control_fresh=False channel_ms=none fresh_ms=none
```

---

## 5. The GREEN

Full transcript: **`BFS-026-channel-green.txt`**. Same arm, same arguments, binaries built from this
row's tree. The order a real client reaches the poll in: **`BFS-026-seed-order.txt`**.

### 5.1 The op is served, and only it

```
capabilities 200  verdict=ok   snapshot 200 verdict=ok   events 200 verdict=ok
status/diff/rev-parse/ls-files/log 501 capability_unavailable (unchanged, slice C5 named)
watch        501  capability_unavailable (scope=target mode=poll) — unchanged
frobnicate   400  op_unknown
```

### 5.2 The declared state at bind, and the seed

```json
{"channel_available": true, "events_total": 1, "last_event_age_ms": 997, "mechanism": "events",
 "mode": "poll", "paths_dropped_total": 0, "poll_interval_ms": 2000,
 "reason": "overflow: the server declared knowledge lost",
 "resyncs_from_gap": 0, "resyncs_total": 1, "seq": 1}
```

Read with §2.4 and `BFS-026-seed-order.txt`: the mount's handshake probes `events` **before** its
bind snapshot, so the ledger's first observation is the probe's, and the mount's own first poll
receives the seed `overflow` and re-establishes its view. `resyncs_from_gap: 0` is the field saying
it was not a sequence gap, and `reason` quotes the server's own word. Measured cost: one extra
whole-tree snapshot op at bind (`snapshot.source=snapshot-op, calls=1` in the mount's status).

The mount log, verbatim — the degradation is still declared, and now names a fallback that exists:

```
bunker-fs: bound to http://127.0.0.1:…/dav tree=… rev=rev:0 proto=HTTP/1.1 extensions="identity,if_match_refuse,rev,tree,op,watch"
bunker-fs: declared degradation: watch (scope=target mode=poll): no inotify watcher on this target; the declared poll form X-Bunker-Op: events carries the channel (mode=poll), or poll with HEAD/ETag
bunker-fs: resync (overflow: the server declared knowledge lost)
```

### 5.3 An agent-side edit IS seen, and the record says which mechanism delivered it

```
=== D. a path NEVER READ (BFS-025's shape), edited on the AGENT ===
  record before    : events=1 dropped=0 resyncs=1 seq=1 available=True mechanism=events
  agent bytes now  : 'never-version-2-EDITED-ON-THE-AGENT' (35 bytes)  [edit at +0 ms]
  read + 104 ms      : 'never-version-2-EDITED-ON-THE-AGENT'
  read + 505 ms      : 'never-version-2-EDITED-ON-THE-AGENT'   (control: still inside the declared window)
  channel reported : +1065 ms  (events=2 dropped=1 resyncs=1 seq=2)
  served fresh at  : +0 ms
  SUMMARY never.txt edit=0 control_fresh=True channel_ms=1065 fresh_ms=0

=== E. a path ALREADY READ (cached): the BFS-024 shape, with its control ===
  mount first read : cached-version-1   (so the path IS cached)
  record before    : events=2 dropped=1 resyncs=1 seq=2 available=True mechanism=events
  agent bytes now  : 'cached-version-2-EDITED-ON-THE-AGENT' (36 bytes)  [edit at +0 ms]
  read +   0 ms      : 'cached-version-1\n'
  read + 401 ms      : 'cached-version-1\n'   (control: still inside the declared window)
  channel reported : +1808 ms  (events=3 dropped=2 resyncs=1 seq=3)
  served fresh at  : +1811 ms
  SUMMARY cached.txt edit=0 control_fresh=False channel_ms=1808 fresh_ms=1811
```

**What each line is worth, stated plainly, because the probe measures the alternative too:**

* **During the channel's own record** it is the *server's* mechanism being reported: the counters the
  mount publishes (`events_total`, `paths_dropped_total`, `seq`) move at **+1065 ms** (never-read)
  and **+1808 ms** (cached), both inside the declared 2000 ms interval. That is the channel
  delivering, read from the mount's own status document — not from a read that might be explained by
  something else.
* **For the cached path**, the fresh serve is attributable to the channel and to nothing else: the
  reads at **+0 ms and +401 ms** were both stale (`control_fresh=False`) — *including a read inside
  the declared window*, so no other mechanism repaired the path — and the fresh bytes arrive at
  **+1811 ms, 3 ms after the event landed**. This is the row's GREEN: the path the release filed as
  never-invalidated is invalidated, and the record names the mechanism (`mechanism:"events"`,
  `mode:"poll"`).
* **For the never-read path the channel does NOT get credit.** That read succeeded at **+104 ms**,
  *before* the channel spoke, with `control_fresh=True`; §4 shows the same repair happening in the
  build with no channel at all. It is the read path's own recovery from BFS-025's rule. The channel
  reports that edit too (+1065 ms), and the counters move — but a probe that claimed the never-read
  fix for the channel would be claiming the wrong mechanism, so this one says so.

### 5.4 The event itself, by hand

```
$ bunker fs op events --url $URL --arg since_seq=2
# op=events verdict=ok status=200 round_trip=3ms (server compute 0 ms)
{"count":1,"events":[{"seq":3,"event":"invalidate","paths":["cached.txt"],"rev":"rev:0","tree":tree:…}],"head_seq":3,"scanned":10}
```

One path, precisely: that is what lets the mount drop one entry instead of re-snapshotting the tree.

### 5.5 On the tree shape the product actually serves (a git working tree)

`BFS-026-git-tree.txt`:

```
seed    count=1 head_seq=1 scanned=66   overflow   seq=1 paths=0
quiet   count=0 head_seq=1 scanned=66
edit    count=1 head_seq=2 scanned=66   invalidate seq=2 paths=1  README.md
commit  count=1 head_seq=3 scanned=72   invalidate seq=3 paths=22 (.git paths: 22) .git,.git/COMMIT_EDITMSG,.git/index,.git/logs/HEAD…
after   count=0 head_seq=3 scanned=72
```

An agent edit arrives as **one** path; a `git add && git commit` arrives as **22** paths, every one
of them git metadata — well under the 4096 cap, so it is a precise drop list and not a resync. That
noise is the honest consequence of observing the tree rather than being *told* about changes:
a `gc`-class operation on a large repository could exceed the cap and produce an `overflow`
(a resync, which is always safe). Named as residual R5.

### 5.6 The instrument's own defect, found and fixed (because a green probe proves nothing if it cannot fail)

The first version of this visibility measurement polled the mount's status file with a loop that
took ~0.26 s per sample; it reported "the channel NEVER reported the change (held open 52461 ms)"
in the GREEN while the counters demonstrably had moved. A second diagnostic — my own, sampling the
pretty-printed JSON with a single-line regex — read `None` for every field, because the JSON is
indented. Both were instrument defects, not product behaviour, and both were found by refusing to
accept the first answer:

* the sampler now runs as **one process** that reads the record and prints only changes, on the same
  clock as the edit and the reads (`visibility.py`);
* the probe measures the **control** (a read inside the window, before the channel) so it can tell
  "the channel did it" from "something else did it" — which is exactly how the never-read
  attribution above was caught.

---

## 6. The negative control (an instrument that must be able to fail)

`mutation-red.sh`, transcript **`BFS-026-mutation-red.txt`**: the mechanism is disabled — `events`
reverted to unserved — and **every** arm must go RED.

**It caught my own defect on the first run**, which is why this file is worth its lines:

```
=== ARM 1 — the live channel arm against the MUTATED build ===
  ARM 1: the arm stayed green with the mechanism disabled — it proves nothing
```

The mutation had removed `events` from `implementedOps`, and the channel still delivered: that table
is a **report** (it feeds the capability document and the degradations list) while the handler's
switch is the **gate**. Two sources of truth, and I had mutated the one that does not gate. Two
things followed:

1. the mutation now reverts both halves — the exact state the row was filed against;
2. `TestOpTableMatchesWhatIsServed` (new) pins the agreement: for every name in the op catalogue, a
   `200` must be advertised in `extensions.op.ops` and a structured `501` must not be. Without it, a
   served op could be advertised as missing and a client would never use a mechanism that exists —
   §4.2 rule 1's failure mode, in the direction nobody checks.

With the gate reverted:

```
=== ARM 1 — the live channel arm against the MUTATED build ===
  VERDICT-LINE label=MUTANT mode=poll mechanism=events channel_available=False events_total=0 paths_dropped_total=0 resyncs_total=0
  VERDICT-LINE cached-path: SUMMARY cached.txt edit=0 control_fresh=False channel_ms=none fresh_ms=none
  VERDICT-LINE reason=poll call failed: op:events : … status=501 verdict=capability_unavailable: no inotify watcher on this target; poll with HEAD/ETag
  ARM 1: RED as required (with the mechanism disabled the cached path is never served fresh)

=== ARM 2 — the client-side arm ===   --- FAIL: TestInvalidatorDeliversThePollFormOfTheChannel
                                      invalidate_test.go:111: the declared poll form is not served, so the channel has no mechanism (BFS-026)
=== ARM 3 — the server-side arms ===  --- FAIL: TestEventsOp… (op events -> 501 …)

RESTORE VERIFIED: ops.go   sha256=90400c31e588b20533e16367a47a21c98fd9658e7f34fff8256633f2cf683cf9 (unchanged)
RESTORE VERIFIED: events.go sha256=b9a48b33a47e1db8f49f61a1081f660fcdd4507cc440278fdf6eb25053538196 (unchanged)
=== RED control reverted: the client-side arm must be GREEN again ===  ok
NON-VACUITY: PROVEN — every arm goes RED with the mechanism disabled and GREEN with it restored
```

---

## 7. What this row did not change

### 7.1 BFS-015's write precondition (requirement 3)

Re-run on this tree, verbatim (`BFS-026-bfs015-tests.txt`):

```
--- PASS: TestExpectedHashIsRevalidatedInsideTheCommit        (while_the_body_arrives, inside_the_commit, inside_the_commit_with_identical_bytes)
--- PASS: TestCreateOnlyRuleIsRevalidatedInsideTheCommit      (inside_the_commit, before_the_precondition_is_evaluated)
--- PASS: TestCommitSectionIsExclusive
--- PASS: TestConcurrentConditionalWritesCannotBothLand
```

The commit-time re-validation and its striped per-path lock are untouched: the new code reads the
tree, never writes, holds no commit stripe, and adds no path into the write sequence.

### 7.2 Existing clients get exactly what they had (requirement 4)

`stock-parity.sh`, transcript **`BFS-026-stock-parity.txt`**: a 29-cell raw verb battery (OPTIONS
including the asterisk form, GET/HEAD/range/404, PROPFIND 0/1/infinity/bare, PUT create + stale
`If-Match` + `If-None-Match`, DELETE, MKCOL, COPY, MOVE, PROPPATCH, LOCK/UNLOCK, FROBNICATE, REPORT,
POST without an op / unknown op / a refused op) run against both builds over byte-identical fixture
trees with identical mutation ordering:

```
DIFF: EMPTY — every answer a client that ignores the channel can see is unchanged
PARITY VERDICT: PASS
```

The one intended difference, and who can see it:

```
document_version   : 1 -> 1        (a client that does not know it must fail closed)
methods            : unchanged
op.ops             : ['capabilities', 'snapshot', 'events']
watch block        : {"max_paths_per_event": 4096, "mode": "poll", "modes": {"poll": "X-Bunker-Op: events", …}}
degradations base  : [… 'op:events', 'op:watch', 'watch']
degradations new   : [… 'op:watch', 'watch']     (op:events is no longer a build-level absence)
(only a client that sends X-Bunker-Op sees any of this)
```

**Instrument note, because the first run failed and the failure was mine.** That first parity run
reported a non-empty diff: three PROPFIND bodies of different length and digests. Attribution
(`propfind-diff.sh`, kept in the probes) found the cause in the instrument, not the code — my two
fixture directories were named `tree-base` and `tree-new`, and a collection's `<D:displayname>` is
its basename, so one byte of the body was my own naming; and the digests were taken over RAW bodies
containing the run's tree token. Both were fixed (identical basenames; digest over the normalised
body) and the diff is empty. Reported because a green-looking parity run that compared nothing is
the failure mode this release keeps finding.

---

## 8. The bounds, measured (requirement 6)

`op-timing.sh`, transcript **`BFS-026-op-timing.txt`**; each bound against its own fresh server.

### 8.1 The cost of one poll

```
tree       = 2007 files, 8.0M
polls      = 20
min/median/mean/max ms = 6.2 / 7.3 / 9.9 / 53.6
declared poll interval = 2000 ms
headroom   = 275x the median poll cost fits in one interval
```

The observation is `stat`-per-path with no reads, so a poll's cost is bounded by the tree's entry
count, not its bytes. The max (53.6 ms) is the seeding poll or a scheduler blip on a loaded host —
reported rather than trimmed.

### 8.2 A large change is answered precisely, not as a resync

```
one poll over 1000 new paths: 9.8 ms
events: overflow(0)@1, invalidate(1002)@2      count=2 scanned=3012 head_seq=2 truncated=False
PRECISION VERDICT: HELD — the answer names every changed path (1002 paths, including the new directory) rather than re-syncing the tree
```

### 8.3 Past the DECLARED cap (4096), with the real cap

```
seeded the ledger: head_seq=1 ; 4100 new paths added ; one poll with the client's own cursor
events: overflow(0)@2      count=1 scanned=4111 head_seq=2 truncated=False
CAP VERDICT: HELD — past the cap the answer is one overflow with no path list (scanned=4111, paths carried=0), never a partial diff
```

A-13's declaration 2, honoured by the poll the client actually uses. The lowered-cap unit arm
(`TestEventsOpSpillsToOverflowAboveThePathCap`) keeps the boundary cheap in CI, and
`TestEventsOpDeclaredBounds` pins 4096/256/100000 so a test can never move the defaults silently.

### 8.4 Nothing is held open

```
HTTP/1.1 200 OK
Cache-Control: no-store          Content-Length: 383
Content-Type: application/json   X-Bunker-Op: events   X-Bunker-Verdict: ok
X-Bunker-Proto: HTTP/1.1         X-Bunker-Rev: rev:0  X-Bunker-Tree: tree:…
```

One envelope with a `Content-Length` — no `Transfer-Encoding: chunked`, no stream, no long-poll. The
not-hang standard is satisfied structurally rather than by a timeout: no request's duration depends
on a client, so a dead client cannot hold anything open. `--invalidation=push` still fails loudly
when the watcher is absent (unchanged client behaviour).

---

## 9. The filed neighbours: reported, not closed

| row | what this row's evidence shows | what this row does NOT do |
|---|---|---|
| **BFS-024** (a cached path serves pre-edit bytes) | §5.3 re-measures its exact shape with its control: stale at +0 ms, **stale at +401 ms** (nothing else repaired it), the channel's record moving at **+1808 ms**, and the fresh bytes at **+1811 ms**. The mechanism that row needs now exists and is named in the record. | **Not closed.** Its row owns its own acceptance (including the attribute/never-read shape), and its criterion is about the cache's behaviour rather than the channel's. |
| **BFS-033** (a refused write recorded then bypassed) | Untouched. §7.2 shows the `412 hash_mismatch` refusal answered identically by both builds, and no code in this row goes near the write path. | Nothing. Not mine here. |
| **BFS-025** (truncated reads) | **The channel is NOT what fixes this, and the probe says so.** In the RED the never-read read is refused (`read bound divergence never.txt: published=16 content=35 refused=true`) and a read 0.4 s later serves the new bytes with no channel at all; the GREEN shows the same repair at +104 ms, *before* the channel spoke. It is the read path's own recovery, and this row claims no credit for it. | Not closed, not touched: `bc1793b`'s refusal is unchanged and is what catches a read the mount cannot explain. |
| **BFS-009 F4** (the `mechanism` label names the mechanism the client prefers, not one that answered) | Still true in the refused case: `mechanism:"events"` while every call is refused (the RED record and the new arm `TestInvalidatorStaysHonestWhenThePollFormIsRefused`). The state stays honest (`channel_available:false` plus the server's refusal verbatim), and with this build's server the label is now *true*, because `events` is what delivers. | Not changed. Selecting `rev` instead would make the record look greener while `rev` cannot see an agent-side edit at all (BFS-009 §3.2); the label question stays where BFS-009 left it. R1 reports what `rev`'s record looks like. |

---

## 10. Residuals, and what I did not prove

**R1 — the `rev` fallback's record is coarse, and is reachable without the bind handshake.** With no
prior `Handshake`, the invalidator cannot know the poll form is declared, so it takes the last-resort
revision poll. Measured while writing the honesty arm:

```
{Mode:poll Mechanism:rev Seq:0 LastEventAge:17.75ms Events:19 DroppedPaths:0
 Available:true Reason:capability_unavailable on watch (scope=target mode=poll): declared poll fallback}
```

`channel_available:true` with `events_total:19` (revision polls counted as events) while `rev` cannot
see an agent-side edit. In this product it is unreachable — `bunker fs mount` always handshakes
before running the invalidator — but it is one call away for any other caller, so it is reported.

**R2 — the mount-start overflow.** A fresh mount re-establishes its view once (§5.2), costing one
extra whole-tree snapshot op. The price of not answering "nothing changed" before the ledger has
looked; the alternative is a silent window at bind. Named, measured, deliberate.

**R3 — the pushed form is still not served.** `watch` needs an inotify watcher on the target (absent
here) or a synthetic one, plus a bounded stream with heartbeats and dead-client handling. This row
implements the mode the spec and the client declare for exactly this target class.

**R4 — the other slice-C5 ops.** `status`, `diff`, `rev-parse`, `ls-files`, `log` still answer
`501 capability_unavailable` naming their slice; identical between the two builds (§7.2).

**R5 — git metadata noise, quantified.** 22 paths per commit on the fixture (§5.5), all under
`.git`. A pathological operation on a large repository could exceed the 4096 cap and produce an
`overflow` — a resync, correct but whole-tree. Not optimised here: excluding `.git` from the
observation would make the channel blind to changes under it.

**R6 — the client's cache still has no *reported* content-age bound.** `--cache-max-age` is the
backstop and `last_event_age_ms` now tells the owner how long since the channel spoke, but the age of
a specific cached entry is not in the status document. That was option (b)'s one genuine
contribution; it is not in this row.

**Not proved, named:**

* **Loopback only, one host, one DC.** Every number here is `127.0.0.1` over plain HTTP/1.1.
* **No h2/h3 arm for the new op.** The mechanism has no version branch (a `POST` op in the shared
  `handlePost`), the existing `TestSameSurfaceOverHTTP1AndHTTP2` battery still passes, and the live
  client arm runs over HTTP/1.1 — but I did not add the op to the per-version battery deliberately:
  that battery compares bodies byte-for-byte after normalising only `proto`, and BFS-009 F6 already
  attributed a load-sensitive flake in its op cells to `duration_ms`. Adding a cell would have
  re-armed that flake in the fleet's commit gate.
* **Two clients polling one ledger.** The ordering is structural (§2.3) but only one client was live
  in every arm.
* **A large event on the client's drop path.** The 1000-path answer was measured server-side (§8.2);
  the live mount's drop loop was exercised with 1- and 2-path events.
* **The `!linux` identity.** The ctime-less fallback compiles (cross-GOOS PASS) and its gap is
  documented; no Windows host ran it.
* **A long-running mount (hours).** Every arm is seconds-to-a-minute long; the journal bound was
  exercised by lowering it in a unit test, not by 256 real events on a live mount.
* **The never-read path's refusal rate.** Whether the immediate read is refused (RED) or served
  (GREEN) differed between runs; the read path's behaviour there is BFS-025's business, which is why
  this row reports both and attributes neither to the channel.

---

## 11. Reproducing this

```bash
WT=/home/kara/worktrees/bunker-BFS-026
BASE=$(mktemp -d)/base                      # the tree as the row was filed
git clone --shared ~/bunker $BASE && git -C $BASE checkout -d 46a5350
(cd $BASE && go build -o bin/davserve ./probes/davserve && go build -o bin/bunker ./cmd/bunker)
(cd $WT && mkdir -p /tmp/bfs026-bin/new \
   && go build -o /tmp/bfs026-bin/new/davserve ./probes/davserve \
   && go build -o /tmp/bfs026-bin/new/bunker   ./cmd/bunker)
mkdir -p /tmp/bfs026-bin/base && cp $BASE/bin/{davserve,bunker} /tmp/bfs026-bin/base/

T=$(mktemp -d); python3 $WT/docs/evidence/BFS-026-probes/mkfixture.py $T
bash $WT/docs/evidence/BFS-026-probes/channel-arm.sh --label RED   --tree $T --bin $BASE/bin/bunker            --davserve $BASE/bin/davserve
bash $WT/docs/evidence/BFS-026-probes/channel-arm.sh --label GREEN --tree $T --bin /tmp/bfs026-bin/new/bunker --davserve /tmp/bfs026-bin/new/davserve

bash $WT/docs/evidence/BFS-026-probes/seed-order.sh   --davserve /tmp/bfs026-bin/new/davserve
bash $WT/docs/evidence/BFS-026-probes/git-tree.sh     --davserve /tmp/bfs026-bin/new/davserve
bash $WT/docs/evidence/BFS-026-probes/op-timing.sh    --davserve /tmp/bfs026-bin/new/davserve
bash $WT/docs/evidence/BFS-026-probes/stock-parity.sh --new-bin /tmp/bfs026-bin/new --base-bin /tmp/bfs026-bin/base
bash $WT/docs/evidence/BFS-026-probes/mutation-red.sh          # restores and sha256-verifies itself

cd $WT && go test ./... -count=1
go test ./internal/server/webdav/ -count=1 -run 'TestExpectedHash|TestCreateOnlyRule|TestCommitSection|TestConcurrentConditional' -v
bash probes/cross-GOOS-build.sh
```

Every probe kills by **explicit PID** (`pkill -f` appears nowhere in this row), bounds every
`fusermount` with `timeout`, uses `mktemp -d`, refuses to reuse an output directory, and its
mutation control restores what it mutates from a copy and sha256-verifies the restore.

**A note on the probes' `.py` files**, because the reproduction above names them: `.gitignore`
excludes `*.py`, so `mkfixture.py` and `visibility.py` are **force-added** here — as BFS-012's
evidence set does — so that the commands in this section actually run from a fresh clone. (Checked
because BFS-009's report names `BFS-009-probes/mkfixture.py` in its own reproduction section, and
that file is neither on disk nor in git: a landed row whose fixture generator cannot be re-run.
Reported, not fixed — that evidence set is not this row's to edit.)

**Artifacts in this evidence set**

| file | what it is |
|---|---|
| `BFS-026-invalidation-poll-form.md` | this report |
| `BFS-026-channel-red.txt` | the RED arm on the tree as filed: the record, the catalogue, the attempt rate, both stale shapes with their controls |
| `BFS-026-channel-green.txt` | the GREEN arm: the record, the delivered event, the controls, the delegated verb |
| `BFS-026-seed-order.txt` | the bind order a real client reaches the poll in, step by step |
| `BFS-026-git-tree.txt` | the poll on a git working tree: an edit, a commit, and the git-metadata noise quantified |
| `BFS-026-op-timing.txt` | the three bounds: poll cost, a 1000-path change, the 4096-cap spill |
| `BFS-026-stock-parity.txt` | the 29-cell stock-verb battery, base vs new, and the one intended difference |
| `BFS-026-mutation-red.txt` | the negative control, including the defect it caught in this change |
| `BFS-026-bfs015-tests.txt` | the re-run of BFS-015's commit-window tests |
| `BFS-026-probes/` | `channel-arm.sh`, `visibility.py`, `seed-order.sh`, `git-tree.sh`, `op-timing.sh`, `stock-parity.sh`, `mutation-red.sh`, `propfind-diff.sh`, `mkfixture.py` |
