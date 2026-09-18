# bunker-mvp residue, made visible and cleaned — live verification (GAP-080)

Date: 2026-09-18 (tick 473). Host: bunker-mvp (78.46.173.180). All commands below are
copied from the tick's captured output; nothing here is reconstructed from memory.

## The row

GAP-080 (P1): *the demo host carries state its own surfaces cannot show — 108
`/home/bunker-*` dirs and 36 systemd linger units for users that no longer exist, while
the daemon reports 0/50 agents.* Two independent halves:

1. **Visibility** — the residue inventory existed in the repo (`DF-BUNKER-21`,
   `internal/agent/inventory.go` + the `Residue:` line in `internal/cli/status.go`) but
   was not deployed: the demo daemon predated it, so its own status surface could not
   show the residue.
2. **Remediation** — `bunker linger prune` (INT-HOST-001) cleans stale linger entries;
   **no command cleaned orphan agent home directories**, so the 125 homes were
   report-only once visible.

## Measured before (read-only probes, 2026-09-18 17:5xZ)

| Plane | Host truth (ssh) | Daemon view (bunker status) |
|---|---|---|
| `/home/bunker-*` dirs, user absent | 125 | not reported |
| systemd linger entries | 52 (51 stale, 1 live `root`) | not reported |
| `bunker-*` system users | 0 | — |
| registered agents | 0 | 0/50 |

Deployed daemon at the time: `/opt/bunker` checkout `a570197`, binaries built
`2026-09-18T08:08:24Z`. `git merge-base --is-ancestor d6c980e a570197` → **false**, i.e.
the running build predates the residue inventory commit.

```
$ bunker status --server bunker-mvp        # HEAD CLI, pre-deploy
  Version:  0.1.4
  Agents:   0/50
  /tmp:     private (per-session pam_namespace instance)
                                             <- no Residue line at all
```

## Half 1 — the inventory, live

Redeploy (CI idle, no battery running on the host, 0 registered agents):

```
$ ssh bunker-mvp 'cd /opt/bunker && git fetch origin -q && git reset --hard origin/main -q'
CHECKOUT 27e06f0 2026-09-18 12:39:30 -0500 chore(foreman): tick 472 ...
$ PATH=/usr/local/go/bin:$PATH make build          # box /usr/bin/go is 1.22.2 — the toolchain is /usr/local/go (1.26.5)
$ ./bunker version
bunker 0.1.4  commit: 27e06f0  built: 2026-09-18T17:56:04Z  go1.26.5  linux/amd64
$ systemctl restart bunkerd && sleep 4
ACTIVE active
HEALTHZ 200
SERVERINFO 401                # auth still enforced without a token
```

The daemon's own surface now answers:

```
$ bunker status --server bunker-mvp
  Version:  0.1.4
  Agents:   0/50
  Residue:  0 orphan users, 125 orphan homes, 0 orphan keys, 50 stale linger entries (0 registered agents)
            residue present: this host holds agent users/homes/keys/linger entries with no registered agent behind them
```

## Half 2 — the remediation

### 2a. Stale linger entries (existing command, now usable on the host)

```
$ cd /opt/bunker && ./bunker linger
  total entries: 52
  live users: 1
  stale (user gone): 51
$ ./bunker linger prune --dry-run
  scanned: 52   stale entries (would remove): 51   kept (live user): 1
$ ls /var/lib/systemd/linger | wc -l     # dry-run left the host untouched
52
$ ./bunker linger prune
  scanned: 52   stale removed: 51   kept (live user): 1
$ ls /var/lib/systemd/linger | wc -l
1                                        # the one LIVE user's entry is never removed
```

### 2b. Orphan agent homes (new command, commit f3bc048)

`bunker homes` (bare) reports, `bunker homes prune [--dir] [--dry-run]` removes exactly
the entries whose user is gone — fail-closed, prefix-scoped, dry-run-safe, mirroring
`bunker linger`'s seams.

```
$ ./bunker homes
homes status: /home
  entries scanned: 125
  stale (user gone): 125
  kept (live user): 0
  unmanaged (ignored): 2
$ ./bunker homes prune --dry-run
  scanned: 125   stale entries (would remove): 125
$ ls -d /home/bunker-* | wc -l           # dry-run removed nothing
125
$ ./bunker homes prune
  scanned: 125   stale removed: 125   kept (live user): 0   entries remaining: 0
$ ls -d /home/bunker-* | wc -l
0
```

Guard before the destructive leg: `docker ps -q | wc -l` = 0, `Agents: 0/50`, every entry
matched `bunker-*` and had no system user (`getent passwd | grep -c '^bunker-'` = 0).

## Result — the host reports a clean baseline (and can back the claim)

```
$ bunker status --server bunker-mvp
  Residue:  0 orphan users, 0 orphan homes, 0 orphan keys, 0 stale linger entries (0 registered agents)
```

The `residue present` warning line is gone; the four planes are individually zero.

## Command verification (local, independent of the live run)

```
$ go build ./... ; go vet ./...                                  # clean
$ go test ./internal/cli/ -run 'TestHomes|TestPruneHomesRoot' -count=1 -v
--- PASS: TestHomesCommandSurface / TestHomesStatusCommand / _HonoursDirFlag / _MissingDir /
    _UnreadableRoot / TestHomesPrune_RemovesOnlyStaleEntries / _DryRunMutatesNothing /
    _InconclusiveLookupRemovesNothing / _IgnoresUnmanagedNames / _MissingDirIsCleanNoOp /
    _KeepsUnreadableHomeDeletable / TestPruneHomesRoot_StopsAtFirstRemoveFailure   (12/12 PASS)
```

Scratch-root run (independent of the worker's own transcript): a root holding
`bunker-orphan-a`, `bunker-orphan-b`, a human home and a dotfile reports `2 stale / 2
unmanaged`, `prune --dry-run` leaves both dirs on disk, `prune` removes exactly the two
`bunker-*` entries and leaves the human home and dotfile untouched.

## New finding filed: the daemon's linger count is prefix-scoped (INT-HOST-004)

Measured at 17:57Z, same instant, same host:

| Counter | Stale linger entries |
|---|---|
| host-local `bunker linger` | **51** (of 52 total, 1 live) |
| daemon `Residue:` line | **50** |

`internal/agent/inventory.go` scans the linger plane for names matching the managed
`bunker-` prefix, so a stale entry carrying any other name is invisible to the counter —
GAP-080's own probe recorded exactly such a name (`bunter-1d2b6cef`, a typo) among the
sampled units. Reproduced live with a one-file probe on the demo host:

```
$ touch /var/lib/systemd/linger/bunter-probe-gap080
$ bunker status --server bunker-mvp | grep Residue
  Residue:  0 orphan users, 0 orphan homes, 0 orphan keys, 0 stale linger entries (0 registered agents)
$ cd /opt/bunker && ./bunker linger
  total entries: 2
  live users: 1
  stale (user gone): 1
$ rm -f /var/lib/systemd/linger/bunter-probe-gap080     # probe removed; host back to 1 entry
```

So a stale entry whose name drifts from the managed pattern counts toward host truth but
not toward the daemon's residue report — the same "surface understates the host" class
GAP-080 describes, one plane narrower. Filed as INT-HOST-004 (P3).

## Deployment state at tick end

- `/opt/bunker` checkout: `27e06f0`; `bunkerd` + `bunker` binaries built from it
  (`0.1.4`, commit `27e06f0`, built `2026-09-18T17:56:04Z`); `bunkerd` active, healthz
  200, unauthenticated ServerInfo 401.
- The CLI binary on the host was then updated to `f3bc048` (the `bunker homes` commit) so
  the new command could be exercised live. The daemon process still runs the `27e06f0`
  build: `git diff --name-only 27e06f0 f3bc048` = `README.md`, `cmd/bunker/main.go`,
  `internal/cli/SKILL.md`, `internal/cli/homes.go`, `internal/cli/homes_test.go` — **no
  daemon-side code**, so no restart was required (a restart during the new CI run is
  forbidden by the shared-host rule). The next redeploy realigns checkout, binaries and
  process at HEAD.
