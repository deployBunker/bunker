#!/usr/bin/env bash
# Hermetic regression test for regression-tests.sh section 4b (the auto-ID
# spawn cell) and for the INT-CI-048 scratch-daemon sizing block.
#
# Why this exists: run 36292076555 reded with
#   "spawn auto-generates ID (got empty — output: Creating agent...)"
# Two independent things made that red unattributable, and NEITHER was the id
# extractor or the CLI:
#   1. the runner carried 9 registered leftover agents (INT-CI-012 keeps them
#      by design) while the suite's scratch daemon was sized for a pristine
#      runner — max_agents 10 AND a port pool of only (20999-20000+1)/100 = 10
#      ranges. Replayed live agents restore both, so 9 + regr-alpha = 10/10 and
#      the spawn was REFUSED at manager_spawn.go Step 1.5, before any side
#      effect: the CLI correctly printed its banner and no "Agent created:"
#      line (correlation across the job: residue ≤ 8 green, residue 9 red);
#   2. the cell's failure diagnostic printed only the FIRST output line, so
#      "capacity full: 10/10 agents" — on the NEXT line — never reached CI.
#
# What this test proves, hermetically (no root, no daemon, no Linux users, no
# host state; one mktemp dir and a stub `bunker` on PATH):
#   1. the cell's extractor DOES read a non-empty id out of a successful
#      auto-ID spawn (grep "Agent created:" | awk '{print $NF}');
#   2. a refused spawn yields an empty id AND the cell's failure diagnostic
#      names the server's refusal — the property the pre-fix `head -1`
#      diagnostic lacked (this assertion is RED-proven against a pre-fix copy
#      of the cell, below);
#   3. the sizing block derives a daemon that clears the residue it replays
#      (pool slots == max_agents, with headroom for this suite's own spawns)
#      and keeps the historical numbers on a pristine host, and the generated
#      configs are WIRED to that derivation (no bare literal can come back).
#
# Run: bash scripts/test-regression-spawn-cell.sh [PATH-TO-SUITE]
# The optional argument points at a MUTATED copy of the suite; that is how
# every assertion below is RED-proven (see the RED-proof case at the end).

set -uo pipefail

SUITE="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/regression-tests.sh}"
[ -f "$SUITE" ] || { echo "FAIL: suite not found at $SUITE"; exit 1; }

RC=0
check() { # check GOT WANT LABEL
    if [ "$1" = "$2" ]; then
        echo "  PASS $3"
    else
        echo "  FAIL $3 (got: $1 / want: $2)"
        RC=1
    fi
}
check_contains() { # check_contains HAYSTACK NEEDLE LABEL
    case "$1" in
        *"$2"*) echo "  PASS $3" ;;
        *) echo "  FAIL $3 (needle '$2' not found in: $1)"; RC=1 ;;
    esac
}
check_missing() { # check_missing HAYSTACK NEEDLE LABEL
    case "$1" in
        *"$2"*) echo "  FAIL $3 (needle '$2' unexpectedly present in: $1)"; RC=1 ;;
        *) echo "  PASS $3" ;;
    esac
}

WORK="$(mktemp -d /tmp/bunker-spawn-cell-XXXXXX)"
# Keep the scratch dir on failure so a red run can be inspected; the fleet's
# unattended sessions refuse rm -rf anyway.
trap 'echo "  (scratch kept at $WORK)"' EXIT

# ── The cell, extracted verbatim from the suite (sed range, the same
# technique scripts/test-regression-cleanup-helpers.sh uses for its helpers) ──
CELL_FILE="$WORK/cell.sh"
sed -n '/^# 4b\. Spawn with auto-generated ID/,/^# 4c\. Verify users exist/p' "$SUITE" \
    | sed '$d' > "$CELL_FILE"
if ! grep -q 'AUTO_ID=$(echo "$OUT"' "$CELL_FILE"; then
    echo "FAIL: could not extract the section-4b cell from $SUITE"
    echo "      (looked for the '# 4b.' … '# 4c.' range and its AUTO_ID extractor)"
    exit 1
fi

# ── Stub `bunker` on PATH: the two output shapes the cell must handle. The
# refusal shape is the REAL one: the CLI prints the progress banner BEFORE the
# RPC (GAP-023), so a refused spawn emits the banner on stdout and the server's
# error on stderr, in that order, with nothing else. ──
STUB_DIR="$WORK/bin"
mkdir -p "$STUB_DIR"
cat > "$STUB_DIR/bunker" <<'STUB'
#!/usr/bin/env bash
case "${STUB_MODE:-success}" in
    success)
        echo "Creating agent..."
        echo "Agent created: 8c954723"
        echo ""
        echo "══════════ Connection Bundle ══════════"
        echo "  Docker SSH:   DOCKER_HOST=ssh://8c954723@127.0.0.1 -p 20022"
        echo "  Port Range:   20000-20099"
        exit 0
        ;;
    refusal)
        # `bunker spawn` against a daemon at capacity: the GAP-023 banner
        # (printed BEFORE the RPC) on stdout, then the failure on stderr in the
        # entry point's own format — cmd/bunker/main.go sets SilenceErrors and
        # prints "bunker: <err>" itself. The error text is the real one: the
        # manager's Step 1.5 refusal (capacity checked before any side effect),
        # wrapped with <unassigned> because the CLI sent no agent id, then
        # mapped to CodeInternal by the service. See
        # internal/agent/intci048_test.go for the same text asserted natively.
        echo "Creating agent..."
        echo "bunker: spawn agent: internal: spawn 9eb02c7c failed at stage capacity: capacity full: 10/10 agents" >&2
        exit 1
        ;;
esac
STUB
chmod +x "$STUB_DIR/bunker"
export PATH="$STUB_DIR:$PATH"
# The stub must win over any real CLI on this host.
if [ "$(command -v bunker)" != "$STUB_DIR/bunker" ]; then
    echo "FAIL: the stub bunker is not first on PATH ($(command -v bunker))"
    exit 1
fi

# run_cell MODE [FILE] — drive the extracted cell (or a mutated copy) with the
# suite's own helpers stubbed to capture the failure text instead of printing
# it. Sets PASS/FAIL/AUTO_ID/FAIL_MSG/AGENT_IDS.
run_cell() {
    local mode="$1" file="${2:-$CELL_FILE}"
    # EXPORTED: the stub runs as a child process and cannot see a plain shell
    # variable (the first draft of this test silently ran the success shape
    # twice).
    export STUB_MODE="$mode"
    PASS=0
    FAIL=0
    AUTO_ID=""
    FAIL_MSG=""
    AGENT_IDS=()
    REGRESSION_TARGET_NAME="regression-local"
    pass() { PASS=$((PASS + 1)); }
    fail() { FAIL=$((FAIL + 1)); FAIL_MSG="$1"; }
    # shellcheck disable=SC1090
    . "$file"
}

echo "── regression-tests.sh section 4b: auto-ID spawn cell ──"

# ── 1. GREEN path: a successful auto-ID spawn yields the id as the LAST field
# of the "Agent created:" line — which is exactly what the cell's
# `grep "Agent created:" | awk '{print $NF}'` parses. ──
run_cell success
check "$PASS" "1" "successful auto-ID spawn passes the cell"
check "$FAIL" "0" "successful auto-ID spawn reports no failure"
check "$AUTO_ID" "8c954723" "the id extractor reads the id out of the bundle"
check "$(printf '%s' "${AGENT_IDS[0]:-}")" "8c954723" "the id is registered for teardown"

# ── 2. RED path: a refused spawn yields NO id (the cell is right to fail) and
# the diagnostic must carry the server's refusal, not just the banner. This is
# the INT-CI-048 reporting fix: the pre-fix diagnostic reported `head -1`,
# which for this output is the banner and nothing else. ──
run_cell refusal
check "$FAIL" "1" "refused spawn fails the cell (no id to report)"
check "$AUTO_ID" "regr-auto-fallback" "refused spawn falls back to the sentinel id"
check_contains "$FAIL_MSG" "Creating agent..." "the banner is still reported"
check_contains "$FAIL_MSG" "capacity full: 10/10 agents" \
    "the diagnostic names the server's refusal (the cause, not the banner)"

# ── 3. The reporting fix is non-vacuous: rebuild the cell with the PRE-FIX
# diagnostic (`head -1`, exactly the line HEAD carried before INT-CI-048) and
# require assertion 2's property check to go RED. Without this arm the whole
# test would pass on the broken cell. ──
PRE_FIX_CELL="$WORK/cell-pre-fix.sh"
awk '
    /^[[:space:]]*fail "spawn auto-generates ID \(got empty/ {
        print "    fail \"spawn auto-generates ID (got empty — output: $(echo \"$OUT\" | head -1))\""
        next
    }
    { print }
' "$CELL_FILE" > "$PRE_FIX_CELL"
if ! grep -q 'head -1' "$PRE_FIX_CELL" || ! grep -q 'AUTO_ID=$(echo "$OUT"' "$PRE_FIX_CELL"; then
    echo "  FAIL could not build the pre-fix cell variant (diagnostic mutation did not apply)"
    echo "       — the RED-proof arm below would be vacuous"
    RC=1
else
    run_cell refusal "$PRE_FIX_CELL"
    case "$FAIL_MSG" in
        *"capacity full: 10/10 agents"*)
            echo "  FAIL the pre-fix diagnostic already reported the cause — assertion 2 is vacuous"
            RC=1
            ;;
        *)
            echo "  PASS the pre-fix diagnostic hides the cause (assertion 2 is RED-provable)"
            ;;
    esac
fi

echo ""
echo "── INT-CI-048: scratch-daemon sizing block ──"

# ── The sizing block, extracted verbatim, run with a SHADOWED grep so the
# residue count can be drive from the test (the block reads /etc/passwd once,
# for the bunker-* user count — the upper bound on the agents the daemon will
# replay LIVE). ──
SIZING_FILE="$WORK/sizing.sh"
awk '/^# ── Scratch-daemon sizing for shared-runner residue/{f=1}
     f && /^if \[ -n "\$BUNKERD_COEXIST" \]/{exit} f' "$SUITE" > "$SIZING_FILE"
if ! grep -q 'REGRESSION_MAX_AGENTS=' "$SIZING_FILE"; then
    echo "FAIL: could not extract the sizing block from $SUITE"
    exit 1
fi

sized_for() { # sized_for RESIDUE -> sets SIZE_MAX_AGENTS/SIZE_PORT_END/...
    (
        set -uo pipefail
        FAKE_RESIDUE="$1"
        grep() { # shadow only the residue count read
            if [ "$1" = "-c" ]; then
                echo "$FAKE_RESIDUE"
                return 0
            fi
            command grep "$@"
        }
        # shellcheck disable=SC1090
        . "$SIZING_FILE"
        echo "$REGRESSION_MAX_AGENTS $REGRESSION_PORT_START $REGRESSION_PORT_END $REGRESSION_PORT_PER_AGENT"
    )
}

read -r M0 S0 E0 P0 <<<"$(sized_for 0)"
check "$M0 $S0 $E0 $P0" "10 20000 20999 100" \
    "pristine host keeps the historical daemon sizing (max_agents 10, 20000-20999)"

read -r M9 S9 E9 P9 <<<"$(sized_for 9)"
SLOTS9=$(((E9 - S9 + 1) / P9))
SPARE9=$((M9 - 9))
check "$SLOTS9" "$M9" "port pool holds exactly as many ranges as max_agents allows"
if [ "$SPARE9" -ge 2 ]; then
    echo "  PASS residue 9 (the RED run's state) leaves $SPARE9 slots for this suite's 2 spawns"
else
    echo "  FAIL residue 9 leaves only $SPARE9 slot(s); the suite spawns 2 (capacity $M9, ports $S9-$E9)"
    RC=1
fi

# ── Wiring: both generated configs must take their numbers from the
# derivation. A bare literal is what let the capacity ceiling and the port pool
# drift apart in the first place. ──
check "$(grep -c '^  max_agents: \$REGRESSION_MAX_AGENTS$' "$SUITE")" "2" \
    "both generated configs wire max_agents to the derivation"
check "$(grep -c '^  max_agents: [0-9]' "$SUITE")" "0" \
    "no bare max_agents literal is left in a generated config"
check "$(grep -c '^  port_range_end: \$REGRESSION_PORT_END$' "$SUITE")" "2" \
    "both generated configs wire port_range_end to the derivation"

echo ""
if [ "$RC" = "0" ]; then
    echo "ALL PASS — section 4b cell contract + INT-CI-048 sizing verified hermetically"
else
    echo "FAILURES — see above"
fi
exit "$RC"
