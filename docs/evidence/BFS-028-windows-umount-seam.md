# BFS-028 — `bunker umount` is POSIX-only; the Windows target, the decision, the evidence

Row: BFS-028 (P1). Repo `/home/kara/bunker`, branch `wt/BFS-028`, base `48ab484`.
Commits: `d2a63ee` (the seam and the decision), `4011bbd` (the cells), this commit (evidence and the guard's ratchet).

---

## 1. THE DECISION, IN ONE PARAGRAPH

**`bunker umount` REFUSES on a platform whose build has no unmount mechanism.** It
does not get a Windows implementation, and it is not a silent success. The refusal
is a named, matchable sentinel (`ErrUmountUnsupported`) expanded into one sentence
that names **the command**, **the platform** (`runtime.GOOS/GOARCH`) and **the way
out**; it reaches the operator as `bunker: <that sentence>` on stderr with exit
status 1 (`cmd/bunker/main.go`), and it fires before any resolution and before any
local side effect. The command stays in the build and in `bunker --help`.

Why a refusal and not an implementation: **there is no mount side on that platform
to be the counterpart of.** `bunker umount` exists to tear down a mount
`bunker mount` created, and every mechanism it is made of is a unix interface — the
kernel mount table to *find* the mount, `fusermount3`/`umount(8)` to *detach* it, a
device-id compare to decide whether a path is a mountpoint. The opt-in bunker-fs
driver already refuses off Linux (BFS-010, decision recorded, "implements nothing"),
and the default sshfs driver's stored invocation is a POSIX one. A Win32 volume-
serial probe could answer *"is this a mountpoint"*, but there is no non-unix answer
to *"what should `bunker umount <agent-id>` DO with that answer"*, so the probe would
only make a command that cannot act look like one that can. Refusal now,
implementation when the mount side has a binding, is the honest order.

Why not a silent success: that is what the code did *by accident*, and it is the
dangerous direction. Every lookup comes back empty on such a platform, so the
command printed `Nothing mounted for <agent> (mount table unreadable; checked the
default mount roots only)` and returned **SUCCESS** — a cleanup verb reporting
success over a mount it cannot see. That is the DF-BUNKER-50 false positive
(BFS-039/DF-BUNKER-50 territory) one platform over, and the `no-gate` control below
reproduces it verbatim.

**THE NUMBERS**

| | before | after |
|---|---|---|
| `GOOS=windows go build ./...` | **exit 1** — 3 errors in 1 file (`undefined: syscall.Stat_t` / `syscall.Stat` ×2) | **exit 0** (and `windows/arm64` exit 0) |
| required cross-GOOS targets that build | linux/amd64, linux/arm64 only | all 4 (`cross-GOOS-build.sh` REQUIRED set) |
| `KNOWN_GAPS` lines in `probes/cross-GOOS-build.sh` | 2 (both this file) | **0** — ratchet contracted |
| `cross-GOOS-build.sh` **BUILD** lane (4 required targets) | windows/amd64 + windows/arm64 = `GAP(internal/cli)` | **all 4 PASS** |
| `cross-GOOS-build.sh` **overall exit** | **1 (FAIL)** | **1 (FAIL) — unchanged, and NOT this row**: its vet lane is red for windows on a pre-existing `internal/fsmount` type error, measured at the base commit (§9c, `BFS-028-crossgos-guard-baseline.txt`) |
| arms outcomes | — | **15 OK, 0 FAIL** (`BFS-028-arms.txt`, exit 0) |
| source mutations, each with a RED and an attribution cell | — | **3** |
| POSIX cells that die if the unix arm is made to refuse | — | **16** (the "unchanged" control) |
| pre-existing untagged test files blocking the cli vet lane | 3 (measured, named §9, **not** fixed — other rows) | 3 (unchanged) |
| linux/amd64 + linux/arm64 build | exit 0 | exit 0 (unchanged) |

The fix is 3 changes to production files plus 2 new test files:

```
internal/cli/umount.go                 the seam's middle, UNTAGGED and testable everywhere
internal/cli/umount_unix.go            //go:build unix   — the probe moved verbatim; the arm answers nil
internal/cli/umount_nonunix.go         //go:build !unix  — the decided refusal
internal/cli/umount_platform_test.go   UNTAGGED         — the cells, executed on this host
internal/cli/umount_nonunix_test.go    //go:build !unix  — the windows arm's own assertions
```

---

## 2. THE DEFECT AS FILED — verbatim, not paraphrased

`GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...` on the base commit:

```
# github.com/deployBunker/bunker/internal/cli
internal/cli/umount.go:272:25: undefined: syscall.Stat_t
internal/cli/umount.go:273:20: undefined: syscall.Stat
internal/cli/umount.go:277:20: undefined: syscall.Stat
```

Exit status 1. Three errors, one file, one function: the mountpoint probe
`isMountPoint` (umount.go:261–281) compared the **device id** of a path with its
parent's via `syscall.Stat`/`syscall.Stat_t`. Nothing else in the module failed for
windows — this one file held the whole target.

Full transcript and its re-runnable reproducer: `BFS-028-red.txt`
(`bash docs/evidence/BFS-028-arms.sh red` re-clones the base commit into a scratch
directory and requires exactly this failure).

## 3. WHAT THE COMMAND IS FOR (read before deciding what Windows should do)

From `bunker umount`'s own doc and the row's history: it tears down a mount
`bunker mount` created — resolving the mountpoint from the mount roots **and the
live kernel mount table** (the DF-BUNKER-50 fix: a mount made at an explicit custom
path must still be found by agent id), refusing ambiguity rather than guessing,
detaching with `fusermount3 -u` and escalating to `-uz` only when a stranded FUSE
session cannot be reached, never disturbing a mountpoint with a live user unless
`--force`, and removing the do-not-build marker with the mount. Idempotence is
deliberate: "already clean" is SUCCESS because cleanup must be safe to run twice.

Every one of those mechanisms is unix. That is the fact the decision follows from —
not "Windows is hard", but "the thing this command drives does not exist there".

## 4. THE SHAPE — following the two seams this repo already has

1. **`internal/fsmount`'s `fs_linux.go` / `fs_unsupported.go`** (`//go:build linux` /
   `//go:build !linux`): a named sentinel, a refusal that happens *before* any local
   side effect, and — the part that mattered here — **the sentence itself living in
   the UNTAGGED file** so a test on the ordinary platform can read it.
2. **`internal/cli`'s own `proc_unix.go` / `proc_windows.go`**: one file per platform,
   **both declaring the same names**, so every call site compiles unchanged.

The seam, concretely:

```
internal/cli/umount.go
  var ErrUmountUnsupported = errors.New("bunker umount: no unmount mechanism on this platform")
  func umountUnsupportedRefusal(goos, goarch string) error      // the sentence, UNTAGGED → testable
  var umountPlatformRefusal = platformUmountRefusal             // the platform's ANSWER, a seam
  func runUmount(...) { if refusal := umountPlatformRefusal(); refusal != nil { return refusal } ... }

internal/cli/umount_unix.go     //go:build unix
  func platformUmountRefusal() error { return nil }
  func isMountPoint(path string) (bool, error)                  // moved VERBATIM (device-id compare)

internal/cli/umount_nonunix.go  //go:build !unix
  func platformUmountRefusal() error { return umountUnsupportedRefusal(runtime.GOOS, runtime.GOARCH) }
  func isMountPoint(path string) (bool, error)                  // refuses; never fabricates "not a mountpoint"
```

Three deliberate choices worth naming:

- **The sentence is untagged.** Buried in a `!unix` file its wording could never be
  asserted on the host that runs tests, so "loud and specific" would have been a
  claim. This is the same reason `internal/fsmount`'s `ErrPlatformUnsupported` lives
  in its untagged `options.go` and is asserted by
  `internal/mountdriver/platform_refusal_test.go`. Here the platform is an
  **argument**, so the text is exercised *and mutation-RED-proved* on linux/amd64.
- **The gate is a package var, like `execCommand` and `readMountTable` in the same
  file.** A build-tagged arm is only ever *compiled* on the platform it is written
  for; substituting the var is the only difference between "Windows" and "Linux" as
  far as the code under test sees, so the refusal is **reachable** from a test that
  runs today (see §5).
- **`isMountPoint` on `!unix` returns an ERROR, not `(false, nil)`.** `(false, nil)`
  is a *fabricated* "not a mountpoint", and it is precisely the answer the clean path
  acts on by deleting the directory.

## 5. THE WINDOWS PATH IS **TESTED**, NOT A CLAIM

### 5.1 The cells (executed here, on linux/amd64)

`internal/cli/umount_platform_test.go` is **untagged**, so the same cells run on
every platform; it substitutes only the platform's ANSWER and then drives the
**real** `runUmount` and the **real** cobra command.

- `TestUmount_UnsupportedPlatformRefusalReachesTheOperator` — two subtests, because
  the accidental behaviour differs in each:
  - **a live mount for the agent** → the accidental behaviour *detaches a path the
    build cannot detach* and prints `Unmounted <path>`;
  - **nothing anywhere, plus the empty leftover directory a previous run left at the
    resolved root** → the accidental behaviour is the *quiet* one: it removes that
    directory and prints `Nothing mounted at … (already clean)`, reporting success
    over a mount it cannot see.
  Both assert: the refusal is returned and matchable, **nothing is printed**, **no
  unmount is executed**, and the leftover directory **survives**.
- `TestUmount_UnsupportedRefusalNamesTheCommandPlatformAndWayOut` — the sentence,
  one named want per clause ("bunker umount", "windows/amd64", "Nothing was
  unmounted", "Linux client", "net use"), plus that the platform is an *argument*
  (a `plan9/riscv64` refusal must carry `plan9/riscv64`).
- `TestUmountCommand_RefusesAsACommand` — the command is still **registered** (not
  `//go:build ignore`-ed, not hidden from `--help`) and **refuses when executed**
  with the root command's own `SilenceErrors`/`SilenceUsage`, which is how
  `cmd/bunker/main.go` runs it: a non-nil error and no success text on its output.

### 5.2 The windows arm's own assertions (`//go:build !unix`)

`internal/cli/umount_nonunix_test.go` is the mirror of
`internal/fsmount/platform_unsupported_test.go`: it asserts the arm's **real**
default with **no substitution** — `platformUmountRefusal()` is the named refusal
carrying *this build's* `GOOS`/`GOARCH`; `runUmount` refuses and writes nothing;
`isMountPoint` refuses instead of answering; the command is constructed and refuses
through cobra.

**These cannot EXECUTE in this environment, and saying so is the point** — there is
no Windows host here. What is verified is that they **compile and type-check for the
platform they are written for**:

```
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./internal/cli
→ PASSES once three PRE-EXISTING untagged test files (other rows, §9) are excluded
```

Measured and recorded as a re-runnable arm (`bash docs/evidence/BFS-028-arms.sh vet`,
transcript in `BFS-028-arms.txt`): the script clones the tree, **names** the three
blockers, removes them **in the clone only**, and requires the lane to pass. So
`umount_nonunix_test.go` and `umount_platform_test.go` are compiled for
windows/amd64 — the strongest check available without a Windows host, and the same
one BFS-010 relies on.

**What is NOT verified, stated plainly:** the refusal has never been *executed* on
Windows. What makes that acceptable here is that the executed cells (§5.1) run the
identical production code path with only the platform's answer substituted, and
§5.2 proves the arm supplies that answer on a Windows build.

## 6. POSIX BEHAVIOUR IS UNCHANGED — and tested as such

`isMountPoint` moved **byte-for-byte** except for one doc sentence, whose claim
("behaves the same on every platform this CLI builds for") the seam made false.
`platformUmountRefusal` returns `nil` on unix, so the gate is a no-op and
`runUmount`'s resolution order, its idempotent "already clean" success and its
unmount escalation are exactly what DF-BUNKER-50 left.

The *testable* form of "unchanged" is the `unix-refuses` control: make the unix arm
return the refusal instead of `nil` and **16 POSIX cells die**, including
`TestIsMountPoint_FalseForPlainDir` and `TestMountFault_StrandedMountPointIsDetectable`
(the two consumers of the moved probe) and every DF-BUNKER-50 cell. A behaviour that
is genuinely unchanged is a behaviour whose cells are all load-bearing.

## 7. THE CONTROLS — three mutations, each RED with an attribution cell

Script: `docs/evidence/BFS-028-arms.sh`; full transcript `BFS-028-arms.txt`
(`mode=all`, **15 OK / 0 FAIL**, exit 0). Each mutation is a patch applied with
`git apply`, run against the named cell, and restored from a byte copy whose sha256
is re-checked; a restore that does not match aborts the run.

| control | mutation | MUST go RED | attribution: MUST stay green |
|---|---|---|---|
| `no-gate` | delete the gate from `runUmount` | GATE cell — **4 `--- FAIL:`**, incl. `runUmount reported success … "Unmounted <path>"` and `"Nothing mounted at … (already clean)"` | POSIX cells (the gate was already a no-op on unix) |
| `vague-refusal` | reword the refusal into `unsupported build (goos/goarch)` | TEXT cell — **1 `--- FAIL:`** naming the three dropped clauses ("Nothing was unmounted", "Linux client", "net use") | GATE cell |
| `unix-refuses` | the unix arm refuses too | POSIX cells — **16 `--- FAIL:`** | GATE cell (a cell that substitutes the seam is unaffected by what the real arm says) |

Restores verified byte-identical:

```
umount.go     699d5db94c1706792fd7748bc9b7fd4a48ab64d10e17b373ee4607b08e5e25e6
umount_unix.go e78c6ba30948e1297da4229ddc527e30a5be260da795cd019ff77d407cf07b64
```

Every mutation COMPILES — each RED above is an assertion failure, never a build
error, which is what makes the cells load-bearing rather than accidentally red.

## 8. THE GUARD'S RATCHET, CONTRACTED

`probes/cross-GOOS-build.sh` has listed this file as a known gap since BFS-010:

```
KNOWN_GAPS="windows/amd64|github.com/deployBunker/bunker/internal/cli
windows/arm64|github.com/deployBunker/bunker/internal/cli"
```

The script's own doctrine is that a listed gap is *a ratchet, not an excuse*:
"removing a line means the gap is fixed, and adding one needs the same evidence a
defect report does". `KNOWN_GAPS` is now **empty**, and the header of the gap
listing records what was removed, why, and who owned it. The run is captured in
`BFS-028-crossgos-guard.txt` with the two windows targets building and the
`internal/fsmount` seam type-checking on all four.

The script also gained a named NOTE where the next seam author will look: item 2's
vet lane is per-package, so a seam inside `internal/cli` is *not* type-checked for
windows by that script, and extending it is blocked by the three files in §9a.

**AND THE HONEST HALF OF THAT: the guard still exits 1, for a reason that is not
this row.** Its two columns come apart, and they should be read separately:

```
TARGET           BUILD      VET internal/fsmount     RESULT
linux/amd64      PASS       PASS                     ok
linux/arm64      PASS       PASS                     ok
windows/amd64    PASS       REFUSED                  UNEXPECTED FAILURE     <- this row's lane: PASS
windows/arm64    PASS       REFUSED                  UNEXPECTED FAILURE     <- this row's lane: PASS
RESULT: FAIL — a required target broke in a package that is not a listed gap
```

The **BUILD** column is the lane whose ratchet this row contracts, and it is PASS on
all four targets. The FAIL comes from the **vet** lane, which is red for windows on a
pre-existing defect in `internal/fsmount` — reproduced at the base commit
`48ab484`, so the script was already exiting FAIL before this row existed. It is
named in §9c, with the one-line cause and the exact commands; this row did not
introduce it and, per the boundary, did not fix it. Both captures:
`BFS-028-crossgos-guard.txt` (after) and `BFS-028-crossgos-guard-baseline.txt` (base).

## 9. FINDINGS NAMED, NOT FIXED (other rows)

Per the row's boundary (`must not scope-creep into the other POSIX-only sites; name
them if you find them in your evidence so they can be filed`):

**a. Three untagged test files in `internal/cli` cannot compile for windows**
(measured by the vet lane tripping over each in turn; each is the *first* error of
its file, and the lane was re-run after each removal):

```
internal/cli/mount_fault_inject_test.go:116  undefined: newMountTestServer   (declared in the unix-tagged mount_test.go)
internal/cli/procbuild_test.go:419           undefined: syscall.Flock
internal/cli/exit_code_pipe_test.go:184      undefined: buildCLIOnce
```

They do **not** affect `go build ./...` (test files are not built), which is why the
row's own target is clean and this is a separate, smaller finding: the package's test
tree is unix-only. Fix is a build tag per file; the vet lane then covers
`internal/cli`, and `probes/cross-GOOS-build.sh` item 2 should be pointed at it.
(A fourth consumer, `internal/cli/mount_lifecycle_test.go`, also called `isMountPoint`
and is compilable for windows now *because* both arms declare that name — the
signature-parity reason the name exists in both files.)

**b. `internal/cli/procbuild_test.go:419` uses `syscall.Flock`** — the same class of
defect as BFS-028 itself, one file over.

**c. THE GUARD'S OWN VET LANE IS RED, for a defect in `internal/fsmount`** — the
same class again, and the more consequential one, because it is the lane that
exists to catch this class. Reproduced at the base commit `48ab484`
(`BFS-028-crossgos-guard-baseline.txt`), so it is not of this row:

```
$ GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./internal/fsmount
# github.com/deployBunker/bunker/internal/fsmount
vet: internal/fsmount/platform_unsupported_test.go:110:5: invalid operation: st != (fsclient.Status{}) (struct containing fsclient.CacheStats cannot be compared)
exit=1
```

`fsclient.Status` is not comparable: its `Cache CacheStats` field
(internal/fsclient/status.go:86) carries `BypassReasons map[string]int64`
(internal/fsclient/cache.go:256), so Go rejects `!=` on the struct at
platform_unsupported_test.go:110. The file is `//go:build !linux`, so it is never
compiled on the host that runs this repo's tests — only `go vet` for a non-linux
target compiles it, which is the check BFS-010's own file header says exists to
catch precisely this ("A seam that is only ever compiled on one platform has not
been checked"). **The consequence to file: item 2 of
`probes/cross-GOOS-build.sh` has not been running as a check**, and the script has
been exiting FAIL on both windows targets regardless of the gap list. One-line fix,
owner's choice: `reflect.DeepEqual(st, fsclient.Status{})`, or assert the fields the
test actually means (the surrounding assertions already do the meaningful ones).

**d. Not investigated here (out of scope, no claim made):** the other non-unix
targets the guard only SURVEYS (darwin, the BSDs, plan9, js/wasm) are not built by
the required set, so the seam's behaviour there is *decided* (`!unix` refuses) but
not measured. The `mount(8)` fallback's output parser is also untested on darwin,
where `mount` prints `device on /path (fstype…)` and the parser would read field 1
as the literal `on` — pre-existing, unrelated to this row, and named here rather than
asserted.

## 10. RE-RUNNING THIS EVIDENCE

```
bash docs/evidence/BFS-028-arms.sh all    # red, green, 3 controls, the vet lane — 15 OK / 0 FAIL
bash probes/cross-GOOS-build.sh           # the guard, KNOWN_GAPS empty, 4/4 targets
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...   # the acceptance criterion
go test ./internal/cli -count=1                          # the whole package, POSIX included
```

Tier 1 `gitreins guard` ran **at commit time** for all three commits of this branch
(the worktree's hooks resolve to the shared `.git/hooks`, and the log is under
`.gitreins/logs/`): `test_mode: full`, `test_targets: all`, overall **PASS** —
secrets, go_build, go_lint, go_tests. No `--no-verify` was used anywhere.
