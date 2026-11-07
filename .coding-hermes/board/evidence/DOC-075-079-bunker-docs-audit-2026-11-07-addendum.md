# Addendum — bunker docs audit tick bunker-docs-2026-11-07-00-08-38 (final write boundary)

This file is an ADDENDUM to the frozen evidence artifact
`.coding-hermes/board/evidence/DOC-075-079-bunker-docs-audit-2026-11-07.md`
(sha256 `f56798f2a1511a8e99826956de8208317aeebe51a2c7cd5bd7340165fde782de`, unchanged).
The frozen artifact predicted this sweep's board writes; this addendum records what actually
happened after it was frozen. Nothing in the frozen file was edited.

## What the frozen artifact predicted

Five `boardctl create` rows (DOC-075..DOC-079), one re-triage (GAP-177 P2 -> P4) and one audit
event. It stated the target docs, `.gitreins/tasks.yaml` and the README-010 surface would not be
touched.

## What actually happened (in order)

1. **DOC-075..DOC-079 were absorbed by a concurrent foreman commit before this lane committed.**
   A sibling tick committed `247be348` ("board: tick 00-10-36 OBS-BUNKER-008 closed with merged
   fix") while this audit was still running; it swept the five freshly created rows into its own
   commit and pushed them. Verified afterwards: `git show origin/main:.coding-hermes/board/tasks.jsonl
   | grep -c '"DOC-07[5-9]"'` -> 5. Their content is unchanged by this lane.
2. **The board was found UNPARSEABLE at origin/main** — the same sibling commit had written the
   OBS-BUNKER-008 row as 7 physical lines (raw newlines inside `acceptance_criteria`):
   - `boardctl show GAP-177` -> `tasks.jsonl: line 879: unexpected EOF`
   - `boardctl validate` -> `RESULT: FAIL (1 error(s), 974 warning(s))`
   - line census: physical lines 893, parsed rows 886, 7 bad lines
   - per-revision census: `9f3c870c` badlines=4, `7004119d` (revert) badlines=0,
     `247be348` badlines=4 — the same writer reproduced the defect after a revert.
3. **Minimal disclosed repair**, because the corruption blocked this lane's own deliverable (a
   foreman cannot read or dispatch a board that will not parse, and board-lint blocked commits):
   the single torn row was re-serialized onto one physical line (strict=False parse +
   `json.dumps`); every other physical line was asserted byte-identical (prefix/suffix comparison
   in the repair script); the row's fields were verified intact — `id`, `status`, `priority`,
   `commit_hash` unchanged and all three acceptance steps still present. 893 -> 887 physical
   lines. Pre-repair board copy:
   `/home/kara/.hermes/stand-in/docs/bunker/tasks.jsonl.pre-repair-2026-11-07`
   (sha256 `e9fd1e61c43bc57132f681f96032a023397c5a65b4b41f504aa7730e6016220e`).
   After: `boardctl validate` -> `RESULT: OK (977 warning(s))` (the pre-audit baseline count) and
   `boardctl show OBS-BUNKER-008` reads normally again.
4. **One additional row was filed that the frozen artifact did not predict: `BOARD-GUARD-002`
   (P1)** — the cause of (2), a board-integrity defect, not a documentation gap. Filed at P1
   deliberately: a board that does not parse blocks every commit in the repository and reads as an
   idle fleet, which is exactly what the P1 tier is for. It is the only non-doc row this lane has
   filed and it carries its own acceptance (identify the writer, make it emit one physical line
   per row or guard the write, prove the guard RED on the 247be348 blob).

## Final publication state

- Commit `3c55b66b` — `board(docs): bunker-docs tick 2026-11-07 — 5 doc gaps + board-integrity repair`
  (3 files: board tasks, board events, the frozen evidence artifact). GitReins Tier 1: `DEGRADED
  PASS` (Go legs skipped — no Go files staged), secrets clean.
- Pushed: `247be348..3c55b66b  main -> main`; `git rev-list --left-right --count HEAD...origin/main`
  -> `0 0`.
- Published-tip verification (not the worktree):
  `git show origin/main:.coding-hermes/board/tasks.jsonl` -> 888 rows, **0 bad lines**;
  DOC-075..DOC-079 and BOARD-GUARD-002 present, all `pending`; GAP-177 present, `pending`;
  `git cat-file -e origin/main:.coding-hermes/board/evidence/DOC-075-079-bunker-docs-audit-2026-11-07.md`
  -> resolves, blob sha256 `f56798f2…82de` (identical to the digest cited in every row marker).

## Boundary that did NOT move

- No target documentation was edited by this lane: `git show --name-only 3c55b66b` lists only the
  three board paths above.
- `.gitreins/tasks.yaml` (a foreign dirty path present before the audit) was never read, staged or
  committed.
- README-010 (`bunker iobattery` / `bunker paths` front-door gap) was left alone — it already owns
  that surface.

ch:trace row=BOARD-GUARD-002 spec=.coding-hermes/board/schema.sql wave=none:serial-audit-tick test=none:post-push-board-parse doc=.coding-hermes/board/tasks.jsonl prompt=~/.hermes/skills/coding-hermes/coding-hermes-docs/SKILL.md evidence=.coding-hermes/board/evidence/DOC-075-079-bunker-docs-audit-2026-11-07-addendum.md witness=path@origin:.coding-hermes/board/tasks.jsonl verdict=none:open-finding commit=3c55b66b08e85e0f479ff195a0ce84330812b7ef memory=/bunker/docs-audit/2026-11-07

STATUS: PASS
TIER: T4 — published tip read back from origin/main (board parse census, row census, evidence blob sha256) after a real CLI probe at T3
EVIDENCE: git show origin/main:.coding-hermes/board/tasks.jsonl -> 888 rows / 0 bad lines; commit 3c55b66b pushed, HEAD...origin/main = 0 0
