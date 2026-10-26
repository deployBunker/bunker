DF-BUNKER-75 live-gate evidence — 2026-10-26

Row: destroy times out (client deadline_exceeded) when the agent home is
large; default archive-before-delete policy blocked the RPC.

PREMISE: the fix ALREADY LANDED under sibling rows and was never linked
back to this row id:
  1217c7c9 fix(destroy): size-aware deadline, partial-archive cleanup,
           docker data-root exclusion, --archive flag. Addresses DF-BUNKER-81.
  1e2baa53 fix(cli): destroy size probe polls past a cold disk-usage cache
           before falling back to the deadline floor (PERF-013)
  8a17be23 fix: stop the cgroup test poisoning the suite; give destroy a
           real archive budget. Addresses INT-CI-050.
  5ab32dd5 fix(destroy): run compensating execs on detached budgeted
           contexts (BNK-DF-001)
Both the three brief-named test files and internal/config/archive_budget.go
exist at origin/main tip (5ad6f508). This worktree is a zero-diff closeout.

BRIEF CONSTRAINT CHECK (all pass, no code change needed):
- destroy RPC signature unchanged (proto/bunker/v1 untouched).
- archive-before-delete still the default; --purge / --archive=false is
  the opt-out.
- probe-based poll: destroyHomeSizePollWindow = 10s <= 30s cap
  (internal/cli/destroy.go).
- client deadline = daemon archive budget + DestroyRequestMargin (2min)
  >= RPC deadline + 5s buffer (internal/config/archive_budget.go).

ACCEPTANCE
1) Named tests (go test ./internal/agent/ ./internal/cli/ -run
   'TestDestroyDeadlineForHomeSize|TestDestroyHomeSizePollNeverExceedsProbeTimeout|TestDestroyCommandDeadlineBoundsTheRPC')
   rc=0, 3/3 PASS (all three live in internal/cli/; agent has no test
   matching the run pattern). Fresh capture: /tmp/df75-named-tests.log
2) go build ./... rc=0; go vet ./... rc=0;
   go test ./... -count=1 -short rc=0 (solo run, SUITE_RC=0,
   /tmp/df75-suite-final.log).
3) Commit message carries "DF-BUNKER-75" and "ch:trace:row=DF-BUNKER-75".

LIVE LEG (row acceptance: ~2.6GB home, bunker-mvp, exit 0, no
deadline_exceeded, id gone from bunker list immediately after):
  bunker spawn df75-live --ttl 1h --server bunker-mvp
  dd 2.3GB urandom into ~/go/pkg/cache.bin -> du -sh $HOME = 2.4G
  bunker destroy df75-live --server bunker-mvp:
    "Destroying agent df75-live (home 2.4 GB; archiving before delete,
     deadline 14m12s)…"
    "Agent df75-live destroyed."
    "Removed local SSH key"
    real 1m53s, rc=0, NO deadline_exceeded
  bunker list --server bunker-mvp immediately after: 3 agents
  (bca55043, crier-lab, e37ccd4d) — df75-live ABSENT.

Deployed daemon: bunker-mvp status shows Version 0.2.1; fix commits
(09-29) predate its build date (2026-10-04), so live ran the fixed code.
