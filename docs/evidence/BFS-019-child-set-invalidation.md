# BFS-019 — a directory listing through the mount can come back EMPTY (or short) while the directory has entries

**Status:** fixed on `wt/BFS-019` (two hunks, one shared code path). The RED, the
mechanism, the fix, the arms and the negative controls are below. Nothing is
pushed; the main tree was never written to.

**The row's sentence:** `readdir` through the mount answers a WRONG SET, with rc 0,
and nothing ever re-reads it. Lookups keep working in the same instant, so it is a
per-directory corruption of the cached child set, not a dead mount.

**Verdict on that sentence: it is FALSE now.** Measured on a live mount against the
served tree at every step (`docs/evidence/BFS-019-green.txt`,
`BFS-019-general.txt`, `BFS-019-foreign.txt`), the listing matches the server after
one `mkdir`, after a through-mount write + unlink, after three mutations in a row,
inside a nested collection, after a rename, in a 120-entry collection, and after
another writer edits the served tree.

---

## 1. The RED, exactly as filed — raw output

Base tree `e33c6bf` (pre-fix) with binaries built from it; mount against
`probes/davserve` serving a 135-entry tree. `bash docs/evidence/BFS-019-arms.sh red`
→ `docs/evidence/BFS-019-red.txt`.

```
[red] SRC=/tmp/bfs019-red-2IoLEP/src MNT=/tmp/bfs019-red-2IoLEP/mnt
--- [red step 0: fresh mount]
    find <mount>  -mindepth 1 | wc -l = 135   (find rc=0)
    find <server> -mindepth 1 | wc -l = 135
    ls -lR <mount>  | wc -l = 139
    ls -lR <server> | wc -l = 139
    mount  root names: CHANGELOG.md .git go.mod pkg README.md scratch src
--- [red step 1] ONE mkdir through the mount
    mkdir rc=0
    find <mount>  -mindepth 1 | wc -l = 0   (find rc=0)
    find <server> -mindepth 1 | wc -l = 136
    ls -lR <mount>  | wc -l = 2
    ls -lR <server> | wc -l = 143
    mount  root names:
    server root names: CHANGELOG.md .git go.mod oob-dir pkg README.md scratch src
--- [red step 2] a through-mount write + unlink
    write rc=0 / unlink rc=0
    find <mount>  -mindepth 1 | wc -l = 1   (find rc=0)
    find <server> -mindepth 1 | wc -l = 136
    ls -lR <mount>  | wc -l = 6
    ls -lR <server> | wc -l = 143
    mount  root names: oob-dir
    server root names: CHANGELOG.md .git go.mod oob-dir pkg README.md scratch src
--- [red] lookups still work (the mount is not dead)
    stat go.mod      : size=2
    stat src/f000.txt: size=2
    cat src/f000.txt through the mount: 2 bytes
ARM PASS: red src subdir: mount listing == server listing (120 names)
```

| step | `find <mount>` | `find <server>` | `ls -lR` mount | `ls -lR` server | rc |
|---|---|---|---|---|---|
| fresh mount | 135 | 135 | 139 | 139 | 0 |
| after ONE `mkdir` | **0** | 136 | **2** | 143 | 0 |
| after write + unlink | **1** | 136 | **6** | 143 | 0 |

**rc is 0 throughout** — the smoking gun: nothing reports a fault, the answer is
simply wrong. Both shapes are in the transcript: EMPTY after the `mkdir` (the six
originals and their subtree gone from the listing while the server holds 136
entries), and SHORT after write + unlink (the listing holds exactly the one name
this mount created since the invalidation — the row's "1 of 3 entries returned" in
`.git/refs/heads`, one directory up). Lookups are untouched in the same instant
(`stat`, `cat` = 2 bytes) and a collection nobody mutated lists 120/120.

Unit-level RED, no kernel and no server (`internal/fsclient/bfs019_child_index_test.go`):

```
--- FAIL: TestBFS019ReReadAfterAMutationRebuildsTheListing
    after ONE mkdir and the re-read that follows it, readdir answers [] while the served tree
    holds [CHANGELOG.md README.md go.mod oob-dir pkg scratch src] — rc 0, no fault reported
--- FAIL: TestBFS019EveryMutationShapeLeavesAListableDirectory
    [mkdir in the root]               listing []              server [7 names]
    [write published in the root]     listing [written.txt]   server [7 names]
    [create then unlink in the root]  listing []              server [6 names]
    [unlink in a 120-entry collection] listing []             server [120 names]
    [rename across two directories]   listing of "scratch" = [one.txt], server [one.txt two.txt]
    [rmdir]                           listing []              server [5 names]
    [mkdir in a nested collection]    listing of "pkg" = [], server [inner one.txt]
--- FAIL: TestBFS019ARepeatedlyMutatedDirectoryStaysListable
```

---

## 2. The mechanism, named — and what falsified the alternatives

There are **two** production points, on the same object (`Snapshot`) and the same
sentence. Both are needed: the first makes the re-read able to change the answer,
the second makes the re-read happen at all when the invalidation channel is the
thing that moved.

### 2a. The re-read happened, and could not change the answer (`putLocked`)

```
Readdir            (internal/fsmount/fs_linux.go)
   if !snap.Known(n.p) {          <-- the re-read DOES happen after a mutation
       PROPFIND Depth:1
       snap.Put(...) per entry
       snap.MarkDirRead(n.p)
   }
   kids := snap.Children(n.p)     <-- serves the CHILD INDEX

DropReaddir(dir)   (internal/fsclient/snapshot.go) deletes read[dir] AND
                                   children[dir] WHOLE, leaving the NODES in place

putLocked          (PRE-FIX)
   if !existed || old.IsDir != n.IsDir {      <-- index insert only for a NEW node
       s.children[parent][base] = struct{}{}
   }
```

Every mutator through the mount drops the affected collection's listing
(`Mkdir`, `Unlink`, `Rmdir`, `Rename`, `createLink`, the write publication at
`fs_linux.go:2202`, `append.go:333`). The next `Readdir` re-reads — and the re-read
is powerless: the entries it Puts back are already in `nodes`, so an insert
conditional on the node being NEW rebuilds nothing. The index returns holding only
the names created since the drop and `Children` — which intersects the index with
the node set — serves that subset: empty, or one name, with rc 0. `Lookup`/`Has`
read `nodes`, which no mutation touches, which is why `stat`/`cat`/`open` kept
working.

### 2b. The drop took a name out and left the directory "read" (`Drop`)

The mount's own mutations drop a whole listing; the **invalidation channel** drops
the individual names an event carries — and a watched surface's event names the
changed COLLECTIONS as well as the changed names. MEASURED (`BFS-019-foreign.txt`,
the surface's own poll form answered by `X-Bunker-Op: events`):

```
append to inner/f000.txt by another process ->
    {"seq":2,"event":"invalidate","paths":["inner/f000.txt"]}
a new root-level file and a new file inside inner/ ->
    {"seq":4,"event":"invalidate","paths":["oob-root.txt","inner/oob-inner.txt"]}
    {"seq":5,"event":"invalidate","paths":[".","oob-root.txt","inner","inner/oob-inner.txt"]}
```

`Drop(paths...)` removed each path's node and its parent's index entry but never
cleared the parent's READ flag, so the directory kept serving an index it had just
emptied a name out of: an append to `inner/f000.txt` made `f000.txt` vanish from
the mount's listing of `inner/`, and the event's own `"."`/`"inner"` entries made
`inner` vanish from the root listing — SHORT listings, rc 0, the same class as the
empty one. (The event's `"."` fell through as a no-op: `Drop("")` cannot drop the
root's node, so it did nothing at all.) `Drop`'s own doc comment already claimed
the other behaviour: *"and with each path its parent's readdir answer"*.

### Alternatives, tested rather than assumed

| candidate | verdict | what decided it |
|---|---|---|
| the child set stays `Known` after a mutation, so `Readdir` never re-reads | **FALSIFIED for 2a; it is 2b** | After a mutation `read[dir]` is gone (asserted: `!s.Known("")` after `DropReaddir`), and the served answer is EMPTY, not the six STALE names — a never-re-read index would have served the stale, complete set. But the invalidation path (2b) did leave directories `Known` over an emptied index, and that IS the "never re-reads" half. |
| the snapshot intersects against a cache the mkdir/write/unlink path did not update | **FALSIFIED as stated** | The path DOES update it — it deletes it (`DropReaddir`). The defect was that the re-read could not REBUILD it, and that `Drop` deleted an entry without invalidating the set. |
| a per-directory generation/serial that is not bumped on mutation | **FALSIFIED** | There is no generation token; `read` is the only state, and it IS bumped by the mount's own mutations. The live arm shows the re-read happening within the same call. |
| the kernel inode/entry cache (metadata caching is OFF by decision, BFS-054) | **NOT IMPLICATED** | The unit cells reproduce the wrong answer with no kernel at all, and `ls -A` (readdir) is empty in the same instant `stat` (attrs) answers. |
| a `Drop`/`dropChildLocked` asymmetry leaving nodes unnamed | **CONFIRMED as the invariant, fixed as 2b** | `Drop` did remove the node and the index entry together, and only `DropReaddir` left an index inconsistent with the nodes; the invariant is now asserted directly for every cell (`bfs019Unindexed`: no node the tree holds may be missing from its parent's index). |

**One-line cause:** the child index was maintained on NODE INSERTION and
invalidated by deletion, so it was not a function of the node set; a re-read could
not restore it, and a drop removed a name from it without saying the set was no
longer trustworthy.

---

## 3. The fix — two hunks, both in `internal/fsclient/snapshot.go`

```go
// 1. putLocked — the index is maintained for EVERY node the tree holds.
func (s *Snapshot) putLocked(n Node) {
	s.nodes[n.Path] = n
	...
	if s.children[parent] == nil { s.children[parent] = map[string]struct{}{} }
	s.children[parent][path.Base(n.Path)] = struct{}{}      // no `if !existed`
	...
}

// 2. Drop — the parent's LISTING goes with the name, not only the name.
func (s *Snapshot) Drop(paths ...string) int {
	...
	s.dropChildLocked(p)
	s.dropListingLocked(parentOf(p))     // delete(s.read, parent)
	...
}
```

`Children` is documented as answering only for a directory the tree has READ
(`Readdir` is the caller that checks `Known` first), and the invariant the two
hunks establish is one line:

> **`Known(dir)` ⟺ the index names every child the tree holds for `dir`.**

**Re-read eagerly, or invalidate? Both — and that is the point.**

* The mutation paths INVALIDATE (`DropReaddir`, one directory per affected
  collection). That half was already right.
* `Readdir` RE-READS lazily, exactly once per invalidated directory and only when
  the kernel asks. That half was already right too.
* What was missing was that the re-read could not be *recorded* (hunk 1), and that
  the invalidation channel's drop left a directory *read* over a set it had just
  edited (hunk 2).
* Nothing re-reads eagerly: no per-readdir PROPFIND, no cache disable, no "always
  re-read everything". A listing that is already read still costs ZERO round trips
  — the point of the bounded cache is untouched.

**Cost.** Hunk 1: one `map[string]struct{}` write per `Put` (per observed entry,
not per readdir) — the entry already existed in the common case, so no growth in
bytes or entries. Hunk 2: an invalidated directory's next readdir costs ONE
Depth:1 PROPFIND, which is exactly what the mount's own mutations already cost
(they call `DropReaddir`), and it is lazy — only if the kernel lists that
directory, and only once per invalidation however many events name it. Measured:
the fsclient cells (120-entry collection, repeated mutations, all shapes) run in
0.005 s; the 135-entry live mount lists identically to the server at every step
(`BFS-019-green.txt`), and `find`/`ls -lR` counts match the server's exactly.

---

## 4. Guarantees, and the arm that shows each

| case | guarantee | arm |
|---|---|---|
| mutated once through the mount | listing == server | `green` step 1; `TestBFS019OneMkdirDoesNotEmptyItsParentsListing` |
| mutated repeatedly through the mount | listing == server after EACH | `general` G1; `TestBFS019ARepeatedlyMutatedDirectoryStaysListable` |
| nested collections | listing == server for the changed collection AND its parent | `general` G2; `TestBFS019ANestedCollectionIsListedCorrectly` |
| a collection with many entries (120) | listing == server | unit `unlink in a 120-entry collection`; `green` `src` subdir 120/120 |
| the write path (create → publish → unlink) | listing == server after EACH | `green` step 2; `TestBFS019ACreateVisitAndUnlinkLeaveTheListingCorrect` |
| a rename across two collections | both listings == server | unit `rename across two directories`; `TestBFS019ARepeatedlyMutatedDirectoryStaysListable` |
| a name created while the directory is not re-read (the SHORT answer) | listing == server | `general` G3 (`.git/refs/heads` 4/4) |
| another writer edits the served tree, surface HAS a watcher | listing == server (after the declared poll interval) | `foreign` F1/F2; `general` G4; `TestBFS019AnInvalidationDropDoesNotLeaveTheListingShort` |
| another writer edits the tree, surface has NO watcher | see §4.1 | `general-nowatch` |
| a directory whose re-read FAILS | named refusal, never a fabricated listing | `Readdir` returns `errnoFor(err)` — unchanged by this fix |

### 4.1 The server-side writer, the honest boundary

* **A surface with a watcher** (`davserve --watch`): the surface names the changed
  paths — including the changed collections (`"."`, `"inner"`) — the mount drops
  them, the affected listings are un-read, and the next readdir re-reads them.
  **Guaranteed correct**, shown by `foreign` (F1 append and F2 new names, in the
  same output as the server's own `ls`) and `general` G4 (12/12 root, 121/121 src).
* **A surface with NO watcher** (the declared `rev:<counter>` mechanism, which by
  its own declaration moves only on mutations *the surface itself performs*):
  another process's native edit is not announced to the mount, so nothing in
  particular un-reads the directory. The arm `general-nowatch` happens to end
  green — "settled after 2s" — and the mechanism is worth naming rather than
  reading as a guarantee: the mount's OWN earlier mutations (G1–G3) were still
  being journalled and polled when the foreign write landed, so the root and
  `inner` were dropped and re-read for their own reasons, and the re-read saw the
  foreign names. **What this row guarantees is a listing that matches the server
  whenever the mount is TOLD something changed or performed the change itself.**
  What it does not manufacture is knowledge the surface never declared: a mount
  bound to a surface that announces nothing about foreign writes keeps the last
  observed listing, and the surface's own `rev_kind`/`rev_gap` (BFS-048/BFS-061)
  is where that limitation is declared. Before this fix the same situation was
  WORSE in a way the row filed: a directory could be dropped by hand and then
  serve a short listing forever.
* Every other cell in §4 is about a mutation the mount PERFORMED or was TOLD about,
  which is what this row's fix is for.


---

## 5. Negative control per cell, attribution cells, and the neighbouring rows

Two rounds, each with its own single-purpose patch, its own cells, and a
sha256-verified byte-identical restore (the fix is two hunks, so a single mutation
would not tell the two mechanisms apart):

**Round 1 — the child index** (`BFS-019-negative-control.patch`), transcript
`BFS-019-mutations.txt`:

```
MUTATION baseline: internal/fsclient/snapshot.go sha256=5df41b54…a355
MUTATION applied:  sha256=2fd3e174…2c55
CELL 1 unit      → RED  (TestBFS019ReReadAfterAMutation…, …EveryMutationShape…, …RepeatedlyMutated…)
CELL 2 handler   → RED  (after one mkdir: mount answers 0 names, server holds 7;
                         after create+write+close: mount answers 1 name, server holds 7)
ATTRIBUTION      → GREEN (stat of an entry already in the tree, both packages)
CELL 3 LIVE      → RED  (mount=[] then mount=[oob-dir] against a server of 8 names,
                         find rc=0 throughout — the filed RED, reproduced)
restore          → byte-identical (5df41b54…a355), cells GREEN again
```

**Round 2 — the drop's listing invalidation** (`BFS-019-negative-control-drop.patch`),
transcript `BFS-019-mutations-drop.txt`:

```
MUTATION applied: sha256=3d4c…  (the same baseline file, the other hunk reversed)
the DROP cell           → RED
the child-index cells   → GREEN  (the two mutations are independent: one mutation
                                  cannot blanket-red the suite)
ATTRIBUTION cell        → GREEN
LIVE foreign-writer arm → RED  (inner = [f001.txt] against the server's
                                [f000.txt f001.txt] after a foreign append)
restore                 → byte-identical (5df41b54…a355), cells GREEN again
```

The attribution cell is `TestBFS019AttributionLookupsSurviveEveryMutation` (`stat`
of `README.md`, `CHANGELOG.md`, `pkg/one.txt`, `src/fa.txt` must still answer, with
the size the served tree holds) — green under BOTH mutations, because the defect is
a wrong LISTING, not a dead tree. That is what makes the cells independent.

**Neighbouring rows, none regressed** — `docs/evidence/BFS-019-suites.txt`, taken on
the committed fix (`56afadb`):

```
ok  github.com/deployBunker/bunker/internal/server/webdav   33.404s
ok  github.com/deployBunker/bunker/internal/fsclient        42.580s
ok  github.com/deployBunker/bunker/internal/fsmount          0.470s
```

That is BFS-018's symlink cells, BFS-020's publication cells, BFS-021's append
cells, BFS-025's bounds, BFS-030/033's refusals, BFS-031's state bound, BFS-037's
hot cache, BFS-039's cancel cells, BFS-046/049/060-063 on the server — all green,
plus this row's new cells. **`putLocked` is shared code**, so those suites are the
argument that making the index total changes nothing the neighbouring rows rely on.
The one refactor in `fs_linux.go` (`Mkdir` → `beginMkdir`, the mount-side half
extracted for the same reason `beginCreate`/`createLink` exist) is
behaviour-preserving.

The same full-suite guard the repo's pre-commit hook runs came back `PASS
(test mode: full)` with `go_tests — passed` on the committed tree
(`.gitreins/logs/guard-20261002T014839.865974Z.log`).

**A pre-existing flake, named rather than absorbed:** `internal/server/webdav` is
red at the BASE commit too on `TestQA36TheFallbackComparesDigestsNotWholeFiles` —
same test, same subtests, in a pristine clone of `e33c6bf`
(`docs/evidence/BFS-019-suites-base.txt`), and green again in another run. The
package is untouched by this diff, and the test is a same-size/mtime-preserved
1 MiB edit, i.e. one of the cells that is sensitive to how much machine it is
given. A second one-off red (`internal/fsclient`, `TestBFS037Cell05NoReaderEver
SeesAPartialFile`, "the refresh to publish: not satisfied within 5s") reproduced
at the base commit as well and passes in isolation on both trees (14.5 s base,
14.0 s fixed) — a 5-second wall-clock wait on a box at loadavg ~30.

**Environment note — the guard's `go_tests` lane is bound to `TMPDIR`**
(`docs/evidence/BFS-019-env-tmpdir.txt`). Two commit attempts failed on
`internal/registry` at exactly Go's 10-minute per-package ceiling while
`TMPDIR=/mnt/bulk/scratch`, a volume that fsyncs at 404 kB/s against /tmp's
46 MB/s; the same package passes in **4.9 s** with `TMPDIR` on a working volume,
and so does the same full-suite guard. The pristine base clone hung the same way
at 15m34s (test binary sleeping on futex, 0.0% CPU, holding `agents.jsonl.lock`),
so it is not this diff. The commit that landed used `TMPDIR=/tmp` for the
invoking shell only: no test skipped, no `--no-verify`, no repo file changed.

---

## 6. Files

* `internal/fsclient/snapshot.go` — the fix (hunk 1 `putLocked`, hunk 2 `Drop` +
  `dropListingLocked`) and the `Children` contract.
* `internal/fsmount/fs_linux.go` — `beginMkdir` extraction (no behaviour change).
* `internal/fsclient/bfs019_child_index_test.go` — unit cells, the drop cell, the
  index invariant, the attribution cell.
* `internal/fsmount/bfs019_readdir_test.go` — handler cells driving the real
  `Readdir`/`beginMkdir`/`dropPaths`, side by side with `os.ReadDir` on the server.
* `docs/evidence/BFS-019-arms.sh` — `red | green | general | general-nowatch |
  foreign | mutations | mutations-drop | suites`.
* `docs/evidence/BFS-019-negative-control.patch` (round 1) and
  `BFS-019-negative-control-drop.patch` (round 2).
* Transcripts: `BFS-019-red.txt`, `-green.txt`, `-general.txt`,
  `-general-nowatch.txt`, `-foreign.txt`, `-mutations.txt`, `-mutations-drop.txt`,
  `-suites.txt` (on the committed fix), `-suites-base.txt` (the pre-existing
  `webdav` flake at the base commit), `-env-tmpdir.txt` (the guard/TMPDIR
  environment note).
