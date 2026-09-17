#!/bin/bash
# Bunker E2E Test Battery — Practical
# Tests what works, documents what doesn't
set -euo pipefail
#
# REQUIREMENT: this battery MUST run as root. It creates Linux users via
# useradd, installs PAM/systemd artifacts, and writes a root-owned daemon log
# under /var/log (see README "Run E2E battery"). Run it as:
#   sudo bash e2e-full-battery.sh
# EXIT CODES:
#   0   = success (final line "STATUS: ALL CORE TESTS PASS" / "VERIFY-PASS",
#         or the --self-test diagnostics check completed)
#   42  = preflight refusal (not root, or the daemon log path is not
#         writable) — the reason is printed to stderr, nothing was changed
#   1   = battery ran and failed, or a non-preflight error killed the run
#         (the ERR trap prints the failing line + command before exit)
# --SELF-TEST: `bash e2e-full-battery.sh --self-test` verifies the harness's
#   own diagnostics (capture helper, ERR trap, non-root refusal decision,
#   binary-commit verdicts, and — INT-CI-010 — the CLI-state isolation and the
#   input precedence: an explicit BUNKERD_REST_ADDR / BUNKER_TOKEN wins, an
#   unset BUNKER_TOKEN resolves to source=default, and a CLI invocation writes
#   the battery's own config while the operator's stays byte-identical; and —
#   INT-CI-012 — the nested suite's isolated ports and single invocation site,
#   section 12's verdict flipping on a failing nested fixture, and the removal
#   helper refusing any path that is not this harness's own scratch)
#   WITHOUT root: it never creates users, never starts daemons, never writes
#   /var/log, and never runs a battery section. Final line:
#   SELF-TEST: PASS
# --BIN-REPORT: `bash e2e-full-battery.sh --bin-report` prints the binary
#   certification banner (which binary reports which commit vs repo HEAD)
#   with ZERO side effects and no root requirement — same output as the
#   certification preflight of a full run, nothing else. Exit 0 on
#   MATCH/SKIP, non-zero on MISMATCH (DF-BUNKER-3 / QA-BUNKER-3).
# --SHOW-PLAN: `bash e2e-full-battery.sh --show-plan` prints the RESOLVED run
#   plan (both binary paths + the certification verdict, the daemon
#   endpoints, the token SOURCE and a masked fingerprint, the battery's own
#   CLI state dir, the operator CLI config path, and which sections run) with
#   ZERO side effects and no root requirement. It never prints the token.
#
# INPUTS (all optional — an explicitly exported value ALWAYS wins):
#   BUNKER_BIN         CLI binary under test        (default /usr/local/bin/bunker)
#   BUNKERD_BIN        daemon binary                (default /usr/local/bin/bunkerd)
#   BUNKER_TOKEN       auth token for the daemon    (default test-regression-token)
#   BUNKER_DAEMON_URL  connect target               (default http://localhost:$REST_PORT)
#   BUNKERD_REST_ADDR  REST listen address          (default :18080, :28081 in coexist)
#   BUNKERD_GRPC_ADDR  gRPC listen address          (default :19090, :29091 in coexist)
#   BUNKERD_COEXIST    non-empty = coexist mode (own daemon, own ports, no
#                      production user sweep) instead of standalone take-over
#   BUNKER_STRICT_BIN  =1 makes a certification MISMATCH fatal before any
#                      host mutation
#
# DAEMON OWNERSHIP / THE STANDALONE CONTRACT (INT-CI-012):
#  * The battery NEVER starts or stops the host's daemon: in standalone mode it
#    talks to the daemon the host already runs on the production ports, and in
#    coexist mode it starts its OWN daemon on its own ports.
#  * Section 12 runs regression-tests.sh with its OWN CLI state dir and its OWN
#    ports (:29092/:28082) in BOTH modes, so the nested suite can never bind —
#    or compete for — the ports this battery is testing. The nested suite stops
#    only the daemon it started itself: a systemd-managed bunkerd is detected
#    (systemctl is-active / MainPID) and left alone, and its agent state
#    (/run/bunker/*, /etc/bunkerd/ssh/*) is not swept.
#  * Section 12's verdict is the nested suite's OWN tally ("PASS: N / FAIL: M")
#    plus its real exit status: a non-zero exit, a non-zero FAIL count, or no
#    tally at all fails the battery.
#  * Sections 13 and 14 probe the daemon (a cheap list) before spawning or
#    destroying anything: an unreachable daemon produces ONE cell naming the
#    endpoint and how to bring it back, not a cascade of "not found" cells.
#
# CLI-STATE ISOLATION (INT-CI-010): the battery runs EVERY CLI invocation with
# BUNKER_HOME *and* HOME pointed at its own throwaway state dir, so it never
# reads or writes the operator's CLI config (`$BUNKER_HOME` or
# `~/.bunker/config.yaml`) — including with a CLI build that predates
# BUNKER_HOME and resolves `~/.bunker` only. The operator config is
# fingerprinted before the first CLI call and re-checked at the end of the run;
# a change fails the run loudly. The previous revision isolated HOME in
# coexist mode only, so a documented standalone run rewrote
# /root/.bunker/config.yaml back to the dev layout.

PASS=0
FAIL=0
NOTE=0

assert() { echo "  ✓ $1"; PASS=$((PASS+1)); }
fail()  { echo "  ✗ $1"; FAIL=$((FAIL+1)); }
note()  { echo "  ⚠ $1"; NOTE=$((NOTE+1)); }

echo "=========================================="
echo " BUNKER END-TO-END TEST BATTERY"
echo " $(date)"
echo "=========================================="
echo ""

# Binary resolution (exactly one statement): BUNKER_BIN env override first,
# falling back to the deployed default /usr/local/bin/bunker. Whatever this
# resolves to is the binary the battery certifies — the certification
# preflight below states it explicitly before any host mutation.
BUNKER="${BUNKER_BIN:-/usr/local/bin/bunker}"

# Coexistence mode (CI on bunker-mvp): the host runs a systemd-managed
# production bunkerd on :19090/:18080. Use dedicated ports + a temp config and
# NEVER wipe production users or touch the live service. Standalone (default):
# full take-over like the original design. In BOTH modes the CLI state is
# isolated (BUNKER_HOME + HOME -> the battery's own throwaway dir), so the
# battery's connect never clobbers the live host's /root/.bunker (INT-CI-010).
BUNKERD_COEXIST="${BUNKERD_COEXIST:-}"
BUNKERD_PID=""
# Feature batteries (GAP-064) may point BUNKERD_BIN at a freshly built daemon
# WITHOUT overwriting the live production bunkerd in coexist mode.
BUNKERD_BIN="${BUNKERD_BIN:-/usr/local/bin/bunkerd}"

# ── Resolved inputs + CLI-state isolation (INT-CI-010) ─────────────────
# Every input is optional and an explicitly exported value ALWAYS wins over
# the default. The previous revision re-exported BUNKER_TOKEN unconditionally
# (clobbering an inherited token with the test token) and forced the
# production ports in standalone mode regardless of BUNKERD_REST_ADDR /
# BUNKERD_GRPC_ADDR.
#
# resolve_inputs() computes the resolved globals and is called once on the
# main path. --self-test re-invokes it in a subshell with a controlled
# environment to prove the precedence rules without touching this process.

# file_fingerprint PATH — "sha256:<hex>" for a readable file, ABSENT when
# there is no file at PATH, UNREADABLE when there is one but it cannot be
# hashed. Used to prove the operator's CLI config is byte-identical after a
# run (the regression this battery had: its connect step rewrote it).
file_fingerprint() {
    local f="$1" h=""
    if [ -f "$f" ]; then
        h="$(sha256sum "$f" 2>/dev/null | awk '{print $1}')"
        printf 'sha256:%s' "${h:-UNREADABLE}"
    else
        printf 'ABSENT'
    fi
}

# token_fingerprint TOKEN — a MASKED identifier for a token: the first 8 hex
# chars of its sha256. The token itself is never printed, so the battery's
# transcript and --show-plan output stay safe to paste into a CI log.
token_fingerprint() {
    printf 'sha256:%s' "$(printf '%s' "${1:-}" | sha256sum | cut -c1-8)"
}

BUNKER_TOKEN_SOURCE=""
BUNKER_DAEMON_URL_SOURCE=""
BUNKERD_GRPC_ADDR_EXPLICIT=""
BUNKERD_REST_ADDR_EXPLICIT=""
GRPC_PORT=""
REST_PORT=""
BUNKER_DAEMON_URL=""
OPERATOR_CLI_CONFIG=""
OPERATOR_CLI_CONFIG_HASH=""
OPERATOR_CONFIG_VIOLATION=0

resolve_inputs() {
    # Token: an inherited value wins; the documented test default applies
    # only when the caller left BUNKER_TOKEN unset or blank.
    if [ -n "${BUNKER_TOKEN:-}" ]; then
        BUNKER_TOKEN_SOURCE="env"
    else
        BUNKER_TOKEN="test-regression-token"
        BUNKER_TOKEN_SOURCE="default"
    fi
    export BUNKER_TOKEN

    # Port inputs: record whether the caller exported one BEFORE applying the
    # documented default, so "the default" stays distinguishable from "an
    # explicit value that happens to equal the default".
    if [ -n "${BUNKERD_GRPC_ADDR:-}" ]; then
        BUNKERD_GRPC_ADDR_EXPLICIT=1
    else
        BUNKERD_GRPC_ADDR_EXPLICIT=""
        BUNKERD_GRPC_ADDR=":29091"
    fi
    if [ -n "${BUNKERD_REST_ADDR:-}" ]; then
        BUNKERD_REST_ADDR_EXPLICIT=1
    else
        BUNKERD_REST_ADDR_EXPLICIT=""
        BUNKERD_REST_ADDR=":28081"
    fi

    # Resolved ports. An explicit address wins in BOTH modes; otherwise the
    # documented layout of the mode applies: coexist = the battery's own
    # daemon (29091/28081), standalone = the production daemon (19090/18080).
    if [ -n "$BUNKERD_GRPC_ADDR_EXPLICIT" ] || [ -n "${BUNKERD_COEXIST:-}" ]; then
        GRPC_PORT="${BUNKERD_GRPC_ADDR#:}"
    else
        GRPC_PORT="19090"
    fi
    if [ -n "$BUNKERD_REST_ADDR_EXPLICIT" ] || [ -n "${BUNKERD_COEXIST:-}" ]; then
        REST_PORT="${BUNKERD_REST_ADDR#:}"
    else
        REST_PORT="18080"
    fi
    # Keep the daemon-config inputs identical to the resolved ports: the
    # coexist temp config and the nested regression suite both read them.
    BUNKERD_GRPC_ADDR=":$GRPC_PORT"
    BUNKERD_REST_ADDR=":$REST_PORT"

    # The address every CLI call targets (section 2 connect, section 13
    # image specs). An exported BUNKER_DAEMON_URL wins — e.g. a TLS-terminated
    # or non-"localhost" URL for the same daemon.
    if [ -n "${BUNKER_DAEMON_URL:-}" ]; then
        BUNKER_DAEMON_URL_SOURCE="env"
    else
        BUNKER_DAEMON_URL="http://localhost:${REST_PORT}"
        BUNKER_DAEMON_URL_SOURCE="default"
    fi

    # Operator CLI config: resolved from the ORIGINAL BUNKER_HOME/HOME, BEFORE
    # this battery exports its own BUNKER_HOME (coexist mode also reassigns
    # HOME further down). This is the file the battery must never touch.
    if [ -n "${BUNKER_HOME:-}" ]; then
        OPERATOR_CLI_CONFIG="${BUNKER_HOME}/config.yaml"
    else
        OPERATOR_CLI_CONFIG="${HOME:-/root}/.bunker/config.yaml"
    fi
    OPERATOR_CLI_CONFIG_HASH="$(file_fingerprint "$OPERATOR_CLI_CONFIG")"
}
resolve_inputs

# battery_cli_state_dir — the battery's OWN CLI state directory, created on
# first use and exported as BOTH BUNKER_HOME and HOME so every CLI invocation
# resolves its config inside this throwaway dir instead of the operator's.
# BUNKER_HOME is the precise knob for a current build; HOME covers a build
# that predates BUNKER_HOME (proven live: the deployed 0.1.3 binary resolves
# ~/.bunker and ignores BUNKER_HOME completely) and any other tool that reads
# $HOME. This is what makes the battery safe on a host whose root CLI points
# at a real daemon (INT-CI-010).
BATTERY_CLI_HOME=""
battery_cli_state_dir() {
    # NOTE: call this with output redirected (`battery_cli_state_dir > /dev/null`),
    # never inside $( ) — a command substitution runs in a subshell, so the
    # BUNKER_HOME/HOME exports would not reach the caller.
    if [ -z "$BATTERY_CLI_HOME" ]; then
        BATTERY_CLI_HOME="$(mktemp -d "${TMPDIR:-/tmp}/bunker-battery-cli-XXXXXX")"
        export BUNKER_HOME="$BATTERY_CLI_HOME"
        export HOME="$BATTERY_CLI_HOME"
    fi
    printf '%s' "$BATTERY_CLI_HOME"
}

# battery_cli_config — the config file the battery's own CLI reads and writes
# (the BUNKER_HOME location; a HOME-only build writes $BATTERY_CLI_HOME/.bunker
# /config.yaml, which is still inside the battery's own scratch dir).
battery_cli_config() {
    battery_cli_state_dir > /dev/null
    printf '%s/config.yaml' "$BATTERY_CLI_HOME"
}

# remove_own_state_dir DIR — remove a scratch dir THIS harness created. Every
# scratch dir it creates lives directly under $TMPDIR with one of the two
# documented prefixes below; anything else (the operator's CLI state dir, a
# HOME, /root, an empty expansion) is REFUSED loudly and left on disk.
# INT-CI-012: the nested suite used to `rm -rf /root/.bunker`, i.e. a harness
# removed a config file it did not create — the operator's own CLI
# registration. A pattern-checked removal cannot do that, whatever a variable
# happens to hold.
remove_own_state_dir() {
    local dir="${1:-}"
    case "$dir" in
        "${TMPDIR:-/tmp}"/bunker-battery-cli-*|"${TMPDIR:-/tmp}"/bunker-battery-nested-cli-*)
            rm -rf "$dir" 2>/dev/null || true
            return 0
            ;;
    esac
    echo "  ⚠ refusing to remove '$dir' — not a scratch dir this harness created (nothing was deleted; operator CLI config: ${OPERATOR_CLI_CONFIG:-<unset>})" >&2
    return 1
}

# bcli — the ONE wrapper every CLI invocation in this battery goes through.
# It forces BUNKER_HOME *and* HOME to the battery's own state dir for the
# duration of the call, so no section (connect, spawn, exec, destroy, ...) can
# read or write the operator's config. It always creates the state dir first:
# an empty BUNKER_HOME would be treated as unset by the CLI and fall back to
# the operator's config.
bcli() {
    # Arm the state dir in THIS shell (not in a subshell — see the note on
    # battery_cli_state_dir) so the run has exactly one state dir.
    battery_cli_state_dir > /dev/null
    HOME="$BATTERY_CLI_HOME" BUNKER_HOME="$BATTERY_CLI_HOME" "$BUNKER" "$@"
}

# operator_config_check — compare the operator's CLI config fingerprint with
# the one captured before the first CLI call. Returns non-zero and prints a
# loud diagnostic ONCE when the file changed: that is exactly the regression
# this isolation exists to prevent, and it must never be a green run.
operator_config_check() {
    local now
    now="$(file_fingerprint "$OPERATOR_CLI_CONFIG")"
    if [ "$now" = "$OPERATOR_CLI_CONFIG_HASH" ]; then
        return 0
    fi
    if [ "$OPERATOR_CONFIG_VIOLATION" != "1" ]; then
        OPERATOR_CONFIG_VIOLATION=1
        echo "" >&2
        echo "  ✗ OPERATOR CLI CONFIG MODIFIED BY THIS RUN" >&2
        echo "    this battery must never read or write the operator's CLI config (INT-CI-010)" >&2
        echo "    path   : $OPERATOR_CLI_CONFIG" >&2
        echo "    before : $OPERATOR_CLI_CONFIG_HASH" >&2
        echo "    after  : $now" >&2
        echo "    the battery's own state dir was: ${BATTERY_CLI_HOME:-<none>}" >&2
    fi
    return 1
}

# GAP-075 section state. Initialized here (not in section 15) so the EXIT trap
# can always reference it under `set -u`, even when the battery dies early.
GAP075_OP_USER=""
GAP075_OP_HOME=""
GAP075_OP_KEYDIR=""
GAP075_RUN_UNIT=""
GAP075_DROPIN_RESTORE=""
GAP075_DROPIN_PATH="/etc/security/namespace.d/50-bunker-agents.conf"
GAP075_HELPER_PATH="/usr/lib/bunker/pam-tmp-guard"
GAP075_HELPER_RESTORE=""
GAP075_HELPERDIR_RESTORE=""
GAP075_MASK_RESTORE=""

# ── Diagnostics helpers (DF-BUNKER-6) ──────────────────────────────────
# run_capture LABEL CMD... — run CMD, capture combined stdout+stderr, and
# make the exit status available WITHOUT letting set -e abort the script.
# Output is stored in RUN_CAPTURE_OUT / RUN_CAPTURE_EXIT. The helper ALWAYS
# returns 0 (a non-zero return would kill a bare call site under set -e —
# the exact silent death this helper exists to prevent); the wrapped
# command's status is reported only via RUN_CAPTURE_EXIT. A failing command
# substitution in a bare `VAR=$(...)` assignment aborts under set -e BEFORE
# the if/else below it can run — that was the silent step-2 death this
# helper replaces.
RUN_CAPTURE_OUT=""
RUN_CAPTURE_EXIT=0
run_capture() {
    local label="$1"; shift
    RUN_CAPTURE_OUT=""
    RUN_CAPTURE_EXIT=0
    set +e
    trap - ERR # the wrapped command's failure is EXPECTED here — handled by the caller's if/else, not the trap
    RUN_CAPTURE_OUT=$("$@" 2>&1)
    RUN_CAPTURE_EXIT=$?
    trap 'diag_err $? $LINENO "$BASH_COMMAND"' ERR
    set -e
    if [ "$RUN_CAPTURE_EXIT" -ne 0 ]; then
        echo "  [capture] $label failed (exit=$RUN_CAPTURE_EXIT)"
    fi
    return 0
}

# ── Section 12/13/14 helpers (INT-CI-012) ──────────────────────────────
# All PURE or read-only: each one is exercised by --self-test with fixtures, so
# the section-12 verdict and the daemon gate can be proven without root, a
# daemon, or a nested suite.

# nested_suite_ports — "<grpc> <rest>" handed to the nested regression suite.
# The SAME isolated, non-production pair in BOTH modes: standalone used to let
# the child inherit this battery's exported BUNKERD_REST_ADDR=:18080 /
# BUNKERD_GRPC_ADDR=:19090, so the child's take-over killed the production
# daemon and then raced it for :18080 (INT-CI-012). Pinned by --self-test.
NESTED_GRPC_ADDR=":29092"
NESTED_REST_ADDR=":28082"
nested_suite_ports() {
    printf '%s %s' "$NESTED_GRPC_ADDR" "$NESTED_REST_ADDR"
}

# nested_tally NESTED_OUTPUT — the nested suite's OWN summary counters
# ("PASS: N / FAIL: M"), echoed as "<pass> <fail>", or "" when the transcript
# carries no tally. The previous revision counted `grep -c "✓\|PASS"` over the
# whole transcript (✓ cells + the word PASS), which is not a result at all.
nested_tally() {
    local out="$1" p="" f=""
    p="$(printf '%s\n' "$out" | grep -oE 'PASS: [0-9]+' | tail -1 | grep -oE '[0-9]+' || true)"
    f="$(printf '%s\n' "$out" | grep -oE 'FAIL: [0-9]+' | tail -1 | grep -oE '[0-9]+' || true)"
    if [ -z "$p" ] || [ -z "$f" ]; then
        printf ''
        return 1
    fi
    printf '%s %s' "$p" "$f"
    return 0
}

# nested_verdict NESTED_EXIT TALLY — PURE verdict string for section 12:
#   PASS: <reason>   the nested suite ran, exited 0 and reported 0 failures
#   FAIL: <reason>   non-zero exit, a non-zero FAIL count, or no tally at all
# A suite that cannot run (dies, is killed, prints nothing) has no tally → FAIL:
# the pre-fix code read REG_EXIT from a `... || true` command substitution, so
# it was ALWAYS 0 and the battery asserted "regression suite PASSED" over a
# transcript that said PASS: 16 / FAIL: 14.
nested_verdict() {
    local exit_code="$1" tally="$2" np="" nf=""
    if [ -n "$tally" ]; then
        np="${tally%% *}"
        nf="${tally##* }"
    else
        echo "FAIL: the nested suite printed no tally (PASS: N / FAIL: M) — its result is UNVERIFIED (exit $exit_code)"
        return 0
    fi
    if [ "$exit_code" -ne 0 ]; then
        echo "FAIL: the nested suite exited $exit_code (tally PASS: $np / FAIL: $nf)"
        return 0
    fi
    if [ "$nf" -ne 0 ]; then
        echo "FAIL: the nested suite reported $nf failing cells (tally PASS: $np / FAIL: $nf)"
        return 0
    fi
    echo "PASS: the nested suite exited 0 with tally PASS: $np / FAIL: $nf"
    return 0
}

# daemon_health_probe — 0 when the daemon this battery is testing answers a
# cheap status call. Read-only (a list); DAEMON_HEALTH_OUT carries the output
# so a failure can quote it.
DAEMON_HEALTH_OUT=""
daemon_health_probe() {
    local out="" rc=0
    set +e
    trap - ERR # an unreachable daemon is EXPECTED here — handled by the caller
    out="$(bcli list --status all 2>&1)"
    rc=$?
    trap 'diag_err $? $LINENO "$BASH_COMMAND"' ERR
    set -e
    DAEMON_HEALTH_OUT="$out"
    [ "$rc" -eq 0 ] || return 1
    printf '%s' "$out" | grep -qiE 'connection refused|unavailable|no route to host' && return 1
    return 0
}

# daemon_gate SECTION — prove the daemon is reachable BEFORE a section spawns
# or destroys anything. On failure this is the section's ONE cell: it names the
# endpoint and how to bring the daemon back, instead of cascading into
# "not found" / "connection refused" cells (section 13's `agent "e2e-imgspec"
# not found` was a consequence, not a second defect). Healthy runs — coexist CI
# and a healthy standalone run — add NO cell here.
daemon_gate() {
    local section="$1"
    daemon_health_probe && return 0
    fail "$section: the daemon at $BUNKER_DAEMON_URL is unreachable, so nothing was spawned or destroyed here — bring it back with 'systemctl restart bunkerd' (or 'bunkerd -c /etc/bunkerd/config.yaml') and re-run this battery. Probe: $(printf '%s' "$DAEMON_HEALTH_OUT" | head -1)"
    return 1
}

# ── Binary certification (DF-BUNKER-3 / QA-BUNKER-3) ───────────────────
# The battery can legitimately certify a DEPLOYED binary (default mode), but
# the run must state WHICH binary and commit it certified — a VERIFY-PASS
# transcript must never be readable as "HEAD was verified" when it was not.
#
# bin_commit_verdict REPO_HEAD BIN_COMMIT — PURE comparison helper: no side
# effects, no I/O beyond its arguments. Returns:
#   MATCH     the binary's commit matches repo HEAD (prefix compare: either
#             side may be a short 7-8 char or a full 40-char SHA; they match
#             when one is a prefix of the other and the shorter is >= 7 chars)
#   SKIP      nothing comparable: BIN_COMMIT empty/"unknown"/"dev", or
#             REPO_HEAD empty (no git worktree / git unavailable) — a SKIP is
#             NOT a mismatch and must never fail a run
#   MISMATCH  both sides are real SHAs and they disagree
bin_commit_verdict() {
    local repo_head="$1" bin_commit="$2"
    local b_lc r_lc short long
    b_lc="$(printf '%s' "${bin_commit:-}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"
    r_lc="$(printf '%s' "${repo_head:-}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"
    case "$b_lc" in
        ""|unknown|dev) echo "SKIP:binary reported no comparable commit ('$bin_commit')"; return 0 ;;
    esac
    case "$r_lc" in
        "") echo "SKIP:no repo HEAD available (not a git worktree or git unavailable)"; return 0 ;;
    esac
    if [ "${#b_lc}" -le "${#r_lc}" ]; then
        short="$b_lc" long="$r_lc"
    else
        short="$r_lc" long="$b_lc"
    fi
    if [ "${#short}" -ge 7 ] && [ "${long:0:${#short}}" = "$short" ]; then
        echo "MATCH"
    else
        echo "MISMATCH"
    fi
    return 0
}

# bin_certify VERDICT BIN_PATH BIN_COMMIT REPO_HEAD — PURE banner renderer
# for the certification decision (no I/O, no mutation; 2>/dev/null not used).
bin_certify() {
    local verdict="$1" bin_path="$2" bin_commit="$3" repo_head="$4"
    echo "  ── binary certification (DF-BUNKER-3 / QA-BUNKER-3) ──"
    echo "  binary under test : $bin_path"
    echo "  commit it reports : ${bin_commit:-<none>}"
    echo "  repo HEAD         : ${repo_head:-<none>}"
    echo "  verdict           : $verdict"
}

# Preflight (runs BEFORE any host mutation): state which binary the battery
# will certify. Sets BIN_CERT_VERDICT / BIN_CERT_BIN_COMMIT for the RESULTS
# SUMMARY. On MISMATCH it is a loud note by default (certifying a deployed
# binary is legitimate); with BUNKER_STRICT_BIN=1 in the environment it is a
# hard fail that exits BEFORE touching the host.
BIN_CERT_VERDICT=""
BIN_CERT_BIN_COMMIT=""
BIN_CERT_REPO_HEAD=""
BIN_CERT_VERDICT_DETAIL=""

# bin_cert_resolve — the RESOLUTION half of the certification preflight (no
# output, no side effects): fills BIN_CERT_BIN_COMMIT / BIN_CERT_REPO_HEAD /
# BIN_CERT_VERDICT / BIN_CERT_VERDICT_DETAIL. Shared by
# bin_certification_preflight and --show-plan so both report the same verdict
# computed by the same code.
bin_cert_resolve() {
    BIN_CERT_BIN_COMMIT="$("${BUNKER:-/usr/local/bin/bunker}" version 2>/dev/null | awk 'NR==2 {print $2}')"
    BIN_CERT_REPO_HEAD=""
    if command -v git > /dev/null 2>&1; then
        BIN_CERT_REPO_HEAD="$(git rev-parse HEAD 2>/dev/null)" || BIN_CERT_REPO_HEAD=""
    fi
    local verdict
    verdict="$(bin_commit_verdict "$BIN_CERT_REPO_HEAD" "$BIN_CERT_BIN_COMMIT")"
    BIN_CERT_VERDICT="${verdict%%:*}"
    BIN_CERT_VERDICT_DETAIL=""
    case "$verdict" in *:*) BIN_CERT_VERDICT_DETAIL="${verdict#*:}" ;; esac
}

bin_certification_preflight() {
    bin_cert_resolve
    local verdict="$BIN_CERT_VERDICT" detail="$BIN_CERT_VERDICT_DETAIL" repo_head="$BIN_CERT_REPO_HEAD"
    echo ""
    echo "=== BINARY CERTIFICATION ==="
    bin_certify "$BIN_CERT_VERDICT" "${BUNKER:-<unset>}" "$BIN_CERT_BIN_COMMIT" "$repo_head"
    case "$BIN_CERT_VERDICT" in
        MATCH)
            assert "battery certifies $BUNKER @ $BIN_CERT_BIN_COMMIT == repo HEAD"
            ;;
        SKIP)
            note "binary certification SKIPPED${detail}"
            ;;
        MISMATCH)
            if [ "${BUNKER_STRICT_BIN:-}" = "1" ]; then
                fail "STRICT: binary $BUNKER reports commit '$BIN_CERT_BIN_COMMIT' but repo HEAD is '${repo_head}' — set BUNKER_BIN to a binary built from HEAD or re-deploy (BUNKER_STRICT_BIN=1)"
                echo ""
                echo "STATUS: binary certification MISMATCH under BUNKER_STRICT_BIN=1 — host untouched, nothing was run"
                exit 1
            else
                note "battery certifies a binary that is NOT repo HEAD${detail} — legitimate for a deployed binary, but do not read VERIFY-PASS as 'HEAD was verified' (set BUNKER_STRICT_BIN=1 to make this fatal)"
            fi
            ;;
        *)
            note "binary certification produced an unknown verdict: $verdict"
            ;;
    esac
}

# Preflight: refuse to run the battery as non-root BEFORE any write to
# /var/log or any host mutation. Preflight_decision() is pure (no side
# effects) so --self-test can exercise it as a non-root user.
preflight_decision() {
    if [ "$(id -u)" -ne 0 ]; then
        echo "ERROR: this battery MUST run as root." >&2
        echo "  Why: it creates Linux users (useradd), installs PAM/systemd" >&2
        echo "  artifacts, and writes a root-owned daemon log under /var/log" >&2
        echo "  (see README \"Run E2E battery\" / AGENTS.md \"E2E verification\")." >&2
        echo "  Run it as: sudo bash e2e-full-battery.sh" >&2
        echo "  (no-root, no-side-effect previews: --show-plan, --self-test, --bin-report)" >&2
        return 1
    fi
    return 0
}
if [ "${1:-}" = "--self-test" ]; then
    # ── Self-test: prove the diagnostics work, with ZERO side effects. ──
    # Never creates users, never starts daemons, never touches /var/log,
    # never runs a battery section. Uses the same helpers as the main path.
    ST_FAIL=0
    # The script under test, so a static check can assert on the real text
    # (BASH_SOURCE works for `bash e2e-full-battery.sh` and for a sourced run).
    ST_SELF="${BASH_SOURCE[0]:-$0}"
    echo "=== 0. Self-test (no root, no side effects) ==="

    # (a) A failing command captured through run_capture must SURFACE its
    # output (the exact helper the section-2 connect step calls).
    run_capture "synthetic failing command" sh -c 'echo "synthetic stderr boom" >&2; exit 7'
    if [ "$RUN_CAPTURE_EXIT" -ne 0 ] && printf '%s' "$RUN_CAPTURE_OUT" | grep -q "synthetic stderr boom"; then
        assert "run_capture surfaces failing-command output (exit=$RUN_CAPTURE_EXIT)"
    else
        fail "run_capture swallowed a failing command's output or status"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (b) A synthetic failure must hit the ERR trap and name line + command.
    ST_TRAP_SAW=""
    diag_err() { ST_TRAP_SAW="line=$2 cmd=$3"; }
    set +e
    false
    set -e
    diag_err() {
        local status="$1" line="$2" cmd="$3"
        echo "" >&2
        echo "  ✗ ERROR: command failed (exit $status) at line $line: $cmd" >&2
        echo "    (run aborted by set -e; partial results above, EXIT cleanup still runs)" >&2
    }
    if printf '%s' "$ST_TRAP_SAW" | grep -qE 'line=[0-9]+ cmd=false'; then
        assert "ERR trap names failing line + command ($ST_TRAP_SAW)"
    else
        fail "ERR trap did not produce a line+command diagnostic (got: '$ST_TRAP_SAW')"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (c) The non-root refusal decision must report "would refuse" for a
    # non-root uid without exiting the self-test (id -u is forced here).
    st_uid() { echo 1000; }
    ST_REFUSED=""
    ST_RC=0
    # if/else condition context: the ERR trap does not fire on the EXPECTED
    # refusal, and $? in the else branch is the real refusal status.
    if ST_REFUSED=$(preflight_decision 2>&1); then
        ST_RC=0
    else
        ST_RC=$?
    fi
    if [ "$ST_RC" -ne 0 ] && printf '%s' "$ST_REFUSED" | grep -q "MUST run as root"; then
        assert "preflight would refuse a non-root uid (rc=$ST_RC, reason printed)"
    else
        fail "preflight_decision did not report the non-root refusal"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (d) bin_commit_verdict: MATCH for a short-vs-full SHA prefix pair, in
    # BOTH orientations (binary may report short, HEAD may be full or vice
    # versa).
    V1=$(bin_commit_verdict "7232f304d46311a30d31d93e3de967666221214d" "7232f30")
    V2=$(bin_commit_verdict "7232f30" "7232f304d46311a30d31d93e3de967666221214d")
    if [ "$V1" = "MATCH" ] && [ "$V2" = "MATCH" ]; then
        assert "bin_commit_verdict: short/full SHA prefixes MATCH both ways"
    else
        fail "bin_commit_verdict: expected MATCH for the short/full prefix pair (got '$V1' / '$V2')"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (e) bin_commit_verdict: MISMATCH for two real but different SHAs.
    V3=$(bin_commit_verdict "7232f30" "4af949d")
    if [ "$V3" = "MISMATCH" ]; then
        assert "bin_commit_verdict: two different real SHAs are a MISMATCH"
    else
        fail "bin_commit_verdict: expected MISMATCH for different SHAs (got '$V3')"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (f) bin_commit_verdict: SKIP (never a failure) for an unreportable
    # binary commit (unknown / dev / empty) and for a missing repo HEAD.
    # The verdict word is the part before ':' — SKIP rows carry a reason.
    V4=$(bin_commit_verdict "7232f30" "unknown")
    V5=$(bin_commit_verdict "7232f30" "dev")
    V6=$(bin_commit_verdict "7232f30" "")
    V7=$(bin_commit_verdict "" "7232f30")
    if [ "${V4%%:*}" = "SKIP" ] && [ "${V5%%:*}" = "SKIP" ] && [ "${V6%%:*}" = "SKIP" ] && [ "${V7%%:*}" = "SKIP" ]; then
        assert "bin_commit_verdict: unknown/dev/empty commit and missing HEAD are SKIP (never MISMATCH)"
    else
        fail "bin_commit_verdict: expected SKIP for unknown/dev/empty/no-HEAD (got '$V4'/'$V5'/'$V6'/'$V7')"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (g) Normalization: an uppercase full SHA (some stampers emit it) still
    # MATCHes a lowercase short one.
    V8=$(bin_commit_verdict "7232f30" "7232F304D46311A30D31D93E3DE967666221214D")
    if [ "$V8" = "MATCH" ]; then
        assert "bin_commit_verdict: case/whitespace normalized before compare"
    else
        fail "bin_commit_verdict: expected MATCH for an uppercase full SHA (got '$V8')"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (h) A shorter-than-7-char prefix must NOT match even when it is a
    # literal prefix of HEAD (git abbreviations are >= 7 chars).
    V9=$(bin_commit_verdict "7232f30" "7232f3")
    if [ "$V9" = "MISMATCH" ]; then
        assert "bin_commit_verdict: a <7-char prefix never MATCHes (not a real git abbreviation)"
    else
        fail "bin_commit_verdict: expected MISMATCH for a 6-char prefix (got '$V9')"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (i) CLI-state isolation (INT-CI-010): one CLI invocation, run through the
    # same wrapper the battery uses (bcli), must write the BATTERY's own state
    # dir and leave the operator's config byte-identical. The sentinel config
    # is one that `bunker use` WOULD rewrite (its active_server differs from
    # the server selected below), so a run that resolved the wrong file shows
    # up immediately as a changed sentinel.
    #
    # No CLI binary (e.g. a bare checkout on a CI runner) means there is
    # nothing to invoke: the check reports that it could not be exercised
    # rather than pretending to have proven the isolation.
    ST_OP_HOME=""
    ST_CLI_HOME=""
    if [ ! -x "${BUNKER:-/usr/local/bin/bunker}" ]; then
        note "isolated CLI-config check NOT exercised — no CLI binary at ${BUNKER:-/usr/local/bin/bunker} (set BUNKER_BIN to a built binary to run it)"
    else
        ST_OP_HOME="$(mktemp -d /tmp/bunker-battery-selftest-op-XXXXXX)"
        ST_SENTINEL="$ST_OP_HOME/.bunker/config.yaml"
        ST_SENTINEL_SNAP="$ST_OP_HOME/sentinel.snapshot"
        ST_USE_OUT="$ST_OP_HOME/use.out"
        mkdir -p "$ST_OP_HOME/.bunker"
        cat > "$ST_SENTINEL" <<'EOF'
servers:
  sentinel:
    name: sentinel
    url: http://127.0.0.1:9
    token: selftest-token-sentinel
  battery:
    name: battery
    url: http://127.0.0.1:9
    token: selftest-token-battery
active_server: sentinel
EOF
        cp "$ST_SENTINEL" "$ST_SENTINEL_SNAP"
        battery_cli_state_dir > /dev/null
        ST_CLI_HOME="$BATTERY_CLI_HOME"
        # Seed the battery's own state dir in BOTH layouts: a current build
        # reads $BUNKER_HOME/config.yaml, a build that predates BUNKER_HOME
        # reads $HOME/.bunker/config.yaml. Either way the file lives inside the
        # battery's own scratch dir, which is the property under test.
        cp "$ST_SENTINEL" "$ST_CLI_HOME/config.yaml"
        mkdir -p "$ST_CLI_HOME/.bunker"
        cp "$ST_SENTINEL" "$ST_CLI_HOME/.bunker/config.yaml"
        ST_USE_RC=0
        set +e
        trap - ERR # the invocation's status is EXPECTED here — read via ST_USE_RC below
        ( export HOME="$ST_OP_HOME"; bcli use battery ) > "$ST_USE_OUT" 2>&1
        ST_USE_RC=$?
        trap 'diag_err $? $LINENO "$BASH_COMMAND"' ERR
        set -e
        # The invocation above runs with HOME pointing at the sentinel (exactly
        # the value that WOULD be the operator's home if the battery did not
        # override it). bcli pins BUNKER_HOME and HOME to the battery's own
        # state dir, so the sentinel must stay byte-identical...
        if cmp -s "$ST_SENTINEL" "$ST_SENTINEL_SNAP"; then
            assert "isolated CLI state: the operator's config is byte-identical after a CLI invocation (HOME=$ST_OP_HOME)"
        else
            fail "a CLI invocation modified the sentinel operator config $ST_SENTINEL — the battery's CLI-state isolation is broken"
            ST_FAIL=$((ST_FAIL+1))
        fi
        # ...while the registration lands INSIDE the battery's own state dir.
        # The exact filename depends on the binary: a BUNKER_HOME-aware build
        # writes $ST_CLI_HOME/config.yaml, an older build writes
        # $ST_CLI_HOME/.bunker/config.yaml. Both are the battery's own scratch.
        ST_CLI_CFG_PATH="$(grep -rl "active_server: battery" "$ST_CLI_HOME" 2>/dev/null | head -1 || true)"
        if [ "$ST_USE_RC" -eq 0 ] && [ -n "$ST_CLI_CFG_PATH" ]; then
            assert "isolated CLI state: the CLI wrote ${ST_CLI_CFG_PATH#$ST_CLI_HOME/} under the battery's own state dir ($ST_CLI_HOME), not the operator's"
        else
            fail "the CLI did not register into the battery's own state dir (rc=$ST_USE_RC, output: $(cat "$ST_USE_OUT" 2>/dev/null | head -3 | tr '\n' ' '))"
            ST_FAIL=$((ST_FAIL+1))
        fi
    fi

    # (i-b) The wrapper must hand the CLI the SAME isolated state dir on every
    # call: a fresh dir per invocation would scatter a run's registration. The
    # stub CLI only reports the environment it was given, so this check needs
    # no real binary and has no side effects.
    battery_cli_state_dir > /dev/null
    ST_STUB_DIR="$(mktemp -d /tmp/bunker-battery-selftest-stub-XXXXXX)"
    ST_STUB="$ST_STUB_DIR/bunker"
    cat > "$ST_STUB" <<'EOF'
#!/bin/sh
echo "${BUNKER_HOME:-<unset>}|${HOME:-<unset>}"
EOF
    chmod +x "$ST_STUB"
    ST_PREV_BUNKER="$BUNKER"
    BUNKER="$ST_STUB"
    ST_HOME_1="$(bcli list --status all)"
    ST_HOME_2="$(bcli exec probe -- true)"
    BUNKER="$ST_PREV_BUNKER"
    if [ "$ST_HOME_1" = "$ST_HOME_2" ] && [ "${ST_HOME_1%%|*}" = "$BATTERY_CLI_HOME" ]; then
        assert "bcli pins ONE stable state dir for every call (BUNKER_HOME=HOME=$BATTERY_CLI_HOME)"
    else
        fail "bcli did not pin a stable state dir (got '$ST_HOME_1' / '$ST_HOME_2', battery state dir '$BATTERY_CLI_HOME')"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (ii) Explicit inputs win (INT-CI-010): BUNKERD_REST_ADDR=:18080 and an
    # exported BUNKER_TOKEN must resolve to :18080 with token source=env, in
    # standalone mode as well as coexist mode. resolve_inputs() is re-run in a
    # subshell with a controlled environment so this process keeps its own.
    ST_EXPLICIT="$( unset BUNKERD_COEXIST; unset BUNKER_DAEMON_URL; export BUNKERD_REST_ADDR=":18080"; export BUNKER_TOKEN="selftest-token-explicit"; resolve_inputs; printf '%s|%s|%s' "$REST_PORT" "$BUNKER_TOKEN_SOURCE" "$BUNKER_DAEMON_URL" )"
    if [ "$ST_EXPLICIT" = "18080|env|http://localhost:18080" ]; then
        assert "explicit inputs honored: BUNKERD_REST_ADDR=:18080 + BUNKER_TOKEN exported → REST :18080, token source=env"
    else
        fail "explicit inputs were not honored (got '$ST_EXPLICIT', want '18080|env|http://localhost:18080')"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (iii) Default case (INT-CI-010): with BUNKER_TOKEN unset the token
    # source must report "default" (the documented test token), so an
    # operator can tell an inherited token apart from the fallback.
    ST_TOKEN_DEFAULT="$( unset BUNKER_TOKEN; resolve_inputs; printf '%s' "$BUNKER_TOKEN_SOURCE" )"
    if [ "$ST_TOKEN_DEFAULT" = "default" ]; then
        assert "unset BUNKER_TOKEN resolves to token source=default (not silently inherited)"
    else
        fail "expected token source=default when BUNKER_TOKEN is unset (got '$ST_TOKEN_DEFAULT')"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (iv) INT-CI-012 — section 12 hands the nested suite its OWN isolated,
    # non-production ports in BOTH modes, and there is exactly ONE invocation
    # site. The pre-fix layout had a second, portless invocation in the
    # standalone branch, so the child inherited this battery's exported
    # BUNKERD_REST_ADDR=:18080 / BUNKERD_GRPC_ADDR=:19090 and then killed and
    # raced the production daemon on those ports.
    ST_NP="$(nested_suite_ports)"
    if [ "$ST_NP" = ":29092 :28082" ]; then
        assert "the nested suite is handed its own isolated ports in both modes ($ST_NP)"
    else
        fail "nested_suite_ports returned '$ST_NP', want ':29092 :28082'"
        ST_FAIL=$((ST_FAIL+1))
    fi
    if printf '%s' "$ST_NP" | grep -qE ':(18080|19090|28081|29091)([^0-9]|$)'; then
        fail "the nested suite is handed a production/battery port ($ST_NP) — it could take over the live daemon"
        ST_FAIL=$((ST_FAIL+1))
    else
        assert "no production/battery port is handed to the nested suite"
    fi
    ST_S12_LINES="$(grep -nF 'bash "$REGRESSION_SCRIPT"' "$ST_SELF" 2>/dev/null | grep -vF 'grep -nF' | cut -d: -f1 | tr '\n' ' ' || true)"
    ST_S12_N=0
    ST_S12_BAD=""
    for st_ln in $ST_S12_LINES; do
        ST_S12_N=$((ST_S12_N+1))
        st_line="$(sed -n "${st_ln}p" "$ST_SELF")"
        printf '%s' "$st_line" | grep -qF 'BUNKERD_GRPC_ADDR="$NESTED_GRPC_ADDR"' || ST_S12_BAD="$ST_S12_BAD $st_ln:no-grpc-port"
        printf '%s' "$st_line" | grep -qF 'BUNKERD_REST_ADDR="$NESTED_REST_ADDR"' || ST_S12_BAD="$ST_S12_BAD $st_ln:no-rest-port"
    done
    if [ "$ST_S12_N" = "1" ] && [ -z "$ST_S12_BAD" ]; then
        assert "the single nested-suite invocation carries explicit non-production ports (line ${ST_S12_LINES% })"
    else
        fail "nested-suite invocations: $ST_S12_N with missing ports:$ST_S12_BAD (want exactly 1, carrying both ports)"
        ST_FAIL=$((ST_FAIL+1))
    fi

    # (v) INT-CI-012 — section 12's verdict must FLIP when the nested suite
    # fails. The pre-fix code read REG_EXIT after a `... || true` command
    # substitution (always 0) and grepped the transcript's words, so a suite
    # that printed PASS: 16 / FAIL: 14 still produced "✓ regression suite
    # PASSED". A fixture script that exits 1 must flip the verdict, a green
    # one must pass, and a suite that prints no tally must be UNVERIFIED.
    ST_FIX_DIR="$(mktemp -d /tmp/bunker-battery-selftest-nested-XXXXXX)"
    cat > "$ST_FIX_DIR/fail.sh" <<'STFIXEOF'
#!/bin/bash
echo "  ✓ synthetic nested cell"
echo "  ✗ synthetic nested failure"
echo "  PASS: 16"
echo "  FAIL: 14"
exit 1
STFIXEOF
    cat > "$ST_FIX_DIR/pass.sh" <<'STFIXEOF'
#!/bin/bash
echo "  ✓ synthetic nested cell"
echo "  PASS: 21"
echo "  FAIL: 0"
exit 0
STFIXEOF
    cat > "$ST_FIX_DIR/notally.sh" <<'STFIXEOF'
#!/bin/bash
echo "bunker: spawn agent: unavailable: dial tcp [::1]:18080: connect: connection refused"
exit 0
STFIXEOF
    run_capture "synthetic failing nested suite" bash "$ST_FIX_DIR/fail.sh"
    ST_V_FAIL="$(nested_verdict "$RUN_CAPTURE_EXIT" "$(nested_tally "$RUN_CAPTURE_OUT" || true)")"
    ST_TALLY_PARSED="$(nested_tally "$RUN_CAPTURE_OUT" || true)"
    run_capture "synthetic passing nested suite" bash "$ST_FIX_DIR/pass.sh"
    ST_V_PASS="$(nested_verdict "$RUN_CAPTURE_EXIT" "$(nested_tally "$RUN_CAPTURE_OUT" || true)")"
    run_capture "synthetic tally-less nested suite" bash "$ST_FIX_DIR/notally.sh"
    ST_V_NOTALLY="$(nested_verdict "$RUN_CAPTURE_EXIT" "$(nested_tally "$RUN_CAPTURE_OUT" || true)")"
    case "$ST_V_FAIL" in
        FAIL:*) assert "a nested suite exiting non-zero flips section 12 to FAIL ($ST_V_FAIL)" ;;
        *) fail "a nested suite exiting 1 did NOT flip section 12 to FAIL (got '$ST_V_FAIL')"; ST_FAIL=$((ST_FAIL+1)) ;;
    esac
    if [ "$ST_TALLY_PARSED" = "16 14" ]; then
        assert "section 12 reads the nested suite's OWN tally lines ($ST_TALLY_PARSED), not a grep of ✓/✗ and the word PASS"
    else
        fail "nested_tally parsed '$ST_TALLY_PARSED', want '16 14' (the suite's own summary counters)"
        ST_FAIL=$((ST_FAIL+1))
    fi
    case "$ST_V_PASS" in
        PASS:*) assert "a green nested suite reports PASS with its own tally ($ST_V_PASS)" ;;
        *) fail "a green nested suite was not reported as PASS (got '$ST_V_PASS')"; ST_FAIL=$((ST_FAIL+1)) ;;
    esac
    case "$ST_V_NOTALLY" in
        FAIL:*) assert "a nested suite that printed no tally is UNVERIFIED (FAIL), never a silent pass" ;;
        *) fail "a tally-less nested suite was not a FAIL (got '$ST_V_NOTALLY')"; ST_FAIL=$((ST_FAIL+1)) ;;
    esac
    rm -rf "$ST_FIX_DIR" 2>/dev/null || true

    # (vi) INT-CI-012 — the harness must never remove a config file it did not
    # create. Behavioural, both arms: a removal pointed at a dir holding the
    # operator's CLI config is REFUSED (and the file survives), while the
    # harness's own scratch dir is removed. Plus the static invariants on the
    # nested suite, whose pre-fix cleanup deleted the operator's CLI state on
    # every standalone run.
    ST_OP_DIR="$(mktemp -d /tmp/bunker-operator-config-XXXXXX)"
    mkdir -p "$ST_OP_DIR/.bunker"
    printf 'servers: {}\nactive_server: operator\n' > "$ST_OP_DIR/.bunker/config.yaml"
    ST_OP_HASH="$(file_fingerprint "$ST_OP_DIR/.bunker/config.yaml")"
    ST_OP_RC=0
    remove_own_state_dir "$ST_OP_DIR" > /dev/null 2>&1 || ST_OP_RC=$?
    if [ "$ST_OP_RC" -ne 0 ] && [ "$(file_fingerprint "$ST_OP_DIR/.bunker/config.yaml")" = "$ST_OP_HASH" ]; then
        assert "remove_own_state_dir REFUSES a dir that is not the harness's own scratch (rc=$ST_OP_RC, the config file is untouched)"
    else
        fail "a removal pointed at a non-scratch dir was not refused / the config file did not survive (rc=$ST_OP_RC)"
        ST_FAIL=$((ST_FAIL+1))
    fi
    ST_SCRATCH_PROBE="$(mktemp -d /tmp/bunker-battery-cli-XXXXXX)"
    if remove_own_state_dir "$ST_SCRATCH_PROBE" && [ ! -d "$ST_SCRATCH_PROBE" ]; then
        assert "remove_own_state_dir still removes the harness's OWN scratch dir"
    else
        fail "the harness's own scratch dir was not removed by remove_own_state_dir"
        ST_FAIL=$((ST_FAIL+1))
    fi
    rm -rf "$ST_OP_DIR" 2>/dev/null || true
    ST_NESTED_SCRIPT="$(dirname "$ST_SELF")/regression-tests.sh"
    if [ -f "$ST_NESTED_SCRIPT" ]; then
        ST_ROOT_RM="$(grep -nE 'rm -rf[[:space:]]+/root' "$ST_NESTED_SCRIPT" 2>/dev/null | grep -v ':[[:space:]]*#' | wc -l | tr -d ' ' || true)"
        if [ "${ST_ROOT_RM:-0}" != "0" ]; then
            fail "regression-tests.sh still removes operator state under /root ($ST_ROOT_RM executable line(s) — the operator's CLI config)"
            ST_FAIL=$((ST_FAIL+1))
        else
            assert "regression-tests.sh removes no path under /root (no operator CLI config deletion)"
        fi
        if grep -qF 'export BUNKER_HOME="$REGRESSION_CLI_HOME"' "$ST_NESTED_SCRIPT" && grep -qF 'export HOME="$REGRESSION_CLI_HOME"' "$ST_NESTED_SCRIPT"; then
            assert "regression-tests.sh pins BOTH BUNKER_HOME and HOME to its own scratch dir (standalone included)"
        else
            fail "regression-tests.sh does not pin BUNKER_HOME *and* HOME to its own scratch dir — an inherited HOME=/root is the operator's"
            ST_FAIL=$((ST_FAIL+1))
        fi
        ST_PKILL_N="$(grep -h 'pkill bunkerd' "$ST_NESTED_SCRIPT" 2>/dev/null | grep -v '^[[:space:]]*#' | wc -l | tr -d ' ' || true)"
        if [ "${ST_PKILL_N:-0}" = "1" ] && grep -qF 'kill_stray_bunkerd()' "$ST_NESTED_SCRIPT" && grep -qF 'host_state_sweep_allowed()' "$ST_NESTED_SCRIPT"; then
            assert "regression-tests.sh funnels its single pkill bunkerd through kill_stray_bunkerd (never a systemd-managed daemon)"
        else
            fail "regression-tests.sh has ${ST_PKILL_N:-?} 'pkill bunkerd' site(s) — a stray unconditional kill can take the production daemon down"
            ST_FAIL=$((ST_FAIL+1))
        fi
    else
        note "regression-tests.sh is not beside this script — the nested-suite static checks were skipped"
    fi

    # The self-test exits BEFORE the cleanup trap is armed, so it removes its
    # own scratch explicitly (a sentinel HOME, a stub CLI, and the battery CLI
    # state dir).
    if [ -n "$ST_OP_HOME" ]; then
        rm -rf "$ST_OP_HOME" 2>/dev/null || true
    fi
    if [ -n "${ST_STUB_DIR:-}" ]; then
        rm -rf "$ST_STUB_DIR" 2>/dev/null || true
    fi
    if [ -n "${BATTERY_CLI_HOME:-}" ] && [ -d "$BATTERY_CLI_HOME" ]; then
        remove_own_state_dir "$BATTERY_CLI_HOME"
        BATTERY_CLI_HOME=""
    fi

    echo ""
    if [ "$ST_FAIL" -eq 0 ]; then
        echo "SELF-TEST: PASS"
        exit 0
    fi
    echo "SELF-TEST: FAIL ($ST_FAIL check(s) failed)"
    exit 1
fi

if [ "${1:-}" = "--bin-report" ]; then
    # ── --bin-report: the certification banner ONLY, ZERO side effects. ──
    # Same output as a full run's certification preflight and nothing else:
    # no root requirement, no host mutation, no battery sections. Exit 0 on
    # MATCH/SKIP, non-zero on MISMATCH — verifiable against a stub BUNKER_BIN
    # on any box (DF-BUNKER-3 / QA-BUNKER-3).
    bin_certification_preflight
    case "$BIN_CERT_VERDICT" in
        MATCH|SKIP) exit 0 ;;
        *) exit 1 ;;
    esac
fi

if [ "${1:-}" = "--show-plan" ]; then
    # ── --show-plan: the RESOLVED run plan, ZERO side effects. ──
    # Prints the same inputs the full run will use, without root, without
    # creating users, starting daemons, writing /var/log, or writing any
    # config file. The token is NEVER printed: only its source (env/default)
    # and a masked sha256 fingerprint. The battery's own CLI state dir is
    # created so its path can be reported, then removed before this exits.
    bin_cert_resolve
    battery_cli_state_dir > /dev/null
    PLAN_CLI_HOME="$BATTERY_CLI_HOME"
    PLAN_CLI_CONFIG="$PLAN_CLI_HOME/config.yaml"
    echo ""
    echo "=== E2E BATTERY PLAN (no root required, nothing was changed) ==="
    echo "  battery CLI binary    : ${BUNKER:-<unset>}"
    echo "  battery daemon binary : ${BUNKERD_BIN:-<unset>}"
    echo "  certification verdict : ${BIN_CERT_VERDICT:-UNKNOWN} (binary reports ${BIN_CERT_BIN_COMMIT:-<none>}; repo HEAD ${BIN_CERT_REPO_HEAD:-<none>})${BIN_CERT_VERDICT_DETAIL:+ — $BIN_CERT_VERDICT_DETAIL}"
    if [ -n "$BUNKERD_COEXIST" ]; then
        echo "  mode                  : coexist ($BUNKERD_COEXIST) — the battery starts its own daemon on its own ports"
    else
        echo "  mode                  : standalone — talks to the daemon already on the ports above; starts/stops no daemon itself (INT-CI-012)"
    fi
    echo "  nested suite (sec 12) : its own ports $(nested_suite_ports) and its own CLI state dir — it never shares this battery's ports; a systemd-managed bunkerd and its agent state are left alone"
    echo "  sections 13/14 policy : probe $BUNKER_DAEMON_URL first — an unreachable daemon fails ONE cell naming the endpoint, not a cascade of 'not found'"
    echo "  daemon URL (connect)  : $BUNKER_DAEMON_URL  [$BUNKER_DAEMON_URL_SOURCE]"
    echo "  REST port             : :$REST_PORT  (BUNKERD_REST_ADDR=$BUNKERD_REST_ADDR${BUNKERD_REST_ADDR_EXPLICIT:+, explicit})"
    echo "  gRPC port             : :$GRPC_PORT  (BUNKERD_GRPC_ADDR=$BUNKERD_GRPC_ADDR${BUNKERD_GRPC_ADDR_EXPLICIT:+, explicit})"
    echo "  token source          : $BUNKER_TOKEN_SOURCE (fingerprint $(token_fingerprint "$BUNKER_TOKEN"); the token itself is never printed)"
    echo "  battery CLI state dir : $PLAN_CLI_HOME  (BUNKER_HOME and HOME for every CLI call; removed on exit)"
    echo "  battery CLI config    : $PLAN_CLI_CONFIG  ($([ -f "$PLAN_CLI_CONFIG" ] && echo exists || echo 'created on the first CLI call'))"
    echo "  operator CLI config   : $OPERATOR_CLI_CONFIG  ($([ -f "$OPERATOR_CLI_CONFIG" ] && echo exists || echo absent); $OPERATOR_CLI_CONFIG_HASH)"
    echo "  operator config policy: never read or written by this battery — the fingerprint is re-checked at the end of the run"
    echo "  root requirement      : the full run needs root (exit 42 otherwise); --self-test/--show-plan/--bin-report never do"
    echo "  sections that run     : 1-11, 13, 14 (14's live-daemon registry replays are skipped in coexist mode)"
    echo "  sections that may skip: 12 only when no regression-tests.sh is present; 15.1 fails without a CLI that has host-provision"
    remove_own_state_dir "$PLAN_CLI_HOME"
    echo ""
    echo "PLAN: OK (nothing was changed)"
    exit 0
fi

if ! preflight_decision; then
    exit 42
fi

# Binary certification preflight (DF-BUNKER-3 / QA-BUNKER-3) — runs BEFORE
# any host mutation: before the CLEANUP user sweep, before the coexist daemon
# is started, before /var/log is touched, before section 1. Pure output +
# counters; under BUNKER_STRICT_BIN=1 a MISMATCH exits 1 right here, while
# nothing on the host has been modified and the EXIT cleanup trap is not even
# armed yet.
bin_certification_preflight

# EXIT cleanup — MAIN BATTERY PATH ONLY. The --self-test path above exits
# BEFORE this is armed, so the self-test can never destroy agents, delete
# users, or run any teardown (it is side-effect-free by construction).
trap cleanup EXIT


# ERR trap: name the failing line + command before any set -e exit. cleanup
# (EXIT trap below) still runs; the script's exit status is preserved.
trap 'diag_err $? $LINENO "$BASH_COMMAND"' ERR
diag_err() {
    local status="$1" line="$2" cmd="$3"
    echo "" >&2
    echo "  ✗ ERROR: command failed (exit $status) at line $line: $cmd" >&2
    echo "    (run aborted by set -e; partial results above, EXIT cleanup still runs)" >&2
}

if [ -n "$BUNKERD_COEXIST" ]; then
    # The CLI state is isolated for EVERY mode by battery_cli_state_dir()
    # (BUNKER_HOME + HOME -> the battery's own throwaway dir, INT-CI-010); no
    # extra HOME reassignment is needed here.
    BATTERY_CONFIG="$(mktemp /tmp/bunkerd-battery-XXXXXX.yaml)"
    cat > "$BATTERY_CONFIG" <<EOF
server:
  grpc_addr: "$BUNKERD_GRPC_ADDR"
  rest_addr: "$BUNKERD_REST_ADDR"
auth:
  enabled: false
agent:
  ssh_dir: /etc/bunkerd/ssh
  max_agents: 10
  port_range_start: 30000
  port_range_end: 30999
  port_range_per_agent: 100
  # GAP-075: keep each agent's shared-scratch cap small so this battery can
  # prove the kernel-enforced bound (16 MiB) instead of only inspecting it.
  # The scratch root stays at the documented default: /srv/bunker-share.
  isolation:
    shared_scratch_enabled: true
    shared_scratch_per_agent_bytes: 16777216
  # GAP-070: this battery's daemon must NEVER open the production registry or
  # reconcile against the host's real agents — on a shared host the default
  # reconciliation mode would DESTROY production agents. Durable-registry
  # behaviour is covered deterministically by the Go suite and by section 14.
  registry:
    enabled: false
EOF
fi

# Ports, daemon URL, the operator-config fingerprint and the CLI-state
# isolation are all resolved ONCE by resolve_inputs() near the top of this
# script (INT-CI-010). Arm the battery's own CLI state dir here — before the
# first CLI invocation in the main path (the certification preflight above
# only runs `version`, which never touches a config file).
battery_cli_state_dir > /dev/null
echo "  battery CLI state dir: $BATTERY_CLI_HOME  (BUNKER_HOME and HOME for every CLI call)"
echo "  battery CLI config   : $(battery_cli_config)"
echo "  operator CLI config  : $OPERATOR_CLI_CONFIG  ($OPERATOR_CLI_CONFIG_HASH — the battery never reads or writes it)"
echo ""

cleanup() {
    # Destroy any agents created during tests. bcli pins BUNKER_HOME and HOME
    # at the battery's own state dir, so cleanup can only ever reach the daemon
    # this run registered — never whatever the operator's CLI config points at.
    for agent in e2e-main e2e-agent-2 e2e-agent-3 e2e-agent-4 e2e-agent-5 e2e-imgspec e2e-imgspec-b e2e-imgspec-bad gap070-idem gap075-a gap075-b; do
        bcli destroy "$agent" --force > /dev/null 2>&1 || true
    done
    # Kill leftover users. Standalone: every bunker- user is fair game.
    # Coexist: only this battery's own agents (bunker-e2e-*) — NEVER touch
    # production users.
    if [ -z "$BUNKERD_COEXIST" ]; then
        for u in $(awk -F: '/^bunker-/ {print $1}' /etc/passwd); do
            userdel -rf "$u" 2>/dev/null || true
        done
    else
        for u in $(awk -F: '/^bunker-e2e-/ {print $1}' /etc/passwd); do
            userdel -rf "$u" 2>/dev/null || true
        done
        # Quarantine this battery's own ssh keys if destroy did not remove
        # them (shared /etc/bunkerd/ssh — never touch production keys).
        mkdir -p "/tmp/bunker-battery-key-quarantine-$$"
        for k in $(ls /etc/bunkerd/ssh 2>/dev/null | grep '^e2e-'); do
            mv "/etc/bunkerd/ssh/$k" "/tmp/bunker-battery-key-quarantine-$$/" 2>/dev/null || true
        done
    fi
    # GAP-075: stop the disposable detached run unit, remove the disposable
    # non-agent test user, restore a backed-up namespace drop-in, and remove the
    # /tmp markers. Scratch dirs are removed by the daemon's destroy; a leftover
    # is unmounted here so the host is clean.
    if [ -n "${GAP075_RUN_UNIT:-}" ]; then
        systemctl stop "${GAP075_RUN_UNIT}" > /dev/null 2>&1 || true
    fi
    if [ -n "${GAP075_DROPIN_RESTORE:-}" ] && [ -f "${GAP075_DROPIN_RESTORE}" ]; then
        cp -a "${GAP075_DROPIN_RESTORE}" "${GAP075_DROPIN_PATH}" 2>/dev/null || true
        rm -f "${GAP075_DROPIN_RESTORE}" 2>/dev/null || true
    fi
    # GAP-075 rework: restore the pam_exec precondition helper if the
    # fail-closed checks were interrupted while it was moved aside, and drop any
    # disposable marker line from the sshd stack.
    if [ -n "${GAP075_HELPER_RESTORE:-}" ] && [ -f "${GAP075_HELPER_RESTORE}" ]; then
        cp -a "${GAP075_HELPER_RESTORE}" "${GAP075_HELPER_PATH}" 2>/dev/null || true
        chown root:root "${GAP075_HELPER_PATH}" 2>/dev/null || true
        chmod 0755 "${GAP075_HELPER_PATH}" 2>/dev/null || true
        rm -f "${GAP075_HELPER_RESTORE}" 2>/dev/null || true
    fi
    # ...and restore the helper directory's mode if the widened-directory check
    # was interrupted mid-flight (the mode is part of the trust chain).
    if [ -n "${GAP075_HELPERDIR_RESTORE:-}" ]; then
        chown root:root "${GAP075_HELPER_DIR:-/usr/lib/bunker}" 2>/dev/null || true
        chmod "${GAP075_HELPERDIR_RESTORE}" "${GAP075_HELPER_DIR:-/usr/lib/bunker}" 2>/dev/null || true
        GAP075_HELPERDIR_RESTORE=""
    fi
    if [ -n "${GAP075_MASK_RESTORE:-}" ]; then
        grep -vF "${GAP075_MASK_RESTORE}" /etc/pam.d/sshd > /etc/pam.d/sshd.bunker-battery-tmp 2>/dev/null &&
            cat /etc/pam.d/sshd.bunker-battery-tmp > /etc/pam.d/sshd 2>/dev/null || true
        rm -f /etc/pam.d/sshd.bunker-battery-tmp 2>/dev/null || true
    fi
    if [ -n "${GAP075_OP_USER:-}" ]; then
        userdel -rf "${GAP075_OP_USER}" > /dev/null 2>&1 || true
    fi
    if [ -n "${GAP075_OP_KEYDIR:-}" ]; then
        rm -rf "${GAP075_OP_KEYDIR}" 2>/dev/null || true
    fi
    rm -f /tmp/gap075-* 2>/dev/null || true
    # A refused arbitrary-entry probe leaves nothing behind; remove anything a
    # FAILED probe may have created directly under the exchange root.
    rm -rf "${BUNKER_SHARED_SCRATCH:-/srv/bunker-share}"/gap075-arb-* 2>/dev/null || true
    for d in /srv/bunker-share/gap075-a /srv/bunker-share/gap075-b; do
        if mountpoint -q "$d" 2>/dev/null; then
            umount -l "$d" 2>/dev/null || true
        fi
        [ -d "$d" ] && rmdir "$d" 2>/dev/null || true
    done
    # Nested regression suite's isolated CLI state dir (section 12).
    if [ -n "${NESTED_CLI_HOME:-}" ] && [ -d "$NESTED_CLI_HOME" ]; then
        remove_own_state_dir "$NESTED_CLI_HOME"
    fi
    # Stop the battery's own bunkerd (coexist mode only)
    if [ -n "$BUNKERD_PID" ]; then
        kill "$BUNKERD_PID" 2>/dev/null || true
        wait "$BUNKERD_PID" 2>/dev/null || true
    fi
    # INT-CI-010: the operator's CLI config must be exactly as we found it.
    # Re-checked here (not only at the summary) so an abort mid-battery still
    # catches a harness that wrote it — the failure that broke the host.
    if ! operator_config_check; then
        OPERATOR_CONFIG_VIOLATION=1
    fi
    # The battery's own CLI state dir is scratch owned by this run — remove it
    # (pattern-checked: a removal can never reach the operator's config).
    if [ -n "${BATTERY_CLI_HOME:-}" ] && [ -d "$BATTERY_CLI_HOME" ]; then
        remove_own_state_dir "$BATTERY_CLI_HOME"
    fi
    if [ "$OPERATOR_CONFIG_VIOLATION" = "1" ]; then
        exit 1
    fi
}
trap cleanup EXIT

# Clean up any leftover test agents
echo "=== CLEANUP ==="
if [ -n "$BUNKERD_COEXIST" ]; then
    # Coexist: only this battery's own agents (bunker-e2e-*) — NEVER touch
    # production users.
    for u in $(awk -F: '/^bunker-e2e-/ {print $1}' /etc/passwd); do
        userdel -rf "$u" 2>/dev/null || true
    done
else
    for u in $(awk -F: '/^bunker-/ {print $1}' /etc/passwd); do
        userdel -rf "$u" 2>/dev/null || true
    done
fi
echo ""

# Start the battery's own daemon in coexist mode (isolated ports, temp config)
if [ -n "$BUNKERD_COEXIST" ]; then
    BUNKERD_BATTERY_LOG="${BUNKERD_BATTERY_LOG:-/var/log/bunkerd-battery.log}"
    if ! [ -w "$BUNKERD_BATTERY_LOG" ] && ! [ -w "$(dirname "$BUNKERD_BATTERY_LOG")" ]; then
        echo "ERROR: cannot write the battery daemon log: $BUNKERD_BATTERY_LOG" >&2
        echo "  (override the location with BUNKERD_BATTERY_LOG=<path>)" >&2
        exit 42
    fi
    touch "$BUNKERD_BATTERY_LOG" 2>/dev/null || true
    "$BUNKERD_BIN" -c "$BATTERY_CONFIG" >> "$BUNKERD_BATTERY_LOG" 2>&1 &
    BUNKERD_PID=$!
    sleep 2
fi

# =============================================
# 1. SERVER HEALTH
# =============================================
echo "=== 1. Server Health ==="
if pgrep bunkerd > /dev/null; then
    assert "bunkerd process running (PID=$(pgrep bunkerd))"
else
    fail "bunkerd not running"
fi

# Coexist mode: the battery MUST run against its OWN daemon. A stale daemon
# squatting the battery ports (leftover repro/crash) would otherwise produce
# a false pass — kill -0 on BUNKERD_PID fails fast instead (GAP-006 follow-up).
if [ -n "$BUNKERD_COEXIST" ] && [ -n "$BUNKERD_PID" ]; then
    if kill -0 "$BUNKERD_PID" 2>/dev/null; then
        assert "battery's own bunkerd alive (PID=$BUNKERD_PID)"
    else
        fail "battery's own bunkerd (PID=$BUNKERD_PID) not running — ports may be squatted by a stale daemon"
    fi
fi

if ss -tlnp | grep -q ":$GRPC_PORT"; then
    assert "gRPC listening on :$GRPC_PORT"
else
    fail "gRPC port $GRPC_PORT not listening"
fi

if ss -tlnp | grep -q ":$REST_PORT"; then
    assert "REST listening on :$REST_PORT"
else
    fail "REST port $REST_PORT not listening"
fi
echo ""

# =============================================
# 2. CONNECT
# =============================================
echo "=== 2. Connect ==="
# INT-CI-010: the endpoint and the token come from the RESOLVED inputs
# (BUNKER_DAEMON_URL / BUNKER_TOKEN) and never from a literal — the previous
# revision hardcoded the dev token here and rewrote the operator's CLI config.
# bcli pins BUNKER_HOME, so this registration lands in the battery's own state
# dir and every later CLI call (sections 3-13, cleanup) targets it.
run_capture "bunker connect" bcli connect "$BUNKER_DAEMON_URL" --token "$BUNKER_TOKEN"
CONNECT_OUT="$RUN_CAPTURE_OUT"
if echo "$CONNECT_OUT" | grep -q "Connected\|Server registered"; then
    assert "connect to bunkerd ($BUNKER_DAEMON_URL, token from $BUNKER_TOKEN_SOURCE)"
else
    fail "connect to bunkerd ($BUNKER_DAEMON_URL) — $CONNECT_OUT"
fi
# The registration must land INSIDE the battery's OWN state dir — BUNKER_HOME
# for a current build, ~/.bunker for a build that predates BUNKER_HOME; both
# live under $BATTERY_CLI_HOME. The operator's config is fingerprinted and
# re-checked by operator_config_check.
CONNECT_CFG="$(grep -rl "url: $BUNKER_DAEMON_URL" "$BATTERY_CLI_HOME" 2>/dev/null | head -1 || true)"
if [ -n "$CONNECT_CFG" ]; then
    assert "CLI registration stored under the battery's own state dir (${CONNECT_CFG#$BATTERY_CLI_HOME/})"
else
    fail "no CLI registration for $BUNKER_DAEMON_URL under the battery's own state dir ($BATTERY_CLI_HOME)"
fi
echo ""

# =============================================
# 3. LIST (empty)
# =============================================
echo "=== 3. List (empty) ==="
run_capture "bunker list" bcli list --status all
LIST_OUT="$RUN_CAPTURE_OUT"
if echo "$LIST_OUT" | grep -q "No agents found"; then
    assert "list returns empty correctly"
else
    fail "list — $LIST_OUT"
fi
echo ""

# =============================================
# 4. SPAWN
# =============================================
echo "=== 4. Spawn ==="
run_capture "spawn e2e-main" bcli spawn --agent-id "e2e-main"
SPAWN_OUT="$RUN_CAPTURE_OUT"
if echo "$SPAWN_OUT" | grep -q "Agent created"; then
    assert "spawn agent e2e-main"
else
    fail "spawn agent — $SPAWN_OUT"
fi
sleep 1

# Verify Linux user created
if id "bunker-e2e-main" >/dev/null 2>&1; then
    assert "Linux user bunker-e2e-main created"
else
    fail "Linux user not created"
fi

# Verify home directory
if [ -d "/home/bunker-e2e-main" ]; then
    assert "home directory exists"
else
    fail "home directory missing"
fi

# Verify SSH keypair
if [ -f "/etc/bunkerd/ssh/e2e-main" ]; then
    assert "SSH private key persisted"
else
    fail "SSH private key missing"
fi

# Verify authorized_keys
if [ -f "/home/bunker-e2e-main/.ssh/authorized_keys" ]; then
    assert "authorized_keys configured"
else
    fail "authorized_keys missing"
fi

# Verify port range
PORT_FILE="/home/bunker-e2e-main/.bunker/ports"
if [ -f "$PORT_FILE" ]; then
    assert "port range assigned: $(cat $PORT_FILE)"
else
    fail "port range file missing"
fi

# Docker check — rootless docker must be running
(echo ""; echo "  --- Docker Status ---")
DOCKERD_RUNNING=$(pgrep -u "bunker-e2e-main" dockerd 2>/dev/null || echo "")
if [ -n "$DOCKERD_RUNNING" ]; then
    assert "dockerd running under agent user (PID=$DOCKERD_RUNNING)"
else
    fail "dockerd NOT running for bunker-e2e-main"
fi

DOCKER_SOCK="/run/bunker/e2e-main/docker.sock"
if [ -S "$DOCKER_SOCK" ]; then
    assert "docker socket exists"
else
    fail "docker socket not created"
fi

DOCKER_RUN=$(bcli exec e2e-main -- docker run --rm alpine:latest echo DOCKER-OK 2>&1 || true)
if echo "$DOCKER_RUN" | grep -q "DOCKER-OK"; then
    assert "docker run inside agent works"
else
    fail "docker run inside agent — $DOCKER_RUN"
fi
echo ""

# =============================================
# 5. LIST (1 agent)
# =============================================
echo "=== 5. List (1 agent) ==="
LIST_OUT=$(bcli list --status all 2>&1)
if echo "$LIST_OUT" | grep -q "e2e-main"; then
    assert "list shows e2e-main"
else
    fail "list does not show agent"
fi
echo ""

# =============================================
# 6. EXEC
# =============================================
echo "=== 6. Exec ==="
# Test whoami
run_capture "exec whoami" bcli exec e2e-main whoami
WHOAMI_OUT="$RUN_CAPTURE_OUT"
if echo "$WHOAMI_OUT" | grep -q "bunker-e2e-main"; then
    assert "exec whoami returns agent user"
else
    fail "exec whoami — $WHOAMI_OUT"
fi

# Test basic command execution
run_capture "exec id" bcli exec e2e-main id
ENV_OUT="$RUN_CAPTURE_OUT"
if echo "$ENV_OUT" | grep -q "bunker-e2e-main"; then
    assert "exec id works"
else
    fail "exec id — $ENV_OUT"
fi

# Docker exec (must work now that dockerd is running)
DOCKER_EXEC=$(bcli exec e2e-main -- docker run --rm alpine:latest echo DOCKER-OK 2>&1 || true)
if echo "$DOCKER_EXEC" | grep -q "DOCKER-OK"; then
    assert "docker run via exec"
else
    fail "docker run via exec — $DOCKER_EXEC"
fi
echo ""

# =============================================
# 6a. DOCKER TUNNEL
# =============================================
echo "=== 6a. Docker Tunnel ==="
TUNNEL_PID=""
TUNNEL_LOG=/tmp/bunker-tunnel-e2e-main.log
# nohup cannot invoke a shell function, so this one CLI call stays direct — it
# inherits the BUNKER_HOME exported by battery_cli_state_dir (the battery's own
# CLI state dir), so it is isolated exactly like every bcli call.
nohup "$BUNKER" tunnel e2e-main > "$TUNNEL_LOG" 2>&1 &
TUNNEL_PID=$!
sleep 3

TUNNEL_OK=0
if kill -0 "$TUNNEL_PID" 2>/dev/null; then
    TUNNEL_DOCKER=$(DOCKER_HOST=tcp://localhost:2376 docker version 2>&1 || true)
    if echo "$TUNNEL_DOCKER" | grep -q "Version"; then
        assert "docker version through SSH tunnel"
        TUNNEL_OK=1
    else
        fail "docker version through SSH tunnel — $TUNNEL_DOCKER"
    fi
    kill "$TUNNEL_PID" 2>/dev/null || true
    wait "$TUNNEL_PID" 2>/dev/null || true
else
    fail "tunnel process exited early (log: $TUNNEL_LOG)"
fi
if [ "$TUNNEL_OK" -eq 0 ]; then
    echo "  tunnel log: $(cat "$TUNNEL_LOG" 2>/dev/null | head -5)"
fi
echo ""

# =============================================
# 7. METRICS
# =============================================
echo "=== 7. Server Metrics ==="
SERVER_METRICS_OUT=$(bcli metrics 2>&1 || true)
if echo "$SERVER_METRICS_OUT" | grep -q "Disk Used"; then
    assert "server metrics show disk usage"
else
    note "server metrics — $SERVER_METRICS_OUT"
fi
if echo "$SERVER_METRICS_OUT" | grep -q "Agents"; then
    assert "server metrics show agent summary"
else
    note "server metrics — no agent summary"
fi

echo "=== 7a. Agent Metrics ==="
METRICS_OUT=$(bcli metrics e2e-main 2>&1 || true)
if [ -n "$METRICS_OUT" ]; then
    assert "agent metrics returns data"
else
    note "agent metrics returned empty (may be expected)"
fi
echo ""

# =============================================
# 8. MULTI-AGENT SPAWN
# =============================================
echo "=== 8. Multi-agent Spawn ==="
# CRITICAL (GAP-005 hang): a bare `wait` would block on the battery's OWN
# backgrounded bunkerd in coexist mode (it never exits) — the battery hung
# here until the CI step timeout killed it, leaking all agents. Wait only for
# the spawn CLIs.
SPAWN_PIDS=()
for i in 2 3 4 5; do
    bcli spawn --agent-id "e2e-agent-$i" > /dev/null 2>&1 &
    SPAWN_PIDS+=($!)
done
wait "${SPAWN_PIDS[@]}"
sleep 3

AGENT_COUNT=0
for i in 1 2 3 4 5; do
    AGENT_LABEL=$([ "$i" -eq 1 ] && echo "e2e-main" || echo "e2e-agent-$i")
    if id "bunker-$AGENT_LABEL" >/dev/null 2>&1; then
        AGENT_COUNT=$((AGENT_COUNT + 1))
    fi
done
if [ "$AGENT_COUNT" -ge 5 ]; then
    assert "5 agents spawned ($AGENT_COUNT/5 Linux users)"
else
    fail "agent spawn — only $AGENT_COUNT/5 users created"
fi

# Verify isolation (each has unique home + port range)
UNIQUE_HOMES=$(for u in bunker-e2e-main bunker-e2e-agent-{2..5}; do
    [ -d "/home/$u" ] && echo "/home/$u"
done | wc -l)
if [ "$UNIQUE_HOMES" -eq 5 ]; then
    assert "all 5 home directories unique"
else
    fail "only $UNIQUE_HOMES/5 home dirs"
fi

UNIQUE_PORTS=$(for u in e2e-main e2e-agent-{2..5}; do
    PORT_FILE="/home/bunker-$u/.bunker/ports"
    if [ -f "$PORT_FILE" ]; then
        head -1 "$PORT_FILE" | tr '-' '\n' | head -1
    fi
done | sort -u | wc -l)
if [ "$UNIQUE_PORTS" -ge 5 ]; then
    assert "all 5 port ranges unique"
else
    note "only $UNIQUE_PORTS/5 unique port ranges"
fi
echo ""

# =============================================
# 9. DESTROY
# =============================================
echo "=== 9. Destroy ==="
run_capture "destroy e2e-main" bcli destroy e2e-main --force
DESTROY_OUT="$RUN_CAPTURE_OUT"
if echo "$DESTROY_OUT" | grep -q "destroyed"; then
    assert "destroy e2e-main"
else
    fail "destroy — $DESTROY_OUT"
fi
sleep 2

if id "bunker-e2e-main" >/dev/null 2>&1; then
    fail "user not removed after destroy"
else
    assert "Linux user removed"
fi

if [ -d "/home/bunker-e2e-main" ]; then
    fail "home directory not removed"
else
    assert "home directory cleaned up"
fi

# Re-spawn (idempotency)
bcli spawn --agent-id "e2e-main" > /dev/null 2>&1
sleep 1
if id "bunker-e2e-main" >/dev/null 2>&1; then
    assert "re-spawn same ID works (idempotency)"
else
    fail "re-spawn failed"
fi
echo ""

# =============================================
# 10. CLEANUP ALL
# =============================================
echo "=== 10. Cleanup All ==="
for agent in e2e-main e2e-agent-2 e2e-agent-3 e2e-agent-4 e2e-agent-5; do
    bcli destroy "$agent" --force > /dev/null 2>&1 || true
done
sleep 3

if [ -n "$BUNKERD_COEXIST" ]; then
    REMAINING=$(grep -c '^bunker-e2e-' /etc/passwd 2>/dev/null || true)
else
    REMAINING=$(awk -F: '/^bunker-/ {print $1}' /etc/passwd | wc -l)
fi
if [ "$REMAINING" -eq 0 ]; then
    assert "all agents cleaned up"
else
    note "$REMAINING agents still present (manual cleanup needed)"
fi
echo ""

# =============================================
# 11. CONFIG FILES
# =============================================
echo "=== 11. Config & CI ==="
if [ -f "/etc/bunkerd/config.yaml" ]; then
    assert "bunkerd config exists"
else
    fail "bunkerd config missing"
fi

# INT-CI-010: the CLI config this battery reads and writes is its OWN isolated
# one, never the operator's. Checking (or touching) the operator's config here
# is what made the harness unsafe on a host with a real root CLI.
BATTERY_CLI_CONFIG_FILE="$(grep -rl "^servers:" "$BATTERY_CLI_HOME" 2>/dev/null | head -1 || true)"
if [ -n "$BATTERY_CLI_CONFIG_FILE" ]; then
    assert "battery's own CLI config exists ($BATTERY_CLI_CONFIG_FILE)"
else
    fail "battery's own CLI config missing under $BATTERY_CLI_HOME — the CLI calls above had no server to talk to"
fi
if [ -f "$OPERATOR_CLI_CONFIG" ]; then
    note "operator CLI config $OPERATOR_CLI_CONFIG untouched ($OPERATOR_CLI_CONFIG_HASH)"
else
    note "no operator CLI config at $OPERATOR_CLI_CONFIG — nothing of the operator's to disturb"
fi

if [ -f "/opt/bunker/.github/workflows/ci.yml" ]; then
    assert "GitHub Actions CI workflow exists"
else
    note "CI workflow not found on server (may only exist in git)"
fi

if [ -f "/opt/bunker/regression-tests.sh" ]; then
    assert "regression-tests.sh present"
else
    fail "regression-tests.sh missing"
fi
echo ""

# =============================================
# 12. REGRESSION SUITE (quick)
# =============================================
echo "=== 12. Regression Suite ==="
# Prefer the repo-local copy (CI checkout), fall back to the deployed one.
if [ -f "$(dirname "$0")/regression-tests.sh" ]; then
    REGRESSION_SCRIPT="$(dirname "$0")/regression-tests.sh"
elif [ -f "/opt/bunker/regression-tests.sh" ]; then
    REGRESSION_SCRIPT="/opt/bunker/regression-tests.sh"
else
    REGRESSION_SCRIPT=""
fi
if [ -n "$REGRESSION_SCRIPT" ]; then
    REGRESSION_DIR="$(dirname "$REGRESSION_SCRIPT")"
    cd "$REGRESSION_DIR"
    # INT-CI-010 regression (run 35215499792): the nested suite runs BARE
    # `bunker`, and the CLI prefers BUNKER_HOME over HOME — so handing it this
    # battery's exported state dir let its `connect` overwrite the config file
    # the battery's own sections read; section 13 then dialed the nested port
    # (127.0.0.1:29092) and died 'connection refused'. Give the child its own
    # state dir so neither suite can reach the other's registration, in BOTH
    # modes (BUNKER_HOME is exported in standalone mode too).
    NESTED_CLI_HOME="$(mktemp -d /tmp/bunker-battery-nested-cli-XXXXXX)"
    # INT-CI-012: the child gets its OWN isolated, non-production ports in BOTH
    # modes. Standalone used to pass no ports at all, so the child inherited
    # this battery's exported BUNKERD_REST_ADDR=:18080 / BUNKERD_GRPC_ADDR=
    # :19090, killed the production daemon and then raced it for :18080 (host
    # journal: 'shutting down' → 'Started bunkerd.service' → 'bind: address
    # already in use'). One invocation site, always carrying the ports.
    NESTED_PORTS="$(nested_suite_ports)"
    NESTED_GRPC_ADDR="${NESTED_PORTS%% *}"
    NESTED_REST_ADDR="${NESTED_PORTS##* }"
    run_capture "nested regression suite" env BUNKER_HOME="$NESTED_CLI_HOME" HOME="$NESTED_CLI_HOME" BUNKERD_GRPC_ADDR="$NESTED_GRPC_ADDR" BUNKERD_REST_ADDR="$NESTED_REST_ADDR" bash "$REGRESSION_SCRIPT"
    REG_OUT="$RUN_CAPTURE_OUT"
    # The nested suite's REAL exit status. The previous revision read `$?` after
    # a `... || true` command substitution, so REG_EXIT was always 0.
    REG_EXIT="$RUN_CAPTURE_EXIT"
    remove_own_state_dir "$NESTED_CLI_HOME"
    # Leak guard: this battery's own registration must still be the resolved
    # endpoint. Without the isolation above it is silently replaced and the
    # failure only surfaces as a confusing 'connection refused' in section 13.
    NESTED_CFG="$(grep -rl "url: $BUNKER_DAEMON_URL" "$BATTERY_CLI_HOME" 2>/dev/null | head -1 || true)"
    if [ -n "$NESTED_CFG" ]; then
        assert "nested regression left this battery's CLI registration on $BUNKER_DAEMON_URL"
    else
        fail "nested regression rewrote this battery's CLI registration — later sections would dial the nested suite's ports"
    fi
    # The nested suite's OWN tally ("PASS: N / FAIL: M"), not a grep of the
    # transcript's ✓/✗ cells and the word PASS.
    REG_TALLY="$(nested_tally "$REG_OUT" || true)"
    REG_PASS="${REG_TALLY%% *}"
    REG_FAIL="${REG_TALLY##* }"
    echo "$REG_OUT" | tail -10
    echo "  nested suite tally: PASS=${REG_PASS:-?} FAIL=${REG_FAIL:-?} (exit $REG_EXIT)"
    REG_VERDICT="$(nested_verdict "$REG_EXIT" "$REG_TALLY")"
    case "$REG_VERDICT" in
        PASS:*) assert "regression suite ${REG_VERDICT#PASS: }" ;;
        *)      fail "regression suite ${REG_VERDICT#FAIL: } — see the nested transcript above" ;;
    esac
else
    note "regression-tests.sh not available (neither beside this script nor at /opt/bunker) — the nested suite was never invoked"
fi
echo ""

# =============================================
# 13. IMAGE SPECS (GAP-064) — allowed/rejected/cache/cleanup
# =============================================
# Proves the four acceptance criteria for per-agent image customization:
#   (a) an allowed package-add spec spawns an agent whose image contains the
#       package (`which <pkg>` via exec),
#   (b) a rejected spec returns invalid_argument WITHOUT a build,
#   (c) same/same/changed spec on the same agent causes 1/1/2 builds,
#   (d) destroy removes the agent's own container before the dockerd stops.
# INT-CI-010: every CLI call in this section goes through bcli, i.e. it uses
# the SAME resolved endpoint + token the connect step registered
# (BUNKER_DAEMON_URL / BUNKER_TOKEN, held in the battery's own CLI config) —
# the previous revision inherited whatever the operator's CLI config said and
# hung on a dev port when that config pointed somewhere the daemon was not.
echo "=== 13. Image Specs (GAP-064) ==="
# INT-CI-012: prove the daemon answers BEFORE any spawn/destroy. An unreachable
# daemon reports ONE actionable cell here (endpoint + how to bring it back)
# instead of cascading into 'allowed spec spawn failed' followed by 'agent
# "e2e-imgspec" not found', which was a consequence of the first failure, not a
# second defect. Healthy runs (coexist CI, healthy standalone) add no cell.
if daemon_gate "13. Image Specs (GAP-064)"; then
GAP064_SPEC="$(mktemp /tmp/gap064-spec-XXXXXX.json)"
GAP064_REJECT="$(mktemp /tmp/gap064-reject-XXXXXX.json)"
GAP064_SPEC2="$(mktemp /tmp/gap064-spec2-XXXXXX.json)"
cat > "$GAP064_SPEC" <<'EOF'
{"packages": [{"manager": "apt", "packages": ["jq"]}]}
EOF
cat > "$GAP064_REJECT" <<'EOF'
{"packages": [{"manager": "apt", "packages": ["curl|sh"]}]}
EOF
cat > "$GAP064_SPEC2" <<'EOF'
{"packages": [{"manager": "apt", "packages": ["jq", "curl"]}]}
EOF

# (b) Rejected spec: invalid_argument, NO build, NO user created.
GAP064_REJECT_OUT=$(bcli spawn --agent-id "e2e-imgspec-bad" --image-spec "$GAP064_REJECT" 2>&1 || true)
if echo "$GAP064_REJECT_OUT" | grep -qi "invalid_argument\|invalid image spec"; then
    assert "rejected spec returns invalid_argument"
else
    fail "rejected spec should fail with invalid_argument — $GAP064_REJECT_OUT"
fi
if id "bunker-e2e-imgspec-bad" >/dev/null 2>&1; then
    fail "rejected spec created a user anyway"
    bcli destroy e2e-imgspec-bad --force > /dev/null 2>&1 || true
else
    assert "rejected spec created no user (no side effects)"
fi

# (a) Allowed spec: spawn, then verify jq exists in the agent image.
GAP064_BUILD_START=$(date +%s)
GAP064_SPAWN_OUT=$(bcli spawn --agent-id "e2e-imgspec" --image-spec "$GAP064_SPEC" 2>&1 || true)
if echo "$GAP064_SPAWN_OUT" | grep -q "Agent created"; then
    assert "allowed spec spawns agent"
else
    fail "allowed spec spawn failed — $GAP064_SPAWN_OUT"
fi
if echo "$GAP064_SPAWN_OUT" | grep -q "bunkerd-imagespec-"; then
    assert "spawn bundle reports customized image ref"
else
    note "image ref not shown in bundle (first build may still be customizing)"
fi
# Wait for the first rootless build to finish (bounded).
GAP064_WAITED=0
until bcli exec e2e-imgspec -- which jq > /dev/null 2>&1; do
    sleep 10
    GAP064_WAITED=$((GAP064_WAITED+10))
    if [ "$GAP064_WAITED" -ge 300 ]; then break; fi
done
GAP064_BUILD_END=$(date +%s)
WHICH_JQ=$(bcli exec e2e-imgspec -- which jq 2>&1 || true)
if echo "$WHICH_JQ" | grep -q "/usr/bin/jq\|/bin/jq"; then
    assert "jq present in customized image (which jq → $WHICH_JQ)"
else
    fail "jq NOT found in agent image — $WHICH_JQ"
fi

# (c) Cache: re-spawn same spec (destroy first to exercise rebuild path),
#     then a changed spec — counts builds via the cache markers.
GAP064_CACHE_ROOT="/var/cache/bunkerd/imagespec/e2e-imgspec"
KEY_JQ=$(ls "$GAP064_CACHE_ROOT" 2>/dev/null | head -1 || true)
if [ -n "$KEY_JQ" ]; then
    assert "spec cache dir exists (key ${KEY_JQ:0:12}…)"
else
    fail "no spec cache dir under $GAP064_CACHE_ROOT"
fi

bcli destroy e2e-imgspec --force > /dev/null 2>&1
sleep 2
# Same spec again: must reuse the cache marker + image inspect (no rebuild).
bcli spawn --agent-id "e2e-imgspec" --image-spec "$GAP064_SPEC" > /dev/null 2>&1 || true
sleep 1
GAP064_SAME_T0=$(date +%s)
until bcli exec e2e-imgspec -- which jq > /dev/null 2>&1; do
    sleep 5
    if [ $(( $(date +%s) - GAP064_SAME_T0 )) -ge 120 ]; then break; fi
done
GAP064_SAME_T1=$(date +%s)
GAP064_SAME_SECS=$((GAP064_SAME_T1 - GAP064_SAME_T0))
if [ "$GAP064_SAME_SECS" -lt 60 ]; then
    assert "same spec re-spawn fast (cache hit, ${GAP064_SAME_SECS}s < 60s)"
else
    note "same-spec re-spawn took ${GAP064_SAME_SECS}s (may have rebuilt)"
fi

# Changed spec on the same agent: fresh daemon per re-spawn, so the image is
# rebuilt for the new key — the changed key must appear alongside the old one.
bcli spawn --agent-id "e2e-imgspec-b" --image-spec "$GAP064_SPEC2" > /dev/null 2>&1 || true
sleep 1
GAP064_KEYS=$(bcli exec e2e-imgspec-b -- sh -c 'docker images --format "{{.Repository}}" 2>/dev/null | grep -c bunkerd-imagespec' 2>/dev/null || echo 0)
if [ "${GAP064_KEYS:-0}" -ge 1 ]; then
    assert "changed spec built a NEW image key (daemon has $GAP064_KEYS imagespec image)"
else
    note "changed-spec image count probe: '$GAP064_KEYS'"
fi

# (d) Cleanup hook: after force destroy, the agent's container is gone.
bcli destroy e2e-imgspec --force > /dev/null 2>&1 || true
bcli destroy e2e-imgspec-b --force > /dev/null 2>&1 || true
sleep 2
if id "bunker-e2e-imgspec" >/dev/null 2>&1 || id "bunker-e2e-imgspec-b" >/dev/null 2>&1; then
    fail "imgspec agents not fully destroyed"
else
    assert "imgspec agents destroyed and users removed"
fi

rm -f "$GAP064_SPEC" "$GAP064_REJECT" "$GAP064_SPEC2"
else
    # daemon_gate already reported the single actionable failure.
    note "section 13 skipped: the daemon is unreachable (no spawn/destroy attempted)"
fi
echo ""

# =============================================
# 14. DURABLE AGENT REGISTRY (GAP-070)
# =============================================
# Deterministic and host-safe: every check runs against a throwaway registry in
# /tmp (3 spawns + 1 heartbeat + 1 destroy, plus a corrupt line and a torn
# tail). Nothing here touches the live registry or any real agent, so this
# section runs in BOTH standalone and coexist (CI) mode.
echo "=== 14. Durable Registry (GAP-070) ==="
GAP070_DIR="$(mktemp -d /tmp/bunker-gap070-XXXXXX)"
GAP070_REG="$GAP070_DIR/agents.jsonl"
cat > "$GAP070_REG" <<'EOF'
{"ts":"2026-09-12T10:00:00Z","kind":"spawn","agent_id":"aa","status":"running","created_at":"2026-09-12T10:00:00Z","expires_at":"2026-09-12T16:00:00Z","port_start":31000,"port_end":31099,"ssh_key_path":"/etc/bunkerd/ssh/aa"}
{"ts":"2026-09-12T10:00:01Z","kind":"spawn","agent_id":"bb","status":"running","created_at":"2026-09-12T10:00:01Z","expires_at":"2026-09-12T16:00:00Z","port_start":31100,"port_end":31199,"ssh_key_path":"/etc/bunkerd/ssh/bb"}
{"ts":"2026-09-12T10:00:02Z","kind":"spawn","agent_id":"cc","status":"running","created_at":"2026-09-12T10:00:02Z","expires_at":"2026-09-12T16:00:00Z","port_start":31200,"port_end":31299,"ssh_key_path":"/etc/bunkerd/ssh/cc"}
{"ts":"2026-09-12T10:05:00Z","kind":"heartbeat","agent_id":"aa","expires_at":"2026-09-12T22:00:00Z","status":"running"}
{"ts":"2026-09-12T10:10:00Z","kind":"destroy","agent_id":"cc"}
{"ts":"2026-09-12T10:11:00Z","kind":"spawn", BROKEN-LINE}
EOF
# A torn final line (no newline) must be ignored, not parse-failed loudly.
printf '%s' '{"ts":"2026-09-12T10:12:00Z"' >> "$GAP070_REG"
chmod 600 "$GAP070_REG"

run_capture "registry compact" bcli registry compact --path "$GAP070_REG"
COMPACT_OUT="$RUN_CAPTURE_OUT"
COMPACT_EXIT="$RUN_CAPTURE_EXIT"
if [ "$COMPACT_EXIT" -eq 0 ] && echo "$COMPACT_OUT" | grep -q "registry compacted:"; then
    assert "bunker registry compact rewrote the registry"
else
    fail "bunker registry compact failed (exit $COMPACT_EXIT): $COMPACT_OUT"
fi
if echo "$COMPACT_OUT" | grep -qE "events: [0-9]+ -> [0-9]+" && echo "$COMPACT_OUT" | grep -qE "live agents: 2 -> 2"; then
    assert "compact printed before/after counts ($(echo "$COMPACT_OUT" | grep 'events:' | sed 's/^ *//'))"
else
    fail "compact did not print before/after counts: $COMPACT_OUT"
fi
if [ "$(grep -c '"kind":"spawn"' "$GAP070_REG")" = "2" ] && ! grep -q '"agent_id":"cc"' "$GAP070_REG"; then
    assert "compacted registry holds exactly one current-state record per live agent"
else
    fail "compacted registry has the wrong record set ($(wc -l < "$GAP070_REG") lines)"
fi
if grep -q '"kind":"known"' "$GAP070_REG" && grep -q '"cc"' "$GAP070_REG"; then
    assert "destroyed ID kept in the bounded known-index record (idempotent destroy)"
else
    fail "destroyed ID was not retained by compaction"
fi
if [ "$(stat -c '%a' "$GAP070_REG")" = "600" ]; then
    assert "registry file mode is 0600"
else
    fail "registry file mode is $(stat -c '%a' "$GAP070_REG"), want 600"
fi
if [ -f "$GAP070_REG.1" ]; then
    fail "rotated backup survived compaction (would resurrect stale events)"
else
    assert "compaction removed superseded rotated backups"
fi
run_capture "registry compact (idempotence)" bcli registry compact --path "$GAP070_REG"
COMPACT_OUT2="$RUN_CAPTURE_OUT"
if echo "$COMPACT_OUT2" | grep -qE "live agents: 2 -> 2"; then
    assert "second compaction is idempotent (live set unchanged)"
else
    fail "second compaction changed the live set: $COMPACT_OUT2"
fi
if [ "$(wc -l < "$GAP070_REG")" = "3" ]; then
    assert "compacted registry replays to 2 live records + 1 known-index record"
else
    fail "compacted registry line count = $(wc -l < "$GAP070_REG"), want 3"
fi
# Spawn metadata needed by adopt must survive the rewrite.
if grep -q '"port_start":31000' "$GAP070_REG" && grep -q '"ssh_key_path":"/etc/bunkerd/ssh/aa"' "$GAP070_REG"; then
    assert "exact port reservation + key path survive compaction (adopt metadata)"
else
    fail "compaction dropped port/key metadata needed to restore an exact reservation"
fi
# Cross-process exclusion: a held advisory lock must make compact wait.
if command -v flock > /dev/null 2>&1; then
    flock "$GAP070_REG.lock" -c 'sleep 3' &
    GAP070_LOCK_PID=$!
    sleep 0.3
    GAP070_START=$(date +%s%N)
    bcli registry compact --path "$GAP070_REG" > /dev/null 2>&1
    GAP070_MS=$(( ($(date +%s%N) - GAP070_START) / 1000000 ))
    wait "$GAP070_LOCK_PID" 2>/dev/null || true
    if [ "$GAP070_MS" -ge 1000 ]; then
        assert "compact waited for the cross-process lock (${GAP070_MS}ms)"
    else
        fail "compact ignored a held cross-process lock (${GAP070_MS}ms)"
    fi
else
    note "flock unavailable — cross-process lock check skipped (covered by go test)"
fi
# --dry-run must not modify anything.
GAP070_SUM_BEFORE=$(md5sum < "$GAP070_REG")
bcli registry compact --path "$GAP070_REG" --dry-run > /dev/null 2>&1
GAP070_SUM_AFTER=$(md5sum < "$GAP070_REG")
if [ "$GAP070_SUM_BEFORE" = "$GAP070_SUM_AFTER" ]; then
    assert "compact --dry-run left the registry untouched"
else
    fail "compact --dry-run modified the registry"
fi
rm -rf "$GAP070_DIR"

# Coexist daemons must never open the production registry (they would reconcile
# against — and destroy — the host's real agents).
if [ -n "$BUNKERD_COEXIST" ]; then
    if grep -q "registry:" "$BATTERY_CONFIG" && grep -q "enabled: false" "$BATTERY_CONFIG"; then
        assert "coexist daemon runs with the durable registry disabled (production agents safe)"
    else
        fail "coexist daemon config does not disable the durable registry"
    fi
    note "live-daemon registry checks skipped in coexist mode — replay/reconcile/destroy-twice are covered by go test ./internal/{registry,agent,server}"
else
    # Standalone: full take-over, so exercise the live daemon's own registry.
    # INT-CI-012: gate on a cheap health probe first, so an unreachable daemon
    # reports ONE actionable cell instead of a cascade of not-found cells (a
    # CLI that cannot reach the daemon is not a registry that lost its
    # knowledge — it cannot report the documented idempotent result either).
    if daemon_gate "14. Durable Registry (GAP-070) live-daemon checks"; then
    GAP070_LIVE="${BUNKER_REGISTRY_PATH:-/var/lib/bunkerd/agents.jsonl}"
    bcli spawn gap070-idem > /dev/null 2>&1 || true
    sleep 2
    if [ -f "$GAP070_LIVE" ] && grep -q '"agent_id":"gap070-idem"' "$GAP070_LIVE"; then
        assert "spawn persisted a durable registry record at $GAP070_LIVE"
    else
        fail "no durable spawn record for gap070-idem in $GAP070_LIVE"
    fi
    if [ "$(stat -c '%a' "$GAP070_LIVE" 2>/dev/null)" = "600" ]; then
        assert "live registry file mode is 0600"
    else
        fail "live registry file mode is $(stat -c '%a' "$GAP070_LIVE" 2>/dev/null), want 600"
    fi
    # Idempotent destroy: the FIRST destroy removes the agent, the SECOND must
    # still succeed because the registry remembers it.
    bcli destroy gap070-idem --force > /dev/null 2>&1
    FIRST_EXIT=$?
    bcli destroy gap070-idem --force > /dev/null 2>&1
    SECOND_EXIT=$?
    if [ "$SECOND_EXIT" -eq 0 ]; then
        assert "repeated destroy of a known absent agent succeeds (first=$FIRST_EXIT second=$SECOND_EXIT)"
    else
        fail "repeated destroy failed (first=$FIRST_EXIT second=$SECOND_EXIT) — registry knowledge was lost"
    fi
    # Never-seen IDs: assert the three facts that actually validate GAP-070 for
    # an ID the registry has never seen, each at the layer where it lives.
    # The CLI maps not_found to exit 0 BY CONTRACT (internal/cli/destroy.go:80,90;
    # internal/cli/SKILL.md; DOGFOOD-005) — the "reports not_found" claim lives at
    # the RPC layer (404) and in the registry, which is what this cell asserts.
    # The ID is unique PER RUN (pid + epoch): durable knowledge from an earlier
    # run must never be able to make "never seen" a lie.
    GAP070_NEVER_SEEN="gap070-never-seen-$$-$(date +%s)"
    # (a) CLI half — the documented idempotent UX: exit 0 + the not-found
    # message. Either half regressing must red this cell.
    run_capture "destroy never-seen ($GAP070_NEVER_SEEN)" bcli destroy "$GAP070_NEVER_SEEN"
    GAP070_NEVER_SEEN_EXIT="$RUN_CAPTURE_EXIT"
    if [ "$GAP070_NEVER_SEEN_EXIT" -eq 0 ] && printf '%s' "$RUN_CAPTURE_OUT" | grep -q "not found"; then
        assert "destroy of a never-seen ID reports not found and exits 0 (via $BUNKER_DAEMON_URL): $(printf '%s' "$RUN_CAPTURE_OUT" | head -1)"
    else
        fail "destroy of a never-seen ID did not report the documented idempotent result (exit=$GAP070_NEVER_SEEN_EXIT via $BUNKER_DAEMON_URL): $(printf '%s' "$RUN_CAPTURE_OUT" | head -1)"
    fi
    # (b) Daemon half — the RPC itself really answers NotFound (HTTP 404), so an
    # unreachable or wrong daemon can never be read as a registry bug.
    if command -v curl > /dev/null 2>&1; then
        run_capture "DestroyAgent RPC for a never-seen ID" curl -s -o /dev/null -w '%{http_code}' \
            -X POST "${BUNKER_DAEMON_URL}/bunker.v1.Bunkerd/DestroyAgent" \
            -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${BUNKER_TOKEN}" \
            -d "{\"agent_id\":\"${GAP070_NEVER_SEEN}\"}"
        GAP070_NEVER_SEEN_HTTP="$RUN_CAPTURE_OUT"
        if [ "$GAP070_NEVER_SEEN_HTTP" = "404" ]; then
            assert "the daemon's DestroyAgent RPC reports not_found (HTTP 404) for a never-seen ID at $BUNKER_DAEMON_URL"
        else
            fail "the daemon's DestroyAgent RPC at $BUNKER_DAEMON_URL answered '${GAP070_NEVER_SEEN_HTTP}' for a never-seen ID, want 404 (curl exit=$RUN_CAPTURE_EXIT)"
        fi
    else
        fail "curl is unavailable, so the RPC-level not_found (404) half of GAP-070 could not be probed at $BUNKER_DAEMON_URL — install curl and re-run"
    fi
    # (c) Registry half — the attempt invented no record: a not-found report
    # must not leave durable knowledge behind.
    if [ ! -f "$GAP070_LIVE" ]; then
        fail "no durable registry at $GAP070_LIVE, so the never-seen destroy could not be checked for an invented record"
    elif [ "$(grep -c "\"agent_id\":\"${GAP070_NEVER_SEEN}\"" "$GAP070_LIVE" || true)" = "0" ]; then
        assert "a never-seen destroy recorded nothing in the durable registry"
    else
        fail "a never-seen destroy wrote a durable registry record for $GAP070_NEVER_SEEN although it was reported as not found"
    fi
    if grep -q '"kind":"destroy"' "$GAP070_LIVE" && grep -q '"agent_id":"gap070-idem"' "$GAP070_LIVE"; then
        assert "destroy lifecycle event durably recorded"
    else
        fail "destroy was not persisted to the registry"
    fi
    else
        note "section 14: live-daemon registry cells skipped — the daemon at $BUNKER_DAEMON_URL is unreachable"
    fi
fi
echo ""

# =============================================
# 15. AGENT /tmp ISOLATION + SCOPED SHARED SCRATCH (GAP-075)
# =============================================
# Security-boundary checks, all with disposable names and cleanup traps:
#
#   15.1 host provisioning: idempotent, never touches /etc/fstab, reports the
#        agent group + classifier + pam_exec precondition + helper integrity,
#        and the DEPLOYED sshd PAM block is the agent-NAME-scoped, fail-closed
#        trio (classifier -> pam_exec precondition -> pam_namespace) with no
#        fail-open option; a re-apply preserves a foreign rule
#   15.2 an agent's /tmp is private: agent-vs-root and agent-vs-agent
#   15.2b the agent user is a member of the agent group (the precondition's input)
#   15.2c an ordinary NON-agent SSH user on the same host still sees the host
#        /tmp — the module must not apply host-wide
#   15.2d a detached run unit carries PrivateTmp=yes and its /tmp is not root's
#   15.3 /srv/bunker-share is the ONLY sanctioned exchange point, with
#        group/setgid semantics and a kernel-enforced per-agent size cap: each
#        per-agent directory is its OWN tmpfs mount, the kernel-reported mount
#        size in BYTES must equal the statfs byte count, and a write past the
#        cap fails with ENOSPC
#   15.3b the exchange ROOT is setgid and NOT writable by agents or the world
#        (mode 2750): an agent cannot create an arbitrary uncapped entry beside
#        its own capped directory
#   15.4 unauthorized paths stay unavailable
#   15.5 the daemon's own units carry PrivateTmp=yes
#   15.6 a MALFORMED namespace drop-in cannot succeed open: the agent session is
#        denied instead of continuing with the host's shared /tmp
#   15.6b a WIDENED helper directory (group/world writable) cannot succeed open:
#        the agent could replace the helper and its manifest, so the session is
#        denied until the mode is restored
#
# Enabling the per-session private /tmp edits the sshd PAM stack, so the section
# is fail-safe: if an agent session cannot open after the install, the
# Bunker-managed PAM block is rolled back immediately and the check fails loudly
# (root sessions are unaffected either way).
echo "=== 15. Agent /tmp Isolation + Shared Scratch (GAP-075) ==="
GAP075_UNIQ="$$"
GAP075_A="gap075-a"
GAP075_B="gap075-b"
GAP075_GROUP="${BUNKER_AGENT_GROUP:-bunker-agents}"
GAP075_SHARE="${BUNKER_SHARED_SCRATCH:-/srv/bunker-share}"
GAP075_A_FILE="gap075-${GAP075_UNIQ}-a"
GAP075_ROOT_FILE="gap075-${GAP075_UNIQ}-root"
GAP075_RUN_FILE="gap075-${GAP075_UNIQ}-run"
GAP075_OP_FILE="gap075-${GAP075_UNIQ}-op"
GAP075_FSTAB_BEFORE=$(md5sum /etc/fstab 2>/dev/null | awk '{print $1}')

# ── 15.1 host provisioning ─────────────────────────────────────────────
if ! bcli host-provision --help > /dev/null 2>&1; then
    fail "bunker binary has no host-provision command — build the candidate CLI and point BUNKER_BIN at it (GAP-075 host half missing)"
else
    assert "bunker host-provision is available"
fi
GAP075_DRY=$(bcli host-provision 2>&1 || true)
if echo "$GAP075_DRY" | grep -q "dry run"; then
    assert "host-provision defaults to a dry run"
else
    fail "host-provision without --apply did not report a dry run: $GAP075_DRY"
fi
bcli host-provision --apply > /dev/null 2>&1 || true
GAP075_STATUS_JSON=$(bcli host-provision --status --json 2>&1 || true)
if echo "$GAP075_STATUS_JSON" | grep -q '"isolated": *true'; then
    assert "host isolation active: agent-scoped per-session private /tmp installed"
else
    fail "host isolation not active after --apply: $GAP075_STATUS_JSON"
fi
if echo "$GAP075_STATUS_JSON" | grep -q "\"agent_group\": *\"$GAP075_GROUP\""; then
    assert "the installer reports the agent isolation group $GAP075_GROUP"
else
    fail "host-provision reports the wrong agent group: $GAP075_STATUS_JSON"
fi
if echo "$GAP075_STATUS_JSON" | grep -q '"classifier_module": *true'; then
    assert "the agent-name classifier module (pam_succeed_if) is installed"
else
    fail "pam_succeed_if is missing — the agent-name scoping cannot be expressed: $GAP075_STATUS_JSON"
fi
if echo "$GAP075_STATUS_JSON" | grep -q '"pam_exec_module": *true'; then
    assert "the fail-closed precondition module (pam_exec) is installed"
else
    fail "pam_exec is missing — the fail-closed precondition cannot run: $GAP075_STATUS_JSON"
fi
if echo "$GAP075_STATUS_JSON" | grep -q '"classifier_pattern_ok": *true'; then
    assert "the classifier module supports the agent-name pattern test (Linux-PAM >= 1.6)"
else
    fail "pam_succeed_if cannot express user !~ bunker-* on this host: $GAP075_STATUS_JSON"
fi
if echo "$GAP075_STATUS_JSON" | grep -q '"helper_integrity_ok": *true'; then
    assert "the pam_exec precondition helper matches its sha256 manifest"
else
    fail "the precondition helper/manifest are missing or drifted: $GAP075_STATUS_JSON"
fi
GAP075_FSTAB_AFTER=$(md5sum /etc/fstab 2>/dev/null | awk '{print $1}')
if [ "$GAP075_FSTAB_BEFORE" = "$GAP075_FSTAB_AFTER" ]; then
    assert "/etc/fstab was not modified by host provisioning"
else
    fail "host provisioning modified /etc/fstab"
fi
if [ -f "$GAP075_DROPIN_PATH" ]; then
    assert "pam_namespace drop-in installed in /etc/security/namespace.d"
else
    fail "pam_namespace drop-in missing"
fi

# The DEPLOYED PAM block must be the agent-scoped, fail-closed block, and must
# not carry the fail-open option that lets a malformed config be skipped.
#
#   session    [success=2 auth_err=ignore default=die]    pam_succeed_if.so quiet user !~ bunker-*
#   session    [success=ignore default=die]               pam_exec.so quiet <helper> verify <group>
#   session    required                                   pam_namespace.so
#
# The classifier keys on the reserved NAME pattern (so a deleted agent group
# cannot turn an agent session into an operator session), and the pam_exec
# precondition must run between it and the module.
GAP075_CLASSIFIER_LINE="session    [success=2 auth_err=ignore default=die]    pam_succeed_if.so quiet user !~ bunker-*"
GAP075_VERIFY_RE="^session[[:space:]]+\[success=ignore default=die\][[:space:]]+pam_exec\.so[[:space:]]+quiet[[:space:]]+${GAP075_HELPER_PATH}[[:space:]]+verify[[:space:]]+${GAP075_GROUP}[[:space:]]*$"
if grep -qF "$GAP075_CLASSIFIER_LINE" /etc/pam.d/sshd; then
    assert "the sshd stack carries the agent-NAME classifier (reserved pattern bunker-*)"
else
    fail "the agent-name classifier line is not in /etc/pam.d/sshd — a deleted agent group could fail OPEN"
fi
if grep -qE "$GAP075_VERIFY_RE" /etc/pam.d/sshd; then
    assert "the sshd stack carries the agent-only fail-closed precondition (pam_exec -> $GAP075_HELPER_PATH, group $GAP075_GROUP)"
else
    fail "the pam_exec precondition line is missing or does not name the helper/group: $(grep -n 'pam_exec' /etc/pam.d/sshd || true)"
fi
if grep -qE '^session[[:space:]]+required[[:space:]]+pam_namespace\.so[[:space:]]*$' /etc/pam.d/sshd; then
    assert "the pam_namespace session line is bare and required (no weakening options)"
else
    fail "the pam_namespace session line is missing or carries options"
fi
if grep -q 'ignore_config_error' /etc/pam.d/sshd; then
    fail "the deployed PAM stack still carries ignore_config_error — a malformed namespace config would be SKIPPED and the session would continue with the shared /tmp"
else
    assert "no fail-open ignore_config_error in the deployed PAM stack"
fi
# Adjacency: the classifier jumps over exactly two modules, so the verifier and
# the module line must follow it immediately, in that order.
if awk -v c="$GAP075_CLASSIFIER_LINE" '
        $0==c {getline v; getline m;
               if (v ~ /^session[[:space:]]+\[success=ignore default=die\][[:space:]]+pam_exec\.so/ &&
                   m ~ /^session[[:space:]]+required[[:space:]]+pam_namespace\.so[[:space:]]*$/) found=1}
        END {exit !found}' /etc/pam.d/sshd; then
    assert "classifier -> precondition -> pam_namespace are adjacent (the two-module jump lands correctly)"
else
    fail "the managed block is not adjacent/ordered — the classifier's jump would skip an unrelated module"
fi

# ── 15.1b the managed precondition helper is a root-owned, managed file ─
# The helper's TRUST CHAIN is directory -> manifest -> helper. A writable
# directory or manifest would let a local agent replace both root-owned files by
# rename/unlink, so all three links are asserted, not just the helper's bytes.
GAP075_HELPER_DIR=$(dirname "$GAP075_HELPER_PATH")
if [ -d "$GAP075_HELPER_DIR" ]; then
    GAP075_DIR_META=$(stat -c '%U:%G %a' "$GAP075_HELPER_DIR" 2>/dev/null || echo "missing")
    if [ "$GAP075_DIR_META" = "root:root 755" ]; then
        assert "the helper directory $GAP075_HELPER_DIR is root:root mode 755 (first link of the trust chain)"
    else
        fail "helper directory ownership/mode = $GAP075_DIR_META, want root:root 755 — an agent could replace the helper AND its manifest"
    fi
else
    fail "the helper directory $GAP075_HELPER_DIR is missing — every agent session would be denied"
fi
if [ -f "$GAP075_HELPER_PATH.sha256" ]; then
    GAP075_MANIFEST_META=$(stat -c '%U:%G %a' "$GAP075_HELPER_PATH.sha256" 2>/dev/null || echo "missing")
    if [ "$GAP075_MANIFEST_META" = "root:root 444" ]; then
        assert "the helper manifest is root:root mode 444 (a writable manifest would prove anything)"
    else
        fail "helper manifest ownership/mode = $GAP075_MANIFEST_META, want root:root 444"
    fi
else
    fail "the helper manifest $GAP075_HELPER_PATH.sha256 is missing — every agent session would be denied"
fi
if [ -f "$GAP075_HELPER_PATH" ]; then
    GAP075_HELPER_META=$(stat -c '%U:%G %a' "$GAP075_HELPER_PATH" 2>/dev/null || echo "missing")
    if [ "$GAP075_HELPER_META" = "root:root 755" ]; then
        assert "the pam_exec precondition helper is root:root mode 755"
    else
        fail "helper ownership/mode = $GAP075_HELPER_META, want root:root 755"
    fi
    if [ "$(sha256sum "$GAP075_HELPER_PATH" | awk '{print $1}')" = "$(head -n 1 "$GAP075_HELPER_PATH.sha256" 2>/dev/null)" ]; then
        assert "the helper content matches its sha256 manifest"
    else
        fail "the helper does not match its manifest — every agent session would be denied"
    fi
else
    fail "the pam_exec precondition helper $GAP075_HELPER_PATH is missing — every agent session would be denied"
fi

# ── 15.1c install/repair never deletes an operator-owned rule ──────────
# A foreign pam_succeed_if rule (a shape Bunker itself used to write) must
# survive a re-apply byte for byte, and so must a foreign bare pam_namespace
# rule line's COUNT (a repair must not remove or relocate the operator's own
# modules). The disposable marker line is removed again by the trap.
GAP075_MASK_RESTORE="session    optional     pam_succeed_if.so quiet user = gap075-foreign-$GAP075_UNIQ"
printf '%s\n' "$GAP075_MASK_RESTORE" >> /etc/pam.d/sshd 2>/dev/null || true
GAP075_NS_COUNT_BEFORE=$(grep -cE '^session[[:space:]]+required[[:space:]]+pam_namespace\.so[[:space:]]*$' /etc/pam.d/sshd 2>/dev/null || echo 0)
bcli host-provision --apply > /dev/null 2>&1 || true
if grep -qF "$GAP075_MASK_RESTORE" /etc/pam.d/sshd; then
    assert "a repair re-apply preserved a foreign pam_succeed_if rule byte for byte"
else
    fail "host-provision --apply deleted an operator-owned pam_succeed_if rule"
fi
GAP075_NS_COUNT_AFTER=$(grep -cE '^session[[:space:]]+required[[:space:]]+pam_namespace\.so[[:space:]]*$' /etc/pam.d/sshd 2>/dev/null || echo 0)
if [ "$GAP075_NS_COUNT_BEFORE" = "$GAP075_NS_COUNT_AFTER" ]; then
    assert "a repair re-apply left the bare pam_namespace rule count unchanged ($GAP075_NS_COUNT_AFTER)"
else
    fail "re-apply changed the bare pam_namespace rule count ($GAP075_NS_COUNT_BEFORE -> $GAP075_NS_COUNT_AFTER)"
fi
grep -vF "$GAP075_MASK_RESTORE" /etc/pam.d/sshd > /etc/pam.d/sshd.bunker-battery-tmp 2>/dev/null &&
    cat /etc/pam.d/sshd.bunker-battery-tmp > /etc/pam.d/sshd 2>/dev/null || true
rm -f /etc/pam.d/sshd.bunker-battery-tmp 2>/dev/null || true
GAP075_MASK_RESTORE=""

# ── 15.2 agent private /tmp ────────────────────────────────────────────
bcli spawn --agent-id "$GAP075_A" > /dev/null 2>&1 || true
bcli spawn --agent-id "$GAP075_B" > /dev/null 2>&1 || true

GAP075_SESSION=$(bcli exec "$GAP075_A" id 2>&1 || true)
if echo "$GAP075_SESSION" | grep -q "bunker-$GAP075_A"; then
    assert "agent session opens with pam_namespace enabled and keeps its own uid"
else
    fail "agent session broken after enabling pam_namespace: $GAP075_SESSION"
    bcli host-provision --uninstall --apply > /dev/null 2>&1 || true
    note "rolled back the Bunker-managed pam_namespace configuration after a failed session"
fi

# Positive control first: the agent can write and read its OWN /tmp. Without
# this, a later "hidden" result could just mean the write failed.
GAP075_WRITE=$(bcli exec "$GAP075_A" -- sh -c "echo gap075-a > /tmp/$GAP075_A_FILE && cat /tmp/$GAP075_A_FILE" 2>&1 || true)
if echo "$GAP075_WRITE" | grep -q "gap075-a"; then
    assert "agent A writes and reads its own /tmp/$GAP075_A_FILE (positive control)"
else
    fail "agent A cannot use its own /tmp: $GAP075_WRITE"
fi

# root (host, outside the session) must not see the file.
if [ -e "/tmp/$GAP075_A_FILE" ]; then
    fail "root sees agent A's private /tmp file at /tmp/$GAP075_A_FILE"
else
    assert "root cannot see agent A's private /tmp file"
fi

# A second agent must not see it either.
GAP075_B_LOOK=$(bcli exec "$GAP075_B" -- sh -c "test -e /tmp/$GAP075_A_FILE && echo VISIBLE || echo HIDDEN" 2>&1 || true)
if echo "$GAP075_B_LOOK" | grep -q "HIDDEN"; then
    assert "agent B cannot see agent A's private /tmp file"
else
    fail "agent B can see agent A's /tmp file: $GAP075_B_LOOK"
fi

# ...and the reverse direction: root writes /tmp, the agent must not see it.
echo "gap075-root" > "/tmp/$GAP075_ROOT_FILE" 2>/dev/null || true
GAP075_A_LOOK=$(bcli exec "$GAP075_A" -- sh -c "test -e /tmp/$GAP075_ROOT_FILE && echo VISIBLE || echo HIDDEN" 2>&1 || true)
if echo "$GAP075_A_LOOK" | grep -q "HIDDEN"; then
    assert "agent A cannot see root's /tmp file"
else
    fail "agent A can see root's /tmp file: $GAP075_A_LOOK"
fi
rm -f "/tmp/$GAP075_ROOT_FILE" 2>/dev/null || true

# ── 15.2b the agent is in the isolation group (the guard's input) ──────
if getent group "$GAP075_GROUP" > /dev/null 2>&1; then
    assert "the agent isolation group $GAP075_GROUP exists on the host"
else
    fail "the agent isolation group $GAP075_GROUP does not exist — every agent session would be denied by the pam_exec precondition"
fi
GAP075_MEMBER=$(bcli exec "$GAP075_A" -- id -nG 2>&1 || true)
if echo "$GAP075_MEMBER" | tr ' ' '\n' | grep -qx "$GAP075_GROUP"; then
    assert "the agent session carries the $GAP075_GROUP membership"
else
    fail "agent A's session groups are '$GAP075_MEMBER', which lack $GAP075_GROUP"
fi

# ── 15.2c an ordinary NON-agent SSH user keeps the host /tmp ───────────
# The module must not be mounted host-wide: a disposable non-agent user on the
# same host must still see the host's /tmp, and the agent must not see what that
# user wrote there. The user and its key are removed by the EXIT trap.
GAP075_OP_KEYDIR=$(mktemp -d /tmp/gap075-op-keys-XXXXXX 2>/dev/null) || GAP075_OP_KEYDIR=""
if [ -n "$GAP075_OP_KEYDIR" ] && ssh-keygen -q -t ed25519 -N '' -f "$GAP075_OP_KEYDIR/id" > /dev/null 2>&1; then
    GAP075_OP_USER="gap075-op-${GAP075_UNIQ}"
    if useradd -m -s /bin/bash "$GAP075_OP_USER" > /dev/null 2>&1; then
        GAP075_OP_HOME=$(getent passwd "$GAP075_OP_USER" 2>/dev/null | awk -F: '{print $6}')
        install -d -m 700 -o "$GAP075_OP_USER" -g "$GAP075_OP_USER" "$GAP075_OP_HOME/.ssh" 2>/dev/null || true
        cat "$GAP075_OP_KEYDIR/id.pub" > "$GAP075_OP_HOME/.ssh/authorized_keys" 2>/dev/null || true
        chown "$GAP075_OP_USER" "$GAP075_OP_HOME/.ssh/authorized_keys" 2>/dev/null || true
        chmod 600 "$GAP075_OP_HOME/.ssh/authorized_keys" 2>/dev/null || true
        # A file that exists ONLY in the host /tmp: a non-agent session must see
        # it, an agent session must not.
        echo "gap075-operator" > "/tmp/$GAP075_OP_FILE" 2>/dev/null || true
        GAP075_OP_OUT=$(ssh -i "$GAP075_OP_KEYDIR/id" -o BatchMode=yes -o StrictHostKeyChecking=no \
            -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o LogLevel=ERROR \
            "$GAP075_OP_USER@127.0.0.1" \
            "test -e /tmp/$GAP075_OP_FILE && echo VISIBLE || echo HIDDEN; echo gap075-op-wrote > /tmp/$GAP075_OP_FILE-wrote; id -nG" 2>&1 || true)
        if echo "$GAP075_OP_OUT" | grep -q "VISIBLE"; then
            assert "ordinary non-agent SSH user $GAP075_OP_USER keeps the host /tmp (pam_namespace is NOT applied host-wide)"
        elif echo "$GAP075_OP_OUT" | grep -q "HIDDEN"; then
            fail "ordinary non-agent SSH user $GAP075_OP_USER cannot see the host /tmp — the module is applied host-wide"
        else
            note "ordinary-user scoping check inconclusive (no non-agent SSH session): $(echo "$GAP075_OP_OUT" | head -2 | tr '\n' ' ')"
        fi
        if [ -e "/tmp/$GAP075_OP_FILE-wrote" ]; then
            assert "the non-agent user's /tmp write landed in the host's /tmp"
            GAP075_AGENT_LOOK=$(bcli exec "$GAP075_A" -- sh -c "test -e /tmp/$GAP075_OP_FILE-wrote && echo VISIBLE || echo HIDDEN" 2>&1 || true)
            if echo "$GAP075_AGENT_LOOK" | grep -q "HIDDEN"; then
                assert "the agent cannot see the non-agent user's host /tmp file"
            else
                fail "the agent sees the non-agent user's host /tmp file: $GAP075_AGENT_LOOK"
            fi
        else
            note "the disposable non-agent user could not write to the host /tmp (SSH session unavailable); cross-check skipped"
        fi
    else
        note "could not create the disposable non-agent user; host-wide-scoping check skipped"
    fi
else
    note "ssh-keygen or mktemp unavailable; host-wide-scoping check skipped"
fi
rm -f "/tmp/$GAP075_OP_FILE" "/tmp/$GAP075_OP_FILE-wrote" 2>/dev/null || true

# ── 15.2d a detached run unit is private too ───────────────────────────
bcli run "$GAP075_A" --detach -- sh -c "echo gap075-run > /tmp/$GAP075_RUN_FILE; sleep 90" > /dev/null 2>&1 || true
sleep 6
GAP075_RUN_UNIT=$(systemctl list-units --all --no-legend "bunker-run-$GAP075_A-*" 2>/dev/null | awk '{print $1}' | head -1 || true)
if [ -n "$GAP075_RUN_UNIT" ]; then
    GAP075_RUN_PT=$(systemctl show "$GAP075_RUN_UNIT" -p PrivateTmp --value 2>/dev/null || true)
    if [ "$GAP075_RUN_PT" = "yes" ]; then
        assert "detached run unit $GAP075_RUN_UNIT runs with PrivateTmp=yes"
    else
        fail "detached run unit $GAP075_RUN_UNIT has PrivateTmp=$GAP075_RUN_PT, want yes"
    fi
    if [ -e "/tmp/$GAP075_RUN_FILE" ]; then
        fail "root sees the detached run's private /tmp file at /tmp/$GAP075_RUN_FILE"
    else
        assert "root cannot see the detached run's private /tmp file"
    fi
    systemctl stop "$GAP075_RUN_UNIT" > /dev/null 2>&1 || true
    GAP075_RUN_UNIT=""
else
    note "no running bunker-run-* unit found after --detach; the detached-run PrivateTmp property is covered by go test"
fi

# ── 15.3 shared scratch is the only sanctioned exchange point ──────────
GAP075_ADIR="$GAP075_SHARE/$GAP075_A"
GAP075_HANDOFF="$GAP075_A_FILE-handoff.txt"
bcli exec "$GAP075_A" -- sh -c "echo handoff-$GAP075_UNIQ > $GAP075_ADIR/$GAP075_HANDOFF" > /dev/null 2>&1 || true
GAP075_READ=$(bcli exec "$GAP075_B" -- cat "$GAP075_ADIR/$GAP075_HANDOFF" 2>&1 || true)
if echo "$GAP075_READ" | grep -q "handoff-$GAP075_UNIQ"; then
    assert "agent B reads agent A's file through $GAP075_SHARE (cross-agent exchange)"
else
    fail "cross-agent exchange through $GAP075_SHARE failed: $GAP075_READ"
fi
GAP075_DMODE=$(stat -c '%a' "$GAP075_ADIR" 2>/dev/null || echo missing)
GAP075_DGROUP=$(stat -c '%G' "$GAP075_ADIR/$GAP075_HANDOFF" 2>/dev/null || echo missing)
if [ "$GAP075_DMODE" = "2770" ]; then
    assert "per-agent scratch dir has setgid mode 2770"
else
    fail "scratch dir mode = $GAP075_DMODE, want 2770"
fi
if [ "$GAP075_DGROUP" = "$GAP075_GROUP" ]; then
    assert "files created in the scratch inherit the agent group $GAP075_GROUP (setgid semantics)"
else
    fail "scratch file group = $GAP075_DGROUP, want $GAP075_GROUP"
fi

# ── 15.3b the exchange ROOT is setgid and NOT writable by agents ────────
# The per-agent cap is only a cap while the root above the per-agent
# directories cannot be written by an agent: a plain (uncapped) directory or
# file created directly under the root would be an unbounded write surface
# inside the exchange tree. The root is root-owned, setgid and mode 2750, and
# an agent must be REFUSED there while still being able to use its own
# sanctioned, capped directory (positive control: the handoff write above).
GAP075_ROOT_MODE=$(stat -c '%a' "$GAP075_SHARE" 2>/dev/null || echo missing)
GAP075_ROOT_META=$(stat -c '%U:%G' "$GAP075_SHARE" 2>/dev/null || echo missing)
if [ "$GAP075_ROOT_MODE" = "2750" ]; then
    assert "the exchange root $GAP075_SHARE is mode 2750 (setgid, not writable by the agent group or the world)"
else
    fail "exchange root mode = $GAP075_ROOT_MODE, want 2750 — an agent could create an uncapped entry beside its capped directory"
fi
if [ "$GAP075_ROOT_META" = "root:$GAP075_GROUP" ]; then
    assert "the exchange root is owned by root:$GAP075_GROUP"
else
    fail "exchange root owner = $GAP075_ROOT_META, want root:$GAP075_GROUP"
fi
GAP075_ARB="gap075-arb-$GAP075_UNIQ"
GAP075_ROOT_MKDIR=$(bcli exec "$GAP075_A" -- sh -c \
    "if mkdir $GAP075_SHARE/$GAP075_ARB 2>/dev/null; then echo CREATED; else echo DENIED; fi" 2>&1 || true)
if echo "$GAP075_ROOT_MKDIR" | grep -q "DENIED"; then
    assert "an agent cannot create an arbitrary directory directly under the exchange root"
else
    fail "agent created $GAP075_SHARE/$GAP075_ARB — the exchange root is writable by agents (uncapped bypass): $GAP075_ROOT_MKDIR"
fi
GAP075_ROOT_TOUCH=$(bcli exec "$GAP075_A" -- sh -c \
    "if touch $GAP075_SHARE/$GAP075_ARB-file 2>/dev/null; then echo CREATED; else echo DENIED; fi" 2>&1 || true)
if echo "$GAP075_ROOT_TOUCH" | grep -q "DENIED"; then
    assert "an agent cannot create an arbitrary file directly under the exchange root"
else
    fail "agent created $GAP075_SHARE/$GAP075_ARB-file — the exchange root is writable by agents (uncapped bypass): $GAP075_ROOT_TOUCH"
fi
rmdir "$GAP075_SHARE/$GAP075_ARB" 2>/dev/null || true
rm -f "$GAP075_SHARE/$GAP075_ARB-file" 2>/dev/null || true
GAP075_ARB=""

# Bounding: the directory must be its OWN tmpfs mount whose size the KERNEL
# reports as the configured cap, and a write past the cap must fail with
# ENOSPC.
#
# `findmnt -b -n -o SIZE` reports the mount size in BYTES as plain digits. The
# previous revision read the human-readable mount OPTION (`size=16384k`),
# compared that string with the statfs byte count (never equal) and then fed
# `16384k` to `[ ... -le ... ]`, which aborts with "integer expression
# expected" — so the over-cap ENOSPC proof below was silently skipped. Read
# bytes, and validate every value as digits BEFORE any arithmetic. A directory
# that is not itself a mount makes findmnt report the CONTAINING filesystem
# (root-fs size, fstype ext4), which a pure size comparison would accept as
# "bounded": the tmpfs/mountpoint assertion closes that false green.
GAP075_CAP_RAW=$(findmnt -b -n -o SIZE --target "$GAP075_ADIR" 2>/dev/null || true)
GAP075_CAP_FSTYPE=$(findmnt -n -o FSTYPE --target "$GAP075_ADIR" 2>/dev/null || true)
GAP075_FS_BLOCKS=$(stat -f -c %b "$GAP075_ADIR" 2>/dev/null || true)
GAP075_FS_BSIZE=$(stat -f -c %S "$GAP075_ADIR" 2>/dev/null || true)
GAP075_CAP=""
GAP075_KERNEL_CAP=0
if printf '%s' "$GAP075_CAP_RAW" | grep -qE '^[0-9]+$' &&
    printf '%s' "$GAP075_FS_BLOCKS" | grep -qE '^[0-9]+$' &&
    printf '%s' "$GAP075_FS_BSIZE" | grep -qE '^[0-9]+$'; then
    GAP075_CAP="$GAP075_CAP_RAW"
    GAP075_KERNEL_CAP=$(( GAP075_FS_BLOCKS * GAP075_FS_BSIZE ))
fi
if [ "$GAP075_CAP_FSTYPE" = "tmpfs" ] && mountpoint -q "$GAP075_ADIR" 2>/dev/null; then
    assert "per-agent scratch $GAP075_ADIR is its own tmpfs mount"
else
    fail "per-agent scratch $GAP075_ADIR is not a dedicated tmpfs mount (fstype=${GAP075_CAP_FSTYPE:-<none>}) — the per-agent cap would not be enforced"
fi
if [ -n "$GAP075_CAP" ] && [ "$GAP075_KERNEL_CAP" = "$GAP075_CAP" ]; then
    assert "per-agent scratch is bounded by the kernel at $GAP075_CAP bytes (mount size bytes == statfs bytes)"
else
    fail "scratch cap not reported by the kernel (mount size bytes=${GAP075_CAP_RAW:-<none>} statfs=$GAP075_KERNEL_CAP)"
fi
if [ -n "$GAP075_CAP" ] && [ "$GAP075_CAP" -le 67108864 ]; then
    GAP075_FILL_MB=$(( GAP075_CAP / 1048576 + 2 ))
    GAP075_FILL=$(bcli exec "$GAP075_A" -- sh -c "dd if=/dev/zero of=$GAP075_ADIR/$GAP075_A_FILE-fill bs=1M count=$GAP075_FILL_MB 2>&1; echo write-exit=\$?" 2>&1 || true)
    if echo "$GAP075_FILL" | grep -qE "write-exit=[1-9]|No space left"; then
        assert "writing past the per-agent cap fails (${GAP075_FILL_MB}MiB into a ${GAP075_CAP}B cap)"
    else
        fail "over-cap write did NOT fail: $GAP075_FILL"
    fi
    bcli exec "$GAP075_A" -- rm -f "$GAP075_ADIR/$GAP075_A_FILE-fill" > /dev/null 2>&1 || true
else
    note "per-agent cap is ${GAP075_CAP:-unknown} bytes — exhaustive ENOSPC fill skipped (cap too large for a battery run; kernel-reported size asserted above)"
fi

# ── 15.4 unauthorized paths stay unavailable ───────────────────────────
GAP075_OUTSIDE=$(bcli exec "$GAP075_A" -- sh -c 'touch /srv/gap075-not-allowed 2>/dev/null && echo WROTE || echo DENIED' 2>&1 || true)
if echo "$GAP075_OUTSIDE" | grep -q "DENIED"; then
    assert "agent cannot write outside the sanctioned scratch tree"
else
    fail "agent wrote outside the scratch tree: $GAP075_OUTSIDE"
fi
rm -f /srv/gap075-not-allowed 2>/dev/null || true
GAP075_INST=$(bcli exec "$GAP075_A" -- ls /var/lib/bunkerd/agent-tmp 2>&1 || true)
if echo "$GAP075_INST" | grep -qi "permission denied"; then
    assert "the private-/tmp instance parent is unreadable from inside a session"
else
    note "instance parent listing returned: $GAP075_INST"
fi

# ── 15.5 the daemon's own units carry PrivateTmp=yes ───────────────────
if command -v systemctl > /dev/null 2>&1; then
    GAP075_PT=$(systemctl show "bunker-docker-$GAP075_A" -p PrivateTmp --value 2>/dev/null || true)
    if [ "$GAP075_PT" = "yes" ]; then
        assert "rootless-dockerd unit runs with PrivateTmp=yes"
    else
        fail "bunker-docker-$GAP075_A PrivateTmp=$GAP075_PT, want yes"
    fi
else
    note "systemctl unavailable — unit PrivateTmp check skipped (covered by go test)"
fi

# ── 15.6 a malformed namespace config cannot fail OPEN ─────────────────
# pam_namespace without ignore_config_error makes a malformed line a session
# error; with it the module would SKIP the line and the session would continue
# with the host's shared /tmp. The agent session must be DENIED, and must work
# again once the drop-in is restored (the trap restores it if this battery dies
# mid-check).
if [ -f "$GAP075_DROPIN_PATH" ]; then
    GAP075_DROPIN_RESTORE=$(mktemp /tmp/gap075-dropin-restore-XXXXXX 2>/dev/null) || GAP075_DROPIN_RESTORE=""
    if [ -n "$GAP075_DROPIN_RESTORE" ] && cp -a "$GAP075_DROPIN_PATH" "$GAP075_DROPIN_RESTORE" 2>/dev/null; then
        # One field: pam_namespace cannot parse it (missing instance_prefix and
        # method), so the module reports an error before any polyinstantiation.
        printf '/tmp\n' > "$GAP075_DROPIN_PATH" 2>/dev/null || true
        GAP075_BROKEN_EXIT=0
        bcli exec "$GAP075_A" -- true > /dev/null 2>&1 || GAP075_BROKEN_EXIT=$?
        if [ "$GAP075_BROKEN_EXIT" -ne 0 ]; then
            assert "a malformed namespace drop-in DENIES the agent session (fail closed, exit=$GAP075_BROKEN_EXIT)"
        else
            fail "a malformed namespace drop-in still let the agent session open — the boundary fails OPEN"
        fi
        cp -a "$GAP075_DROPIN_RESTORE" "$GAP075_DROPIN_PATH" 2>/dev/null || true
        rm -f "$GAP075_DROPIN_RESTORE" 2>/dev/null || true
        GAP075_DROPIN_RESTORE=""
        GAP075_RECOVER=$(bcli exec "$GAP075_A" -- true > /dev/null 2>&1; echo "exit=$?")
        if echo "$GAP075_RECOVER" | grep -q "exit=0"; then
            assert "the agent session opens again after the drop-in is restored"
        else
            fail "the agent session did NOT recover after restoring the drop-in: $GAP075_RECOVER"
        fi
    else
        note "could not back up $GAP075_DROPIN_PATH; malformed-config check skipped"
    fi
else
    note "$GAP075_DROPIN_PATH absent; malformed-config check skipped"
fi

# ── 15.6b fail CLOSED when the boundary itself is gone ─────────────────
# Two live proofs that a broken boundary DENIES an agent session instead of
# handing it the host's shared /tmp:
#   1. the pam_exec precondition helper is moved aside (a deleted helper must
#      deny, not silently skip);
#   2. the agent's membership in the isolation group is removed (a lost
#      membership must deny — the first revision failed OPEN here).
# Both are restored immediately, and the EXIT trap restores them if this battery
# dies mid-check.
bcli exec "$GAP075_A" -- true > /dev/null 2>&1 && GAP075_HEALTHY=1 || GAP075_HEALTHY=0
if [ "$GAP075_HEALTHY" = "1" ] && [ -f "$GAP075_HELPER_PATH" ]; then
    GAP075_HELPER_RESTORE=$(mktemp /tmp/gap075-helper-restore-XXXXXX 2>/dev/null) || GAP075_HELPER_RESTORE=""
    if [ -n "$GAP075_HELPER_RESTORE" ] && cp -a "$GAP075_HELPER_PATH" "$GAP075_HELPER_RESTORE" 2>/dev/null; then
        rm -f "$GAP075_HELPER_PATH" 2>/dev/null || true
        GAP075_NOHELPER_EXIT=0
        bcli exec "$GAP075_A" -- true > /dev/null 2>&1 || GAP075_NOHELPER_EXIT=$?
        if [ "$GAP075_NOHELPER_EXIT" -ne 0 ]; then
            assert "a deleted pam_exec precondition helper DENIES the agent session (fail closed, exit=$GAP075_NOHELPER_EXIT)"
        else
            fail "the agent session opened without the precondition helper — the boundary fails OPEN"
        fi
        cp -a "$GAP075_HELPER_RESTORE" "$GAP075_HELPER_PATH" 2>/dev/null || true
        chown root:root "$GAP075_HELPER_PATH" 2>/dev/null || true
        chmod 0755 "$GAP075_HELPER_PATH" 2>/dev/null || true
        rm -f "$GAP075_HELPER_RESTORE" 2>/dev/null || true
        GAP075_HELPER_RESTORE=""
        if bcli exec "$GAP075_A" -- true > /dev/null 2>&1; then
            assert "the agent session opens again after the helper is restored"
        else
            fail "the agent session did NOT recover after restoring the helper"
        fi
    else
        note "could not back up $GAP075_HELPER_PATH; helper-removal check skipped"
    fi
else
    note "agent session unavailable or helper absent; the helper-removal fail-closed check is covered by go test"
fi

if [ "$GAP075_HEALTHY" = "1" ] && getent group "$GAP075_GROUP" > /dev/null 2>&1; then
    GAP075_AGENT_USER="bunker-$GAP075_A"
    usermod -aG "$GAP075_GROUP" "$GAP075_AGENT_USER" > /dev/null 2>&1 || true
    gpasswd -d "$GAP075_AGENT_USER" "$GAP075_GROUP" > /dev/null 2>&1 || true
    GAP075_NOMEMBER_EXIT=0
    bcli exec "$GAP075_A" -- true > /dev/null 2>&1 || GAP075_NOMEMBER_EXIT=$?
    if [ "$GAP075_NOMEMBER_EXIT" -ne 0 ]; then
        assert "a removed isolation-group membership DENIES the agent session (fail closed, exit=$GAP075_NOMEMBER_EXIT)"
    else
        fail "the agent session opened without the group membership — group drift fails OPEN"
    fi
    usermod -aG "$GAP075_GROUP" "$GAP075_AGENT_USER" > /dev/null 2>&1 || true
    if bcli exec "$GAP075_A" -- true > /dev/null 2>&1; then
        assert "the agent session opens again after the membership is restored"
    else
        fail "the agent session did NOT recover after restoring the group membership"
    fi
else
    note "agent session unavailable or group absent; the membership fail-closed check is covered by go test"
fi

# ── 15.6c a WIDENED helper directory cannot succeed open ───────────────
# The helper's trust chain starts at its DIRECTORY: with a group/world-writable
# /usr/lib/bunker, a local agent can replace the root-owned helper and its
# manifest by rename/unlink, so the runtime helper must refuse before checking
# anything else. The directory is widened only for the duration of this check
# and restored immediately (the EXIT trap restores it too).
if [ "$GAP075_HEALTHY" = "1" ] && [ -d "$GAP075_HELPER_DIR" ]; then
    GAP075_DIR_MODE_BEFORE=$(stat -c '%a' "$GAP075_HELPER_DIR" 2>/dev/null || echo "")
    if [ -n "$GAP075_DIR_MODE_BEFORE" ]; then
        chmod 777 "$GAP075_HELPER_DIR" 2>/dev/null || true
        GAP075_HELPERDIR_RESTORE="$GAP075_DIR_MODE_BEFORE"
        GAP075_WIDEDIR_EXIT=0
        bcli exec "$GAP075_A" -- true > /dev/null 2>&1 || GAP075_WIDEDIR_EXIT=$?
        chmod "$GAP075_DIR_MODE_BEFORE" "$GAP075_HELPER_DIR" 2>/dev/null || true
        GAP075_HELPERDIR_RESTORE=""
        if [ "$GAP075_WIDEDIR_EXIT" -ne 0 ]; then
            assert "a group/world-writable helper directory DENIES the agent session (fail closed, exit=$GAP075_WIDEDIR_EXIT)"
        else
            fail "the agent session opened while $GAP075_HELPER_DIR was mode 777 — an agent could replace the helper and its manifest"
        fi
        if bcli exec "$GAP075_A" -- true > /dev/null 2>&1; then
            assert "the agent session opens again after the helper directory mode is restored"
        else
            fail "the agent session did NOT recover after restoring the helper directory mode"
        fi
    else
        note "could not read $GAP075_HELPER_DIR mode; widened-directory check skipped"
    fi
else
    note "agent session unavailable or helper directory absent; the widened-directory fail-closed check is covered by go test"
fi

# ── 15.7 cleanup ───────────────────────────────────────────────────────
bcli exec "$GAP075_A" -- rm -f "$GAP075_ADIR/$GAP075_HANDOFF" > /dev/null 2>&1 || true
bcli destroy "$GAP075_A" --force > /dev/null 2>&1 || true
bcli destroy "$GAP075_B" --force > /dev/null 2>&1 || true
rm -f "/tmp/$GAP075_A_FILE" 2>/dev/null || true
if [ -d "$GAP075_ADIR" ]; then
    note "scratch dir $GAP075_ADIR survived destroy (unmounted later by the daemon or left for inspection)"
else
    assert "destroy removed the agent's bounded scratch directory"
fi
echo ""

# INT-CI-010: the operator's CLI config was fingerprinted before the first CLI
# call; re-check it here (cleanup checks it again for early aborts). A harness
# that rewrote the operator's config must never be able to report VERIFY-PASS.
# NOTE: this stays ABOVE the `# SUMMARY` marker on purpose — everything from
# that marker to EOF is extracted and run standalone (only PASS/FAIL/NOTE
# supplied) by internal/hostsetup TestBatterySummaryExitCodeTracksFailures.
if ! operator_config_check; then
    FAIL=$((FAIL+1))
fi

# =============================================
# SUMMARY
# =============================================
echo ""
echo "=========================================="
echo " RESULTS SUMMARY"
echo "=========================================="
echo "  ✓ Pass:  $PASS"
echo "  ✗ Fail:  $FAIL"
echo "  ⚠ Notes: $NOTE"
echo ""
# Binary certification (DF-BUNKER-3 / QA-BUNKER-3): state EXACTLY what this
# run certified, so a VERIFY-PASS transcript can never be read as "HEAD was
# verified" when the battery actually exercised a different build. The
# ${VAR:-} defaults keep the line standalone-safe: the summary block is
# extracted and executed on its own by
# internal/hostsetup TestBatterySummaryExitCodeTracksFailures, which supplies
# only PASS/FAIL/NOTE.
echo "  CERTIFIED BINARY: ${BUNKER:-<standalone-summary-fixture>} (commit reported: ${BIN_CERT_BIN_COMMIT:-<none>}; verdict: ${BIN_CERT_VERDICT:-UNKNOWN})"
echo ""

if [ "$FAIL" -eq 0 ]; then
    echo "  STATUS: ALL CORE TESTS PASS"
    echo "  VERIFY-PASS"
    if [ "$NOTE" -gt 0 ]; then
        echo "  ($NOTE issues documented — see notes above)"
    fi
    echo ""
    exit 0
else
    echo "  STATUS: $FAIL FAILURES — review above"
    echo "  VERIFY-FAIL"
    echo ""
    # A failing battery MUST exit non-zero: the previous revision ended with a
    # bare `exit 0`, so the CI E2E step stayed green while VERIFY-PASS was
    # absent and the failures were only visible in the log text.
    exit 1
fi
