# Bunker release readiness — security patch required

Tick: bunker-releng-2026-11-12-10-01-50. Verification-only satellite; no development or deployments.
Target: deployBunker/bunker, canonical remote origin=git@github.com:deployBunker/bunker.git, default branch main.
Code-certified candidate / existing release: de286fdce69cd92f9f182f3e1eecde34404d713e (v0.3.0).
Pre-write verification tip: f24428aee0466507b31b445376c3ff467965e701; ls-remote, origin/main, checkout HEAD and GitHub commits/main agreed.
The verification tip has no installed-source delta from the tag. Subsequent evidence and board carrier SHAs are separate and are reported in delivery.

## Decision

SECURITY PATCH DUE -> v0.3.1 proposed, NOT CUT. Initial source-delta decision was NOT DUE; the fresh advisory scan supersedes that conclusion. GO-2026-6859 / CVE-2026-97054 was published 2026-11-11T02:51:11Z: HTTP/1 server crash due to MaxBytesReader header race in net/http. Source scan at the verified tip and binary scan of the downloaded v0.3.0 bunkerd both exit 3, finding one reachable stdlib vulnerability. Found-in go1.26.10; fixed-in go1.26.11 or go1.27.4. This is scanner reachability, NOT an exploit reproduction.

RELEASE-024 owns the toolchain remediation, version preparation, validation and handoff. No patch tag may be placed on the present 0.3.0-stamped tree. Exact-SHA full release gates, clean build provenance and notes are required on the eventual patch commit. No major bump is proposed; no breaking markers exist. Patch/minor standing authority is sufficient once green; this lane does not implement the prep.

## Evidence table

| Axis | Result | Receipt |
| --- | --- | --- |
| Range | 57 commits: 4 merges, 53 non-merges | v0.3.0..f24428aee0466507b31b445376c3ff467965e701; record-delimited parser count equals git rev-list --count |
| Conventional prefixes | board=17, chore=2, ci=2, docs=7, nonconventional=15, ops=1, pm=4, prd=1, qa=8 | feat=0, fix=0, perf=0, breaking=0; nonconventional subjects inspected alongside changed paths |
| Installed source | Zero changed paths | Full changed-path set below; explicit exclusions .coding-hermes/, .gitreins/, .github/, docs/, specs/, README.md, CHANGELOG.md. CI/docs changes are support changes, not installed-source features. |
| HEAD hosted CI | successful but NOT full release-grade | run 43102485303, head=f24428aee0466507b31b445376c3ff467965e701; changes/build-and-test/unit-tests/gitreins-guard success; root-suite/regression skipped by board/docs path filter. Cache-hit buf setup/generation skips are intentional, not skipped tests. |
| Existing tag CI | full success | run 42566171520, tag code SHA de286fdce69cd92f9f182f3e1eecde34404d713e; all six jobs success; publisher 42566171652 success on same SHA |
| Repo tooling | present and used | Makefile release-binaries, scripts/pre-release-check.sh, scripts/install.sh, docs/release-checklist.md, .github/workflows/release.yml. Tag workflow owns publication; no manual competing release create. No Dockerfile/compose trigger. |
| Version hygiene | PASS for existing release, unprepared for patch | tree pre-release-check rc=0; --tag v0.3.0 de286fdce69cd92f9f182f3e1eecde34404d713e rc=0; Makefile, internal/version and changelog=0.3.0 |
| Prior artifacts | fresh download VERIFIED | six uploaded assets; SHA256SUMS verifies all five entries; four binaries CGO_ENABLED=0, static, vcs.revision=de286fdce69cd92f9f182f3e1eecde34404d713e, vcs.modified=false; amd64 CLI and daemon execute --version=0.3.0 |
| Source build of tag | PASS | GOTOOLCHAIN=go1.26.10 make release-binaries DIST_DIR=<scratch>/source-assets rc=0, four linux amd64/arm64 CLI+daemon binaries plus installer/checksums; CLI version executes and stamps de286fdc. LOCAL PROVENANCE UNVERIFIED: all four local rebuilds omit vcs.revision/vcs.modified even with explicit GOFLAGS=-buildvcs=true (second make run rc=0). Published assets DO carry correct provenance; local build success is not release-grade provenance. RELEASE-024 must prove this on its clean release build. No assets uploaded; byte identity with published assets is not claimed. |
| Dependencies/security | FAIL on source AND shipped binary | govulncheck ./... rc=3; govulncheck -mode=binary downloaded/bunkerd-linux-amd64 rc=3. GO-2026-6859, net/http@go1.26.10 -> go1.26.11. Three additional module-level advisories are reported as uncalled, not reachable findings. go.mod/go.sum have no delta since tag. |
| GitReins | limited, not fresh tip verdict | a4571472 passed=true, pass_class=judged, commit=b3059dc91f95306fa137323de70dcd31bdeea4c4, task RELEASE-023; dirty_path_count=3, narrow CI-workflow criterion only. No stored verdict found for verified tip/tag. Hosted guard at tag is separate CI evidence. |
| QA | FINDINGS, carried source coverage only | tick bunker-qa-2026-11-11-14-12-38, cycle 3, tested SHA 6c1148694a3c274978cf45b08e00d99c8f8da22f; 24 rows/16 cells, digest 35993cc545454caeba9086d9f227bc01691856eb1e04320eff500f922cda9bd5 rehashed exactly. Independent histogram OK=13 PASS=1 FAIL=1 INFO=3 UNVERIFIED=1 N/A=5. Product paths cmd/internal/pkg/probes unchanged tested SHA..verification tip; this is NOT a fresh exact-tip battery. |
| QA cell details | incomplete | tests: 38 ok/0 FAIL; ci-pass FAIL (act unit-tests, no stdout, QA-BUNKER-094); ci-act UNVERIFIED; lint not run (QA-BUNKER-096/102); upgrade uses second-newest v0.2.0, v0.3.0->HEAD untested (QA-BUNKER-101); errorpath is Cobra help (097); UI not exercised (098); cycle3 disconnect executed/OK; resource PASS; Docker/compose/shutdown/corruption N/A by triggers. |
| Installed local binaries | refreshed, not the old stale premise | ~/.local/bin/bunker and bunkerd both 0.3.0 / 0422dae7 / built 2026-11-11T12:33:23Z / go1.26.10. 20 commits to verification tip, zero cmd/internal/pkg/probes delta. Both still use vulnerable toolchain. Remote live daemon versions NOT re-probed by this lane. |
| Public posture | partial repair verified | secret scanning, push protection, dependabot security updates enabled; automated-security-fixes=true; main protection GET 404; non-provider-patterns and validity checks disabled. RELEASE-022 remains owner-gated. |
| Release notes | FAIL disclosure contract | live body has only two historical PR bullets and contributors/compare link, no Known issues section; RELEASE-025 owns factual mutable-notes repair, never tag movement. |
| Memory | UNVERIFIED | Deferred recall was emitted with empty arguments and returned namespace crier, unrelated to bunker/releases. Routing/serialization leg stopped, no write or claimed readback. |

## Changed paths since tag

- .coding-hermes/board/events.jsonl
- .coding-hermes/board/evidence/DF-BUNKER-118-ops-2026-11-11-14-21-55.md
- .coding-hermes/board/evidence/bunker-qa-2026-11-11-00-26-59-cycle2.json
- .coding-hermes/board/tasks.jsonl
- .coding-hermes/waves/bunker-foreman-2026-11-11-11-43-26.json
- .coding-hermes/waves/bunker-foreman-2026-11-11-14-12-38.json
- .github/workflows/ci.yml
- .gitreins/tasks.yaml
- README.md
- docs/compliance.md
- docs/evidence/RELEASE-readiness-2026-11-11.md
- docs/evidence/RELEASE-v0.3.0-cut-2026-11-09.md
- docs/incident-runbook.md
- docs/ops/fleet-inventory.md
- docs/prd/security-readiness.md
- specs/api.md

## Findings and existing ownership

RELEASE-024 (new P1): security rebuild and v0.3.1 prep. Require source AND four rebuilt-binary vulnerability scans, repo tests/gates, exact-SHA CI with root-suite/regression executed, clean provenance and install smoke; no development done here.
RELEASE-025 (new P2): repair v0.3.0 notes with Known issues near top, accurate categorized changelog and measured residuals. New public claim must name user-visible behavior, workaround/none, and tracking ID; no unmeasured exploit claim.
RELEASE-003 remains canonical fleet rollout row. The owner explicitly approved updates on 2026-11-11 in foreman_note; its blocked_reason still says awaiting an owner-approved window. That premise is stale: reconcile the approval and sequence patched artifacts before further vulnerable rollout. Local stale-title premise refuted by current version probes; remote fleet freshness remains UNVERIFIED, not resolved.
RELEASE-008 is explicitly superseded by RELEASE-003; do not dispatch duplicate work.
RELEASE-022: three toggles fixed; only main-protection policy and two disabled scanning extras remain, not the old all-disabled claim.
RELEASE-023 is COMPLETE with narrow judged verdict a4571472 and cache-first pinned action in five jobs; do not repeat the resolved buf-download blocker.
DF-BUNKER-116 remains pending on live reaper acceptance; no fresh live probe here, so its historical 27GB count is NOT reasserted as current. DF-BUNKER-118 remains pending; prior 99%-disk figure likewise historical, not freshly certified.
DF-BUNKER-47-R2 code arm predates and is inside v0.3.0; only live root E2E acceptance remains. Not a newly confirmed code regression.

## Scope and unblock path

No tags, Release bodies/assets, provider settings, toolchain pins, source, installed binaries or live daemons changed. Verification builds are scratch-only. Foreman implements RELEASE-024, then release lane re-gates and publishes v0.3.1 exactly once through the existing tag workflow. Do not clobber v0.3.0 assets or force-move its tag. RELEASE-025 repairs prose separately. New source scan finding makes the patch due even with zero code delta; old NOT DUE statements are superseded by this decision.
