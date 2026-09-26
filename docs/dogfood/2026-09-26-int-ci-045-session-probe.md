# INT-CI-045: root-suite SessionProbe failure classification

Status: residual runner evidence required; no production fix is claimed by this artifact.

## Observed failure

GitHub Actions run 36222825143, head `432265b35b8e72ae8159dbeda0027b87868557ab`, failed the `root-suite` job in `TestSpawn_SessionProbeSuccessReturnsReady` after 856.5 seconds. The exact log fingerprint was:

```text
spawn probeok-72180 failed at stage rootless-install:
chown runtime dir /run/user/1005: exit status 1
(output: chown: cannot access '/run/user/1005': No such file or directory)
```

This is a whole-spawn, privileged-run failure. The non-root developer run of that test is intentionally skipped by `internal/agent/session_probe_test.go`; that skip is not evidence that the root-suite failure is fixed.

## Existing in-tree mitigation

Commit `9ca1e433` (INT-CI-038) already changed `ensureInstallRuntimeDir` in `internal/agent/rootless.go` to:

1. create `/run/user/<uid>`;
2. run the same non-recursive `chown <user>: /run/user/<uid>`;
3. retry the exact ENOENT shape across the bounded `runtimeDirOwnershipAttempts` budget;
4. fail closed, with the last error and mount verdict, after exhaustion.

The corresponding deterministic proof is `TestEnsureInstallRuntimeDir_TeardownRaceConverges` in `internal/agent/rootless_runtime_chown_race_test.go`. It models the chown failing with the measured ENOENT shape on attempts 1 and 2 and succeeding on attempt 3, plus the exhaustion case. The test also asserts the bounded number of non-recursive chown calls.

## What remains unproven

The unit proof establishes the helper's behavior when the helper receives the ENOENT result. It does not prove that the real root-suite host's `user-runtime-dir@<uid>.service`, logind teardown, and rootless-install sequence converge within the three-attempt budget. Run 36222825143 therefore remains a live-host regression signal, not a proof that the existing helper is wrong and not a proof that the host is healthy.

The next live verification must:

1. run the root suite on a clean/idle bunker-mvp runner;
2. preserve the full root-suite log;
3. check whether the same `/run/user/<uid>` ENOENT occurs on all bounded attempts or whether it is emitted by a different spawn stage than `ensureInstallRuntimeDir`;
4. compare the exact attempt count and timestamps with `runtimeDirOwnershipAttempts` and `runtimeDirOwnershipPause`;
5. require `TestSpawn_SessionProbeSuccessReturnsReady` to complete without a skip and with the normal ready/registry assertions.

Until that run is captured, this artifact intentionally classifies INT-CI-045 as an unresolved runner/product boundary and does not claim a deployment or a live E2E pass.
