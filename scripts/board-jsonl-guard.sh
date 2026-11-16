#!/usr/bin/env bash
# BOARD-GUARD-002 / RELEASE-026: refuse board JSONL files that are corrupt.
#
# Usage:
#   scripts/board-jsonl-guard.sh [path ...]        # files or directories
#
# With no arguments it defaults to checking BOTH board files:
#   .coding-hermes/board/tasks.jsonl
#   .coding-hermes/board/events.jsonl
#
# A directory argument expands to every *.jsonl file directly inside it.
#
# Failure modes (each exits 1 with a greppable diagnostic):
#   * a missing file                     "board-jsonl-guard: <path> not found"
#   * a stash-conflict marker line       "board-jsonl-guard: line N of <path>: conflict marker ..."
#     (lines matching ^(<<<<<<<|=======|>>>>>>>)( |$))
#   * any line that is not valid JSON    "board-jsonl-guard: line N is not valid JSON"
#
# Exit 0 only if every file passed, with a PASS line summarising files/lines.
# Fail-closed: an unreadable or missing board file is a failure, never a skip.

set -euo pipefail

DEFAULT_BOARD_DIR=".coding-hermes/board"

# Collect the list of files to check.
FILES=()
if [ "$#" -eq 0 ]; then
	for f in "$DEFAULT_BOARD_DIR/tasks.jsonl" "$DEFAULT_BOARD_DIR/events.jsonl"; do
		FILES+=("$f")
	done
else
	for arg in "$@"; do
		if [ -d "$arg" ]; then
			shopt -s nullglob
			for f in "$arg"/*.jsonl; do
				FILES+=("$f")
			done
			shopt -u nullglob
		else
			FILES+=("$arg")
		fi
	done
fi

if [ "${#FILES[@]}" -eq 0 ]; then
	echo "board-jsonl-guard: no .jsonl files found to check" >&2
	exit 1
fi

TOTAL_BAD=0
TOTAL_LINES=0
FILES_OK=0

for FILE in "${FILES[@]}"; do
	if [ ! -f "$FILE" ]; then
		echo "board-jsonl-guard: $FILE not found" >&2
		TOTAL_BAD=$((TOTAL_BAD + 1))
		continue
	fi

	LINE_NUM=0
	BAD_LINES=0
	while IFS= read -r line || [[ -n "$line" ]]; do
		LINE_NUM=$((LINE_NUM + 1))
		# Stash-conflict markers get their own, named diagnostic so an
		# operator sees the cause, not just a parse failure.
		if [[ "$line" =~ ^(<<<<<<<|=======|>>>>>>>)(\ |$) ]]; then
			echo "board-jsonl-guard: line $LINE_NUM of $FILE: conflict marker (stash/merge residue): ${line:0:40}" >&2
			BAD_LINES=$((BAD_LINES + 1))
			continue
		fi
		if ! printf '%s\n' "$line" | jq -e . >/dev/null 2>&1; then
			echo "board-jsonl-guard: line $LINE_NUM of $FILE is not valid JSON" >&2
			BAD_LINES=$((BAD_LINES + 1))
		fi
	done < "$FILE"

	TOTAL_LINES=$((TOTAL_LINES + LINE_NUM))
	if [ "$BAD_LINES" -gt 0 ]; then
		echo "board-jsonl-guard: FAIL $FILE ($BAD_LINES bad lines out of $LINE_NUM)" >&2
		TOTAL_BAD=$((TOTAL_BAD + BAD_LINES))
	else
		FILES_OK=$((FILES_OK + 1))
	fi
done

if [ "$TOTAL_BAD" -gt 0 ]; then
	echo "board-jsonl-guard: FAIL ($TOTAL_BAD bad lines across $(( ${#FILES[@]} )) file(s), $TOTAL_LINES lines checked)" >&2
	exit 1
fi

echo "board-jsonl-guard: PASS ($FILES_OK file(s), $TOTAL_LINES lines, all valid JSON, no conflict markers)"
exit 0
