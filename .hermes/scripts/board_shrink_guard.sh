#!/usr/bin/env bash
# board_shrink_guard.sh — refuse board-shrinking tasks.jsonl writes (BOARD-GUARD-001).
#
# WHY THIS EXISTS
# ---------------
# Commit 63140484 ("foreman: close QA-BUNKER-093 on e1aca15e (tick 713)",
# 2026-11-02 23:33:25) rewrote .coding-hermes/board/tasks.jsonl from 849 rows
# to 1. The writer was an ad-hoc inline closeout:
#
#     python3 -c "
#     import json
#     with open('.coding-hermes/board/tasks.jsonl') as f:
#         for line in f:
#             row = json.loads(line)
#             if row.get('id') == 'QA-BUNKER-093':
#                 row['status'] = 'complete'
#                 ...
#                 print(json.dumps(row))
#     " >> .coding-hermes/board/tasks.jsonl.tmp \
#       && mv .coding-hermes/board/tasks.jsonl.tmp .coding-hermes/board/tasks.jsonl
#
# The print() lives INSIDE the matching if, so every non-matching row is read
# and silently dropped; the redirect starts from an empty .tmp, so the mv
# replaces an 849-row board with the single re-serialized row. Sibling commit
# 459b59f3 (same session, same tick) repeated the pattern.
#
# A closeout must APPEND an event and SURGICALLY REPLACE one line — it must
# never re-emit a filtered subset of the board. This guard enforces exactly
# that invariant at the write path:
#
#   1. SHRINK RULE — a tasks.jsonl write may not drop more rows than the
#      caller explicitly allows (--allow-shrink N / BOARD_SHRINK_ALLOW,
#      default 0). A one-row surgical closeout keeps the row count and passes;
#      a deliberate archive/split must say so out loud.
#   2. PATH RULE — a staged file NAMED tasks.jsonl is only legitimate at
#      .coding-hermes/board/tasks.jsonl. A stray tasks.jsonl at the repo root
#      (the /home/kara/deployBunker/bunker/tasks.jsonl RELEASE-003 class) is a
#      writer that decided the board path itself, and is refused.
#
# MODES
#   --old OLD --new NEW   standalone: compare two JSONL files by row count.
#   --staged              hook mode: compare the staged (index) blob of
#                         .coding-hermes/board/tasks.jsonl against the HEAD
#                         blob, and apply the path rule to all staged files.
#   --allow-shrink N      permit dropping up to N rows (default: $BOARD_SHRINK_ALLOW, else 0).
#
# EXIT CODES
#   0  write is legitimate (no shrink beyond allowance, no stray tasks.jsonl)
#   1  REFUSED — the write would shrink the board beyond the allowance, or a
#      staged file named tasks.jsonl is not the canonical board path
#   2  usage / guard error (bad flags, not a git repo in --staged mode)
#
# No dependencies beyond bash, awk and git. No network, no root.

set -u

PROG="board_shrink_guard"
CANONICAL=".coding-hermes/board/tasks.jsonl"
allow="${BOARD_SHRINK_ALLOW:-0}"

die_usage() {
	echo "usage: $PROG --old OLD --new NEW [--allow-shrink N] | --staged [--allow-shrink N]" >&2
	exit 2
}

mode=""
old=""
new=""
while [ $# -gt 0 ]; do
	case "$1" in
	--old) [ $# -ge 2 ] || die_usage; old="$2"; shift 2 ;;
	--new) [ $# -ge 2 ] || die_usage; new="$2"; shift 2 ;;
	--staged) mode="staged"; shift ;;
	--allow-shrink)
		[ $# -ge 2 ] || die_usage
		allow="$2"
		shift 2
		;;
	*) echo "$PROG: unknown argument: $1" >&2; die_usage ;;
	esac
done

case "$allow" in
''|*[!0-9]*)
	echo "$PROG: --allow-shrink/BOARD_SHRINK_ALLOW must be a non-negative integer, got '$allow'" >&2
	exit 2
	;;
esac

# Count rows as non-empty lines: blank lines are not rows, and a missing
# trailing newline must not hide or invent a row.
rows_of_file() {
	awk 'NF{n++} END{print n+0}' "$1"
}

rows_of_stdin() {
	awk 'NF{n++} END{print n+0}'
}

refuse_shrink() {
	# $1 label (path or OLD->NEW), $2 old rows, $3 new rows, $4 allowance
	local dropped=$(( $2 - $3 ))
	echo "$PROG: REFUSED — $1 would drop $dropped rows ($2 -> $3; allowed $4)." >&2
	echo "  A board closeout replaces ONE line in place; it never re-emits a filtered" >&2
	echo "  subset. If this shrink is a deliberate archive/split, say so explicitly:" >&2
	echo "  re-run with --allow-shrink <n> or BOARD_SHRINK_ALLOW=<n>." >&2
	exit 1
}

check_pair() {
	# $1 old file, $2 new file, $3 label for messages
	local old_rows new_rows
	old_rows="$(rows_of_file "$1")"
	new_rows="$(rows_of_file "$2")"
	if [ "$new_rows" -lt $(( old_rows - allow )) ]; then
		refuse_shrink "$3" "$old_rows" "$new_rows" "$allow"
	fi
}

if [ -z "$mode" ]; then
	# ---- standalone mode -------------------------------------------------
	[ -n "$old" ] && [ -n "$new" ] || die_usage
	[ -f "$old" ] || { echo "$PROG: no such file: $old" >&2; exit 2; }
	[ -f "$new" ] || { echo "$PROG: no such file: $new" >&2; exit 2; }
	check_pair "$old" "$new" "$old -> $new"
	exit 0
fi

# ---- staged (hook) mode --------------------------------------------------
root="$(git rev-parse --show-toplevel 2>/dev/null)" || {
	echo "$PROG: --staged requires a git repository" >&2
	exit 2
}
cd "$root" || exit 2

# Path rule: any staged file NAMED tasks.jsonl must be the canonical board.
while IFS= read -r p; do
	[ -n "$p" ] || continue
	case "$p" in
	"$CANONICAL") ;;
	*/tasks.jsonl|tasks.jsonl)
		echo "$PROG: REFUSED — staged file '$p' is named tasks.jsonl but is not" >&2
		echo "  the canonical board path '$CANONICAL'. A writer decided the board" >&2
		echo "  path itself (the deployBunker root tasks.jsonl class). Stage board" >&2
		echo "  rows at the canonical path only." >&2
		exit 1
		;;
	esac
done < <(git diff --cached --name-only --diff-filter=ACMR 2>/dev/null)

# Shrink rule: staged canonical board vs HEAD baseline. A board staged for
# DELETION is a shrink to zero rows and is checked the same way.
in_index=0
in_head=0
git cat-file -e ":$CANONICAL" 2>/dev/null && in_index=1
git cat-file -e "HEAD:$CANONICAL" 2>/dev/null && in_head=1
if [ "$in_head" = 1 ]; then
	old_rows="$(git show "HEAD:$CANONICAL" 2>/dev/null | rows_of_stdin)"
	if [ "$in_index" = 1 ]; then
		new_rows="$(git show ":$CANONICAL" 2>/dev/null | rows_of_stdin)"
	else
		new_rows=0
	fi
	if [ "$new_rows" -lt $(( old_rows - allow )) ]; then
		refuse_shrink "staged $CANONICAL" "$old_rows" "$new_rows" "$allow"
	fi
fi
# No HEAD baseline (board first appears in this commit): nothing to shrink from.

exit 0
