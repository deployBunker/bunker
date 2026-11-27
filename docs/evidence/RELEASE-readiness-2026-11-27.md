Release readiness artifact for bunker releng tick bunker-releng-2026-11-27-15-59-08

Candidate: 163439bca81477cd4a83c3d36e04b0eb1cab793e (origin/main, fetched rc=0, parity 0/0)
Baseline: remote v0.3.0 peeled commit de286fdce69cd92f9f182f3e1eecde34404d713e; remote tag object 880ae8e4124c3f061cb5e2790caa038b231b5dbd; ancestor verified.
Decision: NO CUT. Proposed v0.4.0 minor. No major confirmation required. No tag or GitHub Release was created.

Inventory: 279 non-merge commits in de286fdce69cd92f9f182f3e1eecde34404d713e..163439bca81477cd4a83c3d36e04b0eb1cab793e. Conventional counts: feat 3, fix 16, perf 11, docs 29, test 6, chore 19, ci 4, refactor 0, other 191; sum 279. Breaking subject markers 0; BREAKING CHANGE footers 0. Material features include b7286e55, ca927ae9 (held), and 68bc13b8.

Version/tooling: internal/version, Makefile, and CHANGELOG advertise 0.4.0; scripts/pre-release-check.sh rc=0 with the documented between-tags warning. Repository-owned publisher is .github/workflows/release.yml on v* tag push, using make release-binaries to publish four static Linux binaries, SHA256SUMS, and install.sh. Do not also run gh release create. No Dockerfile or GoReleaser.

CI: exact candidate run 45966043800 success, but root-suite and regression were skipped; therefore not release-green. changes/build-and-test/unit-tests/gitreins-guard succeeded. Last full 6/6 product run is 45094609276 at ancestor 183703cfb009b8c84f1d9f877b57c2250560d315, not the candidate. Candidate check-run annotations were warnings/tooling notices, not failures.

GitReins: no verdict bound to the candidate. The latest judged PASS is .gitreins/history/2026-11-23/01d850ef/verdict.json at 183703cfb009b8c84f1d9f877b57c2250560d315; current candidate has release-relevant delta, so this is not exact-candidate evidence.

Dogfood: latest recorded run /dogfood/coding-hermes/run-33/bunker-dogfood-2026-11-24-20-42-53 is PROMISING-BUT-ROUGH, measured on the deployed fleet, battery PARTIAL. Open rows DF-BUNKER-124, DF-BUNKER-125, DF-BUNKER-126.

Deployment: ~/.local/bin/bunker reports 0.3.0, commit 0422dae7, built 2026-11-11; RELEASE-003 remains pending. bunkerd.service is active; running-process binary provenance was not re-established in this pass.

Open blockers: DOC-060-R2 stale 0.4.0 range prose; RELEASE-003 deployed staleness; DF-BUNKER-124/125/126 dogfood P1/P2 findings; INT-CI-093 pending evaluator closeout; RELEASE-022 owner-gated branch-protection shape.

Known issues for any future release notes: default-preset spawn refusal (DF-BUNKER-124), large single-file transfer cliff (DF-BUNKER-125), AppArmor profiles absent from status residue census (DF-BUNKER-126), and deployed 0.3.0 staleness (RELEASE-003).

Unverified: DuckBrain readback (MCP unavailable after stdio failures), full go test ./... result in this pass (tool window timed out), candidate-bound GitReins, candidate-bound dogfood, and running process binary identity.

No source, version, tag, release object, artifact publication, or deployed service was mutated. The artifact is intended to be committed with the releng board carrier.