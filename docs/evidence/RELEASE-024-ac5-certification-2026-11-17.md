# RELEASE-024 AC5 — exact-tree hosted CI certification (run 44052687456)

**Row:** RELEASE-024 — v0.4.0 prep: pin a fixed Go toolchain and rebuild vulnerable go1.26.10
artifacts for GO-2026-6859
**Criterion:** AC5 — "Exact-prep-SHA hosted CI must be terminal green with `root-suite` and
`regression` EXECUTED, not path-filtered; cite run ID and every job."
**Recorded by:** foreman tick `bunker-foreman-2026-11-17-15-26-50`
**Decision:** AC5 SATISFIED. Every other AC (AC1–AC4) was landed and locally proven earlier at
prep `a000797da946`; this document closes the last one.

---

## 1. The certification run

`gh run view 44052687456 --repo deployBunker/bunker --json headSha,event,status,conclusion,jobs`

| field | value |
| --- | --- |
| run id | **44052687456** |
| head SHA tested | **`8f19bfd5ada71c2e17dbd6f40ee07b048bc7b637`** |
| branch | `main` |
| event | `workflow_dispatch` (the certifying battery was deliberately dispatched, so both battery jobs were armed) |
| created | 2026-11-17T09:07:40Z |
| concluded | **2026-11-17T10:11:22Z** |
| run conclusion | **success** |

Every job, terminal:

| job | status | conclusion | started | completed | wall |
| --- | --- | --- | --- | --- | --- |
| `changes` | completed | **success** | 09:07:44Z | 09:07:55Z | 11 s |
| `build-and-test` | completed | **success** | 09:07:44Z | 09:08:19Z | 35 s |
| `gitreins-guard` | completed | **success** | 09:08:21Z | 09:08:34Z | 13 s |
| `unit-tests` | completed | **success** | 09:08:21Z | 09:11:36Z | 3 m 15 s |
| `regression` | completed | **success** | 09:23:13Z | 09:44:35Z | 21 m 22 s |
| `root-suite` | completed | **success** | 09:44:37Z | 10:11:22Z | 26 m 45 s |

Both self-hosted battery jobs **executed** — `regression` and `root-suite` are `completed/success`,
not `skipped`, not `queued`. They ran **serially** (regression finished 09:44:35Z, root-suite
started 09:44:37Z), which is the single-runner serialization RELEASE-028 measured: delay, not loss.

## 2. Why a run at `8f19bfd5` certifies the `a000797d` prep tree

This is the binding the previous tick recorded on the RELEASE-024 row, and both of its conditions
are re-verified here at the closing SHA.

**Condition (a) — both battery jobs reached `completed/success`:** satisfied, table above.

**Condition (b) — the product tree is unchanged since the tested SHA.** Two deltas, both
re-proved this tick with zero deletions:

```
$ git diff --name-status a000797d 8f19bfd5      # prep -> tested head
M  .coding-hermes/board/events.jsonl
M  .coding-hermes/board/tasks.jsonl
M  .gitreins/tasks.yaml
A  docs/evidence/RELEASE-028-ci-queue-2026-11-17.md
$ git diff --name-status 8f19bfd5 HEAD          # tested head -> closing SHA 72965f99
M  .coding-hermes/board/events.jsonl
M  .coding-hermes/board/tasks.jsonl
$ git diff --name-status 8f19bfd5 HEAD | grep -c '^D'
0
```

No `go.mod`, no `internal/`, no `cmd/`, no workflow file, no script. The tested tree and the prep
tree differ only in board/ledger bookkeeping and one added evidence document — the same delta class
the repo's own INT-CI-079 path filter treats as non-code. `go.mod` at the tested SHA still pins the
fix:

```
$ git show 8f19bfd5:go.mod
module github.com/deployBunker/bunker
go 1.26.5
toolchain go1.26.11
```

So the run certified the pinned-toolchain code that RELEASE-024 exists to ship. This is not a
sibling-SHA borrow: a descendant whose **code tree is byte-identical** is not another candidate.

## 3. What this closes, and what it does not

**Closes:** RELEASE-024 AC5. The release gate's "no candidate can obtain terminal green with the
battery executed" condition is met, so RELEASE-024 is complete and hands off to releng for the
v0.4.0 tag/publisher (owner-gated: no tag is created by this tick).

**Also discharges (evidence only, rows stay open on their own terms):**

- RELEASE-028 AC3 — "a fresh candidate run reaches a TERMINAL conclusion with `root-suite` AND
  `regression` completed (not skipped, not queued)": run 44052687456, head `8f19bfd5`, six of six
  jobs terminal, both battery jobs success. The row's *title* claim ("CI cannot certify any
  candidate") is therefore **refuted**: CI can and did certify, in 63 m 42 s.
- The QA-BUNKER-106 flake did **not** reproduce on this run (unit-tests `success` at 09:11:36Z
  after `failure` at 08:38:30Z on run 44047819097 with identical `internal/agent` sources). That is
  the third data point for residual R4 of
  `docs/evidence/RELEASE-028-ci-queue-2026-11-17.md` — a host-topology-dependent flake, not a
  per-SHA verdict.

**Does not close:**

- RELEASE-028 AC2 (make one capacity change). The four enumerated options are still
  forbidden (concurrency groups — `TestCIPushRunCannotBeCancelledIntoASkippedBattery`, INT-CI-083),
  infeasible (battery on a hosted runner — needs `/run/bunker`, `/var/lib/bunkerd`, root SSH state),
  owner/host action (a second self-hosted runner), or ineffective (splitting `root-suite` across
  jobs that would still serialize on the one registered runner). RELEASE-028 stays open on that
  owner action, with AC1 and AC3 recorded as satisfied.
- RELEASE-027 — the GAP-067 ship-or-hold decision record. Its AC2 precondition (a terminal-green
  exact-tree CI) is now met by this run; the decision itself is the next tick's deliverable.

## 4. Reproduce

```bash
gh run view 44052687456 --repo deployBunker/bunker \
  --json headSha,event,status,conclusion,jobs \
  | jq -r '.jobs[]|[.name,.status,.conclusion,.startedAt,.completedAt]|@tsv'
git diff --name-status a000797d 8f19bfd5
git diff --name-status 8f19bfd5 HEAD
git show 8f19bfd5:go.mod | head -5
```
