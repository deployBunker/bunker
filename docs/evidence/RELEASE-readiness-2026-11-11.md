# bunker release-readiness sweep — 2026-11-11

Lane: `bunker-releng` (verify-only). Tick: `bunker-releng-2026-11-11-05-44-30`.
Repo under release management: `deployBunker/bunker` (public, GitHub), resolved from
`git remote -v` at `/home/kara/deployBunker/bunker`, not from the scheduler workdir.

This document is edited in place across passes of the same tick. Pass 4 (below) is the
current authority; earlier passes are preserved in the historical sections and their
mis-statements are labelled where they were corrected.

## Certified candidate

| Field | Value |
| --- | --- |
| Default branch | `main` |
| Certified tip | `c4895284c93cee3327fd1337aded2afe31e59005` |
| Agreement | `git ls-remote origin refs/heads/main` = `c4895284`; local `origin/main` = `c4895284`; local checkout `HEAD` = `c4895284` (only sibling-owned `.gitreins/tasks.yaml` dirty) |
| Last release tag | `v0.3.0` (tag object `880ae8e4`, peeled commit `de286fdce69cd92f9f182f3e1eecde34404d713e`) |
| Release published | 2026-11-09T05:17:25Z, 6 assets |
| Distance from tag | `git describe --tags --always` → `v0.3.0-33-gc4895284` |

## Decision

**NOT DUE.** Census over `v0.3.0..c4895284` = 33 commits, of which 0 `feat`, 0 `fix`,
0 `perf`. Every commit is board/QA/PM/docs bookkeeping. No `BREAKING CHANGE` declaration
and no `type!:` subject exists on the range, so no major proposal is owed either.

A naive `git log --format='%B' | grep -c 'BREAKING CHANGE'` returns 1 on this range. The hit
is prose inside one docs commit body describing the *v0.3.0 cut's* census ("…zero BREAKING
CHANGE"), not a trailer and not a declaration. No commit on the range carries the
`BREAKING CHANGE:` trailer form or a `type!:` subject; the measured count is 0.

The census is confirmed independently of my own parser by `git describe`, which reports
exactly `v0.3.0-33-gc4895284`: 33 commits past the tag, tip `c4895284`.

Diff confirmation of materiality: `git diff --name-only v0.3.0..c4895284` returns six
paths, all non-shipping —
`.coding-hermes/board/events.jsonl`, `.coding-hermes/board/tasks.jsonl`,
`.coding-hermes/board/evidence/bunker-qa-2026-11-11-00-26-59-cycle2.json`,
`docs/evidence/RELEASE-readiness-2026-11-11.md`, `docs/evidence/RELEASE-v0.3.0-cut-2026-11-09.md`,
`docs/ops/fleet-inventory.md`. No installed-surface change exists to earn a patch bump.

## Evidence table

| Axis | Result | Receipt |
| --- | --- | --- |
| Census `v0.3.0..c4895284` | 33 commits, 0 material | `git rev-list --count` = 33; `git describe --tags --always` = `v0.3.0-33-gc4895284`; `grep -cE '^[0-9a-f]+ (feat\|fix\|perf)'` over `git log --format='%h %s'` = 0 |
| CI on certified tip | GREEN (gate scope) | run `42972514220` (CI, push→main) on `c4895284c93cee3327fd1337aded2afe31e59005`; completed success, created 2026-11-11T06:27:07Z. Jobs: `changes` success, `build-and-test` success, `unit-tests` success, `gitreins-guard` success, `root-suite` skipped, `regression` skipped — the two heavy jobs are skipped by the board/docs-only classifier, so this is a gate receipt, not a full clean-machine suite |
| CI on parent tip | GREEN | run `42971129377` on `27f0c2ed` completed success 2026-11-11T06:19:30Z |
| CI at the v0.3.0 cut | GREEN | run `42566171520` (CI) success; run `42566171652` (Release) success on `de286fdc` |
| Repo pre-cut gate | PASS | from a fresh detached worktree at `c4895284`: `sh scripts/pre-release-check.sh` → rc=0 (tree mode); `--tag v0.3.0` → rc=0 (version parity OK, no vendor dir); pre-release-check.sh output: `version parity — internal/version/version.go=0.3.0 CHANGELOG.md=0.3.0 Makefile=0.3.0; latest tag v0.3.0` |
| Repo gate negative control | correctly FAILS | `--tag v0.3.0 c4895284` → rc=1 read unpiped (`FAIL: tag 'v0.3.0' points at de286fdc, not at 'c4895284'`), because the tag is a release point and the tip is 33 commits past it. The gate is not vacuously green |
| Version surfaces vs tag | ALIGNED | `Makefile:22 VERSION ?= 0.3.0`; `internal/version/version.go:25 Version = "0.3.0"`; `README.md` pins `v0.3.0` (lines 274, 348, 1315); `CHANGELOG.md` has empty `## Unreleased` then `## 0.3.0 (2026-11-09)` |
| Release assets | VERIFIED (carried) | all 6 present, published 2026-11-09T05:17:25Z; tag object unchanged (`880ae8e4`), so the earlier `sha256sum -c SHA256SUMS` rc=0 (5/5 OK) still applies to an immutable artifact |
| Released binary self-report | VERIFIED (carried) | `bunker-linux-amd64 --version` → `bunker 0.3.0`, commit `de286fdc`, built 2026-11-09T05:16:34Z |
| Release body | present | 492 chars (categorized notes, not a bare compare link) |
| GitReins verdicts | NONE bound to the tip | `grep -rl c4895284` over `.gitreins/history/` → 0 files; newest history day is 2026-11-09 (`421bb3c3`, `df9271a2`). Absent verdict is UNVERIFIED, not a green, and is not a gate for a not-due decision |
| Dogfood/QA | ticks running, cells partly UNVERIFIED | QA tick `bunker-qa-2026-11-11-00-26-59` cycle 2, 16 cells; grading: toolchain-bootstrap OK, fresh-install OK, tests OK (38 packages), lint UNVERIFIED (`QA-BUNKER-096` runner absent), ci-pass FAIL (local `act` unit-tests rc=1, `QA-BUNKER-094`), ci-act UNVERIFIED, upgrade UNVERIFIED (`QA-BUNKER-101`), docker_build/compose_config/docker-deploy N/A (no Dockerfile). Its hosted-CI field cites run `42837475574` success, but `unit-tests` there runs `-short` with regression and root-suite skipped |
| Deployed binaries | STALE (RELEASE-003 holds) | `~/.local/bin/bunker` and `bunkerd` → 0.2.1 / `bff29c84` / built 2026-11-06T14:02:41Z. Two releases behind (`v0.2.0` 2026-10-02, `v0.3.0` 2026-11-09) |
| Public posture | FAIL (RELEASE-022 holds) | `secret_scanning` disabled, `secret_scanning_push_protection` disabled, `dependabot_security_updates` disabled; vulnerability alerts enabled (`HTTP 204`) |
| Release-gate SPOF | OPEN, wider than recorded (RELEASE-023 holds) | `bufbuild/buf-setup-action@v1` fetched at **5** CI jobs — `ci.yml:118` (`build-and-test`), `:298` (`unit-tests`), `:355` (`gitreins-guard`), `:404` (`regression`), `:528` (`root-suite`) |

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

Because the tip has not moved on the shipped surface since `v0.3.0`, the gap to the tip
is the same magnitude as the gap to the tag. Remediation capacity is now also constrained
by host disk: `DF-BUNKER-118` (P1) records `bunker-las-02` at 99% root FS with 3.8 GiB
free, which blocks staging a new binary on that host. This lane does not install or
restart daemons on live hosts; `RELEASE-003` stays open with these numbers.

## Open blockers carried, not re-filed

Board census: 24 `RELEASE-*` rows — 19 `complete`, 3 `pending`, 1 `blocked`, 1 `closed`.

- `RELEASE-003` (P2, pending) — deployed binary staleness; re-measured, still true.
- `RELEASE-008` (P2, blocked on `RELEASE-003`) — unchanged dependency edge.
- `RELEASE-022` (P2, pending) — public posture hardening; still disabled on all three axes.
- `RELEASE-023` (P2, pending) — external buf download inside the release gate; still present,
  and now measured at 5 job sites rather than the 3 previously recorded.
- `DF-BUNKER-118` (P1, pending) — `bunker-las-02` disk at 99%; blocks `RELEASE-003` remediation.
- `DF-BUNKER-117` (P2, pending) — control host advertises isolation it does not have.

## Release tooling of record (used, not replaced)

The repository ships its own path — `Makefile` (`release-binaries`, `ci`, `lint`,
`docs-check`), `scripts/pre-release-check.sh`, `scripts/install.sh`,
`.github/workflows/ci.yml` + `release.yml`, and `docs/release-checklist.md`. Tag-triggered
`release.yml` owns publication; this lane does not race it with manual `gh release create`.
No Dockerfile and no `.goreleaser*` exist, so the artifact set is the cross-compiled Go
binary matrix produced by `release-binaries`.

## Pass 3 — verification-only re-delivery, with corrections

This tick id was delivered more than once. Pass 1 appended the audit event and committed
the artifact as `e0f31666` while certifying `8f5511f9`; pass 2 committed `27f0c2ed`, which
re-anchored the document to `e0f31666` and thereby left the document one commit behind its
own carrier. Pass 3 re-derives every axis from live git/CI at `27f0c2ed` and lands the
corrections below. It created no `RELEASE-*` row and published no release.

Corrections — each states what the earlier text said, so the record is auditable:

1. **Census arithmetic was wrong in both earlier passes.** This document and the pass-1
   audit event report `v0.3.0..e0f31666` = 32 commits and `v0.3.0..8f5511f9` = 28 commits.
   Measured live: `v0.3.0..8f5511f9` = **30**, `v0.3.0..e0f31666` = **31**,
   `v0.3.0..27f0c2ed` = **32** (`git rev-list --count`, cross-checked with
   `git log --format=%H | wc -l` at each SHA; the three values step by exactly +1 per commit,
   as they must). Canonical figures for this page are now 30 / 31 / 32. The decision is
   unaffected: the material count is 0 at every one of those SHAs.
2. **Certified tip advanced.** Recorded tip `e0f31666` is superseded by `27f0c2ed`, which is
   what `git ls-remote` and `origin/main` both return. CI receipt moved from run `42968620202`
   to run `42971129377`, which is the run whose `headSha` is `27f0c2ed`.
3. **buf setup sites undercounted.** Recorded as 3 jobs (`ci.yml:118,298,355`). Actual: 5
   (`:118` build-and-test, `:298` unit-tests, `:355` gitreins-guard, `:404` regression,
   `:528` root-suite). The SPOF in `RELEASE-023` is broader than its scope statement says;
   the two extra sites are exactly the two jobs the docs-only classifier skips on this
   carrier, so they are dormant-but-present rather than absent.
4. **GitReins line referenced a stale SHA.** Recorded as "no run for `8f5511f9`". Re-stated
   as: no run for `27f0c2ed` or `e0f31666`; newest verdict history is 2026-11-09.
5. **Stale evidence binding.** The pass-1 audit event records `evidence_sha256`
   `a5957cdfe294e6e7c7a857f2bf8979046642bd8cd7dae021224247a3f5a0f74d`, which is the hash at
   `e0f31666`. Pass 2 changed only this file (16 insertions, 11 deletions) and re-pointed no
   digest, so at `27f0c2ed` the tree carries `740bd63b438bbf2a73ab69095e84a349c492bc11942b32a6aecfb44ee63d8ef6`
   against a record that still claims the old one. Pass 3 re-points the binding with a new
   audit event carrying the post-edit digest.
6. **Certification method hardened.** The local checkout at `/home/kara/deployBunker/bunker`
   had `HEAD` at `e0f31666`, one commit behind `origin/main`, so it is not a valid subject
   for exact-tip gates. All pass-3 gates were run in a fresh detached worktree at
   `27f0c2ed`, which is where the `pre-release-check.sh` receipts in the table come from.
7. **Recursion acknowledged.** This pass's carrier commit advances `main` by one docs/board
   commit again. The tip named at the top of this file is the tip the pass measured; the
   commit that contains this text is by construction one commit later and is non-shipping,
   so the NOT DUE decision and the census material count are unchanged by it.

## Pass 4 — same tick re-fired (verification-only, delivery leg)

Trigger: tick `bunker-releng-2026-11-11-05-44-30` fired again. The scheduler records the
previous fire as `failed` at 2026-11-11T01:24:42-05:00 even though its work landed as
`c4895284`, so the leg that was missing was delivery, not measurement. This pass re-measures
at the new tip and then delivers the report the earlier fire did not.

1. **Tip advanced by exactly the lane's own bookkeeping commit.** `27f0c2ed..c4895284` is one
   commit (`docs(release): readiness sweep 2026-11-11 pass 3`). The only non-board path in
   that delta is `docs/evidence/RELEASE-readiness-2026-11-11.md` — documentation, not
   installable surface. `git diff --name-only v0.3.0..c4895284` still returns the same six
   non-shipping paths, so the NOT DUE decision is carried by tree identity rather than by
   assumption, and material count stays 0.
2. **Census re-derived, not carried.** 33 commits over `v0.3.0..c4895284`; 0 `feat`, 0 `fix`,
   0 `perf`; 0 merges / 33 non-merges; no `BREAKING CHANGE:` trailer and no `type!:` subject
   (the single body-level grep hit is the prose described under Decision);
   `git describe` = `v0.3.0-33-gc4895284`.
3. **CI re-measured on the exact SHA.** Run `42972514220`, `headSha`
   `c4895284c93cee3327fd1337aded2afe31e59005`, conclusion success, jobs as tabulated.
   `root-suite` and `regression` are skipped by the docs-only classifier — gate receipt, not a
   clean-machine suite, and not evidence for cutting a release.
4. **Gate re-run in a fresh detached worktree.** Not the stale checkout, not the pass-3
   worktree: `mktemp -d` + `git worktree add --detach … c4895284`, tree clean, tree mode rc=0,
   `--tag v0.3.0` rc=0, negative control rc=1 read unpiped. Version parity line reads
   `internal/version/version.go=0.3.0 CHANGELOG.md=0.3.0 Makefile=0.3.0` — this is the line
   that matters for the next cut.
5. **Tag immutability re-proved.** `refs/tags/v0.3.0` is still annotated object `880ae8e4`
   peeling to `de286fdc`. No tag was created, moved, or force-moved by any pass of this tick.
6. **Deployed binaries unchanged and still stale.** `bunker`/`bunkerd` report `0.2.1` at
   `bff29c84`, built 2026-11-06T14:02:41Z; `git merge-base --is-ancestor bff29c84 v0.3.0` → rc=0,
   so the deployed build predates the `v0.3.0` tag. Two releases behind. `RELEASE-003` holds.
7. **Public posture unchanged.** `secret_scanning` disabled, `secret_scanning_push_protection`
   disabled, `dependabot_security_updates` disabled; `vulnerability-alerts` → HTTP 204
   (enabled). `RELEASE-022` holds.
8. **GitReins still unbound to the tip.** 0 files under `.gitreins/history/` mention
   `c4895284`. Recorded as UNVERIFIED, not as a green.

Unchanged from pass 3: `RELEASE-*` census = 24 rows — 19 `complete`, 3 `pending`
(`RELEASE-003`, `RELEASE-022`, `RELEASE-023`), 1 `blocked` (`RELEASE-008`, on `RELEASE-003`),
1 `closed` (`RELEASE-018`). QA tick `bunker-qa-2026-11-11-00-26-59` cycle 2 remains the newest
graded cycle.

## Carrier

Board annotation: audit event on the rolling `RELEASE-019` row (status left `complete`) plus
an in-place append to its `foreman_note`. No new `RELEASE-*` row was created — the latest
pass is after the tag and the rolling row owns post-cut sweeps.

Each pass whose text changes this file re-points the `evidence_sha256` binding the previous
pass left behind, via a new audit event carrying the post-edit digest. A binding that no
longer matches the file is a broken chain, not a refreshed one.
