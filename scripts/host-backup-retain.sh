#!/usr/bin/env bash
# host-backup-retain.sh — keep-N-per-agent retention for bunker backup tarballs.
#
# GROUPING RULE (documented per deliverable spec):
#   A backup file's agent prefix is everything in its basename BEFORE the first
#   timestamp-like token, where a timestamp-like token matches the backup
#   timestamp format [0-9]{8}T[0-9]{6}Z (e.g. the 20261118T134954Z in
#   "bunker-30f6e54d-20261118T134954Z.tar.gz" -> prefix "bunker-30f6e54d").
#   This never splits an agent hash that itself contains a digit run (e.g.
#   "bunker-d2156465-..."). Files sharing a prefix form one group; within a
#   group only the newest N (by mtime, default N=3) are kept. A file with no
#   timestamp token groups by its full extension-less basename, i.e. it forms
#   a singleton group of its own.
#
# SAFETY:
#   - Only regular files directly inside --dir are considered; subdirectories
#     and dotfiles are never touched.
#   - A group with fewer than N files is REFUSED: no member of that group is
#     ever deleted, because its entire set IS the keep-set.
#   - --dry-run prints the candidate list with bytes and deletes nothing.

set -euo pipefail

KEEP=3
DIR=/var/backups/bunker
DRY_RUN=0
PREFIX_GLOB='*'

usage() {
  echo "usage: $0 [--keep N] [--dry-run] [--dir DIR] [--prefix-glob GLOB]" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --keep)        [ $# -ge 2 ] || usage; KEEP="$2"; shift 2 ;;
    --dir)         [ $# -ge 2 ] || usage; DIR="$2"; shift 2 ;;
    --prefix-glob) [ $# -ge 2 ] || usage; PREFIX_GLOB="$2"; shift 2 ;;
    --dry-run)     DRY_RUN=1; shift ;;
    -h|--help)     usage ;;
    *)             usage ;;
  esac
done

case "$KEEP" in
  ''|*[!0-9]*) echo "ERROR: --keep must be a positive integer, got '$KEEP'" >&2; exit 2 ;;
esac
if [ "$KEEP" -lt 1 ]; then
  echo "ERROR: --keep must be >= 1 (refusing to keep nothing)" >&2
  exit 2
fi
if [ ! -d "$DIR" ]; then
  echo "ERROR: backup dir not found: $DIR" >&2
  exit 2
fi

# Agent prefix of one filename: text before the FIRST timestamp-like token.
# POSIX ERE matching is leftmost, so sed trims from the first timestamp to the
# end of the stem; a stem without one is left whole (singleton group).
prefix_of() {
  local stem
  stem="${1%.*}"
  printf '%s' "$stem" | sed -E 's/[0-9]{8}T[0-9]{6}Z.*$//'
}

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

# basename<TAB>mtime-epoch<TAB>size for every regular file in DIR.
find "$DIR" -maxdepth 1 -type f ! -name '.*' -printf '%f\t%T@\t%s\n' > "$tmpdir/files"
if [ ! -s "$tmpdir/files" ]; then
  echo "no backup files in $DIR; nothing to do"
  exit 0
fi

# group<TAB>mtime<TAB>size<TAB>basename, groups sorted, newest first within group.
: > "$tmpdir/grouped"
while IFS=$'\t' read -r fname mtime size; do
  grp="$(prefix_of "$fname")"
  # shellcheck disable=SC2254  # PREFIX_GLOB is an intentional user glob pattern
  case "$grp" in
    $PREFIX_GLOB) ;;
    *) continue ;;
  esac
  printf '%s\t%s\t%s\t%s\n' "$grp" "$mtime" "$size" "$fname"
done < "$tmpdir/files" | sort -t $'\t' -k1,1 -k2,2rn > "$tmpdir/grouped"

if [ ! -s "$tmpdir/grouped" ]; then
  echo "no files match --prefix-glob '$PREFIX_GLOB'; nothing to do"
  exit 0
fi

# Walk the grouped list; rank members within each group newest-first.
cur_grp=''
grp_count=0
rank=0
: > "$tmpdir/plan"
while IFS=$'\t' read -r grp mtime size fname; do
  if [ "$grp" != "$cur_grp" ]; then
    cur_grp="$grp"
    rank=0
    grp_count=$(awk -F'\t' -v g="$grp" '$1==g{c++} END{print c+0}' "$tmpdir/grouped")
    if [ "$grp_count" -lt "$KEEP" ]; then
      echo "REFUSED group '$grp': $grp_count file(s) < keep=$KEEP; keeping all"
    fi
  fi
  rank=$((rank + 1))
  if [ "$grp_count" -lt "$KEEP" ]; then
    continue
  fi
  if [ "$rank" -le "$KEEP" ]; then
    echo "KEEP   ${fname} ($size bytes)"
  else
    echo "DELETE ${fname} ($size bytes)"
    printf '%s\t%s\n' "$DIR/$fname" "$size" >> "$tmpdir/plan"
  fi
done < "$tmpdir/grouped"

if [ ! -s "$tmpdir/plan" ]; then
  echo "nothing beyond-newest-$KEEP per agent prefix; no deletions"
  exit 0
fi

total_bytes=$(awk -F'\t' '{s+=$2} END{print s+0}' "$tmpdir/plan")
n_files=$(wc -l < "$tmpdir/plan")

if [ "$DRY_RUN" -eq 1 ]; then
  echo "--- DRY RUN: would delete $n_files file(s), freeing $total_bytes bytes"
  exit 0
fi

while IFS=$'\t' read -r path size; do
  rm -f -- "$path"
  echo "deleted $path ($size bytes)"
done < "$tmpdir/plan"

echo "--- deleted $n_files file(s), freed $total_bytes bytes"
