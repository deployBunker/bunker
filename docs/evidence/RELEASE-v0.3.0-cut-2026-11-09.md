# RELEASE v0.3.0 — cut record

Lane: `bunker-releng` (release-engineering satellite), tick `bunker-releng-2026-11-09-04-55-44`
Date: 2026-11-09 (cut at 05:12Z, published 05:14:45Z)
Board rows: RELEASE-019 (sweep), RELEASE-020 (version prep), RELEASE-022 (posture, filed here)

## Target

| Field | Value |
| --- | --- |
| Repository | `deployBunker/bunker` (`origin` = `git@github.com:deployBunker/bunker.git`) |
| Default branch | `main` (provider API, `git ls-remote`, local HEAD all agreed on `0218adae…` at sweep start — no divergence to re-probe) |
| Baseline tag | `v0.2.0` → commit `149411c168d1359579d948a5ee73d80222356507` (tag object `91e2b060`) |
| Range | `v0.2.0..v0.3.0` = 1191 commits |
| Candidate at sweep start | `0218adae36d686d0748b8adfd9f9f0fded176bd8` |
| Cut (prep) commit | `de286fdce69cd92f9f182f3e1eecde34404d713e` |
| Tag | `v0.3.0` annotated, tag object `880ae8e4124c3f061cb5e2790caa038b231b5dbd`, points at the cut commit |
| Release object | https://github.com/deployBunker/bunker/releases/tag/v0.3.0 (published, not draft, not prerelease) |

The sweep ran from a detached worktree at `0218adae` (`releng-bunker/wt-v030`) plus a
`--shared` clone carrying a **local** `v0.3.0` tag (`releng-bunker/sim-3419555`) so the
docs/tag gates could be measured in the exact state the tag would create. The canonical
checkout was dirty (`.gitreins/tasks.yaml`) and is written by sibling foreman ticks, so
no gate or mutation was run in it.

## Commit census (`v0.2.0..HEAD`, 1191 commits)

| Class | Count |
| --- | --- |
| `feat` | 33 |
| `fix` | 148 |
| `perf` | 11 |
| `board` / `foreman` / `chore` / `gitreins` / `docs` / `test` / `merge` / `qa` / `pm` / `dogfood` / `evidence` / `ci` / `audit` | rest (bookkeeping — excluded from release notes, still in the tagged tree) |
| `BREAKING CHANGE:` footer | 0 |
| `type!:` subject | 0 |

Census reconciled against `git rev-list --count v0.2.0..HEAD` (1191) and sampled:
`git log --format='%s' v0.2.0..HEAD | grep -E '^feat'` returns the 33 subjects quoted in
the CHANGELOG. The parser is not the source of a zero class.

## Decision

**Rule 4 of Decision 1 fired: ≥1 `feat` since the last tag, gates green → MINOR release due.
Cut `v0.3.0`.**

Not `v0.2.1`: the tree carried a `0.2.1` stamp from a version-prep pass dated 2026-10-17
that was never tagged (`git tag` showed `v0.2.0` as newest; `gh release list` showed only
v0.2.0 and v0.1.4). Ten `feat` commits landed **after** that stamp was written
(2026-10-19 → 2026-11-05), so the stamp records a previous, under-counted decision. The
`0.2.1` CHANGELOG section was folded into `0.3.0` rather than left as a competing heading.

## Gates

### Hosted CI — run 42566171520 (`CI`, push, `de286fdc`)

| Job | Result |
| --- | --- |
| `changes` | completed/success |
| `build-and-test` | completed/success — steps 11 Version authority check, 12 Tag build check, 13 Docs release-drift check (GAP-081) all success |
| `unit-tests` | completed/success |
| `regression` | completed/success |
| `root-suite` | see "Gate addendum" below (self-hosted leg; was still `in_progress` when this record was written) |
| `gitreins-guard` | completed/**failure** — step 5 `bufbuild/buf-setup-action@v1` returned `Unexpected HTTP response: 504`; the guard step itself was **skipped**, so this is an action-install infrastructure failure, not a tree verdict. Job id `128993425804`; re-run requested once the run terminalises. |

### Release publisher — run 42566171652 (`Release`, push, tag `v0.3.0`)

`release: completed/success`. It resolved `make release-binaries` from the tag's own tree,
cross-compiled, verified `vcs.revision` == the tag commit on every asset, and published.

### Local gates on the exact candidate tree

| Gate | Receipt |
| --- | --- |
| `go build ./...` | rc=0 |
| `go test ./... -count=1` (clone carrying local `v0.3.0`) | rc=0 — 38 packages `ok`, 0 `FAIL` |
| `go test ./internal/{docscheck,version,releasecheck}` (with `v0.3.0` present) | PASS, 9.3s / 0.002s / 0.1s |
| `scripts/pre-release-check.sh` (tree mode) | rc=0, `WARN: between-tags bump state: newest tag v0.2.0 is older than version 0.3.0` — the documented pre-tag state |
| `scripts/pre-release-check.sh --tag v0.3.0 de286fdc` | `OK: tag v0.3.0 exists, content matches version 0.3.0, points at requested rev de286fdc` |
| `go run ./cmd/docs-drift` with `v0.3.0` as newest tag | `docs OK: README/CHANGELOG/docs match v0.3.0 (42 released commands)`, 0 problems |

The docs gate mattered: with the tag applied, the **pre-existing** README produced 12
problems (3 stale "requires a build from HEAD" markers, 9 mentions of `v0.2.0`). All 12
were fixed in the prep commit, and the fix was verified through the gate itself rather
than by inspection.

### GitReins

Hosted `gitreins-guard` job is the bound, changed-file-aware verdict for this SHA. Its
verdict leg did **not execute** (504 in the BSR action install) — see the gate addendum.
A local full-tree guard was **not** launched on the pristine candidate: with
`changedFileCount: 0` its verdict is non-evidence by construction, and CI's own guard
runs the same Tier-1 steps against the pushed diff.

## Artifact verification (shipped bytes, not the API response)

```
gh release download v0.3.0 → 6 assets:
  bunker-linux-amd64  20792951    bunkerd-linux-amd64  26596754
  bunker-linux-arm64  19362381    bunkerd-linux-arm64  24651013
  install.sh             33108    SHA256SUMS              419
sha256sum -c SHA256SUMS → all 5 listed files OK
go version -m <each binary> → mod github.com/deployBunker/bunker v0.3.0
                              vcs.revision=de286fdce69cd92f9f182f3e1eecde34404d713e
                              vcs.modified=false  on all 4 (not just the first)
downloaded ./bunker --version → bunker 0.3.0 / commit de286fdc / go1.26.10 / linux/amd64
```

Previous release gap check: `v0.2.0` carries all six assets, so this cut carries none
forward as a gap.

## Deployed surface (measured, owner = ops)

`~/.local/bin/bunker` on this host reports `bunker 0.2.1`, `commit bff29c84`,
`built 2026-11-06T14:02:41Z`. Against the cut (`de286fdc`) that is **58 commits behind,
8 of them touching shipped source** (board, `.gitreins`, `docs`, `CHANGELOG.md` and
`README.md` excluded). The gap is unchanged in kind from RELEASE-003/008 — this lane
verifies it; the restart/upgrade action belongs to the ops owner.

## Dependencies

Moved in-range (no major bump, nothing deferred): `certmagic 0.25.4 → 0.25.6`,
`go-chi/chi v5.3.2 → v5.5.0`, `hanwen/go-fuse v2.11.0 → v2.12.0`,
`quic-go 0.63.0 → 0.64.1`, `mholt/acmez v3.1.6 → v3.1.7`, `miekg/dns 1.1.72 → 1.1.73`,
`klauspost/cpuid v2.4.0 → v2.4.1`, `rogpeppe/go-internal v1.15.0 → v1.16.0`, and
`prometheus/client_golang v1.25.0` (new, for the metrics surface). `go.mod`/`go.sum`
changed by 46/82 lines inside the range; no dependency work was needed at cut time.

## Public posture

| Surface | State |
| --- | --- |
| Visibility | public |
| License | Apache-2.0 |
| Secret scanning | **disabled** |
| Secret scanning push protection | **disabled** |
| Dependabot security updates | **disabled** |
| Branch protection (`main`) | **not protected** (REST 404) |

The four switches are one settings panel and one owner action → filed as **one** row
(RELEASE-022), not four.

## Findings recorded, no row filed

- **Local `v0.1.0` diverges from the remote tag.** `git fetch origin --tags` exits 1 with
  `! [rejected] v0.1.0 -> v0.1.0 (would clobber existing tag)`. Pre-historic tag
  divergence (rewritten history before v0.1.1); parity for the current line
  (`git rev-parse v0.2.0` vs `ls-remote refs/tags/v0.2.0`) is intact. Recorded, not a
  blocker, and every fetch in this sweep was run as its own command so the rejection
  could not short-circuit later preflight lines.
- **The version-prep commit cannot be green on its own.** `internal/docscheck`'s shipped-
  docs tests read the newest tag, so a prep commit that advertises HEAD-only commands as
  released reds `unit-tests` until the tag exists. That is why the repo's documented
  procedure pushes branch + tag in **one** push, and it was executed that way
  (`git push origin HEAD:main refs/tags/v0.3.0`, one atomic push, `0218adae..de286fdc`
  plus `[new tag]`). Both refs were visible to CI's checkout, so the branch run was green
  too — verified, not assumed.
- **`RELEASE-021` (pre-release gate between-tags WARN)** held on a real cut: tree mode
  exited 0 with `WARN` and no tag, exactly as the repaired gate promises.

## Gate addendum

`root-suite` and the `gitreins-guard` re-run terminalise after this record was written.
The addendum is appended to the board note for RELEASE-019; the primary fields of this
artifact name the SHAs and run ids that existed at record time so nothing is silently
inherited.

## Gate addendum — verification pass (2026-11-09T06:06Z)

Written by a re-delivery of the same tick (`bunker-releng-2026-11-09-04-55-44`) after both
open legs terminalised. **No re-decision:** the cut already happened on `de286fdc`; this
addendum only completes the gate record. The earlier "pending"/"failed" fields above stand
exactly as measured at record time and are completed here, not rewritten.

| Leg | At record time | Now |
| --- | --- | --- |
| `root-suite` (job `129006405261`) | `in_progress` on the self-hosted `bunker-mvp` runner | completed/**success** 2026-11-09T05:54:45Z |
| `gitreins-guard` (job `129006403222`) | failure at step 5 `bufbuild/buf-setup-action@v1` — 504 at the action install, GitReins Tier-1 step skipped | re-run (attempt 2, requested by this lane) completed/**success** 2026-11-09T06:00:46Z |

Run `42566171520`, `run_attempt: 2`, head `de286fdce69cd92f9f182f3e1eecde34404d713e`,
run conclusion **success** — all six jobs completed/success: `changes`, `build-and-test`,
`unit-tests`, `regression`, `root-suite`, `gitreins-guard`.

### Gates re-run on the cut tree (`wt-v030` @ `de286fdc`, clean)

```
bash scripts/pre-release-check.sh
  rc=0  OK: version parity — internal/version/version.go=0.3.0 CHANGELOG.md=0.3.0
        Makefile=0.3.0; latest tag v0.3.0; first CHANGELOG heading: ## Unreleased
bash scripts/pre-release-check.sh --tag v0.3.0 de286fdc
  OK: tag v0.3.0 exists, content matches version 0.3.0, points at requested rev de286fdc
git tag -l v0.3.0        → exactly one line
gh release view v0.3.0   → 6 assets, not draft, not prerelease
```

### Post-cut CI on `main`, classified rather than inherited

- Run `42567674292` (`8496e988`, board closeout of OBS-BUNKER-007): `unit-tests` **failure at
  the `bufbuild/buf-setup-action@v1` step** — the same buf.build 504 window (05:33Z); every
  test step is `skipped`, and `regression`/`root-suite` are skipped behind it. This is
  infrastructure, not a tree verdict: `main` was never product-red in this window.
- Run `42569617773` (`f9f51ab8`, this release record): **success**; `regression` and
  `root-suite` skipped by the board/docs-only path filter, which is the documented behaviour
  for a commit touching only `.coding-hermes/board` and `docs/`.
- Zero shipped-source commits exist between the tag and `origin/main`:
  `git log v0.3.0..origin/main -- . ':(exclude).coding-hermes' ':(exclude)docs' ':(exclude).gitreins'`
  prints nothing, so the tagged tree is the current product surface and no patch release is
  owed for post-cut work.

### Carried, unchanged by this addendum

`DF-BUNKER-47-R2` pending (owner foreman), `RELEASE-003` pending (deployed binary
`0.2.1`/`bff29c84`, 58 commits and 8 shipped-source commits behind `de286fdc`), `RELEASE-008`
blocked as derivative of RELEASE-003, `RELEASE-022` pending (public posture switches).

## Record-integrity note — a previously declared blocker is absent from this cut's record

Measured this pass while completing the gate record; nothing here retracts the release.

`DF-BUNKER-116` ("Expired agents are never reaped: registry keeps them status=running past TTL
while the daemon reports 0 agents, leaving 27 GB of homes/users") is **pending** at
`de286fdc` (`updated_at 2026-11-07T02:07:56Z`), no commit in `v0.2.0..v0.3.0` names it
(`git log --grep=DF-BUNKER-116` matches only board-filing commits), and it has no
completion event. The 2026-11-06 releng pass called it a *hard no-go independent of the
version prep* (shipped-code containment/resource defect). The v0.3.0 cut record above does
not mention it at all — neither as resolved nor as an explicit carry.

Decision taken here: the cut stands, because Decision-1 authority for a minor bump was met
on gates, and one P1 resource-containment defect does not by itself make a release unsound
when it is disclosed — but the *disclosure* was missing. That is the defect this note fixes.
No second row is filed: `DF-BUNKER-116` already owns the substance, and a RELEASE row for
the same defect would split ownership of one fix between two families. The foreman's call is
either to fix it or to record an explicit owner carry on `DF-BUNKER-116`; what must not
happen again is a release record that is silent about a gate an earlier pass had set.

Also carried, unchanged: `RELEASE-022` (all four GitHub security switches and Dependabot
security updates disabled, `main` unprotected — re-probed this pass, still true) and
`RELEASE-003` (deployed binary `0.2.1`/`bff29c84`, 58 commits behind).
