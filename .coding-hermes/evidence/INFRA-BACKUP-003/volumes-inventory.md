# INFRA-BACKUP-003 — anonymous docker volume inventory (AC1)

Measurement time: 2026-11-20T12:04Z. Source: `raw/bunker-las-03.volumes.txt`,
`raw/bunker-las-03.before.txt` (`docker system df -v`), `raw/bunker-mvp.before.txt`.

**No prune, no `docker volume rm`, no `docker system prune` was run.** This file only
measures and lists. Volume reclaim is PARKED pending an owner decision (see the ask at
the bottom).

## Summary per host

| host | local volumes | dangling volumes | dangling bytes (docker accounting) | total volume footprint |
|---|---|---|---|---|
| bunker-las-03 | 15 | **10** | **8.663 GB** | 12.61 GB |
| bunker-mvp | 0 | 0 | 0 B | 0 B |

`docker system df` on las-03 (verbatim, 12:04Z):

```
TYPE            TOTAL     ACTIVE    SIZE      RECLAIMABLE
Images          5         5         2.25GB    0B (0%)
Containers      5         4         486.9kB   486.9kB (100%)
Local Volumes   15        5         12.61GB   8.663GB (68%)
Build Cache     0         0         0B        0B
```

`docker volume ls -qf dangling=true | wc -l` → las-03 **10**, mvp **0**.

## las-03 — the 10 dangling volumes (all GitHub-Actions runner caches)

Every dangling volume matches the pattern
`runner-<container-id>-cache-<cache-hash>[-protected]`, i.e. cache volumes minted by
ephemeral `actions/runner` containers. None is referenced by any container, running or
stopped (`docker ps -a` mounts, verbatim, list only `buildx_buildkit_rt-ci-0010`,
`searxng`'s two anonymous ids `bb5278355bc5…`/`40d6856c64fb…`, `gitlab-runner`'s
`f66de5761416…`, and `9router-edge-data`).

| # | volume name | size | links | created |
|---|---|---|---|---|
| 1 | runner-7efe900bd67cbec6c2ef0873a399a427-cache-3c3f060a0374fc8bc39395164f415a70 | 0 B | 0 | 2026-11-07T08:08:06-08:00 |
| 2 | runner-7efe900bd67cbec6c2ef0873a399a427-cache-3c3f060a0374fc8bc39395164f415a70-protected | 713.4 MB | 0 | 2026-09-30T10:10:10-07:00 |
| 3 | runner-7efe900bd67cbec6c2ef0873a399a427-cache-c33bcaa1fd2c77edfc3893b41966cea8-protected | 2.212 GB | 0 | 2026-09-30T10:10:11-07:00 |
| 4 | runner-324c6c6d9436b4d76b13516ba0827db0-cache-3c3f060a0374fc8bc39395164f415a70 | 0 B | 0 | 2026-11-07T12:09:48-08:00 |
| 5 | runner-324c6c6d9436b4d76b13516ba0827db0-cache-3c3f060a0374fc8bc39395164f415a70-protected | 714.4 MB | 0 | 2026-09-30T10:19:46-07:00 |
| 6 | runner-324c6c6d9436b4d76b13516ba0827db0-cache-c33bcaa1fd2c77edfc3893b41966cea8-protected | 1.829 GB | 0 | 2026-09-30T10:19:49-07:00 |
| 7 | runner-3301f52db1ad110055f1793decfbc2ee-cache-3c3f060a0374fc8bc39395164f415a70 | 0 B | 0 | 2026-11-07T06:42:36-08:00 |
| 8 | runner-3301f52db1ad110055f1793decfbc2ee-cache-3c3f060a0374fc8bc39395164f415a70-protected | 734.5 MB | 0 | 2026-09-30T10:10:10-07:00 |
| 9 | runner-3301f52db1ad110055f1793decfbc2ee-cache-c33bcaa1fd2c77edfc3893b41966cea8 | 21 B | 0 | 2026-11-07T06:42:37-08:00 |
| 10 | runner-3301f52db1ad110055f1793decfbc2ee-cache-c33bcaa1fd2c77edfc3893b41966cea8-protected | 2.460 GB | 0 | 2026-09-30T10:10:11-07:00 |

Sum of listed sizes: 713.4 + 714.4 + 734.5 + 1829 + 2212 + 2460 = **8663.3 MB =
8.663 GB**, matching `docker system df`'s RECLAIMABLE column exactly.

Not dangling, therefore not in the ask: `buildx_buildkit_rt-ci-0010_state` (3.945 GB,
links 1 — held by the persistent buildx container), `9router-edge-data` (352.9 kB),
`searxng`'s two anonymous sizes (0 B each), `gitlab-runner`'s volume (0 B).

## mvp

`docker volume ls -qf dangling=true` → 0 lines; `docker system df` → `Local Volumes 0 0 0B`.
Nothing to inventory or ask for on this host. Reclaimable there is images, not volumes:
`Images 2 2 1.061GB 1.061GB (100%)` (2 dangling images, 1.061 GB) — listed as a parked
lever in report.md, not deleted.

---

## OWNER DECISION NEEDED

> **OWNER DECISION NEEDED: prune 10 anonymous volumes totalling 8.663 GB on bunker-las-03
> (ids listed above) — all are orphaned `runner-*-cache-*` volumes with 0 links and no
> container reference.**

Explicitly NOT done by this task: `docker volume prune`, `docker volume rm`, and
`docker system prune -a` were not run in any form. Executing the ask is one command
(`docker volume prune -a` after owner sign-off), and its effect should be re-measured
with the same `docker system df` probe committed here.
