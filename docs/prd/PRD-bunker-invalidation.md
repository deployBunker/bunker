# PRD — bunker invalidation: the watcher, the push channel, and the hot-file refresh

Status: **design, for Bane's confirmation** · authored 2026-09-27
Supersedes the invalidation sketch in `PRD-bunker-fs.md` §slice C5 (that section stays as history).
Rows: BFS-035 (watcher + server cache alignment) · BFS-036 (push form) · BFS-037 (hot-file refresh)
· BFS-038 (refresh atomicity) · BFS-039 (cancel-IO correctness)

---

## 0. What is true today (measured, not assumed)

- The client's invalidation path calls `X-Bunker-Op: events`. The server **refused it with a 501** and,
  before that, `watch` refused too — so the channel had **no mechanism at all**, and the cache never
  invalidated. That is the root cause behind the stale-read and bypassed-refusal defects.
- `events` is now **served as a poll**: one bounded stat-only observation per poll, diffed against the
  identity last seen — `(size, mtime, ctime)`, with ctime because an edit that preserves size *and*
  mtime still moves ctime. Measured cost: **7.3 ms median for 2000 paths**, with a real 4096-path cap.
- The **pushed `watch` form is still unimplemented and still refuses honestly**. That refusal is correct
  behaviour, not a bug — and it is the gap this PRD is about.
- So today the client learns about a change by **asking**, at a declared interval. It does not get told.
  **Your message is about closing exactly that gap**, plus the two things a push channel makes possible.

## 1. The design you specified, restated as requirements

**R1 — the watcher is on the FILESYSTEM, not on the request handler.** Not all writers are WebDAV. A
WebDAV-op-triggered invalidation would see only our own writes and would be blind to `git checkout`, an
editor, a cron job, a deploy — anything writing the target directly. **Therefore: a server-side inotify
watcher per target.** Confirmed, and it is the correct call: the current poll *does* see out-of-band
edits (mtime/ctime move regardless of who wrote) but it costs O(paths) per poll; a watcher costs
O(changes).

**R2 — the watcher aligns the SERVER's own cached state.** The server serves a tree identity
(`tree:<id>`) and a one-call snapshot. If a file changes out of band, that derived state is stale. The
watcher must **bump the served revision for changed paths**, so the server is not serving a stale
derived view, and so the client's `rev` poll has something meaningful to compare.

**R3 — the server exposes the change channel so CLIENTS can invalidate too.** The pushed form: a client
subscribes and is *told* which paths changed, instead of asking.

**R4 — a hot-file cache, small and bounded, per client.** Track which files *this client* reads and
edits most. On invalidation of a hot file, **proactively re-read it** so the next read is warm.

**R5 — a size rule.** Never proactively re-download a file above a limit. A 2 GB artefact must not be
pulled because someone touched it.

**R6 — a concurrency cap and a queue, with a full stop.** A burst that invalidates thousands of files
must not stampede. The refresh runs from a queue, with a maximum number in flight, and the queue must be
**stoppable in full**.

**R7 — pool-aware, yielding, and able to hang up.** The refresh shares the global connection pool. It
gets a slice of it, must **back off in favour of other files**, and may abandon a refresh entirely
rather than starve foreground work.

**R8 — no half files visible.** A reader must never see a partially refreshed file in the cache.

**R9 — promotion on direct access.** If a queued refresh is asked for by a real read, it **promotes to
full**: it leaves the queue, does not sleep, because it is now on the critical path.

**R10 — cancel-IO correctness, deliberate and accidental.**

## 2. Where the sharp edges are — the parts that will bite if unspecified

These are the things I would not let a worker decide alone, because each one fails *silently* by default,
and silent is the failure mode this whole project keeps finding.

### 2.1 inotify is lossy, and the loss is invisible
`inotify` can **drop events** when its queue overflows (`IN_Q_OVERFLOW`). A watcher that treats "no
events received" as "nothing changed" becomes a **silent-corruption machine** — strictly worse than the
poll it replaces. **Requirement: an overflow is a mandatory full rescan, and it is counted and reported.**
An interval the watcher cannot vouch for must never be reported as "quiet".

### 2.2 inotify is not recursive, and the watch limit is finite
Watches are per directory; renames across directories need both sides. The per-user watch limit is a
kernel setting (commonly 8,192; higher on tuned hosts) and **exceeding it fails, often quietly**.
**Requirement: the watcher is a capability that is probed, and exhaustion or refusal is reported as
`capability_unavailable` with the reason — exactly as `watch` refuses today.** Degrade to the poll. Never
claim to be watching when you are not.

### 2.3 inotify does not work on a network-backed target
If the target is itself a network mount (NFS/sshfs/fuse), the local kernel is not the writer and sees
nothing. **Requirement: detect it, report it, degrade to poll.** The existing refusal text — "no inotify
watcher on this target" — suggests this case is already suspected; it must be *identified*, not lumped in
with "no watcher".

### 2.4 The watcher must not be a correctness dependency for the client
The hot-file refresh (R4-R9) is a **performance** feature. Correctness must continue to come from the
channel plus read-path validation. **If the hot cache is wrong, reads must still be correct** — otherwise
we have built a new silent-corruption path and called it an optimisation.

### 2.5 Half files: swap, never mutate
R8 is not a locking problem, it is a **representation** problem. The refresh must write to a **new
immutable blob** and publish it with **one atomic pointer swap**. A reader either holds the old complete
blob or gets the new one; there is no third state. Corollaries:
- an abandoned or cancelled refresh **discards its blob and never swaps**;
- a reader holding the old blob keeps it alive (refcount), so eviction must respect in-flight reads;
- the size/eviction accounting must count published blobs, not in-flight ones.

### 2.6 Promotion must not double-fetch
R9 has a trap: a queued refresh *and* a direct read of the same path can both fetch it. **Requirement: a
per-path single-flight map**, so a promotion hands the *existing* in-flight fetch to the reader rather
than starting a second one. Without this, promotion makes the stampede worse, not better.

### 2.7 Bounded means reported
The staleness window, the queue depth, the number in flight, the number of overflows, the number of
refreshes abandoned for pool pressure — all must be **countable and visible in the client's status
record**. This is the same standing law as the cache bound: **a bound the owner cannot see is not a
bound.** Note this is not hypothetical here — BFS-031 found the cache bound does not bound the directory,
and BFS-032 found a counter that could never move.

### 2.8 Cancel is two different events
A deliberate cancel arrives as a FUSE interrupt. An **accidental** one — the reader dies, the mount is
signalled — may never arrive at all. **Requirement: every cache mutation is atomic and idempotent, so the
absence of a cancel is never corrupting.** And "cancelled" must be distinguishable from "failed" in the
error returned to the kernel (`EINTR` vs `EIO`), or callers cannot retry correctly. The write side
already has the striped per-path commit lock (BFS-015) — cancel must release it and leave the previous
content intact, never a half-published state. `>` on an existing file currently **empties it and then
fails** (BFS-030, open, P0) — that is the same class and must land with or before this.

## 3. The data type for "hot" (your open question)

You said the data type was off-mind. Concretely, and deliberately simple:

- **Exponentially decayed counts**, one per path: `score = score*decay + weight`, where a read is a small
  weight and an *edit* a larger one (an edited file is more likely to be wanted again than a merely read
  one). Decay-on-touch, so an old favourite falls out naturally and no sweeper is needed.
- Kept in a **fixed-size map** — the "small cache" is bounded in **entries** as well as bytes, so a repo
  with 400k files cannot make the tracker grow without limit.
- **Persisted per endpoint**, keyed the same way the cache dir is keyed, so it survives a remount.
- The tracker's own memory and the hot blob bytes are **separate bounds**, both reported.

I am not proposing anything cleverer. LFU with decay is the right shape here because the workload is a
working tree, where popularity is genuinely skewed and genuinely shifts.

## 4. Ordering, and why

1. **BFS-035 watcher** (R1-R2, §2.1-2.3) — the source of truth. Nothing else is meaningful without it,
   and its failure modes must be honest before anything depends on it.
2. **BFS-036 push form** (R3) — the client's way to hear about it. Its bound is independent of the
   watcher's: it must hold a subscriber without hanging on a dead one.
3. **BFS-038 atomicity** (R8, §2.5) — **before** the refresh, not after. Building the refresh on a
   mutate-in-place cache and fixing atomicity later means shipping the corruption first.
4. **BFS-037 hot refresh** (R4-R7, R9, §2.4, §2.6, §2.7) — the payoff, built on 2 and 3.
5. **BFS-039 cancel-IO** (R10, §2.8) — crosses all of the above; needs 3's representation and 1's
   liveness to be testable end to end.

## 5. What this does not change

- **The poll form stays.** It is the declared degradation and it now works; a host without a watcher
  must keep working exactly as it does today. Push is an upgrade, never a requirement.
- **No new server-side requirement for a stock client.** HTTP/1.1 keeps working; a client that ignores
  the channel behaves as now. (This is the release's own rule, applied to the channel.)
- **The watcher must not make the server's snapshot slower.** The one-call snapshot is the whole reason
  this project exists; R2 must not turn a stat-free snapshot into a stat-per-file one.
- **Honest refusal stays honest.** Where a capability is absent, the surface says so with a reason and a
  named fallback. It never claims a mechanism it does not have.
