# BFS-020 — a created name is visible to the next operation on the same mount

**Row:** BFS-020 (P1) · **Repo:** bunker · **Worktree:** `wt/BFS-020` (not pushed) ·
**Author:** Hermes
**Status:** fixed and measured. The RED is the filed pair on a live mount: `printf x >
<mount>/f.lock` returns 0 and the IMMEDIATE `mv` fails ENOENT, while the same `mv` after a
2 s settle returns 0 (`docs/evidence/BFS-020-repro.txt`). The GREEN is the same arms on the
fixed tree, plus the battery's git block and the mutation battery.
**Design inputs read first:** BFS-021 (the append path landed the publication at FLUSH and
states the publication-point rules), BFS-038 (a refresh publishes by ONE atomic swap onto a
new immutable blob), BFS-033 (a refusal HOLDS), BFS-030 (a failed rewrite must not destroy
the original; the write-shape refusal), BFS-012 (the write-shape measurement), BFS-016
(`BFS-016-defect-repros.txt`, the filed reproduction), BFS-018 (the OPEN row for a directory
listing that comes back empty — relied on, not fixed), BFS-054 (the OPEN kernel-metadata-
cache DECISION — treated as "not enabled", never assumed).
**Code changed:** `internal/fsmount/fs_linux.go` · `internal/fsmount/append.go` ·
`internal/fsclient/write.go` · `internal/fsmount/bfs020_publication_test.go` (new) ·
`docs/evidence/BFS-020-probes/*` · `docs/evidence/BFS-020-arms.sh`.

---

## 0. Verdict in one screen

| # | what the row asks | RED, measured on the tree this row started from | GREEN, measured on the fixed tree | the mutation that reddens the cell |
|---|---|---|---|---|
| 1 | create+close, then rename IMMEDIATELY | `create rc=0`, `mv` → **ENOENT rc=1**, and the served tree has `f.lock` 2 s later | `rename rc=0`, the served tree has `f.final` with the bytes, `f.lock` gone | `flush-does-not-publish` |
| 2 | the same for a ZERO-BYTE create (`: >`, `printf '' >`, `touch`) | create rc=0 and the name is **NEVER** published — the rename fails ENOENT forever, not for a window | the name is on the served tree as the close returns; rename rc=0 | `empty-create-not-published` |
| 3 | a rename of a name this mount is still HOLDING | ENOENT (the file has not been sent) | the barrier publishes it, then the move lands | `no-rename-barrier` |
| 4 | later bytes on a still-open handle land on the NEW name | — | `move.final` = `first-second`, the old name is not resurrected | `no-retarget` |
| 5 | a write after a publication point is still published | — | both halves land, once each | `write-does-not-reopen`, `no-base-advance` |
| 6 | a create of a name that was just REMOVED / MOVED | 412 on the second `.git/index.lock` in a live `git checkout -b` (`fatal: unable to write new index file`) | a fresh base is resolved; both ops land | `path-record-survives-unlink`, `rename-keeps-the-path-record` |
| 7 | a read after this mount replaced a file by rename | the mount served `ref: refs/heads/main` while the served tree held `measure-me` | the mount serves what the served tree holds | `rename-keeps-the-destination-cache` |
| 8 | a refused create does not leave the mount claiming the name | — | the mount answers from the server, not from its own invented entry | `refusal-keeps-the-created-name` |
| 9 | a refusal at close(2) reaches the caller | the failure was swallowed in RELEASE | ESTALE at the FLUSH, and the refusal is still recorded | `flush-swallows-the-refusal` |
| 10 | git's own write-lock-then-RENAME pattern | `checkout -b` / `add` / `commit` / `amend` all rc=128 on `.git/index.lock` | `checkout -b` twice, `index.lock` twice, `AUTO_MERGE.lock`, `packed-refs.lock`, `HEAD.lock`, a ref lock — every one published, renamed, 0 conflicts | (the battery) |
| 11 | the committed battery's git block | **9/14 rc=0, 5 rc=128 — four of them the lock cascade** (the filed figure) | **11/14 rc=0, 3 rc=128 — ZERO lock cascades**; the 3 that remain are attributed to another row (below), and **14/14 rc=0** once that row's precondition is cleared (PASS B) | (the battery) |
| 12 | `rmdir` of a NON-EMPTY directory (found while running the acceptance) | `rmdir` rc=0 and the whole served subtree is **gone**; `git checkout` deleted the served tree's `.git` | ENOTEMPTY, the subtree intact; empty directories still removable | `rmdir-passes-a-non-empty-collection` |

---

## 1. THE MECHANISM, measured rather than assumed

The instrument is the mount's own trace (a temporary `tracef` in a scratch copy of the tree —
the trace lines are NOT in this repo), lined up with the caller's wall clock, on ONE
create+close+rename (`docs/evidence/BFS-020-mechanism.txt`):

```
CALLER close-returned at 1790539063496892844
      -0.438 ms  create             trace.lock
      -0.021 ms  flush-write        trace.lock        <- FLUSH arrived; the mount had NO handler
      +0.010 ms  release-in         trace.lock        <- RELEASE starts the publication
      +0.015 ms  publish-in         trace.lock
      +0.447 ms  rename-in          trace.lock -> trace.final   <- the caller's mv, 0.019 ms after close(2)
      +0.645 ms  rename-out         MOVE ... errno=ENOENT cause=server_error status=404
      +4.145 ms  publish-put-done   trace.lock size=1 err=<nil> <- the name appears 4.1 ms AFTER close(2)
      +4.368 ms  release-out        trace.lock errno=0
   +2006.860 ms  rename-in          trace.lock -> trace.final   <- the same rename, after a settle
   +2007.066 ms  rename-out         rc=0
```

**The publication was deferred to RELEASE, and RELEASE is not a request the kernel waits
for.** The kernel sends FLUSH per close(2) and blocks on its reply; it does not block on
RELEASE (in go-fuse the RELEASE handler's return value is not even a status:
`func (b *rawBridge) Release(cancel <-chan struct{}, input *fuse.ReleaseIn)` has no return,
and `doRelease` sets no `req.status`), so the PUT that makes the name exist was still in
flight when the caller's next syscall reached the server. The one barrier the protocol
offers — FLUSH — went unused, because the write handle implemented `FileWriter`,
`FileFsyncer` and `FileReleaser` but not `FileFlusher`, so go-fuse's bridge answered FLUSH
with a bare `return 0`.

### The alternatives the row names, and what falsified them

* **"The publish is asynchronous relative to the FUSE close/release reply."**
  **SUPPORTED, and located exactly:** the trace above shows close(2) returning at 0.000 ms
  while the publication completed at +4.145 ms, and the operations between them are the
  mount's own RELEASE. It is not merely "asynchronous": the reply that close(2) waits for
  (FLUSH) carried no publication at all.
* **"The kernel's FLUSH/RELEASE ordering, and a handle with SEVERAL publication points."**
  **CONFIRMED, and it is the fix's lever.** FLUSH arrives (see the trace) and its reply is
  what close(2) returns; a handle really does get more than one publication point —
  `exec 3>>f` closes three times (BFS-021's measurement) and `dup`+close+write+close
  publishes twice (`docs/evidence/BFS-020-dupwrite.txt`, arm D1: after close #1 the served
  tree holds a 0-byte file, after close #2 it holds the 30 bytes written in between). So the
  fix had to make a later publication safe, not only the first one (§3, parts 4 and 5).
* **"An attribute/dentry cache on the mount answering the lookup before the publication
  lands (BFS-054)."** **FALSIFIED.** The MOVE was issued and the SERVER answered 404
  (`status=404 verdict=not_found`, in the trace above and in `BFS-020-repro.txt` A1) — the
  mount did not answer the rename from any cache. The mount asks with
  `out.SetEntryTimeout(0)` / `out.SetAttrTimeout(0)` (`fs_linux.go`, Lookup/Create/Getattr),
  and the mount's view of a name removed on the SERVER is truthful (P1: stat ENOENT, readdir
  does not list it, and an `O_EXCL` create of it succeeds). The DECISION row BFS-054 stays
  open and is not assumed anywhere here.
* **"The mount's own directory state not being updated on publish."** **FALSIFIED for the
  ENOENT** (the rename went to the server and 404'd), **but it is a real, separate
  contributor to what the mount SAYS about the name and about the CONTENT — and it is
  fixed:**
  * P2: immediately after a close the mount answered `size=0` for a name the served tree did
    not have at all (`BFS-020-poison.txt`, the unfixed half) — the entry `Create` put in the
    snapshot was still there. A publication that is REFUSED now drops it, so the next attrs
    reply is answered from the server (`refusal-keeps-the-created-name`).
  * V1/V2: after this mount replaced a file by write+rename, a READ through the same mount
    returned the OLD content (`ref: refs/heads/main` while the served tree held
    `measure-me`; `v1-content` while the served tree held `v2-content-longer`). The cache
    entry for the DESTINATION survived the rename that replaced it
    (`docs/evidence/BFS-020-content.txt`), which is also how a live `git checkout -b` read
    its own just-written `HEAD` back as the branch it had left.

---

## 2. THE RED, as a measurement (the UNFIXED tree)

`docs/evidence/BFS-020-repro.txt` (the filed pair, both halves), `BFS-020-timing.txt`,
`BFS-020-empty.txt`, `BFS-020-content.txt`, `BFS-020-poison.txt`, `BFS-020-rmdir.txt`,
`BFS-020-rewrite.txt`, `BFS-020-gitlock.txt`, `BFS-020-dupwrite.txt`, `BFS-020-cost.txt`.

```
== A1: create+close, then rename IMMEDIATELY ==
   create rc=0
mv: cannot move '.../mnt/f.lock' to '.../mnt/f.final': No such file or directory
   rename-immediate rc=1
   served tree after 2 s:  f.lock      <- published, but 4 ms TOO LATE
== A2: create+close, SETTLE 2 s, then rename ==
   create rc=0
   rename-after-2s rc=0
== A3: the same shape inside .git (what git does with <ref>.lock) ==
   create rc=0
mv: cannot move '.../mnt/.git/bfs.lock' to '.../mnt/.git/bfs.out': No such file or directory
   rename-immediate rc=1
   rename-after-2s rc=0
```

**The pair IS the defect**: the caller cannot tell a successful close from a visible name.
The timing arm puts a number on the window — five iterations, the rename issued immediately
after close(2):

```
   t0.lock: close=  0.051ms rename_immediately_after_close_rc=2 rename= 1.746ms visible_from_close=20.230ms
   t1.lock: close=  0.051ms rename_immediately_after_close_rc=2 rename= 1.031ms visible_from_close= 8.746ms
   t2.lock: close=  0.044ms rename_immediately_after_close_rc=2 rename= 1.554ms visible_from_close= 2.897ms
   t3.lock: close=  0.027ms rename_immediately_after_close_rc=2 rename= 0.702ms visible_from_close= 3.100ms
   t4.lock: close=  0.021ms rename_immediately_after_close_rc=2 rename= 0.471ms visible_from_close= 9.676ms
```

`rc=2` is ENOENT, 5 of 5; the window is 2.9–20.2 ms — and close(2) itself is 0.02–0.05 ms,
i.e. it returns long before anything is sent. **Nothing cheaper than publishing at the
barrier can close this**, because the caller's next syscall arrives 0.02 ms after close
returns (the trace: `rename-in` at +0.447 ms, and the *caller's* rename(2) at +0.019 ms).

Two more shapes of the same cause were measured on the unfixed tree:

* **The zero-byte create is NEVER published.** `: > f`, `printf '' > f` and `touch f` all
  arrive as a create with no WRITE at all, and the mount's publication returned early
  ("opened for write but nothing was written"): create rc=0, the served tree has no such
  name, and the rename fails ENOENT *forever* — the 2 s settle in A2 does not help
  (`BFS-020-empty.txt`). The mount answered `0 bytes` for that name, so this is also the
  "the mount claims a file the server does not have" case.
* **Content goes stale behind OUR OWN rename** (above, V1/V2).

---

## 3. THE GREEN (the FIXED tree), and what each part is for

```
== A1  create rc=0 / rename-immediate rc=0 / served tree: f.final (1 byte), f.lock gone
== A2  same
== A3  rename-immediate rc=0 inside .git; the served tree has bfs.out
== V1  mount content after: ref: refs/heads/measure-me  == the served tree
== V2  mount: v2-content-longer == the served tree
== timing, five iterations: rename_immediately_after_close_rc=0 5/5, close=4.3–31.0 ms
== rmdir of a non-empty directory: rc=1 "Directory not empty", the subtree intact
```

The fix has six parts, each answering one measured fact:

1. **`writeHandle.Flush` publishes.** FLUSH is the request whose reply close(2) waits for,
   so the name is on the server before the caller's next operation. It is the rule the
   append path has followed since BFS-021. The RELEASE that follows answers from the
   recorded result with no second request (asserted in cell C1).
2. **A create with NO chunk publishes a ZERO-BYTE body.** An empty file is still a name;
   nothing was being sent at all.
3. **`Rename` takes a publication barrier**: a name this mount is still holding is
   published before the MOVE, and the publication's own refusal is returned as itself rather
   than being flattened into a server 404 → ENOENT ("the file does not exist" and "I have not
   sent it" are different answers, and the caller can act on only one of them).
4. **A chunk after a publication point REOPENS the handle, and the base ADVANCES with each
   landed publication.** The kernel sends more than one publication point per open, so
   without these two the later bytes are buffered and never sent (silent loss with
   close reporting success), or the second publication carries a stale precondition and is
   refused. BFS-021 measured both one publication point into the append path; these are the
   same two rules for the write path.
5. **Everything remembered about a NAME that stopped referring to the file it described is
   dropped**: a rename drops the source's and the destination's base/served records
   (`WritePath.NoteDeleted` now covers the base and the served hash, not only BFS-033's
   refusal hold), and it drops BOTH names' cached BYTES (a rename REPLACES the destination).
6. **A create's metadata entry is dropped when its publication is REFUSED**, so the mount
   answers from the server rather than claiming a name the server refused to create.

### 3.1 The guarantee, argued — and the residual, named

**What is guaranteed:** after a create reaches a publication point **that the kernel waits
for** — FLUSH on close(2) — the name and its bytes are on the server. Every following
operation on the SAME mount therefore either SEES the new name, or receives the
publication's own refusal (ESTALE with the refusal's verdict and hashes), never a bare
ENOENT for a file whose close returned 0.

Why that holds, mechanically rather than by hope: FLUSH is a synchronous request in the FUSE
protocol (the kernel blocks on `fuse_flush`), the publication happens inside the handler, and
the mount answers the reply only after the PUT returned. The trace shows the FLUSH handler
running at −0.021 ms relative to close(2) returning, i.e. *inside* the close.

**The residual, stated rather than asserted away:**

* **The guarantee is per publication point, and the kernel decides how many there are.** If a
  caller never closes (writes and vanishes, or holds the descriptor open), the bytes stay in
  the handle's buffer, and a rename issued by ANOTHER mount of the same server cannot see
  them. Within the same mount that window is closed by the barrier (part 3); across mounts it
  is not, and closing it needs a server-side notion of an unpublished name that this surface
  does not have. Nothing is lost either way: the bytes publish when the handle closes.
* **A handle that is still open when its name moves is followed, not failed.** The rename
  retargets the live handles (part 3), so later writes land on the new name. A delete is
  different: `unlink` of a name whose create has not been published yet still answers ENOENT
  (loud, no loss — the file appears with its bytes when the handle closes). Giving `unlink`
  the same barrier would need a rule for what happens to a handle whose name is deleted
  mid-flight (it must not resurrect the name at close), which is its own row, not this one.
* **A pre-existing refusal limits `git commit` on a real repository** (§6, and it is not this
  row's defect: the same refusal, measured identically on both trees).
* **BFS-018 stays open** (a directory listing can come back empty). §8 closes its most
  destructive consequence but not the lie.

---

## 4. git's ACTUAL pattern, end to end

`docs/evidence/BFS-020-gitlock.txt`. The real sequence — create `<ref>.lock`, write it, close
it, RENAME it over `<ref>` — run through a live mount, with the mount's own trace:

```
G1  a stale lock planted on the served tree by hand, then:
    git checkout -b probe-a                 rc=0
G2  git checkout -b probe-b                 rc=0
    branch: probe-b                          <- the mount reads back what it just wrote
    served .git/index.lock: absent           <- the lock was renamed away, not left behind
    served .git/AUTO_MERGE.lock: absent
    server branches: main  probe-a * probe-b
G3  printf 'ref: refs/heads/probe-b\n' > .git/probe-c.lock; mv .git/probe-c.lock .git/probe-c
    rc=0; the served tree has probe-c with those bytes
    conflicts: (none)
```

The trace of that run shows every git lock going through the same path: `.git/index.lock`
(created, published, renamed over `.git/index` — twice, i.e. `checkout -b` then the index
update), `.git/refs/heads/bunker/<branch>.lock` → the ref,
`.git/HEAD.lock` → `.git/HEAD`, `.git/objects/<xx>/tmp_obj_*` → the loose object (this is
what `git add` writes: on the unfixed tree that rename failed and git reported
`unable to write file .git/objects/54/2a…: No such file or directory` /
`failed to insert into database`), `.git/AUTO_MERGE.lock` and `.git/packed-refs.lock`
(created EMPTY, published, unlinked, and created AGAIN later — the shape part 5 exists for).
**No conflicts were recorded in the run.**

---

## 5. The battery's git block: before and after

`docs/evidence/BFS-020-battery-before.txt`, `BFS-020-battery-after.txt` (the committed
battery, `probes/bunker-fs-battery.sh`, unchanged), and `BFS-020-gitblock.txt` (the block
run verbatim through a purpose-built mount, with PASS A and PASS B).

| op | before (unfixed) | after (fixed) |
|---|---|---|
| rev-parse HEAD / log -1 / log --oneline / ls-files / diff --stat / status --short / status --porcelain | ok (7) | ok (7) |
| `checkout -b scratch` | **rc=128** `Unable to create '.git/index.lock': File exists` | ok |
| `add one file` | **rc=128** (the same lock) | ok |
| `commit` | **rc=128** (the same lock) | rc=128 — a DIFFERENT cause: `could not open '.git/COMMIT_EDITMSG': Operation not supported` |
| `commit --amend` | **rc=128** (the same lock) | rc=128 — the same cause |
| `rebase HEAD~1` | **rc=128** `invalid upstream` (no commit landed) | rc=128 — the same, downstream |
| `symbolic-ref / branch` | ok (`main`) | ok |
| **total, the battery's own rows** | **8/13 rc=0, 5 rc=128** | **10/13 rc=0, 3 rc=128** |
| **total, counting that last row as the two commands it runs** | **9/14 rc=0, 5 rc=128 — 4 of them the lock cascade (the filed figure)** | **11/14 rc=0, 3 rc=128 — 0 lock cascades** |

> The battery's own block summary prints `-> 13/13 ok` on BOTH trees: it counts the rows it
> ran, not their return codes. The filed figure (9/14, 4 lock-caused `rc=128`) is the `rc`
> column of this block, and it reproduces exactly — the lock count is the number that has to
> reach zero, and it does: `grep -c 'index.lock.*File exists'` is **4** in
> `BFS-020-battery-before.txt` and **0** in `BFS-020-battery-after.txt`.

**The four lock failures are gone**, and the two that remain are not this row's defect — and
that is measured, not asserted:

* **The cause, attributed.** `git commit` writes `.git/COMMIT_EDITMSG` with
  `O_WRONLY|O_CREAT|O_TRUNC`, and that file exists in ANY repository that has a commit.
  Overwriting an EXISTING file in place is refused by this surface (BFS-012 clause 2 /
  BFS-030: a resize arriving while a write-intent handle is live is refused, because the
  write half cannot land). `docs/evidence/BFS-020-rewrite.txt` runs the shape on BOTH trees:
  `printf … > <mount>/.git/COMMIT_EDITMSG` is **rc=1 "Operation not supported" on the
  unfixed tree and on the fixed tree alike** — identical, so it is pre-existing.
* **PASS B: the block is green when that one precondition is cleared.**
  `BFS-020-gitblock.txt` runs the SAME 13 ops twice: once exactly as the battery runs them,
  and once with `.git/COMMIT_EDITMSG` removed before each commit-shaped op (a named,
  documented clearing of the OTHER row's precondition). Result: **PASS B 13/13 rc=0**,
  including `commit`, `commit --amend` and `rebase HEAD~1`, with the server and the mount
  agreeing on the branch afterwards and no `.lock` left on the served tree.
* **And `git commit` end to end, with that precondition not in the way:**
  `BFS-020-rewrite.txt` R3b — `commit rc=0`, server HEAD and mount HEAD both
  `219ec2c probe: commit again`, no `index.lock` left behind. On the unfixed tree the same
  arm fails earlier (`add` cannot write its loose object; then the `index.lock` cascade).

---

## 6. The LEFTOVER-MUST-NOT-POISON case

`docs/evidence/BFS-020-poison.txt`.

1. **The correct behaviour is git's own, and it is right.** A `<ref>.lock` that survives a
   crash is a STALE FILE on the served tree; git says so in its own words and stops
   (`fatal: Unable to create '…/.git/index.lock': File exists` + "Another git process seems
   to be running…"), and the operator removes it. This mount does not need to invent
   anything, and it must not: the file really exists.
2. **Not permanently dead.** Removing the stale lock THROUGH THE MOUNT works (`rm rc=0`, the
   served tree no longer has it) and the NEXT git command through the mount then succeeds
   (`rc=0`), checked with a FRESH branch name so the answer is about the path rather than
   about a branch that already existed. The unfixed tree's version of the same sequence is the
   locked cascade: `rm rc=0` and the server is clean, then the next command is
   **`rc=128 fatal: unable to write new index file`** — because the failed attempt left a
   FRESH `index.lock` behind, which is why the poisoning is permanent there and not here.
   Raw numbers for both trees, and the mount-vs-served agreement (`1 bytes` vs `1 bytes`), are
   in `BFS-020-poison.txt` P3.
3. **The mount does not lie about it.** P1 asks the mount about a name removed on the SERVER:
   `stat` → ENOENT, `readdir` does not list it, and an `O_EXCL` create of it succeeds — the
   mount follows the server. The one place the mount DID claim a name the server did not have
   was a create whose publication had not landed (P2, the mount answering `size=0`), and that
   is what part 6 of the fix removes.

---

## 7. `rmdir` of a NON-EMPTY directory — found by running the acceptance

`docs/evidence/BFS-020-rmdir.txt` and `BFS-020-battery-after.txt`'s probe
(`BFS-020-probes/bfs020-passA-steps.sh`).

While measuring the battery's git block, `git checkout main` through a fixed mount deleted
the served tree's entire `.git` directory (10 entries → 0). The trace of that run names the
cause: git deleted the branch's reflog and then walked UP the directory chain
(`.git/logs/refs/heads/<branch>` → … → `.git`), and **the mount answered `rmdir .git` with
rc=0**. git's `remove_empty_directories` stops that walk only when `rmdir` FAILS, and this
mount passed the request straight through to the surface's DELETE on a collection — which is
**RECURSIVE**:

```
== R: rmdir over a non-empty directory, and over an empty one ==   (unfixed tree)
   rmdir non-empty rc=0
   served tree after  :            <- every file under it, gone
   VERDICT: RMDIR OF A NON-EMPTY DIRECTORY DESTROYED THE SERVED SUBTREE
   VERDICT: THE CHAIN WAS DESTROYED                    (a 4-level chain, one file deep)
```

POSIX's ENOTEMPTY is load-bearing, not cosmetic. The fix asks the SERVER whether the
collection is empty (one `Depth: 1` PROPFIND) before deleting, and answers ENOTEMPTY
otherwise — never this mount's own readdir answer, because that listing is exactly what git
believed when it decided the directory was empty (BFS-018 is the open row for it). `unlink`
gets the matching local guard: a path this mount told the kernel is a collection is refused
with EISDIR, with no request. On the fixed tree the same arm reports
`rmdir non-empty rc=1 "Directory not empty"`, every file survives, the chain survives, and
emptied directories are still removable. **This is a data-loss path that the write-lock
defect was masking by failing git one step earlier; it is fixed here because the row's own
acceptance run walks straight into it.**

---

## 8. The NEGATIVE CONTROL per cell (mutation battery)

`docs/evidence/BFS-020-arms.sh` (transcript `BFS-020-arms.txt`). One mutation per cell,
applied by an exact-match replacement that REFUSES unless the anchor appears exactly once
(`BFS-020-probes/anchor-replace.pl`), run BY NAME, each with a declared RED set and a declared
GREEN (attribution) set, restored from `git checkout` and checked against the pre-arm sha256
of every touched file — a restore that is not byte-identical aborts the run.

| mutation | RED (must fail) | GREEN (must stay green) |
|---|---|---|
| `flush-does-not-publish` | C1 C2 C4 C5 | C3 C8 + the three landmarks |
| `empty-create-not-published` | C2 | C1 C5 + landmarks |
| `no-rename-barrier` | C3 | C1 C2 C4 + landmarks |
| `no-retarget` | C4 | C1 C3 C5 + landmarks |
| `write-does-not-reopen` | C5 C4 | C1 C2 C3 + landmarks |
| `no-base-advance` | C5 | C1 C2 + landmarks |
| `path-record-survives-unlink` | C6 C7 | C1 C5 + landmarks |
| `rename-keeps-the-path-record` | C7 | C1 C3 C6 + landmarks |
| `rename-keeps-the-destination-cache` | C8 | C1 C2 C5 + landmarks |
| `refusal-keeps-the-created-name` | C9 | C1 C5 C10 + landmarks |
| `flush-swallows-the-refusal` | C10 C9 | C1 C2 + landmarks |
| `rmdir-passes-a-non-empty-collection` | C11 | C1 C3 C5 + landmarks |
| `unlink-passes-a-collection` | C12 | C1 C2 C5 + landmarks |

The three LANDMARK cells are the rows this one may not change, and they must stay green under
EVERY mutation: BFS-030's write-shape refusal
(`TestResizeThroughAWriteHandleIsRefusedBeforeItPublishes`), BFS-021's append
(`TestAppendThroughAnAppendHandleLandsTheBytes`) and BFS-033's refusal hold
(`TestAStaleBaseRefusesTheAppendAndNothingLands`). The runner also refuses to accept a
VACUOUS run: a `-run` pattern that matches no test, or produces no per-test result line, is a
hard failure, not a pass.

**The committed transcript is `docs/evidence/BFS-020-arms.txt`: 13 mutations, 13 of 13 met
both declared outcomes, 0 arm failures**, each one printing `RED confirmed: the mutation
reddens the cell it is aimed at` and `attribution confirmed: the named cells stay GREEN`, and
each one closing with `restore: sha256 byte-identical for 3 file(s)`. Three declarations had
to be CORRECTED after the first run and the corrections are in the script's table: a mutation
that removes the write-reopen rule also reddens C4 (which writes twice across a publication),
clearing only `holds` on unlink also reddens C7 (the same function serves the rename), and
swallowing the refusal at FLUSH also reddens C9 (which asserts the refusal reached the
caller). Corrected declarations, not weakened mutations — the cells that observed the effect
are named in the RED set, which is what the RED set is for.

---

## 9. Cost, as a number

`docs/evidence/BFS-020-cost.txt` (both trees) and `BFS-020-put.txt` (the reference with no
mount in the path). 30 create+write+close cycles through a real mount:

| | unfixed tree | fixed tree |
|---|---|---|
| close(2) itself | median 0.030 ms, mean 0.062 ms, p95 0.240 ms (**nothing sent**) | **median 226.239 ms, mean 286.484 ms, p95 807.757 ms, max 1 340.911 ms** (one PUT inside the reply) |
| the name on the served tree after close | **no** (30 of 30) | yes (asserted per file, with the bytes) |
| `unlink` of the name through the mount | ENOENT (30 of 30) | ok (0 of 30) |

**The price is one request round trip's latency inside every close(2) of a created file.**
The fix does not add a request: the write path already sent exactly this one PUT, from the
RELEASE handler, after `close(2)` had returned to the caller — that is what made the window
invisible. The change is that the caller now WAITS for it.

**And that number is dominated by the server, not by the mount — measured, same window:**

| measurement (this box) | value |
|---|---|
| raw `PUT` to the served tree, no mount (`BFS-020-put.txt`, loadavg 25.16) | **658 ms** each (30 PUTs, 19 742 ms) |
| raw `GET` from the served tree, no mount, same window | 15 ms each |
| raw `PUT` in the cost window (loadavg ≈22) | 119 ms each |
| the mount's `close` in that same window | median 226 ms |

The box was carrying the fleet's own load while these ran (loadavg 21–25), so the absolute
figures are the box's, not the fix's: a raw PUT with no mount at all costs 119–658 ms on the
same window, which is the same order as the mount's close. A QUIC/HTTP PUT that has to land
an immutable blob and swap a pointer costs what it costs; the mount's own bookkeeping on top
of it is not the dominant term. Nothing is paid on the read path, on `unlink`, or on any
operation that does not close a written file; and no fixed sleep was introduced anywhere
(`close` returns as soon as the publication returns — the callers in `BFS-020-cost.txt` and
`BFS-020-timing.txt` show tens to hundreds of milliseconds against a local server, never the
row's 2 s settle, and the 2 s figure in the repro is only the settle the DEFECT needed).

---

## 10. What this must not do — and did not

* **Not a fixed sleep.** The publication is a barrier, not a delay: close(2) returns when
  the PUT returns. The row's own 2 s figure is what a sleep would cost.
* **No rename that returns 0 and loses the file.** Every rename in the cells and in the arms
  asserts BOTH sides afterwards (the source gone, the destination holding the bytes), and the
  MOVE is the server's own stage-and-rename (BFS-038).
* **BFS-038's atomic publish unchanged.** The publication is still the ONE conditional PUT,
  the same request the surface commits by staging a sibling and renaming it; nothing new is
  written into a published blob.
* **BFS-021's append semantics unchanged, and its cells run.** The landmark arms above are
  green under all 13 mutations, and `go test ./internal/fsmount/ ./internal/fsclient/` is
  green (the guard ran the whole repository's suite for both commits).
* **BFS-033's refusal still HOLDS; BFS-030's refusal still refuses.** Same cells.
* **No cache was disabled to make the repro go away.** The opposite: the destination's cached
  bytes are DROPPED on a rename (a rename replaces the destination), and a create drops any
  entry for its name — both named, with the cost.
* **No kernel metadata cache is assumed.** BFS-054 is open and the answer is "not enabled";
  the fix does not rest on it, and the falsification of that candidate is in §1.

---

## 11. Named residuals

* **Across mounts.** The barrier is on the same mount (which is what the row asks). A rename
  issued through a SECOND mount cannot see a name the first mount has not published yet.
  Trigger for revisiting: a workload that writes through one mount and renames through
  another.
* **`unlink` of a name whose create has not been published** answers ENOENT (loud, nothing
  lost — the bytes land at close). Same class as the rename barrier; it needs a handle-death
  rule, and it is not reachable from git's shapes.
* **BFS-018 stays open** (a directory listing can come back empty while the directory has
  entries). §7 removes its most destructive consequence (a recursive delete through `rmdir`)
  but not the lie itself.
* **An in-place rewrite of an EXISTING file is still refused** (BFS-012 clause 2 / BFS-030),
  which is why `git commit` on a repository whose `.git/COMMIT_EDITMSG` exists still fails at
  that file. Measured identical on both trees; serving it would need the write half of a
  truncate-then-write, and BFS-030's cells are the decision that it is not served.
* **`git rebase HEAD~1` in the committed block** is a downstream consequence of the failed
  commit (there is no second commit to rebase onto); PASS B shows it green (13/13).

---

## 12. Reproduction

```bash
# the live arms, before and after (one fresh tree and one fresh mount per probe)
docs/evidence/BFS-020-probes/run-arms.sh --bin /tmp/bin/bunker-unfixed --davserve /tmp/bin/davserve \
  --tag red   --outdir /tmp/bfs020/evidence-red   --work /tmp/bfs020/w-red
docs/evidence/BFS-020-probes/run-arms.sh --bin /tmp/bin/bunker-fixed   --davserve /tmp/bin/davserve \
  --tag green --outdir /tmp/bfs020/evidence-green --work /tmp/bfs020/w-green

# the committed battery, before and after (its git block is the acceptance measurement)
docs/evidence/BFS-020-probes/run-battery.sh --bin /tmp/bin/bunker-fixed --davserve /tmp/bin/davserve \
  --work /tmp/bfs020/w-after --tag after

# the cells and the mutation battery
go test ./internal/fsmount/ ./internal/fsclient/ -count=1
bash docs/evidence/BFS-020-arms.sh all          # 13 mutations, sha256-verified restores

# the mechanism trace (a scratch copy of the tree with the trace instrument)
#   see BFS-020-probes/arm.sh + the trace fragment quoted in §1

# assemble the committed evidence files from the runs
docs/evidence/BFS-020-probes/assemble-evidence.sh --work /tmp/bfs020
```

Everything cited above was produced by those scripts: the evidence files are assembled from
the runs rather than transcribed by hand, so a reader can regenerate them.

---

## 13. Files changed

| file | change |
|---|---|
| `internal/fsmount/fs_linux.go` | `writeHandle.Flush` (the publication point close(2) waits for, + the `FileFlusher` assertion) · the zero-byte publication for a create with no chunk · a chunk after a publication point reopens the handle · the base advances with each landed publication · the create's entry dropped on a refused publication · `Mount.pendingFor` / `publishPending` / `retargetHandles` and the barrier in `Rename` · `WritePath` records dropped on a name change · `cache.Drop(src, dst)` on a rename and `cache.Drop(cp)` on a create · `Rmdir`'s ENOTEMPTY guard (server-answered) · `Unlink`'s EISDIR guard (local) · `beginCreate` (the mount-side half of Create, so a cell can start the same state) |
| `internal/fsmount/append.go` | `appendHandle.setPath` (an unpublished append follows a rename) |
| `internal/fsclient/write.go` | `NoteDeleted` now drops the remembered base and the hash last served, not only the refusal hold — a removed OR moved name must resolve its own base |
| `internal/fsmount/bfs020_publication_test.go` (new) | 12 cells with their mutations and attribution cells |
| `docs/evidence/BFS-020-probes/*` | the arms (10 live probes), `arm.sh`, `run-arms.sh`, `run-battery.sh`, `mkfixture.sh`, `anchor-replace.pl`, `assemble-evidence.sh` |
| `docs/evidence/BFS-020-arms.sh` + `BFS-020-*.txt` | the mutation battery and the evidence transcripts |
