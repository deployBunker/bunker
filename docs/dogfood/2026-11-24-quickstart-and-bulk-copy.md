# Dogfood integration report — 2026-11-24: the quickstart, bulk copy, and the residue surface

Run: tick `bunker-dogfood-2026-11-24-20-42-53`, single dispatch (gateway 15:43:14 EST / 20:43 UTC).
Workdir: HEAD `6ab8fc9e`, byte-identical to `origin/main` at start (fetched, 0/0).
Driver: this lane, using the fleet CLI `bunker 0.3.0 (0422dae7 2026-11-11)` against the deployed daemon on `bunker-las-03` (v0.2.1). No source changed; findings became board rows.

## Promise

"Bunker claims a user can spin up an isolated Linux coding environment (agent) in one CLI call — `bunker spawn --cpu 2.0 --memory …` — drive it (exec, cp, docker run via the agent's rootless docker), and destroy it cleanly, with visible residue/inventory status."

## What a real run looked like (timeline, PST server-local)

| Step | Command (verbatim shape) | Result | wall |
|---|---|---|---|
| spawn, default preset | `bunker spawn --cpu 2.0 --memory 2147483648 --ttl 50m --server bunker-las-03` | **FAILED** rc=1 at `slice-limits`: `containment landing did not converge within 40 reads: memory.swap.max = "max", want "0" (bar swap)` | 41.84s |
| spawn, verbatim retry | same | **FAILED** rc=1, different property: `memory.high = "7730937856", want 1932735240` | 22.17s |
| spawn, `--preset open` | `+ --preset open` | OK, agent `c940c03f` (uid 1004) | 17.95s |
| exec roundtrip | `bunker exec c940c03f -- sh -lc 'echo; uname'` | OK | 0.55s |
| cp tiny (60B) + warm | `bunker cp tiny.txt c940c03f:/tmp/…` | OK x2 (content verified byte-identical) | 4.63s / 5.81s |
| cp 30MB | `bunker cp ctl.bin (30,000,000B)` | **FAILED** rc=1 — agent file TRUNCATED at 27,156,480 B (3316×8KiB): "1 of 1 files landed, destination may be incomplete" | 33.04s |
| cp 60MB x2 | same shape | **FAILED** truncated, 33s each | 32.92/33.49s |
| pull 27MB | `bunker pull c940c03f /tmp/ctl.bin <dir> --server …` | OK, byte-honest (pulled md5 == agent md5 == truncated agent file!) | 25.50s |
| pull 60MB | same shape | rc=1 `scp: exit status 1` at 30.44s although the file landed **complete** (pulled md5 == source md5) | 30.44s |
| docker quickstart | `bunker exec c940c03f -- docker run --rm alpine echo hello` | OK incl. image pull | 5.13s |
| destroy | `bunker destroy c940c03f --server bunker-las-03` | OK, clean ("home 260.0 MB; archiving before delete"), own slice dir removed | 16.64s |

Agent-side writes were verified innocent before blaming the agent: `ulimit -f unlimited`, 30/40/60MB direct writes full-size, LimitFSIZE 64GB. The cp truncation is the CLI's own transfer budget. Mechanism with numbers, file:line and repro: DF-BUNKER-125; evidence `../../.coding-hermes/evidence/bunker-dogfood-2026-11-24-20-42-53/`.

## Findings (filed as rows)

1. **DF-BUNKER-124 (P1)** — standard-preset spawn (the built-in default and the README quickstart) refuses on the deployed fleet daemon: 2/2 containment-landing non-convergence, a *different* cgroup property each attempt, while `--preset open` and a same-day standard spawn both converge cleanly. Failed spawns strand `/etc/systemd/system/user-<uid>.slice.d/50-bunker.conf` (the 11-23 rollback fix `bffd9823` is not in the deployed v0.2.1 daemon).
2. **DF-BUNKER-125 (P1)** — `bunker cp`/`bunker pull` single-file steps carry a flat 30s budget (`internal/cli/cp.go:503`, applied at `cp.go:246` / `pull.go:157`): on a real fleet link (~0.86–1.9 MB/s) any file over ~26MB gets killed mid-transfer; cp truncates the destination (partial file on disk with an rc=1 and a warning), pull either reports rc=1 while landing the complete file or succeeds byte-honest on a truncated source — neither surface tells the operator what landed. Recursive `deploy` already carries a proportional budget for the identical GAP-144 failure; single-file cp/pull never got one.
3. **DF-BUNKER-126 (P2)** — `bunker status`'s Residue census structurally excludes AppArmor profiles (no `OrphanProfiles` field anywhere in the inventory): live las-03 printed "Residue: 0 orphan users, 0 orphan homes, 0 orphan keys, 0 stale linger entries" the same minute `bunker reap` counted 165 orphan profiles on that host.

## Judgement

**Works:** the quickstart docker workflow on a successfully spawned agent; exec (0.55s); tiny cp/pull; destroy (clean, 16.64s); TTL/expiry surface; status/health reporting; the honest failure messages (the cp incomplete-destination warning text is exactly right).

**Does not deliver today, on the fleet as deployed:** the README's own spawn happy path (default preset) and any file transfer above ~26MB. Both fail on the deployed daemon/CLI pair, not on scratch builds — PERF-024 and GAP-144 already fixed these classes tip-side, which sharpens the finding: the product the users actually run (fleet daemon v0.2.1, CLI 0422dae7) predates the fixes. RELEASE-003/DF-BUNKER-10/GAP-145 own the staleness; my numbers are fresh evidence for them.

**Would I use it again?** Yes — for short-lived exec/docker work the experience is good and fast. Would I seed a project tree today? Not above ~26MB per file at this link, and not with the default preset on a stale daemon: I would pass `--preset open` for the spawn and move files in pieces.

## Perf note (Step 2b)

No PERF- rows minted. The costs that would matter to a user (41.84s spawn → refused; 33s cp → truncated; 30s pull → rc=1) are all correctness failures, not slow-successes, so they live as evidence inside DF-BUNKER-124/125 rather than as perf polish rows. The genuinely fast surfaces (exec 0.55s, docker 5.13s, destroy 16.64s) feel good and are not findings.

## Left unverified this fire

- The install leg (fresh-machine installability battery): launcher preflight on the default host refused (bunker-mvp 96% disk > 94% threshold); relaunched on bunker-las-03 (passed disk admission) as agent `a26671a5` with the battery detached. At close the battery was still running — graded PARTIAL with the synced rows, not SKIPPED (harvest: `bunker-qa.sh collect --evidence <handle>`, handle `evidence=/tmp/bunker-qa-evidence-20261124T211229Z-2075062.jsonl`).
- Why containment landing converged at 12:42 PST today but not at 12:46/12:50 — attributed to conditions on the deployed daemon (PERF-024 owns the mechanism; the row carries both arms), not re-derived further here.
- The 60MB pull's rc=1-but-complete-file: measured twice in shape, not root-caused to the exact step whose exit status scp returned (the budget kill + the landed-complete state are both real observations).
