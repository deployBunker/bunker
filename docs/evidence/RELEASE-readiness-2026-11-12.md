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

## Pass 2 (re-fire of the same tick, delta only)

Tick bunker-releng-2026-11-12-10-01-50 fired a second time at 2026-11-12T10:13:38Z. Pass 1 (artifact commit bace0272, rows commit 4a56e78b) is not re-decided here; only what moved, plus the legs that must be re-derived every pass (live advisory, CI truth, release surface, fleet freshness).

### Movement and re-anchor

Range f24428ae..4a56e78b = 2 commits (bace0272 docs/releng evidence, 4a56e78b board rows). Changed paths: .coding-hermes/board/events.jsonl, .coding-hermes/board/tasks.jsonl, docs/evidence/RELEASE-readiness-2026-11-12.md. Material (installed-source) paths: 0.

Code-certified candidate is unchanged at de286fdce69cd92f9f182f3e1eecde34404d713e (v0.3.0). Verification tip is now 4a56e78b668b64be2b67ec316055a7f2427943a5, which equals ls-remote origin/main and the checkout's origin/main.

### What changed materially since pass 1

The upstream fix is now published and proven reachable. RELEASE-024 is no longer blocked on the Go project.

- go.dev/dl release index lists go1.26.11 and go1.27.4 (both on the 1.26 and 1.27 lines).
- GOTOOLCHAIN=go1.26.11 go version → go version go1.26.11 linux/amd64 (toolchain actually fetched and ran).
- GOTOOLCHAIN=go1.26.11 govulncheck ./... at 4a56e78b → exit 0, "No vulnerabilities found", 0 reachable findings. The same three module-level advisories remain uncalled, exactly as at the tag.

So the patch needs NO source change: one toolchain pin bump clears the reachable finding.

The pin surface is single-source, which is why that is enough: go.mod:3 says `go 1.26.5` and go.mod:5 says `toolchain go1.26.10`, and every build consumer resolves from that file — ci.yml:116, ci.yml:298, ci.yml:408, ci.yml:534 and release.yml:80 all use `go-version-file: "go.mod"`. There is no per-workflow version literal left to chase (a gitreins task already pins that: .gitreins/tasks.yaml:1345-1346).

### Fleet freshness — now measured, replacing pass 1's UNVERIFIED

`bunker status --all` with the installed CLI exits 2. Thirteen daemons answered:

| Version | Count | Hosts of note |
| --- | --- | --- |
| 0.1.3 | 1 | bunker-las-04, FAILED BELOW_FLOOR (below the 0.1.4 floor) |
| 0.1.4 | 7 | bunker-las-02 OK |
| 0.2.1 | 5 | bunker-las-03, bunker-mvp OK |

Two more hosts did not yield a version: cube-las-00 is UNREACHABLE (registered at http://100.84.195.43:18080, no answer), and hermes-fleet-01 was refused before dialling because its stored cert_pin is 62 hex characters, not 64 (`bunker connect --tls self-signed --accept-cert` re-pin required). One host also reports a port discrepancy (registry :19090 vs declared 10001/10002) carried from the documented las-01 class of defect.

Zero hosts run 0.3.0. That corrects the useful part of the staleness picture and fixes the sequencing rule: the fleet's first contact with a patched artifact must be v0.3.1 or later, never v0.3.0, so RELEASE-003 rollout and RELEASE-024 rebuild are one sequence, not two independent tasks.

### Explicitly unchanged surface (re-verified this pass)

| Axis | Result | Receipt |
| --- | --- | --- |
| Decision | SECURITY PATCH DUE → v0.3.1 proposed, NOT CUT | advisory leg re-run; no tag created, no Release created |
| Live advisory (source) | rc=3 at 4a56e78b | GO-2026-6859 ("HTTP/1 server crash due to MaxBytesReader header race in net/http"), found net/http@go1.26.10, fixed net/http@go1.26.11, 60 traced call stacks (#1-#60; server.Run, davserve.Serve, webdav.OptionsAsterisk, fsclient.OpError.Error, ...), 0 callable findings in imported packages, 3 uncalled module advisories |
| Live advisory (official binary) | rc=3 | freshly downloaded bunkerd-linux-amd64, sha256 2bc9cdc3cfda5e78aa22f325fe1046a3b858c7f41d8db394aa45c0dfa45d7e20 == SHA256SUMS; 85 vulnerable symbols (5 listed, 80 behind -show traces), same 3 uncalled module advisories |
| HEAD hosted CI | green but still NOT release-grade | run 43198098463, head=4a56e78b, completed/success 10:15:07Z; changes, build-and-test, gitreins-guard, unit-tests success; root-suite and regression SKIPPED by path filter. Preceding docs commit: run 43197947383 success. |
| Tag-code CI (historical full gate) | all six jobs success | run 42566171520 on de286fdce69cd92f9f182f3e1eecde34404d713e; gitreins-guard, build-and-test, root-suite, unit-tests, changes, regression all success |
| Release surface | unchanged, notes still deficient | v0.3.0 published 2026-11-09T05:17:25Z, 6 assets (both CLI, both daemon, install.sh, SHA256SUMS); body still only 2 historical PR bullets + New Contributors + compare link, no Known issues → RELEASE-025 premise live |
| Tags | no movement | 7 remote tag refs, v0.3.0 still latest; v0.3.0 local commit de286fdc == remote peeled de286fdc |
| Local installed binaries | version fresh, security stale | ~/.local/bin/bunker and bunkerd both 0.3.0 / 0422dae7 / built 2026-11-11T12:33:23Z (bunker also prints go version go1.26.10; bunkerd prints version, commit, caps isolation-grant). RELEASE-003's "local axis fixed and verified" claim holds for version only; the security axis moves to RELEASE-024. |
| Board rows | both still pending, now dispatchable | RELEASE-024 and RELEASE-025 pending, unassigned, acceptance_criteria populated, foreman_note carries the pass-2 receipt |
| Foreman concurrency | unrelated wave in flight | bunker-foreman-2026-11-12-09-53-30 at base f24428ae dispatching GAP-065/066/067 only; no RELEASE row claimed |

### Collision note

The shared checkout /home/kara/deployBunker/bunker was dirty with that foreman wave (modified .coding-hermes/board/{events,tasks}.jsonl, .gitreins/tasks.yaml, untracked wave file). This pass did not write there. All pass-2 writes were made in a detached worktree at 4a56e78b and pushed to main, so the in-flight wave copy cannot carry a stale releng edit. If a sibling wave commit rewrites board rows from a pre-4a56e78b copy, the canonical record for this pass is still this file plus the audit event; re-apply the RELEASE-024 foreman_note if it reverts.

### Standing decision and unblock path (restated, unchanged)

No tag, no Release edit, no provider change, no toolchain pin, no source, no installed binary and no live daemon was modified by this lane. Foreman implements RELEASE-024 (single go.mod toolchain bump to >=1.26.11), then this lane re-gates and publishes v0.3.1 exactly once through the existing tag workflow, with root-suite and regression executed at the exact prep SHA. RELEASE-025 repairs the published prose separately. Patch authority is sufficient; no major bump and no owner ruling is required, and no red-CI override or force-moved tag is permissible.
