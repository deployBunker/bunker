# BFS-033 — the conflict refusal HOLDS

**Status:** fixed and verified live. `412` is no longer something the client merely writes down:
the write it refused cannot land afterwards — not on the retry, not on the kernel's own re-issue,
not under a second writer — until the caller has actually re-read the path.

**The one-line defect.** One `truncate(2)` syscall through a live mount produced a `PUT 412` that was
recorded in `conflicts.jsonl`, immediately followed by a `PUT 204` that LANDED; the caller saw success.
The refusal was written down but not enforced, and the ledger claimed a protection the user did not get.

**The fix, in one paragraph.** §5.2 rule 1 already says the refusal is terminal for that write, and §5.2
rule 5 names the recovery (re-read, then retry). What was missing was the enforcement *between* them: a
refusal now STANDS on its path, and any publication of the same shape on that path is refused with the
same verdict — publishing nothing — until the CALLER has been served bytes for that path. A create
(`If-None-Match: *`) is exempt, because it is not the refused write. The hold is bounded (1024 paths) and
the bound is reported.

**Decision the row asked for, stated plainly.** The ESTALE-specific re-issue is a **kernel behaviour, not a
go-fuse one, and not a client choice** — measured below — and it is worked around rather than fixed by
changing the verdict. `ESTALE` is kept because it is the verdict that *names the recovery* ("your base for
this one file is stale: re-read it"), which is the entire contract of this client; with `EIO` the refusal
never held in the first place, because the kernel does not act on it (BFS-012's experiment). So: keep the
verdict, enforce the refusal.

---

## 1. The RED, live (unfixed tree)

`docs/evidence/BFS-033-red.txt`, produced by `docs/evidence/BFS-033-probes/bfs033-arms.py --mode cell
--expect red` through `mount-arm.sh --trace` (a per-request logging proxy between the mount and the repo's
own WebDAV surface).

The interleaving, so the arm is not a guess:

1. a read through the mount — this read **is** the client's base hash for the path;
2. an out-of-band edit that **preserves size and mtime** (the mount's invalidation is mtime-directed, so the
   conflict survives to the write);
3. one `os.truncate(path, 48)`.

Result, verbatim from the run:

```
truncate to 48          : LANDED — returned cleanly
target unchanged        : False
conflicts.jsonl entries : 1
PUTs on src/target.txt (proxy trace): 2
  #11  status=412  req_bytes=48  if_match=sha256:4435dba64cd01241ed5200836b30e81f5
  #17  status=204  req_bytes=48  if_match=sha256:2be53d0743bdd0be44d38e264a276eb1d
PUT 412 count=1  PUT 2xx count=1
```

`strace` on the same write: **one** syscall — `truncate("…/mnt/src/target.txt", 48) = 0`.

## 2. Why the second PUT exists (attribution, measured)

`docs/evidence/BFS-033-red.txt` §ATTRIBUTION.

**(a) The kernel re-issues the resize.** One syscall, **two** size-carrying `Setattr` dispatches, traced
with a temporary line in `node.Setattr`:

```
BFS033-TRACE setattr path=src/target.txt size=48 fh_nil=true valid=0x208
BFS033-TRACE setattr path=src/target.txt size=48 fh_nil=true valid=0x208
dispatches: 2
```

and the second dispatch's PUT carries `If-Match = 2be53d07…` — the **refusal's own `X-Bunker-Current-Hash`**,
i.e. the base §5.2 rule 3 adopted (which exists FOR the re-read-and-retry loop). That is why the refused
write landed: the machinery below the caller re-sent it with the base the refusal had just corrected.

**(b) The re-issue is bounded — it stops at two.** A probe build that refuses *every* resize (so the
re-issue cannot be escaped) produced **two** dispatches and then stopped: the caller got `ESTALE`, **zero**
PUTs reached the server, and nothing hung. This is what makes an enforcement that refuses the re-issue safe
rather than a hang.

**(c) It is ESTALE-specific.** With the conflict errno changed to `EIO` the refusal held with one PUT
(BFS-012's experiment, `docs/evidence/BFS-012-estale-retry-experiment.txt`). So the re-issue is the kernel
acting on `ESTALE`, not the client retrying — the client's own `Publish` makes exactly one PUT.

## 3. The fix

`internal/fsclient/write.go` (the write path owns the refusal, so it owns the enforcement):

| | |
|---|---|
| `armHold(rec)` | a **real server refusal** arms the path. The verdict, the hashes and the code are the server's own — nothing is invented. |
| `checkHold(path, base)` | a publish on a held path is refused with the **same class** (`ESTALE`, cause `conflict`, the refusing verdict and hashes) and **publishes nothing** — no request leaves the client at all. A create (`If-None-Match: *`) passes: it is not the refused write, and holding it would strand a caller that removed the path and wrote it anew. |
| `NoteRead(path, hash)` | the **caller-facing read** is the one thing that clears a hold — §5.2 rule 5's re-read. Called from the mount's read path only. |
| `NoteDeleted(path)` | a removed path takes its hold with it. |
| bound | the hold map is capped at 1024 paths and evictions are **counted** (`refusal_holds.evicted_total`) — a bound the owner cannot see is not a bound (PRD-bunker-invalidation §2.7). |

`internal/fsmount/fs_linux.go` calls `NoteRead` at the two points the mount records served bytes (the cache
serve and the network serve) and `NoteDeleted` on `Unlink`; `Status()` reports the block.
`internal/cli/fs.go` prints it:

```
refusal holds: held_total=1 outstanding=1 evicted_total=0
  last       : src/target.txt: held=1 code=hash_mismatch (the refusal stands until the path is read)
```

**Why the hook is not in the client's own reads.** A resizing `truncate` reads the path itself before
publishing; clearing the hold there would be the defect back again. The mount's handler-level arm
`TestTheMountInternalReadIsNotTheCallersRead` pins exactly that difference.

## 4. The GREEN, live (fixed tree)

`docs/evidence/BFS-033-green.txt`, same interleaving, same harness:

```
truncate to 48          : REFUSED — OSError: [Errno 116] Stale file handle (errno=116 ESTALE)
target unchanged        : True
PUTs on src/target.txt (proxy trace): 1
  #11  status=412  req_bytes=48  if_match=sha256:4435dba64cd01241ed5200836b30e81f5
PUT 412 count=1  PUT 2xx count=0
```

One refused PUT, **no landing write**, the target byte-identical to the concurrent edit, the caller told
`ESTALE`, and the refusal still recorded for `bunker fs conflicts`. The caller's verdict and the observed
effect now agree in both arms: no success for a write that did not happen, and no conflict for a write that did.

## 5. The negative control (the cell can fail)

`docs/evidence/BFS-033-negative-control.txt`, script `docs/evidence/BFS-033-probes/negative-control.sh`,
patch `docs/evidence/BFS-033-negative-control.patch` — one added line (`return nil` in front of `checkHold`).

* `write.go` sha256 `d2c7c062…` → neutered `979a916f…` → restored `d2c7c062…`, byte-identical to a copy
  taken before the patch, `git status` clean.
* **With the enforcement neutered, the same live cell reproduces the defect**: `PUT #11 412` then
  `PUT #13 204`, the caller told it succeeded, `conflicts.jsonl` carrying an entry the landing contradicts —
  and `refusal_holds` shows `held_total: 0` (the hold never fired).
* The unit cells fail too, with their own messages: `TestARefusalHoldsAgainstTheReissuedWrite`
  ("THE DEFECT: the re-issued write LANDED behind the refusal"), `TestWriteRefusalOnStaleBase`
  ("a publish landed behind an unrecovered refusal"), `TestAReissuedResizeIsRefusedUntilTheCallerReReads`
  ("the re-issued resize must be refused, got errno=errno 0").

## 6. The retry path, and two writers

**Retry (`--mode retry`).** The refused writer re-reads the path and retries: the retry **lands**, and it
publishes the *server's current content* truncated (a resize reads what it is about to replace), so the
recovery is not a dead end and is not a lost update. Trace: exactly one `412` then one sanctioned `204`.

**Two writers (`--mode twowriters`).** Two writers read the path (both bases then made stale by an
out-of-band edit). Writer A is refused. Writer B, a second writer with the same stale base, **cannot publish
behind A's standing refusal** — its refusal is held locally, so *no PUT of its shape ever reaches the
server*. Writer C, on **another** path, still lands (the hold is per path, never a global stall). The
target's bytes are unchanged by both conflicting writers (`PUTs on src/target.txt: 1 total, 1 x 412,
0 x 2xx`; `refusal_holds.held_total: 3` — A's attempt, A's kernel re-issue, B's attempt), one refusal is
recorded, and writer B recovers by re-reading and retrying.

## 7. Cost of the successful path

`docs/evidence/BFS-033-cost.txt`. 200 files per arm, one whole-file conditional PUT per publication.

| | before | after |
|---|---|---|
| RESIZE (modified path) | 264.98 ms/op, 200/200 landed | 309.23 ms/op, 200/200 landed |
| CREATE (new path) | 50.30 ms/op, 200/200 landed | 53.86 ms/op, 200/200 landed |

The live deltas are host noise (the box was at loadavg ~20; a resize is round-trip dominated): **the
structural cost is zero** — every request the mount made in each arm is identical, `PUT=400`, `GET=200`,
`HEAD=400`, before and after. The rule adds **no request** to a successful write.

Measured in isolation (`go test -bench`, 3 runs):

```
BenchmarkCheckHoldMiss            6.6–7.4 ns/op     (the successful publish: one map lookup)
BenchmarkCheckHoldMissContended  17.7–19.4 ns/op    (1024 holds standing, none on this path)
BenchmarkCheckHoldHit             3.1–3.8 us/op     (the refusal itself, which publishes nothing)
BenchmarkNoteRead                 31–55 ns/op       (the caller's read clearing a hold)
```

## 8. What this did NOT do

* **No other row was fixed or closed.** BFS-024/025/026/030/037 are untouched; no board rows and no
  `.gitreins/tasks.yaml` were written; nothing was pushed.
* **The refusal was not weakened into a warning**: a held write fails with the same `ESTALE`/`conflict`
  class as the original refusal.
* **No errno change**: `ESTALE` is kept, and the re-issue is worked around (the enforcement) rather than
  removed (the verdict).

## 9. Named residuals (stated, not hidden)

1. **Which bytes a re-read serves is not this row's.** In the same-size/mtime-preserved interleaving the
   mount's invalidation has nothing to see, so the caller's re-read can be answered from the cache entry its
   first read made. That is the stale-serve class already filed (BFS-024/026, QA-BUNKER-36) and it is
   *reported* by the retry arm rather than asserted away. The enforcement this row adds is that the refused
   write cannot land **without an intervening caller read** — not that the client's cache is truthful.
2. **A held path is refused for other writers too, until any caller reads it.** Conservative by design: a
   second writer with a *correct* base is refused once and must re-read (the same instruction `ESTALE`
   always gives). The per-path isolation is proven (writer C on another path lands).
3. **A refused re-issue costs one GET.** The hold is enforced in the write path (one decision point, as the
   project's doctrine requires), so the re-issued resize still reads the content it was about to replace
   before the publish is refused — one request, no PUT.
4. **The bound evicts.** Past 1024 standing refusals the oldest hold is dropped and counted
   (`evicted_total`); the re-issue this exists for arrives microseconds after its refusal, so the defect
   path cannot reach the bound. Counted rather than silent.
5. **`TestWriteRefusalOnStaleBase` was corrected, not deleted.** It asserted the old behaviour — a "recovered
   write" landing with no caller re-read — which is exactly the omission that let the unit suite pass while
   the live path refused nothing. It now asserts the enforcement and the recovery.

## 10. Reproduce

```bash
# build (add -race if you want the concurrency arm under it)
go build -o /tmp/bfs033/bin/bunker-fixed ./cmd/bunker
go build -o /tmp/bfs033/bin/davserve ./probes/davserve

# live cells (each needs a fresh tree dir; the harness refuses to reuse a run dir)
docs/evidence/BFS-033-probes/mount-arm.sh --label green --tree /tmp/t-green \
  --bin /tmp/bfs033/bin/bunker-fixed --davserve /tmp/bfs033/bin/davserve --work /tmp/bfs033 \
  --trace --reader python3 --reader-args "docs/evidence/BFS-033-probes/bfs033-arms.py --mode cell --expect green"
#   --mode retry | --mode twowriters | --mode cost (COST_FILES=200)

# the negative control (mutates, runs, restores with a sha256 check)
docs/evidence/BFS-033-probes/negative-control.sh --work /tmp/bfs033 --label negcontrol

# the unit cells
go test ./internal/fsclient/ -run 'TestARefusalHolds|TestOnlyTheCallersRead|TestACreateOnAHeldPath|TestTheHoldIsBounded|TestWriteRefusalOnStaleBase' -count=1 -v
go test ./internal/fsmount/ -run 'TestAReissuedResize|TestTheMountInternalRead' -count=1 -v

# rebuild every evidence file from the run dirs
docs/evidence/BFS-033-probes/assemble-evidence.sh --work /tmp/bfs033
```
