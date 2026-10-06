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

## Conclusion
The DF-BUNKER-49 defect (installed CLI predates mount fix 653d763) is resolved by the v0.2.0 release:
RELEASE-007's precondition is satisfied. Both rows are verified-and-closed (no code change required).
ch:trace row=DF-BUNKER-49 row=RELEASE-007 spec=.coding-hermes/board/tasks.jsonl#DF-BUNKER-49 evidence=docs/dogfood/2026-10-06-df-bunker-49-live-proof.md witness=live:ssh://root@78.46.173.180/usr/local/bin/bunker witness=tag:v0.2.0@origin verdict=none:verified-and-closed commit=a4165de memory=none:not-applicable
