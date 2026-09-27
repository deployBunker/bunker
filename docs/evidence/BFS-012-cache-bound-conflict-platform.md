# BFS-012 — the cache bound, conflict refusal, and the platform seam: measured

**Row:** BFS-012 (the last testing row of the release) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-26
**Product code changed: none.** The only committed code is two tests
(`internal/fsclient/cache_test.go`, `internal/fsclient/conflict_test.go`, +191 lines total). Everything
else is evidence and probes.
**Worktree:** `/home/kara/worktrees/bunker-BFS-012` at `994b5b8` (main `3a3305d` is one commit ahead and was
not touched). Nothing was pushed.

**Already proven, verified not re-derived:** `docs/evidence/BFS-012-stock-client-compat.md` (a real
third-party client: HTTP/1.1 on the wire, GET/HEAD/OPTIONS 200, PROPFIND 207, plain PUT 201 with the bytes
confirmed on the server's disk, DELETE 204, a stale `If-Match` refused 412 with a parseable `D:error`
carrying `b:hash-mismatch`, then the correct `If-Match` 204) and `docs/evidence/BFS-016-*` (the mount
battery, 0 stalls in 35 cells). This row extends both and re-measures neither.

---

## 0. Verdict in one screen

| # | the row asked for | result |
|---|---|---|
| **1** | a tree larger than the bound, read through, with the real on-disk size measured **and** the client's reported figure agreeing | **HELD.** The default **256 MiB** bound against a **640.1 MiB** tree (**2.5007×**): `du -sb` of the cache directory peaked at **267,521,528** and the client reported `used_bytes` **267,520,015** — both below 268,435,456 in **all 178 in-flight samples**. `blobs_bytes == du -sb blobs == the real blob files` (delta **0**), `index_bytes == index.json` (delta **0**), and the whole difference between `du` and `used_bytes` is the mount's own `status.json`, **to the byte** (1,513). 841/841 reads, **386 evictions**, 455 entries — and 455 + 386 = 841 exactly. | §2 |
| **2** | what happens when nothing can be evicted — the client must BYPASS, not fail | **HELD, and reported by the counter that names it.** Two live arms: **all cached blobs pinned** (`bypass_events` 3, `pinned_blobs` 3 observed while held, nothing over the bound, no blob left behind) and **every entry over the cap** (nothing cached at all). No read failed in either arm — 6/6 and 6/6. | §2.3 |
| **3** | conflict refusal from the CLIENT's own path, including a size+mtime-preserved edit | **SPLIT — and this is the row's most important result.** The client's *rule* is the content hash, not the metadata: `TestConflictRefusalIgnoresMtimePreservedEdit` still passes, and the new `TestTouchOnlyChangeIsNotAConflict` (mtime moved, bytes identical ⇒ the write must land) passes and goes **RED** under a metadata-derived identity (mutation, sha256-verified restore). But **live, through the mount, the refusal does not stop the mutation**: one `truncate(2)` syscall (strace: exactly one, rc=0) produces a `PUT`→**412** that is recorded in `conflicts.jsonl`, then a second `PUT`→**204** that **lands** — 1 syscall, 2 requests, caller sees success. Changing the same refusal's errno from ESTALE to EIO makes the second request disappear and the error reach the caller (measured). | §3 |
| **4** | cross-platform build, verbatim, including that it PASSES while printing the Windows CLI failure | **PASS, printed verbatim.** linux/amd64 and linux/arm64 build and the seam type-checks; **windows/amd64 and windows/arm64 do NOT build** — `internal/cli/umount.go:272` `undefined: syscall.Stat_t` (filed as BFS-028), carried as a printed `KNOWN_GAPS` entry, which is what makes the guard exit 0. No Windows build is claimed. | §4 |

**Four defects found, none fixed here** (§5): an in-place write of an existing file is unreachable
(EOPNOTSUPP); the oversize-bypass counter can never move from the live read path; the cache DIRECTORY
exceeds the bound at small bounds because two files are outside `used_bytes`; and the refusal/retry
interaction above.

---

## 1. What was measured, on what, and with which instruments

**The fixture** — deterministic and committed, because BFS-009's 400 MiB arm named a generator
(`probes/BFS-009-probes/mkfixture.py`) that was never committed with its evidence, so that arm cannot be
rebuilt from the repo. `docs/evidence/BFS-012-probes/mkfixture.py` builds the same tree on any host and
prints a total it re-derives from a fresh walk of what it wrote (a generator that mis-states its own
fixture fails loudly):

| arm | tree | files | bytes | vs the bound |
|---|---|---|---|---|
| A (default bound) | `mkfixture.py /tmp/bfs012/armA-tree --bulk 640 --bulk-bytes 1048576 --small 200` | 841 | **671,148,678 = 640.1 MiB** | **2.5007×** the 256 MiB default |
| PIN / OVER | `mkfixture.py /tmp/bfs012/small-tree --bulk 6 --bulk-bytes 1048576 --small 8` | 15 | 6,293,894 | 1.5× a 4 MiB bound |
| conflict | `mkconflict_tree.py` (3 files, 64/63/63 B) | 3 | 190 | — |

The row asked for a tree larger than the bound: BFS-009's own arm A was 400.0 MiB against the same
256 MiB default (§2.1 there, transcript `BFS-009-armA-bound.txt`, probe `BFS-009-probes/bound-arm.sh`,
both committed on main). That premise of this row is therefore already false in the committed record — but
the *fresh* arm here is 2.5× rather than 1.56×, and more importantly BFS-009's arms both reported
`bypass=0 oversize=0`, i.e. **neither it nor BFS-016 ever reached the case where nothing can be evicted,
which is the part the owner's constraint actually turns on** (§2.3).

**The endpoint** — `probes/davserve`, the repo's own client-facing surface (the same
`internal/server/webdav` handler, no registry, no agent lifecycle), on loopback, plain HTTP/1.1, no TLS.
Every request in this row is HTTP/1.1; the status documents report `"proto": "HTTP/1.1"`.

**The instruments, and why three of them.** A bound nobody can see is not a bound, so every arm carries
all of:
* `du -sb` of the cache directory, sampled **while the run is in flight** (`sample.py`), plus the REAL file
  sizes of `blobs/` and `index.json` so "files" and "du" are never confused;
* the client's own `status.json` figures (`cache.used_bytes`, `blobs_bytes`, `index_bytes`, entries, blobs,
  evictions, bypasses, pins) sampled at the same instants — and `bunker fs status` / `bunker fs conflicts`,
  the owner-facing surface;
* the counters, which separate "bounded" from "bounded because nothing was ever cached".

A reader that reads a file twice makes the counters lie (the first version of `bypass_check.py` read each
file once to digest it and again to compare, doubling the insert attempts), so **every file is read
through the mount exactly once**; the digest it is compared against comes from the server's own copy.
`accounting.py` then reconciles the three and **names the residual to the byte** instead of waving at block
rounding.

Every probe kills its processes by explicit PID, bounds every `fusermount` with `timeout`, refuses to reuse
a run directory, and never uses `pkill -f`. `mount-arm.sh --mount-timeout-s` exists because the mount's own
`timeout` must exceed the arm it wraps (BFS-009's 90 s bound would have killed a 2-minute arm).

**The host** — 16 cores, loadavg 10-28 throughout (sibling workers running). No wall time here is a
performance result.

---

## 2. DELIVERABLE 1 — the bound, exceeded and held

### 2.1 Arm A — the default 256 MiB bound against a 640.1 MiB tree (2.5007×)

`bash docs/evidence/BFS-012-probes/mount-arm.sh --label A-default256 --tree /tmp/bfs012/armA-tree
--reader read-all.sh --concurrency 25` (no `--cache-max-size`: this IS the shipped default). Transcript
`BFS-012-armA-bound.txt`; the 178 in-flight samples are `BFS-012-armA-sample.csv`; the final status
document is `BFS-012-armA-status.json`.

```
tree        = /tmp/bfs012/armA-tree  (841 files, 671148678 B)      = 2.5007x the bound
reader      = read-all.sh  → read ok=841 failures=0 wall_ms=91121
              tree_bytes_on_disk=671148678  mount_bytes_seen=671148678
BOUND       max_bytes (reported)      :    268435456
            du -sb cache dir, MAX     :    267521528   ratio=0.9966
            used_bytes, MAX           :    267520015   ratio=0.9966
            samples where used_bytes > max_bytes : 0   OK
            samples where du(cache dir) > max_bytes : 0   OK     (178 samples)
AGREEMENT   reported blobs_bytes      :    267446880
            du -sb blobs              :    267446880   delta=0
            real blob file bytes      :    267446880   delta=0
            blob files on disk        :          455   reported blobs=455 entries=455
            reported index_bytes      :        73135
            index.json on disk        :        73135   delta=0
            reported used_bytes       :    267520015   == blobs+index: True
            du -sb cache dir          :    267521528   delta vs used_bytes = 1513
            status.json on disk       :         1513   <- the delta, EXACTLY (a file, not the cache)
            accounted: used_bytes + status.json + conflicts.jsonl == du(cache dir): True
EVICTION    evictions_total 386   bypass_events 0   oversize_bypasses 0   pinned_blobs 0
            841 reads - 386 evictions = 455 entries, EXACTLY
```

**Read as three separate claims, all of which hold.** (i) The cache directory's real on-disk size never
exceeded the bound — sampled 178 times *during* the run, not only after it. (ii) The figure the client
reports is the figure on disk: the content-addressed blobs agree exactly with the files, the index agrees
exactly with `index.json`, and the only difference between `used_bytes` and `du -sb <cache dir>` is the
mount's own `status.json`, to the byte. (iii) Eviction actually runs — 386 of them — and the arithmetic
closes: 455 retained + 386 evicted = 841 read.

**Owner-facing surface, same instant:** `bunker fs status` → `used_bytes=267520015 max_bytes=268435456
(blobs=267446880 index=73135) entries=455 blobs=455`, `cache events : ... evictions=386 bypasses=0
oversize_bypasses=0 pinned=0`.

### 2.2 Arm A, second instrument view

`read-all.sh` also reconciles the two byte totals: the tree on disk is 671,148,678 B and the mount's own
view of it is 671,148,678 B. This is *not* a claim that every read was correct (BFS-009 §3.3 and BFS-024/025/026
document stale and truncated reads) — it is the serial sweep's own total, and it is reported as measured.

### 2.3 Arm PIN and Arm OVER — "nothing can be evicted" ⇒ BYPASS, not failure

This is the part of the bound story **neither BFS-009 nor BFS-016 measured live** (BFS-009: `bypass=0
oversize=0` in every arm; the `Cache` unit tests reach these classes, but the mount's own path did not).
Transcript `BFS-012-bypass-arms.txt`, probe `bypass-arms.sh` + `bypass_check.py`.

**Arm PIN — the cache is full of bytes it may not reclaim.** `--cache-max-size 4194304` against 1 MiB
files, with **three read handles held open** across the run (each open read pins its blob, and a pinned blob
is never an eviction candidate), then the remaining three files are read:

```
status before      : entries=0 blobs=0 used=26   evictions=0 bypass=0 oversize=0 pinned=0
  held open        : f0000.dat / f0001.dat / f0002.dat   (1 MiB each, handles kept open)
  reads            : 6 (one per file)  ok=6  mismatched=[]
status while pinned: entries=3 blobs=3 used=3146239 bypass=3 oversize=0 pinned=3 evictions=0
status after       : entries=3 blobs=3 used=3146239 evictions=0 bypass=3 oversize=0 pinned=0
  PASS  every read returned the right bytes (6/6)
  PASS  3/3 reads that could not be cached left NO blob on disk
  PASS  used_bytes 3146239 <= max_bytes 4194304
  PASS  bypass_events moved (3) — a full cache of pinned blobs bypassed instead of failing
  PASS  pins were real and visible while held (during=3, after=0)
  PASS  no entry was stored for a bypassed read (entries=3, blobs on disk=3, held=3)
```

Three things worth naming. The **eviction counter is 0** in this arm: nothing was evictable, and the
client did not pretend otherwise. The bypasses are **counted in the counter that names the reason**
(`bypass_events`, not a generic error). And the three reads that could not be cached left **no blob on
disk** (checked by looking for each file's own sha256 in `blobs/`), so the read was served straight through
rather than half-cached.

**Arm OVER — nothing can be cached at all.** `--cache-max-entry-bytes 65536` against the same 1 MiB files:
all six reads succeed, nothing is cached (`entries=0 blobs=0`, zero blob files), `used_bytes` stays at 26
(the empty index), and the cache directory peaks at 1,485 B against a 256 MiB bound. But the counter that
exists for exactly this case **stays 0**:

```
  PASS  every read returned the right bytes (6/6 matched; mismatched=[])
  PASS  6/6 reads that could not be cached left NO blob on disk
  PASS  nothing over the entry cap was cached (entries=0 blobs=0 files=0)
  MEASURED (finding F-B): oversize_bypasses=0 while 6 reads exceeded --cache-max-entry-bytes and were
          not cached — the mount pre-filters at internal/fsmount/fs_linux.go:1053, so the counter can
          never move
```

That is finding **F-B**: the bound is respected by *not caching*, and the client's own report does not say
so — the same class of defect BFS-009 recorded as F2 for the un-cacheable read, at a different site
(`Cache.Insert` counts it at `internal/fsclient/cache.go:312`, and
`TestCacheOversizeEntryIsNeverCached` passes; the mount never calls `Insert` for an entry over the cap, so
the live path is invisible). It is printed as MEASURED and not asserted: a probe that turned red the day
someone fixed it would be a probe nobody could keep.

### 2.4 What the bound DOES and DOES NOT cover (finding F-C)

The row states the property as "the cache DIRECTORY's real on-disk size never exceeds the bound". At the
default bound that is true with 1,513 bytes to spare, because the directory holds four things and only two
are counted:

| item | counted in `used_bytes`? | how it behaves |
|---|---|---|
| `blobs/` | yes | content-addressed, bounded by the cap |
| `index.json` | yes | the path index |
| `status.json` | **no** | the mount's own document, rewritten on a 1 s cadence (1.3-2.2 KB) |
| `conflicts.jsonl` | **no** | the refusal LOG, appended once per refused write — grows with refusals, not with the tree |

`max_bytes` bounds `blobs + index`. At a bound of 1024 B, measured in one arm
(`small_bound_edge.py --refusals 40`, transcript `BFS-012-smallbound-edge.txt`):

```
phase 0. baseline (nothing read)  du(cache dir)=1262   used_bytes=26    max_bytes=1024
phase 1. after reading the tree   du(cache dir)=2003   used_bytes=636   max_bytes=1024
phase 2. after 40 refused writes  du(cache dir)=30689  used_bytes=636   max_bytes=1024
         conflicts.jsonl=27836 bytes / 40 lines        status.refusals_total=40
  PASS  the client's used_bytes never exceeded max_bytes in any phase (max used=636, bound=1024)
  MEASURED (finding F-C): the cache DIRECTORY exceeded the bound in 3 phase(s) — worst 30689 B = 29.97x
```

**The client's own contract held in every phase; the DIRECTORY-level claim did not.** At a bound of 1 KiB
the unaccounted floor (`status.json`, ~1.4 KB) is already larger than the bound, and the refusal log adds
27.8 KB on top of a 1 KiB cache. This is not a violation of the owner's constraint — the refusal log does
not grow with the *tree* — but it is a precise statement of what "bounded local storage" means here, and
it is only visible if the directory is measured rather than the reported counter.

### 2.5 What arm A proves and what it does not

**Proves:** the cap is enforced over the bytes actually on the client's disk; the cache grows only on
access; eviction runs and its arithmetic closes; the reported figure is the figure `du` sees, with the
residual named to the byte; the bound is never exceeded while a tree 2.5× its size is read through it; and
when nothing can be evicted the client bypasses instead of failing or growing.

**Does not prove** (named, not implied):
* **No concurrent-write arm.** Every arm is a serial reader, so pins from in-flight *writes*, the write
  buffer (`writebuf-*` temp files, which live under the same cache directory and which `used_bytes` does not
  count), and `MarkInFlight`/`MarkSettled` were never in play. Arm PIN pins from READ handles.
* **No multi-handle read concurrency claim.** BFS-016 measured the concurrency lever; this row does not
  reopen it.
* **Loopback only, one host.** The release's two-DC rule is not satisfied here.
* **The 1 h backstop TTL is untested** (`--cache-max-age`): no arm waited an hour.
* **`--invalidation poll`/`push` were not separate arms** (one `auto` arm produced the declared `mode=poll`
  state; BFS-009 measured that channel and it is unchanged).
* **The write-path deviation (BFS-005 §5.4)** — the one place `du` could legitimately exceed `used_bytes` by
  more than `status.json` — was not measured, because no bound arm wrote through the mount.

---

## 3. DELIVERABLE 2 — conflict refusal, tested and not fooled by the mtime (and not enforced either)

### 3.1 The rule, at unit level: the content hash, never the metadata

BFS-009's `TestConflictRefusalIgnoresMtimePreservedEdit` (an edit that preserves size **and** mtime must be
refused) passes on this tree, and this row adds the other direction, because "never mtime" has a
false-positive side that no test covered: **a change that moves ONLY the mtime, with the bytes identical,
must NOT be a conflict** — there is nothing to refuse, and a spurious refusal here is a build that cannot
write a file until someone re-reads it.

`internal/fsclient/conflict_test.go::TestTouchOnlyChangeIsNotAConflict` asserts the fixture first (the size
did not move, the mtime DID move, the bytes are identical) and then that the write lands, is not a noop, is
not counted as a refusal, is not written to the conflict log, and that the new bytes are on the server.

**Non-vacuity, mutation-RED, sha256-verified** (`BFS-012-mutation-mtime-identity.txt`, probe
`mutation_mtime_identity.py`): the served content identity in `internal/server/webdav/tree.go::hashFile` is
made to depend on the **mtime** — which is what "the conflict rule is metadata" looks like in code:

```
tree.go sha256 before : 852bac8d8e0fd28f4243a8aa3f3a9a99f4808ae3104b8c458361fbae9ca695ce
1. GREEN baseline: TestTouchOnlyChangeIsNotAConflict PASS · …IgnoresMtimePreservedEdit PASS · …IdenticalContentIsARecordedNoop PASS
2. RED (mtime in the identity): all three FAIL
3. RESTORE: sha256 after restore = 852bac8d… RESTORE VERIFIED: True → all three PASS
sensitive to a metadata-derived identity: [all three]
NON-VACUITY: PROVEN
```

The mutation is coarse (any mtime-derived identity also breaks the identical-content noop arm — reported
rather than trimmed, because that is what the mutation is); the arm it must break does break.

### 3.2 Live, through the mount: what is reachable at all (finding F-A)

The stock-client half proves a **412 at the surface** with curl. That is not the client's path. Before
driving writes through a live mount, the shapes were measured (`write_shape_probe.py`, transcript
`BFS-012-writeshape.txt`):

| the caller's shape | result | bytes reached the server? |
|---|---|---|
| `open(new,'wb')` → write → close (a path that does not exist) | **ok** (Create) | yes |
| `open(existing,'r+b')` → write (same length) → close | **error 95 EOPNOTSUPP** | no |
| `open(existing,'wb')` → write → close (the shell's `>` shape) | **error 116 ESTALE** | no |
| `open(existing,'ab')` → write → close (append) | **error 95 EOPNOTSUPP** | no |
| `truncate(existing, 32)` | **error 116 ESTALE** | no |
| `truncate(existing, 48)` (grow back) | **error 116 ESTALE** | no |

**Finding F-A: writing an EXISTING file through this mount is unreachable by every ordinary shape.**
`node.Open` (`internal/fsmount/fs_linux.go:826`) always returns a `readHandle` — it records
`writeIntent` from the open flags but creates no writer — and a `writeHandle` is created only by
`node.Create` (a path that does not exist). So `r+b`, `>` and `>>` on an existing file die at the first
`write(2)` with EOPNOTSUPP, and nothing in this row's fixture could be overwritten in place. That is why
the conflict arms below use **`truncate`** — `Setattr(size)` is the one reachable write path for an existing
file, and it is a real one: a read-modify-write published as ONE conditional `PUT` through
`WritePath.ResolveBase`/`PublishBytes` (the same refusal classifier, the same refusal log).

*Caveat, stated because it bounds the finding:* BFS-016's mount battery has a write cell
(`ops,write file (printf>),0.111,0,ok`) — that cell targets a path it creates, which is exactly the shape
that works. This row's table is the first to separate create-from-overwrite in the live path.

### 3.3 Live, through the mount: the refusal fires — and the mutation lands anyway

`conflict-arms.py` drives three arms through a live mount, with a logging reverse proxy
(`countproxy.py`) between the mount and the surface, so the exact request sequence is evidence rather than
inference. Transcript `BFS-012-conflict-arms.txt`; raw traces `BFS-012-trace-edit-requests.jsonl`,
`BFS-012-trace-noedit-requests.jsonl`.

```
=== ARM A — CONTROL: truncate a file NEVER read through the mount (fetch-then-check base) ===
  truncate 63 -> 43: LANDED — returned cleanly        server size now: 43
  PASS  the control write LANDED (a path never read resolves its base with fetch-then-check)

=== ARM B — an edit that preserves SIZE and MTIME must still be REFUSED ===
  read through the mount    : 64 B sha256=0a17f4b16061232c…   (this read fixes the base)
  server edited out-of-band : 64 B sha256=0e215e369e1be3d5…  (size same=True, mtime same=True, bytes differ=True)
  truncate to 48            : LANDED — returned cleanly
  FAIL  the write did NOT land over a same-size, mtime-preserved concurrent edit
  FAIL  the server's bytes were left untouched by the refused write
  conflicts.jsonl entries   : 1   {expected sha256:0a17f4b1…, current sha256:0e215e36…, code hash_mismatch}
  PASS  a conflict record names the post-edit hash (the refusal was recorded loudly)
  PASS  status.json counted the refusal
```

The request trace for arm B, in order (the whole mechanism):

```
  11 GET   200        0  -                             /dav/src/target.txt   <- arm B's read fixes the base
  12 GET   200        0  -                             /dav/src/target.txt   <- the truncate's own read
  13 PUT   412       48  "0a17f4b1606…                 /dav/src/target.txt   <- REFUSED: the base we served
  14 GET   200        0  -                             /dav/src/target.txt
  15 PUT   204       48  "0e215e369e1…                 /dav/src/target.txt   <- LANDED: the base the refusal corrected
  16 PROPFIND 207   276  -                             /dav/src/target.txt
```

**What this is, exactly.** The refusal is real, is interoperable, and is recorded (the log line names the
expected and the current hash). But it is **not terminal**: the client's own rule 3 updates the base from
the refusal
(`internal/fsclient/write.go:253-254`), and something *below the client's API* re-issues the operation — so
the second attempt carries the corrected base, matches, and **writes**. The caller saw no error: the
truncation landed and `os.truncate()` returned success.

**Attribution of the second request, measured.** It is not the caller (below), and not the client's code:
`WritePath.PublishBytes` issues exactly one `PUT` and `truncate` calls it once
(`internal/fsmount/fs_linux.go:961`), go-fuse's bridge dispatches exactly one `Setattr` per FUSE request
(`fs/bridge.go:600-605`, an `if/else if`), and the syscall count is one:

```
trace arm A (accurate base)  : ONE syscall  ->  GET, PUT 204                     (1 request)
trace arm B (stale base)     : ONE syscall  ->  GET, PUT 412, GET, PUT 204      (2 requests)
strace arm B                 : 213821 truncate("/…/mnt/src/target.txt", 32) = 0   (1 truncate syscall, 0 failures)
```

So the re-issue comes from the FUSE client stack beneath the mount. And it is **specific to the ESTALE
verdict**, measured by building the same binary with the conflict refusal carrying EIO (5) instead of
ESTALE (116) and running the identical arm (`BFS-012-estale-retry-experiment.txt`, probe
`estale_retry_experiment.py`; `client.go` restored byte-for-byte, sha256 verified):

```
arm with the conflict errno = EIO (5) instead of ESTALE (116):
   9 PUT       412  …  /dav/src/target.txt          <- the refusal, as before
   /dav/src/target.txt: REFUSED at request #9, never landed afterwards — the refusal held
client.go sha256 restored: …  RESTORE VERIFIED: True
```

**Finding F-D: the client's conflict errno defeats the client's conflict policy.** ESTALE is the return the
code's own comment names "write precondition refused: re-read and retry", and the FUSE client re-issues the
operation on it; because the client has already adopted the refused base, the re-issued write *succeeds*.
The refusal is loud in the *log* and silent in the *effect*. With EIO the same call fails, the error reaches
the caller, and the server's bytes are untouched. This is not a data-loss bug in the path measured (a
truncate re-reads the current bytes before writing, so the bytes written are derived from the server's own
current content, not from the client's stale copy — and the truncate body is written only when the corrected
base matches) but it *is* a failure of the row's stated rule — "the default is to REFUSE loudly" — at the
mount boundary, for any caller that retries, and the FUSE client is such a caller.

### 3.4 Can the client's conflict detection be fooled by a preserved mtime?

**No — the detection is the content hash, and the size+mtime-preserved edit is refused by the classifier**
(§3.1, both the pre-existing test and its mutation-RED). What the mtime-preserved case is *not* protected
from is the retry interaction in §3.3: the refusal happens, is recorded, and the mutation still lands.
Both halves are true and must be read together.

### 3.5 The touch-only live arm — SKIPPED, with the reason named

Arm C of `conflict-arms.py` (read through the mount, move only the mtime, truncate) is a **named skip** on
this build, and the probe prints why rather than turning a defect of a different class into a finding about
the conflict rule:

```
=== ARM C: bytes IDENTICAL, mtime moved — the write must LAND (never mtime) ===
  read through the mount    : 64 B sha256=0a17f4b16061232c…
  the server's bytes        : 48 B sha256=aa355b7698f38747…
  SKIP (unreliable cell): the mount served STALE bytes for this path …
     Known defect, filed as BFS-024/025/026 and re-observed here; the touch-only direction is carried by
     the unit test TestTouchOnlyChangeIsNotAConflict (mutation-RED proven)
```

The mount served bytes the server does not have (the known staleness class the brief names: invalidation
depends on server ops this build answers `capability_unavailable`). An arm whose "before" read is already
wrong cannot attribute a later outcome to the mtime rule, so the live touch-only direction is **not proven**
here. The unit test carries that direction, with the mutation RED behind it.

---

## 4. DELIVERABLE 3 — cross-platform build, verbatim

`bash probes/cross-GOOS-build.sh --survey` (BFS-010's committed guard, unchanged, run from this worktree;
full transcript `BFS-012-cross-GOOS.txt`):

```
host: karaHermes-mde-7840hs 16 cores, go version go1.26.5 linux/amd64
worktree HEAD: 994b5b84051175fe67f848b6e395b1d70d47e04b

cross-GOOS platform-seam guard (BFS-010)
go               : go version go1.26.5 linux/amd64
required targets : linux/amd64 linux/arm64 windows/amd64 windows/arm64
known gaps (fixed by a named row, printed every run):
  windows/amd64 -> github.com/deployBunker/bunker/internal/cli   (internal/cli/umount.go:272 syscall.Stat_t; see docs/evidence/BFS-010-windows-mint-decision.md)
  windows/arm64 -> github.com/deployBunker/bunker/internal/cli   (internal/cli/umount.go:272 syscall.Stat_t; see docs/evidence/BFS-010-windows-mint-decision.md)

TARGET           BUILD      VET internal/fsmount     RESULT
---------------- ---------- ------------------------ ------
linux/amd64      PASS       PASS                     ok
linux/arm64      PASS       PASS                     ok
windows/amd64    GAP(cli)   PASS                     ok
windows/arm64    GAP(cli)   PASS                     ok

--- build-windows-amd64 (verbatim) ---
# github.com/deployBunker/bunker/internal/cli
internal/cli/umount.go:272:25: undefined: syscall.Stat_t
internal/cli/umount.go:273:20: undefined: syscall.Stat
internal/cli/umount.go:277:20: undefined: syscall.Stat

--- build-windows-arm64 (verbatim) ---
# github.com/deployBunker/bunker/internal/cli
internal/cli/umount.go:272:25: undefined: syscall.Stat_t
internal/cli/umount.go:273:20: undefined: syscall.Stat
internal/cli/umount.go:277:20: undefined: syscall.Stat

== survey (informational; does not affect the exit status) ==
  darwin/amd64     rc=1 github.com/deployBunker/bunker/internal/fsclient
  darwin/arm64     rc=1 github.com/deployBunker/bunker/internal/fsclient
  freebsd/amd64    rc=1 github.com/deployBunker/bunker/internal/fsclient
  openbsd/amd64    rc=1 github.com/deployBunker/bunker/internal/fsclient github.com/deployBunker/bunker/internal/server
  netbsd/amd64     rc=1 github.com/deployBunker/bunker/internal/fsclient github.com/deployBunker/bunker/internal/server
  solaris/amd64    rc=1 github.com/deployBunker/bunker/internal/fsclient github.com/deployBunker/bunker/internal/registry
  illumos/amd64    rc=1 github.com/deployBunker/bunker/internal/fsclient github.com/deployBunker/bunker/internal/server
  plan9/amd64      rc=1 github.com/deployBunker/bunker/internal/fsclient github.com/deployBunker/bunker/internal/mountdriver github.com/deployBunker/bunker/internal/registry
  js/wasm          rc=1 github.com/deployBunker/bunker/internal/fsclient github.com/deployBunker/bunker/internal/registry github.com/deployBunker/bunker/internal/tunnel
  (a platform outside REQUIRED that fails is a finding to file, not a red guard)

RESULT: PASS — every required target builds; the seam type-checks on all of them
GUARD EXIT=0
```

**Which targets build and which do not, precisely:**
* **linux/amd64 — builds** (`./...` clean, `go vet ./internal/fsmount` clean). This is the platform the
  whole row was measured on.
* **linux/arm64 — builds**, and the seam type-checks (vet clean).
* **windows/amd64 — does NOT build `./...`**: `internal/cli` fails at `internal/cli/umount.go:272`
  (`undefined: syscall.Stat_t`; `syscall.Stat` at 273/277). The `internal/fsmount` seam itself VET-passes
  for this target, so the failure is the CLI package, not the platform seam. Filed as **BFS-028**.
* **windows/arm64 — the same failure, same site.**
* Every surveyed platform (`darwin`, `freebsd`, `openbsd`, `netbsd`, `solaris`, `illumos`, `plan9`,
  `js/wasm`) fails to build: `internal/fsclient` is the package they all fail in, which is the "no FUSE
  binding off Linux" boundary, not a Windows-specific surprise.

**The guard exits 0 while printing that Windows CLI failure, and that is by design, not a false green:**
`KNOWN_GAPS` is a ratchet — the windows/amd64 and windows/arm64 rows are listed, the gap is printed
verbatim on every run with the site that owns the fix, and an unlisted failure in a required target is a
hard exit 1 (BFS-010 measured the guard's own first run doing exactly that, from a SIGPIPE race it then
fixed). This row claims **no working Windows build**, for the CLI or the mount.

---

## 5. DEFECTS FOUND, NOT FIXED (each with its site and its measurement)

This row's brief is explicit that these get reported, not fixed. None is fixed here.

### F-A — an in-place write of an existing file is unreachable through the mount
`internal/fsmount/fs_linux.go:826` (`node.Open`) returns a `readHandle` whatever the open flags say (it
records `writeIntent` and nothing else), and `newWriteHandle` is called only from `node.Create`
(`:839`, a NEW path). Measured: `r+b` → 95 EOPNOTSUPP, `>` on an existing file → 116 ESTALE, `ab` → 95
EOPNOTSUPP, all with the server's bytes unmoved (`BFS-012-writeshape.txt`). Consequence for the release:
the client can create files and truncate them, but a build step that rewrites an existing file in place
(a shell's `>`, an editor save, `sed -i`'s rewrite, `tar -x` over an existing tree) cannot go through the
mount at all. It also means the conflict refusal cannot be exercised by the overwrite path a real user
would take — which is why §3.3 had to use `truncate`.

### F-B — the oversize-bypass counter can never move from the live read path
`internal/fsmount/fs_linux.go:1053` pre-filters `len(data) <= MaxEntryBytes()` before calling
`Cache.Insert`, so `OversizeBypasses` (`internal/fsclient/cache.go:312`) is unreachable from the mount.
Measured: 6 reads over `--cache-max-entry-bytes` left nothing cached and `oversize_bypasses` stayed 0, while
`used_bytes` reported 26 B (the empty index) as if nothing had been declined. Same class as BFS-009's F2
(the un-cacheable read), different site; the counter exists precisely so "the bound is being respected by
not caching anything" is visible.

### F-C — the cache DIRECTORY is not what the bound bounds
`status.json` (~1.3-2.2 KB, rewritten on a cadence) and `conflicts.jsonl` (append-only, one line per refused
write, no rotation) live in the cache directory and are outside `used_bytes`. At the default bound the
difference is 1,513 bytes; at a 1 KiB bound the directory reached **30,689 B = 29.97× the bound** with the
client's own figures still inside it. Not a growth-with-the-tree problem — refusals are not the tree — but
the honest statement of the property is "`used_bytes` is bounded", not "the directory is bounded".

### F-D — a refused write is re-issued with the corrected base and lands
Measured in §3.3: one `truncate(2)` syscall (strace: 1), two `PUT`s (412 recorded, then 204 landed), one
conflict record, caller sees success. The re-issue is below the client's API and is specific to the ESTALE
verdict (EIO arm: one PUT, error reaches the caller, nothing written). The client's own rule 3
(`internal/fsclient/write.go:253-254`) is what makes the retry successful, so the two behaviours are
correct individually and defeat the policy together. Worth a decision rather than a patch: the options are
(a) return a non-retryable errno for a refusal and let a caller that wants the `re-read and retry`
semantics ask for it, (b) make the write path detect a re-issue of the same operation and refuse it too,
or (c) narrow `--on-conflict` to an explicit opt-in whose semantics include retry. This row names the
options and does not choose.

**Re-observed, already filed (not new):** the mount served bytes the server does not have for a path an
out-of-band edit changed (§3.5), and the snapshot-based node tree answered `404` for a file that existed on
the served tree after bind (`BFS-012-writeshape.txt`, requests #7-#9) — both are the BFS-018/024/025/026
family the brief names.

---

## 6. What I did NOT prove (and what is a claim rather than a measurement)

* **The live touch-only arm is not proven** — arm C skipped for a stale read (§3.5); the unit test carries
  that direction, with its mutation RED.
* **The conflict refusal was not exercised through a plain overwrite** — impossible on this build (F-A), so
  the only live path tested is `truncate`. The stock-client half covers the surface with curl; the client's
  own overwrite path has no live test because there is no live overwrite.
* **No write path through the mount in any bound arm** (F-A again): the write-buffer temp files, pins from
  live write handles and BFS-005 §5.4's deliberate deviation were never measured against `du`.
* **Loopback, one host, HTTP/1.1 only.** No TLS, no h2c, no second DC.
* **Windows is not built.** The guard's PASS is a ratchet over a printed `KNOWN_GAPS` entry; the CLI does
  not compile for windows/amd64 or windows/arm64.
* **The retry's layer is bracketed, not named to the function.** It is proven to be below the client's API
  (one syscall; one `PUT` per publication in the client; one `Setattr` dispatch per request in go-fuse),
  and proven ESTALE-specific (the EIO arm), but whether the FUSE kernel client or the VFS issues the second
  request is not attributed further.
* **`loadavg` was 10-28 throughout** (sibling workers). No wall time here is a performance result, and the
  91 s for 841 files is quoted only as the arm's own duration.
* **Claims, not measurements:** that an mtime-derived identity would break other consumers in ways this
  mutation did not show; that the F-D retry would behave the same for a *write handle* path (it was
  measured for `truncate`, the only reachable one); and that a 1 KiB bound is a configuration anyone ships
  (it is used here to make the unaccounted floor visible, and it is labelled as such).

---

## 7. Reproducing this

```
# fixtures (deterministic; each prints a total re-derived from a fresh walk)
P=docs/evidence/BFS-012-probes
python3 $P/mkfixture.py /tmp/bfs012/armA-tree --bulk 640 --bulk-bytes 1048576 --small 200
python3 $P/mkfixture.py /tmp/bfs012/small-tree --bulk 6 --bulk-bytes 1048576 --small 8
python3 $P/mkconflict_tree.py /tmp/bfs012/conflict-tree
go build -o /tmp/bfs012/bin/bunker ./cmd/bunker && go build -o /tmp/bfs012/bin/davserve ./probes/davserve

# deliverable 1 — the bound (the big one takes ~95 s and ~1 GB of disk, all under /tmp/bfs012)
B=/tmp/bfs012/bin; bash $P/mount-arm.sh --label A-default256 --tree /tmp/bfs012/armA-tree \
     --bin $B/bunker --davserve $B/davserve --reader $P/read-all.sh --concurrency 25 --mount-timeout-s 1800
python3 $P/accounting.py /tmp/bfs012/run-A-default256
bash $P/bypass-arms.sh --bin $B/bunker --davserve $B/davserve --tree /tmp/bfs012/small-tree
bash $P/mount-arm.sh --label P-smallbound --tree /tmp/bfs012/conflict-tree --bin $B/bunker --davserve $B/davserve \
     --flag "--cache-max-size 1024" --reader $P/small_bound_edge.py --reader-args "--refusals 40" --mount-timeout-s 300

# deliverable 2 — the client's rule and the live path
go test ./internal/fsclient/ -count=1 -v -run 'TestTouchOnlyChangeIsNotAConflict|TestCacheAllPinnedBypassesWithoutLeaking'
python3 $P/mutation_mtime_identity.py                       # RED/GREEN + sha256-verified restore
bash $P/mount-arm.sh --label N-conflict --tree /tmp/bfs012/conflict-tree --bin $B/bunker --davserve $B/davserve \
     --trace --trace-port 18476 --reader $P/conflict-arms.py --mount-timeout-s 300
bash $P/mount-arm.sh --label T-writeshape-trace --tree /tmp/bfs012/conflict-tree --bin $B/bunker --davserve $B/davserve \
     --trace --trace-port 18479 --reader $P/write_shape_probe.py --mount-timeout-s 300
python3 $P/estale_retry_experiment.py --work /tmp/bfs012     # ESTALE vs EIO, restored and verified

# deliverable 3
bash probes/cross-GOOS-build.sh --survey

# cleanup (the fixture is ~950 MiB); prints every path removed and df before/after
python3 $P/cleanup-fixture.py /tmp/bfs012 /tmp/bfs012/armA-tree /tmp/bfs012/small-tree
```

**Artifacts in this evidence set**

| file | what it is |
|---|---|
| `BFS-012-cache-bound-conflict-platform.md` | this report |
| `BFS-012-probes/` | every probe (19 files): `mkfixture.py`, `mkconflict_tree.py`, `sample.py`, `mount-arm.sh`, `accounting.py`, `read-all.sh`, `bypass-arms.sh`, `bypass_check.py`, `small_bound_edge.py`, `conflict-arms.py`, `write_shape_probe.py`, `truncate_diag.py`, `countproxy.py`, `trace_summary.py`, `mutation_mtime_identity.py`, `estale_retry_experiment.py`, `strace-truncate.sh`, `archive-run-artifacts.sh`, `cleanup-fixture.py` |
| `BFS-012-armA-bound.txt` | arm A, the full transcript (default bound, 640.1 MiB tree) |
| `BFS-012-armA-sample.csv` | arm A's **178 in-flight samples** (du × 4 measures + every reported counter) |
| `BFS-012-armA-status.json` | arm A's final status document, as the client wrote it |
| `BFS-012-bypass-arms.txt` | arm PIN and arm OVER, with their assertions and the F-B measurement |
| `BFS-012-writeshape.txt` | the write-shape table (F-A) |
| `BFS-012-conflict-arms.txt` | the live conflict arms, ending in the F-D finding |
| `BFS-012-trace-truncate.txt`, `BFS-012-trace-edit-requests.jsonl`, `BFS-012-trace-noedit-requests.jsonl` | the two request traces: accurate base (1 PUT) vs stale base (412 → 204) |
| `BFS-012-truncate-diag.txt` | the step-by-step diagnostic that first exposed the contradiction |
| `BFS-012-estale-retry-experiment.txt` | ESTALE vs EIO, with the sha256-verified restore |
| `BFS-012-mutation-mtime-identity.txt` | the non-vacuity proof for the touch-only test |
| `BFS-012-smallbound-edge.txt` | the bound-vs-directory measurement (F-C) |
| `BFS-012-cross-GOOS.txt` | the platform guard, verbatim |

**Disk.** The row asked for `df` before and after and for the fixture to be cleaned up, so both are
recorded rather than asserted. Before the fixture: `/dev/nvme0n1p2 1.8T 1.6T 160G 91%`. Peak during:
~950 MiB under `/tmp/bfs012` (a 640.1 MiB tree, a 267 MiB cache full to its bound, the binaries and 21 run
directories). After cleanup, re-measured: **`/dev/nvme0n1p2 1.8T 1.6T 157G 92%`, 949.4 MiB reclaimed**,
0 mounts left under `/tmp/bfs012` and no leftover processes — `cleanup.py` prints every path it removes and
the `df` before and after, so the custody is auditable. `df` on this host is shared with sibling workers,
so no single arm's footprint can be attributed from `df` alone; the per-arm numbers above are the
measurement, and this is the cleanup.
