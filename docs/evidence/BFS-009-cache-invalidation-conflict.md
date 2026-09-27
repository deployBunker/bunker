# BFS-009 — client cache + diff: the bound, the invalidation channel, the hash refusal

**Row:** BFS-009 (P1, complexity 3) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-26
**Depends on:** BFS-005 (the spec this implements), BFS-008 (the client)
**Product code changed:** **none.** The only code in this row is a test
(`internal/fsclient/conflict_test.go`, +145 lines, one new test with two arms). Everything else is
evidence and probes.

---

## 0. Verdict in one screen

| # | deliverable | result | where |
|---|---|---|---|
| **1** | the bound, measured and reported | **HELD, and the two figures agree EXACTLY.** Two arms: the default **256 MiB** bound against a **400.0 MiB** tree (**1.56×**), and an **8 MiB** bound against the same tree (**50.0×**). `du -sb` of the cache dir peaked at **267,519,668** and **7,381,779**; the client reported `used_bytes` peaks of **267,467,111** and **7,380,281**; both ≤ their bound in every one of 229 in-flight samples. `blobs_bytes` == `du -sb blobs` == the sum of the blob files' real sizes (delta **0**), `index_bytes` == `du -sb index.json` (delta **0**), and the only difference between `used_bytes` and `du -sb <cache dir>` is `status.json` itself, to the byte (1,506 and 1,498). Evictions 146 / 394. | §2 |
| **2** | an agent-side edit becomes visible | **FAILS, and the mechanism that delivered it is NONE.** The watcher answers `capability_unavailable` (scope=target); the DECLARED POLL FALLBACK runs — measured at **0.5 requests/s = one POST per 2 s interval** — and *also* answers `capability_unavailable` (this build serves neither `watch` nor `events`). The client declares this honestly (`mode=poll`, `channel_available=false`, the server's refusal verbatim), and the third mechanism (`rev`) **cannot see an agent-side edit either** — measured on both a plain tree and a git-rooted tree. Two measured consequences: a **cached** path serves pre-edit bytes (22 stale chars against 45 on disk), and a **never-read** path silently returns the first 23 bytes of a 60-byte file. | §3 |
| **3** | content-hash conflict refusal | **HELD, including the case the suite was missing** — an edit that preserves size **and** mtime. The refusal is `412` / `hash_mismatch`, names the post-edit hash, leaves the bytes unchanged, and a one-line mutation that makes the commit trust the metadata cache **fails the new test** (non-vacuity proven). | §4 |
| **+** | the `diff` feature | **NOT BUILT.** The client has a *delegating* verb (`bunker fs op diff`, with `--stat-only/--cached/--name-only/--no-renames`) and no diff engine of its own; the server answers `501 capability_unavailable` for the op, live, and the client reports that refusal verbatim and exits non-zero. No half-feature was invented. | §5 |

**Four defects found while doing this, none of them fixed here** (this row measures and proves; the
brief says report what I was not asked to fix). They are in §6, each with `file:line` and a
reproduction: a stale content identity on the read path (server), an un-cacheable read that is
invisible in every figure (client), **stale attributes that produce silent truncated reads** (the
sharpest consequence of the dead channel), and the `mechanism` label naming a mechanism this build
refuses.

---

## 1. What was measured, on what, and on what configuration

**The function under test** — the merged bunker-fs client and the landed WebDAV surface, from this
worktree, built at `57b12b7` plus the test file this row adds.

**The fixture** — deterministic, so every number below is re-derivable: `probes/BFS-009-probes/mkfixture.py`
builds it from each file's own path by a sha256 chain.

| property | value |
|---|---|
| path | `/tmp/bfs009/tree` |
| files | **601** (400 × exactly 1,048,576 B, 200 small text files, 1 README.md) |
| bytes | **419,437,925 = 400.0 MiB** (1.56× the default 256 MiB bound) |
| manifest | `manifest-sha256.txt`, sha256 **40b32ca8db7c698fdcd7c263f3969769c279076d4178fbd273532c446148494b** |

**The endpoint** — `probes/davserve` (the repo's own client-facing surface: the same
`internal/server/webdav` handler, no registry, no agent lifecycle) on **loopback, plain HTTP/1.1**,
no TLS, no h2c. The H1 constraint holds throughout: every request in this row is HTTP/1.1, and the
status documents report `"proto": "HTTP/1.1"`.

**The client under test** — `bunker fs mount` at `--concurrency 25` (bound arms) / `4` (probes), the
cache bound as stated per arm, and no other override. The read pattern in both bound arms is a
**serial `cat` loop over every file** — deliberately the worst case for the bound (each file is read
once, so every read is a fetch, an insert and an index flush) and deliberately not a concurrency
claim: BFS-016 already measured the concurrency lever and this row does not re-open it.

**The host** — 16 cores, `loadavg 26.74` at the start of arm A (sibling workers were running). Every
wall time below is under load, which inflates rather than flatters it, and none of them is used as a
performance result.

---

## 2. DELIVERABLE 1 — the bound, measured and reported

### 2.1 Arm A — the default bound (256 MiB) against a tree 1.56× larger

`bash bfs009-bound-arm.sh --label A256b --tree /tmp/bfs009/tree --read-set all` (no `--cache-max-size`:
this IS the shipped default). **601 files read, 0 failures, 69,616 ms.** A `du -sb` sample of the cache
directory and a read of the mount's own `status.json` were taken every 0.5 s **while the run was in
flight** (108 samples), so the number is a measurement during the run and not only after it.

```
BOUND       max_bytes (reported) :        268435456
            du -sb cache dir, MAX during the run : 267519668   (0.9967x the bound)
            used_bytes, MAX during the run       : 267467111   (0.9964x the bound)
AGREEMENT   reported blobs_bytes : 267394334   du -sb blobs    delta=0
                                real blob bytes  delta=0
            reported index_bytes :     72777   du -sb index.json delta=0
            reported used_bytes  : 267467111   == blobs_bytes + index_bytes: True
            du -sb cache dir     : 267468617   delta vs used_bytes = 1506
            status.json on disk  :      1506   <- the delta, EXACTLY
EVICTION    evictions_total 146   entries 454   blob files on disk 454   bypass 0   oversize 0
```

The full accounting is reproducible with `bfs009-bound-accounting.sh <run-dir>`; the transcript is
`BFS-009-armA-bound.txt`, and the 108 in-flight samples are `BFS-009-sample-A256b.csv`.

**A second, independent run of the same arm** (`--label A256`, before the probes' millisecond
formatting was fixed) reproduced it: `used_bytes` 267,467,305, `entries` 455, `evictions_total` 146,
max `du` 267,468,810, `du − used = 1,505 = status.json`. Two runs, same behaviour.

### 2.2 Arm B — an 8 MiB bound against the same tree (50.0× the bound)

`--flag "--cache-max-size 8388608"`. **601 files read, 0 failures, 71,099 ms** (slower than arm A, as
it must be: every read past the first seven now evicts and re-flushes the index).

```
BOUND       max_bytes (reported) :  8388608
            du -sb cache dir, MAX during the run : 7381779   (0.880x the bound)
            used_bytes, MAX during the run       : 7380281   (0.880x the bound)
AGREEMENT   blobs_bytes 7347522 == du -sb blobs == real blob bytes   delta=0
            index_bytes   32759 == du -sb index.json                 delta=0
            used_bytes  7380281 == blobs + index: True
            du -sb cache dir 7381779 - used_bytes 7380281 = 1498 = status.json on disk (exactly)
EVICTION    evictions_total 394   entries 207   blob files 207   bypass 0   oversize 0
            601 reads - 394 evictions = 207 entries, EXACTLY
```

**Stated honestly rather than smoothed:** the 8 MiB arm does **not** saturate its bound. The largest
entry is 1 MiB, so after seven of them (7,340,032 B) plus the index there is ~1.04 MiB the allocator
cannot use — the observed ceiling is 7,381,779 of 8,388,608 (88.0%). "Never exceeded" is the
requirement, and it holds; "reaches the bound" is a function of entry size and is **not** what this
arm shows. Arm A, with 255 one-MiB entries plus 199 small ones, sits at 99.6% of its bound.

### 2.3 What this proves and what it does not

**Proves:** the cap is enforced at insert over the bytes actually on the client's disk (`used_bytes` =
blobs + index); the read path grows the cache only on access; eviction happens (146 and 394); the
figure the client reports is the figure `du` sees, with the delta NAMED (a file the client does not
count as cache: its own `status.json`) rather than waved at as "block rounding". `evictions_total > 0`
as V-1 requires. The tree is 1.56× and 50.0× the bound respectively, so this is past the bound, not up
to it (BFS-016's arm was 40 files over a 64 KiB bound; this is 601 files over a 256 MiB bound and then
over an 8 MiB bound).

**Does not prove:** anything about `du` agreement under **concurrent multi-handle load** (every arm
here is a serial reader, so pins and in-flight writes are never in the picture); anything about the
write-handle temp files, which the mount keeps under the same cache directory and which
`used_bytes` therefore does not count — this row read-only, so that path was never taken (BFS-005
§5.4's deliberate deviation is the one place where `du` could exceed `used_bytes` by more than
`status.json`); and no arm ran on a second DC.

---

## 3. DELIVERABLE 2 — an agent-side edit, and which mechanism delivered it

### 3.1 The declared state, and which mechanism actually ran

At bind, against the landed surface (`bunker fs probe`, and the mount's own log):

```
watcher         : false
poll form       : false
degradation     : watch (scope=target mode=poll) no inotify watcher on this target; poll with HEAD/ETag or an X-Bunker-Op: snapshot diff
```

After the mount settles, its own status document — the surface the owner can see — reads:

```json
{"mode":"poll","mechanism":"events","poll_interval_ms":2000,"channel_available":false,
 "events_total":0,"last_event_age_ms":null,"paths_dropped_total":0,"resyncs_total":0,
 "reason":"poll call failed: op:events : errno=EOPNOTSUPP cause=server_error status=501 verdict=capability_unavailable: no inotify watcher on this target; poll with HEAD/ETag or an X-Bunker-Op: snapshot diff"}
```

**The fallback RAN, and is refused every time.** With no filesystem I/O of ours for 8 s,
`transport.requests_total` went **6 → 10** — 0.5 req/s, one POST per 2 s, which is exactly the
declared `poll_interval_ms`. Each of those POSTs carried `X-Bunker-Op: events` and got this, live:

```
capabilities 200    snapshot 200
status       501 capability_unavailable  scope=build  phase=C5
diff         501 capability_unavailable  scope=build  phase=C5
rev-parse    501 capability_unavailable  scope=build  phase=C5
ls-files     501 capability_unavailable  scope=build  phase=C5
log          501 capability_unavailable  scope=build  phase=C5
events       501 capability_unavailable  scope=target mode=poll
watch        501 capability_unavailable  scope=target mode=poll
bogus-op     400 op_unknown
```

**So: the watcher did not work, the poll fallback ran and was refused (the op it depends on is not in
this build — `implementedOps` is `{capabilities, snapshot}`), and nothing delivered an event.** The
client does not paper over it: `channel_available:false`, `events_total:0`, and the server's own
refusal text in `reason`. `mechanism:"events"` is the label of the poll form the client prefers; see
finding **F4** — it is chosen from the capability document's *declared* modes and the already-probed
`PollOpAvailable=false` is never consulted, so that one field overstates while the rest does not.

### 3.2 The third mechanism cannot cover the gap — MEASURED, both tree shapes

`rev` (OPTIONS + `X-Bunker-Rev`, one cheap request per interval) is the client's last-resort poll. It
is worth knowing whether it *could* have delivered the edit. It cannot:

| tree shape | token | after an edit ON THE AGENT | after a mutation THROUGH the surface | after a git commit |
|---|---|---|---|---|
| plain (no `.git`) | `rev:<counter>` | `rev:0 → rev:0` **NO MOTION** | `rev:0 → rev:1` moved | n/a |
| git-rooted | `git:<HEAD>` | `git:10aa4cf3… → git:10aa4cf3…` **NO MOTION** | `git:10aa4cf3… → git:10aa4cf3…` **NO MOTION** | `git:a38d18bf…` moved |

The probe sleeps **1.1 s between every observation**, longer than the surface's own 500 ms git-rev
cache (`internal/server/webdav/tree.go:30-31`): the first attempt read inside that window and would
have reported "no motion after a commit" for a value that was merely cached, which is the kind of
green-by-luck this fleet's evidence rules exist to catch, so it was re-measured rather than kept.

**Read `BFS-009-rev-motion*.txt` as the authoritative table, not the P1 block inside
`BFS-009-visibility-*.txt`.** The visibility probe also prints a rev-motion block, and in the git-arm
transcript its last line reads "after a git COMMIT … NO MOTION" — that is the surface's 500 ms cache
being sampled too soon by a probe that did not sleep, not a different behaviour. The dedicated probe
sleeps and shows the commit moving the token; both transcripts are shipped so the difference is
visible rather than papered over.

**Consequence, stated plainly:** on this build, no mechanism the surface serves can observe an edit
made on the agent. `watch` is absent, `events` is not implemented, and `rev` is a *surface-mutation*
token — a counter this surface bumps, or a git HEAD — so an out-of-band edit does not move it. The
client's design has a hole here that no configuration can close.

### 3.3 The two staleness shapes, separated because they are different failures

Both measured through a live mount, each with a native control read of the same file at the same
instant (transcript `BFS-009-visibility-nongit.txt`):

**A path ALREADY READ (so cached), replaced on the agent with different-length content**

```
mount (pre-edit) : 'arm-b-original-content'                     (22 chars)
agent now        : 'arm-b-REPLACED-with-different-length-content'  size=45
mount (post-edit): 'arm-b-original-content'                     (22 chars)   <- STALE
NATIVE control   : the 45 bytes above
status.cache     : entries=1 blobs=1 (unchanged by both reads)
```

**A path NEVER READ through the mount (only `stat`ed), replaced on the agent**

```
mount stat (pre) : size=23
agent now        : 60 bytes
mount stat (post): size=23                                       <- STALE attributes
mount read       : 'arm-a-REPLACED-with-a-m'                     (23 chars of 60)
NATIVE control   : the 60 bytes
```

The second one is the sharpest form: **the mount silently returned the first 23 bytes of a 60-byte
file and exited 0.** Nothing is cached for that path (the `stat` before the edit does not fetch the
bytes), so this is not staleness of a copy — it is the kernel being told the file is 23 bytes long
from a snapshot that was taken at bind and is never refreshed. A build reading a file through this
mount would consume a truncated input and succeed. See finding **F3**.

### 3.4 One more measurement the row's "which mechanism" question needs

The declared fallback's *attempt* is itself part of the evidence, so it is measured rather than
inferred: with the client's own log at `--verbose` there is no line claiming an event was applied, and
`InvalidationState.events_total` stays **0** for the whole session in every arm, while
`paths_dropped_total` and `resyncs_total` stay 0. A channel that delivered nothing and says so is
worth more than one that silently retries forever.

---

## 4. DELIVERABLE 3 — content-hash conflict refusal

### 4.1 The tests that exist

`go test ./internal/fsclient/... -count=1` — **19/19 pass**, including the whole of
`conflict_test.go` (stale base, absent target, identical-content no-op, the narrow
`overwrite-if-unchanged`, the four-row base-resolution table, the unconditional-PUT compatibility
rule, the 422 body-hash check, one-request streaming).

### 4.2 The case they do not reach, and the new test

Every refusal test in that file edits through `os.WriteFile`, which moves the mtime. An
implementation that compared **(size, mtime)** would pass all of them and still clobber the case that
matters: an agent-side edit that preserves **both** — same length, mtime restored exactly. The new
test is `TestConflictRefusalIgnoresMtimePreservedEdit`
(`internal/fsclient/conflict_test.go`), and it asserts the fixture really is the trap (size equal,
mtime equal by `os.Chtimes`, bytes different) before asserting anything about the write.

| arm | what it pins | result |
|---|---|---|
| 1 | the default policy refuses; the refusal names the post-edit hash; the bytes are unchanged; the refusal is counted, recorded and on disk for `bunker fs conflicts` | **PASS** |
| 2 | `--on-conflict overwrite-if-unchanged` must **not** treat it as a metadata-only mismatch: we served these bytes, but the server's bytes are no longer those, so a retry would be the silent overwrite the spec forbids | **PASS** |

### 4.3 The new test is not vacuous — mutation RED, sha256-verified

The test claims the commit re-reads the **bytes**. So the mechanism was neutered: one short-circuit
inserted at the top of `tree.freshEntry` making it return the `(size, mtime)`-keyed cache entry when
the metadata has not moved (`internal/server/webdav/tree.go:203`, the path BFS-015 landed).

```
mutation test exit code = 1   (--- FAIL: arm 1: the write LANDED; a same-size, mtime-preserved edit must still be refused)
RESTORE VERIFIED: tree.go sha256=852bac8d8e0fd28f4243a8aa3f3a9a99f4808ae3104b8c458361fbae9ca695ce (unchanged)
restored test exit code = 0
NON-VACUITY: PROVEN
```

### 4.4 The same case, live, through the surface

```
agent bytes now  : 36 bytes, sha256 18e7b97be3…   (same size as before, mtime restored)
GET  body sha256 : 18e7b97be3…   X-Bunker-Hash: sha256:1e4617dd42…   <- the SURFACE's identity is stale
PUT  If-Match: "sha256:1e4617dd42…"
HTTP/1.1 412 Precondition Failed
X-Bunker-Verdict: hash_mismatch   X-Bunker-Current-Hash: sha256:18e7b97be3…   X-Bunker-Expected-Hash: sha256:1e4617dd42…
target bytes after the refusal: 18e7b97be3… (unchanged)
```

**Both halves are true and they must be read together:** the refusal rule HELD even when the base came
from the surface's own stale answer — the commit re-reads the bytes, so a stale identity cannot
authorise a clobber (this is BFS-015's fix doing exactly its job) — and the surface's *identity header*
was wrong for the bytes it was serving. The second half is finding **F1**.

---

## 5. The `diff` feature — NOT BUILT

The row asked whether the client-side diff exists, and to report it as not-built rather than invent a
half-feature. It is not built, and the scaffolding around its absence is honest:

* **Server, live**: `POST` + `X-Bunker-Op: diff` → **`501 capability_unavailable`**, `capability=diff`,
  `scope=build`, `phase=C5`, detail *"delegated whole-tree op not in this build (slice C5); read the
  tree with PROPFIND or X-Bunker-Op: snapshot"*. Also `status`, `rev-parse`, `ls-files`, `log`. The
  E-4 catalogue declares all nine; `implementedOps` serves two (`capabilities`, `snapshot`). Verified
  live rather than trusted from the note the brief carried, and **still true**.
* **Client**: `bunker fs diff` does not exist. There is `bunker fs op diff`, which is the delegated
  verb carrying diff-shaped arguments (`--stat-only`, `--cached`, `--name-only`, `--no-renames`,
  `--path`, `--ref`) plus every delegated op, and its entire behaviour is: send the op, print the
  server's refusal verbatim, exit non-zero. Live: `exit=1`, `OP REFUSED: diff status=501
  verdict=capability_unavailable … (the delegated surface needs the slice the refusal names; the mount
  is unaffected)`.
* **No diff engine anywhere**: a grep for hunk/`@@ -`/unified-diff/LCS machinery in
  `internal/fsclient/*.go` and `internal/cli/fs.go` returns nothing but comments about FUSE WRITE
  chunks. `DelegatedOps` lists `diff` as a name to delegate; nothing computes one.

So a caller cannot obtain a diff from this release, by any path: not locally (there is no engine) and
not from the agent (the op is not implemented). That is the finding, and it is the whole finding.

---

## 6. DEFECTS FOUND, NOT FIXED (each with `file:line` and a reproduction)

This row measures and proves; the brief is explicit that a defect I was not asked to fix gets
reported, not silently fixed. None of the four below is fixed here.

### F1 — the surface's content identity is stale for an edit that preserves size and mtime

`internal/server/webdav/tree.go:149-185` (`hashFile`, the `(size, mtime)`-keyed identity cache),
reached from `handler.go:346` (`handleGet`), `:792` (`hashIfRegular`, whose callers include HEAD) and
`:447` (the request-time precondition). After a same-size, mtime-restored edit the surface serves the
**new** bytes under the **old** `ETag`/`X-Bunker-Hash`, and `HEAD` reports the old hash:

```
curl -sS -I $U/src/small/s0000.txt | grep X-Bunker-Hash
  -> sha256:1e4617dd42…      while   curl -sS $U/src/small/s0000.txt | sha256sum
  -> 18e7b97be3…
```

Why it is a defect worth a row rather than a footnote: the client's `resolveBase` uses `HEAD` as its
fetch-then-check path (`internal/fsclient/write.go:150`), so a client that asks the server for the
current hash can be handed a base the server will then refuse — the refusal is safe, but the *base it
was told to use* was wrong, which is precisely the drift between "what I served" and "what I expect"
that the content-addressed cache exists to prevent. BFS-015 closed the **commit** half of this with
`freshEntry` and named the read half as an out-of-scope residual; this row re-measured it and it is
still open. Not fixed here: it is the server surface (a different area), the fix needs its own RED
proof and a decision about what identity a served body may claim, and BFS-015 already recorded it as
residual rather than silent.

### F2 — a read the client could not cache is invisible in every figure

`internal/fsclient/cache.go:301-306`: the content-hash refusal returns `OutcomeBypass` and an error
**without touching a counter** — unlike the other two bypass classes, which are counted at `:313`
(`OversizeBypasses`) and `:331` (`BypassEvents`). `internal/fsmount/fs_linux.go:1054` discards the
error (`_, _ = h.m.cache.Insert(...)`). BFS-005 §3.4 says bypass must be reported *so that "the bound
is being respected by not caching anything" is visible rather than disguised as a healthy bound*; this
class is the one that is disguised.

Measured with a control arm, ground truth being the cache **directory** rather than the status
document (transcript `BFS-009-uncacheable.txt`):

```
ARM 1 (control) a plain never-read file, read through the mount:
  before: blobs_on_disk=0  control.txt_in_index= absent    (entries=0 blobs=0 blobs_bytes=0)
  after : blobs_on_disk=1  control.txt_in_index= present   (entries=1 blobs=1 blobs_bytes=14)
ARM 2 the un-cacheable read (surface hash sha256:1195d8cc… vs body sha256:de7c33de…):
  before: blobs_on_disk=1  target.txt_in_index= absent     (entries=1 blobs=1 blobs_bytes=83)
  mount served the NEW bytes -> it was a real GET, a real cache MISS
  after : blobs_on_disk=1  target.txt_in_index= absent     (entries=1 blobs=1 blobs_bytes=83)
          bypass_events=0  oversize_bypasses=0  misses=0   <- nothing moved anywhere
```

Without the control arm, "nothing moved" proves nothing; with it, the instrument demonstrably sees an
insert and demonstrably did not see this one. Not fixed here: the root cause is F1 (the surface lies
first), and making the counter move would add a figure for a case that should not arise — the honest
repair belongs with F1.

**Instrument note, because the transcripts show it.** The status document is written on a 1 s cadence
(`internal/fsmount/fs_linux.go` status loop), and the `BFS-009-visibility-*.txt` transcripts sample it
250 ms after a read, so a counter can appear one sample late there — e.g. P2a's 60-byte insert shows up
in the *next* status line. That lag is why this finding is decided by the **cache directory itself**
(blob files + `index.json`) and by a control arm, with 2-3 s sleeps, in
`BFS-009-uncacheable.txt`; the visibility transcript is not the instrument for F2 and is not used as
one.

### F3 — stale attributes from a never-refreshed snapshot produce SILENT TRUNCATED READS

`internal/fsmount/fs_linux.go:746` (`Getattr`) and `:766` (`Lookup`) answer from the node snapshot with
`out.SetTimeout(0)`; the snapshot is created empty at `:215` and refreshed only at bind (`:346`) or on
an invalidation resync (`:265`) — which, per §3.1, never fires on this build. So the size the kernel
uses to bound a read is the size at bind time.

Reproduction, and it is not a race — it is the steady state (`BFS-009-visibility-nongit.txt`, P2a):

```
mount stat (pre) : size=23
agent now        : 60 bytes
mount stat (post): size=23          <- stale
mount read       : 'arm-a-REPLACED-with-a-m'   (23 chars of a 60-byte file, exit 0)
NATIVE control   : the full 60 bytes
```

**A truncated read that reports success is worse than a stale read**, because the consumer (a build, a
`git add`, a `cat > file`) gets plausible input and no error. This is the sharpest consequence of the
dead invalidation channel, and it is why deliverable 2 is reported as a failure rather than a
configuration note. Not fixed here: the honest fixes (revalidate attributes on open, or a bounded
snapshot age, or the channel) are design decisions for BFS-008/BFS-012, not a patch this row can make
without changing the client's cost model, and this row's job was to measure the delivered behaviour.

### F4 — the reported `mechanism` names a mechanism this build refuses

`internal/fsclient/invalidate.go:446-451` and `:555-561` choose `MechanismEvents` from the capability
document's *declared* watch modes (`Capabilities.WatchPollOp()`, `:608-618`) and never consult the
availability the handshake already probed at bind (`internal/fsclient/capabilities.go:138-139` records
`PollOpAvailable=false`). The result is a mount that reports `mechanism:"events"` while POSTing an op
that answers 501 every two seconds, and that never falls through to `rev` — the one poll form this
build *does* serve. The wider state is still honest (`channel_available:false` plus the server's
refusal verbatim in `reason`), which is why this is a reporting defect and not a lie.

Not fixed here, deliberately: selecting `rev` would not make the declared property hold (see §3.2 —
`rev` does not move for an agent-side edit), so the change would buy a truer label and a cheaper poll,
not a working channel; and the decision about which mechanism to prefer belongs with whoever serves
`events`. It is recorded so the next row has it.

### F5 (cosmetic) — `bunker fs mount` prints `invalidation : auto`

`internal/cli/fs.go` prints `m.Status().Invalidation.Mode` immediately after `MountAt` returns, before
the invalidator's `Run()` has chosen a mode, so the owner's first sight of the new mount says `auto` —
neither of the two declared modes (`push`/`poll`). The status document settles to `poll` within a
second. No behaviour follows from it; noted so it is not mistaken later for a third mode.

### F6 — the fleet's own commit gate is load-sensitive (found by being blocked by it)

The first `gitreins guard` run of this row failed the `go_tests` lane:
`internal/server/webdav`, `TestSameSurfaceOverHTTP1AndHTTP2/op_capabilities`, *"body differs between
versions"*. `internal/server/webdav` is **byte-identical to HEAD** in this change set
(`git diff HEAD -- internal/server/webdav` is empty) and the test passes alone on this tree, so the
failure was attributed rather than retried: the cell compares the two version bodies byte-for-byte
after normalising **only** the `proto` key (`transport_test.go:200-216`, `blankProto`), while the E-4
envelope returned by a POST op carries **`duration_ms`** — a measured server-side elapsed time. The
field-level diff of the failing pair:

```
  DIFF  /duration_ms                     h1=2            h2=0
  DIFF  /proto                           h1='HTTP/1.1'   h2='HTTP/2.0'      <- normalised by the test
  DIFF  /result/capabilities/server/proto h1='HTTP/1.1'  h2='HTTP/2.0'      <- normalised by the test
```

Reproduced on demand under CPU contention (25 runs of the package with 10 burners → 1 failure, the
same cell). Measured independently of the test: **40 identical capability POSTs returned
`duration_ms` 0 thirty-nine times and 6 once**, with every other field byte-identical. So on a loaded
host this cell fails for a reason no diff can explain, and *every* op cell of that battery carries the
same exposure — a commit gate that fails on a pre-existing, load-sensitive comparison is worth more
attention than a rerun, because the honest response to it is indistinguishable from the dishonest one.
Not fixed here: it is the server package's test, outside this row's change set. Transcript and
reproduction committed as `BFS-009-guard-flake-attribution.txt`.

---

## 7. What I did NOT prove, and what is a claim rather than a measurement

**Not proved, named:**

* **Loopback only, one host.** The release's own rule (both DCs) is not satisfied by this row. Every
  number here is `127.0.0.1`, plain HTTP/1.1, no TLS.
* **The watcher path is unexercised.** This build cannot serve `watch`, so I never measured a pushed
  event, an applied drop, or the kernel notification half (V-5). What I measured is the *declared
  fallback* being refused.
* **The 1 h backstop TTL is untested.** I did not wait an hour to see whether a stale cached path is
  eventually re-fetched; the measured staleness is therefore "at least 3 s and for as long as the
  entry survives", not "bounded by `--cache-max-age`".
* **No concurrent-load arm on the bound.** Every arm is a serial reader; pins, in-flight write blobs
  and `MarkInFlight`/`MarkSettled` were never in play.
* **No write path through the mount in any bound arm.** `write_buffer_bytes` and the write-handle temp
  files under the cache directory are untouched, so the one place `du` could legitimately exceed
  `used_bytes` by more than `status.json` was not measured.
* **Arm A's tree is 1.56× the default bound, not 10×.** The row said "larger than the bound, go past
  it"; 1.56× is past it and the 8 MiB arm is 50×, but a 400 MiB tree against the 256 MiB default is a
  1.56× margin and I am not claiming more.
* **`--invalidation poll` / `push` were not exercised as separate arms.** Both would be refused or fail
  loudly (`push` by design), and one default (`auto`) arm already produced the `mode=poll` +
  non-null `poll_interval_ms` state V-3 asks for.
* **The live-server E2E battery on `bunker-mvp` was not run.** AGENTS.md's E2E class is agent
  spawn/destroy/exec/docker/SSH behaviour. This row's only code change is a test file, so I judge it
  **not in that class** — the judgment is stated rather than implied.

**Claims, not measurements:**

* *"A watcher would deliver an agent-side edit."* That is the design (BFS-004 §3 E-6 / BFS-005 §4), not
  something I observed; no build in this tree serves one.
* *"The rev token's semantics are HEAD-or-counter."* I read `internal/server/webdav/tree.go:270-278`
  **and** measured the behaviour on both tree shapes; the measurement is the evidence, the reading is
  the explanation.
* The fixture's `mkfixture.py` prints `total_bytes`; the first generation printed 419,437,930 while the
  files summed to 419,437,925 (a 5-byte accounting slip in the probe script). Fixed in the script
  before the final fixture was generated, and the final print matches `du` exactly
  (419,437,925) — recorded because a probe that mis-states its own fixture is exactly the class of
  instrument defect this fleet keeps finding.

---

## 8. Reproducing this

```
STAGE=/tmp/bfs009
python3 BFS-009-probes/mkfixture.py $STAGE/tree --bulk 400 --bulk-bytes 1048576 --small 200
sha256sum -c <(tr -d ' ' < BFS-009-probes/manifest-sha256.txt | awk '{print $1"  '$STAGE'/tree/"$2}')  # optional integrity check
go build -o $STAGE/bin/bunker ./cmd/bunker && go build -o $STAGE/bin/davserve ./probes/davserve

# deliverable 1 — the bound, twice
bash BFS-009-probes/bound-arm.sh --label A --tree $STAGE/tree --bin $STAGE/bin/bunker \
     --davserve $STAGE/bin/davserve --read-set all
bash BFS-009-probes/bound-arm.sh --label B --tree $STAGE/tree --bin $STAGE/bin/bunker \
     --davserve $STAGE/bin/davserve --read-set all --flag "--cache-max-size 8388608"
bash BFS-009-probes/bound-accounting.sh $STAGE/run-A

# deliverable 2 — the channel, the fallback's attempt rate, the two staleness shapes
bash BFS-009-probes/invalidation-arm.sh --label INV --tree $STAGE/tree2 \
     --bin $STAGE/bin/bunker --davserve $STAGE/bin/davserve
bash BFS-009-probes/rev-probe.sh --davserve $STAGE/bin/davserve --tree $STAGE/vistree-git
bash BFS-009-probes/visibility-probe.sh --bin $STAGE/bin/bunker --davserve $STAGE/bin/davserve \
     --tree $STAGE/vistree-ng --label ng
bash BFS-009-probes/uncacheable-probe.sh --bin $STAGE/bin/bunker --davserve $STAGE/bin/davserve --work $STAGE

# deliverable 3 — the suite, the new test, and the proof it is not vacuous
go test ./internal/fsclient/... -count=1 -run TestConflictRefusalIgnoresMtimePreservedEdit -v
bash BFS-009-probes/mutation-red.sh     # mutates tree.go, restores it, sha256-verifies, exits non-zero-proof

# the diff question
bash BFS-009-probes/diff-probe.sh http://127.0.0.1:18491/dav
```

Every probe kills its processes by **explicit PID** and bounds every `fusermount` with `timeout`; no
probe uses `pkill -f`, and none removes anything recursively (each run refuses to reuse its output
directory). `mutation-red.sh` copies the file it mutates and sha256-verifies the restore.

**Artifacts in this evidence set**

| file | what it is |
|---|---|
| `BFS-009-cache-invalidation-conflict.md` | this report |
| `BFS-009-probes/` | every probe script, plus `mkfixture.py` and `manifest-sha256.txt` |
| `BFS-009-armA-bound.txt`, `BFS-009-armB-bound-8MiB.txt` | the two bound arms, full transcripts |
| `BFS-009-sample-A256b.csv` | the 108 in-flight `du`/status samples of arm A |
| `BFS-009-invalidation.txt` | the channel's declared state, its attempt rate, the two staleness shapes |
| `BFS-009-visibility-nongit.txt`, `BFS-009-visibility-git.txt` | the mount-level staleness/truncation measurements |
| `BFS-009-rev-motion.txt`, `BFS-009-rev-motion-git.txt` | the rev-motion table, both tree shapes |
| `BFS-009-uncacheable.txt` | the un-cacheable read, with its control arm |
| `BFS-009-diff.txt` | the op catalogue live, and the client's delegating verb |
| `BFS-009-mutation-red.txt` | the non-vacuity proof |
| `BFS-009-guard-flake-attribution.txt` | the Tier-1 gate flake: arms A/B/C, the field-level diff of the two bodies, and the independent `duration_ms` variance measurement |
