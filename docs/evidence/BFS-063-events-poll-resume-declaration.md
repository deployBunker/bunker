# BFS-063 — the `events` poll's off-the-end resume: a fresh client is told it cannot be vouched for, a seeded one is not

Row: `DEFECT (live, P0): the events poll's OFF-THE-END resume is a SILENT GAP for a freshly-bound
client` (source `BFS-041-spec-findings-2026-09-27`, hole H-10).

Everything below is a measurement taken on this tree. Every number quoted was printed by a command
shown next to it; nothing is remembered.

## 0. Verdict in one screen

**The fix is a DECLARATION, not a guard.** The server cannot tell a client that bound with a
whole-tree snapshot from one that has observed nothing by looking at the request it used to get:
both sent `since_seq: 0`. So the cursor becomes a resume declaration, and the server MINTED it —
the whole-tree `snapshot` answer now carries `result.head_seq`, the ledger cursor read **before** its
observation walk. A poll that presents no cursor at all declares *I hold no observation*, and it is
answered `overflow` (a full rescan), never a quiet tail. A poll that presents the minted cursor is
answered from the ledger as before.

| fact | before | after |
|---|---|---|
| a client that has observed nothing, quiet tree, journal seeded by another client | `{"events":[],"count":0,"head_seq":0}` — "nothing changed", `resyncs_total:0`, `channel_available:true`, its stale bytes still served | one `overflow` → resync → re-observation → per-path invalidations |
| a client whose cursor is `0` and whose journal has rotated past `seq 1` | a tail starting at `seq 3`, no marker, and its own gap rule cannot fire at zero | one `overflow` |
| a client that bound with a whole-tree snapshot | quiet tail (correct) | quiet tail, **unchanged** — the minted cursor keeps it from earning an overflow |
| a client with a cursor above zero that fell behind the retained journal | the retained tail, gap visible to it | the retained tail, **unchanged** |
| measured RED | `cached_sum=faa5b4816800` / `served_sum=342bf04cde9e`, `drops=null full=0 resyncs=null`, `channel_available:true` | (the arm above fails when the clause is disabled — §5) |
| the fix's own cost | — | one mutex + one `int64` read per snapshot call; 14 bytes of JSON (`"head_seq":0,`); the walk is untouched (§7) |

**Not what I did:** I did not "just delete the guard". Deleting it in the client would have made
every fresh client resync on every first poll and still left the cursor-`0`-with-rotated-journal case
unreadable; a server-side "cursor 0 ⇒ overflow, always" would have handed an unearned overflow to
every client that binds with a snapshot, which is the arm the row names as the regression risk. Both
are measured below (§4).

## 1. The case-separation rule, and why it is this one

The row asks the question precisely, so it is answered in the row's own three forms.

**Does the client declare it seeded a baseline? Yes — and the declaration is a cursor the server
issued, which is what makes it checkable.** `since_seq` present means *my view is the tree as of this
ledger cursor*; `since_seq` **absent** means *I hold no observation of this tree*. The cursor cannot
be invented: the only value that makes the first statement true is one the server minted for an
observation the client actually took (`snapshot`'s `result.head_seq`). A client that presents a
cursor it was never issued either reads as a gap (nothing in the journal below it is retained) or, if
it is above the ledger, is declared lost outright — the existing R-4 case, unchanged.

**Does the bind itself establish it? Yes — and it always did.** `seedEvents` has recorded the mount's
whole-tree snapshot as the ledger's baseline since BFS-026; what was missing was the client's half of
the same fact, so the bind now also HANDS BACK the cursor that observation corresponds to. The
ledger's baseline and the client's declaration are then two views of one observation rather than two
unrelated claims.

**Is cursor `0` meaningful only when a baseline exists? That is exactly the rule.** `since_seq: 0` is
a claim ("my view is the tree as of seq 0"), and the absent field is its negation. Without the split,
the two situations are indistinguishable from the request alone, and the honest answer to "can the
server tell them apart?" is **no** — which is why the distinction had to be added to the wire rather
than guessed at.

**The rule, as implemented (`internal/server/webdav/events.go`, `pollEvents`), in order:**

1. `cursor > head` → advance `seq` past the cursor, then `overflow` (R-4, unchanged).
2. **no declaration** (`!p.Baseline`) → `overflow`. The client has observed nothing, so no interval
   is vouched for it, and it has no cursor against which a partial tail could read as a gap.
3. **cursor `0` with `base > 1`** → `overflow`. This is the narrow case and it is H-10's window: a
   retained tail is self-describing only because the client's own monotonicity rule reads its first
   `seq` as a gap — and that rule is guarded on a non-zero cursor
   (`internal/fsclient/invalidate.go`: `ev.Seq > i.seq+1 && i.seq != 0`). When seq 1 is still
   retained the tail IS the whole history and needs no marker.
4. otherwise → the retained tail (SPEC-watcher-capability §2.2/§5.3 unchanged: for a cursor above
   zero the tail's first `seq` is above `cursor+1`, so the client reads the gap itself).

The overflow notice in cases 2 and 3 is **not journaled**: it is a statement about one request, not
an event in the tree's history. Journaling it would put it in every other client's tail — where it
would buy them a resync they did not earn. Its `seq` is the ledger's head, which is above the
presented cursor in every case that can reach there except one, named in the code: a cursor of `0`
against a ledger that has issued nothing at all (a freshly seeded quiet tree), where no `seq` above
the cursor exists and nothing can be confused with it because nothing else has been issued.

## 2. Which mechanism is AUTHORITATIVE for a fresh client

The row asks for this explicitly, and today the answer differs per mechanism. It does, and the
difference is real rather than a documentation gap:

| mechanism | authoritative for a fresh client? | why |
|---|---|---|
| **the `events` poll's cursor** | **YES — and it is the only one that carries a coverage claim.** After this row it is authoritative *as a declaration*: the client says what it holds, the server vouches for exactly that interval or says it cannot. | it is the ledger's own `seq`, shared with the push form by construction (SPEC-push-channel §3.1), and its answers are per-path |
| the watch heartbeat | **NO.** It is a liveness signal and nothing else: BFS-040 §5.1 states a heartbeat carries no path claim, and the landed client does not even record its `seq` (BFS-061 — deliberately NOT touched here). | a heartbeat that cannot advance a cursor cannot vouch for an interval |
| the `rev` token | **NO**, and it never was: BFS-048 measured that on a git tree it moves on committed ref movement only, so an out-of-band edit moves nothing. It answers "is my view the same revision I bound to", never "what did I miss". | no per-path detail and a declared gap (`rev_gap`) |

So for a fresh client the poll's cursor is authoritative, the heartbeat is liveness-only and the rev
token is a coarse equality check that must never be read as coverage. This answer **does depend** on
BFS-048 (which corrected the rev poll's own claim) and on BFS-061 (the heartbeat's `seq`, still
open); neither was fixed here, and nothing in this change depends on them being fixed — the poll no
longer needs any client rule to be correct.

## 3. What was implemented

**Server — `internal/server/webdav/`:**

- `events.go`: `resumePoint{Cursor, Baseline}`; the four-case resume rule above; `ledgerCursor()`,
  which reads the ledger's `seq` **before** an observation begins; `eventLog.overflow()`, the
  un-journaled notice; the op's doc comment now states the rule rather than the old
  "the client's own rule reads the gap" assumption.
- `ops.go`: the whole-tree `snapshot` answer carries `head_seq` (the cursor read before the walk), so
  a change the walk races lands above the cursor and is delivered rather than skipped.
- `handleEvents`: `since_seq` is decoded as a pointer and its presence IS the declaration; a negative
  cursor is still a `400 bad_arguments`.

**Client — `internal/fsclient/` and `internal/fsmount/`:**

- `snapshot.go`: `Snapshot.ObservationCursor() (int64, bool)` — set only for a whole-tree,
  untruncated snapshot-op answer (the PROPFIND fallback was never minted, and a truncated view is
  missing paths it does not know are missing).
- `invalidate.go`: `Invalidator.Observed(cursor, minted)` (the caller reports the view it holds);
  `Resync` clears the declaration (a dropped view is not a claimable one); `apply` records the
  cursor of an `overflow` notice so a *walk*-based re-observation can adopt it (sound because the
  observation happened after the notice); `pollEventsOnce` presents the declaration or omits it; the
  status record gains `resume_seq` (absent = "I hold no observation"), because a client stuck
  without an observation must not look like a quiet healthy channel.
- `fs_linux.go`: `refreshSnapshot` reports the observation (minted cursor, or the fallback), and the
  bind-time snapshot is reported before the channel starts.

**Docs, kept true rather than left to drift:** `docs/spec/BFS-004-webdav-surface.md` (the E-6 op row
and a new paragraph on the declaration), `docs/spec/BFS-005-client-cache-and-diff.md` (the poll-form
row), `docs/prd/SPEC-watcher-capability.md` §2.2 (three rows: the cursor-0 journal case, the resume
declaration, the minted cursor), `docs/prd/SPEC-push-channel.md` §10.2 (H-10's disposition).

## 4. The RED, on the tree as filed, reproduced as a measurement

Two levels, both driven by the real objects. The clause under test is disabled exactly as the
unfixed tree had it (`case false:` — `since_seq` absent and `since_seq: 0` were the same request),
and the whole file is sha256-restored afterwards (§6).

**4a. The client's own record and bytes** (`go test ./internal/fsclient/ -run TestBFS063 -count=1 -v`):

```
=== RUN   TestBFS063AFreshClientIsNeverToldNothingChanged
    invalidate_bfs063_test.go:156: a client that has observed nothing was not told the interval is
    unvouched: cached=the pre-edit copy is STILL CACHED cached_sum=faa5b4816800
    served_sum=342bf04cde9e drops=drops=null full=0 resyncs=null
    state={"mode":"auto","mechanism":"events","seq":0,...,"resyncs_total":0,"events_total":0,
    "paths_dropped_total":0,"channel_available":true,...}
--- FAIL: TestBFS063AFreshClientIsNeverToldNothingChanged (0.00s)
```

Read the three lines together, because each is a different claim:

- `cached_sum=faa5b4816800` vs `served_sum=342bf04cde9e` — **the bytes differ**. The client's cache
  still holds the pre-edit content while the served tree holds the post-edit content.
- `drops=null full=0 resyncs=null` — **nothing was dropped, no resync happened**. The client was told
  nothing about the interval.
- `channel_available:true`, `resyncs_total:0` — **the client reports itself current.** It has no way
  to learn that it is not; this is the silent gap, in the client's own numbers.

The window needs no journal rotation to open, and that is worth stating because H-10 was filed
against the rotation: BFS-026 deliberately records a whole-tree snapshot as an observation **without
emitting an event**, so an edit that lands before another client's bind is inside the ledger's
baseline and no event will ever name it. The rotated-journal form of the same defect is 4b.

**4b. The protocol's own answers** (`go test ./internal/server/webdav/ -run 'TestEventsOpSnapshotSeed|TestEventsOpCursorZero' -count=1 -v`):

```
    events_test.go:163: a client that has observed nothing was answered [], want one overflow
    events_test.go:454: cursor 0 with a rotated journal was answered
      [{Seq:3 Event:invalidate Paths:[README.md]} {Seq:4 Event:invalidate Paths:[README.md]}], want one overflow
```

`[]` is the "I observed, and nothing moved in the interval I am accountable for" answer
(SPEC-watcher-capability §5.1) given to a client that has observed nothing. The second line is the
retained tail starting at `seq 3` for a cursor of `0` — a gap the presenting client cannot read.

## 5. The GREEN — both arms, and the regression arm that guards them

**Arm (ii), the client with no observation** (`go test ./internal/fsclient/ -run TestBFS063 -count=1 -v`):

```
=== RUN   TestBFS063AFreshClientIsNeverToldNothingChanged
    invalidate_bfs063_test.go:184: BFS-063 measured: pre-edit=faa5b4816800 served=342bf04cde9e
    resume_seq=0 resyncs=1 gaps=0 drops=drops=null full=1 resyncs=["overflow: the server declared knowledge lost"]
--- PASS
```

One `overflow`, one full drop, one re-observation (`resume_seq=0` — the cursor the *re-observation*
was minted at), `gaps=0` (an `overflow` is the server declaring knowledge lost, not a client-detected
gap — the record keeps the two facts apart), the stale copy gone, and a read through the client
returns the served bytes. `TestBFS063OneUnvouchedAnswerThenConvergence` pins the cost: **one** resync
across four polls, then per-path invalidations — a fix that answered `overflow` to every poll of such
a client would be honest and useless.

**Arm (i), the seeded client — the regression risk** (`TestBFS063ASeededClientIsNotGivenAnUnearnedOverflow`):
a client that bound with a whole-tree snapshot presents the cursor its own snapshot answer minted,
gets a quiet tail (`resyncs=0`, no drops), and then receives the very next edit as a per-path
invalidate with no resync. The arm also asserts the bind cost **one** server call — the resume point
must not turn the one-call whole-tree read into a walk.

**The contrast arm** (`TestBFS063AStaleCursorThatTheClientCanReadIsStillATail`): a client that HOLDS a
cursor above zero, driven behind the retained journal by 300 events from another client, still gets
the retained tail and reads the gap itself (`resyncs_from_gap` moves). That is the documented §2.2/
§5.3 behaviour, and the fix must not have turned it into silence — nor into an overflow.

The full `TestEventsOp*` battery (12 arms, including the declared bounds, the path cap, the capping
walk, bad arguments and the wire shape) passes unchanged except where an arm's body now HAS to
declare the cursor it means.

## 6. The negative control

Disabling the clause (`case !p.Baseline || (p.Cursor == 0 && l.base > 1):` → `case false:`) turns the
arm red at both levels, verbatim in §4. Restore is sha256-verified:

```
$ sha256sum -c /tmp/bfs063-pre.sha256
internal/server/webdav/events.go: OK
internal/fsclient/invalidate.go: OK
internal/server/webdav/ops.go: OK
```

and the same three files are byte-identical to the hashes taken **before** the control. The arms pass
again after the restore.

## 7. What must NOT have changed, and the numbers

- **A client that ignores the channel sees exactly what it saw.** Nothing here touches the standard
  surface, the snapshot payload's entries, ETags, or any response a client that never calls `events`
  receives. The two request shapes that DO change answer differently, and both are the defect being
  closed:
  - a request with **no** `since_seq` (formerly identical to `since_seq: 0`) is now the "no
    observation" declaration;
  - a request with `since_seq: 0` whose journal has rotated past seq 1 (formerly a silent tail) is
    now `overflow`.
- **The one-call snapshot and the fast path do not regress.** `snap.Calls() == 1` is asserted in
  every client arm that binds. Cost, interleaved on the same fixture with the same test, base (HEAD)
  vs fixed — three rounds:

```
base : files=1000 token_memo_hit=101ns token_refresh=24.492µs whole_tree_read=39.58209ms  | files=10000 ... whole_tree_read=88.971949ms
fixed: files=1000 token_memo_hit= 62ns token_refresh=21.732µs whole_tree_read= 5.497003ms  | files=10000 ... whole_tree_read=97.452113ms
base : files=1000 token_memo_hit= 58ns token_refresh=19.711µs whole_tree_read= 5.464543ms  | files=10000 ... whole_tree_read=51.014887ms
fixed: files=1000 token_memo_hit= 55ns token_refresh=18.501µs whole_tree_read= 6.801266ms  | files=10000 ... whole_tree_read=68.603456ms
base : files=1000 token_memo_hit= 56ns token_refresh=18.492µs whole_tree_read= 4.382179ms  | files=10000 ... whole_tree_read=45.611758ms
fixed: files=1000 token_memo_hit= 53ns token_refresh=17.884µs whole_tree_read= 4.557173ms  | files=10000 ... whole_tree_read=48.939376ms
```

  Same band in every round (this host is concurrently running two other fleet workers' Go builds, and
  the 1000-file arm ranges 4.4–39.6 ms on the BASE tree alone). The added work is one mutex acquire
  plus one `int64` read per snapshot call and 14 bytes of JSON (`"head_seq":0,`) — independent of
  tree size, and invisible next to a walk that costs tens of milliseconds at 1000 paths.
- **`go test ./... -count=1` is green on the whole repo.**

## 8. What I did NOT touch, and what I did not prove

- **BFS-061 (heartbeat `seq`), BFS-062 (frame size), BFS-024/BFS-033 (read/refusal honesty) are
  untouched.** `git diff --stat` shows only the files listed in §3. If this work incurs on any of
  them it is by clarifying the poll's contract, not by changing their subjects.
- **The push form's half of the declaration is not landed.** `watchOnce` still sends `since_seq`
  unconditionally; the stream is not served in this build (501) and BFS-036 owns its build. The
  stream is *already* protected against the silent gap by R-2's unconditional marker (the reason this
  row is only about the poll) — but when BFS-036 lands it must carry the same declaration, or the
  stream will inherit the blindness this row just closed for the poll. **Named, not fixed.**
- **The PROPFIND-fallback path adopts a notice cursor rather than a minted one.** It is sound (the
  walk happened after the notice, so the tree it saw is at least as new as the ledger state the notice
  named) but it is a weaker claim than a minted cursor, and it costs one resync before the client can
  declare anything. A build that serves the snapshot op never takes this path (the op is served here).
- **Two harness gaps had to be closed to measure this honestly, and both are disclosure rather than
  cosmetics:** BFS-026's client harness had a no-op `OnResync`, so it modelled a client that can never
  re-observe (which is now — correctly — told on every poll that it cannot be vouched for); it now
  re-observes the way the mount does, and drops through the cache the way the mount does. An arm that
  measured a client that cannot exist would have been worse than no arm.
- **The notice's `seq` has one admitted corner:** a cursor of `0` against a ledger that has issued
  nothing at all is answered at `seq 0` (no higher `seq` exists), which a client implementing
  "discard anything ≤ my cursor" would drop. The landed client does not (it never enters the seq
  comparison for `seq <= 0` and still treats the `overflow` as a resync), and inventing a `seq` above
  the ledger would collide with the next real event — the worse failure. Reported rather than
  smoothed over.
- **The ledger is per tree, not per client.** The declaration makes one client's claim checkable; it
  does not (and cannot) prove that a *different* client's view is current. Two clients with different
  views on one tree is exactly what the declaration expresses, and what the server answers
  accordingly.
- **Not measured: the FUSE-visible consequence end to end** (a mount serving a stale byte through the
  kernel). The arms measure the channel, the cache and the read path in-process; the kernel's own
  caches are BFS-024's and BFS-054's subjects.

## 9. Reproducing this

```
cd /home/kara/bunker            # or this worktree
export GOCACHE=/tmp/bfs063-gocache     # the shared build cache is churned by sibling fleet workers
go test ./internal/fsclient/   -run TestBFS063  -count=1 -v
go test ./internal/server/webdav/ -run TestEventsOp -count=1 -v
go test ./... -count=1
```

For the RED: replace the clause in `internal/server/webdav/events.go` with `case false:` (the comment
removed with it), re-run the first two commands, then restore the line and check `sha256sum` against
§6's recorded hashes. The RED and GREEN transcripts used above are kept verbatim at
`/tmp/bfs063-red/{negative-control,green,restore,cost}.txt` on the box this ran on.
