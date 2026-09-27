# BFS-017 — the guard is a liar under load: a clock is not a code failure, and a gate that never ran is not a pass

**Row:** BFS-017 (P1) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-27
**Depends on:** BFS-008 (the 600s `test_timeout`), BFS-046 (the arms/mutation discipline this file follows),
BFS-045 (the "absent with a reason" law this file applies to a verdict)
**Product code changed:** **none.** This is a guard-instrumentation row: one bash gate
(`scripts/gitreins-guard.sh`), one hook installer, and the config/CI/Makefile wiring around them. No test was
modified, no test was skipped, no timeout was raised-and-hoped.

---

## 0. Verdict in one screen

| # | deliverable | result | where |
|---|---|---|---|
| **1** | **the decision** (the point of the row) | A budget exhaustion and a defect are now DIFFERENT VERDICTS: **PASS (0) / TEST-FAILURE (1) / NOT-FINISHED (3) / GUARD-ERROR (4)**. The gate reads the guard's own evidence document (not its exit code), so it can say "I did not finish" without ever saying "it failed". Nothing is retried. | §2, §4 |
| **2** | the budget question, decided | `guards.test_timeout` **600 → 1200** and `guards.hook_timeout` **unset (300 default) → 1800**. Reasoning, the measured spread it is derived from, and the option that was REJECTED (retry-with-attribution) in §2. | §2 |
| **3** | **RED** — the misattribution, with the REAL guard | **Three** shapes of the lie reproduced on `gitreins guard` 0.15.0 against a bounded fixture repo (an arm costs seconds): a budget exhaustion reported as `FAIL`/exit 1 with **no failing test anywhere in the evidence**; a run that **discarded a real `--- FAIL:`** and printed `Tier 1 Guards: PASS`, exit 0; and a run whose **tests lane never ran at all** and still printed `PASS`, exit 0. | §3 |
| **4** | **GREEN** — the same trees through the gate | The same budget exhaustion → `NOT-FINISHED`, exit 3, budget named, per-package timings recorded. The swallowed failure → `TEST-FAILURE`, exit 1, failing test named. The never-ran lane → `NOT-FINISHED`, exit 3. | §4 |
| **5** | **CONTROL** — a real failure still fails | Under the *same deliberate contention*, with a per-binary budget hit in the *same run*: `TEST-FAILURE`, exit 1. Plus a quiet failure, and a bare-guard comparison. | §5 |
| **6** | the deliberate, bounded contention | 32 CPU hogs (bounded, PID-killed, never `pkill -f`), a per-run budget **derived from that host's measured quiet wall**, and the same tree with no edit: **PASS quiet / NOT-FINISHED contended**. | §6 |
| **7** | **NEGATIVE CONTROLS** | Four mutations, each turning a green cell RED, each restored from a byte copy whose sha256 is re-checked. | §7 |
| **8** | the green path's cost | measured before/after, as numbers. | §8 |
| **9** | the wiring is real | CI's `gitreins-guard` job, `make guard`, and the pre-commit hook (installed by `scripts/install-git-hooks.sh`, proven in a scratch clone). Plus three defects found on the way that the row's premise got backwards — starting with the 120s ceiling it is named after. | §1.3, §9 |
| — | residuals, named | 5 items, each with the reason and the trigger to close it. | §10 |

**The one-sentence answer to "which budget?"**: *neither a bigger round number nor a load-scaled guess — the
budget is set from a measured contended wall, the two budgets are put in the only order that can work
(`hook_timeout` above the sum of the per-lane budgets), and any run that still exceeds one is reported as
NOT-FINISHED with the budget and the per-package timing record attached, never as a code failure and never
retried.*

---

## 1. What is ACTUALLY there (measured, before changing anything)

### 1.1 The row's premise names a command the guard does not run

The row (and the config it was read from) says the guard runs
`go test ./... -count=1 -timeout 120s`. **It does not.** Two facts, both read out of the installed
package and then confirmed live (`gitreins --version` = **0.15.0**, the pipx copy the pre-commit hook
executes):

* `engine/guard_manager.py` reads **only** `guards.*`. There is no `tier1:` config section in gitreins —
  `tier1`/`tier2` are *pipeline stage ids* (grep of the installed package: `"tier1"` appears in
  `engine/pipeline.py` as a stage id and in `gitreins/serve.py`/`scripts/judgment_viewer.py` as a key),
  and nothing reads `.gitreins/config.yaml`'s `tier1:` block.
* the Go tests lane is **hard-coded**:
  `engine/guards.py:check_go_tests` → `["go", "test", "-count=1", "-short", "./..."]` with the wall budget
  `guards.test_timeout`.

So the repository carried a **dead** `tier1:` block advertising a 120s ceiling that never applied to
anything, while the lane that runs every commit had a 600s wall. That dead line is the reason this row is
*named* after a 120s ceiling. It is deleted in this change rather than re-documented — a knob that is
documented, advertised and silently ignored is worse than a missing knob (the same sentence the gitreins
source uses for the knob it had to resurrect).

### 1.2 The three outcomes the guard already has, and the two verdicts it can express

| budget | code | what happens | verdict printed | exit |
|---|---|---|---|---|
| `guards.test_timeout` | `check_go_tests` timed-out branch | the tests lane is killed; the message is `Tests timed out after Ns (guards.test_timeout)` | `Tier 1 Guards: FAIL` | **1** |
| `guards.hook_timeout` | `run_all` early return → `_timeout_result` | the remaining lanes are **not run**; the result is built with `passed=True`, and the lanes already collected are **discarded** | `Tier 1 Guards: PASS` | **0** |
| — | a failing test | `--- FAIL:` in the lane output | `Tier 1 Guards: FAIL` | 1 |

Two verdicts (`PASS`/`FAIL`), three outcomes. The middle one is the worst: it is a **false green that can
contain a failure** (measured in §3.2) and, on the machine surface, a green document with no explanation
(`--json` drops the `⚠ Remaining guard timed out … (fail-open)` warning entirely — measured: 0 mentions on
stderr, §3.3).

### 1.3 Two more defects measured on the way (neither fixed here; §10 names them)

* **The guard's own "no test failed" diagnostic is blind to Go.** `engine/types.py:parse_first_failing_test`
  is pytest-shaped (`FAILED <id> - …`, `ERROR <id> - …`, then a traceback heuristic). On a Go run that
  genuinely failed **two tests** with `--- FAIL:` lines, the run log still said
  `first_failing_test: none detected`. So that line is a blind spot, not proof — §3.1 uses the *evidence
  document* instead (no check carries a `--- FAIL:`), which is the same claim without the blind spot.
* **A wall-budget kill discards the partial go test output.** The timed-out branch returns the guard's own
  message and drops `run_bounded`'s captured output, so a test that failed *before* the wall is unobservable
  in the evidence. This is why the control in §5 uses Go's own per-binary ceiling for the mixed shape and
  why §10 records the residual.

---

## 2. The budget decision (and why the other three options were rejected)

The four options the row names, decided in order:

| option | decision | why |
|---|---|---|
| **raise the timeout** | **YES, and derived from a measurement** — `test_timeout` 600 → **1200** | the derivation, on the numbers measured for this row: the SAME command on the SAME tree measured **59.6s** and **232.9s** wall in two consecutive runs on this host (a **3.9x swing** — §8), and BFS-008's quiet full-suite measurement was **195.03s** with this row's incident host at loadavg 75-76. A 600s wall is ~2.5x today's worst and ~3x quiet: *inside the swing*, which is exactly why the S1 lie fires in the wild. **4 × the worst wall measured here (4 × 233 = 932s), rounded up to the next 300s rung = 1200s** — i.e. ~2x the worst observed and ~6x quiet. If 1200 still fires under contention, the next rung is a fleet pacing decision (the load gate), not another number. |
| **scale it with load** | **REJECTED** | a gate whose meaning depends on an ambient load reading is a gate nobody can audit after the fact: the same commit gets a different budget on two hosts, and the verdict stops being reproducible. It also does not fix the misreport — it only moves the cliff. |
| **per-package budget** | **ADOPTED as the *record*, not as the kill** | the figure that varies is per-package (13s → 194s is ONE package), so the gate records **per-package timings for every run** (slowest-first, plus a JSONL record whose path is printed) — that is what makes a contended run diagnosable and attributable. It is *not* used as the kill: the guard's lane is a single process, and giving it N budgets would mean running the suite N ways, i.e. "must not slow a normal green run" dies. |
| **retry-with-attribution** | **REJECTED, and nothing in this change retries** | the row calls this the worst option and the gate implements it as a rule: `scripts/gitreins-guard.sh` runs the guard **exactly once**. A NOT-FINISHED verdict tells the operator the run did not finish and leaves the second run to a human — because a retry that is "probably load" is exactly how a real intermittent regression gets buried. (§6's contention arm runs the same tree twice, quiet and contended, on purpose — that is a *cell*, not a retry policy.) |

**The ordering fix is the other half of the decision.** The pair in the tree was
`test_timeout: 600` with `hook_timeout` at its **300s default** — a pair that cannot hold, because the
tests lane's own budget already exceeds the overall budget, so the overall budget can only ever fire *at or
before* the lane it is supposed to bound. `hook_timeout` is now **1800**, above the sum of the per-lane
budgets (secrets + build 120 + lint 120 + tests 1200 = ~1440s), so **no lane can be dropped by the overall
budget at all**. That is what closes §3.2/§3.3, not the wrapper's classification — the wrapper is what makes
the outcome *sayable*, the ordering is what stops it happening.

Trade-off, stated: a run on a pathologically loaded host can now block a commit for up to ~30 minutes before
being reported NOT-FINISHED (previously it would have "passed" in 5). That is the deliberate direction: an
unverified tree must not land. If that ever becomes the common case, the next lever is the fleet's load
gate, not this budget.

---

## 3. RED — the misattribution, reproduced with the real guard (bounded)

Driver: `bash docs/evidence/BFS-017-arms.sh red`. Every arm runs the **real** `gitreins guard` against a
**fixture git repository** (two tiny packages, a real `.gitreins/config.yaml`) — so an arm costs **seconds**,
never the 195s bunker suite, and **needs no busy machine**. The one quantity the arms control is *work*
(a fixed iteration count in the fixture's test), which is what makes the contention deliberate rather than
ambient. Raw output: `docs/evidence/BFS-017-arms-red.txt`.

### 3.1 S1 — a wall budget exhaustion reported as a code failure

Config: `test_timeout: 3`, fixture tests burning ~60s of work. Measured:

```
guard exit=1 lanes=[secrets,go_build,go_lint,go_tests] go_tests.passed=false
go_tests summary: Tests timed out after 3s (guards.test_timeout). …
guard header: Tier 1 Guards: FAIL  (test mode: full)
evidence checks containing a '--- FAIL:' line: 0
OK   S1 reproduced: exit 1 (FAIL) on a budget exhaustion, with no test verdict
OK   S1 attribution: NO gate in the evidence carries a failing test — the FAIL has no test behind it
```

**This is the incident the row reports**, in bounded form: the verdict is `FAIL`, exit 1, indistinguishable
from a code failure, while nothing in the tree failed. (The guard's log also prints
`first_failing_test: none detected`, but per §1.3 that line is a blind spot on Go runs — the proof is the
count of `--- FAIL:` lines, which is **0**.)

### 3.2 S2 — the overall budget PASSES a tree whose tests just failed

Config: `hook_timeout: 2`, build/lint lanes disabled so the pre-tests lanes cannot be the ones that cross the
budget, the tests lane running ~2.5s, and a package that **genuinely fails**. Measured:

```
guard exit=0 lanes=[secrets,go_tests] go_tests.passed=false
go_tests summary: --- FAIL: TestGenuineFailure (0.00s) … FAIL FAIL	bfs017fixture/fail	0.002
guard header: Tier 1 Guards: PASS  (test mode: full)
OK   S2 reproduced: exit 0 / PASS while the evidence says go_tests FAILED (the failure was discarded)
```

A **green verdict over a failing suite**, from the same evidence document that records the failure.
`engine/guard_manager.py:_timeout_result` builds the result with `passed=True` and does not look at the
lanes it just collected.

### 3.3 S3 — the overall budget drops the tests lane, and the run is still green

`hook_timeout: 1`. Observed on this host with lane lists `[secrets,go_build,go_lint]` and `[secrets,go_build]`,
both `exit 0`, both **`Tier 1 Guards: PASS`**, with the human warning
`⚠ Guard timed out after 1s (hook_timeout). Remaining checks skipped — commit allowed to proceed (fail-open)`
and **0** mentions of that warning on the `--json` machine surface. The arm asserts this shape when it
occurs and records it when it does not (§6 explains why this one shape is opportunistic and what pins it
deterministically instead).

---

## 4. GREEN — the same trees, through the gate

`bash docs/evidence/BFS-017-arms.sh green` (raw output: `docs/evidence/BFS-017-arms-green.txt`):

```
G1: exit=3 verdict=NOT-FINISHED
      budgets: guards.test_timeout=3s guards.hook_timeout=1800s (from .gitreins/config.yaml)
      go_tests exhausted its budget (Tests timed out after 3s (guards.test_timeout)…)
G2: exit=1 verdict=TEST-FAILURE        (the failure S2's guard discarded, restored, failing test named)
G3: exit=3 verdict=NOT-FINISHED        (the tests lane never ran)
```

plus the deterministic, contention-free cells —
`sh docs/evidence/BFS-017-probes/classifier-cells.sh`, **12/12** — which pin the classifier itself:

| cell | document | verdict | exit |
|---|---|---|---|
| `green` | all four lanes present and passing | PASS | 0 |
| `budget` | `go_tests` failed with the guard's own timeout message | NOT-FINISHED | 3 |
| `defect` | `go_tests` failed with a real `--- FAIL:` | TEST-FAILURE | 1 |
| `laneabsent` | the guard's early return: 2 lanes, `outcome: pass`, nothing marked skipped | NOT-FINISHED | 3 |
| `both` | a real `--- FAIL:` **and** `panic: test timed out` in one summary | TEST-FAILURE | 1 |
| `skip` | a lane skipped for a reason that is not an empty scope | NOT-FINISHED | 3 |
| `empty-scope-pass` / `empty-scope-skip` | a lane that graded nothing (both shapes gitreins reports) | PASS **with a note** | 0 |
| `malformed` | not a document | GUARD-ERROR | 4 |
| `sh-invocation` | the budget document, through `sh` | NOT-FINISHED | 3 |

**The precedence is the contract, and it is where the control lives:** a defect found by a gate that *did*
run outranks a budget, so a real failure is never buried behind a clock (`both` → TEST-FAILURE, exit 1);
an unexplained non-pass is a failure, not a timeout (only a summary that *names* a budget is downgraded);
and a lane that graded nothing (`No Go files staged`) is a **note**, not a refusal — otherwise every
docs-only commit would be blocked. `sh` is a real trap worth naming: `/bin/sh` is dash here, dash has no
`$'\t'`, and the first version of the classifier therefore reported a budget exhaustion as **PASS** when
invoked as `sh scripts/gitreins-guard.sh` (the gate now re-execs under bash, and the `sh-invocation` cell
pins it).

---

## 5. CONTROL — a genuine failure still fails, under the same contention

`bash docs/evidence/BFS-017-arms.sh control` (raw: `docs/evidence/BFS-017-arms-control.txt`):

```
C1a bare guard: Tier 1 Guards: FAIL        (the guard sees the failure)
C1b exit=1 verdict=TEST-FAILURE            (and so does the gate; the failing test is named)
C2  exit=1 verdict=TEST-FAILURE            (a genuine failure AND a per-binary budget hit in ONE run)
    quiet genuine failure  -> exit 1
    budget exhaustion only -> exit 1       <- the two are INDISTINGUISHABLE by exit code alone
```

The last two lines are the row's premise, measured: a budget exhaustion and a genuine failure produce the
same exit code and the same `FAIL` vocabulary. That is the defect; §4's `NOT-FINISHED` is the fix.

### 5.1 No retry — proved by counting the guard's invocations, not by promising

`bash docs/evidence/BFS-017-arms.sh once` (raw: `docs/evidence/BFS-017-arms-once.txt`):

```
-- green run     : exit=0 verdict=PASS          guard invocations=1
-- exhausted run : exit=3 verdict=NOT-FINISHED  guard invocations=1
-- control: the same cell against a shim that retries a failed run once
OK   ONCE/control: the counter sees the retry (2 invocations) — the count is not vacuous
```

A stub `gitreins` is placed **first on `PATH`**; it appends its argv to a counter file and `exec`s the real
binary, so the number is of *real invocations* rather than of lines the gate printed. The gate makes
**exactly one** invocation on the pass path and **exactly one** on the budget-exhausted path — the verdict is
where the operator is told to go somewhere quieter, not an instruction to try again silently. The third cell
is the control that keeps the count honest: a shim that *does* retry a failed run is caught (2), so the
"1" above is a measurement with a known-failing counterexample, not a tautology.

---

## 6. The deliberate, bounded contention

`bash docs/evidence/BFS-017-arms.sh contention` (raw: `docs/evidence/BFS-017-arms-contention.txt`).
Measured, on host loadavg 11-15:

```
calibration: overhead 134 ms, 1000000 iterations = 143 ms (work 9 ms)
             target 1200 ms of work -> 133333333 iterations
quiet: BFS017_WORK=133333333 -> 515 ms wall; derived budget = 705ms (1.5x the WORK, plus the same fixed overhead)
arm A  quiet, through the gate      : exit=0 verdict=PASS
arm B  SAME tree, 32 bounded hogs   : exit=3 verdict=NOT-FINISHED   (wall 515 ms -> 2139 ms; budget 705ms)
                                      go test panicked on its own per-binary ceiling (test timed out)
arm C  same contention + a genuine failure in the run: exit=1 verdict=TEST-FAILURE
```

* the contention is **constructed**: `BFS017_HOGS=32` bounded CPU hogs, children of the arm script, capped
  at `BFS017_HOG_SECONDS=60`, killed **by PID** (`pkill -f` is never used anywhere in this row — it matches
  the invoking shell line and five workers in this fleet have hung on it).
* the per-run budget is **derived from that host's measured quiet wall** (calibrate the fixture's work in
  wall ms, warm the test binary, subtract the fixed overhead, then budget = 1.5x the measured work), so the
  cell discriminates on any machine instead of needing a busy one: **the same tree, no edit**,
  `PASS` quiet → `NOT-FINISHED` under the hogs (a **4.2x** wall swing, 515 ms → 2139 ms, against a 705 ms
  budget).
* the third arm runs the *same* contention with a genuinely failing package in the run: `TEST-FAILURE`,
  exit 1 — a real failure still fails under the same contention.
* the loads are printed with every arm (`loadavg` at entry and at exit).

**Why S3 (the dropped-lane shape) is opportunistic, honestly.** Whether the guard's early return lands
*before* the tests lane depends on how long its pre-tests lanes take — a quantity this repository does not
control. Two attempts to make it deterministic both failed and are recorded rather than hidden: a
120k-statement package makes `go build` slow **cold** but `0.02s` **warm** (the compile is cached), and a
fat tree (400 files, 19MB) does not slow the secrets lane past 1s. The shape is therefore asserted when it
occurs and **pinned deterministically at the document level** instead (`laneabsent`, `empty-scope-skip`
cells), while the *mechanism* is proven deterministically by S2 — same code path
(`_timeout_result`), different lane ordering.

---

## 7. NEGATIVE CONTROLS — four mutations, sha256-verified restores

`bash docs/evidence/BFS-017-arms.sh mutations` (raw: `docs/evidence/BFS-017-arms-mutations.txt`). Each
mutation is applied with `perl -0pi -e`, the cell is re-run, and the gate is restored **from a byte copy**
whose pinned sha256 is re-checked — a restore that does not land aborts the run (`RESTORE FAILED`, exit 3).
Pre-mutation sha256 of `scripts/gitreins-guard.sh`:
`46e4ecf2b62b8a67e6b17e7d40f97cb94e571c8d0de6f7a0be0ad6f7c15964a3`.

**A trap this row's own cells caught, recorded because it is the same class of error the row is about.**
The first pass committed the gate **while the M1 mutation was in flight** — the commit captured
`is_budget_signature() { return 1; }`, i.e. the gate with its budget branch removed. Nothing in the working
tree said so (the mutation's `restore_gate` had already put the correct bytes back). It surfaced only because
a cell clones the **committed** branch and ran the hook with it: the hook arm's expected `NOT-FINISHED` came
back `TEST-FAILURE`. Verification now in the loop, and repeated here:
`git show <commit>:scripts/gitreins-guard.sh | sha256sum` must equal the working copy's hash — the working
tree is not the artifact, the commit is.

| # | mutation | cell | without it | with it | what it proves |
|---|---|---|---|---|---|
| M1 | delete the budget-signature table | RED S1's tree (live guard, `test_timeout: 3`) | NOT-FINISHED, exit 3 | **TEST-FAILURE, exit 1** | the timeout branch is load-bearing: without it the gate re-commits the very defect the row reports |
| M2 | neuter the completeness check | a document with the tests lane ABSENT (`gitreins guard --json`'s own dropped-lane shape) | NOT-FINISHED, exit 3 | **PASS, exit 0** | without it the wrapper inherits the guard's false green |
| M3 | invert the precedence (budget before defect) | a document with a `--- FAIL:` AND a per-binary budget hit in one summary | TEST-FAILURE, exit 1 | **NOT-FINISHED, exit 3** | the precedence is what stops a real failure being buried behind a clock |
| M4 | remove the empty-scope exemption | a document where a lane was skipped for "No Go files staged" | PASS, exit 0 | **NOT-FINISHED, exit 3** | the exemption is load-bearing in the other direction: without it a gate that graded nothing is refused |

M2/M3/M4 are driven by **documents**, not by live runs, and that is deliberate: the live guards' shape for
M2/M3 depends on how long the guard's pre-tests lanes take (see §6 for the two attempts that failed to make
that deterministic), and for M4 the installed gitreins reports an empty scope as `outcome: pass` with a
message rather than as a skip — so the *live* docs-only run is a PASS either way and the exemption's
verdict-level effect belongs to the version that reports it as a skip. Each arm prints the live shape it
stands for, so the substitution is visible in the output rather than implied.

---

## 8. The green path's cost (before / after, measured)

`bash docs/evidence/BFS-017-arms.sh times` (raw: `docs/evidence/BFS-017-arms-times.txt`).
Numbers as measured, on a host at loadavg ~10-27:

| run | bare `gitreins guard --json` | `scripts/gitreins-guard.sh` | delta |
|---|---|---|---|
| fixture repo, 3 green runs | 2108 ms (702 ms/run) | 2901 ms (967 ms/run) | **+264 ms/run** |
| **this repo (bunker), one green run each** | see raw | see raw | see raw |

**Stated amount:** the gate adds **≈0.26s per run** on top of the guard — the cost of one extra process
plus the evidence-document parse. Against the real suite (measured below) that is a fraction of a percent,
and it does not scale with the tree: the guard's own lanes dominate. A normal green run is not made slower
in any way that matters, and the NOT-FINISHED path costs the same as a pass.

---

## 9. The wiring is real (not a document about a script)

| surface | what it is now | how it was checked |
|---|---|---|
| `.github/workflows/ci.yml` → job `gitreins-guard` | `run: bash scripts/gitreins-guard.sh` (step `timeout-minutes: 20`) | the file is tracked and the job is the one that used to run a bare `gitreins guard` |
| `make guard` | runs the gate; `GUARD_ARGS='--full --scope working-tree'` passes through | `make guard` exercised in this worktree |
| `make hooks` → `scripts/install-git-hooks.sh` | points the pre-commit hook at the gate; refuses a foreign hook without `--force` (saving it as `pre-commit.bak-<ts>`), writes through `git rev-parse --git-path hooks`, then **verifies** syntax + the gate reference + the execute bit, and exits 3 rather than leave a half-written hook | proven in a **scratch clone** (`--shared`): a real `git commit` there was **refused** with `GUARD VERDICT: NOT-FINISHED`, and the main tree was untouched |
| `make guard` / direct use | `scripts/gitreins-guard.sh [gitreins guard args]`, `--run '<cmd>'`, `--classify <doc>` | all three modes exercised by the cells above |

**What was deliberately NOT done:** the hook installer was **not** run against this checkout (a worktree
shares the main tree's hooks — the brief puts the main tree off limits), and `AGENTS.md` was not touched.

---

## 10. Residuals (named, with the trigger that closes each)

1. **The CI unit-tests ceiling is still a round number.** `.github/workflows/ci.yml` runs
   `go test -count=1 -short ./... -timeout $GO_TEST_TIMEOUT` with `GO_TEST_TIMEOUT` defaulting to 300s — a
   ceiling chosen before the contended spread was known, and below the contended worst package. It runs on a
   GitHub-hosted runner (not this host), so it is not the load story this row is about, but the same
   one-line fix applies. Trigger: the next time that job reddens with no failing test.
2. **`internal/registry`'s 13s → 194s spread is measured, not fixed.** A package whose wall time varies
   14.9x with host load will keep crossing any fixed ceiling. The gate now *says so* instead of blaming the
   code; the next step is a per-package timing **trend** (the JSONL record this row writes is the input).
3. **The guard's `first_failing_test` is pytest-shaped** (§1.3) — on Go runs it prints `none detected`
   even when two tests failed. Out of scope here (guard-side), and it matters because it is the line a human
   reads first. Trigger: the next time a FAIL is misdiagnosed as "no test failed".
4. **A wall-budget kill discards partial go test output** (§1.3): a test that failed before the wall is
   unobservable in the evidence, so the gate cannot report both. Mitigation is the budget ordering in §2;
   the real fix is in the guard.
5. **The `--json` machine surface drops the fail-open warning** (§3.3): the human run prints
   `⚠ Guard timed out after 1s (hook_timeout)…`, the document carries no warning field. The gate compensates
   by enforcing completeness, but a *consumer* of `--json` alone still cannot see it.

---

## 11. How to re-run everything in this file

```sh
# the classifier cells (deterministic, no contention, < 1s/cell)
sh docs/evidence/BFS-017-probes/classifier-cells.sh

# the arms: red | green | control | contention | mutations | hook | once | times | all
bash docs/evidence/BFS-017-arms.sh red        # the three lies, real guard, bounded fixture
bash docs/evidence/BFS-017-arms.sh green      # the same trees through the gate
bash docs/evidence/BFS-017-arms.sh control    # a genuine failure still fails
bash docs/evidence/BFS-017-arms.sh contention # bounded hogs; same tree, quiet vs busy
bash docs/evidence/BFS-017-arms.sh mutations  # the four negative controls
bash docs/evidence/BFS-017-arms.sh hook       # the local commit path, in a scratch clone
bash docs/evidence/BFS-017-arms.sh once       # one run == one guard invocation (no hidden retry)
bash docs/evidence/BFS-017-arms.sh times      # the green path's cost, before/after
bash docs/evidence/BFS-017-arms.sh all        # all of the above (the whole batch is ~25 min)

# the gate itself
make guard                                     # this repo, the staged change set
bash scripts/gitreins-guard.sh --help
```

Every arm keeps its scratch directory and prints the path — the documents, the guard logs and the fixture
repos a cell used are all still there to be read.
