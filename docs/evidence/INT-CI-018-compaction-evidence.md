# INT-CI-018 repository-local measurement trace

The committed `docs/tier2-eval-budget.md` section compares an existing uncompacted run
(PRE v2 `616c1d33`) with an existing compacted run (POST E `2aaef334`). The repository
preserves run-level totals and the compaction event, but not the per-criterion
transcripts or verdict evidence arrays. The documentation records unavailable
criterion-level counts as integer `0` fields with an explicit limitation; these are not
claims that zero files or zero evidence were read.

Source facts verified in the repository:

- `.gitreins/config.yaml` contains `evaluator.compaction_threshold: 0.005`.
- PRE v2 `616c1d33`: 0 compactions, 23 calls, 408,009 cumulative input tokens.
- POST E `2aaef334`: 1 compaction at 80,860 tokens, 74 calls, 3,221,740 real input
  tokens, and 445,943 final counter tokens.
- Raw spy JSONL and evaluator per-item transcripts are `/tmp` artifacts and are not
  committed, so no per-criterion file:line or distinct-file count can be recovered
  without inventing data.

ch:trace row=INT-CI-018 spec=docs/tier2-eval-budget.md#compaction-evidence-depth wave=.coding-hermes/waves/bunker-foreman-2026-10-20-23-42-41.json#INT-CI-018 test=grep-and-documentation-check doc=docs/tier2-eval-budget.md prompt=/tmp/bunker-foreman-2026-10-20-23-42-41-INT-CI-018-brief.txt evidence=docs/tier2-eval-budget.md witness=none:repo-local-measurement verdict=none:pending-foreman-judge commit=<full-sha> memory=none:not-written
STATUS: complete | partial | blocked
TIER: T3 (state exact checks invoked)
EVIDENCE: docs/tier2-eval-budget.md; docs/evidence/INT-CI-018-compaction-evidence.md
