# INT-CI-52 + INT-CI-53 verify-and-close (tick #572, 2026-10-06)

Foreman-run verify-and-close, zero new code. Both rows are stale P1 CI-reds from
2026-09-30 / 2026-10-01 whose failure conditions no longer reproduce.

## INT-CI-52 — root-suite 1590s hang + linger "Connection timed out" (09-30)

Row claim: TestSpawn_SessionProbeFailureNeverReportsReady hung 1590s in
installRootlessDocker (internal/agent/rootless.go:82), and regression spawn failed
`enable linger for bunker-776af320: exit status 1 (Connection timed out)` —
self-hosted runner systemd user-manager unhealthy. Runner hygiene, not code.

Evidence this tick:

1. Latest completed push run on main, job level (run 37510912113, sha 969189c,
   2026-10-06T18:23Z): `gh run view 37510912113 --json jobs` → build-and-test,
   regression, unit-tests, root-suite, gitreins-guard ALL success — root-suite
   (which runs TestSpawn_SessionProbeFailureNeverReportsReady as root) is green
   on the current tree.
2. Local tree fef6674: `go test ./internal/agent/ -run
   'TestSpawn_SessionProbeFailureNeverReportsReady|TestConcurrency_SpawnFiveAgents'
   -count=1 -v` → both compile and run to completion in 0.009s (SKIP without the
   root build tag locally; no hang). The root run happens in CI and is green (1).
3. Failure-window census: `gh run list --limit 40` → exactly ONE concluded-failure
   run in 2026-10-02..06 (37217377973, regression — a separate SSH transport blip,
   filed as its own row). Zero runs since 09-30 carry the
   `Connection timed out` linger fingerprint. The runner's systemd user-manager
   recovered (multiple full root-suite passes across Oct 02-06).

Verdict: verified-and-closed, no code. worker_status: none-verified-and-closed.

## INT-CI-53 — build-and-test red 2026-10-01, run 36842601871

Job-level via `gh run view 36842601871 --json jobs`: ONLY `build-and-test`
failed (step: Version authority check); guard/root-suite/regression/unit-tests
SKIPPED (needs-gating).

Step log (gh api .../jobs/110304986533/logs), verbatim:

    latest tag: v0.1.4 | bunker version: 0.2.0 | CHANGELOG: 0.2.0
    ##[error]Process completed with exit code 1.

Mechanism: the version-authority gate ran on a sha where the v0.2.0 tag did not
exist yet — the release-cut sequencing class from the tick #390 anchor
(v0.1.4 "Version authority check" failure healed 2 minutes later by the tag;
"sequencing artifact, no board task"). The tag v0.2.0 was cut on the release
commit; current tree verifies `git describe --tags --abbrev=0` = v0.2.0,
`grep -m1 -E '^## [0-9]' CHANGELOG.md` = 0.2.0. The gate is green on every
subsequent run (37510912113 et al.).

Verdict: verified-and-closed, no code — sequencing artifact superseded by the
landed v0.2.0 tag. worker_status: none-verified-and-closed.

## New finding filed this tick (Oct-4 regression red attribution)

Run 37217377973 (88c39ea, board annotate commit, 2026-10-04T16:36Z): run-level
failure, ONLY `regression` / `E2E battery` red (continue-on-error run-level mask).
Step log: nested suite PASS=37/FAIL=0, then section isolation/share cells each
failing `kex_exchange_identification: read: Connection reset by peer` on ssh to
localhost within one 9-second window (17:02:23-17:02:32), 7 FAILs total,
VERIFY-FAIL. All 7 failures share ONE transport fingerprint — the same class as
pending QA-BUNKER-46 (SSH transport to all 5 bunker servers failing in one hour),
not a code defect. Next completed runs green. Filed as QA-BUNKER-69 with the
verbatim fingerprint and PASS criteria.

ch:trace row=INT-CI-52 row=INT-CI-53 row=QA-BUNKER-69 spec=.coding-hermes/board/tasks.jsonl evidence=docs/dogfood/2026-10-06-intci52-53-verify-close.md witness=live:gh-run-37510912113 verdict=INT-CI-52 commit=<board-commit>
