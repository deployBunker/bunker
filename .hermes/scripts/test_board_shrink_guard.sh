#!/usr/bin/env bash
# test_board_shrink_guard.sh — RED/GREEN battery for board_shrink_guard.sh
# (BOARD-GUARD-001).
#
# RED  : a simulated replay of the real defect — an 849-row compact
#        .coding-hermes/board/tasks.jsonl rewritten to the single spaced
#        19-key row commit 63140484 left behind — must be REFUSED (rc != 0)
#        and the refusal must NAME the shrink (849 -> 1, 848 rows dropped).
# GREEN: (a) the corrected writer — a surgical one-line closeout of the same
#        849-row board (every other line byte-identical) — passes rc=0;
#        (b) the same 849 -> 1 rewrite passes rc=0 ONLY with an explicit
#        --allow-shrink 848 (or BOARD_SHRINK_ALLOW=848).
# Also exercised: the hook (--staged) mode against a scratch git repository —
# refusal on a staged truncation, refusal on a stray root-level tasks.jsonl
# (the deployBunker RELEASE-003 class), pass on the staged surgical closeout.
#
# Self-contained: fixtures are generated locally under a temp dir; the only
# tools used are bash, awk, seq, git and the guard itself. No network.
#
# usage: bash .hermes/scripts/test_board_shrink_guard.sh
# exit:  0 all arms behaved; 1 one or more arms misbehaved (each printed).

set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
GUARD="$HERE/board_shrink_guard.sh"

# Host ambient allowance must never flip an arm green.
unset BOARD_SHRINK_ALLOW

TMP="$(mktemp -d "${TMPDIR:-/tmp}/bbsg.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fails=0
red_arms=0
green_arms=0

ok() { printf 'GREEN: %s rc=%s\n' "$1" "$2"; green_arms=$((green_arms + 1)); }
bad() {
	printf 'UNEXPECTED: %s\n  wanted %s\n  got: %s\n' "$1" "$2" "$3" >&2
	fails=$((fails + 1))
}

rows_of() { awk 'NF{n++} END{print n+0}' "$1"; }

# ---------------------------------------------------------------------------
# Fixture — the real defect shape, rebuilt locally.
#
# BOARD-849 mirrors the board as it stood before commit 63140484: 848 compact
# filler rows plus the QA-BUNKER-093 row in its ORIGINAL compact form, with the
# status-first key order the row carried on the real board.
# TRUNC-1 is byte-shaped like the row 63140484 actually left behind: the same
# row re-serialized by python json.dumps defaults — SPACED separators and
# ensure_ascii escapes — with the closeout fields stamped in.
# ---------------------------------------------------------------------------
BOARD="$TMP/board-849.jsonl"
TRUNC="$TMP/board-1.jsonl"

: >"$BOARD"
i=0
while [ "$i" -lt 848 ]; do
	printf '{"id":"ROW-%04d","title":"filler","status":"pending","priority":"P3","complexity":2}\n' "$i" >>"$BOARD"
	i=$((i + 1))
done
printf '%s\n' '{"status":"pending","id":"QA-BUNKER-093","title":"[P3] test_bunker_qa_stdin_ship.sh is standing-red: 12 of 23 checks fail at base 3810ad15","detail":{"fingerprint":"88df89e9f1402dd1fd47da24cb25bb04fcbe3d8dc5a911c752e2b1a188a4cb82"},"priority":"P3","source":null,"reasoning":"standing-red","acceptance_criteria":"23/23 green","created_at":"2026-11-03T00:18:07Z","updated_at":"2026-11-03T00:18:07Z","worker_status":"pending","foreman_note":null,"worktree":null,"branch":null,"commit_hash":null,"guard_result":null,"ci_result":null,"worker_summary":null,"complexity":2}' >>"$BOARD"

printf '%s\n' '{"status": "complete", "id": "QA-BUNKER-093", "title": "[P3] test_bunker_qa_stdin_ship.sh is standing-red: 12 of 23 checks fail at base 3810ad15", "detail": {"fingerprint": "88df89e9f1402dd1fd47da24cb25bb04fcbe3d8dc5a911c752e2b1a188a4cb82"}, "priority": "P3", "source": null, "reasoning": "standing-red", "acceptance_criteria": "23/23 green", "created_at": "2026-11-03T00:18:07Z", "updated_at": "2026-11-03T00:18:07Z", "worker_status": "complete", "foreman_note": "COMPLETE tick 713 (worker deepseek-v4-flash): default stdin-ship test to repo harness, 23/23 green.", "worktree": null, "branch": null, "commit_hash": "e1aca15e", "guard_result": null, "ci_result": null, "worker_summary": null, "complexity": 2}' >"$TRUNC"

[ "$(rows_of "$BOARD")" = 849 ] || { echo "FATAL: fixture board is not 849 rows" >&2; exit 1; }
[ "$(rows_of "$TRUNC")" = 1 ] || { echo "FATAL: fixture truncation is not 1 row" >&2; exit 1; }

# ---------------------------------------------------------------------------
# RED arm — the simulated 849 -> 1 rewrite must be refused, and named.
# ---------------------------------------------------------------------------
out="$(bash "$GUARD" --old "$BOARD" --new "$TRUNC" 2>&1)"
rc=$?
printf 'RED: board_shrink_guard.sh --old board-849.jsonl --new board-1.jsonl rc=%s\n' "$rc"
if [ "$rc" -eq 0 ]; then
	bad "RED arm: 849->1 rewrite was ACCEPTED" "rc != 0" "rc=0"
elif ! printf '%s' "$out" | grep -q -- '849 -> 1' || ! printf '%s' "$out" | grep -q -- 'drop 848 rows'; then
	bad "RED arm: refusal does not NAME the shrink" "output naming '849 -> 1' and 'drop 848 rows'" "output: $out"
else
	red_arms=$((red_arms + 1))
fi

# ---------------------------------------------------------------------------
# GREEN (a) — the corrected writer: a surgical one-line closeout. Every
# non-target row must be byte-identical; only the QA-BUNKER-093 line changes.
# ---------------------------------------------------------------------------
CLOSED="$TMP/board-849-closed.jsonl"
awk '{
	if (index($0, "\"id\":\"QA-BUNKER-093\"") > 0) {
		gsub(/"status":"pending"/, "\"status\":\"complete\"")
		gsub(/"worker_status":"pending"/, "\"worker_status\":\"complete\"")
		gsub(/"commit_hash":null/, "\"commit_hash\":\"e1aca15e\"")
	}
	print
}' "$BOARD" >"$CLOSED"

changed=$(diff "$BOARD" "$CLOSED" | grep -c '^<') || true
if [ "$(rows_of "$CLOSED")" != 849 ] || [ "$changed" != 1 ]; then
	bad "GREEN(a) fixture: surgical closeout must keep 849 rows and change exactly 1 line" "849 rows / 1 changed" "$(rows_of "$CLOSED") rows / $changed changed"
else
	out="$(bash "$GUARD" --old "$BOARD" --new "$CLOSED" 2>&1)"
	rc=$?
	if [ "$rc" -eq 0 ]; then
		ok "board_shrink_guard.sh --old board-849.jsonl --new board-849-closed.jsonl (surgical 1-line closeout)" "$rc"
	else
		bad "GREEN(a): surgical closeout refused" "rc=0" "rc=$rc out: $out"
	fi
fi

# ---------------------------------------------------------------------------
# GREEN (b) — the same 849 -> 1 rewrite passes only with an explicit
# allowance (flag form and env form).
# ---------------------------------------------------------------------------
out="$(bash "$GUARD" --old "$BOARD" --new "$TRUNC" --allow-shrink 848 2>&1)"
rc=$?
if [ "$rc" -eq 0 ]; then
	ok "board_shrink_guard.sh --old board-849.jsonl --new board-1.jsonl --allow-shrink 848" "$rc"
else
	bad "GREEN(b): explicitly allowed shrink refused" "rc=0" "rc=$rc out: $out"
fi

out="$(BOARD_SHRINK_ALLOW=848 bash "$GUARD" --old "$BOARD" --new "$TRUNC" 2>&1)"
rc=$?
if [ "$rc" -eq 0 ]; then
	ok "BOARD_SHRINK_ALLOW=848 board_shrink_guard.sh --old board-849.jsonl --new board-1.jsonl" "$rc"
else
	bad "GREEN(b): env-allowed shrink refused" "rc=0" "rc=$rc out: $out"
fi

# An allowance that is too small must still refuse.
out="$(bash "$GUARD" --old "$BOARD" --new "$TRUNC" --allow-shrink 847 2>&1)"
rc=$?
printf 'RED: board_shrink_guard.sh --allow-shrink 847 (insufficient) rc=%s\n' "$rc"
if [ "$rc" -ne 0 ]; then
	red_arms=$((red_arms + 1))
else
	bad "insufficient allowance accepted" "rc != 0" "rc=0"
fi

# ---------------------------------------------------------------------------
# Hook (--staged) mode against a scratch git repository.
# ---------------------------------------------------------------------------
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null

R="$TMP/mini"
git init -q "$R" || { echo "FATAL: cannot init scratch repo" >&2; exit 1; }
cd "$R"
git config user.email "board-guard@test.local"
git config user.name "board-guard test"

mkdir -p .coding-hermes/board
cp "$BOARD" .coding-hermes/board/tasks.jsonl
git add .coding-hermes/board/tasks.jsonl
git -c core.hooksPath=/dev/null commit -qm "base: 849-row board"

# RED staged — staged truncation refused.
cp "$TRUNC" .coding-hermes/board/tasks.jsonl
git add .coding-hermes/board/tasks.jsonl
out="$(bash "$GUARD" --staged 2>&1)"
rc=$?
printf 'RED: board_shrink_guard.sh --staged (849->1 staged) rc=%s\n' "$rc"
if [ "$rc" -ne 0 ]; then
	if printf '%s' "$out" | grep -q -- '849 -> 1'; then
		red_arms=$((red_arms + 1))
	else
		bad "staged RED does not name the shrink" "output naming '849 -> 1'" "output: $out"
	fi
else
	bad "staged 849->1 accepted" "rc != 0" "rc=0"
fi

# GREEN staged — allowed shrink, then the surgical closeout.
out="$(BOARD_SHRINK_ALLOW=848 bash "$GUARD" --staged 2>&1)"
rc=$?
if [ "$rc" -eq 0 ]; then
	ok "BOARD_SHRINK_ALLOW=848 board_shrink_guard.sh --staged" "$rc"
else
	bad "staged allowed shrink refused" "rc=0" "rc=$rc out: $out"
fi

git checkout -q -- .coding-hermes/board/tasks.jsonl
cp "$CLOSED" .coding-hermes/board/tasks.jsonl
git add .coding-hermes/board/tasks.jsonl
out="$(bash "$GUARD" --staged 2>&1)"
rc=$?
if [ "$rc" -eq 0 ]; then
	ok "board_shrink_guard.sh --staged (surgical 1-line closeout)" "$rc"
else
	bad "staged surgical closeout refused" "rc=0" "rc=$rc out: $out"
fi

# RED staged — stray root-level tasks.jsonl (the deployBunker class).
git reset -q
printf '%s\n' '{"id": "RELEASE-003", "status": "blocked"}' >tasks.jsonl
git add tasks.jsonl
out="$(bash "$GUARD" --staged 2>&1)"
rc=$?
printf 'RED: board_shrink_guard.sh --staged (stray root tasks.jsonl) rc=%s\n' "$rc"
if [ "$rc" -ne 0 ]; then
	if printf '%s' "$out" | grep -q "tasks.jsonl but is not"; then
		red_arms=$((red_arms + 1))
	else
		bad "path-rule refusal does not name the stray path" "output naming the stray path" "output: $out"
	fi
else
	bad "stray root tasks.jsonl accepted" "rc != 0" "rc=0"
fi

# ---------------------------------------------------------------------------
printf 'board_shrink_guard battery: %s RED arms, %s GREEN arms, %s unexpected\n' "$red_arms" "$green_arms" "$fails"
if [ "$fails" -gt 0 ]; then
	echo "test_board_shrink_guard.sh: FAIL ($fails arms misbehaved)" >&2
	exit 1
fi
if [ "$red_arms" -lt 2 ] || [ "$green_arms" -lt 5 ]; then
	echo "test_board_shrink_guard.sh: FAIL (expected >=2 RED and >=5 GREEN arms, ran $red_arms/$green_arms)" >&2
	exit 1
fi
echo "test_board_shrink_guard.sh: PASS"
exit 0
