#!/usr/bin/env bash
# BOARD-GUARD-004: census of task-board commit_hash evidence bindings.
#
# Answers one question per board row: is the recorded commit_hash a real,
# verifiable binding, or an unverifiable placeholder?
#
#   full       full 40-hex, commit exists in this repo
#   abbrev     short sha (7-39 hex) that `git cat-file` still resolves
#   multi      several shas, every one resolvable
#   disposed   no sha, but explicit disposition text ("n/a", "none:...",
#              "foreman-direct", "EVIDENCE", "Merge wt/X into main")
#   foreign    sha-like token that does not resolve here while the row's own
#              text labels it as another repository (coding-hermes-tools,
#              kara-hermes, "~/.hermes/scripts", "meta-repo", "ops repo",
#              "host-local", "fleet-shared", "not tracked in the bunker repo")
#   documented sha-like token that does not resolve here, and the row carries
#              an explicit BOARD-GUARD-004 verification note saying why
#   stale      sha-like token that does not resolve and has no disposition at
#              all -- the only UNVERIFIABLE class
#
# An abbreviated sha is NOT a defect when git resolves it: `git rev-parse`
# turns it into exactly one commit. Do not "repair" those by hand -- rewriting
# hundreds of historical rows is churn, and expanding a short sha that came
# from a DIFFERENT repository binds the row to an unrelated commit here.
#
# Exit contract (a matrix checker exits 0 on a well-formed board and non-zero
# only on integrity violations):
#   0  every line parses, no undocumented stale bindings
#   1  at least one line is not valid JSON (integrity violation)
#   2  at least one undocumented stale binding (evidence cannot be verified)
#
# Usage: scripts/board-census-commit-hash.sh [--board PATH] [--json OUT] [--quiet]

set -uo pipefail

BOARD=".coding-hermes/board/tasks.jsonl"
JSON_OUT=""
QUIET=0

while (($# > 0)); do
  case "$1" in
    --board) BOARD="$2"; shift 2 ;;
    --json) JSON_OUT="$2"; shift 2 ;;
    --quiet) QUIET=1; shift ;;
    -h|--help) sed -n '2,38p' "$0"; exit 0 ;;
    *) echo "board-census: unknown argument $1" >&2; exit 1 ;;
  esac
done

[[ -f "$BOARD" ]] || { echo "board-census: $BOARD not found" >&2; exit 1; }

JQ_ROW='
  def ctx: ([ ((.foreman_note // "")|tostring), ((.worker_summary // "")|tostring), ((.reasoning // "")|tostring) ]
            | join(" ") | ascii_downcase);
  ctx as $c
  | [ (.id|tostring),
      (.status|tostring),
      ((.commit_hash // "")|tostring),
      (if (($c|contains("coding-hermes-tools")) or ($c|contains("kara-hermes"))
           or ($c|contains("meta-repo")) or ($c|contains("ops repo"))
           or ($c|contains("host-local")) or ($c|contains("fleet-shared"))
           or ($c|contains(".hermes/scripts"))
           or ($c|contains("not tracked in the bunker repo")))
       then "1" else "0" end),
      (if (((.foreman_note // "")|tostring)|ascii_downcase|contains("board-guard-004:"))
       then "1" else "0" end)
    ] | join("\u001F")'

bad_report=""
stale_report=""
foreign_report=""
disposed_report=""
declare -A hist=([full]=0 [abbrev]=0 [multi]=0 [disposed]=0 [foreign]=0 [documented]=0 [stale]=0)
lines_total=0
bad_lines=0
rows_with_hash=0
short_values=0

resolve_token() {
  # echoes the full sha when the token names a commit in this repo
  local tok="$1" kind
  kind="$(git cat-file -t "$tok" 2>/dev/null)" || return 1
  [[ "$kind" == "commit" ]] || return 1
  git rev-parse --verify --quiet "${tok}^{commit}" 2>/dev/null
}

while IFS= read -r line || [[ -n "$line" ]]; do
  [[ -z "${line//[[:space:]]/}" ]] && continue
  lines_total=$((lines_total + 1))

  if ! row="$(printf '%s' "$line" | jq -r "$JQ_ROW" 2>/dev/null)"; then
    bad_lines=$((bad_lines + 1))
    bad_report+="  line ${lines_total}: not valid JSON
    ${line:0:140}
"
    continue
  fi

  IFS=$'\x1f' read -r id status value foreignflag docflag <<< "$row"
  [[ -z "${value:-}" ]] && continue
  rows_with_hash=$((rows_with_hash + 1))
  (( ${#value} < 40 )) && short_values=$((short_values + 1))

  lowered="${value,,}"
  labelled_foreign=0
  case "$lowered" in
    *meta-repo*|*deployed*|*"other repo"*|*"external repo"*|*"not in this repo"*|*"ops repo"*|*host-local*) labelled_foreign=1 ;;
  esac

  mapfile -t tokens < <(printf '%s' "$lowered" | grep -oE '[0-9a-f]{7,40}' || true)

  if (( ${#tokens[@]} == 0 )); then
    hist[disposed]=$(( ${hist[disposed]} + 1 ))
    disposed_report+="  line ${lines_total}"$'\t'"${id}"$'\t'"[${status}]"$'\t'"${value}"$'\n'
    continue
  fi

  tok_unresolved=""
  for tok in "${tokens[@]}"; do
    resolve_token "$tok" >/dev/null || tok_unresolved+="${tok} "
  done

  if [[ -z "$tok_unresolved" ]]; then
    if (( ${#tokens[@]} > 1 )); then
      klass="multi"
    elif [[ "$lowered" =~ ^[0-9a-f]{40}$ ]]; then
      klass="full"
    else
      klass="abbrev"
    fi
  elif [[ "$docflag" == "1" ]]; then
    klass="documented"
  elif (( labelled_foreign == 1 )) || [[ "$foreignflag" == "1" ]]; then
    klass="foreign"
  else
    klass="stale"
  fi

  hist[$klass]=$(( ${hist[$klass]} + 1 ))
  case "$klass" in
    foreign)    foreign_report+="  line ${lines_total}"$'\t'"${id}"$'\t'"[${status}]"$'\t'"${value}"$'\n' ;;
    documented) foreign_report+="  line ${lines_total}"$'\t'"${id}"$'\t'"[${status}]"$'\t'"${value}"$'\t'"documented-unreachable"$'\n' ;;
    stale)      stale_report+="  line ${lines_total}"$'\t'"${id}"$'\t'"[${status}]"$'\t'"${value}"$'\t'"unresolved: ${tok_unresolved}"$'\n' ;;
  esac
done < "$BOARD"

verdict="PASS"
rc=0
if (( bad_lines > 0 )); then verdict="FAIL(unparseable-lines)"; rc=1; fi
if (( hist[stale] > 0 )); then
  if (( rc == 0 )); then verdict="FAIL(stale-bindings)"; rc=2; else verdict+="+FAIL(stale-bindings)"; fi
fi

if (( QUIET == 0 )); then
  if (( bad_lines > 0 )); then
    printf -- '-- UNPARSEABLE LINES (integrity violation) --\n%s\n' "$bad_report"
  fi
  if (( hist[stale] > 0 )); then
    printf -- '-- STALE -- sha-like value this repo does not contain, no disposition (repair or document) --\n%s\n' "$stale_report"
  fi
  if (( hist[foreign] + hist[documented] > 0 )); then
    printf -- '-- FOREIGN / DOCUMENTED -- not verifiable in this repo, disposition recorded on the row --\n%s\n' "$foreign_report"
  fi
  if (( hist[disposed] > 0 )); then
    printf -- '-- DISPOSED -- no sha, explicit disposition text --\n%s\n' "$disposed_report"
  fi
fi

printf 'board=%s\n' "$BOARD"
printf 'lines_total=%d lines_unparseable=%d\n' "$lines_total" "$bad_lines"
printf 'rows_with_commit_hash=%d\n' "$rows_with_hash"
printf 'classes: full=%d abbrev=%d multi=%d disposed=%d foreign=%d documented=%d stale=%d\n' \
  "${hist[full]}" "${hist[abbrev]}" "${hist[multi]}" "${hist[disposed]}" "${hist[foreign]}" "${hist[documented]}" "${hist[stale]}"
printf 'rows_with_short_non_full_value=%d (abbreviated values that git resolves are VERIFIED, not defects)\n' "$short_values"
printf 'verdict=%s rc=%d\n' "$verdict" "$rc"

if [[ -n "$JSON_OUT" ]]; then
  jq -nc \
    --arg board "$BOARD" \
    --argjson lines_total "$lines_total" \
    --argjson lines_unparseable "$bad_lines" \
    --argjson rows_with_commit_hash "$rows_with_hash" \
    --argjson rows_with_short_non_full_value "$short_values" \
    --argjson full "${hist[full]}" --argjson abbrev "${hist[abbrev]}" \
    --argjson multi "${hist[multi]}" --argjson disposed "${hist[disposed]}" \
    --argjson foreign "${hist[foreign]}" --argjson documented "${hist[documented]}" \
    --argjson stale "${hist[stale]}" \
    --arg verdict "$verdict" \
    '{board:$board, lines_total:$lines_total, lines_unparseable:$lines_unparseable,
      rows_with_commit_hash:$rows_with_commit_hash,
      rows_with_short_non_full_value:$rows_with_short_non_full_value,
      classes:{full:$full, abbrev:$abbrev, multi:$multi, disposed:$disposed,
               foreign:$foreign, documented:$documented, stale:$stale},
      verdict:$verdict}' > "$JSON_OUT"
  printf 'json=%s\n' "$JSON_OUT"
fi

exit "$rc"
