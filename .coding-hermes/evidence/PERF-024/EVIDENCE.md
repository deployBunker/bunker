# PERF-024 evidence — untracked (per brief)

All raw material for PERF-024. Captured live on bunker-mvp (systemd 255,
cgroup2, binary built from this worktree @ 950a282a) and from the CI run
45082347982 (gh run view --log, deployBunker/bunker, 2026-11-22T23:16Z).

## The claimed symptom, verified

- The 3/3 failing spawns were the e2e-full-battery.sh late-section cells
  (e2e-imgspec re-spawn, gap075-a, gap075-b) on the CI scratch daemon
  (:28081/:29091, registry disabled) — NOT the "E2E battery" step's first 12
  sections, which PASSED 37/0 at 23:50:55 (see ci-run-battery-section.log).
- Residue verified live: /etc/systemd/system/user-1732.slice.d (EMPTY dir —
  mkdir succeeded, conf write did not) and /etc/systemd/system/user-1733.slice.d
  (full standard-preset conf, mtime 23:57:49) survived the rollbacks. The
  foreman's cited user-1003.slice.d/user-1004.slice.d dirs are NOT on
  bunker-mvp ("no 1003 dir"); they are on another host (likely las-03,
  foreman-owned) — same mechanism, different uids.

## Root cause of the residue (AC3) — code-level, proven

manager_spawn.go armed the rollback's slice cleanup (createdUserSlice) only
when applyUserSliceLimitsAndVerify FULLY converged. A landing-check failure
returns (content, verifyErr) — the drop-in is on disk, the flag stayed false,
the rollback skipped removeUserSliceLimits while userdel removed the user.
Secondary leak: the daemon-reload failure leg returned content "" (the conf
was on disk) and the conf-write failure leg left the empty mkdir'd dir behind.

## AC1/AC2 instrumented spawns (all CONVERGED — see below)

- repro-driver.sh / battery-run.log: preset-battery --tier standard,
  PRESET_WORKLOADS=none, fresh scratch daemon :38081. Standard tier PASSED:
  conf 00:03:21.585 → daemon-reload complete (9114ms) → "containment landing
  converged attempt 1 elapsed_ms 0".
- cgroup-poll.log: 100ms external samples; perf024-peer (uid 1732) flip
  max→0 at 00:03:09.085, perf024-standard (uid 1733) at 00:03:30.811 —
  both with systemctl MemorySwapMax=0 at the same sample.
- force1/force2: two CONCURRENT standard spawns under the battery's live
  abuse-cell load — both converged attempt 1 (uids 1734/1735).
- force-churn2.sh / churn2-*: fresh scratch daemon + 3 destroy→spawn cycles
  (uid recycling, resetStaleUserSlice races) — all spawned rc=0, all destroyed
  cleanly (no residue).

## CI journal reconstruction (failing window, UTC)

23:56:59 useradd bunker-gap075-a uid 1732 → 23:57:20 conf written →
23:57:20 ITS OWN daemon-reload requested → 23:57:26 systemd parses the conf
("Unknown key 'LimitNOFILE'" proves the scan saw it) → 23:57:28 "Reloading
finished in 8874 ms" → landing poll (40x50ms=2s) exhausted by 23:57:30 →
gap075-b useradd at 23:57:30. Same shape for gap075-b (conf 23:57:49, reload
→ finished ~23:58:0x, poll exhausted, conf SURVIVES = the AC3 bug).
Back-to-back reloads at 23:56:30 (9139ms), 23:57:08 (9865ms), 23:57:20
(8874ms) show the reload queue under the battery's own churn; on THIS host a
fresh reload takes 9-10s even idle (measured on my runs).

## What was NOT reproduced

6/6 instrumented standard-preset spawns converged attempt-1 on the same host
and binary (idle, concurrent, recycled-uid, destroy→spawn churn). The
non-convergence needs a condition my runs did not create (the CI's
accumulated multi-hour churn + queued reloads); the failing window's journal
shows the reload completed AND parsed the conf while the following 2s poll
still read max. The landing budget (40x50ms, DF-BUNKER-67) is the lever, but
a budget change without a reproducible failing shape cannot be red-proven —
left for the foreman's call (AC4-style leftover).

## Files

- repro-driver.sh — the instrumented battery run driver (scp'd to the host)
- battery-run.log — battery stdout (standard tier PASSED; abuse cells failed
  under PRESET_WORKLOADS=none — A1 exec deadline, unrelated to landing)
- cgroup-poll.log / cgroup-poll2.log — 100ms cgroup+systemctl samples
- force1.log / force2.log — concurrent forced spawns (both ok)
- forcing-timeline.txt — forced + churn timelines
- churn2-daemon-lines.txt — the churn daemon's landing lines (all converged)
- churn2-journal.txt — systemd journal stream during the churn
- ci-journal-reconstruction.log — the failing window's journal lines (from
  journalctl over the CI window, quoted verbatim)
- ci-run-battery-section.log — the CI run's "E2E battery" step output
- churn-daemon-lines.txt — daemon lines up to the forced-spawn round
