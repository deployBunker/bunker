# bunker release-readiness sweep — 2026-11-11

Lane: `bunker-releng` (verify-only). Tick: `bunker-releng-2026-11-11-05-44-30`.
Repo under release management: `deployBunker/bunker` (public, GitHub), resolved from
`git remote -v` at `/home/kara/deployBunker/bunker`, not from the scheduler workdir.

## Certified candidate

| Field | Value |
| --- | --- |
| Default branch | `main` |
| Certified tip | `e0f3166670c76b66778f2f8e51c762ecd6411e91` |
| Agreement | local `origin/main`, `git ls-remote origin refs/heads/main` agree |
| Last release tag | `v0.3.0` (tag object `880ae8e4`, peeled commit `de286fdce69cd92f9f182f3e1eecde34404d713e`) |
| Release published | 2026-11-09T05:17:25Z, 6 assets |

## Decision

**NOT DUE.** Census over `v0.3.0..e0f31666` = 32 commits, of which 0 `feat`, 0 `fix`,
0 `perf`. Every commit is board/QA/PM/docs bookkeeping. The prior certified tip
`8f5511f9` was followed only by this sweep's own evidence/board carrier commit
`e0f31666`; its delta is also non-shipping. No `BREAKING CHANGE` declaration and no
`type!:` subject exists on the range, so no major proposal.

Diff confirmation of materiality: `git diff --name-only v0.3.0..e0f31666` returns six
paths, all non-shipping —
`.coding-hermes/board/events.jsonl`, `.coding-hermes/board/tasks.jsonl`,
`.coding-hermes/board/evidence/bunker-qa-2026-11-11-00-26-59-cycle2.json`,
`docs/evidence/RELEASE-readiness-2026-11-11.md`, `docs/evidence/RELEASE-v0.3.0-cut-2026-11-09.md`,
`docs/ops/fleet-inventory.md`. No installed-surface change exists to earn a patch bump.

## Evidence table

| Axis | Result | Receipt |
| --- | --- | --- |
| Census `v0.3.0..e0f31666` | 32 commits, 0 material | `git log --format='%H %s' v0.3.0..e0f31666` then prefix tally; 31 prior commits plus this carrier |
| CI on certified tip | GREEN | run `42968620202` (CI, push, main) completed success 2026-11-11T05:57:10Z; exact jobs: changes/build-and-test/unit-tests/gitreins-guard success, root-suite/regression skipped by board/docs-only classification |
| CI on parent tip | GREEN | run `42946753509` completed success |
| CI at the v0.3.0 cut | GREEN | run `42566171520` (CI) success; run `42566171652` (Release) success on `de286fdc` |
| Version surfaces vs tag | ALIGNED | `Makefile: VERSION ?= 0.3.0`; `internal/version/version.go: Version = "0.3.0"`; `README.md` pins `v0.3.0` (lines 274, 348, 1315); `CHANGELOG.md` has empty `## Unreleased` then `## 0.3.0 (2026-11-09)` |
| Release assets | VERIFIED | all 6 present; downloaded 5 hashed assets, `sha256sum -c SHA256SUMS` rc=0 (5/5 OK) |
| Released binary self-report | VERIFIED | `bunker-linux-amd64 --version` → `bunker 0.3.0`, commit `de286fdc`, built 2026-11-09T05:16:34Z |
| Release body | present | 492 chars (categorized notes, not a bare compare link) |
| GitReins verdicts | none bound to the tip | no `.gitreins/history` run for `8f5511f9`; not a gate for a not-due verdict |
| Dogfood/QA | ticks running | QA tick `bunker-qa-2026-11-11-00-26-59` cycles landed in the range; no `RELEASE-*` gate attached |
| Deployed binaries | STALE (RELEASE-003 holds) | `~/.local/bin/bunker` and `bunkerd` → 0.2.1 / `bff29c84` / built 2026-11-06T14:02:41Z |
| Public posture | FAIL (RELEASE-022 holds) | `secret_scanning` disabled, `secret_scanning_push_protection` disabled, `dependabot_security_updates` disabled; vulnerability alerts enabled (`HTTP 204`) |
| Release-gate SPOF | OPEN (RELEASE-023 holds) | `bufbuild/buf-setup-action@v1` still fetched at 3 CI jobs (`ci.yml:118`, `:298`, `:355`) |

## Deployed staleness — measured, direction stated

Deployed build `bff29c84` (2026-11-06) is an ancestor of `v0.3.0` (`git merge-base
--is-ancestor bff29c84 v0.3.0` rc=0), i.e. the release content is absent from the
running binaries. Gap measured at the tag: 58 commits total, of which 8 touch shipped
source and 14 shipped files differ —

```
de286fdc release: bump to v0.3.0 …, 649232ef metrics(webdav): add bunker_webdav_enabled …,
3db86573 board(BOARD-GUARD-004): census vocabulary + exec bit,
3f17494e board(BOARD-GUARD-004): commit-hash census tool,
6e6a33bd board(release): RELEASE-021 — pre-release-check.sh between-tags WARN,
4030831f guard(board): line-by-line JSONL validator,
61e40bf2 fix(metrics): refuse non-bunkerd HTTP 200 responses in textfile shim,
92e5b992 perf(audit): reduce fast-path hash allocations
```

Two releases behind the line (v0.2.0 shipped 2026-10-02, v0.3.0 2026-11-09). This lane
does not install or restart daemons on live hosts; the row stays open with these numbers.

## Open blockers carried, not re-filed

- `RELEASE-003` (P2, pending) — deployed binary staleness; re-measured, still true.
- `RELEASE-008` (P2, blocked) — depends on `RELEASE-003`.
- `RELEASE-022` (P2, pending) — public posture hardening; still disabled on all three axes.
- `RELEASE-023` (P2, pending) — external buf download inside the release gate; still present.

## Release tooling of record (used, not replaced)

The repository ships its own path — `Makefile` (`release-binaries`, `ci`, `lint`,
`docs-check`), `scripts/pre-release-check.sh`, `scripts/install.sh`,
`.github/workflows/ci.yml` + `release.yml`, and `docs/release-checklist.md`. Tag-triggered
`release.yml` owns publication; this lane does not race it with manual `gh release create`.

## Carrier

Board annotation: audit event on the rolling `RELEASE-019` row plus a refreshed
`foreman_note` measurement on `RELEASE-003`. No new `RELEASE-*` row was created — the
latest pass is after the tag and the rolling row owns post-cut sweeps.

This artifact was first committed at `e0f31666` while certifying `8f5511f9`; this
pass re-anchors the header, census, and exact-SHA CI receipt to the carrier tip.
The carrier's own run is `42968620202` and is terminal success, but its two
self-hosted jobs were correctly skipped because the carrier is board/docs-only.
