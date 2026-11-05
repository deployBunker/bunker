# BOARD-GUARD-001 — board writer truncated tasks.jsonl 849 → 1 (commit 63140484)

Row: `BOARD-GUARD-001` [P0] — "Board writer truncated .coding-hermes/board/tasks.jsonl
from 849 rows to 1 (848 rows deleted, 130 of them pending) in commit 63140484".
Worktree: `/home/kara/worktrees/bunker-BOARD-GUARD-001`, branch `wt/BOARD-GUARD-001`,
base `8b15a26a`.

## Arm 1 — the exact writer, named

**The writer was NOT a repo file, NOT boardctl, and NOT a fleet helper. It was an
ad-hoc inline `python3 -c` closeout command issued by Hermes foreman session
`cc938535-7a84-4e0f-84a7-3561c42d1eea` (model `qwen/qwen3.7-plus:free`, source
`api_server`, tick `bunker-foreman-2026-11-03-04-13-41` — a serial-recovery
sibling tick running concurrently with tick `bunker-foreman-2026-11-03-01-02-30`
in the SAME shared checkout `/home/kara/deployBunker/bunker`).**

The truncating command, verbatim from `state.db` messages (recorded 2026-11-02
23:32:55, executed ~23:33:24):

    cd /home/kara/deployBunker/bunker && python3 -c "
    import json
    with open('.coding-hermes/board/tasks.jsonl') as f:
        for line in f:
            row = json.loads(line)
            if row.get('id') == 'QA-BUNKER-093':
                row['status'] = 'complete'
                row['worker_status'] = 'complete'
                row['commit_hash'] = 'e1aca15e'
                row['foreman_note'] = 'COMPLETE tick 713 (worker deepseek-v4-flash): ...'
                print(json.dumps(row))
    " >> .coding-hermes/board/tasks.jsonl.tmp && mv .coding-hermes/board/tasks.jsonl.tmp .coding-hermes/board/tasks.jsonl && echo "QA-BUNKER-093 closed on board"

Mechanism: `print(json.dumps(row))` lives INSIDE the `if` that matches the
target row, so all 848 non-matching rows are read and silently dropped. The
`>>` redirect starts from an empty `.tmp`, and the `mv` then atomically replaces
the 849-row board with the one re-serialized row. This is exactly the "re-emit a
filtered subset" anti-pattern; a correct closeout appends an event and replaces
ONE line in place, keeping every other byte.

### Proof chain (four independent matches)

1. **The commit command is in the same tool-call batch.** The recorded batch at
   23:32:55 (session `cc938535`) contains, immediately after the row update:
   `git add .coding-hermes/board/tasks.jsonl .coding-hermes/board/events.jsonl
   && git commit -m "foreman: close QA-BUNKER-093 on e1aca15e (tick 713)"` —
   the exact subject of 63140484 (committed 23:33:25).
2. **Event id arithmetic.** The events.jsonl line added by 63140484 has
   `"id": 1793680404438107136` and `"timestamp": "2026-11-02T23:33:24.438116"`.
   The same batch contains an inline event writer computing
   `int(datetime.datetime.now().timestamp() * 1e9)`; 1793680404438107136 / 1e9
   = 1793680404.438107 = `2026-11-03T04:33:24Z` = 23:33:24 local — the exact
   id AND timestamp, and the `isoformat()` shape explains the `T` separator
   that no prior event in the file has.
3. **Spaced-separator fingerprint.** The surviving row uses `", "` / `": "`
   separators with `\u2014` escapes — Python `json.dumps()` defaults. The
   event line is spaced the same way. Go `encoding/json` and boardctl emit
   compact separators, so neither can have produced either line.
4. **Key order.** The surviving row's key order (`status, id, title, detail,
   priority, source, reasoning, ...`, 19 keys) is byte-for-byte the key order
   the QA-BUNKER-093 row already had in the pre-commit board
   (`git show 63140484~1:... | grep QA-BUNKER-093`) — the parsed original row
   re-serialized, not a newly-built dict.

Sibling commit `459b59f3` (QA-BUNKER-092, 23:37:00, same session/tick) is the
same writer class: its surviving board is the same single spaced QA-BUNKER-093
row, i.e. the second closeout ran against the already-truncated board.

### Candidates positively refuted

- **boardctl** (`/home/kara/.local/bin/boardctl`, source `/home/kara/coding-hermes-boardctl`):
  emits compact JSON, cannot produce the spaced lines. The boardctl
  `event`/`update` calls for this tick were recorded in session `2df2e8f8...`
  at 23:35:13 — AFTER 63140484 (23:33:25) — and their event payloads
  (block_drive details) do not match the committed event.
- **`~/.hermes/skills/coding-hermes/coding-hermes-foreman/append_board_event.py`,
  `append_board_task_completed.py`, `board_row_closeout.py`**: all three write
  compact separators (`(",", ":")`); `board_row_closeout.py` is byte-preserving
  (rewrites only the target line); `append_board_task_completed.py` re-emits
  every parsed row, so it cannot shrink a file it could read. None of them
  appear in the tool-call record for the window.
- **The repo's own Go code**: no in-repo writer touches
  `.coding-hermes/board/tasks.jsonl` on the closeout path; Go JSON marshaling
  is compact.

### Scope note for the foreman (R1)

The defect writer is an unversioned inline session command (prompt-authored),
so there is no external tree to patch. The durable fix that this repo CAN own
is the enforcement layer below; the fleet-side follow-up (outside this repo) is
that foreman closeouts should use the byte-preserving helper
(`board_row_closeout.py`) or boardctl rather than hand-rolled inline JSON
rewrites. The untracked `/home/kara/deployBunker/bunker/tasks.jsonl`
(spaced one-row RELEASE-003, mtime 2026-11-01 05:21) is the same failure class
— a writer deciding the board path itself — and is covered by the guard's
path rule.

## Arms 2+3 — enforcement layer (in-repo deliverable)

Per scope ruling R2 (writer is external/unversioned), arms 2+3 land as the
in-repo ENFORCEMENT layer on the tasks.jsonl write path:

- `.hermes/scripts/board_shrink_guard.sh` — refuses a tasks.jsonl write that
  drops more rows than explicitly allowed (`--allow-shrink N` /
  `BOARD_SHRINK_ALLOW`, default 0), in both standalone (`--old/--new`) and
  hook (`--staged`: index blob vs HEAD blob) modes; also refuses any staged
  file NAMED tasks.jsonl that is not `.coding-hermes/board/tasks.jsonl`
  (the stray root-level tasks.jsonl class). Legitimate one-row closeouts
  (row count unchanged) and explicitly-allowed archives/splits pass.
- `.hermes/scripts/test_board_shrink_guard.sh` — self-contained RED/GREEN
  battery. RED fixture built from the real defect shape: 848 compact filler
  rows + the QA-BUNKER-093 row (compact, original key order) = 849 rows,
  rewritten to the single spaced 19-key row 63140484 left behind.
- `scripts/install-git-hooks.sh` — the tracked pre-commit installer now wires
  `board_shrink_guard.sh --staged` ahead of the Tier-1 gate, so a real box
  gets the refusal at `git commit` time.

## Gate output (verbatim)

    $ bash -n .hermes/scripts/board_shrink_guard.sh   → rc=0
    $ bash -n .hermes/scripts/test_board_shrink_guard.sh → rc=0
    $ bash .hermes/scripts/test_board_shrink_guard.sh
    RED: board_shrink_guard.sh --old board-849.jsonl --new board-1.jsonl rc=1
    GREEN: board_shrink_guard.sh --old board-849.jsonl --new board-849-closed.jsonl (surgical 1-line closeout) rc=0
    GREEN: board_shrink_guard.sh --old board-849.jsonl --new board-1.jsonl --allow-shrink 848 rc=0
    GREEN: BOARD_SHRINK_ALLOW=848 board_shrink_guard.sh --old board-849.jsonl --new board-1.jsonl rc=0
    RED: board_shrink_guard.sh --allow-shrink 847 (insufficient) rc=1
    RED: board_shrink_guard.sh --staged (849->1 staged) rc=1
    GREEN: BOARD_SHRINK_ALLOW=848 board_shrink_guard.sh --staged rc=0
    GREEN: board_shrink_guard.sh --staged (surgical 1-line closeout) rc=0
    RED: board_shrink_guard.sh --staged (stray root tasks.jsonl) rc=1
    board_shrink_guard battery: 4 RED arms, 5 GREEN arms, 0 unexpected
    test_board_shrink_guard.sh: PASS
    → rc=0

The RED refusal names the shrink:

    board_shrink_guard: REFUSED — <path> would drop 848 rows (849 -> 1; allowed 0).

Wiring proof (scratch repo, real installer + real guard, stub Tier-1 gate):

    $ bash scripts/install-git-hooks.sh        → rc=0, installed .git/hooks/pre-commit
    $ git commit -qm "truncating closeout"     → rc=1
      board_shrink_guard: REFUSED — staged .coding-hermes/board/tasks.jsonl would drop 2 rows (3 -> 1; allowed 0).
    $ git commit -qm "surgical closeout"       → rc=0 (stub gitreins-guard PASS)

Go gates:

    $ go build ./...   → rc=0
    $ go vet ./...     → rc=0
    $ go test ./... -count=1 -short → exit 1
      37 packages ok; single FAIL:
        FAIL github.com/deployBunker/bunker/internal/hermes 600.084s
      (package test timeout with TestIntegration_SkillLifecycle running at the
      deadline; host loadavg 15-36 on 16 cores during the run. This change is
      shell-only — .hermes/scripts/*, scripts/install-git-hooks.sh, this file —
      and touches no Go package.)

gitreins guard: run by the pre-commit gate on the commit that ships this
artifact; its verdict is in the commit's run record.
