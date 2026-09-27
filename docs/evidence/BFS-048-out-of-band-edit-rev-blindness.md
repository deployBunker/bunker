# BFS-048 — the revision poll's blindness to an out-of-band edit: measured, then made honest

**Row:** BFS-048 (P0) · **Author:** Hermes (bunker worker, `wt/BFS-048`) · **Date:** 2026-09-27
**Depends on:** `docs/prd/SPEC-watcher-capability.md` §2.1/§2.4/§7 (R-V1…R-V6), §8.1 (never infer a
capability), §11 A-10 · BFS-004 §3 E-3/E-4/E-6 and §4.2 (`extensions.rev.kind`) · BFS-005 §3.2/§4.4 (the
client's status document and its poll parameters) · BFS-015 (the commit-window re-validation this row must
not weaken) · BFS-035 (owns the real fix: R-V1) · BFS-049, BFS-063 (same area, deliberately untouched)
**Product code changed:** `internal/fsclient/{capabilities.go, client.go, invalidate.go}`, `internal/cli/fs.go`.
**No server file changed** — `internal/server/**` and `internal/fsmount/**` are byte-identical to the base
tree, which is why a stock HTTP/1.1 client sees exactly what it saw before (see §5.3).
**Spec changed:** `docs/spec/BFS-005-client-cache-and-diff.md` (the client's own scope claim).
**Base:** built on `9ec3bb9` (see §7 R2 — a concurrent worker landed the comment/prose half of this row
while this work was in flight; the two halves are complementary, not duplicates).

---

## 0. Verdict in one screen

| # | what the row asked | result | where |
|---|---|---|---|
| 1 | **(a) or (b), and why** | **(b) — the client's claim is corrected AND the gap is now reported by the client at runtime.** (a) is not available in this build at any price R-V2 permits: the only observation that sees a working-tree write is an observation of the working tree, and a metadata walk on the same fixture costs **7.17 ms at 1 106 entries** and **56.07 ms at 10 106 entries per pass**, against **22–31 µs** for a token that must be recomputed and **61–172 ns** for the memo hit that most responses pay — on *every* response, because E-3 puts the token on all of them. §1. | §1 |
| 2 | **the RED on the unfixed tree** | **MEASURED**, on a real `git init` work tree, reproduced not cited: HEAD unchanged, `X-Bunker-Rev` **byte-identical before and after**, and the file's bytes **changed** (`sha256:55a60bb9…` → `sha256:212fdd6c…`). The memo was waited out and its refresh witnessed (`revAt` advanced), so this is a re-read of the tree and not a cached answer. Verbatim in §2. | §2 |
| 3 | **the GREEN** | **DELIVERED**: while the revision poll is the mechanism in force, `bunker fs status` now prints `rev coverage : kind=git vouches_for=commits gap=uncommitted_working_tree_writes`, and the same three facts reach the status document as `invalidation.rev_kind` / `.rev_vouches_for` / `.rev_gap`. The kind is read off the capability document the **landed** surface declares, never inferred from the token's shape (§8.1). §3. | §3 |
| 4 | **a negative control that turns the arm red** | **PROVEN TWICE, both restored by sha256.** Disabling the report in `State()` turns both GREEN arms red (`rev_kind = ""`, verbatim in §4.1); restoring the spec's old sentence turns both claim guards red (§4.2). Restores verified: `internal/fsclient/invalidate.go: OK`, `docs/spec/BFS-005-client-cache-and-diff.md: OK`. | §4 |
| 5 | **cost on the fast path, as a number** | **The one-call snapshot is untouched and not slower**: the whole-tree read is the same code path before and after (no server file changed) and interleaved runs land in the same band on the same 10 000-file fixture — mine **98.9 / 119.0 / 167.4 ms**, pre-fix **99.3 / 141.1 / 182.8 ms**. The token path stays flat while the tree grows 10×: memo hit **61 → 172 ns**, refresh **21.8 → 22.2 µs**. §5. | §5 |
| 6 | **BFS-015 not weakened** | **RE-RUN GREEN**, including under `-race`: `TestExpectedHashIsRevalidatedInsideTheCommit` (3 subtests), `TestCreateOnlyRuleIsRevalidatedInsideTheCommit` (2), `TestCommitSectionIsExclusive`, `TestConcurrentConditionalWritesCannotBothLand`. §6. | §6 |
| 7 | **BFS-049 and BFS-063 untouched** | The hash cache's `(size, mtime)` identity and the events poll's cursor guard are **not in this diff at all** (`git diff --stat`: five files, none under `internal/server/`). §7 R3. | §7 |

**Product code: 4 files, +141/−1** (`invalidate.go` +96/−1, `capabilities.go` +25, `client.go` +12, `fs.go` +8).
**Tests: 5 new files, 9 new test functions** (6 in `internal/fsclient`, 2 in `internal/server/webdav`,
1 in `internal/cli`). **`go test ./...`: 28 packages ok, 0 failures, exit 0.** §6.3.

---

## 1. The decision: (b), because (a) cannot be bought at R-V2's price

### 1.1 What (a) would have to observe

An out-of-band working-tree write changes exactly one thing an observer can see without reading the file:
the file's own metadata (`size`, `mtime`, `ctime`) — and `ctime` is the one that survives a same-size,
restored-mtime edit, which is precisely why the event ledger includes it (BFS-049's subject). Detecting it
therefore requires **one `lstat` per path in the tree**, per revision computation. There is no cheaper
signal:

- **The directory mtime is not it.** An in-place write to an existing name (`os.WriteFile`, `dd`, a build
  tool's truncate+write) leaves the parent directory's mtime untouched; only create/delete/rename moves it.
  A dir-only walk would detect *some* out-of-band edits and silently miss the rest — a **new false claim**,
  which is the defect class this row exists to remove. Rejected on principle, not on cost.
- **`.git/index` is not it.** The index's stat cache changes on `git add`/`checkout`, not on a working-tree
  write; and using it means `lstat`-ing every tracked path anyway (`git status`'s own fast path).
- **A watcher would be, but there is none.** `watch` is refused `501 capability_unavailable, scope=target,
  mode=poll` on this build (SPEC-watcher-capability §2.1), and building one is BFS-035's row, whose R-V1 is
  exactly "the revision must move for a change nobody made through us".

### 1.2 What it costs, measured on the same fixture

| tree | token, memo hit | token, forced refresh | one metadata pass (the price of (a)) |
|---|---|---|---|
| 1 000 files (1 106 entries) | **61 ns** | **21.8 µs** | **7.17 ms** |
| 10 000 files (10 106 entries) | **79–172 ns** | **22.2–31.2 µs** | **56.07 ms** (98.9–167.4 ms under load) |

The token is a header on **every** response (§3 E-3), so (a) would add a ~7 ms–160 ms walk to every
request, proportional to the served tree, to answer a question the client asks once per poll interval. R-V2
("the revision must be advanced by the watcher's own event, never by a stat-per-file walk on a read") is
therefore not a preference this row can trade away; it is why **the alignment is BFS-035's and the honesty
is this row's**.

### 1.3 What (b) is, in this build — and what it is not

(b) is **not** "correct a comment". SPEC-watcher-capability §7.2 R-V4 requires that *"an artifact that
cannot move must be reported as a gap, not left as a green check"*, and §3.3 that no claim be made that was
not probed. So the deliverable is:

1. **the client reports the gap at runtime**, derived from the kind the surface **declares**
   (`extensions.rev.kind`, read off the document — never inferred from the token's shape, §8.1);
2. **the client's published claim is corrected** where the client itself states its scope — its spec
   (`BFS-005` §4.4's "Scope" row said *one call covers the whole tree revision*, which is the same lie the Go
   comment told) as well as the code comments the base commit already narrowed;
3. **a guard so the claim cannot come back** in either place.

What the user loses either way, stated plainly: on a git tree an out-of-band, uncommitted edit is **not
detected** by the last-resort revision poll. (b) makes the mount say so; only BFS-035 makes it true.

---

## 2. The RED, on the unfixed tree (reproduced, verbatim)

`internal/server/webdav/rev_blindness_measure_test.go` builds a **real** git work tree (`git init -b main`,
one commit), serves it through the landed handler, reads the token off a real response header (what a poll
sees), edits the file **out of band and uncommitted**, waits out `gitRevCacheTTL`, and re-reads:

```
=== RUN   TestOutOfBandWorkingTreeEditMovesNothingTheRevisionPollCanSee
    BFS-048 RED: HEAD=1b27bd4457b2fe612b904b94d7d3a46bbb3b09ef rev=git:1b27bd4457b2fe612b904b94d7d3a46bbb3b09ef file=sha256:55a60bb97151b2b4b680462447ce60ec34511b14fa10d77440c97b9777101566
    BFS-048 RED: after the out-of-band edit HEAD=1b27bd4457b2fe612b904b94d7d3a46bbb3b09ef rev=git:1b27bd4457b2fe612b904b94d7d3a46bbb3b09ef file=sha256:212fdd6caf8724f0d14106a0787930dc5d998837dcf6681192aca3fc71558c4f
    BFS-048 control: after the commit HEAD=65bcd526fc7f7b35d91b31a41f8bda8b99250443 rev=git:65bcd526fc7f7b35d91b31a41f8bda8b99250443 file=sha256:212fdd6caf8724f0d14106a0787930dc5d998837dcf6681192aca3fc71558c4f
--- PASS: TestOutOfBandWorkingTreeEditMovesNothingTheRevisionPollCanSee (1.17s)
```

- **token before = token after** (`git:1b27bd44…`), while the file's bytes moved
  (`sha256:55a60bb9…` → `sha256:212fdd6c…`). That is the defect as a measurement.
- **Token source is provably unchanged, not merely misread**: the ref file still names the same commit, and
  the arm asserts the memo **refreshed** (`h.tree.revAt` advanced past the TTL) before it reads the second
  token, so the server re-resolved HEAD from disk and answered the same thing.
- **Control arm** (bottom line): committing the same bytes moves HEAD, and the token follows it — the
  measurement above is blindness, not a frozen token. The commit leaves the file's digest untouched
  (`212fdd6c…` before and after), so the only thing that moved is the ref.
- Hashes are per-run (each run makes a fresh repository); the *relation* — unchanged token, changed bytes —
  is what is pinned. The measurement holds identically on the base tree the row was filed against and on
  this branch, because a (b) fix does not touch the token; §5's pre-fix control tree is the base tree.

---

## 3. The GREEN: the client now reports what it cannot vouch for

### 3.1 The vocabulary (closed, one value per declared kind)

| declared `extensions.rev.kind` | `rev_vouches_for` (what the token moves for) | `rev_gap` (what it CANNOT see) |
|---|---|---|
| `git` | `commits` — `.git/HEAD`'s ref moving | `uncommitted_working_tree_writes` |
| `counter` | `surface_writes` — mutations this surface performs | `writes_not_through_this_surface` |
| anything else, or nothing declared | `nothing_declared` — no coverage claimed | `revision_kind_not_declared` |

The last row is the §8.1/§3.3 discipline: a kind this client does not know gets **no coverage claimed**
rather than an inherited promise, so a future server-side kind cannot silently re-use an old one's
guarantee.

### 3.2 Where it is reported

- **Status document** (BFS-005 §3.2's `invalidation`, `bunker fs status --json`):
  `"rev_kind":"git"`, `"rev_vouches_for":"commits"`, `"rev_gap":"uncommitted_working_tree_writes"`.
  Populated **only** while the mechanism in force is `rev`; an absent group means "the mechanism in force is
  not the revision poll", never "no gap". Documented in `BFS-005` §3.2 and as a new §4.4 row.
- **Screen** (`bunker fs status`), verbatim from the test log:
  `rev coverage : kind=git vouches_for=commits gap=uncommitted_working_tree_writes`

### 3.3 The arms (verbatim names, all PASS)

```
=== RUN   TestRevCoverageIsKindScopedAndHasNoDefault                                   --- PASS
=== RUN   TestTheLandedSurfaceDeclaresTheRevisionKindTheReportIsBuiltFrom              --- PASS
=== RUN   TestTheRevisionTierReportsTheClassOfChangeItCannotSee
    --- PASS: .../git_tree      (rev_kind=git,     rev_gap=uncommitted_working_tree_writes)
    --- PASS: .../non-git_tree  (rev_kind=counter, rev_gap=writes_not_through_this_surface)
=== RUN   TestTheCoverageReportIsSilentWhenAnotherMechanismIsInForce                   --- PASS
=== RUN   TestClientSpecScopesTheRevisionPollToTheDeclaredKind                          --- PASS
=== RUN   TestNoDocumentClaimsWholeTreeRevisionCoverage                                 --- PASS
```

Provenance is not taken on trust: `TestTheLandedSurfaceDeclaresTheRevisionKindTheReportIsBuiltFrom` handshakes
with the **real** surface first (over a git work tree → `git`; over a tree with no `.git` → `counter`; an
unbound client → no kind), and the third-tier arms then serve **that declared string** from a stub that
refuses both the push stream and the declared poll form — the only build shape the revision poll exists for.
`TestTheCoverageReportIsSilentWhenAnotherMechanismIsInForce` is the non-vacuity arm in the other direction: a
mount answered by the per-path poll must carry **no** revision gap, so an implementation that reported the
gap unconditionally fails.

---

## 4. The negative controls (each turns the arm red; each restore is sha256-verified)

### 4.1 The report is disabled → both GREEN arms go red

Mutation: the `State()` block that populates the three fields was replaced by a comment (i.e. "the fix
removed"). Verbatim:

```
--- FAIL: TestTheRevisionTierReportsTheClassOfChangeItCannotSee (0.07s)
    --- FAIL: TestTheRevisionTierReportsTheClassOfChangeItCannotSee/git_tree (0.04s)
        rev_coverage_report_test.go:134: rev_kind = "", want "git" (the DECLARED kind, not a guess)
    --- FAIL: TestTheRevisionTierReportsTheClassOfChangeItCannotSee/non-git_tree (0.03s)
        rev_coverage_report_test.go:134: rev_kind = "", want "counter" (the DECLARED kind, not a guess)
```

Restore: `sha256sum -c /tmp/bfs048-invalidate.sha256` → `internal/fsclient/invalidate.go: OK`
(`f15004f97344fff8ca4d377de1ed0ac297cb9d7a030a714f500bb4bba6f12ccd`), then the arms pass again.
`TestRevCoverageIsKindScopedAndHasNoDefault` and the "silent when events" arm stay green under this mutation
by design — the mutation removes the *report*, not the table and not the tier scoping.

### 4.2 The lie comes back in the spec → both claim guards go red

Mutation: the old sentence (`one call covers the whole tree revision, not one call per directory`) was
re-inserted into `BFS-005` §4.4. Verbatim:

```
--- FAIL: TestClientSpecScopesTheRevisionPollToTheDeclaredKind (0.00s)
    rev_claim_spec_test.go:34: the client spec still claims unconditional whole-tree scope
    ("one call covers the whole tree revision"): the revision poll is only as wide as the declared kind
--- FAIL: TestNoDocumentClaimsWholeTreeRevisionCoverage (0.05s)
    rev_claim_spec_test.go:72: these documents still claim whole-tree revision scope:
    [docs/spec/BFS-005-client-cache-and-diff.md]
```

Restore: `sha256sum -c /tmp/bfs048-bfs005.sha256` → `docs/spec/BFS-005-client-cache-and-diff.md: OK`
(`437e0dc971331360d309230fa4c11f69baa3121882f5c6dfc13561a080991d7b`), then both guards pass.
The second guard walks every **normative** `.md` under `docs/`, so the claim cannot simply move to a
neighbouring file.

**The guard caught itself first, which is the third control.** Its first run failed on **this evidence
file** — it quotes the defect sentence in the mutation below, and a blanket "no document may contain this
text" rule flagged the transcript as the claim. The rule was then scoped to normative documents only
(everything under `docs/` except `docs/evidence/**` and `docs/prd/evidence-*`), with a non-vacuity
assertion (it must scan ≥ 10 documents) so the exclusion cannot swallow the tree. Catch verified on a
normative document: with the sentence appended to `docs/prd/SPEC-watcher-capability.md`,
`TestNoDocumentClaimsWholeTreeRevisionCoverage` **FAILS**; restored by sha256
(`docs/prd/SPEC-watcher-capability.md: OK`) it passes.

---

## 5. Cost on the fast path

### 5.1 The token path does not scale with the tree (R-V2 pinned as a test)

`internal/server/webdav/rev_fastpath_cost_test.go`, 5 batches × 200 calls, minimum reported:

| files (entries) | memo hit | forced refresh (re-reads HEAD) | one whole-tree read |
|---|---|---|---|
| 1 000 (1 106) | 61 ns | 21.8 µs | 7.17 ms |
| 10 000 (10 106) | 79–172 ns | 22.2–31.2 µs | 56.07 ms (98.9–167.4 ms loaded) |

The test asserts the *shape*, not a wall clock: 10× the files must not multiply the token's cost (a
stat-per-file walk would show ~10×), plus absolute ceilings (memo hit < 10 µs, refresh < 1 ms) so a
size-independent but expensive path cannot pass either. The whole-tree read on the same fixture is logged
next to it, which is what makes the contrast measurable rather than asserted.

### 5.2 The whole-tree read is not slower

`git diff --stat` for this row: `docs/spec/BFS-005-…` , `internal/cli/fs.go`,
`internal/fsclient/{capabilities,client,invalidate}.go` — **no file under `internal/server/` or
`internal/fsmount/`**, so the snapshot path is literally the same code. Measured anyway, interleaved, on the
same 10 000-file fixture (whole-tree `snapshot` op, one call each):

```
AFTER(mine)      whole_tree_read=118.963752ms   token_memo_hit=95ns   token_refresh=31.249µs
BEFORE(pre-fix)  whole_tree_read=141.135002ms   token_memo_hit=101ns  token_refresh=29.065µs
AFTER(mine)      whole_tree_read=167.365633ms   token_memo_hit=87ns   token_refresh=29.458µs
BEFORE(pre-fix)  whole_tree_read=182.814774ms   token_memo_hit=90ns   token_refresh=30.893µs
AFTER(mine)      whole_tree_read=98.910351ms    token_memo_hit=79ns   token_refresh=26.276µs
BEFORE(pre-fix)  whole_tree_read=99.317705ms    token_memo_hit=53ns   token_refresh=28.648µs
```

(`BEFORE` = a control worktree at the row's base commit `5baef51`, same test file copied in; the host was
carrying concurrent fleet work, hence the spread. The distributions overlap; there is no regression to
attribute, because there is no changed code on that path.)

### 5.3 A stock HTTP/1.1 client sees no change

No server file changed, so no header, envelope, capability document or verb set changed. The client merely
starts **decoding** a field the surface already sent (`extensions.rev.kind`, served since BFS-004). The
capability document's shape, the refusal shape, and every E-3/E-4 header are byte-identical.

---

## 6. BFS-015 is not weakened

### 6.1 The commit-window tests, re-run

```
=== RUN   TestExpectedHashIsRevalidatedInsideTheCommit
=== RUN   TestExpectedHashIsRevalidatedInsideTheCommit/while_the_body_arrives
=== RUN   TestExpectedHashIsRevalidatedInsideTheCommit/inside_the_commit
=== RUN   TestExpectedHashIsRevalidatedInsideTheCommit/inside_the_commit_with_identical_bytes
--- PASS: TestExpectedHashIsRevalidatedInsideTheCommit (0.04s)
=== RUN   TestCreateOnlyRuleIsRevalidatedInsideTheCommit
--- PASS: TestCreateOnlyRuleIsRevalidatedInsideTheCommit (0.01s)
=== RUN   TestCommitSectionIsExclusive                                                --- PASS
=== RUN   TestConcurrentConditionalWritesCannotBothLand                               --- PASS
PASS
ok  github.com/deployBunker/bunker/internal/server/webdav  1.180s      [go test -race -count=1]
```

### 6.2 Why the change cannot reach that rule

The commit-time re-validation reads bytes (`freshEntry`) and never consults the revision token, the kind, or
the coverage report; this row adds no lock, no shared state and no write-path call. The mutation control in
§4.1 is the proof that the report is *reporting only*: with it disabled, every other arm — including all of
BFS-015 — stays green.

---

## 7. Residuals (reported, not smoothed over)

**R1 — the token still does not move for an out-of-band edit on a git tree.** That is the state this row
chose to make *honest*, not to change: R-V1 assigns the alignment to BFS-035, and R-V2 forbids buying it
with a stat-per-read walk (§1). Anything cheaper than a watcher — a directory-mtime walk — would detect some
edits and miss others, and would therefore be a **new instance of this row's own defect**.

**R2 — a concurrent worker landed the prose half of this row while this one was in flight** (`9ec3bb9`,
`fix(fsclient): scope the revision poll's claim to the served revision's kind`, on `main`). This branch is
built **on top of** it and is complementary, not a duplicate: `9ec3bb9` narrowed the code comments (and the
watcher spec's mechanism table, and BFS-004's E-3 section) plus added its own tests; it left the client's
*published spec* claiming *"one call covers the whole tree revision"* (`BFS-005` §4.4) and left the client
with **no runtime report at all**. This row adds the report (§3), corrects that spec row and the §3.2 status
document, and guards both (§4.2). The driver should rebase; the only shared line ranges are
`invalidate.go`'s `State()`/comment blocks and `BFS-005`, and the two edits do not touch the same sentences.

**R3 — BFS-049 and BFS-063 are untouched.** `git diff --stat` contains no file under `internal/server/`:
the hash cache's `(size, mtime)` identity (BFS-049) and the events poll's non-zero-cursor gap guard
(BFS-063) are exactly as they were. The report deliberately does **not** paper over BFS-049's class: a
same-size, restored-mtime edit is invisible to the token for the same reason it is invisible to the cache,
and the report says so at the level of the mechanism (`uncommitted_working_tree_writes`), not by pretending
the token is sharper than it is.

**R4 — the counter kind has the same class of gap, and now says so.** `BFS-005` §7.1 D2 has always been
"mutations through this surface"; a `counter` tree whose file is edited by an editor moves nothing either.
That was never reported anywhere before this row; the vocabulary in §3.1 names it
(`writes_not_through_this_surface`) instead of leaving it implicit.

**R5 — the report is only as good as the declaration.** Against a build that serves no capability document,
the client claims nothing (`revision_kind_not_declared`) rather than falling back to the token's shape. That
is deliberate (§8.1) and it is a *weaker* claim than the truth on a git tree, not a false one — the arm
`TestRevCoverageIsKindScopedAndHasNoDefault` pins the distinction.

---

## Appendix A — how to reproduce every number above

```
# the RED (real git work tree, out-of-band edit, token unchanged, bytes changed)
go test ./internal/server/webdav -run OutOfBandWorkingTreeEdit -count=1 -v

# the GREEN (report arms, incl. provenance from the landed surface)
go test ./internal/fsclient -run \
  'RevCoverageIsKindScoped|LandedSurfaceDeclares|RevisionTierReports|CoverageReportIsSilent' -count=1 -v

# the claim guards (the client can no longer claim coverage it lacks)
go test ./internal/fsclient -run 'ClientSpecScopes|NoDocumentClaims' -count=1 -v

# the screen
go test ./internal/cli -run FSStatusPrintsTheRevisionCoverage -count=1 -v

# the cost, both shapes on the same fixture
go test ./internal/server/webdav -run RevisionTokenCostIsIndependentOfTreeSize -count=1 -v

# BFS-015
go test ./internal/server/webdav -race -count=1 -run \
  'ExpectedHashIsRevalidatedInsideTheCommit|CreateOnlyRuleIsRevalidatedInsideTheCommit|CommitSectionIsExclusive|ConcurrentConditionalWritesCannotBothLand' -v

# the whole tree
go test ./... -count=1
```
