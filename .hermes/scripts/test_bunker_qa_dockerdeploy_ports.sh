#!/bin/bash
# QA-BUNKER-27 — regression test: the docker-deploy arm must extract and PROBE
# bind-address-qualified compose mappings ("127.0.0.1:31299:80"). History: the
# generator's two patterns (quote-adjacent bare digits:digits + whitespace-
# adjacent bare digits:digits) matched NEITHER arm of a qualified mapping, so
# zero ports were probed and a healthy deploy graded 'no HTTP response within
# 60s (probe deadline exhausted)' while compose up itself was rc=0 (duckbrain
# 2026-09-27). The same generated leg carried a heredoc-expanded $0 in the awk
# dedupe (seen[.hermes/scripts/bunker-qa.sh] in the emitted bytes).
# Arms (1-2 red on the pre-fix harness):
#   1. the board's verbatim pre-fix pipeline on the acceptance fixture prints
#      NOTHING (the bug, pinned so a revert re-fails here);
#   2. the harness's stage-one alternation through the same pipeline shape
#      prints exactly 127.0.0.1 (the acceptance property);
#   3. extraction table: quoted/unquoted ip:host:container, bare host:container,
#      dedupe across files, head -3 cap, [ipv6]-bracket noise still yields the
#      host port;
#   4. block eval with a stubbed curl: the dotted host is probed at its own
#      host:port and the cell reports it (AC-2 mechanism);
#   5. block eval on a compose file with no mappings: cell grades UNVERIFIED,
#      never a deploy success;
#   6. rendered script: awk dedupe arrives as !seen[$0]++ (live $0), no host-
#      path contamination, UNVERIFIED gate present, per-URL quoting present.
# Mirrors the render-based discipline of test_bunker_qa_disconnect_leg.sh.
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bunker-qa.sh}"
pass=0; fail=0
check() { # <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  ok   $1"; pass=$((pass+1))
  else echo "  FAIL $1: expected [$2] got [$3]"; fail=$((fail+1)); fi
}
gcount() { grep -c "$@" 2>/dev/null; :; } # grep -c always prints a count

TMPD="${TMPDIR:-/tmp}/qa_bunker_27.$$"
mkdir -p "$TMPD"
trap 'rm -rf "$TMPD"' EXIT

# Arm 0: the harness must still parse.
if bash -n "$HARNESS" 2>"$TMPD/syn.err"; then
  check "harness parses under bash -n" yes yes
else
  check "harness parses under bash -n" yes "no: $(cat "$TMPD/syn.err")"
fi

# Extract the stage-one alternation from the harness's docker-deploy loop (the
# 'for m in $( ... ); do' line body) and the whole QA-BUNKER-27 block for evals.
# awk index() on a literal: the file bytes carry a single backslash before $(,
# which a sed BRE/ERE address cannot match without adding ANOTHER backslash.
EXTRACT_LINE=$(awk '{ i = index($0, "for m in \\$( "); if (i) { s = substr($0, i + length("for m in \\$( ")); sub(/\); do$/, "", s); print s; exit } }' "$HARNESS")
if [ -n "$EXTRACT_LINE" ]; then
  check "docker-deploy extraction present in harness" yes yes
else
  check "docker-deploy extraction present in harness" yes no
fi
PIPELINE=$(printf '%s' "$EXTRACT_LINE" | sed 's/[\\]\$/$/g')

# Extract the whole QA-BUNKER-27 block (comment .. closing fi) for block evals.
BLOCK=$(sed -n '/^    # QA-BUNKER-27 (2026-10-29)/,/^    fi$/p' "$HARNESS" | sed 's/[\\]\$/$/g')
if [ -n "$BLOCK" ]; then
  check "QA-BUNKER-27 block present in harness" yes yes
else
  check "QA-BUNKER-27 block present in harness" yes no
fi

# The acceptance fixture (board AC-1, verbatim bytes).
mkfix() { # <dir> <extra compose body lines...>
  local d="$1"; shift
  mkdir -p "$d"
  { printf 'services:\n  app:\n    ports:\n'
    [ $# -gt 0 ] && printf '%s\n' "$@"
    :; } > "$d/docker-compose.yml"
}

# Arms 1+2 (board AC-1, verbatim fixture bytes): the OLD pipeline prints NOTHING
# on the qualified mapping (the bug, pinned so a revert re-fails here) while the
# harness's alternation through the same pipeline shape prints exactly 127.0.0.1.
mkfix "$TMPD/ac1" '      - "127.0.0.1:31299:80"'

# Arm 1 (RED on pre-fix): the board's verbatim old pipeline prints nothing.
{ grep -hoE '"[0-9]+:[0-9]+"' "$TMPD/ac1/docker-compose.yml"; \
  grep -hoE '[[:space:]][0-9]+:[0-9]+' "$TMPD/ac1/docker-compose.yml"; } \
  | grep -oE '[0-9]+:[0-9]+' | awk '!seen[$0]++' | cut -d: -f1 > "$TMPD/ac1.out" 2>/dev/null
check "pre-fix pipeline prints nothing on qualified mapping (red arm)" "" "$(cat "$TMPD/ac1.out")"

# Arm 2 (the acceptance property): the harness's stage-one alternation through
# the same pipeline shape prints exactly 127.0.0.1.
( cd "$TMPD/ac1" && eval "$PIPELINE" ) 2>/dev/null | grep -oE '([0-9]{1,3}\.){3}[0-9]+:[0-9]+|[0-9]+:[0-9]+' | awk '!seen[$0]++' | cut -d: -f1 > "$TMPD/ac1new.out"
check "harness alternation prints exactly 127.0.0.1 through the AC pipeline" "127.0.0.1" "$(cat "$TMPD/ac1new.out")"

# Arm 3: extraction table (tokens = stage-one output after dedupe + head -3).
toks() { ( cd "$1" && eval "$PIPELINE" ) 2>/dev/null; }

mkfix "$TMPD/q-quoted" '      - "127.0.0.1:31299:80"'
check "quoted ip:host:container extracted" "127.0.0.1:31299" "$(toks "$TMPD/q-quoted")"

mkfix "$TMPD/q-unquoted" '      - 127.0.0.1:31299:80'
check "unquoted ip:host:container extracted" "127.0.0.1:31299" "$(toks "$TMPD/q-unquoted")"

mkfix "$TMPD/q-bare" '      - "9000:9000"'
check "quoted bare host:container intact" "9000:9000" "$(toks "$TMPD/q-bare")"

mkfix "$TMPD/q-bare2" '      - 8443:8443'
check "unquoted bare host:container intact" "8443:8443" "$(toks "$TMPD/q-bare2")"

mkdir -p "$TMPD/q-dedupe"
printf 'services:\n  app:\n    ports:\n      - "9000:9000"\n' > "$TMPD/q-dedupe/docker-compose.yml"
printf 'services:\n  app:\n    ports:\n      - "9000:9000"\n' > "$TMPD/q-dedupe/compose.yml"
check "duplicate mapping across files deduped" "9000:9000" "$(toks "$TMPD/q-dedupe")"

mkfix "$TMPD/q-mixed" '      - "127.0.0.1:31299:80"' '      - "9000:9000"' '      - 8443:8443' '      - "10.0.0.5:7000:7000"' '      - 6000:6000' '      - 5000:5000'
check "mixed forms, first three after dedupe" "127.0.0.1:31299
9000:9000
8443:8443" "$(toks "$TMPD/q-mixed")"

mkfix "$TMPD/q-v6" '      - "[::1]:31299:80"'
check "bracketed non-dotted host still yields the host port" "31299:80" "$(toks "$TMPD/q-v6")"

# Arm 4: block eval, stubbed curl — the dotted host is probed at its own
# host:port and the OK cell names it (AC-2 mechanism, real control flow).
run_block() { # <dir> <curl-body>
  local dir="$1" curl_body="$2"
  (
    cd "$dir" || exit 1
    cell() { printf 'CELL\t%s\t%s\t%s\n' "$1" "$2" "$3"; }
    curl() { printf '%s' "$curl_body"; }
    eval "$BLOCK"
  ) 2>/dev/null
}

mkfix "$TMPD/probe" '      - "127.0.0.1:31299:80"'
OUT=$(run_block "$TMPD/probe" 200)
case "$OUT" in
  *$'docker-deploy\tOK'*'probe 200 on 127.0.0.1:31299'*)
    check "dotted host probed at its own host:port, OK cell names it" yes yes ;;
  *)
    check "dotted host probed at its own host:port, OK cell names it" yes "no: [$OUT]" ;;
esac

# Arm 5: a compose file with NO parseable mappings must grade UNVERIFIED.
mkdir -p "$TMPD/noports"
printf 'services:\n  app:\n    image: nginx:alpine\n' > "$TMPD/noports/docker-compose.yml"
OUT=$(run_block "$TMPD/noports" 200)
case "$OUT" in
  *$'docker-deploy\tUNVERIFIED'*)
    check "zero probed ports grades UNVERIFIED" yes yes ;;
  *)
    check "zero probed ports grades UNVERIFIED" yes "no: [$OUT]" ;;
esac

# Arm 6: the generated script carries the escaped awk dedupe (live $0 agent-
# side, no host-path contamination) and the UNVERIFIED gate.
gen_render() { # <dir> <outfile>
  local d="$1" o="$2"
  mkdir -p "$d"
  printf 'module example.com/qa27render\n\ngo 1.21\n' > "$d/go.mod"
  BUNKER_QA_SKIP_PREFLIGHT=1 bash "$HARNESS" __gen-remote-detected "$d" > "$o" 2>/dev/null
}
gen_render "$TMPD/render" "$TMPD/render.sh"
if [ -s "$TMPD/render.sh" ]; then
  check "harness renders a generated script" yes yes
else
  check "harness renders a generated script" yes no
fi
check 'render carries live agent-side awk dedupe (escaped \$0 generator-side)' \
  "$(gcount -F "awk '!seen[\$0]++'" "$TMPD/render.sh")" "1"
check "render free of host-path $0 contamination" \
  "$(gcount -F 'seen[.hermes/scripts/bunker-qa.sh]' "$TMPD/render.sh")" "0"
check "render carries the UNVERIFIED zero-port gate" \
  "$(gcount -F 'probed_n' "$TMPD/render.sh")" "3"
check "render probes a quoted URL per normalized host:port" \
  "$(gcount -F '"http://$hp_host:$hp_port/"' "$TMPD/render.sh")" "1"
check "render drops the bare-digits-only cut tail" \
  "$(gcount -F 'cut -d: -f1 ); do' "$TMPD/render.sh")" "0"

echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
