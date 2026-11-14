# Bunker release readiness — security patch required

Tick: bunker-releng-2026-11-14-11-02-16. Verification-only satellite; no development or deployments.
Target: deployBunker/bunker, canonical remote origin=git@github.com:deployBunker/bunker.git, default branch main.
Code-certified candidate: 50c73cc0c17634d18f180b83a1444f3d9a4f4210. Existing release baseline: de286fdce69cd92f9f182f3e1eecde34404d713e (v0.3.0). Pass 5 below supersedes the prior candidate and records the current exact-SHA red gate. Historical passes are retained.
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


## Pass 3 — material movement and committed board corruption

Observed at 2026-11-12T10:46:33.673221+00:00. This is a re-fire of bunker-releng-2026-11-12-10-01-50, not a second release. It supersedes Pass 2's unchanged-code/patch-only conclusion for main.

Certified code SHA: ca927ae9ad4c4f5cb8af66d401812ee73bd57ce8; origin/main, ls-remote and detached final-wt HEAD agree. Baseline v0.3.0 peels to de286fdce69cd92f9f182f3e1eecde34404d713e; annotated object 880ae8e4124c3f061cb5e2790caa038b231b5dbd remains unchanged.

DECISION: NO CUT. Main now proposes v0.4.0 by feat -> minor, not v0.3.1; no breaking marker or major proposal. RELEASE-024 remains the security-remediation owner; reconcile its patch-specific version acceptance before a main-line cut. Do not silently ship features under its old patch proposal. A separate minimal security hotfix would need the release-branch exception and fresh gates, and is not created here.

| Axis | Fresh result |
| --- | --- |
| Commit census | 65 commits, 5 merges, 60 non-merges. Prefixes: {"board": 18, "chore": 2, "ci": 2, "docs": 8, "feat": 2, "foreman": 1, "nonconventional": 16, "ops": 1, "pm": 4, "prd": 1, "qa": 8, "releng": 2}. Sum equals git rev-list --count. feat=2, fix=0, perf=0, breaking=0. |
| Materiality | 68bc13b8 GAP-065 adds container command scaffolding but explicitly is not wired into spawn. ca927ae9 GAP-067 changes the live containment disclosure path. Do not advertise GAP-065 as completed container lifecycle. Product delta: internal/agent/container_mode.go, internal/agent/container_mode_test.go, internal/server/gap075_test.go, internal/server/service.go. |
| Exact-SHA hosted CI | Run 43202833421: changes=success, build-and-test=success, root-suite=queued, unit-tests=in_progress, regression=queued, gitreins-guard=success. Not terminal green; do not borrow parent run 43202076932 or historical tag CI. |
| Board gate | FAIL: committed events.jsonl contains <<<<<<< Updated upstream at line 1690, ======= at 1695, >>>>>>> Stashed changes at 1699. Introduced in 488197ee. jq parse fails at 1690; boardctl validate exits 1 with 1 error / 1002 inherited warnings. Both conflict arms carry distinct events with colliding numeric ids 6065-6067 suffixes. Preserve both arms and repair ids through the foreman; no board cleanup performed here. |
| Security | Fresh source scan at 181a5817 and official v0.3.0 daemon scan each rc=3: GO-2026-6859, net/http go1.26.10 -> go1.26.11. ca927ae9 changes service disclosure, not go.mod/go.sum; security scan is carried across that named source delta, NOT a fresh exact-ca927 scan. No exploit reproduction claimed. |
| Version/tooling | Still 0.3.0 and go1.26.10. Repo pre-release-check tree mode rc=0 at 181a5817; no version prep for a new cut. Makefile release-binaries and release-checklist own builds; release.yml owns publication. No tags/publisher invoked. |
| Artifacts | Fresh v0.3.0 download: all 5 SHA256SUMS entries OK (four binaries + installer); six assets present, no replacements. Published daemon embeds the baseline SHA and go1.26.10. No new release cross-build: security/version/CI/board gates already block it; prior tag source build is Pass 1 evidence only. |
| Dependencies | go.mod/go.sum delta since tag empty; current security advisory remains a live blocker. |
| GitReins | No stored exact-candidate verdict found in canonical history; hosted gitreins-guard succeeds on candidate. A hosted guard is not an exact-candidate tier-2 pass. Concurrent foreman judge activity observed; no competing judge launched. |
| QA/dogfood | Prior cycle 3 evidence rehashed 35993cc545454caeba9086d9f227bc01691856eb1e04320eff500f922cda9bd5; 24 rows, statuses {"FAIL": 1, "INFO": 3, "N/A": 5, "OK": 13, "PASS": 1, "UNVERIFIED": 1}. FINDINGS, not green. Tested 6c114869; four product paths changed since then, so its source coverage does NOT certify this candidate. Lint/upgrade/UI/errorpath/act residuals remain explicitly unverified per existing rows. |
| Deployed staleness | Local CLI freshly executes 0.3.0 / 0422dae7 / go1.26.10; it lacks both new candidate changes and fixed toolchain. Remote fleet counts are historical Pass 2 measurements, not re-probed in this pass. RELEASE-003 owns rollout; use patched artifacts, not vulnerable v0.3.0. |
| Notes | Live v0.3.0 body still lacks Known issues and contains only two PR bullets. RELEASE-025 remains pending; no notes edit performed by verify-only lane. |
| Memory | UNVERIFIED: outbound deferred recall had empty arguments and returned unrelated crier namespace. Leg stopped; no memory write claimed. |

Unblock order: repair committed board conflict markers/colliding ids; complete RELEASE-024 fixed-toolchain and version prep reconciled with main's minor bump; finish exact-prep-SHA CI including root-suite/regression; obtain non-vacuous QA/verdict evidence; build four clean static/provenance-verified artifacts; publish once through tag workflow and verify assets/tag CI. RELEASE-025 owns existing notes repair. Shared checkout/foreman changes, tags, assets, toolchain, binaries and daemons were not mutated.

## Pass 4 — board gate repaired and independently verified; materiality re-derived; NO CUT

Re-fired tick, verification-only, observed 2026-11-12T10:57:05Z. This pass supersedes Pass 3's board-integrity finding and Pass 3's patch-only bump literal. The NO-CUT decision is unchanged.

| Area | Finding |
| --- | --- |
| Pre-write tip | fd2fc75c2b0314acbcb04f843a62cbc2cd7a1da7 (origin/main, checkout HEAD and ls-remote agreed). Diff be033243..fd2fc75c is `.coding-hermes/board/events.jsonl` +1 line — bookkeeping only, so the code-certified candidate stays ca927ae9ad4c4f5cb8af66d401812ee73bd57ce8. |
| Board gate | PASS — repaired by bc088d39 and verified here at fd2fc75c: events.jsonl 1697 lines, 0 lines matching `^(<<<<<<<|=======|>>>>>>>)`, `jq -c .` rc=0 on both board files, and `boardctl validate -C .coding-hermes/board` rc=0 (915 tasks, 1697 events, 3 fixtures, header parsed; 1002 inherited warnings — the exact baseline RELEASE-026 AC2 permits). RELEASE-026 AC1+AC2 are satisfied. AC3 is NOT: no conflict-marker detection exists in `scripts/board-jsonl-guard.sh`, any `scripts/*.sh`, or any `.github/workflows/*.yml`. |
| Candidate CI | 43202833421 on ca927ae9 is NOT terminal: changes, build-and-test, unit-tests, gitreins-guard = completed/success; root-suite and regression = queued, never started. Cause measured, not guessed: the repo registers exactly one self-hosted runner (bunker-mvp, online, busy=true), and runs 43202076932 / 43202833421 / 43203568771 / 43204029531 are stacked behind it. A queued job is unproven; no green is borrowed from a sibling SHA. |
| GitReins | No canonical verdict binds ca927ae9. The GAP-067 tier-2 record `.gitreins/history/2026-11-12/d17bbbcf/verdict.json` is passed=false / merge_recommendation=changes_required on worker commit 0325d3c6, evaluated 2026-11-12T10:51:05Z, with no superseding entry. GAP-067's board row is still pending. Release consequence: code that a judge refused is on main and would ship in the next cut. |
| Materiality / bump | Re-derived, not rehashed. `containerModeRunArgs` (GAP-065) is defined at internal/agent/container_mode.go:12 and has zero non-test references repo-wide — scaffolding, no behavior change. The only behavior delta since v0.3.0 is GAP-067's containment-disclosure path (internal/server/service.go, default off). feat -> minor: next main-line release is v0.4.0, not v0.3.1. Zero subject `!`; the two commit bodies containing the literal "BREAKING" (680f89c0, d8bf5e2b) are releng/docs commits quoting policy, not change markers. |
| Census | 65 commits v0.3.0..ca927ae9 (5 merges, 60 non-merges). Non-merge prefixes: board 18, nonconventional 11, qa 8, docs 8, pm 4, releng 2, feat 2, ci 2, chore 2, prd 1, ops 1, foreman 1 = 60. |
| Installed-source delta | 4 files, +93/-10: internal/agent/container_mode.go, internal/agent/container_mode_test.go, internal/server/gap075_test.go, internal/server/service.go. |
| Deployed staleness | Re-probed live this pass: `bunker status --all` -> 12 ONLINE, 3 OFFLINE (bunker-las-01, bunker-las-03, cube-las-00). Versions: 0.1.4 x7, 0.2.1 x4, 0.1.3 x1, 0.3.0 x0. No daemon runs the current release, every answering daemon embeds go1.26.10 and is therefore still exposed to GO-2026-6859; bunker-las-04 (0.1.3) is two releases behind. Patch-bump-within-line (go1.26.10 -> go1.26.11) does not clear the advisory, so RELEASE-003's patch arms are deprecated and rollout must ride the rebuild (RELEASE-008). |
| Version surfaces | Unchanged at 0.3.0 (internal/version/version.go:25, Makefile:22, CHANGELOG top entry 0.3.0, go.mod toolchain go1.26.10). No prep exists; nothing to certify. |
| Release surface | v0.3.0 unchanged: 6 assets, 7 tag refs on origin, no v0.4.0 tag, published body still lacks the Known-issues section (RELEASE-025). |
| Memory | No ledger claim made this pass; the earlier deferred-recall leg remains UNVERIFIED rather than retried into a false confirmation. |

Release decision: NO CUT — unchanged, and now blocked by one fewer thing than Pass 3 but by two things Pass 3 did not name. Gates, in order: (1) RELEASE-024 retargeted from 0.3.1 to 0.4.0 — fixed toolchain, five version surfaces, categorized notes; (2) RELEASE-027 ship-or-hold for GAP-067, whose judge verdict is changes_required while its code sits on main; (3) exact-candidate CI terminal green with root-suite AND regression executed on ca927ae9 (or its replacement), citing the run; (4) RELEASE-026 AC3's regression guard; (5) RELEASE-025 published-notes repair; (6) RELEASE-003/008 fleet rollout with fixed artifacts. This verification lane created no tag, no release, no asset, no toolchain change and touched no daemon.
# Bunker release-readiness sweep — 2026-11-14

Tick: bunker-releng-2026-11-14-11-02-16. Verification-only satellite; no release mutation performed.
Target: deployBunker/bunker; remote: git@github.com:deployBunker/bunker.git; forge: GitHub; default branch: main.
Certified candidate: 50c73cc0c17634d18f180b83a1444f3d9a4f4210.
Baseline: v0.3.0, peeled commit de286fdce69cd92f9f182f3e1eecde34404d713e.

## Decision

NO CUT. The candidate has a material minor bump (2 feat commits), but exact-head CI is red (run 43550985552, unit-tests failure), the reachable Go standard-library vulnerability remains present (GO-2026-6859, go1.26.10 fixed in go1.26.11), GAP-067 has no passing exact-candidate GitReins verdict and its latest stored verdict is changes_required on worker commit 0325d3c6, and release blockers RELEASE-024, RELEASE-025, RELEASE-027 and RELEASE-028 remain pending. No tag, Release object, asset, or source was changed.

## Known issues

- GO-2026-6859 is reachable through net/http in the current toolchain; fixed-toolchain rebuild and v0.4.0 prep are owned by RELEASE-024.
- Exact-head CI run 43550985552 is red because unit-tests failed; regression and root-suite were skipped, so no full release gate exists.
- GAP-067 containment disclosure is present on main but its latest GitReins judge is INCOMPLETE/changes_required because live bunker-mvp E2E evidence is absent; RELEASE-027 owns ship-or-hold.
- Deployed daemon `/opt/bunker/bunkerd` still reports 0.2.1 at bff29c84/go1.26.10; local CLI is 0.3.0 at 0422dae7/go1.26.10. Fleet rollout remains RELEASE-003.

## Evidence table

| Axis | Result | Receipt |
| --- | --- | --- |
| Candidate/parity | PASS | origin/main, local HEAD and ls-remote agree at 50c73cc0c17634d18f180b83a1444f3d9a4f4210; clean detached probe worktree |
| Range | 94 non-merge commits | v0.3.0..50c73cc0c17634d18f180b83a1444f3d9a4f4210; `git rev-list --no-merges --count` = 94 |
| Bump | MINOR proposal: v0.4.0 | 2 feat, 1 fix, 7 perf, 0 breaking markers; features ca927ae9 (GAP-067) and 68bc13b8 (GAP-065 scaffolding; zero non-test references) |
| CI | RED / release-blocking | run 43550985552, exact head SHA; build-and-test, changes, gitreins-guard success; unit-tests failure; regression/root-suite skipped |
| Release surface | intact | v0.3.0 tag object 880ae8e4124c3f061cb5e2790caa038b231b5dbd peels to de286fdce69cd92f9f182f3e1eecde34404d713e; GitHub Release has 6 assets |
| GitReins | UNVERIFIED exact tip; GAP-067 latest FAIL | no verdict bound to 50c73cc0; latest GAP-067 verdict `.gitreins/history/2026-11-12/d17bbbcf/verdict.json`, worker 0325d3c6, incomplete/changes_required |
| Dogfood/QA | NOT GREEN | pending DOGFOOD-015/016/017; latest clean-machine evidence has product/harness residuals, including QA-BUNKER-108 and QA-BUNKER-109; no fresh exact-tip full battery |
| Version hygiene | current-release PASS, next-cut UNPREPARED | pre-release-check.sh rc=0 for 0.3.0; all version surfaces still 0.3.0; no v0.4.0 prep |
| Security | FAIL | `govulncheck ./...` rc=3: GO-2026-6859, net/http@go1.26.10 -> go1.26.11; 0 reachable module findings and 3 uncalled module advisories |
| Tooling | repository-owned path present | Makefile `release-binaries`, `scripts/pre-release-check.sh`, `docs/release-checklist.md`, tag-triggered `.github/workflows/release.yml`; Go cross-compile path used by future cut; no Dockerfile/goreleaser |
| Deployed staleness | OPEN | local CLI 0.3.0/0422dae7; daemon 0.2.1/bff29c84; no live deployment performed |
| Public notes | deficient | live v0.3.0 Release body lacks `Known issues` and only has two historical PR bullets; RELEASE-025 owns repair |
| Board | durable row state already present | RELEASE-024/025/026/027/028 are pending on origin/main; boardctl validate rc=0 with 1004 legacy warnings |

## Commit categories

- feat (2): ca927ae9ad4c4f5cb8af66d401812ee73bd57ce8 feat(bunker): containment disclosure marker is final stdout line (GAP-067); 68bc13b895921cd3aeafc43f0d83c7ccee61772a feat(bunker): container-mode command builder scaffolding (GAP-065).
- fix (1): 02eb50d42dbb8ee591facd93fcac9d6bf8ac3502 fix(qa): wire collect evidence path.
- perf (7): PERF-021 measurement/records-only commits, no new shipped behavior established.
- Other conventional/non-conventional: 73 conventional and 11 non-conventional of the 94 non-merge range; subjects are mostly board, QA, docs, PM, releng and foreman bookkeeping. Full categorized subject census is in the pass scratch receipt.
- No `BREAKING CHANGE:` footer or `type!:` subject was found. Major confirmation is not applicable.

## Tooling and release path

The repo-owned tag workflow triggers on v* tags and publishes six assets from the tag tree via `make release-binaries`: bunker/bunkerd for linux amd64/arm64, SHA256SUMS and install.sh. Because exact-head CI is red, the release path was not executed and no tag was created.

## Required next gates

1. Foreman completes RELEASE-024: pin Go >=1.26.11, prepare all five 0.4.0 version surfaces, rebuild and scan all four binaries, and obtain exact-prep-SHA CI with regression/root-suite executed.
2. Resolve RELEASE-027 ship-or-hold for GAP-067 and obtain a passing exact-tree verdict/live evidence if shipping it.
3. Resolve RELEASE-028 runner capacity and rerun the full exact-candidate gate; do not borrow sibling SHA results.
4. Repair RELEASE-025 release notes and reconcile RELEASE-003 fleet rollout only after fixed artifacts exist.

No tag, Release, asset, toolchain, daemon, or board row was mutated by this pass.

## Pass 5 — exact-candidate re-anchor; NO CUT

This pass is authoritative for the 2026-11-14 candidate. The standalone scratch artifact hash was ced8255ce966ac2cf5c425ffd73e17aa492748366f83414a7c300cc1ae26e4e4 before repository insertion; the committed repository blob is the digest to bind after commit.

## Pass 6 — `ad0c3634` re-certification after carrier advance; NO CUT

Tick `bunker-releng-2026-11-14-11-02-16`, 2026-11-14T11:45Z. Candidate is the
origin/main tip `ad0c3634`.

The product tree is **identical** to Pass 5's candidate `9449f9bc`:
`git diff --name-only 9449f9bc ad0c3634`, filtered to exclude `.coding-hermes/`,
`.gitreins/`, `docs/`, `README.md` and `CHANGELOG.md`, is **0 paths**. This pass
therefore certifies a new CI run on the same product source, not a new tree.

### Exact-head CI — run `43591046880`, terminal failure

| job | conclusion |
|---|---|
| changes | success |
| build-and-test | success |
| unit-tests | **failure** |
| gitreins-guard | success |
| regression | skipped (path-filtered) |
| root-suite | skipped (path-filtered) |

Exactly one failing test:

```
--- FAIL: TestBuildAgentPersistentExecCommand_EnsureThenExecInsideContainer (0.40s)
    exec_persistent_test.go:164: built remote command failed: exit status 1
        compile: version "go1.26.10" does not match go tool version "go1.26.11"
```

### Controlled pair — same product source, two runs, different failure sets

| run | commit | unit-tests | GAP118 io-bandwidth assertions | go-toolchain test |
|---|---|---|---|---|
| 43589219296 | `9449f9bc` | failure | 3 failed (`gap118_test.go:179`, `:347`, `:426`) | failed |
| 43591046880 | `ad0c3634` | failure | **3 passed** | failed |

The GAP118 trio fails with `root filesystem device not identifiable: source
"/dev/mapper/cow-root" ... no whole-disk device in /sys/block holds partition
"dm-0"`, and it passed on the second run against unchanged product source. The
non-determinism asserted by QA-BUNKER-106 is therefore **measured**, and the
deterministic CI blocker collapses to exactly one test, owned by INT-CI-090.

**Inference, not measurement:** RELEASE-024 AC1's toolchain pin should also
clear INT-CI-090, because the failure is precisely a `go1.26.10`-compiled
artifact being driven by a `go1.26.11` go tool — pinning the module toolchain
to a fixed version re-aligns the pair. This has not been re-measured after a
bump and must not be reported as fixed until it is.

### Version hygiene — surfaces consistent, but Unreleased is empty

| surface | value |
|---|---|
| newest tag / Release | `v0.3.0` (Latest, published 2026-11-09) |
| `Makefile:22` | `VERSION ?= 0.3.0` |
| `CHANGELOG.md` | `## Unreleased` followed immediately by `## 0.3.0 (2026-11-09)` |

No version drift against the tag. But `## Unreleased` holds **no entries** while
main carries three material commits since the tag, so the next cut has no
accumulated categorized material and RELEASE-024 AC1's changelog step starts
from nothing. The material set to write up:

- `68bc13b8` feat(bunker): container-mode command builder scaffolding (GAP-065) — dead scaffolding, zero non-test references
- `ca927ae9` feat(bunker): containment disclosure marker is final stdout line (GAP-067) — the only behaviour delta
- `02eb50d4` fix(qa): wire collect evidence path

### Materiality and deployed artifacts

`v0.3.0..ad0c3634 --no-merges`: 2 `feat`, 1 `fix`, 0 breaking markers → minor →
**v0.4.0**.

Measured host-local deployment: `/home/kara/.local/bin/bunker` reports `0.3.0`
at commit `0422dae7`, built `2026-11-11T12:33:23Z` — 65 commits behind this
candidate. That is a post-tag main build and is expected between releases; it is
not a v0.4.0 artifact, and no v0.4.0 binary exists anywhere yet.

### Board changes made by this pass (rows only, no code)

- RELEASE-024 title corrected from the superseded `Security patch v0.3.1` to
  `v0.4.0 prep: pin a fixed Go toolchain and rebuild vulnerable go1.26.10
  artifacts for GO-2026-6859`, so the title no longer contradicts the row's own
  AC1/AC5 literals (the retarget was already recorded in the note and ACs on
  2026-11-12 but never reached the title).
- QA-BUNKER-106 received its first `foreman_note` carrying the controlled-pair
  proof and the fix-worker consequence (make the io-bandwidth assertions resolve
  the device truthfully or skip loudly with a named reason; never silently pass).
- `boardctl validate -C .coding-hermes/board` → `RESULT: OK`, **0 errors**, 1004
  warnings. RELEASE-026 AC2's "exactly 1002 inherited warnings" literal has
  drifted +2 from other writers' event appends; it is still zero new errors, so
  the criterion holds in substance but its count is stale.

### Decision

**NO CUT.** Blockers are unchanged and every one is already filed: RELEASE-024
(toolchain pin + five version surfaces + changelog), RELEASE-025 (published
v0.3.0 notes), RELEASE-026 AC3 (conflict-marker regression guard still absent
from `scripts/board-jsonl-guard.sh` and every workflow), RELEASE-027 (GAP-067
ship-or-hold), RELEASE-028 (runner capacity / full gate). No tag, Release,
asset, toolchain, daemon or binary was mutated by this pass.

## Pass 7 — `573d684c` re-certification; NO CUT

Tick: `bunker-releng-2026-11-14-11-02-16`; candidate re-anchored at `origin/main` `573d684cd5dc2b3504d40ff1288377c5c071afac`. Product-tree movement from the prior certified carrier `ad0c3634` is bookkeeping-only: the filtered material path set is empty. This is a fresh exact-head CI measurement, not a new product census.

### Decision

**NO CUT.** No tag, GitHub Release, or asset was created. A minor proposal remains `v0.4.0`: the exact range `v0.3.0..573d684c` contains 2 `feat`, 1 `fix`, 7 `perf`, 0 breaking markers, and 97 non-merge commits total. The release gate is closed by exact-head CI run `43591759168`, terminal `failure` on this candidate: `changes`, `build-and-test`, and `gitreins-guard` succeeded; `unit-tests` failed; `regression` and `root-suite` were skipped. The failing test is `TestBuildAgentPersistentExecCommand_EnsureThenExecInsideContainer` at `internal/server/exec_persistent_test.go:164`; its nested Go build reports `go1.26.10` compiled packages versus `go1.26.11` go tool. This confirms the current `go.mod` toolchain pin remains behind the runner/tool mismatch and is owned by `RELEASE-024`/`INT-CI-090`. Per doctrine, no sibling green run is borrowed.

### Evidence table

| Area | Result | Receipt |
| --- | --- | --- |
| Repository identity | PASS | `git@github.com:deployBunker/bunker.git`; forge default `main`; `gh repo view` confirms `deployBunker/bunker` |
| Candidate/parity | PASS | `origin/main`, local `HEAD`, and candidate resolve to full SHA `573d684cd5dc2b3504d40ff1288377c5c071afac`; checkout clean |
| Baseline/release surface | PASS | baseline `v0.3.0` peeled commit `de286fdce69cd92f9f182f3e1eecde34404d713e`; 7 remote tag refs; GitHub Releases `v0.3.0`, `v0.2.0`, `v0.1.4`; no `v0.4.0` |
| Delta census | PASS | `git rev-list --no-merges --count v0.3.0..573d684c` = 97; record-delimited parser accepted 97 records; 2 feat, 1 fix, 7 perf, 0 breaking; 66 other/non-conventional bucket |
| Materiality | MINOR proposal | `ca927ae9` GAP-067 containment disclosure and `68bc13b8` GAP-065 scaffolding are feat subjects; GAP-065 has zero non-test references per prior verified record |
| Exact-head CI | RED / blocking | run `43591759168`, exact head SHA; `build-and-test` success, `changes` success, `gitreins-guard` success, `unit-tests` failure, `regression` skipped, `root-suite` skipped |
| CI failure detail | RED | `TestBuildAgentPersistentExecCommand_EnsureThenExecInsideContainer`: `compile: version "go1.26.10" does not match go tool version "go1.26.11"` |
| GitReins | UNVERIFIED exact tip | no verdict file in the fetched `origin/main` tree binds `573d684c`; the latest local stored history is not present in `origin/main` and is not used as exact-tip evidence; RELEASE-027 remains pending on GAP-067 ship-or-hold |
| Dogfood/QA | FAIL / stale to candidate | latest committed install-classified evidence has 26 rows, product verdict `FAIL`: tests and ci-pass FAIL, lint INFO, several N/A; no fresh exact-tip full battery |
| Version hygiene | current release consistent; next cut UNPREPARED | `internal/version/version.go`, `version_test.go`, `Makefile` remain 0.3.0; `CHANGELOG.md` has empty `Unreleased`; no v0.4.0 prep |
| Security | BLOCKING carried finding | prior committed sweep identifies reachable GO-2026-6859 in `net/http` with go1.26.10, fixed in go1.26.11; current checkout toolchain is go1.26.10 and `go.mod` pins go1.26.10; RELEASE-024 pending |
| Release tooling | PASS / repository-owned | Makefile `release-binaries`; `scripts/pre-release-check.sh`; `docs/release-checklist.md`; tag-triggered `.github/workflows/release.yml`; Go artifacts are four static Linux binaries plus `SHA256SUMS` and `install.sh`; no Dockerfile or goreleaser |
| Release workflow trigger | PASS | `.github/workflows/release.yml` has `push.tags: ['v*']`; CI workflow does not run on tag pushes |
| Deployed staleness | OPEN | prior live probe recorded local CLI 0.3.0 and daemon 0.2.1; no v0.4.0 artifact exists; no deployment mutated |
| Published notes | OPEN | RELEASE-025 pending; prior live probe says v0.3.0 body lacks Known issues and categorized changes |
| Board blockers | OPEN | RELEASE-003, RELEASE-024, RELEASE-025, RELEASE-026, RELEASE-027, RELEASE-028, and RELEASE-022 remain non-terminal in the fetched board; no duplicate release row filed |

### Exact conventional material subjects

- `ca927ae9ad4c4f5cb8af66d401812ee73bd57ce8` — `feat(bunker): containment disclosure marker is final stdout line (GAP-067)`
- `68bc13b895921cd3aeafc43f0d83c7ccee61772a` — `feat(bunker): container-mode command builder scaffolding (GAP-065)`
- `02eb50d42dbb8ee591facd93fcac9d6bf8ac3502` — `fix(qa): wire collect evidence path`
- `ffa730726098a7a4f75d3ba9ee873831520fc6eb` — `perf: fire-7 records-only proof on PERF-021 (2026-11-13g)`
- `8ed0b8e6162cdbc60ac94b69c245be57e570d022` — `perf: fire-6 records-only proof on PERF-021 (2026-11-13f)`
- `3ef564801a9abadc8e49b8c3905b99fdb446699b` — `perf: fire-5 quiet-window paired arm on PERF-021 (2026-11-13e)`
- `9cdf1a92392cfe5d72d828f9cb3045eb63705d10` — `perf: fire-4 contention remeasurement on PERF-021 (2026-11-13d)`
- `1eb472810da5dad08d94296b6f5d0c3609f64f57` — `perf: fire-3 records-only proof on PERF-021 (2026-11-13c)`
- `7a3e85ea9478a8cf4cd22d9996d0adb21d99e982` — `perf: attach fire-2 contention remeasurement on PERF-021 (records-only)`
- `fe3473e80d6eea8ad31bc1c01e8914c210cd54ae` — `perf: file PERF-021 (ListAgents = one /proc sweep, 96-99%, registry-independent)`

### Required next gates

1. RELEASE-024: pin Go >=1.26.11, prepare all five 0.4.0 version surfaces and categorized notes, rebuild/scan all four artifacts, and obtain exact-prep-SHA green CI with regression and root-suite executed.
2. RELEASE-027: choose ship/hold for GAP-067 and obtain exact-tree GitReins/live evidence if shipping it.
3. RELEASE-028: restore full candidate CI certification; do not borrow sibling results.
4. RELEASE-025: repair published v0.3.0 notes without retagging or replacing assets.
5. RELEASE-003/008: refresh deployed artifacts only after the fixed release exists.

No board row was added because the existing canonical RELEASE-* rows already own each blocker. This pass changed no product code, tag, Release object, asset, toolchain, or daemon.
