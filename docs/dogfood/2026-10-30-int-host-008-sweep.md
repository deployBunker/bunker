# Bunker host-residue backlog sweep — bunker-mvp — 2026-10-30

ch:trace tick=bunker-foreman-2026-10-30-int-host-008; row=INT-HOST-008; host=bunker-mvp; repo-head=5f58f6ab

## Verdict

The accumulated host-residue backlog on bunker-mvp is removed. Kernel-loaded agent
AppArmor profiles went from **36,171 to 3**, profile files from **441 to 3**, agent
home leftovers from **4 to 2**, runtime directories from **159 to 2**, and the agent
registry from **1 listed agent to zero**.

The sweep is not a "zero, stable" result, and this document does not claim one. Two
independent actors were running on that host for the whole sweep — a failing
`bunker-qa.sh` spawn loop and a GitHub Actions CI job on its self-hosted runner — and
every failed spawn attempt re-creates exactly the residue this row exists to remove.
The measured floor is therefore "zero accumulated backlog, plus the live fixtures of
the test that is running right now".

Three defects were found and are reported here rather than fixed: `bunker reap` never
unloads AppArmor profiles from the kernel, `bunkerd` kept a phantom agent listed as
`running` for ~25 hours after it was destroyed, and this host's `systemd-logind` is
hung, which is what makes every spawn fail and re-seed the residue.

## Authority for the deletion

`bunker reap --apply` deletes host state and INT-HOST-008 is gated on an owner
deletion decision. That decision is the dispatch of this task: "Execute a full backlog
sweep on the live E2E host (bunker-mvp), verify zero orphaned agents/users, and commit
evidence", with step 2 ("destroy them") and step 4 ("Check system users for `bunker-*`
leftovers; clean if found"). Recorded here as the gate's required note. Two earlier
ticks (2026-10-20, 2026-10-22) correctly left the row pending because no such decision
existed at the time.

## Host access

The dispatch names `78.46.173.180`. SSH to that IP from the control host times out
(rc=124, no banner), so every host command in this sweep ran over the host's Tailscale
address `root@100.115.67.116`, which resolves to `hostname=bunker-mvp` — the same
machine, whose tailnet entry shows `direct 78.46.173.180:41641`. Daemon-side commands
(`bunker list/destroy`) used the registered server `bunker-mvp`. Host clock is UTC;
all timestamps below are UTC.

## Before state — 2026-10-30T19:18:43Z to 19:22Z

| Surface | Measured |
|---|---|
| `bunker list --server bunker-mvp` | 1 agent: `crier-lab`, status `running`, 0 B |
| `getent passwd \| grep '^bunker-'` | 1 (`bunker-gap128-91074`) |
| `ls -d /home/bunker-*` | 4 |
| `ls /var/lib/systemd/linger` | 1 (`root`) |
| `ls /etc/apparmor.d \| grep home.*.bin.rootlesskit` | 441 |
| `ls -d /run/bunker/*/` | 159 |
| `aa-status` line 2 | `36286 profiles are loaded` |
| loaded `/home/bunker-*/bin/rootlesskit` profiles | 36,171 |
| `df -h /` | 29G used / 150G — 20% |

The row's recorded premise (≈18,043 orphan profiles, ~610 orphan homes, ~346 stale
linger, disk near 97% during the 2026-10-20 battery) no longer described the host:
homes, linger and disk had already been reclaimed, and the profile population had
grown to 36,171. The backlog was larger than recorded and concentrated in one class.

Pre-sweep dry run, saved to `/tmp/bunker-reap-presweep.txt` on the host:

```
reap: DRY RUN (no changes written; pass --apply to remove)
apparmor profiles: /etc/apparmor.d
  would remove (user gone): 443
  kept (live user): 2
  unmanaged (ignored): 102
agent homes: /home
  would remove (user gone): 3
  kept (live user): 2
  unmanaged (ignored): 11
linger entries: /var/lib/systemd/linger
  would remove (user gone): 0
  kept (live user): 1
agent runtime dirs: /run/bunker
  would remove (user gone): 161
  kept (live user): 2
```

## Actions

**1. Phantom registry entry destroyed.** `crier-lab` was listed `running` with no user,
no home, no runtime dir and no profile — while `/var/lib/bunkerd/agents.jsonl` had
recorded `{"ts":"2026-10-29T18:24:54Z","kind":"destroy","agent_id":"crier-lab"}` about
25 hours earlier.

```
$ bunker destroy --server bunker-mvp crier-lab
Destroying agent crier-lab (home size unknown; archiving before delete, deadline 12m0s)…
Agent crier-lab destroyed.
Removed local SSH key /home/kara/.config/bunker/keys/crier-lab

$ bunker list --server bunker-mvp
No agents found.
```

**2. `bunker reap --apply`** (`/tmp/bunker-reap-apply.log`), banner
`grep -c '^reap: applied'` = 1:

```
reap: applied
...
  removed: 607
```

607 = 443 profile files + 3 homes + 0 linger + 161 runtime dirs. Two entries were kept
because their users existed at scan time.

**3. Kernel unload of the loaded — but fileless — orphan profiles.** `bunker reap`
cannot do this (see Defect A), so it was done directly through the AppArmor filesystem
interface, one profile at a time, re-reading `/etc/passwd` every 2000 entries so a user
created mid-run was never treated as absent (`/tmp/bunker-unload-orphans.log`):

```
start 2026-10-30T19:26:22Z
candidates 36173
removed=36173 kept=0 failed=0
remaining=1
end 2026-10-30T19:29:04Z
```

`kept=0` is a true reading, not a broken predicate: the two users alive at 19:25
(`bunker-bc916c39`, `bunker-test-agent-89177`) were both **failed spawns** rolled back
by `bunkerd` at 19:26:31 and 19:27:12, inside the run window. One profile was unloaded
in error during setup probing before that (`bunker-bc916c39`, whose user appeared
between two measurements); it was re-loaded immediately with
`apparmor_parser -r /etc/apparmor.d/home.bunker-bc916c39.bin.rootlesskit` and verified
present again. That is disclosed here because it is exactly the race this host's churn
creates.

**4. Second `bunker reap --apply`** after the unload, plus a single fileless-orphan
unload — `removed: 4`.

## After state — 2026-10-30T19:33:54Z

| Surface | Before | After |
|---|---|---|
| `bunker list` | 1 (phantom) | **No agents found.** |
| `aa-status` line 2 | 36,286 profiles loaded | **121 profiles loaded** |
| loaded `/home/bunker-*/bin/rootlesskit` | 36,171 | **3** |
| profile files | 441 | **3** |
| agent homes | 4 | **2** |
| linger entries | 1 (`root`) | **1 (`root`)** |
| `/run/bunker` runtime dirs | 159 | **2** |
| `df -h /` | 20% | 20% (unchanged; residue was inodes/kernel, not disk) |

The 3 remaining loaded profiles, 3 profile files, 2 homes, 2–3 runtime dirs and 2 users
belong to the **live fixture agents of the CI job running on the host at that moment**
(`/opt/actions-runner/_work/... regression-tests.sh`, spawning `bunker-regr-alpha`,
`bunker-4d1a8910`, `bunker-da5af7ce`, `bunker-973bbbdd` in rotation). They are not
orphans, and deleting them would have broken an in-flight CI run — so the sweep
deliberately stopped at the boundary the classifier draws: does the user exist.

Three consecutive dry runs 20 s apart show the churn directly, and they are the honest
statement of what "zero" means on this host while CI is active:

```
19:33:09Z  would remove: 1 | 0 | 0 | 1    users: bunker-4d1a8910 bunker-da5af7ce
19:33:31Z  would remove: 2 | 0 | 0 | 2    users: bunker-da5af7ce bunker-973bbbdd
19:33:54Z  would remove: 2 | 0 | 0 | 2    users: bunker-da5af7ce bunker-973bbbdd
```

(reap class order: apparmor profiles | agent homes | linger | runtime dirs.)

## Acceptance criteria

| # | Criterion | Result |
|---|---|---|
| 1 | `bunker list --server bunker-mvp` enumerates agents | PASS — 1 agent found at start |
| 2 | Destroy agents that exist | PASS — `crier-lab` destroyed (phantom record) |
| 3 | Zero agents remain | PASS — `No agents found.` |
| 4 | `bunker-*` system users checked and leftovers cleaned | PASS with a caveat — no orphan user survived; the only users present are live CI fixtures that must not be deleted |
| 5 | Evidence committed to `docs/dogfood/` | PASS — this file |
| 6 | Board row `complete` with commit hash | PASS — see the row |

The row's own PASS line ("loaded profile count <= live agent count") is met in the
sense it was written for — loaded **orphan** profiles = 0 against 0
daemon-registered agents — but not as a literal total: 3 loaded profiles belong to live
users the daemon does not list as agents because they are CI fixtures created directly
on the host.

**The dispatch's verification commands are partly unsatisfiable as written, and that is
reported rather than worked around:**

* `bunker list --server bunker-mvp | grep -c 'bunker-'` returns **1**, not 0, even with
  zero agents: the CLI prints the preamble `bunker: reading server "bunker-mvp"`, and
  that line contains `bunker-`. The criterion can never pass on any host whose server
  name starts with `bunker-`.
* `getent passwd | grep 'bunker-'` is **not empty** while a CI job is running on the
  host, because CI fixtures are real users by design. Empty is achievable only at a
  moment with no test running.
* `go test ./... -short` — **PASS**: `go build ./... ` rc=0, `go vet ./...` rc=0,
  `go test ./... -count=1 -short -parallel 4` rc=0, zero FAIL lines
  (`/tmp/int-host-008-gates.log`).

## Defects found

**A. `bunker reap` never unloads AppArmor profiles from the kernel.**
`reapRemove` (`internal/cli/reap.go:117`) routes the profile class to
`removeLingerEntry` (`internal/cli/linger.go:37`), which is a plain `os.Remove`. The
only unload (`apparmor_parser -R`) lives in the destroy path. Consequence: every reap
run deletes the profile file and leaves the profile loaded forever, and a fileless
orphan profile is invisible to the next run's scan. This is why the host held 36,171
loaded profiles against 441 files — the backlog was created by the reaper itself. The
sweep needed a separate unload pass to satisfy the row.

**B. This host's `systemd-logind` is hung, and that is the residue factory.**
`loginctl list-users` produces no output and takes the full 15 s timeout; `systemctl
is-system-running` reports `degraded`. Every spawn then fails at
`rootless-install`: "enable linger for bunker-<id>: exit status 1 (output: Could not
enable linger: Connection timed out)". `spawn-failures.jsonl` is 4.4 MB. Each failure
rolls the user back but leaves a profile file and a runtime directory behind, which is
exactly the 1–2 "would remove" entries appearing every 20 s in the churn readings
above. The backlog will re-accumulate until logind is restarted and the spawn rollback
unloads what it installed.

**C. Phantom agent state outlives its destroy.** `crier-lab` was destroyed at
2026-10-29T18:24:54Z per `agents.jsonl`, yet `bunker list` reported it `running` with a
fresh `Expires At` 25 hours later, with no user, home, runtime dir or profile on the
host. Either the daemon's in-memory registry is not updated for that destroy path, or
it is not reconciled from the event log on restart. `bunker destroy` cleared it.

## Concurrent actors (disclosed, not coordinated)

* `bunker-qa.sh launch` retry loop against bunker-mvp, all attempts failing at
  `rootless-install`.
* A GitHub Actions self-hosted-runner job on bunker-mvp running `regression-tests.sh`
  (`/opt/actions-runner/_work/...`), which creates and tears down agent fixtures
  continuously. This is why no stable "zero users" endpoint exists on this host.
* A second agent session (PID 1783116, started ~10 min before this one) executing the
  identical INT-HOST-008 brief — a duplicate dispatch of the same row. No commit for
  this row existed when this sweep started (`git log`, board row `pending`), so this
  session proceeded; the earlier sweep of 607 reaped entries and the 36,173-profile
  unload are attributable to this session's log files on the host
  (`/tmp/bunker-reap-presweep.txt`, `/tmp/bunker-reap-apply.log`,
  `/tmp/bunker-unload-orphans.log`, `/tmp/bunker-reap-post.txt`,
  `/tmp/bunker-reap-final.txt`).

## Residual, stated plainly

* Loaded AppArmor profiles on bunker-mvp are 121 total / 3 agent-owned, not zero. The 3
  are live CI fixtures.
* The residue **source** is untouched: logind is still hung, and every failed spawn on
  that host still leaks a profile file and a runtime directory.
* No `bunker-*` user was deleted by hand in this sweep; the orphan users that existed at
  the start were removed by `bunkerd`'s own rollback/teardown during the window, and
  reap removed the homes, files and runtime dirs they left behind.

## Final verification — 2026-10-30T19:37:56Z

Two cleanup passes were run after the churn above finished re-seeding the host, to
leave the deepest honest reading on record:

* `bunker reap --apply` at 19:37:48Z — `removed: 8` (the entries the CI job's failed
  spawns had left in the four minutes since the sweep).
* orphan unload pass at 19:37:56Z — `candidates 5 removed=4 kept=1 failed=0`,
  `remaining=1`.

Result: **loaded agent AppArmor profiles = 1, live `bunker-*` users = 1, live
daemon-registered agents = 0, `bunker list` = "No agents found."** The single remaining
loaded profile belongs to the one live CI fixture user, which the classifier keeps by
design. This is the reading the row's PASS line ("post-sweep loaded profile count <=
live agent count") refers to: zero orphan profiles against zero registered agents.

