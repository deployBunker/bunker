# INFRA-BACKUP-003 — residual disk headroom after INFRA-BACKUP-002

Row: *"Residual disk headroom: las-03 84% (target <=70) and mvp 84% (target <=80) after
INFRA-BACKUP-002"*

Worked 2026-11-20, 11:56Z–12:16Z. Raw probe output for every number below is committed
under `raw/`. Verdict up front: **AC1, AC2, AC3, AC5 satisfied; AC4 NOT-REACHED on both
hosts** — las-03 needs 24.77 GiB more than its non-agent-home levers supply, and mvp's
Use% is churn-dominated QA work that took the box **84% → 100% → 98% during this task**.

**Two findings matter more than the percentages:**

1. **mvp holds 33 agent home directories (79.01 GiB) that `bunker list` does not report** —
   against 5 registered agents totalling 38.28 GiB. Reaping them is the single lever that
   exceeds the ≤80% target outright, and `/home` deletion is forbidden by this brief, so
   it is an owner decision (and a product defect: agent destroy does not reap the home or
   its rootless-docker tree).
2. **las-03's ≤70% target is not reachable** even with every owner-approved lever pulled:
   backups (13 G) + dangling volumes (8.66 GB) + alloy log opts (1.4 GB) sum to 21.1 GiB
   against a 24.77 GiB gap, and the rest of the box is agent-home data (136 G) that AC5
   protects.

## Per-host result

| host | baseline (row, 06:52Z) | measured before (this task) | after | freed (df -k pair) | target | verdict |
|---|---|---|---|---|---|---|
| bunker-las-03 | 84% (175G/35G) | 84% · used 183,342,228 KB · avail 36,176,660 KB (11:56Z) | **82%** · used 179,631,932 KB · avail 39,886,956 KB (12:14Z) | **3,710,296 KB = 3.54 GiB** | ≤70% | **NOT-REACHED** — gap 25,968,710 KB = **24.77 GiB** |
| bunker-mvp | 91% (131G/14G) | 84% · used 126,520,680 KB · avail 24,195,856 KB (11:59Z) | **98%** · used 147,407,368 KB · avail 3,309,168 KB (12:16Z) | **1,260,464 KB = 1.20 GiB** (log rotation) + 0.25 GiB (apt cache) | ≤80% | **NOT-REACHED** — gap 5.15 GiB at 12:10Z, **25.6 GiB at 12:16Z** as QA batteries ramped (see P1 below) |

Target arithmetic (df -k, denominator = used+available, matching how df computes Use%):
las-03 total 219,518,888 KB → 70% means used ≤ 153,663,222 KB; mvp total 150,716,536 KB →
80% means used ≤ 120,573,229 KB.

Two baseline corrections, stated because the row's numbers moved:

* **las-03's journald was unbounded and had grown from 8 M (06:52Z) to 4 G by 11:56Z** —
  a 4 GB growth in five hours on a host with no `journald.conf.d` drop-in. That is what
  AC2 exists to stop, and it was the single largest reclaimable item on the box.
* **mvp measured 82% (117G/27G) at 11:54Z, not 91%** — 13 GB left that filesystem between
  the row being filed and this task starting. This task did not cause it (first write was
  11:56Z); it is not attributed here.

## AC2 — journald capped durably and vacuumed (MET)

Drop-in written on **both** hosts, `/etc/systemd/journald.conf.d/99-ch-cap.conf`
(readback quoted in `raw/<host>.ac2.txt` and `raw/<host>.after.txt`):

```
[Journal]
SystemMaxUse=1G
RuntimeMaxUse=256M
```

`systemctl restart systemd-journald` + `journalctl --vacuum-size=1G`, then re-measured.

| host | journald before | journald after | df pair (KB) | freed |
|---|---|---|---|---|
| las-03 | **4 G** | **1001 M** | 183,342,228 → 180,119,240 | 3,222,988 KB = 3.07 GiB |
| mvp | 968.2 M | 976.2 M | 126,520,680 → 126,520,680 (window) | **0 B** — already under the 1 G cap; the cap now prevents the unbounded growth las-03 just demonstrated |

las-03's reclaim happened at journald restart, which itself enforces `SystemMaxUse` (the
explicit vacuum then reported `freed 0B` — quoted in the raw file, not hidden).

Also taken, from the explicitly-allowed cache-only class: `apt-get clean` —
las-03 `/var/cache/apt` 477 M → 104 K, mvp 256 M → 24 K.

## AC3 — /opt/actions-runner and /var/log classified (MET)

### bunker-mvp `/opt/actions-runner` — 3.6 G, **PROVEN NEEDED** (live CI for this repo)

```
2.1G  _work        (bunker 725M checked-out workspace, _update 683M, _tool 632M, _actions 25M)
601M  externals.2.338.0      601M  externals.2.338.2
 82M  bin.2.338.0             82M  bin.2.338.2
115M  runner.tar.gz          101M  _diag
```

Liveness proof (`raw/bunker-mvp.runner.txt`): `Runner.Listener run --startuptype service`
(pid 3648932) under `runsvc.sh` (pid 3648855); `.runner` registers agent **“bunker-mvp”**
(agentId 21) for `https://github.com/deployBunker/bunker`. Removing it would stop CI for
this repository, so it is classified **needed**, not superfluous. Parked sub-levers:
stale runner version `externals.2.338.0` + `bin.2.338.0` (683 M), installer tarball
`runner.tar.gz` (115 M), `_diag` logs (101 M).

### bunker-mvp `/var/log` — 2.9 G → **1.7 G**, ROTATED (config change, non-destructive)

The mass was one file: **`/var/log/auth.log.1` = 1,022,990,561 B** (976 MiB) accumulated
since the 2026-11-15 rotation, written by **snoopy** (every executed command produces an
auth.log line — 1,378,835 lines sampled). `syslog` was 188 M of the same firehose.

Changed `/etc/logrotate.d/rsyslog` (original kept at `/etc/logrotate.d/rsyslog.ib003.bak`):

```
rotate 4 → rotate 8     (nothing currently on disk is deleted; newest archive was .6)
weekly  → daily
+ maxsize 100M          (the durable cap: rotate early instead of growing for a week)
+ create 0640 syslog adm
```

and added `/etc/logrotate.d/bunkerd-logs` for `/var/log/bunkerd-battery.log` +
`/var/log/bunkerd/*.log` (`rotate 4`, `daily`, `maxsize 50M`, `compress`, `delaycompress`,
`copytruncate` — copytruncate keeps the writer's fd valid, no daemon restart).

Effect: `/var/log` **3.0 G → 1.7 G**; df pair 126,520,680 → 125,260,216 KB (1.20 GiB);
`auth.log.1` 976 MiB → `auth.log.2.gz` 27.7 MB, content retained compressed.

**Incident disclosed (self-inflicted, repaired, no log loss).** The first forced rotations
renamed `auth.log`/`syslog`/`kern.log` away and rsyslog — which `$PrivDropToUser syslog` —
could not recreate them: `/var/log` is `0755 root:syslog` (not group-writable), so the
privdropped user gets `open error: Permission denied` and suspends its omfile actions
(~30 s window). Repaired by pre-creating the three files `syslog:adm 0640` as root and
adding `create 0640 syslog adm` to the stanza so logrotate itself recreates them at every
future rotation. Delivery verified live afterwards (`logger` probe landed in syslog;
omfile `resumed` lines in `raw/bunker-mvp.rsyslog-fix.txt`, `raw/bunker-mvp.final2.txt`).
**No log records were lost:** journald is the primary store (`ForwardToSyslog=yes` only
duplicates), and 4,332 journal records — 3,642 of them `sshd`/`sudo`/`snoopy` — cover the
suspension window (`raw/bunker-mvp.lossless.txt`).

`/var/log/btmp` (84 M) + `btmp.1` (131 M) are failed-login security records — **listed,
not deleted, not rotated**; that record's retention is an owner call.

### bunker-las-03 — `/opt/actions-runner` absent; `/var/log` is journal-only

`du -sh /opt/actions-runner` → no output (does not exist on this host). `/var/log` was
4.1 G, of which 4.0 G was the journal — this host runs **journald-only logging** (no
`rsyslog`, no `auth.log`, no `syslog`), so AC2's drop-in *is* its `/var/log` cap:
4.1 G → **1022 M**, and no logrotate change was needed there.

## AC1 — anonymous volume inventory, prune PARKED (MET)

Full table: `volumes-inventory.md` (committed alongside this report).

* **bunker-las-03**: 10 dangling volumes, **8.663 GB** (docker accounting: `Local Volumes
  15 5 12.61GB 8.663GB (68%)`). All 10 are orphaned `runner-<id>-cache-<hash>[-protected]`
  volumes, `LINKS 0`, created 2026-09-30 / 2026-11-07, and **no container — running or
  stopped — references any of them** (verified against `docker ps -a` mounts).
* **bunker-mvp**: 0 local volumes, nothing to ask about.

**Nothing destructive was run:** no `docker volume prune`, no `docker volume rm`, no
`docker system prune -a`, no `rm -rf` on any path.

> **OWNER DECISION NEEDED: prune 10 anonymous volumes totalling 8.663 GB on bunker-las-03
> (ids in `volumes-inventory.md`) — all orphaned runner cache volumes, 0 links, no
> container reference.**

## AC4 — NOT-REACHED, with the gap and the named levers

### las-03: 82% now, 24.77 GiB short of 70%

Where the 179.6 GB used actually is (`raw/bunker-las-03.attribution.txt`, top-level `du`):
`/home` **136 G** (of which `/home/kara` 127 G), `/var/lib` 18 G (docker), `/var/backups`
13 G, `/usr` 6 G, `/opt` 107 M, `/tmp` 602 M.

Non-agent-home levers, largest first (none taken without an owner decision):

| lever | GB | gate |
|---|---|---|
| `/var/backups/bunker` — 20 tarballs, keep-3-per-prefix pruned only by the daily 04:17 guard | **13 G** | owner: retention window / ship off-host |
| 10 dangling runner-cache volumes (AC1) | **8.66 GB** | owner approval (ask above) |
| `buildx_buildkit_rt-ci-0010_state` volume | 3.95 GB | in use by the persistent buildx container |
| docker container json logs (alloy 1.045 GiB + 2 × ~170 MB) | **1.37 GB** | needs `daemon.json` log-opts + **dockerd restart** → would disturb live CI/alloy; parked per brief |
| `/home/*` agent homes | 136 G | **AC5 protected** — untouched |

Sum of the three movable items is 21.1 GiB against a 24.77 GiB gap: **even with the owner
approving both the backup retention change and the volume prune, las-03 lands ≈72–73%,
still short of ≤70%** — closing it requires touching agent-home data on this host, which
AC5 forbids without a separate decision.

### mvp: 84% → 98% during this task; ≥5.15 GiB short of 80% at the calm sample, 25.6 GiB at 12:16Z

The Use% here is not a stable quantity — it is churn from live QA work. Roadmap, all
`df -P -k` (`raw/bunker-mvp.churn.txt`, `raw/bunker-mvp.homes-du.txt`,
`raw/bunker-mvp.orphans.txt`):

| Z time | used KB | avail | Use% |
|---|---|---|---|
| 11:59 | 126,520,680 | 24,195,856 | 84% |
| 12:10 (3 samples / 90 s) | 128,224,296 → 125,976,184 → 128,849,940 | 22.5–24.7 G | 84–86% |
| 12:11 | 147,772,968 | 2,943,568 | 99% |
| 12:14 | — | **92 M** | **100%** |
| 12:16 | 147,407,368 | 3,309,168 | 98% |

**P1 — the host hit 100% while this task was measuring it, and the cause is not logs.**
The mass is concurrent QA batteries running inside agent homes: three agents created
within seven minutes (`bunker-0c48d903` 12:05:37Z, `bunker-0edba5f9` 12:05:58Z,
`bunker-064c9f1f` 12:08:48Z) are running `act`, Playwright and Go builds, and each runs
its **own rootless dockerd**, so every image layer is duplicated per agent home —
identical blob sizes `170,631,124` and `294,920,132` B appear under both
`/home/bunker-0c48d903` and `/home/bunker-0edba5f9`. `bunker list` at 12:16Z reports the
5 registered agents holding **38.28 GiB**; `/home` holds **117.3 GiB across 38 agent
directories** — i.e. **33 home directories totalling 79.01 GiB belong to agent ids
bunkerd does not report**, their unix users still exist, and 0 processes run under the
sampled ones.

That is the real lever, and it is larger than every other item on this host combined:

| lever | reclaimed | gate |
|---|---|---|
| **33 unregistered agent homes** (`/home/bunker-*` not in `bunker list`) | **79.01 GiB** | **`/home` deletion is forbidden by this brief (AC5)** — owner must decide; would take mvp to ~42% and past the ≤80% target outright |
| `/var/backups/bunker` (4.9–5.0 G; 3 tarballs / 1.87 GB landed in 20 min) | ~5 G | owner: retention window / ship off-host |
| `/root/.pulse-data` (`heartbeat.jsonl` alone 287 MB, still growing; ~1.3 G total) | 1.3 GB | `/root` protected (AC5) |
| `/opt/actions-runner` stale 2.338.0 (683 M) + `runner.tar.gz` (115 M) + `_diag` (101 M) | 0.9 GB | `/opt` deletion forbidden; runner is live CI |
| 2 dangling docker images (100% reclaimable) | 1.06 GB | not in the enumerated allowed reclaim class — parked |

Emergency attempt, disclosed: with 92 M free at 12:14Z I ran
`journalctl --vacuum-size=256M` (same cache-only class as the mandated vacuum). It
**freed 0 B** — disk usage stayed 984.2 M and the host recovered only because the QA
agents' own scratch writes receded. journald's vacuum will not shrink the bytes it is
currently accounting for; only the `SystemMaxUse=1G` cap stops future growth, and that
cap is now in place.

Failure check for the 100% moment (`raw/bunker-mvp.enospc.txt`): **no `No space left on
device` records exist in the journal for the window.** The first grep appeared to find 2
hits, but all matches were snoopy records of my *own* grep command lines (self-match) — the
same trap as `pgrep` matching itself. The box touched 100% and recovered without a logged
allocation failure; that is luck, not headroom, and it is one large write away from real
ENOSPC.

The durable half of the log fix is landed (rotation cap `maxsize 100M` on the snoopy
firehose + `maxsize 50M` on bunkerd logs), so mvp no longer grows by GB-per-rotation.
The residual growth is (a) unregistered agent homes and (b) agent backups — both owner
decisions, plus a product defect worth its own row: **agent destroy does not reap
`/home/<agent>` or the rootless-docker tree inside it.**

## AC5 — agent homes untouched (MET)

Read-only `du` only: las-03 `/home/kara` **127 G**, `/home` 136 G, `/root` 13 M; mvp
`/root` **6.4 G** (+ `.pulse-data` 1.3 G inside it), `/home` **117.3 GiB across 38 agent
directories**. Nothing under `/home`, `/root`, `/opt/actions-runner`, `/srv` or any docker
volume was deleted or modified. Ambiguous items are **listed, not deleted**: 33
unregistered agent homes, runner cache volumes, backup tarballs (both hosts), `btmp`,
`_work/_update`, `_diag`, dangling images, `buildx_buildkit_rt-ci-0010_state`.

## Parked levers (destructive or owner-gated) — nothing here was executed

| host | lever | reclaim | why parked |
|---|---|---|---|
| las-03 | prune 10 dangling runner-cache volumes | 8.66 GB | AC1 owner approval |
| las-03 | `/var/backups/bunker` retention (20 tarballs, 13 G) | up to 13 G | owner retention decision |
| las-03 | docker json log opts for alloy (1.045 GiB) | ~1.4 GB | needs dockerd restart → disturbs live alloy/CI |
| las-03 | `buildx_buildkit_rt-ci-0010_state` | 3.95 GB | in-use CI cache |
| mvp | **33 agent homes bunkerd does not report** (`/home/bunker-*`) | **79.01 GiB** | `/home` deletion forbidden by the brief; owner decision |
| mvp | `/root/.pulse-data` (heartbeat.jsonl 287 MB, growing) | 1.3 GB | `/root` protected by the brief |
| mvp | `/var/backups/bunker` retention (~5 G, +1–2 G/20 min) | ~5 G | owner retention decision |
| mvp | stale runner version + installer + `_diag` | 0.9 GB | `/opt` deletion forbidden; CI live |
| mvp | 2 dangling docker images | 1.06 GB | not in the enumerated allowed reclaim class |

## UNVERIFIED / assumptions

* **mvp's Use% cannot be quoted as a single number** — it ran 84% → 100% → 98% inside this
  task (QA batteries). Both gaps are reported (5.15 GiB at 12:10Z, 25.6 GiB at 12:16Z);
  neither is a steady state, and the ≤80% target cannot be held while batteries run.
* **Why journald's vacuum freed 0 B on mvp is not proven.** `--vacuum-size=1G` and
  `--vacuum-size=256M` both reported `freed 0B` while `journalctl --disk-usage` stayed at
  984.2 M with 119 `.journal` files present. Observed, not explained.
* The 33 unreported agent homes may be *destroyed agents whose homes were never reaped*
  or *a bunkerd registry that lost track of live-but-idle agents* — both fit the evidence
  (dirs present, unix users present, 0 processes under the sampled users, 5 agents in
  `bunker list`). Distinguishing them needs bunkerd-side state, which this task did not read.
* The 13 GB that left mvp between the row's 06:52Z baseline (91%) and this task's first
  measurement (82%) is **unattributed** here.
* The 8.663 GB volume figure is docker's own accounting; its post-prune real effect is
  unproven because the prune was (correctly) not executed.
* las-03's `/tmp` (602 M) and `/var/lib/docker` container logs have no owner-sanctioned
  reclaim path in this brief, so they stay listed rather than actioned.
* `docker system df` reports las-03 images as `0B (0%)` reclaimable — so image pruning is
  not a lever there; mvp's 1.061 GB of dangling images is, but was parked as above.
