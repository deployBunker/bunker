# RELEASE-030 — 0.4.0 changelog verdict sweep

**Row:** RELEASE-030 — "0.4.0 CHANGELOG advertises GAP-067 under Added while
its only verdict is changes_required".

**Date:** 2026-11-17. **Repo:** `deployBunker/bunker`. **Worktree:**
`/home/kara/worktrees/bunker-RELEASE-030` (`wt/RELEASE-030`), based on
`3776f526fdb1c4b55c1711ad674d396be181b4c2` (`origin/main`).

---

## 1. Verdict sweep

The 0.4.0 section has three release-note bullets. The command below recursively
read all 866 stored verdict JSON files from the read-only canonical history
corpus, selecting records for the three associated board rows. No verdict file
was modified.

Exact command:

```
python3 /home/kara/.hermes/cache/scratch/release030_sweep.py /home/kara/deployBunker/bunker/.gitreins/history
```

Full output:

```
verdict_files_scanned=866
GAP-065: 0 stored verdict(s)
GAP-067: 2 stored verdict(s)
  path=2026-09-11/84f8498b/verdict.json
  commit=None passed=False merge_recommendation=None evaluated_at=2026-09-11T21:58:38.975278
  path=2026-11-12/d17bbbcf/verdict.json
  commit=0325d3c6ed3f061d8d886f7ef6fec7f899ab8e0f passed=False merge_recommendation=changes_required evaluated_at=2026-11-12T10:51:05.598852
QA-BUNKER-FOREMAN-3: 3 stored verdict(s)
  path=2026-11-12/dba1a2af/verdict.json
  commit=fbdf61dd3740fd0866d69e5c1668666130e4c97f passed=False merge_recommendation=None evaluated_at=2026-11-12T20:16:47.287275
  path=2026-11-12/e3304f26/verdict.json
  commit=b49d4990a2dc377601bd8ece33fe8d72d57b377b passed=False merge_recommendation=None evaluated_at=2026-11-12T19:05:35.800900
  path=2026-11-12/ea7de237/verdict.json
  commit=b49d4990a2dc377601bd8ece33fe8d72d57b377b passed=False merge_recommendation=None evaluated_at=2026-11-12T19:01:03.098682
```

| 0.4.0 bullet | board row | judged commit | verdict path(s) | passed | merge recommendation | advertised as accepted? | action |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Containment disclosure marker (GAP-067, `ca927ae9`) | GAP-067 | `0325d3c6` (latest); older record has no commit | `.gitreins/history/2026-11-12/d17bbbcf/verdict.json`; older: `.gitreins/history/2026-09-11/84f8498b/verdict.json` | false (both records) | `changes_required` (latest); older record has no recommendation | Yes, previously under Added | Moved into Held / not shipped; cites verdict and RELEASE-027 hold decision |
| Container-mode command-builder scaffolding (GAP-065, `68bc13b8`) | GAP-065 | none | none found in the 866-file sweep | no verdict | no verdict | Yes, under Added | Left in place; absence is not a pass and no verdict is invented |
| `bunker-qa.sh` collect evidence path wiring (`02eb50d4`) | QA-BUNKER-FOREMAN-3 | `b49d4990` (twice); `fbdf61dd` | `.gitreins/history/2026-11-12/e3304f26/verdict.json`; `.gitreins/history/2026-11-12/ea7de237/verdict.json`; `.gitreins/history/2026-11-12/dba1a2af/verdict.json` | false (all three) | null (all three) | Yes, previously under Fixed | Moved into Held / not shipped; all stored verdict paths and judged commits listed |

The GAP-067 `changes_required` verdict remains the release-blocking finding.
The sweep also found failing verdicts for the QA bullet, so it too was removed
from accepted release notes. GAP-065 has no stored verdict in this corpus;
absence of a verdict is not a pass. The board row is still pending and its
worker summary says full live acceptance was not met, but this sweep does not
invent or substitute a GitReins verdict.

## 2. GAP-067 before and after

Before (`origin/main:CHANGELOG.md`, under `### Added`):

```
- **Containment disclosure marker is the final stdout line (GAP-067,
  ca927ae9).** The server's containment disclosure is emitted as the last
  line of stdout, so downstream consumers can anchor on a stable final
  marker instead of parsing mid-stream output.
```

After (`CHANGELOG.md`, under `### Held / not shipped in this release`):

```
- **Containment disclosure marker (GAP-067, ca927ae9) is not accepted as
  shipped behavior.** The stored verdict for judged commit `0325d3c6` is
  `passed=false` / `changes_required`
  (`.gitreins/history/2026-11-12/d17bbbcf/verdict.json`); the RELEASE-027
  decision record chose option (c), hold
  (`docs/evidence/RELEASE-027-decision-20261117T160350Z.md`).
```

## 3. Gates and output

`sh scripts/pre-release-check.sh` (exit code 0):

```
OK: no top-level vendor/ directory (go stays in module mode)
WARN: between-tags bump state: newest tag v0.3.0 is older than version 0.4.0 — this is the bump commit, so cut the tag next: git tag v0.4.0 HEAD && git push --tags (docs/release-checklist.md); the tag-side gate is CI's Tag build check
OK: version parity — internal/version/version.go=0.4.0 CHANGELOG.md=0.4.0 Makefile=0.4.0; newest tag v0.3.0 (untagged bump to 0.4.0 — tag it next); first CHANGELOG heading: ## Unreleased
EXIT_CODE=0
```

The documented between-tags WARN is expected. No tag was cut and neither
`internal/version` nor the Makefile was changed.

`grep -n "GAP-067" CHANGELOG.md`:

```
15:- **Containment disclosure marker (GAP-067, ca927ae9) is not accepted as
```

It is in the held subsection, not under Added. `go run ./cmd/docs-drift`:

```
docs OK: README/CHANGELOG/docs match v0.3.0 (42 released commands, 180 commit(s) after the tag)
  released: [agent-tools audit connect cp deploy destroy env exec fs guard heartbeat homes host-provision info iobattery key linger list metrics mount operator paths pull reap registry renew resource-events restart run spawn ssh start status stop subid-migrate surface systemd toolchain-check tunnel umount use version]
```

No table was introduced by the changelog edit. The evidence table above has a
consistent eight-cell pipe-delimited shape for its header, separator, and each
data row.

`git -C /home/kara/worktrees/bunker-RELEASE-030 diff --name-only origin/main..HEAD`
was run before this evidence document was created; at that point the only
changed path was `CHANGELOG.md`. The intended final diff consists only of
`CHANGELOG.md` and this evidence document.

## 4. Release state

The 0.4.0 section remains version-surface preparation only. RELEASE-027 chose
option (c), hold; this evidence does not re-judge GAP-067, change its verdict,
cut a tag, or alter version authority files. The release remains held pending
the RELEASE-027 decision process.
