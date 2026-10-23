# INT-CI-081 — deploy the ISO-002 /run/bunker fix to bunker-mvp, prove the agent contract, reap the residue

**When:** 2026-10-23, 13:41–14:31Z (all timestamps UTC, host clock of `bunker-mvp`).
**Host touched:** `bunker-mvp` (`78.46.173.180`, tailnet `100.115.67.116`) — the repo's only
self-hosted CI runner and the live-E2E demo host. No other host runs agents as unprivileged
users on this fleet today (`bunker-las-0{1..4}` are offline; `cube-las-00` runs no agents —
see `docs/ops/fleet-inventory.md`).
**Verdict:** all five acceptance criteria PASS. Two premise corrections and one operator hazard
are recorded in §6 — read them before repeating this deploy.

## 1. What was deployed, and from which commit

| Path | Before | After |
|---|---|---|
| `/opt/bunker/bunkerd` (unit ExecStart) | `0.2.0`, commit `149411c`, built `2026-10-02T12:00:36Z`, sha256 `3fb29c67…` | `0.2.1`, commit `ab183477`, built `2026-10-23T14:07:39Z`, sha256 `1ee9fe61…` |
| `/usr/local/bin/bunkerd` (battery/legacy path) | same `0.2.0`/`149411c` | same `0.2.1`/`ab183477`, byte-identical to `/opt/bunker/bunkerd` |
| `/usr/local/bin/bunker` (CLI) | `0.2.0`/`149411c` | `0.2.1`/`ab183477`, sha256 `0f2643cd…` |
| `bunkerd.service` | active, pre-fix build | active, `MainPID 3854268`, restarted `14:09:22Z`, listeners bound `14:13:53Z` |

Source commit: **`ab183477`** (`feat(ops): one-pass residue reap incl. /run/bunker + pre-job
runner reap`). Deployed with `scripts/fleet-upgrade.sh --hosts bunker-mvp`, which built from a
clean detached worktree at that commit and verified the RUNNING binaries report it:

```
fleet-upgrade.sh: building bunkerd+bunker v0.2.1 (ab183477) from /tmp/bunker-deploy-intci081
fleet-upgrade.sh: build ok: bunker 20255423
bunkerd 25436096
bunker-mvp OK: bunkerd bunkerd 0.2.1 -> bunkerd 0.2.1; CLI bunker 0.2.1
fleet-upgrade.sh: fleet upgrade complete: all hosts verified at HEAD
```

### 1a. Premise correction — `7cfd4c52` is NOT an ancestor of `HEAD`

The row asks for "the bunkerd/bunker built from a commit containing `7cfd4c52`". That commit
never reached `main`: it was the head of run 39898490380, which GitHub **cancelled** before its
battery could land it (INT-CI-083's own evidence block in `.github/workflows/ci.yml`). What
landed is its rebased twin — `4c1e3223`, re-applied as `7d2cd190` — plus the later INT-HOST-011
destroy fix `c3935613`:

```
$ git merge-base --is-ancestor 7cfd4c52 HEAD ; echo $?
1
$ git merge-base --is-ancestor 7d2cd190 HEAD ; echo $?      # twin that DID land
0
$ git diff 7cfd4c52 HEAD -- internal/agent/ e2e-full-battery.sh specs/network-isolation.md
 internal/agent/manager_destroy.go             | 19 +++-
 internal/agent/manager_destroy_linger_test.go | 130 ++++++++++++++++++++++
 (2 files, both from the LATER c3935613 fix; every INT-CI-080 file is byte-identical)
$ git show HEAD:internal/agent/sockdir_root_linux.go | grep 'RunRootMode os.FileMode'
const RunRootMode os.FileMode = 0711
```

So the deployed build contains the fix **content** byte-for-byte; the literal hash in the
acceptance criterion is unreachable on `main` and should be read as "the ISO-002 fix that
`7cfd4c52` proposed".

## 2. AC-1 — the NEW daemon enforces `/run/bunker` 0711 on spawn, twice

The pre-fix state was forced by hand first, so the repair could only come from the daemon:

```
14:14:15  stat before            drwx--x--x root:root /run/bunker
14:14:15  chmod 0700             drwx------ root:root /run/bunker      <-- pre-fix breakage, forced
14:14:15  spawn #1 intci081-a    (bunker spawn --server bunker-mvp --agent-id intci081-a --ttl 1h)
14:14:15  stat after spawn #1    drwx--x--x root:root ctime=2026-10-23 14:14:15.327676643 +0000
14:15:19  stat 64s later         drwx--x--x root:root ctime=2026-10-23 14:14:15.327676643 +0000   <-- no revert
14:15:19  spawn #2 intci081-b
14:15:19  stat after spawn #2    drwx--x--x root:root ctime=2026-10-23 14:15:19.640489433 +0000
          per-agent children     drwx------ bunker-intci081-a:root /run/bunker/intci081-a
                                 drwx------ bunker-intci081-b:root /run/bunker/intci081-b
```

Both spawns returned their connection bundle (port ranges 11100–11199 and 10400–10499, TTL 1h).
The second spawn ran **64 s** after the first and the parent stayed `drwx--x--x`: the 0700 revert
that INT-CI-081 measured twice within 25 s no longer happens.

## 3. AC-2 — agent-side contract, probed AS the agent (not from root)

Run on the deployed host, as `bunker-intci081-a`, after the AC-1 spawns:

```
$ su - bunker-intci081-a -s /bin/bash -c 'docker ps'
CONTAINER ID   IMAGE     COMMAND   CREATED   STATUS    PORTS     NAMES
rc=0
$ su - bunker-intci081-a -s /bin/bash -c 'ls /run/bunker'
ls: cannot open directory '/run/bunker': Permission denied
rc=2
$ su - bunker-intci081-a -s /bin/bash -c 'ls -la /run/bunker/intci081-a'
drwx------   4 bunker-intci081-a root     100 Oct 23 14:14 .
drwx--x--x 351 root              root    7020 Oct 23 14:15 ..
srw-rw----   1 bunker-intci081-a 1476365    0 Oct 23 14:14 docker.sock
drwx------   2 bunker-intci081-a root      40 Oct 23 14:14 run
rc=0
$ su - bunker-intci081-a -s /bin/bash -c 'ls /run/bunker/intci081-b'
ls: cannot open directory '/run/bunker/intci081-b': Permission denied
rc=2
$ stat -c '%A' /run/bunker          # root-side, for contrast only
drwx--x--x
```

Both halves of the contract hold simultaneously: the agent's own docker control channel works
(`docker ps` rc=0) **and** the tenant set cannot be enumerated (`ls /run/bunker` → EACCES),
with a peer's directory still refusing at the child. The same probe was re-run after the residue
reap (§4) and gave identical results.

## 4. AC-4 — residue disposition: reaped, counted before/after

`bunker reap` (built by this commit; it could not touch `/run/bunker` before) was run as root with
the newly deployed CLI. Dry run first, then apply; the classification predicate is unchanged —
"does this user still exist" — with the run-dir class asking about `bunker-<id>`, since the
directory name is the agent id.

| Class | Dry run (14:16–14:17Z) | Apply (14:17:35Z) |
|---|---|---|
| AppArmor profiles | 35,775 would remove · 14 kept · 102 unmanaged | 35,775 removed |
| Agent homes | 0 would remove · 16 kept · 11 unmanaged | 0 removed |
| Linger entries | 0 would remove · 12 kept | 0 removed |
| `/run/bunker/<id>` runtime dirs | 335 would remove · 13 kept | 335 removed |
| **total** | | **36,110 removed in 11.6 s** |

Measured host counts:

| Metric | Before | After reap (14:17:57Z) | Final (14:31:18Z, after the job leftovers below were destroyed) |
|---|---|---|---|
| files in `/etc/apparmor.d` | 35,891 (35,789 `home.bunker-*`) | 116 (14 `home.bunker-*`) | 108 (6 `home.bunker-*`) |
| dirs under `/run/bunker` | 348 | 13 | 6 |
| `bunker-*` users | 17 | 16 | 10 |
| `bunker-*` homes | 16 | 16 | 10 |
| linger entries | 12 | 12 | 5 |
| `/run/bunker` mode | `drwx--x--x` | `drwx--x--x` | `drwx--x--x` |

Nothing a live user owned was touched: `intci081-a`/`intci081-b` and the five agents already in the
daemon's registry (`crier-lab`, `0e8aec07`, `22fbe449`, `53097b60`, `19675466`) kept their profiles,
homes, linger entries and runtime dirs across the apply.

A second dry-run/apply pair after the host cleanup converged and is idempotent — it found only
3 newly-orphaned homes (from agents destroyed in between) and removed exactly those:

```
reap: DRY RUN   profiles 0/6/102   homes 3 would remove / 10 kept   linger 0/5   run dirs 0/6
reap: applied   … removed: 3
```

Why the residue existed and why it cannot silently re-accumulate:

* **Spawn side** — `EnsureAgentRunRoot` (`internal/agent/sockdir_root_linux.go`) repairs and then
  re-stats the parent on every spawn; the deployed build is the first on this host that carries it
  (AC-1 proves it live).
* **Destroy side** — the deployed build removes the runtime directory (`internal/agent/manager_destroy.go`
  “Step 4: Clean up /run/bunker/<id>/ directory”) and unloads + deletes the AppArmor profile
  (`internal/agent/apparmor.go`, `RemoveAgentAppArmorProfile` → `apparmor_parser -R` then
  `os.Remove`). Both were missing from the daemon this host was running (`0.2.0/149411c`), which is
  why 35.8 k profiles accumulated.
* **Reaper** — `bunker reap` covers what the destroy path never got to, for all four classes.
  It removes profile FILES without `apparmor_parser -R` (the same tolerance the destroy path
  documents: a loaded name whose file is gone is self-limiting). One orphan-uid survivor was
  noted and left alone: a 9.5-day-old `systemd --user` manager running as uid 1340 (no passwd
  entry, no agent of record) — outside every class the reaper classifies, named here rather than
  “cleaned” by hand.

## 5. AC-3 — cancellation, measured; the mitigation, implemented and proven

### 5a. What cancellation actually does (measured live, this tick)

A `workflow_dispatch` run (39968160980) was started and then cancelled at `14:27:41Z` while its
`root-suite` job was executing:

```
14:27:37  before cancel   pid 3864623  /opt/actions-runner/bin.2.337.0/Runner.Worker spawnclient 158 161
                          pid 3865147  bash scripts/root-suite.sh
14:27:41  gh run cancel 39968160980      -> "Request to cancel workflow … submitted"
14:27:45  procs=2
14:28:02  procs=2
14:28:07  procs=0                        <-- Runner.Worker tree gone 26 s after the cancel
run 39968160980 conclusion: cancelled; root-suite job: cancelled
```

**The worker tree does die within a minute when a cancel reaches a live session.** But the agents
that job had spawned did **not**:

```
14:28:5x  bunker-regr-alpha(1500) 6 procs · bunker-82799be6(1501) 7 · bunker-imgspec-none(1503) 8
          bunker-315a7196(1504) 8 · bunker-testagent-29013(1505) 2      <-- all still alive
```

Cancellation kills the process tree it owns; the per-agent systemd `--user` units are outside it,
and the battery wrapper's EXIT trap (which normally cleans them) never ran because the job was
killed. **So cancellation cannot be relied on for the full criterion.** The remedy used here and
available to any job: `bunker destroy <id>` — all five were destroyed through the CLI
(`Agent <id> destroyed.`, archive-before-delete), which is also what the daemon's boot reconcile
does automatically for unregistered orphans (§6).

### 5b. Why it cannot be relied on even for the worker tree

Measured 03:31:50Z the same day (INT-CI-081's own evidence):

```
03:31:50  Runner log: "Running job: root-suite"        (job dispatched to the runner)
03:48:15  parent /run/bunker reverted to 0700 by that worker's spawn loop
03:48Z    gh run list: NO active run (latest concluded 03:18)
03:50:52  Runner log: "Job root-suite completed with result: Failed" (code 102)
```

A job for a run that GitHub no longer listed as active ran for **19 m 02 s** on the host. The unit
is `KillMode=process`, so systemd does not sweep children either. That is the mechanism that
reverted the mode twice inside 25 s.

### 5c. The mitigation shipped: pre-job reap of stranded workers

`scripts/runner-stranded-reap.sh` runs as the first step of **both** self-hosted jobs (after
checkout). It kills every `Runner.Worker` tree that is **not an ancestor of the current step**
(leaf-first TERM, then KILL after a bounded wait), skips workers younger than `--min-age` (30 s),
never matches the `Runner.Listener` or PID 1, never fails the job, and refuses to kill at all when
it is **not** inside a runner job (`--force` is the deliberate escape hatch).

Proven on the runner host itself, against real processes, at 14:18:3x — the script's dry run named
a live stranded tree and the real run reaped it:

```
runner-stranded-reap.sh: would terminate child 3864062 (bunker spawn --server regression-local)
runner-stranded-reap.sh: would terminate child 3864061 (bash regression-tests.sh)
runner-stranded-reap.sh: would terminate stranded worker 3862135 (/opt/actions-runner/bin.2.337.0/Runner.Worker spawnclient 156 159)
runner-stranded-reap.sh: TERM sent to stranded worker 3862135 and <13 descendants>
runner-stranded-reap.sh: reaped 1 stranded runner worker tree(s): 3862135
Runner.Listener (pid 2433959) untouched
```

That demonstration also exposed the hazard now fixed in the script: **run from an operator shell,
every worker looks stranded**, and that invocation killed the live `regression` job of run
39968160980 (its conclusion `failure` is this, not a code defect). The script now refuses without
`--force` unless its own ancestry contains a `Runner.Worker`; `scripts/test-runner-stranded-reap.sh`
pins that arm (15 assertions: leaf-first order, self/own-worker/Listener/init never signalled,
min-age skip, dry-run, outside-job refusal, `--force`, fail-open vs `--strict`, clean-table no-op).

## 6. Deploy side effects and findings (read before repeating)

1. **First boot after the upgrade reconciles, in destroy mode.** `14:09:22–14:19:37Z` the new
   daemon replayed 13,281 events / 12 live agents, restored 9 from the registry, and **destroyed 10**
   (2 orphans, 3 force-destroyed because their exact port reservation was no longer restorable,
   4 TTL-expired, 1 purge). Every home was archived before deletion into
   `/var/backups/bunker` (largest `bunker-c2783eaf-…tar.gz` = 2,062,862,627 B); disk went from 70 %
   to 55 % used as the destroyed homes were freed. This is the documented archive-before-delete
   contract, but it means **a daemon restart is not a no-op on a host with orphan agents** — do it
   in a window with no CI battery running (the battery's fixed-name scratch agents are unknown to
   the main daemon and can be swept mid-test).
2. **`scripts/fleet-upgrade.sh` was unusable and silently half-deployed** — three defects found and
   fixed here: (a) its version probe anchored on `^Version`, but `internal/version/version.go`
   declares it inside an indented `var (` block, so every run died with “could not read Version”;
   (b) it shipped only `bunkerd`, although it builds the CLI too — this host served the old build
   from both daemon paths, which is exactly the drift the row measured; it now deploys and verifies
   `/opt/bunker/bunkerd`, `/usr/local/bin/bunkerd` and `/usr/local/bin/bunker`; (c) a plain
   `go build` does **not** always embed `vcs.revision` (in a linked worktree both binaries reported
   `commit: unknown`), so version/commit/date are now injected with the Makefile's `-ldflags` and
   the VERIFY step correctly refused the first, unstamped ship.
3. **`core.hooksPath` is a relative `.git/hooks` in this repo.** In a linked worktree that path does
   not exist, so `pre-commit` (guard) and `prepare-commit-msg` (co-author trailer) are **silently
   skipped**. The Tier-1 guard was run explicitly before committing
   (`gitreins guard` → PASS: secrets, build, lint, tests) and the fleet trailer hook was applied to
   the message file by hand so the commit still carries exactly one `Co-authored-by`.
4. **Proof agents were destroyed after the evidence was captured** (`intci081-a`, `intci081-b`), and
   the five agents stranded by the cancelled job were destroyed with `bunker destroy` — the host is
   left with 10 `bunker-*` users, 6 managed profiles, 6 runtime dirs, 10 homes, 5 linger entries,
   `/run/bunker` at `drwx--x--x`, and no runner worker.

## 7. Exact commands (abridged)

```sh
# build + deploy from a clean worktree at the commit, verify all three binaries
git worktree add --detach /tmp/bunker-deploy-intci081 ab183477
BUNKER_REPO_DIR=/tmp/bunker-deploy-intci081 BUNKER_SSH_USER=root \
  scripts/fleet-upgrade.sh --hosts bunker-mvp

# AC-1: force the broken state, then spawn twice with the NEW daemon
ssh root@bunker-mvp 'chmod 0700 /run/bunker; stat -c %A /run/bunker
  /usr/local/bin/bunker spawn --server bunker-mvp --agent-id intci081-a --ttl 1h
  stat -c "%A %U:%G ctime=%z" /run/bunker'
sleep 40
ssh root@bunker-mvp '/usr/local/bin/bunker spawn --server bunker-mvp --agent-id intci081-b --ttl 1h
  stat -c "%A %U:%G ctime=%z" /run/bunker'

# AC-2: agent-side contract
ssh root@bunker-mvp 'su - bunker-intci081-a -s /bin/bash -c "docker ps; echo rc=\$?"
  su - bunker-intci081-a -s /bin/bash -c "ls /run/bunker; echo rc=\$?"'

# AC-3: cancellation measurement, then the mitigation
gh run cancel 39968160980
bash /root/runner-stranded-reap.sh --dry-run     # inspect
bash /root/runner-stranded-reap.sh --force       # only from outside a job, deliberately

# AC-4: residue, dry run then apply, counts before/after
ssh root@bunker-mvp '/usr/local/bin/bunker reap --dry-run'
ssh root@bunker-mvp '/usr/local/bin/bunker reap --apply'
```

Related: [`../ops/fleet-version-gap.md`](../ops/fleet-version-gap.md) (the deploy-process gap this
run exercised), [`../../internal/agent/sockdir_root_linux.go`](../../internal/agent/sockdir_root_linux.go)
(the ISO-002/INT-CI-080 contract), [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml)
(the pre-job reap wiring).
