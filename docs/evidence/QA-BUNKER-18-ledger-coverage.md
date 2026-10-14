# QA-BUNKER-18 — bunker ledger coverage backfill

Row: `QA-BUNKER-18` — "bunker is absent from the QA ledger — coverage for this
target is never recorded (structural)" (P3, filed 2026-09-15).

## What the row claimed (2026-09-15)

`~/.hermes/qa/ledger.jsonl` held 37 rows and zero with `project: "bunker"` (the
only near keys were `bunker-las-03` — the server name used as a project key).
`qa_discover.py` reads that ledger for never-qa/age scoring, so the bunker
target scored "never QA'd" forever and its coverage was never recorded.

## What is true now (2026-10-11 closure census)

The non-dagger QA driver resumed writing ledger rows on 2026-09-29. Census of
`~/.hermes/qa/ledger.jsonl` at closure:

- Rows with `"project": "bunker"`: 11 (grep `"project":"bunker"` = 10 pre-fix,
  plus this backfill row). Matched line numbers pre-fix: 6, 30, 77, 141, 165,
  205, 246, 248, 272, 273.
- Span: 2026-09-29 through 2026-10-10; modes include `full-battery`,
  `same-head-withhold+off-battery`, and full-fresh-battery cycles; verdicts
  FINDINGS / FINDINGS-withheld-recurrence / PASS.
- `qa_discover.py::load_ledger()` therefore scores bunker from real rows today;
  the never-qa structural gap the row described is closed.

## Action taken

1. Census (above) proving bunker is covered by the live ledger.
2. One backfill row appended to `~/.hermes/qa/ledger.jsonl` keyed
   `project=bunker`, `lane=bunker-qa`, `target_type=repo-qa-coverage`,
   `coverage_status=covered`, `verdict=PASS`, `findings_filed=0`, naming this
   evidence doc — so the closure itself is a first-class ledger entry rather
   than a silent fix. Ledger validated line-by-line after append (345 valid
   rows, 0 bad).
3. No product defect existed; no new QA finding filed.

## Why a commit if the ledger already covers bunker

The row premise is stale but the row was real on 09-15, and the closure has to
be auditable from the repo: this doc is the census record, and the ledger
backfill row points at it. Ledger hygiene (row premise vs current state) is
recorded in the row's `observations` field.
