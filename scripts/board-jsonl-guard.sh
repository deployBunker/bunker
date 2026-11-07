#!/usr/bin/env bash
# BOARD-GUARD-002: refuse tasks.jsonl writes that contain unescaped newlines
# Usage: scripts/board-jsonl-guard.sh <tasks.jsonl>
# Exit 0 if every line is valid JSON, exit 1 otherwise

set -euo pipefail

FILE="${1:-.coding-hermes/board/tasks.jsonl}"

if [[ ! -f "$FILE" ]]; then
  echo "board-jsonl-guard: $FILE not found" >&2
  exit 1
fi

LINE_NUM=0
BAD_LINES=0
while IFS= read -r line || [[ -n "$line" ]]; do
  LINE_NUM=$((LINE_NUM + 1))
  if ! echo "$line" | jq -e . >/dev/null 2>&1; then
    echo "board-jsonl-guard: line $LINE_NUM is not valid JSON" >&2
    BAD_LINES=$((BAD_LINES + 1))
  fi
done < "$FILE"

if [[ $BAD_LINES -gt 0 ]]; then
  echo "board-jsonl-guard: FAIL ($BAD_LINES bad lines out of $LINE_NUM)" >&2
  exit 1
fi

echo "board-jsonl-guard: PASS ($LINE_NUM lines, all valid JSON)"
exit 0
