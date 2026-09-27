# BFS-025 — the read the kernel cannot show, served anyway

**Status: fixed, with the RED proven on the tree as it stands and the fast path measured.**
**Scope check: BFS-024 and BFS-026 are left alone and are shown to be left alone (§7). BFS-018/019/020/021/030/033 are untouched.**

The row's summary was: *the client returns TRUNCATED reads, silently, with rc=0 — a STAT populates a size, and
a later read is BOUNDED BY THAT STALE SIZE.* That reproduction is exact and this row did not need to change a
character of it. The row's own hypothesis about the *site* ("a cached SIZE from stat being used to bound a
read") is **directionally right and mechanically wrong**, and the difference is what the fix had to be built
on. It was verified, not assumed, and the verification is in §2 with the raw output committed.

---

## 1. The reproduction, both trees, side by side

`docs/evidence/BFS-025-probes/driver-repro.sh`, run under `docs/evidence/BFS-012-probes/mount-arm.sh` (a
fresh mount per arm, everything killed by PID, every `fusermount` bounded by `timeout`):

| | the pre-fix tree (`bunker-red`) | the fix (`bunker-fixed`) |
|---|---|---|
| server side | `'SHORT'` (5 bytes) | same |
| mount stat (no read) | `size=5` | same |
| agent now | 82 bytes | same |
| mount stat (post) | `size=5` (stale) | same |
| **`cat` (splice)** | **`'X-REP'` (5 chars) `rc=0`** | **`''` (0 chars) `rc=1`, `cat: …: Stale file handle`** |
| `python` `read()` | 82 bytes OK | 82 bytes OK |
| native truth | 82 bytes | same |
| verdict line | `SILENT TRUNCATION — cat returned 5 of 82 bytes with rc=0` | `LOUD — cat failed (rc=1) instead of returning a fragment` |

Full logs: `BFS-025-driver-repro-red.txt`, `BFS-025-driver-repro-green.txt`.
The acceptance arm as a runnable check (exit code flips 1 → 0): `BFS-025-probe-red.txt`,
`BFS-025-probe-green.txt`.

---

## 2. The mechanism — measured, not assumed

### 2.1 It is a READER, not a byte count

Four readers, one fixture, a fresh mount each (`BFS-025-reader-attribution.txt`):

| reader | bytes it received | the mount was asked for | the mount served |
|---|---|---|---|
| uutils `cat` (splices) | **5** (`'X-REP'`), `rc=0` | `dest=4096` | 82 |
| `python` `os.splice` | **5**, `rc=0` | `dest=4096` | 82 |
| `python` `os.read` | 82 | `dest=65536` | 82 |
| `dd bs=4096` (read) | 82 | `dest=4096` | 82 |

The daemon was asked for a page-sized window in **both** the truncating and the non-truncating case, and
served **all 82 bytes** every time. `strace` of the reader (`BFS-025-reader-attribution.txt`, last block):

```
statx(… "/tmp/…/mnt/target.txt" … {stx_size=5, …}) = 0      <- the reader's own stat: the STALE size
openat(… "/tmp/…/mnt/target.txt", O_RDONLY|O_CLOEXEC) = 3
splice(3, NULL, 1, NULL, 1048576, 0) = -1 EINVAL            <- uutils cat's fast path needs a pipe
splice(3, NULL, 5, NULL, 1048576, 0) = 5                    <- file->pipe: CLAMPED at i_size = 5
splice(4, NULL, 1, NULL, 5, 0)       = 5                    <- pipe->stdout: the 5 bytes it got
splice(3, NULL, 5, NULL, 1048576, 0) = 0                    <- pos >= i_size: EOF, rc=0
```

**The truncation happens in the kernel, at the inode's `i_size`, on the splice/page-cache read path.** A
FUSE read reply longer than `i_size` is discarded past the bound; a plain `read(2)` is not clamped that way,
which is why the same mount serves the same bytes correctly to `dd` and `python` on the unfixed tree.

That has a consequence the fix had to respect: **the daemon cannot detect the clamp.** The request it receives
is a page-sized window either way (`dest=4096`), and after the clamped chunk the kernel asks for nothing — no
second request, no error, nothing the read handler can see.

### 2.2 Where the stale size comes from

The `BFS025` trace lines (temporary diagnostics, not in the tree) show the whole sequence: the first `stat`
on a path the snapshot does not hold costs ONE live `PROPFIND` and `Put`s the node (`size=5`); every later
`attrs` reply is answered from the snapshot with zero round trips (`snap_hit=true size=5`) and the node is
never invalidated, because the invalidation channel is dead (that is BFS-026, filed). The kernel's `i_size`
is set from those replies and to nothing else.

Control arm B (`driver-repro.sh` shape without the stat): the same file, the same replacement, **no stat
before the read** → `cat` returned all 82 bytes. **The stat is the trigger**, which is exactly the row's
report, and it also rules out "the cache lost bytes" as the mechanism.

### 2.3 What this row owns, and what it does not

* The **stale metadata** (a snapshot entry that never moves) is BFS-024/026's family, filed.
* What BFS-025 owns is the consequence: **a read bounded by a stale size is served as a fragment with rc=0.**
  That is a silent corruption of *new* data, not a stale read of old data — the bytes served are the first
  bytes of the NEW content, presented as a complete file.

---

## 3. The fix

The client cannot stop the kernel clamping, so it must not let the kernel hold a size it cannot defend:

> **The mount never serves content LONGER than the size it last published to the kernel for that path.**

* **longer → REFUSE, loudly.** `ESTALE` (116) with the named cause `stale_bound`, zero bytes, and the
  snapshot's entry corrected from the server's own answer — so the recovery `ESTALE` asks for (re-open the
  path, retry) succeeds. The `Detail` names both numbers.
* **shorter → serve, and correct.** Every byte the resource has reaches the reader (EOF is our own
  end-of-content, not the kernel's clamp); the metadata was demonstrably wrong, so it is corrected and
  counted rather than left to be discovered later.
* **equal → the fast path.** One integer comparison. No request, no byte, no log line.

**Where the bound comes from** (`internal/fsmount/bound.go`): the kernel's `i_size` changes in exactly two
ways in this filesystem, and the mount is the source of both — an **attrs reply** (getattr/lookup/create: the
kernel sets `i_size` from it) and an **acknowledged write** (the kernel grows `i_size` to
`max(i_size, end)`). So the registry records, per path, `published` (last attrs reply) and `written`
(largest acknowledged end since), and answers `max(published, written)`. A new attrs reply resets the write
growth, because the reply replaces `i_size`. Unknown (a path the kernel never asked about) is an answer:
no evidence, no refusal.

**Where it is checked** (`internal/fsmount/fs_linux.go`, `readHandle.load`), on both ways a read can get
bytes:

* a **cache hit** whose entry is LONGER than the bound is **distrusted**: the entry is dropped and the read
  goes to the server (RE-FETCH — the second half of the row's rule). A cached entry SHORTER than the bound is
  served (every byte it holds reaches the reader; whether those bytes are current is BFS-024, §7).
* a **fetched** content longer than the bound refuses as above. The fetched bytes are cached (they are the
  server's verified answer, so the retry is local) but deliberately **not** recorded as served — they were
  never handed to a caller, and the served-hash record is what the write path's conflict classification uses.

**Reported, not silent** (`read_bound` in the status document, `bunker fs status`):

```
read bound   : refusals_total=3 corrections_total=5
  last       : target.txt: published=5 content=82 refused=true
```

plus a mount log line per divergence. The counters count **divergences**, and a single reader can produce
more than one refused request (measured: `refusals_total=3` for one `cat`, which issues a read per clamp
window), which is stated here so the figure is not read as "3 tools were refused".

**New vocabulary, deliberately:** `Cause("stale_bound")` — ESTALE's class (a stale per-file handle,
recoverable by re-reading one path) with a cause that says what happened. Reusing `conflict` would have made
the status document describe a write conflict that never happened; the `CauseConflict` branch in the mount's
failure classifier exists precisely so a cause string is not a lie.

---

## 4. RED — the tree as it stands, with the driver's own reproduction

`probes/bfs025-truncated-read`, built from a pristine checkout of the release commit (`6dd6d0b`) with **only
the probe** copied in (`bunker-red` = `f3f248ad…`, `davserve-red`, probe `44c72753…`):

```
== arms 1/2/3: stat, replace, read (the driver's reproduction) ==
  stat (no read)     : size=5  <- this alone is what the kernel will bound a read by
  agent now          : size=82
  stat (post)        : size=5  (stale on both trees: the mount's metadata has not moved)
  splice read        : 5 bytes errno=<nil>
  *** FAIL: SILENT TRUNCATION: the splice reader received 5 of 82 bytes with rc=0
  re-open + reread   : stat=5 splice=5 bytes errno=<nil>
  ok: the unclamped control read all 82 bytes: the fragment is the kernel's bound, not a lost byte
…
BFS-025 ARMS: FAIL
  - SILENT TRUNCATION: the splice reader received 5 of 82 bytes with rc=0
RED_PROBE_RC=1
```

The retry arm is red on the same tree too (`stat=5 splice=5`): **on the unfixed tree the file is unreadable
correctly and permanently** through a splice reader — there is no recovery at all.

`BFS-025-probe-red.txt` is the full run. This is the arm the acceptance asks for: it goes RED on the current
tree with the driver's reproduction and GREEN with the fix, and it is a committed artifact, not a transcript.

**Why the RED arm had to be a live mount, and not a handler-level test.** Before the fix the read handler
returned every byte it held and the kernel threw the tail away; a unit test that drives `readHandle.Read`
would have passed on the unfixed tree and would have proven nothing. The row warns about exactly that trap,
and the honest answer here is the live arm (this is stated again in §9).

---

## 5. GREEN

`probes/bfs025-truncated-read` on the fixed tree (`BFS-025-probe-green.txt`, `rc=0`):

```
  splice read        : 0 bytes errno=input/output error
  ok: the read FAILED LOUDLY (input/output error) instead of returning a fragment with rc=0
  re-open + reread   : stat=82 splice=82 bytes errno=<nil>
  ok: recovered: the retry returned all 82 bytes and stat reports the true size
  ok: the unclamped control read all 82 bytes: the fragment is the kernel's bound, not a lost byte
  status.read_bound  : refusals_total=2 corrections_total=4 last="long-then-short.txt: published=82 content=5 refused=false"
  mount log          : bunker-fs: read bound divergence short-then-long.txt: published=5 content=82 refused=true …
```

The driver's tool sees **`Stale file handle`** (`ESTALE`, rc=1, zero bytes). Two errno surfaces are worth
naming honestly: uutils `cat` reported `ESTALE` (the daemon's errno); a raw `splice(2)` from Go saw `EIO`
(the kernel's own mapping for a read error on that path). **Both are loud and both deliver zero bytes** —
which is the requirement. The daemon's record always names `errno=ESTALE cause=stale_bound`.

Unit arms (all in the tree, all green):

```
ok  github.com/deployBunker/bunker/internal/fsmount   0.028s   (-run TestRead|TestWrite|TestCacheHit|TestBound)
```

---

## 6. What the fix costs on the fast path

**Zero requests, zero bytes, one integer comparison per read.** The rule was built so that the common case
(what the client holds has the length the kernel was told) does no work at all: no validation request, no
cache entry dropped, no metadata refreshed, no log line.

Measured with the same tree, the same reader and a fresh mount per arm, pre-fix vs post-fix
(`BFS-025-fastpath.txt`). The instrument that matters is the client's own request count, because the box's
wall clock is noisy at loadavg ≈ 10:

| arm | tree | pre-fix | post-fix |
|---|---|---|---|
| whole-tree read (40 files, 145 408 B, serial `cat`) | 40 files | **48 requests**, 4200 ms | **48 requests**, 4163 ms |
| metadata walk (40 `stat`s, no content) | 40 files | **8 requests**, 4213 ms | **8 requests**, 4253 ms |
| in-process read-all (`read(2)`, 3 runs each) | 40 files | 45 / 46 / 47 requests, 96 / 23 / 1282 ms | 46 / 46 / 47 requests, 28 / 379 / 2781 ms |

The request counts are identical (the ±1 is the mount's own status/poll request); the wall times overlap
completely and their spread is the host, not the fix. For contrast, the design this row *rejected* —
validating a file's metadata live on every attrs reply — would have added one round trip per `stat`, i.e. one
per file in the metadata arm and one per file in the read arm (a reader stats before it opens): 40 extra
requests on a 40-file tree, 40 000 on a 40 000-file tree, on the exact path the release measured as the
snapshot op's win.

**And the rule never fires when it should not.** On a normal workload — those same arms, 40 reads and 40
stats, with the fixture changed only out of band in the other arms — the fixed client reports:

```
read bound   : refusals_total=0 corrections_total=0
```

Zero refusals and zero corrections across the whole-tree read and the metadata walk
(`BFS-025-fastpath.txt`): the guard is a comparison that passes, never a request that validates.

### 6.1 The repo's own mount battery, run against the fixed binary

`probes/bunker-fs-battery.sh` (BFS-008's instrument, the one the release used) completes against a clone of
this tree served by `davserve`: **rc=0, every op group fully ok, 0 stalls** — 13/13 file ops, 13/13 of the
14-operation shape, the whole-tree read arms, git over the mount, the conflict case, the cache bound, the
transport kill. Its three op-level non-ok results are named rather than glossed: `ls src` is a fixture
mismatch (this repo has no `src/` directory for the battery's 120-entry expectation), and `mv`/`rm` fail on
the rename-before-publish defect the release already filed as BFS-020. The snapshot arm still reads the tree
in **0.010 s** against 0.226 s for the PROPFIND walk at the same concurrency, and nothing in the run was
refused by this change. Raw run: `BFS-025-battery-fixed.txt`, `BFS-025-battery-fixed.csv`.

---

## 7. BFS-024 and BFS-026 — left alone, with evidence either way

**BFS-024 (a path already read keeps serving pre-edit bytes) is NOT fixed, and the probe says so.** Arm 4 of
the acceptance probe is precisely its shape (read → replace → read again, with contents that differ in both
bytes and length, 16 → 38):

```
  phase 1 (before)   : read()=16 bytes "OLD-CONTENT-AAAA"
  stat (post)        : size=16 (want 40, the live content)
  phase 2 (after)    : splice=16 bytes errno=<nil> ; read()=16 bytes "OLD-CONTENT-AAAA"
  -> STALE, unchanged by this row: BFS-024 is a filed row with its own acceptance
```

The mount still serves the pre-edit bytes. A Go control test
(`TestCacheHitBytesAreUnchangedByTheGuard`) pins the property that keeps it that way: a cache hit whose
length AGREES with the bound is served verbatim — the guard does not touch the entry, and no refusal is
recorded for that arm.

**The one variant of BFS-024 this row does change, stated plainly:** when a cache entry's *length*
contradicts the published bound, that entry is no longer served at all — it is dropped and re-fetched
(`distrustCached`), and if the bound is still stale the read refuses. Before the fix, that combination was a
silent *fragment of stale bytes* (the kernel clamped it); now it is loud and re-fetched. That is not a fix of
BFS-024 — BFS-024's acceptance is "the mount serves the NEW bytes", and when the lengths agree that is still
not what happens — but it is a change in behaviour in that one corner and it belongs in this report, not in a
footnote.

**BFS-026 (the dead invalidation channel) is untouched.** No watcher, no push, no resync, no
`DropAll`; `invalidate.go` and the invalidation wiring are not edited by this change. The correction of a
snapshot entry is triggered by a read the client had to make anyway, and it corrects one path — it is not an
invalidation mechanism and it does not make one work.

**But it IS a step toward BFS-026, and the row asks for that to be said.** The client now *learns* "this path
changed on the server" from the content it fetches, and acts on it (correct the size, drop one cache entry).
An invalidation channel that is dead today is exactly the thing that would have told it earlier; this fix is
what the client does when nobody tells it. Building invalidation out of this discovery loop would be a
different row with its own acceptance, and it is not attempted here.

---

## 8. The mirror-image case: the published size LARGER than the live content

**Decision: serve it, correct it, report it — do not refuse.**

Rationale, in the order the row's rule implies them:

1. *Correctness first* is about **bytes**: serving shorter content under a larger bound delivers **every byte
   the resource has** — the reader hits our own end-of-content, not the kernel's clamp (the clamp can only
   bite BELOW the bound). No content is lost, so there is nothing to refuse.
2. *A loud error is an acceptable outcome* is a licence, not an obligation: refusing here would break a read
   that is complete, i.e. trade a usability loss for zero correctness gain.
3. The metadata LIE is not left silent: the entry is corrected from the served content, counted
   (`corrections_total`) and named in the log and in `bunker fs status` (`last`). A reader that trusts
   `st_size` over the content gets a short read at EOF — the POSIX-meaningful "no more data" — and the next
   `stat` is true.

Evidence: arm 5 of the acceptance probe (a path never read, so the content is a live fetch and the arm
measures the bound rather than the cache — the first draft of the probe did not do that and measured its own
leftovers, which is why the arms now use one file each):

```
  stat (published)   : size=82
  live content       : 5 bytes "SHORT"
  splice read        : 5 bytes errno=<nil>
  read()             : 5 bytes "SHORT" errno=<nil>
  ok: every byte the resource has was served, then a true EOF (no fragment of the larger published size)
  status.read_bound  : refusals_total=2 corrections_total=4 last="long-then-short.txt: published=82 content=5 refused=false"
```

plus `TestReadServesAndCorrectsWhenTheBoundIsLarger` at the handler level.

### 8.1 The arm next to the fix: write, then read (a regression check)

The rule reads the size the kernel holds, and the kernel grows `i_size` on every write it acknowledges — so
the arm that could break first is a read of bytes the mount itself just wrote. Run on both binaries with the
same reader (`write-then-read.sh`, evidence `BFS-025-write-read-red.txt` / `-green.txt`):

```
write              : rc=0
server side        : size=31
read back (cat)    : rc=0 'HELLO-WRITTEN-THROUGH-THE-MOUNT'  (31 chars)
WRITE-READ VERDICT: OK — the read of our own write was served in full
```

Identical on both trees: the fix does not refuse a read of the mount's own write.

---

## 9. What I did NOT fix, and the limits of this evidence

* **The kernel's `i_size` model is reconstructed, not read.** FUSE offers no way to read an inode's size, so
  the bound is the mount's model of it: `max(last attrs reply, acknowledged writes since)`. That model is
  exact for this filesystem because the mount is the only writer and the only responder for the path (§3);
  if a future change lets some other path grow `i_size` without an attrs reply, the model has to be extended
  with it. Named here as an assumption with its evidence, not as a proof.
* **A false refusal is possible, and it is loud.** If an attrs reply for a path grows the registry's bound
  between our read's decision and a reader's earlier open, the reader is refused although the kernel would
  now allow the bytes. It costs a retry, never a wrong byte. Stated because the counters are the audit trail
  and a support engineer will see it.
* **The counters count divergences per READ REQUEST**, not per tool: one `cat` produced
  `refusals_total=3` (the kernel asks once per clamp window).
* **The errno surface differs by reader** (uutils `cat` → `ESTALE`; Go `splice(2)` → `EIO`, the kernel's
  mapping). Both loud, zero bytes. The daemon's own record is `errno=ESTALE cause=stale_bound`.
* **The read-bound vocabulary is not in the spec.** `docs/spec/BFS-005-client-cache-and-diff.md` §7.1's errno
  table has no row for "the published size is stale, so the read would be a fragment" and no `stale_bound`
  cause. Adding that row is a design-authority edit to the spec, which this row did not make; the code names
  its cause so the table can be written from it.
* **Untouched neighbours**: BFS-018 (symlinks), BFS-019 (readdir empty), BFS-020 (rename before publish),
  BFS-021 (`>>` loses bytes), BFS-024, BFS-026, BFS-030 (`>` empties an existing file — the same write path;
  its arms were not touched), BFS-033 (the conflict refusal does not hold live).
* **The end-to-end arm is a live mount**, so it needs `/dev/fuse` and `fusermount` and it is not part of
  `go test`. That is stated rather than papered over: the defect is produced by the kernel's clamp, which no
  in-process test can reproduce, and a handler-level test that claims to would be the trap the row warns
  about.
* **The repo's live-server battery is not required for this surface.** `AGENTS.md` gates the
  `bunker-mvp` battery on changes to agent spawn/destroy/exec/docker/SSH behaviour; this change is in the fs
  client and the mount, so the arms here (live mounts against the repo's own landed surface, loopback) are
  the applicable verification. `go build`, `go vet` and `go test ./...` are green (28 packages ok, 0 FAIL),
  and `probes/cross-GOOS-build.sh` still reports PASS on all four required targets.

---

## 10. Reproducing this

```bash
# RED, on a pristine checkout of the release commit (6dd6d0b) + the probe only:
go build -o /tmp/x/bunker ./cmd/bunker && go build -o /tmp/x/davserve ./probes/davserve
go run ./probes/bfs025-truncated-read --work /tmp/x/arm     # exits 1: 5 of 82 bytes with rc=0

# GREEN, on the fixed tree:
go run ./probes/bfs025-truncated-read --work /tmp/x/arm2    # exits 0: loud refusal, then recovery

# the driver's reproduction, both readers, under the mount harness:
docs/evidence/BFS-012-probes/mount-arm.sh --label dr \
  --tree /tmp/x/tree --bin /tmp/x/bunker --davserve /tmp/x/davserve \
  --reader "$PWD/docs/evidence/BFS-025-probes/driver-repro.sh" --work /tmp/x --no-sample

# the fast-path cost (pre-fix binary vs post-fix binary, same tree):
docs/evidence/BFS-012-probes/mount-arm.sh … --reader "$PWD/docs/evidence/BFS-025-probes/whole-tree-read.sh" …
docs/evidence/BFS-012-probes/mount-arm.sh … --reader "$PWD/docs/evidence/BFS-025-probes/meta-walk.sh" …

# the rule itself:
go test ./internal/fsmount/ -run 'TestRead|TestWrite|TestCacheHit|TestBound' -count=1 -v
```

Committed alongside this report: `BFS-025-probe-red.txt`, `BFS-025-probe-green.txt`,
`BFS-025-driver-repro-red.txt`, `BFS-025-driver-repro-green.txt`, `BFS-025-reader-attribution.txt`,
`BFS-025-fastpath.txt`, `BFS-025-write-read-red.txt`, `BFS-025-write-read-green.txt`, and the arm scripts
under `BFS-025-probes/`.
