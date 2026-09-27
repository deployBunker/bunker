# BFS-030 — a failed write must not destroy the original (the write-side twin of BFS-025)

**Status: fixed, with the RED, the GREEN, a negative control that goes red, the
successful-path cost as a number, and the cancel interaction stated and tested.**
Branch `wt/BFS-030` (worktree `/home/kara/worktrees/bunker-BFS-030`), not pushed.
Row: `BFS-030` (P0, data loss). Design authority read first:
`docs/prd/PRD-bunker-invalidation.md` §2.5 and §2.8; related rows read:
BFS-012 (the write-shape measurement), BFS-015 (the commit window, landed),
BFS-025 (the read-side twin, landed), BFS-039 (cancel-IO, open).

---

## 1. The decision

**A resize that arrives while a handle opened for WRITING is live on the path is
refused with `EOPNOTSUPP`, before anything is read or published.**

That is the mechanism: the *destructive half of an in-place rewrite is never
published*, because this surface can never complete the write half of one
(`node.Open` hands back a read handle whatever the open flags say; a write handle
is only ever created by `node.Create`, for a path that does not exist — BFS-012
measured every in-place shape as refused). Since the truncation has no durable
successor here, it is never published at all. Of the three shapes the brief named,
this is **"do not publish the truncation until the write is durable"** taken to its
limit: the write can never be durable through this surface, so the truncation is
never published. It is not a rollback (nothing to roll back) and not copy-on-write
(the original is simply left alone).

**Why the predicate is a live-handle scan and not the Setattr's file handle.**
That was measured before choosing, and it is the reason this mechanism is the only
one of its kind available:

```
BFS030-TRACE setattr path=src/target.txt size=0 fh_nil=true fh_type=<nil> valid=0x208
```

The kernel's `O_TRUNC` half arrives as `SETATTR` with **`fh = 0`**
(`valid = FATTR_SIZE|FATTR_FH`) — it names no handle, exactly like a deliberate
path-based `truncate -s`. Only the *in-place handle* shape (`open('r+b')` +
`ftruncate`) carries a handle (`valid=0x248`, `*fsmount.readHandle`). So a rule
keyed on "the request came from a handle" would miss the data-loss shape entirely;
the live write-intent handle on the path is the discriminator. Full table:
`BFS-030-trace.txt`.

**Cost on the successful path: none, and the numbers say so** (§6): median
per-file write latency 0.438–0.498 ms before and after (the spread of the *same*
binary is wider than the difference between binaries), because the rule adds no
work to the create/write/publish path — a create issues **no** size-carrying
`SETATTR` at all (`BFS-030-trace.txt`, run-shapes4). The added work is one scan of
the mount's own handle table, only on a size-carrying `Setattr`:
**51 ns/op** for the predicate and **3.4 µs** for a full refusal.

---

## 2. The RED, as a measurement (the UNFIXED tree)

`docs/evidence/BFS-030-red.txt`, `BFS-030-trace.txt`. Fixture: a file of
**4352 B, sha256 = `df601c57892e9fcaa7fbba47be14a84d06b934a269edde4123c5b0071656347a`**
on the server's own disk, read once through the mount (which is what fixes the
client's base), then the shape a shell's `>` or an editor's save performs.

| shape (on an EXISTING file) | result on the unfixed tree | server after |
|---|---|---|
| `open(O_WRONLY\|O_CREAT\|O_TRUNC)` → write (`>`) | open **ok**, write fails **errno 95 EOPNOTSUPP** | **0 B, sha256 `e3b0c442…`** — the ORIGINAL IS GONE |
| `open(O_RDWR)` → `ftruncate(fd, 0)` → write | ftruncate **ok**, write fails **95** | **0 B, sha256 `e3b0c442…`** — the ORIGINAL IS GONE |
| `open('r+b')` → write | open ok, write fails 95 | unchanged ✓ |
| `open(O_WRONLY\|O_APPEND)` → write | open ok, write fails 95 | unchanged ✓ |
| `os.truncate(path, 32)` (deliberate) | **ok**, landed | 32 B ✓ |
| endpoint SIGKILLed, then `>` | open fails **errno 5 EIO**, nothing can land | unchanged ✓ |
| `>`, no prior read (base resolved by `HEAD` fetch-then-check) | open **ok**, write fails **95** | **0 B, sha256 `e3b0c442…`** — the ORIGINAL IS GONE |

Two of those are the filed defect (a second one, the in-place `ftruncate`, was
found by the same probe and is the same class: a resize published as the first
half of a write this mount cannot serve). `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`
is the sha256 of the empty string — the original content is not truncated, it is
**gone**, while the caller is told the write failed. That is the worst pairing the
row names.

### The attribution: one HTTP request

`BFS-030-trace.txt` — the mount behind a logging reverse proxy, every request with
a monotonic timestamp, `>` on the unfixed tree (shape took 119 ms):

```
  9  296084.935  GET   200   0 B  -                                  /dav/src/target.txt
 10  296084.978  GET   200   0 B  -                                  /dav/src/target.txt
 11  296085.053  PUT   204   0 B  "sha256:df601c57892e9fcaa7fbba47be..."  /dav/src/target.txt
 12  296085.054  PROPFIND 207 ...                                    /dav/src/target.txt
 13  296085.103  GET   200   0 B  -                                  /dav/src/target.txt
```

Request 11 is the defect as one line: **a 204 on a conditional PUT of ZERO bytes
whose `If-Match` is the hash of the original content**. The destructive half was
published *inside the open*, and only then did the write half report failure — and
the trace shows the mount was neither retrying nor confused: it did exactly what
the current code says, and the current code says it is safe. `e3b0c442…` after.

### Which shapes are distinguishable at the FUSE layer (measured, before choosing)

```
a = os.truncate(path, 32)        size=32 fh=nil  valid=0x208   -> deliberate, MUST keep working
b = open('r+b') + ftruncate(0)   size=0  fh=*readHandle 0x248 -> in-place resize (emptied the file)
c = open('wb') + write           size=0  fh=nil  valid=0x208   -> the shell's '>' (the data loss)
d = open('wb') + nothing         size=0  fh=nil  valid=0x208   -> ': > file' (a deliberate empty)
e = os.truncate(path, 0)         size=0  fh=nil  valid=0x208   -> deliberate empty, MUST keep working
```

`c` and `e` are byte-identical at this layer. No rule reading only the `Setattr`
can separate them; the live write-intent handle can, and does (§4).

---

## 3. The GREEN (the FIXED tree)

`docs/evidence/BFS-030-green.txt`. Same fixture, same shapes, same instruments:

| shape | result on the fixed tree | server after |
|---|---|---|
| `>` on an existing file | **open fails errno 95 EOPNOTSUPP** (the refusal lands at the kernel's `O_TRUNC` half, so `open()` itself reports it) | **byte-identical** `df601c57…` |
| `open(O_RDWR)` → `ftruncate(fd, 0)` | ftruncate **refused 95**, write still fails 95 | **byte-identical** |
| `open('r+b')` → write | write fails 95 (unchanged) | byte-identical |
| `open(O_WRONLY\|O_APPEND)` → write | write fails 95 (unchanged) | byte-identical |
| `os.truncate(path, 32)` | **still lands** (32 B, byte-for-byte) | 32 B ✓ |
| endpoint SIGKILLed, then `>` | open fails 95 (refused locally, no publication attempted) | byte-identical |
| `>`, no prior read (base resolved by `HEAD` fetch-then-check) | open fails 95 | byte-identical |

The failure is still **loud**: the caller gets `EOPNOTSUPP` (errno 95) at the open,
and the owner-facing surface carries it two ways — a log line, and a countable
record in `bunker fs status`:

```
write shape  : refusals_total=1
  last       : src/target.txt: size=0
  cause      : SETATTR src/target.txt: errno=EOPNOTSUPP cause=write_shape_unsupported: refusing to
               resize src/target.txt to 0 bytes: a handle opened for writing is live on the path,
               and this surface cannot write an existing file in place — publishing the resize would
               destroy the original before the write that follows it fails. ...
```

### The transport-kill control says what the defect actually is

On the **unfixed** tree with the endpoint SIGKILLed, `>` fails at the open with
`EIO` and the **original survives** (`BFS-030-red.txt`, red3-serverdown). So the
loss does not come from "the transport failed" — it comes from **the destructive
publish landing and the write half failing after it**. That is exactly BFS-030's
claim, and it is now measured from both directions.

---

## 4. The rule, in the code

`internal/fsmount/fs_linux.go`:

* `writeIntentOn(path)` — scans the mount's **own live handle table** (`m.reads`,
  the table `Release` empties) for a handle whose `writeIntent` flag is set. Read
  from the same table the read path maintains, so the record cannot outlive the
  handle it describes and needs no second bookkeeping to stay in step.
* `node.truncate` refuses, first statement, when that predicate holds: no `Get`,
  no `ResolveBase`, no `Publish`, nothing published and nothing read
  (`BFS-030-green.txt`: the client made **0** requests during the refusal).
* The refusal is reported through the same channels BFS-025's read refusal uses: a
  `Cause` (`write_shape_unsupported`, deliberately distinct from
  `local_capability` — nothing about the local environment is wrong), an atomic
  counter and a last-refusal string in the status document, a log line.

**Deliberate resizes are untouched**: with no write handle open,
`truncate -s N file` / `os.truncate` still read the content, publish exactly N
bytes and succeed — the shape BFS-012's truncate measurements depend on.

**Why not refuse the Open instead?** That was the alternative: refuse
`open(O_WRONLY|O_RDWR)` on an existing file. It closes the same hole, but it also
breaks **reads through an `O_RDWR` handle** — a capability that works today — and
this rule does not. Narrower rule, same safety: the refusal is attached to the
destructive step itself.

---

## 5. The NEGATIVE CONTROL (the fix disabled → the arm goes red)

`BFS-030-negative-control.patch` (the guard removed), the run in
`BFS-030-negative-control.txt`:

```
fixed-tree sha256 (before): bf2fe192337581ac76834fc69037dea2fb3917199ff363d2e4171bd08c2bf61e
patch applied; sha256 (disabled): 7e19d2f6698ccbe63d56390fdfb6185705f1fcf45ea000ee3e18821212537cef
$ go build ./...                       OK
$ go test ./internal/fsmount/ -count=1
--- FAIL: TestResizeThroughAWriteHandleIsRefusedBeforeItPublishes
--- FAIL: TestFtruncateWithAWriteHandleIsRefusedToo
--- FAIL: TestResizeIsAllowedAgainOnceTheWriteHandleIsReleased
--- FAIL: TestRefusedResizeDoesNotPoisonTheTransportVerdict
FAIL  github.com/deployBunker/bunker/internal/fsmount
restored sha256 : bf2fe192337581ac76834fc69037dea2fb3917199ff363d2e4171bd08c2bf61e
backup sha256   : bf2fe192337581ac76834fc69037dea2fb3917199ff363d2e4171bd08c2bf61e
$ go test ./internal/fsmount/ -count=1
ok    github.com/deployBunker/bunker/internal/fsmount
```

And the live arms on the disabled tree (`BFS-030-negative-control.txt`, built from
the patched tree as `bunker-nofix`): `trunc_open` and `ftrunc_fd` go **RED** with
the data-loss verdict (4352 B → 0 B, `e3b0c442…`), while the deliberate-resize
control arm still passes — i.e. the arms are pointed at the fix and not at the
fixture. Restore is sha256-verified above.

---

## 6. The successful-path cost, as a number

The shape that works — a whole-file write: create a NEW path, write 8 KiB, close —
200 files per arm, run three times per binary, on the loaded fleet box
(loadavg 10–26 during the runs). `docs/evidence/BFS-030-cost.txt`:

| arm | per-file median | mean | throughput |
|---|---|---|---|
| before, buffered (close only) | 0.498 ms | 3.97 ms | 1.97 MiB/s |
| after, buffered | 0.476 ms | 2.84 ms | 2.75 MiB/s |
| before, buffered (2nd/3rd run) | 0.498 / 0.455 ms | 0.81 / 5.97 ms | 9.7 / 1.3 MiB/s |
| after, buffered (2nd/3rd run) | 0.447 / 0.438 ms | 0.82 / 0.94 ms | 9.6 / 8.3 MiB/s |
| before, fsync (publication included) | 37.44 ms | 46.7 ms | 0.167 MiB/s |
| after, fsync | 33.32 ms | 43.1 ms | 0.181 MiB/s |

**The number: 0.44–0.50 ms per file either way.** The between-binary difference is
*smaller* than the run-to-run spread of the same binary (before: 1.3–9.7 MiB/s),
because the fix adds no work at all to this path: no syscall, no request, no new
step. The means are outlier-dominated (p95 ≈ 25 ms buffered, ≈ 111 ms fsync on this
host) which is why the medians are the figure to read. Every file was verified
byte-identical on the server's disk in every arm (200/200), so a "faster" run that
silently stopped writing would be caught rather than reported.

The only new work in the whole change, measured directly:

```
BenchmarkWriteIntentOn-16      20000    51.00 ns/op     (the predicate)
BenchmarkRefusedResize-16      20000  3433 ns/op        (a full refusal: error, counter, log, record)
```

And the refused shape is *cheaper* than the destructive one it replaces: **1 ms**
refused (t_mono 296112.493 → .494, `BFS-030-trace.txt`) against **119 ms** for the
round trip that used to empty the file.

---

## 7. Cancellation (BFS-039's subject) — stated, tested, and scoped

PRD §2.8 splits cancellation in two, and its requirement is a property of state
("every cache mutation is atomic and idempotent, so the absence of a cancel is
never corrupting"), with a separate vocabulary requirement (`EINTR` vs `EIO`).
`docs/evidence/BFS-030-cancel.txt`, four arms, all with the victim killed by
`SIGKILL` at a **deterministic** point (the victim reports its own open phase
through a sentinel, so the kill cannot land before the destructive half would have
published — the first version of this arm was vacuous and was rebuilt):

| arm | unfixed tree | fixed tree |
|---|---|---|
| caller SIGKILLed mid-`>` (no cancel ever delivered) | the destructive half had published; the file is **EMPTY** (`e3b0c442…`), PUTs 0 → 1 | the open is **refused before publication** (PUTs 0 → 0), the file is **byte-identical**, the mount still reads |
| caller SIGKILLed while HOLDING a write-intent handle | — | a dead caller's handle does **not** stick: a later deliberate resize lands (32 B) |
| the errno vocabulary | — | **95 EOPNOTSUPP**, not `EINTR` (4), not `EIO` (5) |

So: **a cancelled write also leaves the original intact**, because the refusal (and
therefore the whole operation) precedes publication — and because the publication
that remains is a single atomic conditional PUT.

**The EINTR/EIO half is BFS-039's, and this row does not implement it.** Measured,
so the claim is not an impression: `grep -rn EINTR internal/` → **0 occurrences**,
and nothing in `internal/fsmount` consults the FUSE interrupt channel
(`fuse.Context.Cancel`) at all — `errnoFor` maps an error with no named errno to
`EIO`. Today, therefore, a *deliberate* cancel cannot be told from a failure at the
errno level. **Coordination needed:** BFS-039 must own delivering `EINTR` (and
deciding what a cancelled-but-already-refused shape reports). This row's
contribution is that the fix introduces no new ambiguity — the refusal is a
capability answer (`EOPNOTSUPP`, neither `EINTR` nor `EIO`), it is idempotent, and
it happens before any state changes, so the absence of a cancel cannot corrupt
anything. BFS-039's own dependence on BFS-030 (`depends_on: [..., "BFS-030"]`) is
satisfied: the P0 it waited for is closed by this change.

---

## 8. What this must not do — and did not

* **BFS-015's commit window is untouched.** The fix is entirely inside
  `internal/fsmount` + the status document; `internal/server/webdav` (the striped
  per-path commit lock and the in-commit re-validation) is not in the diff at all.
  Its four tests run green under `-race -count=1`
  (`BFS-030-tests.txt`): `TestExpectedHashIsRevalidatedInsideTheCommit`,
  `TestCreateOnlyRuleIsRevalidatedInsideTheCommit`, `TestCommitSectionIsExclusive`,
  `TestConcurrentConditionalWritesCannotBothLand`.
* **No change to what a stock HTTP/1.1 client sees.** No wire change: the refusal
  never reaches the wire, and a stock client's requests are byte-for-byte what they
  were (the PUT shape, the preconditions, the status codes are untouched).
* **The read path is not slower, and its tests are green.** `-race -count=1` on
  `internal/fsmount` (BFS-025's read-bound arms included) and `internal/fsclient`
  (`BFS-030-tests.txt`). No read-path code changed; the only new predicate is read
  from a map the read path already owns, and it is not on any read path.
* **BFS-021 is not implemented** (append still fails at the write, unchanged —
  "work or be refused", not fixed here).

## 9. Named costs, and the honest limits

* **Two shapes that used to "work" are now refused**: `: > file` (open `O_TRUNC`
  with no write) and a *growing* `ftruncate` through an `O_RDWR` handle. Both are
  in-place write shapes this surface cannot complete, and the row's acceptance
  explicitly allows refusal; the supported substitutes are `truncate -s 0 file` and
  `truncate -s N file` (both verified working, §3). This is the cost of the
  narrow-but-blind rule: the mount cannot know whether a write follows a
  truncation, so it refuses the destructive step of a shape it cannot finish.
* **The refusal is per-path and depends on a live handle**: a deliberate
  `truncate -s` issued while *another* process holds a write-intent handle on the
  same path is refused too. The alternative would be publishing a truncation whose
  companion write cannot land — the defect.
* **Handle liveness is the kernel's contract.** The predicate is only sound because
  every successful `FUSE_OPEN` gets a `RELEASE`; a handle leaked *by the mount*
  would refuse resizes on that path until a remount. Measured: a caller killed
  while holding a handle does not leave the record behind (§7), and `Release` is
  the same code path the read path already depends on.
* **Not fixed here, by design**: BFS-021 (`>>`), BFS-012's clause (2) (in-place
  writes unreachable), BFS-033 (a refusal recorded then a 204 landing), BFS-039
  (`EINTR`). Each has its own row; this change is compatible with all of them
  because it *adds* a refusal and publishes nothing.
* **Sibling lanes**: nothing in this area had landed since BFS-025 (`bc1793b`); the
  BFS-030 worktree was clean at `main` HEAD `0998ae2` when this started, so this is
  not a duplicate. Only this row's files are touched.

## 10. Reproduction

```bash
# the live battery (unfixed = RED set, fixed = GREEN set), ~2 min per arm
docs/evidence/BFS-030-probes/run-arms.sh --bin <bunker>        --tag red   --expect-set red   --outdir /tmp/bfs030/evidence
docs/evidence/BFS-030-probes/run-arms.sh --bin <bunker-fixed>  --tag green --expect-set green --outdir /tmp/bfs030/evidence

# one shape by hand (the arms are one-liners over this probe)
docs/evidence/BFS-030-probes/mount-arm.sh --label mine --tree /tmp/t --bin <bunker> \
  --davserve <davserve> --trace --reader python3 \
  --reader-args "docs/evidence/BFS-030-probes/bfs030-arms.py --mode trunc_open --expect loss"

# cancellation, cost, the negative control, and the gates
docs/evidence/BFS-030-probes/cancel-arms.py        # via mount-arm.sh, --mode {kill-during-trunc,...}
docs/evidence/BFS-030-probes/successful-path-cost.py
docs/evidence/BFS-030-probes/negative-control.sh
docs/evidence/BFS-030-probes/tests-evidence.sh
```

The probes are in-tree (`docs/evidence/BFS-030-probes/`) and every file cited above
was produced by them; the assemble scripts exist so the evidence files can be
regenerated from a run rather than trusted.

## 11. Files changed

| file | change |
|---|---|
| `internal/fsmount/fs_linux.go` | the rule: `writeIntentOn` + the refusal at the top of `truncate`; the `write shape` figures in `Status()`; a `/tmp`-free, comment-documented block (70 added lines) |
| `internal/fsclient/errors.go` | `CauseWriteShapeUnsupported` |
| `internal/fsclient/status.go` | `WriteShapeState` + the `write_shape` block of the status document |
| `internal/cli/fs.go` | `bunker fs status` prints the write-shape figure and its last refusal |
| `internal/fsmount/write_shape_test.go` | 8 new tests (the rule, the second hole, the deliberate-resize control, reads through a write handle, release does not stick, the flag table, the transport verdict) |
| `internal/fsmount/write_shape_bench_test.go` | the guard's cost as a benchmark |
| `internal/fsmount/read_bound_test.go` | `testMount` takes `testing.TB` so a benchmark can build the same substrate (no behaviour change) |
| `docs/evidence/BFS-030-probes/*` | the probe suite: arms, cancel arms, cost, SETATTR shapes, the logging proxy, the mount arm, the assemblers, the negative control |
| `docs/evidence/BFS-030-{red,green,trace,cancel,cost,tests,negative-control}.*` | the evidence |
