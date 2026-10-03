# INT-CI-058 — the go_tests lane budget vs go test's per-binary alarm

Date: 2026-10-03. Row: INT-CI-058 (board). Prior instances of the same class:
INT-CI-006 (ROOT_SUITE_TIMEOUT 300s→780s), BFS-008/BFS-017 (guard lane
180s→600s→1200s). Rule applied: RAISE THE BUDGET from a measurement, never
narrow criteria.

## The defect

`.gitreins/config.yaml` declared `guards.test_timeout: 1200` — the go_tests
lane's wall budget — while the lane's invocation was the hard-coded
`go test -count=1 -short ./...` with NO `-timeout` flag. Go's per-binary
panic alarm therefore sat at its 600s default: any SINGLE package slower than
600s panicked the lane while the 1200s lane budget said it was fine. The
1200s budget was unreachable for exactly the contended runs it was sized for.

## Measurements (internal/fsclient, the row's named driver)

| date | command | wall | result |
|---|---|---|---|
| 2026-10-02 | guard's lane argv, 3 runs (row) | 371s / 601s / 164s | 601s FAILED the lane, 164s passed — same tree, same command |
| 2026-10-03 (this fix) | `go test -count=1 -short ./internal/fsclient/` | 395.192s (`ok` line), 438.71s incl. compile (`/usr/bin/time -v`) | exit 0, loadavg 26.44 (1-min) at start |

The 2026-10-03 run alone exceeded Go's alarm headroom concerns nothing: it
passed at 395s, but the 601s measurement on the SAME tree shows the package
crosses the 600s alarm under heavier contention. Four data points
(164/371/395/601) span 3.7x on one tree — the budget must be sized from the
contended tail, per BFS-017 §2.

## The fix (option (a) — configuration, not code)

gitreins 0.15.0 (pipx, `~/.local/bin/gitreins`) exposes a knob the repo's
config had believed nonexistent: `guards.test_command`. Verified in the
installed source:

- `engine/guard_manager.py:_check_go_tests` passes
  `test_command=self.config.get("guards", {}).get("test_command")` into the lane;
- `engine/guards.py:_resolve_go_test_argv` (DF-GITREINS-POC-45): a present,
  non-empty value replaces the historical argv `["go","test","-count=1","-short","./..."]`
  and runs through `/bin/sh`; the lane's output carries the provenance line
  `go_tests graded by the configured guards.test_command: <cmd>`. Empty/absent
  keeps the historical argv unchanged.

`.gitreins/config.yaml` now sets:

```
test_command: "go test -count=1 -short -timeout 1200s ./..."
```

ORDERING INVARIANT (criterion 4): the `-timeout` equals `guards.test_timeout`
(SAME number), so the per-binary alarm and the lane wall budget can no longer
contradict — go panics a package only at/above the wall the lane already
declares, and `scripts/gitreins-guard.sh` classifies BOTH timeout shapes
(lane `Tests timed out after`, go `test timed out after`) as NOT-FINISHED
(a budget refusal, exit 3), never as a test failure. The config comment binds
the two numbers together and says what happens if either is edited alone.

## Controlled proof (criterion 1) — real guard, real alarm, staged index

Scratch repos `/tmp/int-ci-058-proof2/{old,new}`, identical except the config
key; single synthetic package sleeping 610s (>600s alarm, <1200s budget);
everything STAGED (the pre-commit hook's context — a committed-then-guarded
tree grades an empty index and is vacuous, proven by the discarded v1 run);
the actual pipx `gitreins guard --json` (0.15.0) run once per variant:

- `old/` (no `test_command`): **REPRODUCED** — go_tests FAIL with go's own
  alarm: `panic: test timed out after 10m0s … TestSlowPackage (10m0s)`,
  `FAIL proof-old 600.103s`; guard exit 1, evidence `outcome: fail`,
  `degraded: false`, all four lanes ran (`skippedSteps: []`). The lane budget
  (test_timeout 1200) never got a say — the package alarm fired first.
- `new/` (`test_command` with `-timeout 1200s`): **PASSES** — go_tests PASS:
  `ok proof-new 610.090s`; guard exit 0, `outcome: pass`. The lane summary
  carries the in-band provenance line `go_tests graded by the configured
  guards.test_command: go test -count=1 -short -timeout 1200s ./...`,
  proving the knob (not a proxy) drove the invocation.

Identical trees, identical `test_timeout: 1200`; the only variable is the
config key. Raw evidence: `/tmp/int-ci-058-proof2/{old,new}/*-evidence.json`
(v1 schema, producer gitreins 0.15.0; OLD generatedAt 2026-10-03T17:44:00Z,
NEW 2026-10-03T17:54:12Z).

## Residuals

- TestBFS063's ~256-iteration rotation floor (journal bound
  `eventsJournalEvents` unexported in internal/server/webdav) is DEFERRED:
  shrinking it changes internal/fsclient test runtime, which INT-CI-059 owns;
  this row's constraint forbids touching that package's timing budgets.
- The lane honoring `-timeout` via `test_command` is per-repo configuration;
  gitreins itself still defaults the bare lane to no `-timeout`. An upstream
  change making the default argv carry `-timeout guards.test_timeout` remains
  open (option (b)) for every OTHER Go repo on the fleet.
