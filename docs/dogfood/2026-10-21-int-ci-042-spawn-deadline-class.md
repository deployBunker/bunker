# INT-CI-042 — E2E battery spawn "deadline_exceeded / re-list miss": classification + fix

- Row: `INT-CI-042` (`.coding-hermes/board/tasks.jsonl`), filed 2026-10-07 from CI runs
  `37666760535` and recurrence `37654405376`.
- Worktree: `wt/INT-CI-042` on base `85c802e6`.
- Verdict up front: **REAL DEFECT (not environmental, not covered by QA-BUNKER-58)** — plus a
  documentation correction: the two runs show TWO different signatures, and the board row's
  paraphrase only matches one of them. Both signatures share one root cause, fixed here.

## 1. What the runs actually say (fetched logs, not the row's paraphrase)

Job logs pulled with `gh api repos/deployBunker/bunker/actions/jobs/<id>/logs`:

- **Run 2 = 37654405376** (job 112906055293, regression job, 18:49–19:06Z) — the row's signature,
  verbatim at `19:00:43Z`:

  ```
  ✗ allowed spec spawn failed — Creating agent...
  Reconciling spawn: the RPC failed (deadline_exceeded: context deadline exceeded) but the
  daemon may still be creating agent "e2e-imgspec" — re-listing...
  bunker: spawn agent: deadline_exceeded: context deadline exceeded; agent e2e-imgspec was
  NOT found on re-list after 5 attempts — the spawn likely failed server-side; check
  `bunker list` before retrying to avoid a duplicate
  ```

  Timing detail that decides the classification: the previous battery section (step-13 cleanup,
  destroys at `18:55:43Z`) finished, and the deadline fired **exactly 300s later** (`19:00:43Z`)
  — the CLI's full spawn budget (`internal/cli/spawn.go:209`, `context.WithTimeout(..., 300s)`)
  was consumed by a daemon stuck somewhere before agent registration. At `19:05:44Z` the follow-up
  exec cell reports `not_found: agent "e2e-imgspec" not found`, and at `19:06:46Z` the CI
  **step timeout** kills the job: `The action 'E2E battery' has timed out after 15 minutes.`
  The GitHub orphan-process cleanup then terminates `bunkerd` mid-spawn:
  `Terminate orphan process: pid (…) (bunkerd)`.

- **Run 1 = 37666760535** (job 112948125845, 19:37–19:55Z, same runner ~30 min later) — a
  DIFFERENT signature the board row does not describe, at `19:44:01Z`:

  ```
  ✗ allowed spec spawn failed — Creating agent...
  bunker: spawn agent: internal: spawn e2e-imgspec failed at stage keygen: ssh-keygen failed:
  exit status 1 (output: Generating public/private ed25519 key pair.
  /tmp/bunker-key-e2e-imgspec already exists.
  Overwrite (y/n)? )
  ```

  The nested suite passed 37/0 in both runs; both jobs were killed by the same 15-minute step
  timeout (since raised to 25 minutes by INT-CI-065, in the base of this worktree).

## 2. Code path and root cause

- Daemon spawn Step 3 writes the SSH keypair to a **deterministic path**:
  `internal/agent/manager_spawn.go` — `keyFile = filepath.Join(os.TempDir(), "bunker-key-"+agentID)`
  (pre-fix line 424), executed by `ssh-keygen -t ed25519 -f <path> -N '' -C bunker-<id>`.
- `ssh-keygen -f` on an occupied path asks `Overwrite (y/n)?` **on the daemon's stdin**. A daemon
  has no terminal: the prompt reads EOF and exits nonzero — the spawn dies at `stage keygen`
  (run 1's 0.4s failure), or, in the run-2 variant, the daemon's spawn pipeline stalls
  pre-registration long enough to burn the caller's entire 300s RPC window.
- Cleanup for these files exists ONLY in paths a SIGKILL never reaches: the rollback closure and
  the deferred remove (`manager_spawn.go:240-242, 831`). The CI step timeout does not stop the
  daemon — the runner's orphan cleanup **kills** it, so the pair at
  `/tmp/bunker-key-e2e-imgspec` (and `.pub`) survives.
- Causal chain across the two runs (same persistent self-hosted runner, `BATTERY_EXPECTED_HOST=bunker-mvp`):
  run 2's killed mid-spawn daemon seeded the stale file; run 1's spawn of the SAME battery agent
  id tripped over it 30 minutes later. The shared agent id `e2e-imgspec` is what makes the
  residue from one run lethal to the next.

## 3. Why this is a real defect and not covered by prior fixes

- **QA-BUNKER-58 (eb62789b, 2026-10-06, one day before these runs)** fixed the *reporting*
  semantics: ambiguous spawn failures now run the bounded re-list probe. The probe worked exactly
  as designed in run 2 — and reported honestly that the agent never appeared, because the spawn
  genuinely never completed. The probe re-lists; it cannot unstick a daemon. Different layer, no
  overlap.
- **In-repo deadline raise 30s→300s (38029292, Aug)** — covers slow rootless installs, not an
  interactive prompt that never answers and not a SIGKILL leak.
- **INT-CI-069 (6ced96a0) / INT-CI-070 (3e3a3088), 2026-10-14, post-dating the runs, in this
  worktree's base** — harden rootless docker unit-start and scale the dockerd readiness budget.
  They address *slow-start* stalls (the run-2 stall shape may shrink further), but nothing in
  them removes the deterministic stale-keyfile mechanism, which produces run 1's signature
  deterministically.
- **INT-HOST-007 reaper (45738749)** — reaps orphan homes, AppArmor profiles and linger entries.
  It does not know about `/tmp/bunker-key-*`.
- Grep at HEAD (`git grep bunker-key -- internal/`): the only reference is the creation site.
  Nothing in the repo or the CI workflow removes the residue.

**Live confirmation (2026-10-21, during this task):** `bunker-mvp:/tmp` currently holds five
stale pairs — `bunker-key-83b18967` (Oct 16), `bunker-key-a246cd48` (Oct 21 04:50 — today),
`bunker-key-att072w`, `bunker-key-procvis-23959`, `bunker-key-procvis-31762` (Oct 14). The leak
is not historical; it is accumulating from every killed daemon (attestation/proc-visibility lanes
among them). Any future spawn reusing one of those ids fails at keygen exactly as run 1 did.

**Mechanism red-proof on this box (2026-10-21):** with a stale file seeded at the deterministic
path and stdin closed, the pre-fix argv reproduces the run-1 transcript verbatim —
`... already exists.` / `Overwrite (y/n)? ` and **no key generated**; after removing the stale
pair (the exact action of the fix) the same argv generates a fresh key. The test
`TestINTCI042_KeygenSucceedsOverStaleKeyFile` drives the shared command constructor
(`newKeygenCmd`) so the tested argv is the argv the daemon runs.

## 4. Classification

**REGRESSION/REAL DEFECT — deterministic, fixed.** The trigger (a daemon killed before its
rollback) is environmental, but the *defect* is in-repo: a deterministic keyfile path with no
crash-safe pre-clear, on a persistent runner, with a battery that reuses fixed agent ids. Run 1
fails the same way every time the stale file exists; it is not load-bound (it failed in 0.4s on
an otherwise green run). Run 2's stall component is partially addressed by INT-CI-069/070, but
the killed-daemon residue mechanism this row is really about was open until this fix.

## 5. The fix (minimal, in `internal/`)

- `internal/agent/spawn_keyfile.go` (new): `keyFilePath(agentID)` (single source of the
  deterministic path), `preClearSpawnKeyFiles(agentID)` (removes THIS agent's stale pair before
  keygen; `os.IsNotExist` tolerated, any other removal failure fails the spawn loudly at
  `stage keygen`), `newKeygenCmd(ctx, agentID)` (the exact keygen argv, shared with the tests).
- `internal/agent/manager_spawn.go` Step 3: call `preClearSpawnKeyFiles` before keygen; build the
  command via `newKeygenCmd`. No exported surface added. Rollback/deferred cleanup semantics
  unchanged — they keep owning the files THIS spawn creates.
- Blast radius: exactly two paths — `<tmp>/bunker-key-<agentID>{,.pub}` for the id being spawned.
  No sweeps of shared `/tmp` prefixes.

## 6. Tests

`internal/agent/intci042_test.go` (table-driven + wiring arm):

- `TestINTCI042_PreClearStaleKeyFiles` — stale pair removed / clean path no-op / removal failure
  surfaces loudly (non-empty directory at the `.pub` slot as the ENOTEMPTY stand-in).
- `TestINTCI042_PreClearDoesNotTouchOtherAgents` — other agents' keyfiles and prefix-sharing
  siblings survive.
- `TestINTCI042_KeygenSucceedsOverStaleKeyFile` — the run-37666760535 fingerprint: the real argv
  succeeds over a seeded stale pair. Fails against the pre-fix tree (red proof: tests were run
  against the stash of the fix and failed with `preClearSpawnKeyFiles` undefined plus the
  mechanism demo above).

## 7. Residual risk

- The run-2 "daemon stuck >300s pre-registration" stall had at least two candidate contributors
  (rootless-install slowness, later hardened by INT-CI-069/070 which were not yet in the runs'
  tree). If a stall recurs despite those, it will now surface as a *fast* keygen-free failure or
  a successful re-list — but the 300s budget itself is unchanged by this fix.
- `/tmp/bunker-key-*` residue from OTHER ids (the five files live right now) still sits on disk;
  they are inert until some spawn reuses those ids. A future INT-HOST-007-style reap extension
  (`bunker reap` learning the keyfile class) would close the accumulation; not done here to keep
  the fix minimal.
- The CI step timeout kill → orphan `bunkerd` termination is runner behavior; the fix makes
  spawns resilient to it rather than preventing it.

## 8. Verification performed for this note

- Both run logs fetched and quoted from `gh api .../jobs/<id>/logs` (transcripts above are
  verbatim; line timestamps preserved).
- `hilo graph impact internal/agent/manager_spawn.go` — single crate-scoped importer
  (`internal/server/service.go`), no consumer-surface change.
- Full gates: `go build ./... && go vet ./...` clean, `gofmt` clean, `go test ./... -count=1`
  full suite (result recorded in the commit message; not truncated to a subset).
- Live battery: see the commit message and board row — run only if executed and observed;
  this note does not claim VERIFY-PASS speculatively.
