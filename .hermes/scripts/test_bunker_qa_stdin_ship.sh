#!/usr/bin/env bash
# QA-BUNKER-64 — harness test that LOCKS the stdin-ship transport of qa-run.sh
# (bunker-qa.sh launch()/run()). History: the OLD transport embedded the whole
# generated remote script as ONE ssh argv word (`echo '$b64' | base64 -d`), which
# died E2BIG ("Argument list too long") once the script passed the kernel's
# MAX_ARG_STRLEN (131072). Fix commit 048ffa07a ships it via ssh STDIN:
#   build_remote_script ... | agent_ssh "cat > ~/qa-run.sh && bash ~/qa-run.sh"
#   build_remote_script ... | agent_ssh "cat > ~/qa-run.sh && echo SCRIPT_BYTES=... && ( nohup setsid bash ~/qa-run.sh </dev/null >~/qa-run.log 2>&1 & ) && echo LAUNCHED"
# This test drives the REAL bodies (extracted verbatim from bunker-qa.sh, never a
# paraphrase) with a PATH-shim ssh that captures argv + stdin bytes, and asserts:
#   1. ship-command contract:  the captured argv contains "cat > ~/qa-run.sh"
#   2. stdin integrity:        the stdin bytes EQUAL the generated script bytes
#   3. E2BIG tripwire:         NO captured argv word exceeds MAX_ARG_WORD_LIMIT
# Plus a REGRESSION arm (`--self-test`): a simulated echo-b64 argv transport
# (the pre-fix shape, base64-argv-in-one-word) MUST fail the same assertions.
# The generated script (~99KB for the placeholder render) sits at ~75% of the
# kernel MAX_ARG_STRLEN (131072) — i.e. it would NOT trip the kernel limit yet,
# so argv-based transports that survive TODAY would break the moment the script
# grows ~32KB more; the tripwire limit below (60000) locks the transport CLASS
# (stdin-ship), not a byte threshold. No network, no real ssh, no root.
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$HOME/.hermes/scripts/bunker-qa.sh}"
pass=0; fail=0
check() { # <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  ok   $1 -> $3"; pass=$((pass+1))
  else echo "  FAIL $1: expected $2 got $3"; fail=$((fail+1)); fi
}

# ── transport verdict helpers (each returns NON-ZERO on any violation) ──
MAX_ARG_WORD_LIMIT=60000
# argv logs are NUL-delimited words; tr keeps them grep-able without bash's
# "ignored null byte" command-substitution warnings
ship_cmd_present() { tr -d '\0' < "$1" | grep -a -q -F -- 'cat > ~/qa-run.sh'; }
stdin_eq_script() { cmp -s "$1" "$2"; }
max_word_len() { # <argv-log> -> length of the longest whitespace-delimited word
  tr -s '[:space:]' '\n' < "$1" | sed '/^$/d' | awk '{ if (length($0) > m) m = length($0) } END { print m + 0 }'
}
verdict_transport_ok() { # <argv-log> <stdin-file> <expected-script> -> 0 only when the stdin-ship contract holds
  local rc=0
  ship_cmd_present "$1" || rc=1
  [ -s "$2" ] && stdin_eq_script "$2" "$3" || rc=1
  [ "$(max_word_len "$1")" -le "$MAX_ARG_WORD_LIMIT" ] || rc=1
  return "$rc"
}

# ── function-body extraction from the real harness (verbatim, not paraphrase) ──
# build_remote_script() wraps ~1300 lines of remote-script CONTENT in a quoted
# heredoc: bare `}` and pseudo function defs (cell(), fail_detail(), ...) inside
# are DATA, not code. A state-aware walk (heredoc-delimiter tracking) is required
# — a naive grep/brace extraction returns garbage or swallows the real closing
# brace. Every extracted body is bash -n checked below, so extraction drift (a
# harness edit that breaks the extractor) fails the test loudly.
extract_top_function() { # <name> <outfile> -> 0 when def found + bare-} terminator hit
  local name="$1" out="$2" line on=0 inhd=0 delim=""
  # regex in a variable: an unquoted `<<` inside [[ =~ ]] is parsed as a redirect
  # token by bash ("unexpected token `<<-'"), a variable is treated as the regex.
  local hdre="(^|[^<])<<-?[[:space:]]*[\'\"]?([A-Za-z_][A-Za-z0-9_]*)[\'\"]?\)?([[:space:]]|$)"
  : > "$out"
  while IFS= read -r line; do
    if [ "$on" -eq 0 ]; then
      if [[ "$line" =~ ^${name}\(\)[[:space:]]+\{ ]]; then
        on=1
        printf '%s\n' "$line" >> "$out"
      fi
      continue
    fi
    if [ "$inhd" -eq 1 ]; then
      printf '%s\n' "$line" >> "$out"
      if [ "$line" = "$delim" ]; then inhd=0; fi
      continue
    fi
    # heredoc OPEN: `<<[-]DELIM`, optionally quoted, optionally inside $(...).
    # The (^|[^<]) guard keeps `<<<` here-strings (read -ra x <<< "$y") from
    # being misread as heredocs, which would stick the walker in heredoc state.
    if [[ "$line" =~ $hdre ]]; then
      inhd=1
      delim="${BASH_REMATCH[2]}"
      printf '%s\n' "$line" >> "$out"
      continue
    fi
    printf '%s\n' "$line" >> "$out"
    if [ "$line" = "}" ]; then on=0; return 0; fi
  done < "$HARNESS"
  return 1
}

# ── validate the extraction before anything consumes it ──
if [ ! -f "$HARNESS" ]; then
  echo "FATAL: harness not found at $HARNESS" >&2
  exit 1
fi
TD=$(mktemp -d /tmp/qa64.XXXXXX)
mkdir -p "$TD/home" "$TD/home/.bunker" "$TD/shim/home" "$TD/repo"
# preflights() requires the server to exist in ~/.bunker/config.yaml; the ssh
# reachability probe passes because the shim ssh exits 0 for any invocation.
mkdir -p "$TD/home/.bunker"
printf '    qa64-shim.invalid:\n' > "$TD/home/.bunker/config.yaml"
for fn in log write_fail_if_empty preflights detect_cmds npm_pkg_name npm_pkg_publishable detect_publish_pkg_name detect_upgrade_inputs ship_prev_tag_tree sync_repo build_remote_script spawn_agent qa_destroy_agent agent_ssh run launch; do
  extract_top_function "$fn" "$TD/fn_$fn.sh" || { echo "FATAL: extract $fn failed (def or bare-} terminator not found)" >&2; exit 1; }
  if ! bash -n "$TD/fn_$fn.sh" 2>"$TD/fn_$fn.err"; then
    echo "FATAL: extracted body $fn does not parse:" >&2
    cat "$TD/fn_$fn.err" >&2
    exit 1
  fi
done
check "run body extracted (>=100 lines)" yes "$( [ "$(wc -l < "$TD/fn_run.sh")" -ge 100 ] && echo yes || echo no)"
check "launch body extracted (>=100 lines)" yes "$( [ "$(wc -l < "$TD/fn_launch.sh")" -ge 100 ] && echo yes || echo no)"
check "ship line present in real run body" yes "$( grep -aqF 'cat > ~/qa-run.sh' "$TD/fn_run.sh" && echo yes || echo no)"
check "ship line present in real launch body" yes "$( grep -aqF 'cat > ~/qa-run.sh' "$TD/fn_launch.sh" && echo yes || echo no)"
# prologue (env defaults: SERVER/EVIDENCE/TTL/BUNKER_QA_HOST_KEY...) — the
# extracted bodies EXECUTE against these globals. Slice = everything above the
# log() definition, self-located so prologue edits don't break the line number.
PROLOGUE_END=$(grep -n '^log()[[:space:]]\+{' "$HARNESS" | head -1 | cut -d: -f1)
sed -n "1,$((PROLOGUE_END - 1))p" "$HARNESS" > "$TD/prologue.sh"
bash -n "$TD/prologue.sh" || { echo "FATAL: prologue slice does not parse" >&2; exit 1; }

# ── minimal repo fixture (a git repo with a HEAD for `git archive`) ──
git -C "$TD/repo" init -q
git -C "$TD/repo" -c user.name=qa-bot -c user.email=qa@bunker commit -q --allow-empty -m seed

# ── PATH shim: fake ssh + fake bunker (+timeout passthrough), capture argv+stdin ──
for t in nohup setsid; do
  printf '#!/bin/sh\nexec "$@"\n' > "$TD/shim/$t"
  chmod +x "$TD/shim/$t"
done
cat > "$TD/shim/timeout" <<'SHIM'
#!/bin/sh
# pass-through timeout: drop the leading numeric duration, then exec the command
# (`timeout 30 ssh ...` — without this, exec would run a program named `30`)
[ $# -gt 0 ] && case "$1" in *[!0-9]*) ;; *) shift ;; esac
exec "$@"
SHIM
chmod +x "$TD/shim/timeout"
cat > "$TD/shim/ssh" <<'SHIM'
#!/bin/bash
# QA-BUNKER-64 shim: capture argv (NUL-delimited words) + stdin bytes, then
# behave like a fake remote: `cat > ~/qa-run.sh` stores stdin; SCRIPT_BYTES /
# SYNC-OK / LAUNCHED / evidence echoes make the real launch()/run() code run to
# completion without any network or real ssh.
D="$(dirname "$0")"
printf '%s\0' "$@" >> "$D/argv.log"
CMD="$*"
case "$CMD" in
  *"cat > ~/qa-run.sh"*)
    # stdin-ship: consume the pipe (EOF arrives when the producer finishes)
    cat > "$D/stdin.last"
    cp "$D/stdin.last" "$D/qa-run.sh"
    if [[ "$CMD" == *"SCRIPT_BYTES="* ]]; then
      echo "SCRIPT_BYTES=$(wc -c < "$D/qa-run.sh")"
      echo "LAUNCHED"
    else
      touch "$D/qa-run.finished"
      echo "BATTERY-DONE"
    fi
    printf '{"project":"repo","cell":"shim","status":"OK","detail":"shim","ts":"2026-10-06T00:00:00Z"}\n' > "$D/home/qa-evidence.jsonl"
    ;;
  *"-xf -"*)
    # repo sync stream (`tar -C ~/qa-<proj> -xf -`): drain the tar pipe, confirm
    cat > /dev/null
    echo "SYNC-OK"
    ;;
  *"cat ~/qa-evidence.jsonl"*)
    # evidence pull (run): the fake agent-side evidence written by the ship arm
    cat "$D/home/qa-evidence.jsonl" 2>/dev/null
    ;;
  *)
    # preflight/hygiene/probe calls never read stdin — do NOT touch it here,
    # `cat` would block on the caller's held-open stdin until the real
    # `timeout` kills the shim and the preflight grades the host unreachable.
    true
    ;;
esac
exit 0
SHIM
cat > "$TD/shim/bunker" <<'SHIM'
#!/bin/bash
# fake bunker CLI: `spawn` prints an id-shaped line; destroy/list succeed.
case "${1:-}" in
  spawn) echo "keys/aaa11111" ;;
  destroy|list) exit 0 ;;
  *) exit 0 ;;
esac
SHIM
chmod +x "$TD/shim/ssh" "$TD/shim/bunker"

# ── driver: run the REAL extracted launch()/run() in a sandbox HOME ──
drive() { # <body.sh> <argv-log> <stdin-file> <evidence>  (uses $TD, repo fixture, shim)
  local body="$1" argvlog="$2" stdinfile="$3" ev="$4"
  : > "$argvlog"; : > "$stdinfile"
  local rc=0
  (
    export HOME="$TD/home"
    export PATH="$TD/shim:$PATH"
    export BUNKER_QA_EVIDENCE="$ev"
    export BUNKER_QA_SERVER="qa64-shim.invalid"
    export BUNKER_QA_HOST_KEY="$TD/home/dummy-hostkey"
    export BUNKER_QA_SKIP_PREFLIGHT=1
    export BUNKER_QA_SSH_TIMEOUT=5
    export BUNKER_QA_RETRY_SLEEP=1
    export BUNKER_QA_DESTROY_BACKOFF=1
    cd "$TD/repo"
    # source the prologue (env/global defaults) + real bodies in dep order
    local f
    source "$TD/prologue.sh"
    for f in log write_fail_if_empty preflights detect_cmds npm_pkg_name npm_pkg_publishable detect_publish_pkg_name detect_upgrade_inputs ship_prev_tag_tree sync_repo build_remote_script spawn_agent qa_destroy_agent agent_ssh run launch; do
      source "$TD/fn_$f.sh"
    done
    case "$(basename "$body")" in
      fn_launch.sh) launch "$TD/repo" ;;
      fn_run.sh)    run "$TD/repo" ;;
    esac
  ) >"$TD/cell.out" 2>"$TD/cell.err"
  rc=$?
  cp "$TD/shim/argv.log" "$argvlog" 2>/dev/null || : > "$argvlog"
  cp "$TD/shim/stdin.last" "$stdinfile" 2>/dev/null || : > "$stdinfile"
  rm -f "$TD/shim/argv.log" "$TD/shim/stdin.last" "$TD/shim/qa-run.sh" "$TD/shim/qa-run.finished" "$TD/home/qa-evidence.jsonl"
  return "$rc"
}

echo
echo "=== launch(): real body ships qa-run.sh via ssh STDIN"
G_SCRIPT="$TD/gen.sh"
# the fake bunker spawn prints keys/aaa11111 and the drive repo dir is named
# `repo` (PROJ is its basename). The EXPECTED script must come from the same
# REAL detection the drive runs, and from a render whose $PATH and $0 match the
# drive exactly: build_remote_script's heredoc is UNQUOTED — it deliberately
# expands $PATH (toolchain-failure comment) and $0 (an awk self-exclusion guard)
# at render time — so the fixture is rendered through the SAME extracted bodies
# with `bash -c ... "$0"` carrying the test's own $0 into the child.
TD="$TD" PATH="$TD/shim:$PATH" BUNKER_QA_SERVER=qa64-shim.invalid \
BUNKER_QA_HOST_KEY="$TD/home/dummy-hostkey" BUNKER_QA_EVIDENCE="$TD/gen.ev.jsonl" \
bash -c '
  source "$TD/prologue.sh"
  for f in log write_fail_if_empty preflights detect_cmds npm_pkg_name npm_pkg_publishable detect_publish_pkg_name detect_upgrade_inputs ship_prev_tag_tree sync_repo build_remote_script spawn_agent qa_destroy_agent agent_ssh run launch; do
    source "$TD/fn_$f.sh"
  done
  cd "$TD"
  agent=aaa11111
  detect_cmds repo aaa11111
  build_remote_script repo "$DETECT_INSTALL" "$DETECT_CI" "$DETECT_NATIVE"
' "$0" > "$G_SCRIPT"
check "generated fixture script non-empty" yes "$( [ -s "$G_SCRIPT" ] && echo yes || echo no)"
L_RC=0
drive "$TD/fn_launch.sh" "$TD/launch.argv" "$TD/launch.stdin" "$TD/launch.ev.jsonl" || L_RC=$?
check "launch() exits rc=0 under shim" 0 "$L_RC"
check "LAUNCHED printed" yes "$( grep -aq 'LAUNCHED agent=' "$TD/cell.out" && echo yes || echo no)"
check "launch ship command contains 'cat > ~/qa-run.sh &&'" yes "$( tr -d '\0' < "$TD/launch.argv" | grep -aqF 'cat > ~/qa-run.sh &&' && echo yes || echo no)"
check "launch stdin byte-identical to the generated script" yes "$( stdin_eq_script "$TD/launch.stdin" "$G_SCRIPT" && echo yes || echo no)"
check "launch stdin non-empty" yes "$( [ -s "$TD/launch.stdin" ] && echo yes || echo no)"
check "launch max argv word within tripwire" yes "$( [ "$(max_word_len "$TD/launch.argv")" -le "$MAX_ARG_WORD_LIMIT" ] && echo yes || echo no)"
check "launch evidence carries the launch cell" yes "$( grep -aq '"cell":"launch"' "$TD/launch.ev.jsonl" && echo yes || echo no)"

echo
echo "=== run(): real body ships qa-run.sh via ssh STDIN"
R_RC=0
drive "$TD/fn_run.sh" "$TD/run.argv" "$TD/run.stdin" "$TD/run.ev.jsonl" || R_RC=$?
check "run() exits rc=0 under shim" 0 "$R_RC"
check "run ship command contains 'cat > ~/qa-run.sh &&'" yes "$( tr -d '\0' < "$TD/run.argv" | grep -aqF 'cat > ~/qa-run.sh &&' && echo yes || echo no)"
check "run stdin byte-identical to the generated script" yes "$( stdin_eq_script "$TD/run.stdin" "$G_SCRIPT" && echo yes || echo no)"
check "run stdin non-empty" yes "$( [ -s "$TD/run.stdin" ] && echo yes || echo no)"
check "run max argv word within tripwire" yes "$( [ "$(max_word_len "$TD/run.argv")" -le "$MAX_ARG_WORD_LIMIT" ] && echo yes || echo no)"
check "run evidence carries the battery row" yes "$( grep -aq '"cell":"shim"' "$TD/run.ev.jsonl" && echo yes || echo no)"

echo
echo "=== verdict helper + REGRESSION arm: echo-b64 argv transport MUST fail"
check "verdict_transport_ok accepts the real stdin-ship (launch)" 0 "$( verdict_transport_ok "$TD/launch.argv" "$TD/launch.stdin" "$G_SCRIPT"; echo $?)"
check "verdict_transport_ok accepts the real stdin-ship (run)" 0 "$( verdict_transport_ok "$TD/run.argv" "$TD/run.stdin" "$G_SCRIPT"; echo $?)"
# Simulated PRE-FIX transport shape: the whole base64 payload embedded as ONE
# ssh argv word (`echo '<b64>' | base64 -d | bash`). 90KB payload exceeds the
# 60000-byte tripwire exactly the way the ~99KB real render would exceed it.
BA="$TD/b64.argv"
{ printf '%s\0' ssh -p 2223 -o IdentitiesOnly=yes "bunker-x@qa64-shim.invalid"
  printf '%s' "echo '"
  head -c 90000 /dev/zero | tr '\0' 'A' | base64 | tr -d '\n'
  printf "' | base64 -d | bash"
  printf '\0'
} > "$BA"
check "regression fixture carries one giant argv word" yes "$( [ "$(max_word_len "$BA")" -gt "$MAX_ARG_WORD_LIMIT" ] && echo yes || echo no)"
check "regression fixture lacks the stdin-ship command" yes "$( ship_cmd_present "$BA" && echo no || echo yes)"
check "regression arm: verdict FAILS the echo-b64 argv shape" 1 "$( verdict_transport_ok "$BA" "$G_SCRIPT" "$G_SCRIPT"; echo $?)"

echo
echo "=== --self-test: negative arm against the known-bad fixture only"
if [ "${1:-}" = "--self-test" ]; then
  # Run ONLY the regression assertions against the known-bad fixture: the verdict
  # helper must reject it. This proves the test BITES without touching the harness.
  ST=0
  [ "$(max_word_len "$BA")" -gt "$MAX_ARG_WORD_LIMIT" ] || ST=1
  verdict_transport_ok "$BA" "$G_SCRIPT" "$G_SCRIPT" 2>/dev/null && ST=1
  if [ "$ST" -eq 0 ]; then
    echo "SELF-TEST PASS: known-bad echo-b64 transport is rejected by the tripwire"
    exit 0
  else
    echo "SELF-TEST FAIL: known-bad echo-b64 transport was NOT rejected"
    exit 1
  fi
fi

echo
echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
