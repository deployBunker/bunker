# DOC-080 withdrawal + re-fire verification — bunker-docs tick bunker-docs-2026-11-07-00-08-38

Lane: bunker-docs (documentation audit). Target: `bunker` at `/home/kara/deployBunker/bunker`.
This file records the work of the SECOND execution of tick `bunker-docs-2026-11-07-00-08-38`
(the first execution, at 00:27–00:58Z, filed DOC-075..DOC-079 and BOARD-GUARD-002 and pushed
them in `3c55b66b` / addendum `0871f1eb`). The re-fire did not re-audit the same surface from
scratch; it verified what the first execution landed, and it found one defect in that work.

## 1. Defect found in this tick's own earlier work: DOC-080 duplicates DOC-066

The earlier execution left an UNPUBLISHED `DOC-080` row in the working tree (plus one untracked
evidence file) claiming this finding:

> `P4 — missing-reference: document the repository-local traceability doctrine` — "no existing
> row covers this missing contract".

That claim is false. `DOC-066` at `origin/main` already owns exactly this finding.

```
$ git show origin/main:.coding-hermes/board/tasks.jsonl \
    | jq -r 'select(.id=="DOC-066")|"PRIO:\(.priority) STATUS:\(.status)\nTITLE:\(.title)\n\(.reasoning[0:400])"'
PRIO:P4 STATUS:pending
TITLE:P4 — missing-reference: repository-local traceability doctrine is absent
Type: missing-reference. Policy severity: P4 by the 2026-09-30 documentation ruling. Reader:
developer/operator/foreman. Source surface: repository-required docs/traceability-doctrine.md
is absent; exact probe was `test -f docs/traceability-doctrine.md && echo PRESENT || echo MISSING
docs/traceability-doctrine.md` and output was `MISSING docs/traceability-doctrine.md`.
```

`DOC-068` ("P4 — re-evaluate documentation audit findings DOC-066 and DOC-067") is its paired
re-evaluation row, so the surface already has both a fix row and a follow-up. A second row would
have made two foremen dispatch the same work and would have cost a second warning-census rise for
no new information — the exact "manufacture a finding" failure the docs lane is told to avoid.

Note the failure mode precisely: the earlier execution checked only its OWN sibling rows
(DOC-075..DOC-079, BOARD-GUARD-002) for prior ownership and did not search the board's older
`DOC-*` rows. Searching `select(.id|test("^DOC-"))` at `origin/main` returns DOC-066 in one call.

## 2. Withdrawal — performed, and proven byte-exact

The row was never committed or pushed (`git status` showed it only as a working-tree modification).
It was removed from the working tree, together with the five uncommitted `events.jsonl` lines it
had produced, and nothing else was touched.

Pre-withdraw snapshots (stand-in, outside the repo):

```
$ sha256sum $S/tasks.jsonl.pre-withdraw-2026-11-07 $S/events.jsonl.pre-withdraw-2026-11-07
287043a6bb84e67bfc6c8e6d09c9d3edc73e0e7382450d37cf91b230ca9b4405  tasks.jsonl.pre-withdraw-2026-11-07
83cdc12314e96b5a4a18a8f6e612f3f2b248f7b822280ca7179dfabd6f2d0621  events.jsonl.pre-withdraw-2026-11-07
```

Line numbers removed (located by id, not by index guess):

```
$ grep -n '"id":"DOC-080"' .coding-hermes/board/tasks.jsonl | cut -c1-60
889:{"id":"DOC-080","status":"pending","title":"P4 — missing
$ grep -n 'DOC-080' .coding-hermes/board/events.jsonl | cut -c1-90
1606:{"id":1794011329814495559,...,"event_type":"task_create
1607:{"id":1794011329814495560,...,"event_type":"audit",...
1608:{"id":1794011329814495561,...,"event_type":"task_update
1609:{"id":1794011329814495562,...,"event_type":"task_update
1610:{"id":1794011329814495563,...,"event_type":"audit",...
```

After removal (only line 889 of tasks and lines 1606-1610 of events dropped):

```
$ wc -l .coding-hermes/board/tasks.jsonl .coding-hermes/board/events.jsonl
    888 .coding-hermes/board/tasks.jsonl
   1605 .coding-hermes/board/events.jsonl
$ git diff --stat .coding-hermes/board/tasks.jsonl .coding-hermes/board/events.jsonl
[diff-end]                       # EMPTY: both files are byte-identical to HEAD again
$ grep -c 'DOC-080' .coding-hermes/board/tasks.jsonl .coding-hermes/board/events.jsonl
.coding-hermes/board/tasks.jsonl:0
.coding-hermes/board/events.jsonl:0
$ jq -e . .coding-hermes/board/tasks.jsonl > /dev/null && echo "tasks.jsonl parses OK"
tasks.jsonl parses OK
$ jq -e . .coding-hermes/board/events.jsonl > /dev/null && echo "events.jsonl parses OK"
events.jsonl parses OK
```

`git diff` being empty is the load-bearing check: the withdrawal restored the board to its
published state exactly, so this tick publishes no row and leaves no orphan event.

The probe commands from the withdrawn finding are preserved verbatim in
`DOC-080-traceability-doctrine-probe-2026-11-07.md`, whose header and conclusion now say
WITHDRAWN / owned by DOC-066. That file is committed as a probe record, not as a finding.

## 3. Re-fire verification of the rows this tick DID land

Census at the published tip (not the worktree):

```
$ git show origin/main:.coding-hermes/board/tasks.jsonl \
    | jq -r 'select(.id|tostring|test("^DOC-07[5-9]"))|"\(.id)\t\(.status)\t\(.priority)"'
DOC-075 pending P4
DOC-076 pending P4
DOC-077 pending P4
DOC-078 pending P4
DOC-079 pending P5
```

Three of the five premises were re-executed at HEAD rather than trusted from the row text:

```
# DOC-078 — README still documents the pre-GAP-181 ladder, env var still undocumented
$ grep -c 'BUNKER_CONFIG_HOME' README.md specs/configuration.md docs/backup-set.md
README.md:0
specs/configuration.md:0
docs/backup-set.md:0
$ sed -n '1471,1474p' README.md
BUNKER_HOME=/path/to/state        # config at $BUNKER_HOME/config.yaml
$HOME/.bunker/config.yaml         # default (unchanged)

# DOC-075 — the spec still declares /metrics absent while the server registers it
$ sed -n '54p' docs/observability-spec.md
script but **is absent from the Prometheus inventory** — it has never landed.
$ grep -n '"/metrics"' internal/server/metrics.go
30:		r.Get("/metrics", newMetricsHandler(tracker, buildVersion, restAddr, lifecycle, webdavSnapshot...).ServeHTTP)

# DOC-079 — both dangling links, still dangling at HEAD
$ sed -n '126p' docs/prd/SPEC-bunker-native-file-tools.md
Related: [docs/both-ways.md](../../both-ways.md) — when to use the verbs vs the SSHFS mount.
$ sed -n '174p' specs/architecture.md
[specs/safety-presets.md](specs/safety-presets.md) ("Per-file size") and
$ ls -1 both-ways.md ; ls -d specs/specs
ls: cannot access 'both-ways.md': No such file or directory
ls: cannot access 'specs/specs': No such file or directory
$ ls -1 docs/both-ways.md specs/safety-presets.md
docs/both-ways.md
specs/safety-presets.md
```

All three hold: the rows are still true at HEAD, not stale, not already fixed.

Board health at the start and end of this execution:

```
$ boardctl -C . validate 2>&1 | tail -1
RESULT: OK (977 warning(s))
```

977 is the post-repair baseline recorded in the addendum `0871f1eb`; this execution raised it by
zero (it filed no row) and the board parses (`jq -e` on both files above).

## 4. Re-triage census (2026-09-30 doc-severity ruling)

Rows still open at P1-P3 whose reasoning is typed as a documentation finding
(`missing-reference|wrong-reference|rot|missing-audience|operator-path`):

```
$ git show origin/main:.coding-hermes/board/tasks.jsonl \
    | jq -r 'select(.status=="pending" or .status=="in_progress")
             | select(.priority=="P1" or .priority=="P2" or .priority=="P3")
             | select((.reasoning|tostring)|test("Type:\\s*(missing-reference|wrong-reference|rot|missing-audience|operator-path)";"i"))
             | "\(.id)\t\(.priority)"'
(no output)
```

Zero. The one open P1-P3 row that READS like a doc row, `DF-BUNKER-92` (P2, "README/docs teach
`bunker spawn --mount-driver rclone` … every released daemon refuses it"), is deliberately NOT
demoted: its finding is that a documented command does not work on the shipped daemon, which the
ruling keeps out of the doc-severity bucket precisely because it is a functional defect wearing
docs clothing. Left at P2, unmodified.

## 5. Boundary — what this execution did not touch

- No target documentation was edited (no README, no `docs/`, no `specs/`).
- No new board row was filed; the only board change is one `audit` event appended by `boardctl`.
- `.gitreins/tasks.yaml` — a foreign dirty path that predates this tick — was not read, staged or
  committed.
- Not verified: whether the five landed rows' fixes work (that is the workers' job), and DuckBrain
  read-back of the namespace record is reported in the tick report, not asserted here.

ch:trace row=DOC-066 spec=none:repository-contract-absent wave=none:serial-audit-tick test=none:duplicate-row-census doc=docs/traceability-doctrine.md prompt=~/.hermes/skills/coding-hermes/coding-hermes-docs/SKILL.md evidence=.coding-hermes/board/evidence/DOC-080-withdrawal-and-tick-verification-2026-11-07.md witness=path@origin:.coding-hermes/board/tasks.jsonl verdict=none:open-finding commit=none:open memory=/bunker/docs-audit/2026-11-07

STATUS: PASS
TIER: T4 — board state read back from the published tip (git show origin/main) plus live CLI (boardctl validate, jq parse census) and re-executed doc probes at HEAD
EVIDENCE: .coding-hermes/board/evidence/DOC-080-withdrawal-and-tick-verification-2026-11-07.md
