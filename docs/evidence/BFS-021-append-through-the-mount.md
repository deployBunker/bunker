# BFS-021 — an append through the mount lands its bytes

**Row:** BFS-021 (P1, DATA LOSS) · **Repo:** bunker · **Worktree:** `wt/BFS-021` (not pushed) ·
**Author:** Hermes
**Status:** fixed and measured. The RED (`docs/evidence/BFS-021-red.txt`) is a live mount: the
appended bytes are gone from the file READ BACK, and one of the shapes reports **success** while
dropping them. The GREEN (`docs/evidence/BFS-021-green.txt`) is the same arms on the fixed tree,
every one of them byte-for-byte. Eleven red-proof arms
(`docs/evidence/BFS-021-arms.sh`, transcript `BFS-021-arms.txt`) turn every claiming cell red and
every named attribution cell stays green, with the sha256-verified byte-identical restore printed
per arm.
**Design inputs read first:** BFS-030 (`>` emptied a file then failed — the same class, landed),
BFS-033 (a refusal HOLDS), BFS-038 (a refresh publishes by ONE atomic swap onto a new immutable
blob), BFS-039 (cancel-IO: a retried identical write must not move the mtime), BFS-012
(`BFS-012-writeshape.txt`, the write-shape measurement), BFS-005 §5.3 (where the write
precondition comes from).
**Code changed:** `internal/fsmount/append.go` (new) · `internal/fsmount/fs_linux.go` ·
`internal/fsclient/status.go` · `internal/cli/fs.go` · `internal/fsmount/append_test.go` (new) ·
`docs/evidence/BFS-021-probes/*`.

---

## 0. Verdict in one screen

| # | what the row asks | RED on the tree this row started from (measured) | GREEN | the mutation that turns the cell red |
|---|---|---|---|---|
| 1 | an append through the mount lands its bytes | `>>` rc=1 *"printf: I/O error"*, tail ABSENT, file unchanged; `O_WRONLY\|O_APPEND` write **errno 95 ENOTSUP** | the tail is on the server byte-for-byte, read back through the mount as well | `write-not-dispatched` |
| 2 | …and a retried append does not double-apply | (no write ever landed) | a chunk retried at the same offset rewrites the same bytes: tail once, `identical_content`, **mtime unmoved** | `chunk-applied-at-the-end`, `size-follows-the-write-not-the-offset` |
| 3 | a refused append does not land, and the refusal holds | (no write ever landed) | ESTALE, the target keeps the concurrent edit, the next append on the path is refused too, and a caller re-read recovers it | `publication-bypasses-the-refusal-hold` |
| 4 | the bound is loud, and nothing is silently dropped | — | EFBIG with a Detail naming what to do instead; ONE GET and not a single PUT | `bound-not-enforced` |
| 5 | the O_TRUNC/refusal semantics do not change | `r+b` write refused 95, `>` refused at the open, original intact | unchanged, byte-for-byte | `any-write-intent-open-is-an-append`, `resize-refusal-removed` |
| 6 | a later publication on the same handle carries the earlier one's bytes | — | PUT 204 (107 B) → PUT 204 (120 B) → PUT 204 (132 B), every one landed | `base-does-not-advance` |
| 7 | a concurrent reader never sees a torn file | — | **12 appends against 1063 concurrent reads, 0 torn** (2 refused ESTALE, the documented re-open recovery) | `commit-in-place-instead-of-rename` |
| 8 | a cancelled append is retryable and applies once | — | EINTR, then ONE more attempt, tail once, no named buffer left behind | `cancel-is-terminal`, `named-buffer` |

Two of those rows (6 and 8) are defects **in this row's own first implementation**, found by the
arms rather than by review — see §4.3 and §4.4.

---

## 1. THE DECISION, and the measurement that chose it

**Append is SERVED, by read-modify-publish: the handle reads the content once, buffers the arriving
chunks at their own offsets on top of it, and publishes the whole result as ONE conditional PUT
through the existing `WritePath`.**

### 1.1 First, WHICH shape an append is (measured, not assumed)

`docs/evidence/BFS-021-trace.txt`. A scratch copy of the tree
(`/tmp/bfs021/instr`, rsync'd from the worktree — the worktree itself is untouched) carried ONE
temporary trace line in `node.Open` and ONE temporary `node.Write` that logs the request and then
answers exactly what go-fuse's bridge answered before any `Write` method existed. Every arm's
`BFS021-TRACE` lines, verbatim:

```
BFS021-TRACE open  path=src/target.txt flags=0x8401 O_APPEND=true O_WRONLY=true O_RDWR=false O_TRUNC=false
BFS021-TRACE write path=src/target.txt off=82 len=25 handle=*fsmount.readHandle
```

* The open flags DO reach the mount: **`0x8401` is `O_WRONLY|O_APPEND`** (`0x8000` is go-fuse's own
  `FOPEN_DIRECT_IO` bit). So the mount knows this is an append.
* The write arrives **at an offset equal to the current size** (the file is 82 bytes), on a handle
  that is **not an `fs.FileWriter`** — which is exactly why the write failed: go-fuse's bridge is
  `if fr, ok := f.file.(FileWriter); ok {…}` … `return 0, fuse.ENOTSUP`, i.e. **errno 95**.
* There is **no size-carrying SETATTR** in the append shapes at all (the trace shows the `O_TRUNC`
  shape's open `0x8001` and no write). That is why BFS-030's refusal — a *resize* arriving while a
  write handle is live — never covers append, and why this row needs its own path rather than a
  widened guard.
* Two more measured facts that shaped the fix:
  * **`multi`** (three writes inside ONE open, `exec 3>>f`) sends all three at **the same offset**:
    `off=82 len=25`, `off=82 len=13`, `off=82 len=12` — a failed write never grew `i_size`, so the
    kernel re-issued every chunk at the same place. A fix that concatenated chunks would therefore
    write a three-times-too-long file the moment it started working. **The chunk must be applied at
    the offset the kernel gave it** — which is also the idempotence (§6).
  * **`rplus`** (`r+b`, no `O_APPEND`) sends `off=0`, i.e. a genuine in-place write, which stays
    refused (BFS-012's clause 2, kept by BFS-030) — the regression the arms pin in §8.

### 1.2 The options, and why the others were rejected

* **A ranged/offset write against the existing resource.** The WebDAV surface has no partial-`PUT`
  shape: `internal/server/webdav/handler.go:handlePut` reads the whole body, stages it as a sibling
  (`stageBody`) and commits by `os.Rename` (`tree.go:581`). Serving append this way means a new wire
  shape **plus** a server-side append — and a server-side append is a **delta**, which cannot satisfy
  this row's idempotence requirement without a dedupe key of its own: a retried delta double-applies
  its bytes, while a retried whole content cannot, because the retry IS the final content and the
  surface already answers that with its own reported no-op (BFS-039's `identical_content`, mtime
  unmoved — reused here rather than re-implemented, §6). Named as a residual in §10.
* **A declared refusal of `O_APPEND`.** A refusal is only acceptable if it cannot lose data and it is
  LOUD — and it does not lose data (nothing is published). But it is not ENOUGH here, and the
  measurement is what settles it: one of the shapes of this very defect **reports success while
  dropping the bytes**. `exec 3>>f` + three `printf >&3` → **shell rc=0**, the server byte-identical
  to the fixture, all three chunks nowhere (`BFS-021-red.txt`, arm `multi`). A loud refusal of the one
  shape the shell reports correctly still leaves the silent one silent, and append is a POSIX
  guarantee a mount cannot decline without breaking every tool that logs.
* **Chosen: read-modify-publish.** It needs **no wire change and no server change**, and it inherits
  the landed invariants instead of re-implementing them: the publication is the existing ONE
  conditional PUT, so BFS-038's single atomic swap, BFS-033's refusal hold and BFS-039's retry-is-a-
  no-op all hold **by construction**. The price is O(size) per append (§9), stated as a number rather
  than implied.

---

## 2. The RED, as a measurement (the UNFIXED tree)

`docs/evidence/BFS-021-red.txt` and `BFS-021-trace.txt`. Fixture: a file of **82 B, sha256
`66ac40abbf895c383fd020e209816a78b322a8f0e23eb4376edbbb77533c874d`** on the server's own disk, read
ONCE through a real mount (which is what fixes the client's base and publishes `i_size` — i.e. what
makes the append offset well defined). The tail is **25 B**, `APPENDED-TAIL-0123456789\n`; the
correct result is therefore 107 B, sha256
`60cbd86bc803056198122ffb2093f0fb2fb5424dfd85e8ad8d0722e0c133d6d5`.

| shape (on an EXISTING file) | result on the unfixed tree | server after | the appended bytes |
|---|---|---|---|
| `sh -c 'printf %s "$1" >> "$2"'` — **the shape the row filed** | rc=1, `sh: 1: printf: printf: I/O error` | 82 B, sha256 unchanged | **ABSENT** |
| `os.open(O_WRONLY\|O_APPEND)` + `write` | errno **95 ENOTSUP** "Operation not supported" | unchanged | **ABSENT** |
| `open(path,'ab')` + `write` | errno **95** | unchanged | **ABSENT** |
| `os.open(O_RDWR\|O_APPEND)` + `write` | errno **95** | unchanged | **ABSENT** |
| **`exec 3>>f` + 3 writes inside ONE open** | **rc=0 — the caller is told SUCCESS** | unchanged | **ABSENT** — *silent* loss |
| `>>` a path that does NOT exist (the control) | rc=0, the new file lands | 25 B ✓ | present |
| `open('r+b')` + write (BFS-012's shape) | errno **95** refused | unchanged ✓ | refused, as documented |
| `>` on an existing file (BFS-030's shape) | refused at the open | unchanged ✓ | refused, as documented |

**The lost bytes, stated as a claim the arm decides rather than a remark.** Every arm reads the file
back — through the mount AND on the server's own disk — and asserts three things: the shape reported
an error (or, for the silent shape, that it did **not**), the appended tail is **not** in the
content, and the original is **byte-identical** (not merely prefix-equal). So this row's defect is
distinguished from BFS-030's, which is the opposite pairing (an error AND the original destroyed) and
which the same table shows does not recur.

---

## 3. The GREEN (the FIXED tree)

`docs/evidence/BFS-021-green.txt`. Same fixture, same shapes, same instruments:

| shape | result on the fixed tree | server after | wire |
|---|---|---|---|
| `>>` (the shell) | rc=0 | **107 B, sha256 `60cbd86b…`**, read back through the mount as well | 1 GET + 1 PUT, 204 |
| `O_WRONLY\|O_APPEND`, `'ab'`, `O_RDWR\|O_APPEND` | all land | 107 B ✓ | 1 GET + 1 PUT each |
| `exec 3>>f` + 3 writes in ONE open | rc=0 | **132 B `aa1035f5…` = base + ALL THREE chunks, each exactly once** | **3 PUTs, 204, 204, 204** — the `If-Match` chain advancing `66ac40ab…` → `60cbd86b…` → `2df2fc…` |
| `>>` a NEW path (control) | lands | 25 B ✓ | — |
| `r+b` write | **still refused 95** | unchanged ✓ | 0 requests |
| `>` on an existing file | **still refused** (BFS-030's rule, at the open) | unchanged ✓ | — |
| reader during 12 appends | — | final = base + T1..T12 exactly | **1063 reads served, 0 torn** |

The owner-facing surface also carries it (`bunker fs status`, from a green run):

```
append       : published_total=1 refused_total=0 max_file_bytes=268435456
  last       : src/target.txt: published=107 bytes
```

---

## 4. What the mechanism is, in the code

### 4.1 The dispatch

`node.Write` becomes the ONE entry point for a FUSE write (`fs.NodeWriter`):

* a CREATE's `*writeHandle` → called exactly as the bridge called it, so the whole existing write path
  is untouched;
* an `*readHandle` that carries an append state → the append path;
* **anything else → `EOPNOTSUPP`**, which is byte-for-byte what go-fuse's bridge answered before the
  method existed. That is BFS-012's refusal of an in-place write through an existing path, unchanged.

`node.Open` still hands back a READ handle whatever the flags say (BFS-030's premise), and merely
attaches the append state when the flags carry `O_APPEND`.

### 4.2 The buffer, and why the chunk goes where the kernel put it

The base content is read ONCE per handle (`client.Get`), and the hash that pins it comes from the
SAME exchange — the publication's precondition is therefore the exact content the append was built
on, and a concurrent edit is a loud 412 instead of a lost update. The buffer is an **anonymous**
(unlinked-from-birth) temp file, the same construction the write path uses — one buffer rule, not
two — so a killed appender leaves nothing behind. Each arriving chunk is written **at the offset the
kernel gave it**, never "at the end".

### 4.3 The base advances with the publication (a defect the arms found)

The kernel asks for **more than one publication point per open** — `FLUSH` per `close(2)`, and
`exec 3>>f` closes three times. The first implementation pinned every publication to the base the
handle started from, so the second one was refused 412 while the caller's later chunks were dropped
and the shell still exited 0. The wire shows it (`BFS-021-red.txt` records this on the fixed tree
before the rule existed):

```
PUT 204  req_bytes=107  if_match="66ac40ab…"
PUT 412  req_bytes=120  if_match="66ac40ab…"   ← the stale base: the caller's bytes dropped
```

The base now advances with every landed publication, and the same shape on the fixed tree publishes
`204 (107 B)` → `204 (120 B)` → `204 (132 B)`. It is pinned by
`TestALaterPublicationOnTheSameHandleCarriesTheEarlierOnesBytes`, whose mutation (`base-does-not-
advance`) reddens it and nothing else (§8).

### 4.4 A chunk after a publication point reopens the handle

`write` clears `flushed` on the next arriving chunk, so a publication point followed by more writes
(the `fsync`-then-more shape) cannot leave the later bytes buffered and never sent. Without this the
handle would accept bytes and silently never publish them — the class of defect this row exists for.

### 4.5 The bound

An append publishes the whole file, so the buffer holds the file and `--write-buffer-max-bytes`
bounds it (256 MiB, `writeBufferMaxDefault`). At or above the bound the append is **refused loudly
with a named cause and nothing written**, and the refusal says what to do instead:

> refusing to append to `src/target.txt`: the file is 82 bytes and this surface publishes an append as
> ONE whole-file conditional PUT, bounded by `--write-buffer-max-bytes` (40). Nothing was written and
> the file is unchanged. **Append to this file on the server instead, or write it whole**

The bound is the SAME variable the status document reports (`append.max_file_bytes`), so a figure
reported one way and enforced another — a defect class this project keeps re-finding — cannot happen
here. `appendBound` is a variable precisely so the boundary can be tested at a value a test can
reach; the reported figure and the enforced one are the same object.

---

## 5. The concurrent-reader guarantee

**Stated:** a reader sees the OLD complete content or the NEW complete content, never a torn mixture.
**Why it holds:** the append publishes through the ONE conditional PUT the surface commits by staging
a sibling and renaming it (BFS-038), so a reader's open resolves to one inode or the other — the
append path never writes into a published blob, and never in place.
**Tested twice:**

* **live**, `BFS-021-green.txt` arm `reader`: one process appending into a real mount while another
  reads through the same mount in a tight loop. Every complete state is known exactly
  (base + T1..Tk), so a torn read is a sha256 that is in **no** state. Result: **12/12 appends
  landed, 1063 reads served, 2 refused with ESTALE (BFS-025's read bound, the documented re-open
  recovery), 0 torn**, and the final server content is exactly base + T1..T12 (nothing lost, nothing
  doubled).
* **handler-level**, `TestAReaderDuringAnAppendSeesAWholeContentNeverATear`: 40 appends interleaved
  with a reader goroutine. It is **non-vacuous by construction** — it fails if the reader observed
  fewer than 2 distinct complete states, i.e. if it never raced the publications. Its mutation
  (`commit-in-place-instead-of-rename`) makes the surface commit in place and the cell goes red with
  torn reads, which is what proves the assertion can fail.

---

## 6. The idempotence rule (BFS-039 survives)

Three claims, each measured:

1. **A chunk retried at the same offset rewrites the same bytes.** Applying each chunk at its own
   offset makes a retry an OVERWRITE of the same place, and the result is then the whole content the
   server already holds — so the surface answers its own **reported no-op**:
   `verdict=identical_content`, `noop=1`, **and the mtime does not move** (asserted directly, with the
   mtime read from the server's own file before and after). This is not hypothetical: the trace shows
   the kernel re-issuing every chunk of a failing open at the same offset (`off=82`, `off=82`,
   `off=82`), which is exactly the state an accidental cancel leaves.
2. **A cancelled publication is not a verdict.** The cancel is returned as **EINTR**, nothing is
   recorded as a failure, the handle stays publishable, and the retry lands **once** — the
   discriminating assertion is that the tail appears exactly once in the read-back. Mutation
   `cancel-is-terminal` reddens it.
3. **A whole-content retry cannot double-apply.** The body is the final content, not a delta, so even
   a retry that reached the server and was answered successfully is the same bytes: the surface
   reports `identical_content` and writes nothing. That is the property the delta wire shape (§1.2)
   could not have without a dedupe key.

---

## 7. The precondition contract (BFS-033)

A refused append does not land, and the refusal **holds**:
`TestAStaleBaseRefusesTheAppendAndNothingLands` builds the append on one read, moves the target under
it, then publishes. The publication is refused **ESTALE**, the target keeps the concurrent edit
byte-for-byte, the refusal is counted (`append.refused_total=1`), and — the BFS-033 half — a SECOND
writer on the same path is refused too *without a single request landing*, because the publication
goes through `WritePath.Publish` and its hold. The cell then walks the documented recovery: a
**caller** read clears the hold (the mount's own internal reads deliberately do not — the truncate
path documents the same rule), and the retry lands the right bytes. Mutation
`publication-bypasses-the-refusal-hold` reddens exactly that hold half.

---

## 8. The NEGATIVE CONTROL (one mutation per cell, sha256-verified restore)

`docs/evidence/BFS-021-arms.sh` (transcript `BFS-021-arms.txt`); eleven mutations, each an exact
source edit applied with `git apply`, run **by name**, with a declared RED set and a declared GREEN
(attribution) set, restored from `git checkout` and checked against the pre-arm sha256 of every
touched file — a restore that is not byte-identical aborts the run. Result: **11/11 arms met both
declared outcomes, 11/11 restores byte-identical.**

| mutation | RED (must fail) | GREEN (must stay green) |
|---|---|---|
| `write-not-dispatched` | C1 append lands | C5 refusal, BFS-030 |
| `chunk-applied-at-the-end` | C2 retry | C1, C5 |
| `size-follows-the-write-not-the-offset` | C2 | C1, C5 |
| `publication-bypasses-the-refusal-hold` | C3 refusal holds | C1, C5 |
| `bound-not-enforced` | C4 bound | C1, C5 |
| `any-write-intent-open-is-an-append` | C5 refusal | C1, C5b, BFS-030 |
| `commit-in-place-instead-of-rename` | C7 no torn read | C1, C5 |
| `base-does-not-advance` | C6 later publication | C1, C5 |
| `cancel-is-terminal` | C9 cancel+retry | C1, C5 |
| `named-buffer` | C9 anonymous buffer | C1, C5 |
| `resize-refusal-removed` | BFS-030, C5b | C1, C10 |

The cells are INDEPENDENT: no mutation blanket-reddens the suite, and each declares the cells that
must survive it. Two of the arms found real problems while being written, which is the point of
writing them:

* **`named-buffer` first left C9 GREEN.** C9 asserted that the append buffer is anonymous by globbing
  the mount's own directory — and the shared `testMount` helper left that directory empty, so
  `os.CreateTemp("")` put the buffer in the default temp directory and the glob could never see
  anything. The cell was **blind**. Fixed by giving the cell a mount whose directory it can look at
  (`testMountAppend`); the mutation now reddens it.
* **`resize-refusal-removed` first reddened an "attribution" cell.** The cell it reddened was
  asserting BFS-030's rule, so it was a CLAIMING cell for that mutation, not an attribution one: the
  cell was split into C5 (the non-append write refusal) and C5b (BFS-030's rule under the new
  write-intent shape), and the declarations were corrected rather than the mutation weakened.

The runner also refuses to accept a **vacuous** run: a `-run` pattern that matches no test
(`no tests to run`) or produces no per-test result line is a hard failure, not a pass.

---

## 9. Cost, as a number

`docs/evidence/BFS-021-cost.txt`. N=50 appends of one 25 B tail to one file through a real mount (a
fresh `open(O_WRONLY|O_APPEND)`/`write`/`close` per append — the `>>` shape a log or a shell uses), on
the loaded fleet box (loadavg ≈ 20):

```
50 appends to one file : 4645 ms total, 93.99 ms median, 92.90 ms mean (p95 121.66, min 47.94, max 133.65)
file                   : 82 B -> 1332 B, sha256=5b3174710074707e…
wire                   : 50 PUT (35975 B published), 51 GET (34807 B read back)
```

**The figure to read is `bytes_put`:** an append is O(size), not O(tail) — 35,975 B published for
1,250 B appended at this size, and the ratio grows with the file. The latency is the two serialized
round trips (one base GET, one publication PUT) that the mechanism costs by construction. That is the
price of serving append through a whole-file surface without a new wire shape; it is named here, and
the alternative that would remove it is a residual with a trigger, not a hidden assumption (§10).

**Nothing was traded away on the path that already worked:** `go build ./...` rc=0, `go vet ./...`
rc=0, `gofmt -l` empty, and `go test ./... -count=1` green with **0 failures** across 29 packages
(including `internal/fsclient`, `internal/fsmount`, `internal/server/webdav`, `internal/cli` and
`internal/docscheck`).

---

## 10. What this must not do — and did not

* **No bytes lost on any path this row adds — including the failure path.** Every refusal writes
  nothing (asserted by request count: the bound refusal makes ONE base GET and not a single PUT) and
  leaves the file byte-identical; the base-advance bug that DID lose bytes in the first
  implementation (§4.3) was found by the arms, fixed, and is now pinned by a cell with its own
  mutation.
* **Nothing is written into a published blob (BFS-038).** The publication is the existing
  stage+rename conditional PUT. The `commit-in-place-instead-of-rename` mutation exists to show that
  the cell would catch a violation.
* **No re-truncation on a failed operation (BFS-030).** The append path publishes no
  size-carrying SETATTR at all — the trace shows an append issues no SETATTR — and BFS-030's refusal
  is untouched: `resize-refusal-removed` shows its own cells (including the new C5b) are the ones
  that go red, while every append cell stays green.
* **No silent success while dropping data — the worst possible outcome.** This is the shape the row
  warns about and the RED measured it on the unfixed tree (arm `multi`: rc=0, bytes gone). On the
  fixed tree the same shape lands all three chunks, and "each chunk appears exactly once" plus
  "every publication landed" are asserted per arm.
* **The O_TRUNC / refusal semantics are unchanged.** `rplus` and `trunc` carry the SAME expectation
  (`loss`, i.e. refused with the original byte-identical) on BOTH the red and the green batteries, so
  the fix cannot be bought by changing what is refused; and `write_shape_test.go`'s BFS-030 cells run
  green in every arm.
* **Append was not fixed by disabling `O_APPEND` or making the mount read-only.** The mount is
  read-write, the append works, and the new refusal is narrower than the one it replaces (one
  boundary, loudly named).
* **The non-Linux seam is untouched.** `append.go`/`append_test.go` are `//go:build linux`; the
  cross-GOOS guard's transcripts before and after this change are **byte-identical**
  (`/tmp/bfs021/cross-goss-baseline.txt` vs `cross-goss.txt`, produced on a scratch `git clone
  --shared` of the pre-fix `main` and on the worktree), including the pre-existing windows/cli
  failure at `internal/cli/fs.go:164` — which is therefore attributed to the baseline, NOT to this
  row.

---

## 11. Named costs and honest residuals

* **O(size) per append.** The wire carries the whole file, twice (one GET, one PUT). For a small file
  appended in a loop this is the measured 94 ms median; for a large file it is proportional. The
  alternative — a server-side append with an idempotency key — is **not implemented**, deliberately:
  it needs a new wire shape, a server-side commit, and a dedupe key with its own bound, and it must
  satisfy §6's third claim. Trigger for revisiting: an append-dominated workload (a log) on a large
  file, where the published-bytes figure above is the cost.
* **A file at or above 256 MiB cannot be appended through this surface.** Refused loudly (EFBIG, the
  message names the substitute), reported as `append.max_file_bytes`, nothing written. The substitute
  is to append on the server, or write the file whole.
* **The refusal is on the WHOLE append**, not on the chunk that crossed the bound: the handle is
  spent (sticky failure), because the mount publishes the whole content and a partial append would
  have to invent a merge this surface cannot express.
* **A refused append is terminal for the HANDLE and stands on the PATH** (BFS-033's hold). The
  recovery is the documented one: re-read the path, then append again. A caller that retries the
  same handle without re-reading is refused again with the same verdict — deliberately, because
  retrying a stale base is the lost update the precondition exists to prevent.
* **Not fixed here, by design:** BFS-012's clause (2) for a NON-append in-place write (`r+b` stays
  refused — it is not expressible as a whole-file publication without an in-place read the caller
  never agreed to), and the kernel's `FUSE_FLUSH`-per-close means an open with several closes pays
  several publications (measured: three for `exec 3>>f` on a 132-byte file). Both are recorded rather
  than hidden.
* **One extra GET per append** relative to a theoretical minimum: the base is read on the first chunk
  even when the mount holds a cached copy of the path. It is deliberate — the base bytes must be the
  bytes the precondition pins, and the cache's bytes are not evidence about the server (the same rule
  BFS-025's `guardServed` follows). Skipping it would trade one round trip for spurious 412s.

---

## 12. Reproduction

```bash
# the live batteries (unfixed = the RED set, fixed = the GREEN set), ~1 min per battery
docs/evidence/BFS-021-probes/run-arms.sh --bin <bunker-unfixed> --davserve <davserve> \
  --tag red --set red   --work /tmp/bfs021/w-red   --outdir /tmp/bfs021/evidence-red
docs/evidence/BFS-021-probes/run-arms.sh --bin <bunker-fixed>   --davserve <davserve> \
  --tag green --set green --work /tmp/bfs021/w-green --outdir /tmp/bfs021/evidence-green

# the shape measurement and the cost arm
docs/evidence/BFS-021-probes/mount-arm.sh --label trace --tree /tmp/t --bin <bunker> --davserve <ds> \
  --trace --reader python3 --reader-args "docs/evidence/BFS-021-probes/bfs021-arms.py --mode fd --expect loss"
COST_APPENDS=50 docs/evidence/BFS-021-probes/mount-arm.sh --label cost --tree /tmp/t --bin <bunker> \
  --davserve <ds> --trace --reader python3 \
  --reader-args "docs/evidence/BFS-021-probes/bfs021-arms.py --mode cost --expect ok"

# the cells, the red-proof arms, and the evidence files
go test ./internal/fsmount/ ./internal/fsclient/ -count=1
bash docs/evidence/BFS-021-arms.sh all
bash docs/evidence/BFS-021-probes/assemble-evidence.sh
```

The probes and the arms are in-tree (`docs/evidence/BFS-021-probes/`, `docs/evidence/BFS-021-arms.sh`)
and every file cited above was produced by them: the evidence files are assembled from the runs by a
script rather than transcribed by hand, so a reader can regenerate them.

---

## 13. Files changed

| file | change |
|---|---|
| `internal/fsmount/append.go` (new) | the append path: the base read with its own hash, the anonymous buffer, the offset-addressed chunk, the publication that advances its base, the bound and the loud refusal, the owner-facing figures |
| `internal/fsmount/fs_linux.go` | `node.Write` (the one dispatch point, delegating to the write handle and refusing everything else with the bridge's own `ENOTSUPP`), the append state on an `O_APPEND` open, Flush/Release publication points, the shared anonymous-buffer helper, the `append` block in `Status()` |
| `internal/fsclient/status.go` | `AppendState` (`published_total`, `refused_total`, `max_file_bytes`, `last`) |
| `internal/cli/fs.go` | `bunker fs status` prints the append figures beside the write-shape figures |
| `internal/fsmount/append_test.go` (new) | 10 cells + the `testMountAppend` helper, each with its mutation and attribution cell declared in the arms script |
| `docs/evidence/BFS-021-probes/*` | the arms (8 live shapes + reader + cost), the mount arm, the request-counting proxy, the assembler |
| `docs/evidence/BFS-021-arms.sh` + 11 `BFS-021-redproof-*.patch` | the red-proof battery and its mutations |
| `docs/evidence/BFS-021-{red,green,trace,arms,cost}.*` | the evidence |
