# Live evidence: deployed CLI includes DF-BUNKER-39 mount fix (2026-10-06, bunker-mvp 78.46.173.180)

Verified by foreman tick #571 via SSH (root@78.46.173.180):

## Binary identity
- `/usr/local/bin/bunker version` → `bunker 0.2.0`, `commit: 365ac56` (resolved live)
- sha256(/usr/local/bin/bunker) = e10a64cf36cac919f6b4b0410a2c5fa5cf473f1bb9eca46319f1c32180982c3d
- Local control host: every installed CLI (PATH, /usr/local/bin) reports commit 9b924f0 = HEAD a4165de tree, version 0.2.0.
- Fresh build proof: `go build ./cmd/bunker` in a clean clone of origin/main HEAD a4165de → `/tmp/bh-1791313593`, commit a4165de, sha256 420e9303b0f5ce7696f0d484c0ed9b2a43bd21419048007fcae7bf50bc8b0e66.

## Fix containment
- `git tag --contains 653d763` → v0.2.0 (mount fix DF-BUNKER-39 is inside the release tag).
- `git merge-base --is-ancestor 653d763 v0.2.0` → 0 (ancestor).
- v0.2.0 tag tree = 149411c (origin), GitHub Release v0.2.0 marked Latest with 6 assets (release workflow 37004146114 success).

## Live behavior of the deployed binary
- `bunker mount d3962808 --server mvp-live /tmp/mnt-t571` (demo daemon) → the OLD failure signature
  "mount preflight: no remote path to check" does NOT appear; the command proceeds past path resolution to
  `SSH key not found at /root/.config/bunker/keys/d3962808` — i.e. remote path resolution (the 653d763 fix)
  works; the error is the expected client-side missing key for a not-spawned-from-this-CLI agent.
- The pre-v0.2.0 default-form failure ('mount preflight: no remote path to check') is no longer reproducible.

## The installable release binary (what install.sh serves)
- GitHub Release v0.2.0 asset `bunker-linux-amd64` downloaded 2026-10-06 and checksum-verified:
  sha256 982c57fcf2290168c82b24b89e32cf3477c9ecf33c9c66dc2ab401ee3e278088 == SHA256SUMS entry.
- `./bunker-v020 version` → `bunker 0.2.0`, `commit: 149411c` = the v0.2.0 TAG COMMIT itself
  (`git rev-parse v0.2.0^{commit}` / tag 91e2b07 → tree 149411c). `git tag --contains 653d763` → v0.2.0,
  so the binary a user installs from the release channel contains fix 653d763.

## Demo-host deployed binary (context, not this row's defect)
- `/usr/local/bin/bunker version` → `bunker 0.2.0`, `commit: 365ac56` — a dirty local build
  (vcs.modified=true) whose sha256 = e10a64cf36cac919f6b4b0410a2c5fa5cf473f1bb9eca46319f1c32180982c3d.
  Its provenance/staleness is the RELEASE-003/008 deploy-freshness family, NOT DF-BUNKER-49.

## Live behavior of the demo-host binary
- `bunker mount d3962808 --server mvp-live /tmp/mnt-t571` (demo daemon) → the OLD failure signature
  "mount preflight: no remote path to check" does NOT appear; the command proceeds past path resolution to
  `SSH key not found at /root/.config/bunker/keys/d3962808` — i.e. remote path resolution (the 653d763 fix)
  works; the error is the expected client-side missing key for a not-spawned-from-this-CLI agent.
- The pre-v0.2.0 default-form failure ('mount preflight: no remote path to check') is no longer reproducible.

## Conclusion
DF-BUNKER-49's defect (installed CLI predates mount fix 653d763) is resolved by the v0.2.0 release channel:
the released, checksum-verifiable, install.sh-served binary is built from the v0.2.0 tag commit which
contains 653d763. RELEASE-007's precondition is satisfied. Both rows are verified-and-closed (no code change
required). RELEASE-003/008 (deployed-binary staleness on hosts) remain OPEN and are unaffected by this closure.
ch:trace row=DF-BUNKER-49 row=RELEASE-007 spec=.coding-hermes/board/tasks.jsonl#DF-BUNKER-49 evidence=docs/dogfood/2026-10-06-df-bunker-49-live-proof.md witness=release:v0.2.0 witness=tag:v0.2.0@origin witness=live:ssh://root@78.46.173.180/usr/local/bin/bunker verdict=none:verified-and-closed commit=1fe8971 memory=none:not-applicable
