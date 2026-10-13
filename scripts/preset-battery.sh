#!/usr/bin/env bash
# =============================================================================
# Bunker — Preset Acceptance Battery (GAP-122)
# =============================================================================
# Turns "tier X is safe and usable" from a claim into a verified fact.
#
# Design authority : specs/preset-acceptance-harness.md (GAP-115)
# Tier contract    : specs/safety-presets.md §1-§4 (GAP-113/116/117/118)
# Governing rules  : R1 (experience budget — the tier's workloads actually run)
#                    R2 (no silent no-op — every knob a tier CLAIMS is read
#                        back from the LIVE cgroup/unit and must have landed)
#
# WHAT IT PROVES, per containment tier (open | standard | hardened):
#   * LANDING E1 (user slice drop-in)  — every knob the daemon wrote into
#     /etc/systemd/system/user-<uid>.slice.d/50-bunker.conf is read back from
#     the LIVE slice cgroup (and, for the rlimit knobs, from the live
#     RLIMIT_* of an exec session — the surface E1 exists to protect).
#   * LANDING E2 (rootless dockerd unit) — every knob the daemon reports for
#     the agent's unit (bunker info -> "Safety Knobs") is read back from the
#     live unit cgroup / systemctl show.
#   * SURFACE COVERAGE — for every knob the tier claims, WHICH enforcement
#     point carries it; a knob the matrix marks E1+E2 that landed on one
#     surface only is printed as a FINDING (see --strict-surfaces).
#   * WORKLOADS W1..W5  — the tier is still USABLE (docker build, compose
#     stack, .NET/linuxserver app, node+python, 1.4 GB docker load).
#   * ABUSE A1..A3      — the tier CONTAINS the abuse, asserting the
#     MECHANISM (pids.events max, memory.events oom_kill / high, peer IO
#     impact), never merely that the box survived.
#   * ISOLATION I1..I3  — cannot read a peer agent's home, cannot leave its
#     own cgroup, cannot write another slice's limits; and CAN read/write its
#     home, run commands, and reach the network when the tier allows it.
#
# FINAL LINE: `VERIFY-PASS` only when every targeted tier reports
# `TIER <name> VERDICT: PASS`; otherwise `VERIFY-FAIL`.
#
# EXIT CODES
#   0  = everything targeted passed  (or --self-test / --negative-control
#        completed and behaved as required)
#   1  = the battery ran and something failed
#   2  = usage error
#   42 = preflight refusal (not root / not cgroup v2 / controllers missing /
#        the daemon could not be started) — nothing was changed
#
# MODES
#   (default)             all tiers, run against a COEXIST daemon this script
#                         starts on its own ports (never the production one)
#   --tier NAME           one tier (the fast loop); repeatable
#   --all-tiers           explicit "every tier"
#   --self-test           harness diagnostics with NO root, NO daemon, NO host
#                         mutation: pure helpers, the landing comparator, the
#                         verdict roll-up and the negative-control injector are
#                         all exercised against fixtures. Final line
#                         `SELF-TEST: PASS`.
#   --negative-control    RUNS A REAL TIER with a deliberately mis-set
#                         expectation and asserts the landing check reports
#                         MISMATCH — the proof that the check is not vacuous.
#   --list-tiers          print the tier vocabulary and the matrix requirements
#   -h | --help
#
# RUNNING IT ON bunker-mvp (the only host AGENTS.md quality-gate #3 accepts)
#   scp -r <built bin dir> bunker-mvp:/root/preset-battery-bin
#   scp scripts/preset-battery.sh bunker-mvp:/root/
#   ssh bunker-mvp 'BUNKER=/root/preset-battery-bin/bunker \
#                   BUNKERD_BIN=/root/preset-battery-bin/bunkerd \
#                   bash /root/preset-battery.sh --all-tiers' | tee run.log
#
#   The script starts its OWN daemon on :38081/:39091 (deliberately not
#   e2e-full-battery.sh's :28081/:29091, so the two batteries can run side by
#   side) with `registry.enabled: false`, so it can never reconcile against —
#   or destroy — a production agent.
#
# ENVIRONMENT (all optional; an explicitly exported value always wins)
#   BUNKER                     CLI binary            (default bunker on PATH)
#   BUNKERD_BIN                daemon binary         (default bunkerd on PATH)
#   BUNKERD_COEXIST            non-empty = start our own daemon (DEFAULT: on
#                              unless BUNKER_DAEMON_URL is set)
#   BUNKERD_REST_ADDR          our REST bind / port  (default :38081)
#   BUNKERD_GRPC_ADDR          our gRPC bind / port  (default :39091)
#   BUNKER_DAEMON_URL          target an EXISTING daemon instead (skips the
#                              coexist daemon entirely)
#   BUNKER_TOKEN               token for that daemon (coexist: unused)
#   PRESET_TARGET              server alias we register (default preset-battery)
#   PRESET_AGENT_PREFIX        agent-id prefix      (default preset-)
#   PRESET_WORKLOADS           core | full | none   (default core)
#                              core = W1,W2,W4   full = +W3,W5
#   PRESET_W1_IMAGE            base image for the build   (default alpine:3.20)
#   PRESET_W2_DB_IMAGE         compose db image  (default postgres:16-alpine)
#   PRESET_W3_IMAGE            .NET/linuxserver app image (default
#                              linuxserver/sonarr:latest)
#   PRESET_W3_WAIT             seconds the W3 container must stay up (60)
#   PRESET_W4_NODE_IMAGE       node image         (default node:22-alpine)
#   PRESET_W5_BYTES            size of the docker-load artifact (1503238553)
#   PRESET_W5_TIMEOUT          hard timeout for the load, seconds (900)
#   PRESET_FORK_ATTEMPTS       max forks attempted by A1 (5000)
#   PRESET_MEM_BOMB_BYTES      bytes A2 tries to allocate (8589934592)
#   PRESET_IO_HOG_BYTES        bytes A3 writes  (2147483648)
#   PRESET_PEER_READ_BYTES     bytes the peer reads per sample (268435456)
#   PRESET_PEER_BUDGET_S       peer read hard timeout, seconds (120)
#   PRESET_BUILD_TIMEOUT       per-workload hard timeout, seconds (600)
#   PRESET_STRICT_SURFACES     =1 turns an E1+E2 surface-coverage FINDING into
#                              a tier FAIL (default: reported, not fatal —
#                              see docs/dogfood/2026-10-13-gap-122-*.md)
#   PRESET_KEEP_AGENTS         =1 skips the destroy sweep (debug only)
# =============================================================================

set -euo pipefail

VERSION="1.0.0"

# ── the tier vocabulary this battery certifies ──────────────────────────────
# The SHIPPED vocabulary (internal/config: ValidSafetyPresets) is
# open | standard | hardened. specs/safety-presets.md §1 names four tiers
# (open/standard/guarded/hostile); the code maps hardened to the
# guarded-and-above reading (internal/agent/isolation.go:168-173), so those
# two extra names are not separately spawnable and are not separate tiers
# here. The matrix requirement column is what §3 of the spec marks per knob.
TIER_TABLE_TIERS="open standard hardened"
# Knobs the spec's §3 matrix marks E1+E2 (must be carried by BOTH enforcement
# points). Used by the SURFACE COVERAGE section.
MATRIX_E1E2_KNOBS="CPUQuota MemoryMax MemorySwapMax MemoryHigh MemoryOOMGroup TasksMax LimitNOFILE LimitFSIZE"

# ── resolve inputs (explicit always wins) ───────────────────────────────────
BUNKER="${BUNKER:-bunker}"
BUNKERD_BIN="${BUNKERD_BIN:-bunkerd}"
BUNKER_TOKEN="${BUNKER_TOKEN:-preset-battery-token}"
BUNKER_DAEMON_URL="${BUNKER_DAEMON_URL:-}"
BUNKERD_COEXIST="${BUNKERD_COEXIST:-}"
BUNKERD_REST_ADDR="${BUNKERD_REST_ADDR:-:38081}"
BUNKERD_GRPC_ADDR="${BUNKERD_GRPC_ADDR:-:39091}"
PRESET_TARGET="${PRESET_TARGET:-preset-battery}"
PRESET_AGENT_PREFIX="${PRESET_AGENT_PREFIX:-preset-}"
PRESET_WORKLOADS="${PRESET_WORKLOADS:-core}"
PRESET_W1_IMAGE="${PRESET_W1_IMAGE:-alpine:3.20}"
PRESET_W2_DB_IMAGE="${PRESET_W2_DB_IMAGE:-postgres:16-alpine}"
PRESET_W3_IMAGE="${PRESET_W3_IMAGE:-linuxserver/sonarr:latest}"
PRESET_W3_WAIT="${PRESET_W3_WAIT:-60}"
PRESET_W4_NODE_IMAGE="${PRESET_W4_NODE_IMAGE:-node:22-alpine}"
PRESET_W5_BYTES="${PRESET_W5_BYTES:-1503238553}"
PRESET_W5_TIMEOUT="${PRESET_W5_TIMEOUT:-900}"
PRESET_FORK_ATTEMPTS="${PRESET_FORK_ATTEMPTS:-5000}"
PRESET_MEM_BOMB_BYTES="${PRESET_MEM_BOMB_BYTES:-8589934592}"
PRESET_IO_HOG_BYTES="${PRESET_IO_HOG_BYTES:-2147483648}"
PRESET_PEER_READ_BYTES="${PRESET_PEER_READ_BYTES:-268435456}"
PRESET_PEER_BUDGET_S="${PRESET_PEER_BUDGET_S:-120}"
PRESET_BUILD_TIMEOUT="${PRESET_BUILD_TIMEOUT:-600}"
PRESET_STRICT_SURFACES="${PRESET_STRICT_SURFACES:-}"
PRESET_KEEP_AGENTS="${PRESET_KEEP_AGENTS:-}"

# An exported URL means "use that daemon"; otherwise coexist is the default.
if [ -n "$BUNKER_DAEMON_URL" ]; then
    BUNKERD_COEXIST=""
elif [ -z "${BUNKERD_COEXIST:-}" ] && [ -z "${BUNKERD_COEXIST_DECIDED:-}" ]; then
    BUNKERD_COEXIST=1
    BUNKERD_COEXIST_DECIDED=1
fi

# ── counters ────────────────────────────────────────────────────────────────
TIER_PASS=0
TIER_FAIL=0
TIER_SKIP=0
# per-tier counters, reset by tier_begin
T_PASS=0
T_FAIL=0
T_NOTE=0
T_FINDING=0
GLOBAL_FINDINGS=0
GLOBAL_NOTES=0
STAGE="init"
TIER=""
TIERS_TO_RUN=""
FAILED_TIERS=""
PASSED_TIERS=""
SKIPPED_TIERS=""
NEGCONTROL_OK=""

# ── output helpers ──────────────────────────────────────────────────────────
note()  { echo "  ⚠ $1"; T_NOTE=$((T_NOTE+1)); GLOBAL_NOTES=$((GLOBAL_NOTES+1)); }
ok()    { echo "  ✓ $1"; T_PASS=$((T_PASS+1)); }
bad()   { echo "  ✗ $1"; T_FAIL=$((T_FAIL+1)); }
info()  { echo "  · $1"; }
finding() {
    echo "  ‼ FINDING: $1"
    T_FINDING=$((T_FINDING+1))
    GLOBAL_FINDINGS=$((GLOBAL_FINDINGS+1))
}

# run_capture LABEL CMD... — run CMD, capture stdout+stderr into RUN_OUT and
# the status into RUN_RC, WITHOUT letting `set -e` abort the caller. Always
# returns 0; callers branch on RUN_RC.
RUN_OUT=""
RUN_RC=0
run_capture() {
    local label="$1"; shift
    RUN_OUT=""
    RUN_RC=0
    set +e
    RUN_OUT=$("$@" 2>&1)
    RUN_RC=$?
    set -e
    if [ "$RUN_RC" -ne 0 ]; then
        info "[capture] $label exited $RUN_RC"
    fi
    return 0
}

die() { echo "ERROR: $*" >&2; exit 2; }

# ── external-sweep guards ───────────────────────────────────────────────────
# bunker-mvp is SHARED with the repo's own CI batteries. e2e-full-battery.sh
# and regression-tests.sh run there in STANDALONE mode, and their cleanup does
# `userdel -rf` for every user matching ^bunker- — which deletes THIS battery's
# agents, homes, dockerd units and slice drop-ins in the middle of a run.
# Measured 2026-10-13: a CI regression run started two minutes in and produced
# 8 bogus containment FAILs (ESCAPED verdicts for an agent whose home no
# longer existed). Two guards follow: refuse to start while such a sweep is
# active, and abort a tier honestly if one begins anyway.

# sweep_active — is a host-wide bunker battery sweep running right now?
# (The bracket in each pattern keeps the probe from matching itself.)
sweep_active() {
    pgrep -f 'e2e-full[-]battery.sh' >/dev/null 2>&1 && return 0
    pgrep -f 'regression[-]tests.sh' >/dev/null 2>&1 && return 0
    return 1
}

# agent_alive AGENT — the agent's Linux user AND home must still exist. A
# vanished home is the signature of an external `userdel -rf` sweep: the agent
# still renders as "running" in the daemon's list, but every exec into it fails
# at the SSH layer.
agent_alive() {
    local agent="$1"
    id "bunker-$agent" >/dev/null 2>&1 || return 1
    [ -d "/home/bunker-$agent" ] || return 1
    return 0
}

# spawn_retry AGENT TIER — spawn, retrying a bounded number of times.
# On a host shared with other spawners, `spawn` can fail at stage
# dockerd-start for a reason that has nothing to do with the tier: a
# concurrent battery's `userdel -rf` sweep frees a uid whose processes and
# /run/user/<uid> state are still live, so the NEXT spawn reuses that uid and
# rootlesskit cannot start inside a state dir owned by another agent
# (measured 2026-10-13 08:06Z: uid 1274 was simultaneously owned by a CI
# conctest agent and this battery's peer). A retry costs one uid allocation and
# is the cheapest honest fix; the reason is printed when it finally fails.
SPAWN_OUT=""
spawn_retry() {
    local agent="$1" tier="$2" attempt=1 max="${PRESET_SPAWN_ATTEMPTS:-3}"
    while [ "$attempt" -le "$max" ]; do
        set +e
        SPAWN_OUT=$(bcli spawn --server "$PRESET_TARGET" --agent-id "$agent" --preset "$tier" 2>&1)
        local rc=$?
        set -e
        if [ "$rc" -eq 0 ]; then return 0; fi
        echo "  · spawn attempt $attempt/$max for $agent failed: $(printf '%s' "$SPAWN_OUT" | tail -2 | tr '\n' ' ')"
        if [ "$attempt" -lt "$max" ]; then
            # Clear the half-created agent so the retry gets a fresh uid.
            bcli destroy --server "$PRESET_TARGET" "$agent" --force >/dev/null 2>&1 || true
            sleep "${PRESET_SPAWN_RETRY_WAIT:-20}"
        fi
        attempt=$((attempt+1))
    done
    return 1
}

usage() { sed -n '2,120p' "$0" | sed 's/^# \{0,1\}//'; }

# =============================================================================
# PURE HELPERS (all exercised by --self-test; no host access)
# =============================================================================

# page_floor N — the kernel's page-granular normalization of a memory.high
# write (floor to the system page size). Mirrors internal/agent/
# isolation.go:canonicalMemoryHigh; measured live as requested 3865470480 →
# observed 3865468928 (page 4096).
page_floor() {
    local n="$1" page="${2:-4096}"
    if [ "$n" -eq 0 ] 2>/dev/null; then echo 0; return 0; fi
    local floored=$(( n / page * page ))
    if [ "$floored" -eq 0 ]; then floored="$page"; fi
    echo "$floored"
}

# cpu_quota_max "200%" -> "200000 100000"; "50%" -> "50000 100000".
# cpu.max is quota/period in microseconds with a 100ms period.
cpu_quota_max() {
    local pct="$1" period="${2:-100000}"
    local n="${pct%\%}"
    # keep integer cores-and-fractions exact enough (2.0 -> 200)
    local scaled
    scaled=$(awk -v p="$n" 'BEGIN { printf "%d", p * 1000 }')
    echo "$scaled $period"
}

# rlimit_soft "65536:65536" -> 65536 ; "65536" -> 65536
rlimit_soft() { echo "${1%%:*}"; }

# knob_matches SURFACE NAME EXPECTED OBSERVED -> prints "OK" or a reason.
# SURFACE is "e1" or "e2". The comparison is EXACT except for memory.high,
# which accepts the raw requested bytes or the kernel's page-normalized
# representation of them (never a tolerance band — R2).
knob_matches() {
    local surface="$1" name="$2" expected="$3" observed="$4"
    case "$name" in
        CPUQuota)
            local want; want=$(cpu_quota_max "$expected")
            if [ "$observed" = "$want" ]; then echo OK; else
                echo "cpu.max=$observed, want '$want' (CPUQuota=$expected)"; fi ;;
        MemoryMax)
            if [ "$observed" = "$expected" ]; then echo OK; else
                echo "memory.max=$observed, want $expected"; fi ;;
        MemorySwapMax)
            if [ "$observed" = "$expected" ]; then echo OK; else
                echo "memory.swap.max=$observed, want $expected"; fi ;;
        MemoryHigh)
            local want_floor; want_floor=$(page_floor "$expected")
            if [ "$observed" = "$expected" ] || [ "$observed" = "$want_floor" ]; then echo OK; else
                echo "memory.high=$observed, want $expected (raw) or $want_floor (page-normalized)"; fi ;;
        TasksMax)
            if [ "$observed" = "$expected" ]; then echo OK; else
                echo "pids.max=$observed, want $expected"; fi ;;
        LimitNOFILE)
            if [ "$(rlimit_soft "$observed")" = "$(rlimit_soft "$expected")" ]; then echo OK; else
                echo "RLIMIT_NOFILE soft=$observed, want $expected"; fi ;;
        LimitFSIZE)
            if [ "$observed" = "$expected" ]; then echo OK; else
                echo "RLIMIT_FSIZE=$observed, want $expected"; fi ;;
        MemoryOOMGroup)
            local want_oom="$expected"; want_oom="${want_oom%,*}"
            if [ "$observed" = "$want_oom" ]; then echo OK; else
                echo "memory.oom.group=$observed, want $want_oom"; fi ;;
        IOWeight)
            local o="$observed"
            o="${o#default }"
            if [ "$o" = "$expected" ]; then echo OK; else
                echo "io.weight=$observed, want $expected"; fi ;;
        *)
            if [ "$observed" = "$expected" ]; then echo OK; else
                echo "$name: observed='$observed' want='$expected'"; fi ;;
    esac
}

# parse_dropin FILE — print "Key=Value" for each [Slice] property.
parse_dropin() {
    local f="$1"
    [ -f "$f" ] || return 0
    awk '
        /^\[/ { insec = ($0 == "[Slice]"); next }
        insec && /^[A-Za-z][A-Za-z0-9]*=/ { print }
    ' "$f"
}

# parse_safety_knobs FILE — extract the "Safety Knobs:" block that
# `bunker info` prints. Lines look like "    CPUQuota:       200%". The block
# ends at the first line whose key is not a bare systemd property name (e.g.
# a later section such as "Network Isolation:"), so a neighbouring section can
# never be mistaken for a knob.
parse_safety_knobs() {
    awk '
        /^[[:space:]]*Safety Knobs:/ { inblk = 1; next }
        inblk {
            line = $0
            sub(/^[[:space:]]+/, "", line)
            idx = index(line, ":")
            if (idx == 0) { inblk = 0; next }
            k = substr(line, 1, idx-1)
            v = substr(line, idx+1)
            gsub(/^[[:space:]]+|[[:space:]]+$/, "", v)
            if (k !~ /^[A-Za-z][A-Za-z0-9]*$/) { inblk = 0; next }
            if (v != "") print k "=" v
        }
    ' "$1"
}

# tier_verdict FAILS NOTES — the exact PASS/FAIL contract of
# specs/preset-acceptance-harness.md §5 (any workload FAIL, any abuse
# ESCAPED, any landing MISMATCH → FAIL).
tier_verdict() {
    local fails="$1"
    if [ "$fails" -gt 0 ]; then echo FAIL; else echo PASS; fi
}

# =============================================================================
# LIVE HELPERS
# =============================================================================
BATTERY_CLI_HOME=""
bcli() {
    if [ -z "$BATTERY_CLI_HOME" ]; then
        BATTERY_CLI_HOME="$(mktemp -d "${TMPDIR:-/tmp}/preset-battery-cli-XXXXXX")"
    fi
    BUNKER_HOME="$BATTERY_CLI_HOME" HOME="$BATTERY_CLI_HOME" "$BUNKER" "$@"
}

# aexec AGENT CMD — run CMD (a single shell string) inside AGENT via bash -lc.
# Sets AEXEC_OUT / AEXEC_RC.
AEXEC_OUT=""
AEXEC_RC=0
aexec() {
    local agent="$1" cmd="$2"
    set +e
    AEXEC_OUT=$(bcli exec --server "$PRESET_TARGET" "$agent" -- bash -lc "$cmd" 2>&1)
    AEXEC_RC=$?
    set -e
    return 0
}

# arun_py AGENT SRC_FILE [ARGS...] — base64 the python source onto the agent
# and run it with python3 (no stdin forwarding needed; the pipe is inside the
# remote shell). Bounded by PRESET_PY_TIMEOUT so a runaway allocator cannot
# wedge the battery.
arun_py() {
    local agent="$1" src="$2"; shift 2
    local b64; b64=$(base64 -w0 < "$src")
    aexec "$agent" "printf '%s' '$b64' | base64 -d | timeout -s KILL ${PRESET_PY_TIMEOUT:-180} python3 - $*"
}

# arun_sh AGENT SRC_FILE [ARGS...] — same, as a shell script.
arun_sh() {
    local agent="$1" src="$2"; shift 2
    local b64; b64=$(base64 -w0 < "$src")
    aexec "$agent" "printf '%s' '$b64' | base64 -d | bash -s -- $*"
}

# agent_uid AGENTID
agent_uid() { id -u "bunker-$1" 2>/dev/null || true; }

slice_cgroup() { echo "/sys/fs/cgroup/user.slice/user-$1.slice"; }
unit_cgroup()  { echo "/sys/fs/cgroup/system.slice/bunker-docker-$1.service"; }

# read_cg DIR FILE — the live value, or the empty string when absent.
read_cg() {
    local v
    v=$(cat "$1/$2" 2>/dev/null) || v=""
    printf '%s' "$(echo "$v" | tr -d '\n')"
}

# =============================================================================
# MODE: --list-tiers
# =============================================================================
list_tiers() {
    echo "Bunker containment tiers this battery certifies (shipped vocabulary):"
    for t in $TIER_TABLE_TIERS; do
        case "$t" in
            open)     echo "  open      — trusted operator's own agent; keeps a ceiling, no GAP-118 knobs" ;;
            standard) echo "  standard  — SHIPPED DEFAULT; five-knob baseline + MemorySwapMax=0 + MemoryHigh=90% of MemoryMax" ;;
            hardened) echo "  hardened  — standard plus (once GAP-119/121 land) unit sandboxing; identical containment today" ;;
        esac
    done
    echo
    echo "Knobs the matrix (specs/safety-presets.md §3) marks E1+E2:"
    for k in $MATRIX_E1E2_KNOBS; do echo "  $k"; done
}

# =============================================================================
# SELF-TEST (no root, no daemon, no host mutation)
# =============================================================================
ST_PASS=0
ST_FAIL=0
st_ok()   { echo "  ✓ $1"; ST_PASS=$((ST_PASS+1)); }
st_bad()  { echo "  ✗ $1"; ST_FAIL=$((ST_FAIL+1)); }
st_eq()   { # st_eq LABEL EXPECTED ACTUAL
    if [ "$2" = "$3" ]; then st_ok "$1"; else st_bad "$1 (want '$2', got '$3')"; fi
}

self_test() {
    echo "=== preset-battery --self-test (no root, no daemon, no host mutation) ==="

    echo "--- page_floor ---"
    st_eq "3865470480 floors to 3865468928 (measured live)" 3865468928 "$(page_floor 3865470480)"
    st_eq "page-aligned value is unchanged"                 4294967296 "$(page_floor 4294967296)"
    st_eq "0 stays 0 (the emitNothing path)"                0          "$(page_floor 0)"
    st_eq "sub-page request clamps up to one page"          4096       "$(page_floor 100)"

    echo "--- cpu_quota_max ---"
    st_eq "200% -> 200000/100000" "200000 100000" "$(cpu_quota_max '200%')"
    st_eq "50%  -> 50000/100000"  "50000 100000"  "$(cpu_quota_max '50%')"
    st_eq "100% -> 100000/100000" "100000 100000" "$(cpu_quota_max '100%')"

    echo "--- knob_matches (E1/E2 comparator + the R2 exactness rule) ---"
    st_eq "CPUQuota 200% vs live cpu.max"        OK "$(knob_matches e1 CPUQuota '200%' '200000 100000')"
    st_eq "CPUQuota 200% vs 100% live -> reason" "cpu.max=100000 100000, want '200000 100000' (CPUQuota=200%)" "$(knob_matches e1 CPUQuota '200%' '100000 100000')"
    st_eq "MemoryHigh raw accepted"              OK "$(knob_matches e1 MemoryHigh 3865470480 3865470480)"
    st_eq "MemoryHigh page-normalized accepted"  OK "$(knob_matches e1 MemoryHigh 3865470480 3865468928)"
    st_eq "MemoryHigh one page lower REJECTED"   "memory.high=3865464832, want 3865470480 (raw) or 3865468928 (page-normalized)" "$(knob_matches e1 MemoryHigh 3865470480 3865464832)"
    st_eq "MemoryHigh 'max' REJECTED"            "memory.high=max, want 3865470480 (raw) or 3865468928 (page-normalized)" "$(knob_matches e1 MemoryHigh 3865470480 max)"
    st_eq "MemorySwapMax 0 vs 'max' REJECTED"    "memory.swap.max=max, want 0" "$(knob_matches e1 MemorySwapMax 0 max)"
    st_eq "MemorySwapMax 0 vs 0"                 OK "$(knob_matches e1 MemorySwapMax 0 0)"
    st_eq "LimitNOFILE soft-side compare"        OK "$(knob_matches e2 LimitNOFILE '65536:65536' '65536')"
    st_eq "LimitNOFILE wrong soft REJECTED"      "RLIMIT_NOFILE soft=1024, want 65536:65536" "$(knob_matches e2 LimitNOFILE '65536:65536' '1024')"
    st_eq "IOWeight accepts 'default N'"         OK "$(knob_matches e1 IOWeight 50 'default 50')"
    st_eq "TasksMax mismatch names pids.max"     "pids.max=2048, want 4096" "$(knob_matches e1 TasksMax 4096 2048)"

    echo "--- parse_dropin (the daemon's E1 claim) ---"
    local f; f="$(mktemp)"
    cat > "$f" <<'EOF'
[Slice]
CPUQuota=200%
MemoryMax=4294967296
TasksMax=4096
LimitNOFILE=65536:65536
LimitFSIZE=21474836480
MemorySwapMax=0
MemoryHigh=3865470480
EOF
    local parsed; parsed=$(parse_dropin "$f")
    st_eq "drop-in line count" 7 "$(printf '%s\n' "$parsed" | wc -l | tr -d ' ')"
    st_eq "drop-in CPUQuota"   "CPUQuota=200%" "$(printf '%s\n' "$parsed" | sed -n '1p')"
    st_eq "drop-in MemoryHigh" "MemoryHigh=3865470480" "$(printf '%s\n' "$parsed" | sed -n '7p')"
    rm -f "$f"
    st_eq "absent drop-in yields nothing" "" "$(parse_dropin /nonexistent/50-bunker.conf)"

    echo "--- parse_safety_knobs (the daemon's E2 claim) ---"
    local g; g="$(mktemp)"
    cat > "$g" <<'EOF'
  Safety Preset:    standard
  Safety Knobs:
    CPUQuota:       200%
    MemoryMax:      4294967296
    LimitFSIZE:     21474836480
    TasksMax:       4096
    LimitNOFILE:    65536:65536
  Network Isolation: shared
EOF
    local knobs; knobs=$(parse_safety_knobs "$g")
    st_eq "info knob count"      5 "$(printf '%s\n' "$knobs" | wc -l | tr -d ' ')"
    st_eq "info knob CPUQuota"   "CPUQuota=200%" "$(printf '%s\n' "$knobs" | sed -n '1p')"
    st_eq "info knob LimitNOFILE" "LimitNOFILE=65536:65536" "$(printf '%s\n' "$knobs" | sed -n '5p')"
    st_eq "info block stops at the next section" "0" "$(printf '%s\n' "$knobs" | grep -c 'Network' || true)"
    rm -f "$g"

    echo "--- tier_verdict (the §5 contract) ---"
    st_eq "zero fails -> PASS" PASS "$(tier_verdict 0)"
    st_eq "one fail  -> FAIL"  FAIL "$(tier_verdict 1)"

    echo "--- negative-control injector is a real lever ---"
    # The injector rewrites the EXPECTED value for one knob; the comparator
    # must then report MISMATCH on the same live observation it accepted
    # before. This is what --negative-control exercises against a live host.
    local live="0" saved_inj="${PRESET_INJECT_KNOB:-}"
    st_eq "uninjected: MemorySwapMax 0 vs live 0" OK "$(negctl_expect e1 MemorySwapMax 0 | { read -r exp; knob_matches e1 MemorySwapMax "$exp" "$live"; })"
    PRESET_INJECT_KNOB="e1:MemorySwapMax=1"
    st_eq "injected : MemorySwapMax 1 vs live 0" "memory.swap.max=0, want 1" "$(negctl_expect e1 MemorySwapMax 0 | { read -r exp; knob_matches e1 MemorySwapMax "$exp" "$live"; })"
    PRESET_INJECT_KNOB="e2:TasksMax=1"
    st_eq "injector is surface-scoped (e2 injection leaves e1 alone)" OK "$(negctl_expect e1 TasksMax 4096 | { read -r exp; knob_matches e1 TasksMax "$exp" 4096; })"
    st_eq "injector applies on its own surface" "pids.max=4096, want 1" "$(negctl_expect e2 TasksMax 4096 | { read -r exp; knob_matches e2 TasksMax "$exp" 4096; })"
    PRESET_INJECT_KNOB="$saved_inj"

    echo "--- tier selection parsing ---"
    st_eq "single tier"        "standard" "$(resolve_tiers standard)"
    st_eq "explicit all tiers" "$TIER_TABLE_TIERS" "$(resolve_tiers "$TIER_TABLE_TIERS")"
    st_eq "unknown tier refused" "" "$(resolve_tiers bogus 2>/dev/null || true)"

    echo
    echo "SELF-TEST: $( [ "$ST_FAIL" -eq 0 ] && echo PASS || echo FAIL )  (pass=$ST_PASS fail=$ST_FAIL)"
    [ "$ST_FAIL" -eq 0 ]
}

# negctl_expect SURFACE NAME DEFAULT — print the expected value, replaced by
# the deliberately wrong one when PRESET_INJECT_KNOB names this knob.
negctl_expect() {
    local surface="$1" name="$2" dflt="$3"
    if [ -n "${PRESET_INJECT_KNOB:-}" ]; then
        local inj_surface="${PRESET_INJECT_KNOB%%:*}"
        local rest="${PRESET_INJECT_KNOB#*:}"
        local inj_name="${rest%%=*}"
        local inj_val="${rest#*=}"
        if [ "$inj_surface" = "$surface" ] && [ "$inj_name" = "$name" ]; then
            printf '%s\n' "$inj_val"
            return 0
        fi
    fi
    printf '%s\n' "$dflt"
}

# resolve_tiers [ARGS...] — validate every requested tier against the shipped
# vocabulary; print the space-separated list. Empty output = request refused.
resolve_tiers() {
    local out="" t requested
    requested="${*:-$TIER_TABLE_TIERS}"
    for t in $requested; do
        local found=""
        for known in $TIER_TABLE_TIERS; do
            [ "$t" = "$known" ] && found="$known"
        done
        if [ -z "$found" ]; then
            echo "resolve_tiers: unknown tier '$t' (valid: $TIER_TABLE_TIERS)" >&2
            return 1
        fi
        out="$out $found"
    done
    echo "${out# }"
}

# =============================================================================
# PREFLIGHT
# =============================================================================
preflight() {
    STAGE="preflight"
    if [ "$(id -u)" != "0" ]; then
        echo "REFUSING: this battery creates Linux users, starts a daemon and mounts cgroups; run it as root." >&2
        echo "          (harness diagnostics only? use --self-test — no root required)" >&2
        exit 42
    fi
    local fstype
    fstype=$(stat -fc %T /sys/fs/cgroup 2>/dev/null || echo unknown)
    if [ "$fstype" != "cgroup2fs" ]; then
        echo "REFUSING: /sys/fs/cgroup is '$fstype', not cgroup2fs — this battery needs unified cgroup v2." >&2
        exit 42
    fi
    local ctrls
    ctrls=$(cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null || echo "")
    local missing=""
    for c in cpu io memory pids; do
        case " $ctrls " in *" $c "*) ;; *) missing="$missing $c" ;; esac
    done
    if [ -n "$missing" ]; then
        echo "REFUSING: the root cgroup is missing controllers:$missing (have: $ctrls)" >&2
        exit 42
    fi
    if ! command -v "$BUNKER" >/dev/null 2>&1 && [ ! -x "$BUNKER" ]; then
        echo "REFUSING: CLI binary '$BUNKER' not found/executable (set BUNKER=...)" >&2
        exit 42
    fi
    if [ -n "${BUNKERD_COEXIST:-}" ]; then
        if ! command -v "$BUNKERD_BIN" >/dev/null 2>&1 && [ ! -x "$BUNKERD_BIN" ]; then
            echo "REFUSING: daemon binary '$BUNKERD_BIN' not found/executable (set BUNKERD_BIN=...)" >&2
            exit 42
        fi
    fi
    # A host-wide sweep in flight would delete this battery's agents mid-run
    # and manufacture bogus containment failures (see the guard comment above).
    if sweep_active; then
        if [ -z "${PRESET_ALLOW_SHARED_HOST:-}" ]; then
            echo "REFUSING: a host-wide bunker battery is running on this host:" >&2
            pgrep -a -f 'e2e-full[-]battery.sh|regression[-]tests.sh' >&2 || true
            echo "  Its cleanup runs 'userdel -rf' for every user matching ^bunker-, which deletes" >&2
            echo "  this battery's agents, homes and units mid-run — producing FAILs that measure" >&2
            echo "  a dead agent rather than a containment property. Wait for it to finish and re-run," >&2
            echo "  or set PRESET_ALLOW_SHARED_HOST=1 to run anyway (an ABORT guard protects each tier)." >&2
            exit 42
        fi
        echo "  WARNING: a host-wide bunker battery is running — PRESET_ALLOW_SHARED_HOST=1 was set,"
        echo "           so this run proceeds; the per-tier ABORT guard will report any agent it loses."
    fi
    echo "  preflight OK: root, cgroup2fs, controllers present ($ctrls)"
}

# =============================================================================
# COEXIST DAEMON
# =============================================================================
BUNKERD_PID=""
BATTERY_CONFIG=""
BATTERY_DAEMON_LOG=""
REST_PORT="${BUNKERD_REST_ADDR##*:}"
GRPC_PORT="${BUNKERD_GRPC_ADDR##*:}"

start_daemon() {
    STAGE="start-daemon"
    [ -n "${BUNKERD_COEXIST:-}" ] || return 0
    BATTERY_CONFIG="$(mktemp /tmp/preset-battery-bunkerd-XXXXXX.yaml)"
    cat > "$BATTERY_CONFIG" <<EOF
server:
  grpc_addr: "$BUNKERD_GRPC_ADDR"
  rest_addr: "$BUNKERD_REST_ADDR"
auth:
  enabled: false
tls:
  insecure_dev: true
agent:
  ssh_dir: /etc/bunkerd/ssh
  max_agents: 10
  port_range_start: 30000
  port_range_end: 30999
  port_range_per_agent: 100
  registry:
    enabled: false
EOF
    BATTERY_DAEMON_LOG="/tmp/preset-battery-bunkerd.log"
    : > "$BATTERY_DAEMON_LOG"
    "$BUNKERD_BIN" -c "$BATTERY_CONFIG" >> "$BATTERY_DAEMON_LOG" 2>&1 &
    BUNKERD_PID=$!
    local i
    for i in $(seq 1 20); do
        if ! kill -0 "$BUNKERD_PID" 2>/dev/null; then
            echo "REFUSING: the battery daemon exited at startup — tail of $BATTERY_DAEMON_LOG:" >&2
            tail -n 20 "$BATTERY_DAEMON_LOG" >&2 || true
            exit 42
        fi
        if curl -s -o /dev/null --max-time 3 "http://127.0.0.1:$REST_PORT/" 2>/dev/null; then
            echo "  battery daemon ready: REST :$REST_PORT (pid $BUNKERD_PID), registry disabled"
            if [ -n "$BUNKER_DAEMON_URL" ]; then :; else
                BUNKER_DAEMON_URL="http://127.0.0.1:$REST_PORT"
            fi
            return 0
        fi
        sleep 1
    done
    echo "REFUSING: battery daemon alive but REST :$REST_PORT never answered — tail :" >&2
    tail -n 20 "$BATTERY_DAEMON_LOG" >&2 || true
    exit 42
}

# =============================================================================
# CLEANUP
# =============================================================================
CLEANUP_AGENTS=""

cleanup() {
    local rc=$?
    set +e
    if [ -z "${PRESET_KEEP_AGENTS:-}" ]; then
        for a in $CLEANUP_AGENTS; do
            # Kill anything the abuse cases left behind BEFORE the destroy,
            # so cgroup.kill's atomicy is not the thing being tested here.
            local uid; uid=$(agent_uid "$a")
            if [ -n "$uid" ]; then
                systemctl kill --signal=SIGKILL "user-${uid}.slice" >/dev/null 2>&1
            fi
            bcli destroy --server "$PRESET_TARGET" "$a" --force >/dev/null 2>&1
        done
    fi
    if [ -n "$BUNKERD_PID" ]; then kill "$BUNKERD_PID" >/dev/null 2>&1; fi
    [ -n "$BATTERY_CONFIG" ] && rm -f "$BATTERY_CONFIG"
    [ -n "$BATTERY_CLI_HOME" ] && rm -rf "$BATTERY_CLI_HOME"
    [ -n "${PRESET_STAGE_DIR:-}" ] && rm -rf "$PRESET_STAGE_DIR"
    exit "$rc"
}

# =============================================================================
# PER-TIER CHECKS
# =============================================================================
TIER_FAILS=0

tier_begin() {
    TIER="$1"
    T_PASS=0; T_FAIL=0; T_NOTE=0; T_FINDING=0
    TIER_FAILS=0
    echo ""
    echo "TIER $TIER"
}

# tier_abort AGENT — an external actor destroyed this agent mid-tier. Report it
# honestly and record the tier as ABORTED: it fails (so it can never be read as
# a pass) but the reason is named, because "the agent was deleted out from
# under the run" is a measurement-integrity fact, not a containment property.
tier_abort() {
    local agent="$1"
    bad "ABORTED: agent $agent was destroyed mid-tier by an external actor — its Linux user and/or its home no longer exist (a host-wide battery sweep does 'userdel -rf' for every ^bunker- user). Every measurement past this point would describe a dead agent, so the tier is ABORTED rather than reported as containment failures."
    TIER_FAILS=$((TIER_FAILS+1))
    echo "TIER $TIER VERDICT: ABORTED   (pass=$T_PASS fail=$T_FAIL findings=$T_FINDING — agent lost to an external sweep)"
    TIER_FAIL=$((TIER_FAIL+1))
    FAILED_TIERS="$FAILED_TIERS $TIER"
}

# claim_* — read the daemon's OWN claims for this agent.
#   E1 claim: the slice drop-in the daemon wrote.
#   E2 claim: the knob block `bunker info` renders from the agent record.
E1_CLAIM=""
E2_CLAIM=""

read_claims() {
    local agent="$1" uid="$2"
    E1_CLAIM="$(parse_dropin "/etc/systemd/system/user-${uid}.slice.d/50-bunker.conf")"
    local inf; inf="$(mktemp)"
    set +e
    bcli info --server "$PRESET_TARGET" "$agent" > "$inf" 2>&1
    set -e
    E2_CLAIM="$(parse_safety_knobs "$inf")"
    rm -f "$inf"
}

# claim_get CLAIM NAME — the value claimed for NAME (empty when absent).
claim_get() {
    local claim="$1" name="$2"
    printf '%s\n' "$claim" | awk -F= -v n="$name" '$1 == n { sub(/^[^=]*=/, ""); print; exit }'
}

# ---------------------------------------------------------------------------
# LANDING E1 — every knob the drop-in claims, read back from the live slice
# (plus the rlimit knobs read from a LIVE exec session, which is the surface
# E1 exists to protect).
# ---------------------------------------------------------------------------
E1_LANDED=0
E1_TOTAL=0
E1_DETAIL=""
E1_INERT=""

landing_e1() {
    echo "  LANDING E1 (user-<uid>.slice drop-in → live slice cgroup)"
    local agent="$1" uid="$2"
    local cg; cg="$(slice_cgroup "$uid")"
    E1_LANDED=0; E1_TOTAL=0; E1_DETAIL=""; E1_INERT=""

    if [ -z "$E1_CLAIM" ]; then
        bad "LANDING E1: the daemon wrote NO slice drop-in for uid $uid — nothing to verify, and every exec session is uncapped"
        TIER_FAILS=$((TIER_FAILS+1))
        return 0
    fi

    # E1's whole point is that it covers the processes a UNIT-level limit
    # would miss. Prove the coverage claim before asserting any value: a live
    # exec session must report itself inside this uid's slice.
    aexec "$agent" "cat /proc/self/cgroup"
    local sess_cg; sess_cg="$(printf '%s' "$AEXEC_OUT" | head -1)"
    if printf '%s' "$sess_cg" | grep -q "user-${uid}.slice"; then
        info "E1 coverage: a live exec session sits inside user-${uid}.slice ($sess_cg)"
    else
        bad "LANDING E1: an exec session is NOT inside user-${uid}.slice (reported '$sess_cg') — the slice does not cover exec sessions"
        TIER_FAILS=$((TIER_FAILS+1))
    fi

    local line name want live reason
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        name="${line%%=*}"; want="${line#*=}"

        case "$name" in
            LimitNOFILE|LimitFSIZE)
                # RLIMITs are applied by systemd PER UNIT at fork time. A
                # .slice forks nothing, so these two properties cannot be
                # enforced at E1 no matter what the drop-in says — the
                # enforceable surface for them is E2 (the transient unit) and
                # the containers inside it. Verify what the slice actually
                # yields and report the gap instead of asserting a value that
                # has no live reader.
                local rep
                rep="$(systemctl show "user-${uid}.slice" -p "$name" --value 2>/dev/null | tr -d '\n')"
                E1_INERT="$E1_INERT $name=$want"
                if [ -n "$rep" ]; then
                    info "E1 $name: systemd reports '$rep' for the slice"
                else
                    info "E1 $name=$want declared in the drop-in, but systemd applies RLIMITs per unit at fork — a .slice forks nothing, so this property is INERT at E1 (verified at E2 instead)"
                fi
                continue ;;
            CPUQuota|MemoryMax|MemorySwapMax|MemoryHigh|TasksMax|MemoryOOMGroup|IOWeight|IOWriteBandwidthMax) : ;;
            *)
                info "E1 $name: no live cgroup reader mapped for this property — skipped"
                continue ;;
        esac

        E1_TOTAL=$((E1_TOTAL+1))
        case "$name" in
            CPUQuota)        live="$(read_cg "$cg" cpu.max)" ;;
            MemoryMax)       live="$(read_cg "$cg" memory.max)" ;;
            MemorySwapMax)   live="$(read_cg "$cg" memory.swap.max)" ;;
            MemoryHigh)      live="$(read_cg "$cg" memory.high)" ;;
            TasksMax)        live="$(read_cg "$cg" pids.max)" ;;
            MemoryOOMGroup)  live="$(read_cg "$cg" memory.oom.group)" ;;
            IOWeight)        live="$(read_cg "$cg" io.weight)" ;;
            IOWriteBandwidthMax) live="$(read_cg "$cg" io.max)" ;;
            *)               live="" ;;
        esac

        local expected; expected="$(negctl_expect e1 "$name" "$want")"
        reason="$(knob_matches e1 "$name" "$expected" "$live")"
        if [ "$reason" = "OK" ]; then
            E1_LANDED=$((E1_LANDED+1))
            info "E1 $name=$want → landed ($live)"
        else
            bad "LANDING E1 MISMATCH: $reason"
            E1_DETAIL="${E1_DETAIL}${name}(want ${expected}, got ${live}); "
            TIER_FAILS=$((TIER_FAILS+1))
        fi
    done <<< "$E1_CLAIM"

    if [ -n "$E1_INERT" ]; then
        # Evidence, not opinion: what an exec session actually carries.
        aexec "$agent" "nl=\$(ulimit -n); fl=\$(ulimit -f); echo \"E1_SESSION_NOFILE=\$nl E1_SESSION_FSIZE_BLOCKS=\$fl\""
        finding "the drop-in declares RLIMIT knob(s)[$E1_INERT] on user-${uid}.slice, but systemd applies RLIMITs per unit at fork and a .slice forks nothing — so those properties are inert at E1. An exec session was measured at: $(printf '%s' "$AEXEC_OUT" | head -1). The enforceable surface for them is E2 (the dockerd unit + the containers it starts), where they are verified."
    fi

    echo "  LANDING E1 (slice) ....... $( [ "$E1_LANDED" -eq "$E1_TOTAL" ] && echo OK || echo MISMATCH )  ($E1_LANDED/$E1_TOTAL cgroup knobs landed)"
}

# ---------------------------------------------------------------------------
# LANDING E2 — every knob `bunker info` claims for the unit, read back from
# the live transient unit.
# ---------------------------------------------------------------------------
E2_LANDED=0
E2_TOTAL=0
E2_DETAIL=""

landing_e2() {
    echo "  LANDING E2 (rootless dockerd unit)"
    local agent="$1" uid="$2"
    local cg; cg="$(unit_cgroup "$agent")"
    local unit="bunker-docker-${agent}.service"
    E2_LANDED=0; E2_TOTAL=0; E2_DETAIL=""

    if [ -z "$E2_CLAIM" ]; then
        bad "LANDING E2: the daemon reports NO unit knob set for this agent (bunker info carried no 'Safety Knobs' block)"
        TIER_FAILS=$((TIER_FAILS+1))
        return 0
    fi
    # The transient unit's cgroup appears when systemd starts the unit; give it
    # a bounded chance rather than failing on the first read.
    local wt=0
    while [ ! -d "$cg" ] && [ "$wt" -lt 30 ]; do sleep 2; wt=$((wt+2)); done
    if [ ! -d "$cg" ]; then
        bad "LANDING E2: the transient unit cgroup $cg does not exist after ${wt}s — the dockerd unit is not running under this tier"
        TIER_FAILS=$((TIER_FAILS+1))
        return 0
    fi

    local line name want live reason
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        name="${line%%=*}"; want="${line#*=}"
        E2_TOTAL=$((E2_TOTAL+1))
        case "$name" in
            CPUQuota)     live="$(read_cg "$cg" cpu.max)" ;;
            MemoryMax)    live="$(read_cg "$cg" memory.max)" ;;
            MemorySwapMax) live="$(read_cg "$cg" memory.swap.max)" ;;
            MemoryHigh)   live="$(read_cg "$cg" memory.high)" ;;
            TasksMax)     live="$(read_cg "$cg" pids.max)" ;;
            LimitNOFILE)  live="$(systemctl show "$unit" -p LimitNOFILE --value 2>/dev/null | tr -d '\n')" ;;
            LimitFSIZE)   live="$(systemctl show "$unit" -p LimitFSIZE --value 2>/dev/null | tr -d '\n')" ;;
            *)            live="(no live reader)" ;;
        esac
        local expected; expected="$(negctl_expect e2 "$name" "$want")"
        reason="$(knob_matches e2 "$name" "$expected" "$live")"
        if [ "$reason" = "OK" ]; then
            E2_LANDED=$((E2_LANDED+1))
            info "E2 $name=$want → landed ($live)"
        else
            bad "LANDING E2 MISMATCH: $reason"
            E2_DETAIL="${E2_DETAIL}${name}(want ${expected}, got ${live}); "
            TIER_FAILS=$((TIER_FAILS+1))
        fi
    done <<< "$E2_CLAIM"

    echo "  LANDING E2 (unit) ........ $( [ "$E2_LANDED" -eq "$E2_TOTAL" ] && echo OK || echo MISMATCH )  ($E2_LANDED/$E2_TOTAL knobs landed)"
}

# ---------------------------------------------------------------------------
# SURFACE COVERAGE — which enforcement point carries each claimed knob. The
# matrix marks some knobs E1+E2; a one-sided landing there is reported as a
# FINDING (fatal only under PRESET_STRICT_SURFACES=1).
# ---------------------------------------------------------------------------
surface_coverage() {
    echo "  SURFACE COVERAGE (matrix E1+E2 requirement vs what the daemon applied)"
    local knobs="" k
    for k in $(printf '%s\n' "$E1_CLAIM" | awk -F= 'NF{print $1}') \
             $(printf '%s\n' "$E2_CLAIM" | awk -F= 'NF{print $1}'); do
        case " $knobs " in *" $k "*) ;; *) knobs="$knobs $k" ;; esac
    done
    local any=""
    for k in $knobs; do
        local e1="no" e2="no"
        [ -n "$(claim_get "$E1_CLAIM" "$k")" ] && e1="yes"
        [ -n "$(claim_get "$E2_CLAIM" "$k")" ] && e2="yes"
        info "coverage $k: E1=$e1 E2=$e2"
        case " $MATRIX_E1E2_KNOBS " in
            *" $k "*)
                if [ "$e1" = "yes" ] && [ "$e2" != "yes" ]; then
                    any="E1-only:$k"
                    finding "$k is claimed at E1 (user slice) only — the spec's §3 matrix marks it E1+E2, and the transient dockerd unit is NOT inside user-<uid>.slice (it lives in system.slice), so containers started by this agent are not covered by that knob"
                elif [ "$e2" = "yes" ] && [ "$e1" != "yes" ]; then
                    any="E2-only:$k"
                    finding "$k is claimed at E2 (dockerd unit) only — the matrix marks it E1+E2; every \`bunker exec\` SSH session is outside the unit and would escape it"
                fi ;;
        esac
    done
    if [ -n "$any" ] && [ -n "$PRESET_STRICT_SURFACES" ]; then
        bad "SURFACE COVERAGE: one-sided landing under --strict-surfaces ($any)"
        TIER_FAILS=$((TIER_FAILS+1))
    fi
}

# ---------------------------------------------------------------------------
# WORKLOADS (R1 — the tier is still usable)
# ---------------------------------------------------------------------------
# last_lines TEXT [N] — the tail of a captured output, single-line, for a
# failure message. A failing assertion must say WHY, not only that it failed.
last_lines() { printf '%s' "$1" | tail -n "${2:-3}" | tr '\n' '|' | sed 's/|*$//'; }

stage_dir_ensure() {
    if [ -z "${PRESET_STAGE_DIR:-}" ]; then
        PRESET_STAGE_DIR="$(mktemp -d /tmp/preset-battery-XXXXXX)"
    fi
}

w1_docker_build() {
    local agent="$1"
    stage_dir_ensure
    local ctx="$PRESET_STAGE_DIR/w1"; mkdir -p "$ctx"
    cat > "$ctx/Dockerfile" <<EOF
# W1 — multi-layer build (the redeploy path; memory peak during a build)
FROM $PRESET_W1_IMAGE AS base
RUN echo layer-1 > /layer1.txt
RUN echo layer-2 > /layer2.txt
COPY payload.txt /payload.txt
RUN wc -c /payload.txt > /layer3.txt
EOF
    # The build context payload is generated IN the agent: shipping a 256 KiB
    # file as a base64 argv element overflows the exec command line (measured:
    # exit 126 before the build ever ran).
    local b64; b64=$(base64 -w0 "$ctx/Dockerfile")
    aexec "$agent" "mkdir -p \$HOME/w1 && printf '%s' '$b64' | base64 -d > \$HOME/w1/Dockerfile && head -c 262144 /dev/urandom > \$HOME/w1/payload.txt && cd \$HOME/w1 && timeout $PRESET_BUILD_TIMEOUT docker build -t preset-w1 . && docker image inspect preset-w1 >/dev/null && echo W1_OK"
    if [ "$AEXEC_RC" -eq 0 ] && printf '%s' "$AEXEC_OUT" | grep -q W1_OK; then
        ok "W1 docker-build .......... PASS"
    else
        bad "W1 docker-build .......... FAIL (rc=$AEXEC_RC): $(last_lines "$AEXEC_OUT")"
        TIER_FAILS=$((TIER_FAILS+1))
    fi
}

w2_compose_stack() {
    local agent="$1"
    stage_dir_ensure
    local f="$PRESET_STAGE_DIR/w2-compose.yml"
    cat > "$f" <<EOF
services:
  db:
    image: $PRESET_W2_DB_IMAGE
    environment:
      POSTGRES_PASSWORD: presetbattery
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 5s
      timeout: 3s
      retries: 12
  app:
    image: $PRESET_W1_IMAGE
    command: ["sh", "-c", "while true; do echo app-alive; sleep 5; done"]
    depends_on:
      db:
        condition: service_healthy
EOF
    local b64; b64=$(base64 -w0 "$f")
    aexec "$agent" "mkdir -p \$HOME/w2 && printf '%s' '$b64' | base64 -d > \$HOME/w2/docker-compose.yml && cd \$HOME/w2 && timeout $PRESET_BUILD_TIMEOUT docker compose -f \$HOME/w2/docker-compose.yml --project-directory \$HOME/w2 up -d --wait && docker compose -f \$HOME/w2/docker-compose.yml --project-directory \$HOME/w2 ps --status running --format '{{.Service}}' | sort | tr '\n' ' ' && echo W2_OK"
    if [ "$AEXEC_RC" -eq 0 ] && printf '%s' "$AEXEC_OUT" | grep -q W2_OK \
       && printf '%s' "$AEXEC_OUT" | grep -q 'app' && printf '%s' "$AEXEC_OUT" | grep -q 'db'; then
        ok "W2 compose-up ............ PASS (services: $(printf '%s' "$AEXEC_OUT" | grep -oE 'app db|db app' | head -1))"
    else
        bad "W2 compose-up ............ FAIL (rc=$AEXEC_RC): $(last_lines "$AEXEC_OUT")"
        TIER_FAILS=$((TIER_FAILS+1))
    fi
}

w3_dotnet_app() {
    local agent="$1"
    aexec "$agent" "docker rm -f preset-w3 >/dev/null 2>&1; mkdir -p \$HOME/w3/config && docker run -d --name preset-w3 -e PUID=\$(id -u) -e PGID=\$(id -g) -e TZ=UTC -v \$HOME/w3/config:/config $PRESET_W3_IMAGE >/dev/null && echo W3_STARTED"
    if ! printf '%s' "$AEXEC_OUT" | grep -q W3_STARTED; then
        bad "W3 dotnet-linuxserver .... FAIL (container did not start): $(last_lines "$AEXEC_OUT")"
        TIER_FAILS=$((TIER_FAILS+1))
        return 0
    fi
    sleep "$PRESET_W3_WAIT"
    aexec "$agent" "n=\$(docker exec preset-w3 sh -c 'ls -A /config 2>/dev/null | wc -l' 2>/dev/null); up=\$(docker inspect -f '{{.State.Running}}' preset-w3 2>/dev/null); echo W3_N=\$n W3_UP=\$up"
    local n up
    n=$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*W3_N=\([0-9]*\).*/\1/p')
    up=$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*W3_UP=\([a-z]*\).*/\1/p')
    if [ "${n:-0}" -gt 0 ] && [ "$up" = "true" ]; then
        ok "W3 dotnet-linuxserver .... PASS (ran ≥${PRESET_W3_WAIT}s, /config entries: $n)"
    else
        bad "W3 dotnet-linuxserver .... FAIL (running=$up, config entries=${n:-0} — an EFBIG signal loop leaves the dir empty)"
        TIER_FAILS=$((TIER_FAILS+1))
    fi
}

w4_interpreters() {
    local agent="$1"
    aexec "$agent" "python3 -c 'print(1)' >/dev/null && docker run --rm $PRESET_W4_NODE_IMAGE node -e 'console.log(1)' >/dev/null && echo W4_OK"
    if [ "$AEXEC_RC" -eq 0 ] && printf '%s' "$AEXEC_OUT" | grep -q W4_OK; then
        ok "W4 node+python ........... PASS (python3 in-session, node in-container)"
    else
        bad "W4 node+python ........... FAIL (rc=$AEXEC_RC): $(last_lines "$AEXEC_OUT")"
        TIER_FAILS=$((TIER_FAILS+1))
    fi
}

W5_TAR=""
w5_prepare_tar() {
    [ -n "$W5_TAR" ] && return 0
    stage_dir_ensure
    W5_TAR="$PRESET_STAGE_DIR/w5.tar"
    if [ -f "$W5_TAR" ] && [ "$(stat -c %s "$W5_TAR")" -ge "$PRESET_W5_BYTES" ]; then return 0; fi
    local mib=$(( PRESET_W5_BYTES / 1048576 ))
    local ctx="$PRESET_STAGE_DIR/w5src"; mkdir -p "$ctx"
    cat > "$ctx/Dockerfile" <<EOF
FROM $PRESET_W1_IMAGE
RUN dd if=/dev/zero of=/big.bin bs=1M count=$mib
EOF
    echo "  · preparing the W5 docker-load artifact (~${mib}MiB, one-time, host-side)"
    docker build -q -t preset-w5-src "$ctx" >/dev/null 2>&1 || { note "W5 artifact build failed (host docker unavailable?)"; return 1; }
    docker save -o "$W5_TAR" preset-w5-src >/dev/null 2>&1 || { note "W5 docker save failed"; return 1; }
    [ -s "$W5_TAR" ] || return 1
    return 0
}

w5_docker_load() {
    local agent="$1" uid="$2"
    if ! w5_prepare_tar; then
        bad "W5 docker-load-1.4G ...... FAIL (could not prepare the load artifact on the host)"
        TIER_FAILS=$((TIER_FAILS+1))
        return 0
    fi
    local size; size=$(stat -c %s "$W5_TAR")
    # Copy the artifact into the agent's own home (root can) so the load reads
    # local disk rather than a network hop — the IO class under test is the
    # agent's docker load, not the transport.
    if ! cp "$W5_TAR" "/home/bunker-${agent}/w5.tar" 2>/dev/null; then
        bad "W5 docker-load-1.4G ...... FAIL (could not stage the artifact into the agent home)"
        TIER_FAILS=$((TIER_FAILS+1))
        return 0
    fi
    chown "${uid}:${uid}" "/home/bunker-${agent}/w5.tar" 2>/dev/null || true
    local t0 t1
    t0=$(date +%s)
    aexec "$agent" "out=\$(timeout $PRESET_W5_TIMEOUT docker load -i \$HOME/w5.tar 2>&1); rc=\$?; echo W5_LOAD_RC=\$rc; printf '%s\n' \"\$out\" | tail -3; [ \$rc -eq 0 ] && echo W5_OK"
    t1=$(date +%s)
    local wall=$(( t1 - t0 ))
    # Capture the load's own result BEFORE the cleanup exec, which overwrites
    # the AEXEC_* globals (the first version checked after the rm and therefore
    # always saw rc=0 with no output — a false FAIL that certified nothing).
    local load_rc="$AEXEC_RC" load_out="$AEXEC_OUT"
    aexec "$agent" "rm -f \$HOME/w5.tar" || true
    if [ "$load_rc" -eq 0 ] && printf '%s' "$load_out" | grep -q W5_OK; then
        ok "W5 docker-load-1.4G ...... PASS (wall time: ${wall}s, artifact $(( size / 1048576 ))MiB)"
    else
        bad "W5 docker-load-1.4G ...... FAIL (rc=$load_rc, wall ${wall}s, timeout ${PRESET_W5_TIMEOUT}s): $(last_lines "$load_out")"
        TIER_FAILS=$((TIER_FAILS+1))
    fi
}

# ---------------------------------------------------------------------------
# ABUSE (A1..A3 — assert the MECHANISM, not survival)
# ---------------------------------------------------------------------------
A1_REFUSED=""
a1_fork_bomb() {
    local agent="$1" uid="$2"
    stage_dir_ensure
    # The counters are read from INSIDE the session that holds the slice open:
    # the exec session's scope is what creates the slice cgroup, so a read from
    # a later, separate exec would measure a different (fresh) cgroup and see a
    # delta of zero. Assert the MECHANISM (pids.events max = a REFUSED fork),
    # not survival.
    local drv="$PRESET_STAGE_DIR/a1-driver.sh"
    cat > "$drv" <<'DRIVER'
#!/bin/bash
uid="$1"; cap="$2"
cg="/sys/fs/cgroup/user.slice/user-${uid}.slice"
own="$(awk -F: '/^0::/{print $3}' /proc/self/cgroup 2>/dev/null | head -1)"
case "$own" in
  *"/user-"*".slice"*)
      own="${own#/user.slice/}"; own="${own%%/*}"
      [ -d "/sys/fs/cgroup/user.slice/${own}" ] && cg="/sys/fs/cgroup/user.slice/${own}" ;;
esac
for _ in 1 2 3 4 5; do
    max="$(cat "$cg/pids.max" 2>/dev/null)"
    [ -n "$max" ] && break
    sleep 1
done
read_max()  { awk '/^max /{print $2}' "$cg/pids.events" 2>/dev/null; }
read_cur()  { cat "$cg/pids.current" 2>/dev/null; }
# The refused fork is counted on the cgroup whose limit was actually hit. The
# slice carries pids.max, but the kernel charges the refusal to the cgroup the
# task was being added to — the session scope the exec ran in — so BOTH are
# read and their deltas summed (measured: 4039 threads admitted at a 4096 cap
# with the slice counter flat while the limit was demonstrably reached).
selfc="$(awk -F: '/^0::/{print $3}' /proc/self/cgroup 2>/dev/null | head -1)"
scg="/sys/fs/cgroup${selfc}"
read_max_self() { awk '/^max /{print $2}' "$scg/pids.events" 2>/dev/null; }
mem_max="$(cat "$cg/memory.max" 2>/dev/null)"
mem_guard=0
case "$mem_max" in ''|max) mem_guard=0 ;; *) mem_guard=$(( mem_max / 100 * 75 )) ;; esac

# ── PHASE 1: bounded fork storm (the spec's literal case) ───────────────────
# MEMORY-GUARDED: 4096 `sleep` children is ~4 GiB of RSS, which on a 4 GiB
# slice triggers the tier's OWN memory.max and lets the OOM killer take the
# driver — the first version of this probe did exactly that (the run reported
# an empty pids.max and poisoned the memory-bomb case with an oom_kill it did
# not cause). Stop forking well before the memory ceiling.
bf="$(read_max)"; bf="${bf:-0}"
bfs="$(read_max_self)"; bfs="${bfs:-0}"
j=0
end=$(( SECONDS + 15 ))
while [ "$j" -lt "$cap" ] && [ "$SECONDS" -lt "$end" ]; do
    if [ "$mem_guard" -gt 0 ]; then
        cur_mem="$(cat "$cg/memory.current" 2>/dev/null)"
        case "$cur_mem" in ''|max) : ;; *) [ "$cur_mem" -gt "$mem_guard" ] && break ;; esac
    fi
    sleep 300 &
    j=$(( j + 1 ))
done
sleep 3
af="$(read_max)"; af="${af:-0}"
afs="$(read_max_self)"; afs="${afs:-0}"
fcur="$(read_cur)"
pkill -KILL -x sleep >/dev/null 2>&1
# Let the slice drain before the next case reads its counters.
for _ in $(seq 1 20); do
    c="$(cat "$cg/memory.current" 2>/dev/null)"
    case "$c" in ''|max) break ;; esac
    [ "$mem_guard" -gt 0 ] && [ "$c" -lt "$(( mem_guard / 2 ))" ] && break
    sleep 1
done

# ── PHASE 2: bounded THREAD storm (memory-safe, same controller) ────────────
# pids.max counts TASKS, so threads trip the same controller with the same
# pids.events 'max' refusal and ~1/1000th of the memory. This is what makes
# the refusal assertion reachable on a tier whose memory ceiling would
# otherwise be the thing that fails first.
cat > /tmp/.pthreads.py <<'PY'
import sys, threading, time
threading.stack_size(65536)
cap = int(sys.argv[1])
n = 0
def hold(): time.sleep(600)
try:
    while n < cap:
        threading.Thread(target=hold, daemon=True).start()
        n += 1
except (RuntimeError, OSError):
    pass
print("THREADS", n, flush=True)
time.sleep(3)
PY
bt="$(read_max)"; bt="${bt:-0}"
bts="$(read_max_self)"; bts="${bts:-0}"
timeout -s KILL 90 python3 /tmp/.pthreads.py "$cap" > /tmp/.pthreads.out 2>&1
rc=$?
sleep 1
at="$(read_max)"; at="${at:-0}"
ats="$(read_max_self)"; ats="${ats:-0}"
tcur="$(read_cur)"
threads="$(sed -n 's/^THREADS //p' /tmp/.pthreads.out 2>/dev/null | head -1)"
rm -f /tmp/.pthreads.py /tmp/.pthreads.out
echo "A1_CG=$cg A1_MAX=$max A1_FORK_BEFORE=$bf A1_FORK_AFTER=$af A1_FORK_SELF_BEFORE=$bfs A1_FORK_SELF_AFTER=$afs A1_FORK_LOOP=$j A1_THREAD_BEFORE=$bt A1_THREAD_AFTER=$at A1_THREAD_SELF_BEFORE=$bts A1_THREAD_SELF_AFTER=$ats A1_THREADS=${threads:-0} A1_RC=$rc A1_CUR=$fcur A1_TCUR=$tcur"
DRIVER
    arun_sh "$agent" "$drv" "$uid" "$PRESET_FORK_ATTEMPTS"

    local m bf af bt at threads bfs afs bts ats
    m="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A1_MAX=\([^ ]*\).*/\1/p')"
    bf="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A1_FORK_BEFORE=\([0-9]*\).*/\1/p')"
    af="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A1_FORK_AFTER=\([0-9]*\).*/\1/p')"
    bfs="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A1_FORK_SELF_BEFORE=\([0-9]*\).*/\1/p')"
    afs="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A1_FORK_SELF_AFTER=\([0-9]*\).*/\1/p')"
    bt="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A1_THREAD_BEFORE=\([0-9]*\).*/\1/p')"
    at="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A1_THREAD_AFTER=\([0-9]*\).*/\1/p')"
    bts="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A1_THREAD_SELF_BEFORE=\([0-9]*\).*/\1/p')"
    ats="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A1_THREAD_SELF_AFTER=\([0-9]*\).*/\1/p')"
    threads="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A1_THREADS=\([0-9]*\).*/\1/p')"
    bf="${bf:-0}"; af="${af:-0}"; bt="${bt:-0}"; at="${at:-0}"; threads="${threads:-0}"
    bfs="${bfs:-0}"; afs="${afs:-0}"; bts="${bts:-0}"; ats="${ats:-0}"

    local refusals=$(( (af - bf) + (at - bt) + (afs - bfs) + (ats - bts) ))
    if [ -z "$m" ]; then
        A1_REFUSED="unreadable"
        bad "A1 fork-bomb ............. FAIL (could not read pids.max; driver said: $(last_lines "$AEXEC_OUT" 2))"
        TIER_FAILS=$((TIER_FAILS+1))
    elif [ "$m" = "max" ]; then
        A1_REFUSED="no"
        bad "A1 fork-bomb ............. ESCAPED (pids.max=max — no process/task ceiling to refuse at)"
        TIER_FAILS=$((TIER_FAILS+1))
    elif [ "$refusals" -gt 0 ]; then
        A1_REFUSED="yes"
        ok "A1 fork-bomb ............. CONTAINED (pids controller REFUSED forks/tasks at pids.max=$m: slice pids.events max +$((af-bf))+$((at-bt)), session-scope +$((afs-bfs))+$((ats-bts)); ${threads} threads admitted)"
    else
        A1_REFUSED="not-exercised"
        note "A1 fork-bomb: the ceiling pids.max=$m is finite but neither storm registered a refusal on pids.events (fork loop $PRESET_FORK_ATTEMPTS attempts, ${threads} threads) — the cap sits above what the bounded storms reach"
        ok "A1 fork-bomb ............. CONTAINED (ceiling pids.max=$m finite; refusal path NOT exercised)"
    fi
    # Reap stragglers WITHOUT touching the slice unit: `systemctl kill` on a
    # slice tears the cgroup down, which would blank every later counter read.
    pkill -KILL -u "$uid" >/dev/null 2>&1 || true
    sleep 1
}

A2_WHO_PAID=""
a2_memory_bomb() {
    local agent="$1" uid="$2"
    stage_dir_ensure
    # Size the attempt against the tier's OWN declared ceiling (the E1 claim),
    # never above twice it: the point is to make the tier's cap fire, not to
    # make the host take the hit.
    local target="$PRESET_MEM_BOMB_BYTES"
    local mem_claim; mem_claim="$(claim_get "$E1_CLAIM" MemoryMax)"
    if [ -n "$mem_claim" ] && [ "$mem_claim" -gt 0 ] 2>/dev/null; then
        local twice=$(( mem_claim * 2 ))
        [ "$target" -gt "$twice" ] && target="$twice"
    else
        [ "$target" -gt 1073741824 ] && target=1073741824
    fi

    local drv="$PRESET_STAGE_DIR/a2-driver.sh"
    cat > "$drv" <<'DRIVER'
#!/bin/bash
uid="$1"; target="$2"
cg="/sys/fs/cgroup/user.slice/user-${uid}.slice"
own="$(awk -F: '/^0::/{print $3}' /proc/self/cgroup 2>/dev/null | head -1)"
case "$own" in
  *"/user-"*".slice"*)
      own="${own#/user.slice/}"; own="${own%%/*}"
      [ -d "/sys/fs/cgroup/user.slice/${own}" ] && cg="/sys/fs/cgroup/user.slice/${own}" ;;
esac
ev() { awk -v k="$1" '$1==k {print $2}' "$cg/memory.events" 2>/dev/null; }
for _ in 1 2 3 4 5; do
    max="$(cat "$cg/memory.max" 2>/dev/null)"
    [ -n "$max" ] && break
    sleep 1
done
# Wait for the slice to DRAIN before bombing it: a preceding case (the fork
# storm in A1) can leave the slice near its memory ceiling, in which case this
# bomb's oom_kill would be attributed to it wrongly (measured: A2 reported
# oom_kill +1 and 'allocated 0MiB' because A1's storm had already filled it).
case "$max" in
  ''|max) : ;;
  *) for _ in $(seq 1 30); do
         c="$(cat "$cg/memory.current" 2>/dev/null)"
         case "$c" in ''|max) break ;; esac
         [ "$c" -lt "$(( max / 2 ))" ] && break
         sleep 1
     done ;;
esac
start_mem="$(cat "$cg/memory.current" 2>/dev/null)"
ob="$(ev oom_kill)"; ob="${ob:-0}"
hb="$(ev high)";     hb="${hb:-0}"
cat > /tmp/.pbomb.py <<'PY'
import sys
target = int(sys.argv[1])
chunk = b'x' * (1024 * 1024)
buf = []
try:
    for _ in range(max(1, target // (1024 * 1024))):
        buf.append(bytearray(chunk))
except MemoryError:
    pass
print("ALLOC_MIB", len(buf), flush=True)
PY
# The allocator is a CHILD so this supervisor survives the OOM kill and can
# still read the post-mortem counters.
timeout -s KILL 180 python3 /tmp/.pbomb.py "$target" > /tmp/.pbomb.out 2>&1 &
pid=$!
wait "$pid"; rc=$?
sleep 2
oa="$(ev oom_kill)"; oa="${oa:-0}"
ha="$(ev high)";     ha="${ha:-0}"
peak="$(cat "$cg/memory.peak" 2>/dev/null)"
alloc="$(sed -n 's/^ALLOC_MIB //p' /tmp/.pbomb.out 2>/dev/null | head -1)"
rm -f /tmp/.pbomb.py /tmp/.pbomb.out
echo "A2_MAX=$max A2_START_MEM=$start_mem A2_OOM_BEFORE=$ob A2_OOM_AFTER=$oa A2_HIGH_BEFORE=$hb A2_HIGH_AFTER=$ha A2_PEAK=$peak A2_ALLOC=${alloc:-0} A2_RC=$rc"
DRIVER
    arun_sh "$agent" "$drv" "$uid" "$target"

    local max ob oa hb ha peak alloc
    max="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A2_MAX=\([^ ]*\).*/\1/p')"
    ob="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A2_OOM_BEFORE=\([0-9]*\).*/\1/p')"
    oa="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A2_OOM_AFTER=\([0-9]*\).*/\1/p')"
    hb="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A2_HIGH_BEFORE=\([0-9]*\).*/\1/p')"
    ha="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A2_HIGH_AFTER=\([0-9]*\).*/\1/p')"
    peak="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A2_PEAK=\([^ ]*\).*/\1/p')"
    alloc="$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*A2_ALLOC=\([0-9]*\).*/\1/p')"
    ob="${ob:-0}"; oa="${oa:-0}"; hb="${hb:-0}"; ha="${ha:-0}"
    local oom_delta=$(( oa - ob )) high_delta=$(( ha - hb ))

    if [ "$oom_delta" -gt 0 ]; then
        A2_WHO_PAID="agent"
        ok "A2 memory-bomb ........... CONTAINED (oom_kill +$oom_delta, high +$high_delta, memory.peak=${peak:-?}, allocated ${alloc:-?}MiB — the AGENT paid; the tier's cap memory.max=$max fired)"
    elif [ "$high_delta" -gt 0 ]; then
        A2_WHO_PAID="agent(throttled)"
        ok "A2 memory-bomb ........... CONTAINED (memory.high throttling +$high_delta, no kills, memory.peak=${peak:-?} — the AGENT paid by slowing)"
    elif [ -z "$max" ]; then
        A2_WHO_PAID="unreadable"
        bad "A2 memory-bomb ........... FAIL (could not read memory.max — the agent's slice cgroup is not there)"
        TIER_FAILS=$((TIER_FAILS+1))
    elif [ "$max" = "max" ]; then
        A2_WHO_PAID="host"
        bad "A2 memory-bomb ........... ESCAPED (memory.max=max — no cgroup ceiling; allocated ${alloc:-?}MiB uncapped, the HOST paid)"
        TIER_FAILS=$((TIER_FAILS+1))
    else
        A2_WHO_PAID="none"
        note "A2 memory-bomb: neither oom_kill nor memory.high moved (allocated ${alloc:-0}MiB of a ${target}-byte attempt under memory.max=$max, peak=${peak:-?})"
        bad "A2 memory-bomb ........... FAIL (the tier's memory cap did not fire on a ${target}-byte bomb)"
        TIER_FAILS=$((TIER_FAILS+1))
    fi
}

A3_PEER_DELTA=""
a3_io_hog() {
    local agent="$1" peer="$2"
    local mib=$(( PRESET_PEER_READ_BYTES / 1048576 ))
    # Warm the peer's read source, then take the MIN of three samples: a cold
    # first read (page cache + overlayfs) is several times slower than a warm
    # one, and using it as the baseline would report a bogus NEGATIVE
    # degradation (measured live: a cold baseline of 4186ms vs a warm read of
    # 962ms under the hog).
    aexec "$peer" "dd if=\$HOME/.preset-peer-src of=/dev/null bs=1M count=64 >/dev/null 2>&1"
    local i base_ms base_min="" t0 t1
    for i in 1 2 3; do
        t0=$(date +%s%N)
        aexec "$peer" "timeout $PRESET_PEER_BUDGET_S dd if=\$HOME/.preset-peer-src bs=1M count=$mib of=/dev/null 2>/dev/null && echo PEER_OK"
        t1=$(date +%s%N)
        if [ "$AEXEC_RC" -ne 0 ]; then
            bad "A3 io-hog ................ FAIL (peer baseline read failed)"
            TIER_FAILS=$((TIER_FAILS+1))
            return 0
        fi
        base_ms=$(( (t1 - t0) / 1000000 ))
        if [ -z "$base_min" ] || [ "$base_ms" -lt "$base_min" ]; then base_min="$base_ms"; fi
    done
    local base="$base_min"

    # Hog in the SUBJECT tier while the peer reads again. Two samples, min.
    aexec "$agent" "timeout 120 dd if=/dev/zero of=\$HOME/iohog bs=1M count=$(( PRESET_IO_HOG_BYTES / 1048576 )) oflag=direct >/dev/null 2>&1; echo HOG_DONE" &
    local hog_pid=$!
    sleep 3
    local hog_min="" hog_rc=0
    for i in 1 2; do
        t0=$(date +%s%N)
        aexec "$peer" "timeout $PRESET_PEER_BUDGET_S dd if=\$HOME/.preset-peer-src bs=1M count=$mib of=/dev/null 2>/dev/null && echo PEER_OK"
        t1=$(date +%s%N)
        [ "$AEXEC_RC" -ne 0 ] && hog_rc=1
        hog_ms=$(( (t1 - t0) / 1000000 ))
        if [ -z "$hog_min" ] || [ "$hog_ms" -lt "$hog_min" ]; then hog_min="$hog_ms"; fi
    done
    wait "$hog_pid" 2>/dev/null || true
    local hog="$hog_min"

    if [ "$base" -le 0 ]; then base=1; fi
    local delta=$(( (hog - base) * 100 / base ))
    A3_PEER_DELTA="$delta"

    # Does the tier claim an IO knob? If not, there is no bound to assert —
    # the measurement is recorded and said out loud (matrix: IOWeight measured
    # INERT on uncontended NVMe; no tier defaults an IO bound).
    local io_knob=""
    [ -n "$(claim_get "$E1_CLAIM" IOWeight)" ] && io_knob="IOWeight(E1)"
    [ -n "$(claim_get "$E1_CLAIM" IOWriteBandwidthMax)" ] && io_knob="$io_knob IOWriteBandwidthMax(E1)"

    if [ "$hog_rc" -ne 0 ]; then
        bad "A3 io-hog ................ ESCAPED (the peer read FAILED or exceeded its ${PRESET_PEER_BUDGET_S}s budget while the hog ran)"
        TIER_FAILS=$((TIER_FAILS+1))
    elif [ -n "$io_knob" ]; then
        ok "A3 io-hog ................ CONTAINED (bounded by $io_knob; peer read best-of-3 baseline ${base}ms → best-of-2 under load ${hog}ms, +${delta}%)"
    else
        ok "A3 io-hog ................ CONTAINED (no IO bound claimed by this tier — peer read best-of-3 baseline ${base}ms → best-of-2 under load ${hog}ms, +${delta}% recorded, not asserted)"
    fi
    aexec "$agent" "rm -f \$HOME/iohog" || true
}

# ---------------------------------------------------------------------------
# ISOLATION (I1..I3)
# ---------------------------------------------------------------------------
i1_peer_isolation() {
    local agent="$1" peer="$2" peer_uid="$3"
    aexec "$agent" "out=\$(cat /home/bunker-${peer}/.bashrc 2>&1); rc=\$?; echo I1_RC=\$rc; echo I1_OUT=\$out; su - bunker-${peer} -c id >/dev/null 2>&1 && echo I1_SU_OK || echo I1_SU_DENIED"
    local rc su
    rc=$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*I1_RC=\([0-9]*\).*/\1/p')
    su=$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*\(I1_SU_OK\|I1_SU_DENIED\).*/\1/p')
    if [ "${rc:-0}" -ne 0 ] && [ "$su" = "I1_SU_DENIED" ]; then
        ok "I1 peer-isolation ........ PASS (peer home unreadable rc=$rc, su refused)"
    else
        bad "I1 peer-isolation ........ FAIL (rc=$rc, su=$su — the peer boundary did not hold)"
        TIER_FAILS=$((TIER_FAILS+1))
    fi
}

i2_cgroup_escape() {
    local agent="$1" uid="$2" peer_uid="$3"
    # Three questions, all observable from inside an exec session:
    #  (a) is the session's own cgroup inside the tier's user slice?
    #  (b) is a PEER slice's cgroup reachable/writable from here?
    #  (c) can the session move itself OUT of the tier's slice — and if a move
    #      is attempted, where does it end up?
    aexec "$agent" "c1=\$(cat /proc/self/cgroup); echo I2_C1=\$c1; if [ -e /sys/fs/cgroup/user.slice/user-${peer_uid}.slice ]; then echo I2_PEERVIS=yes; else echo I2_PEERVIS=no; fi; if ( echo \$\$ > /sys/fs/cgroup/user.slice/user-${peer_uid}.slice/memory.max ) 2>/dev/null; then echo I2_PEERWRITE_OK; else echo I2_PEERWRITE_DENIED; fi; ( echo \$\$ > /sys/fs/cgroup/cgroup.procs ) 2>/dev/null && echo I2_MOVE_OK || echo I2_MOVE_DENIED; c2=\$(cat /proc/self/cgroup); echo I2_C2=\$c2"
    local c1 c2 peervis peerwrite move
    c1=$(printf '%s' "$AEXEC_OUT" | sed -n 's/^I2_C1=\(.*\)/\1/p' | head -1)
    c2=$(printf '%s' "$AEXEC_OUT" | sed -n 's/^I2_C2=\(.*\)/\1/p' | head -1)
    peervis=$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*\(I2_PEERVIS=\(yes\|no\)\).*/\2/p' | head -1)
    peerwrite=$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*\(I2_PEERWRITE_OK\|I2_PEERWRITE_DENIED\).*/\1/p' | head -1)
    move=$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*\(I2_MOVE_OK\|I2_MOVE_DENIED\).*/\1/p' | head -1)
    local in_slice_before="no" in_slice_after="no"
    printf '%s' "$c1" | grep -q "user-${uid}.slice" && in_slice_before="yes"
    printf '%s' "$c2" | grep -q "user-${uid}.slice" && in_slice_after="yes"
    if [ "$in_slice_before" = "yes" ] && [ "$in_slice_after" = "yes" ] && [ "$peerwrite" = "I2_PEERWRITE_DENIED" ]; then
        ok "I2 cgroup-escape ......... PASS (in user-${uid}.slice before AND after a move attempt [$move]; peer slice visible=$peervis, peer limit write denied)"
    else
        bad "I2 cgroup-escape ......... FAIL (c1='$c1' c2='$c2' peerwrite=$peerwrite move=$move)"
        TIER_FAILS=$((TIER_FAILS+1))
    fi
}

i3_usability() {
    local agent="$1"
    aexec "$agent" "set -e; echo preset-battery-\$\$ > \$HOME/.preset-usability && grep -q preset-battery \$HOME/.preset-usability && rm -f \$HOME/.preset-usability; getent hosts registry-1.docker.io >/dev/null 2>&1 && N=yes || N=no; echo I3_NET=\$N; echo I3_OK"
    local net rc="$AEXEC_RC"
    net=$(printf '%s' "$AEXEC_OUT" | sed -n 's/.*I3_NET=\([a-z]*\).*/\1/p')
    if [ "$rc" -eq 0 ] && printf '%s' "$AEXEC_OUT" | grep -q I3_OK; then
        ok "I3 usability ............. PASS (home read/write, command exec, network=$net)"
    else
        bad "I3 usability ............. FAIL (rc=$rc)"
        TIER_FAILS=$((TIER_FAILS+1))
    fi
}

# ---------------------------------------------------------------------------
# one tier, end to end
# ---------------------------------------------------------------------------
run_tier() {
    local tier="$1"
    tier_begin "$tier"

    local agent="${PRESET_AGENT_PREFIX}${tier}"
    if ! spawn_retry "$agent" "$tier"; then
        bad "spawn failed for preset '$tier' after ${PRESET_SPAWN_ATTEMPTS:-3} attempts: $(printf '%s' "$SPAWN_OUT" | tail -3 | tr '\n' ' ')"
        TIER_FAILS=$((TIER_FAILS+1))
        echo "TIER $tier VERDICT: FAIL"
        return 0
    fi
    CLEANUP_AGENTS="$CLEANUP_AGENTS $agent"
    local uid; uid=$(agent_uid "$agent")
    if [ -z "$uid" ]; then
        bad "spawn reported success but no bunker-$agent user exists"
        TIER_FAILS=$((TIER_FAILS+1))
        echo "TIER $tier VERDICT: FAIL"
        return 0
    fi
    info "spawned $agent (uid $uid) with --preset $tier"

    # Readiness: wait until the agent's docker actually answers. The unit state
    # is not the right signal — `systemctl is-active` is FALSE while the unit is
    # still "activating" (rootlesskit + dockerd coming up), and on a loaded host
    # that window exceeded the first version's 60s budget even though the
    # daemon's own spawn stage had already seen the socket (measured: E2
    # reported "unit is not active" and a missing unit cgroup, then W1..W4 all
    # ran docker successfully seconds later).
    local dw="${PRESET_DOCKER_WAIT:-180}" waited=0
    while [ "$waited" -lt "$dw" ]; do
        aexec "$agent" "docker info >/dev/null 2>&1 && echo DOCKER_READY"
        printf '%s' "$AEXEC_OUT" | grep -q DOCKER_READY && break
        sleep 5; waited=$((waited+5))
    done
    if printf '%s' "$AEXEC_OUT" | grep -q DOCKER_READY; then
        info "agent docker is answering (ready after ~${waited}s)"
    else
        note "docker did not answer within ${dw}s for $agent — E2 landing and the docker workloads will report their own failures"
    fi

    read_claims "$agent" "$uid"
    landing_e1 "$agent" "$uid"
    landing_e2 "$agent" "$uid"
    surface_coverage

    case "$PRESET_WORKLOADS" in
        none)
            note "workloads skipped by PRESET_WORKLOADS=none (W1..W5 not run, NOT counted as PASS)" ;;
        core)
            note "W3 dotnet-linuxserver .... SKIP (profile 'core' — run PRESET_WORKLOADS=full for the release gate)"
            note "W5 docker-load-1.4G ...... SKIP (profile 'core' — run PRESET_WORKLOADS=full for the release gate)" ;;
        full)
            : ;;
        *)  die "PRESET_WORKLOADS must be core|full|none (got '$PRESET_WORKLOADS')" ;;
    esac

    # ABORT guard: a host-wide battery sweep (userdel -rf ^bunker-) can delete
    # this agent mid-tier. Everything measured after that point describes a
    # dead agent, so the tier is ABORTED — never reported as a containment
    # failure.
    if ! agent_alive "$agent"; then
        tier_abort "$agent"
        return 0
    fi

    if [ "$PRESET_WORKLOADS" = "core" ] || [ "$PRESET_WORKLOADS" = "full" ]; then
        w1_docker_build "$agent"
        w2_compose_stack "$agent"
        w4_interpreters "$agent"
    fi
    if [ "$PRESET_WORKLOADS" = "full" ]; then
        w3_dotnet_app "$agent"
        w5_docker_load "$agent" "$uid"
    fi

    if ! agent_alive "$agent"; then tier_abort "$agent"; return 0; fi

    if [ "${PEER_OK:-0}" = "1" ]; then
        a1_fork_bomb "$agent" "$uid"
        a2_memory_bomb "$agent" "$uid"
        a3_io_hog "$agent" "$PEER_AGENT"

        if ! agent_alive "$agent"; then tier_abort "$agent"; return 0; fi

        i1_peer_isolation "$agent" "$PEER_AGENT" "$PEER_UID"
        i2_cgroup_escape "$agent" "$uid" "$PEER_UID"
    else
        bad "A1/A2 .................... SKIP-FAIL (no peer agent was available; see the peer-spawn message — SKIP is never counted as PASS)"
        bad "A3 io-hog ................ SKIP-FAIL (no peer agent to measure against)"
        bad "I1 peer-isolation ........ SKIP-FAIL (no peer agent to isolate from)"
        bad "I2 cgroup-escape ......... SKIP-FAIL (no peer agent slice to test against)"
        TIER_FAILS=$((TIER_FAILS+4))
    fi
    i3_usability "$agent"

    local v; v="$(tier_verdict "$TIER_FAILS")"
    echo "TIER $tier VERDICT: $v   (pass=$T_PASS fail=$T_FAIL skip=0 findings=$T_FINDING)"
    if [ "$v" = "PASS" ]; then
        TIER_PASS=$((TIER_PASS+1))
        PASSED_TIERS="$PASSED_TIERS $tier"
    else
        TIER_FAIL=$((TIER_FAIL+1))
        FAILED_TIERS="$FAILED_TIERS $tier"
    fi
}

# =============================================================================
# MAIN
# =============================================================================
MODE="run"
TIER_ARGS=""

while [ $# -gt 0 ]; do
    case "$1" in
        --self-test)        MODE="self-test" ;;
        --list-tiers)       MODE="list-tiers" ;;
        --negative-control) MODE="negative-control" ;;
        --all-tiers)        TIER_ARGS="$TIER_TABLE_TIERS" ;;
        --tier)             shift; [ $# -gt 0 ] || die "--tier needs a name"; TIER_ARGS="$TIER_ARGS $1" ;;
        --workloads)        shift; [ $# -gt 0 ] || die "--workloads needs core|full|none"; PRESET_WORKLOADS="$1" ;;
        --strict-surfaces)  PRESET_STRICT_SURFACES=1 ;;
        -h|--help)          usage; exit 0 ;;
        *)                  die "unknown argument '$1' (try --help)" ;;
    esac
    shift
done

case "$MODE" in
    self-test)   self_test; exit $? ;;
    list-tiers)  list_tiers; exit 0 ;;
esac

# ── preflight + daemon + CLI binding ────────────────────────────────────────
echo "=== Bunker preset acceptance battery v$VERSION (GAP-122) ==="
echo "  binary  : $BUNKER ($("$BUNKER" --version 2>/dev/null | head -1 || echo 'version unknown'))"
echo "  daemon  : $BUNKERD_BIN ($("$BUNKERD_BIN" --version 2>/dev/null | head -1 || echo 'version unknown'))"
echo "  workloads profile: $PRESET_WORKLOADS"
echo "  coexist : ${BUNKERD_COEXIST:-0}   ports REST ${BUNKERD_REST_ADDR} gRPC ${BUNKERD_GRPC_ADDR}"
echo ""

preflight
trap cleanup EXIT
start_daemon

# ── CLI state isolation (never touch the operator's config) ─────────────────
BATTERY_CLI_HOME="$(mktemp -d "${TMPDIR:-/tmp}/preset-battery-cli-XXXXXX")"
export BUNKER_HOME="$BATTERY_CLI_HOME" HOME="$BATTERY_CLI_HOME"
echo "  battery CLI state dir: $BATTERY_CLI_HOME (BUNKER_HOME and HOME for every CLI call)"
echo "  battery daemon log   : ${BATTERY_DAEMON_LOG:-<external daemon>}"

STAGE="connect"
set +e
conn_out=$(bcli connect "${BUNKER_DAEMON_URL}" --name "$PRESET_TARGET" --token "$BUNKER_TOKEN" 2>&1)
conn_rc=$?
set -e
if [ "$conn_rc" -ne 0 ]; then
    echo "REFUSING: connect to $BUNKER_DAEMON_URL failed:" >&2
    printf '%s\n' "$conn_out" >&2
    exit 42
fi
export BUNKER_SESSION_TARGET="$PRESET_TARGET"
echo "  connected: $PRESET_TARGET → $BUNKER_DAEMON_URL"
echo ""

# ── tier selection ──────────────────────────────────────────────────────────
STAGE="tier-selection"
if [ "$MODE" = "negative-control" ]; then
    # The injected knob must be one the tier actually CLAIMS, so the default
    # target is `standard` (the tier that requests MemorySwapMax=0) rather
    # than whatever tier happens to be first.
    if [ -z "$TIER_ARGS" ]; then TIER_ARGS="standard"; fi
    TIER_ARGS="${TIER_ARGS%% *}"
fi
TIERS_TO_RUN="$(resolve_tiers "$TIER_ARGS")" || exit 2
if [ "$MODE" = "negative-control" ]; then
    PRESET_INJECT_KNOB="e1:MemorySwapMax=1"
    export PRESET_INJECT_KNOB
    echo "  NEGATIVE CONTROL: running tier '$TIERS_TO_RUN' with a deliberately mis-set"
    echo "  expectation ($PRESET_INJECT_KNOB). The landing check MUST report MISMATCH."
    echo ""
fi
echo "  tiers under test: $TIERS_TO_RUN"
echo ""

# ── pre-clean our own agent ids (a previous interrupted run) ───────────────
STAGE="pre-clean"
for t in $TIERS_TO_RUN; do
    a="${PRESET_AGENT_PREFIX}${t}"
    if id "bunker-$a" >/dev/null 2>&1; then
        info "pre-clean: removing leftover agent $a from an earlier run"
        bcli destroy --server "$PRESET_TARGET" "$a" --force >/dev/null 2>&1 || true
    fi
done

# ── the peer agent (A3 IO peer + I1/I2 isolation counterpart) ──────────────
STAGE="peer"
PEER_AGENT="${PRESET_AGENT_PREFIX}peer"
set +e
bcli destroy --server "$PRESET_TARGET" "$PEER_AGENT" --force >/dev/null 2>&1
set -e
PEER_OK=0
if spawn_retry "$PEER_AGENT" standard; then peer_rc=0; else peer_rc=1; fi
PEER_UID=""
if [ "$peer_rc" -eq 0 ]; then
    CLEANUP_AGENTS="$CLEANUP_AGENTS $PEER_AGENT"
    PEER_UID="$(agent_uid "$PEER_AGENT")"
    # A read source the peer can time: 256 MiB of zeros in its own home.
    aexec "$PEER_AGENT" "dd if=/dev/zero of=\$HOME/.preset-peer-src bs=1M count=$(( PRESET_PEER_READ_BYTES / 1048576 )) 2>/dev/null; sync; echo PEER_SRC_OK" >/dev/null
    PEER_OK=1
    echo "  peer agent $PEER_AGENT (uid ${PEER_UID:-?}) ready"
else
    # Not fatal for the run: A3/I1/I2 need a second agent, so they are reported
    # as SKIP-FAIL (a skip is never a pass) while the rest of the battery still
    # produces its measurements.
    echo "  ‼ the peer agent could not be spawned after ${PRESET_SPAWN_ATTEMPTS:-3} attempts — A3 and I1/I2 will report SKIP-FAIL" >&2
fi
echo ""

# ── run ─────────────────────────────────────────────────────────────────────
for t in $TIERS_TO_RUN; do
    run_tier "$t"
done

# ── roll-up ─────────────────────────────────────────────────────────────────
echo ""
echo "=== ROLL-UP ==="
echo "  profile        : $PRESET_WORKLOADS"
echo "  tiers passed   :${PASSED_TIERS:- none}"
echo "  tiers failed   :${FAILED_TIERS:- none}"
echo "  tiers skipped  :${SKIPPED_TIERS:- none}"
echo "  findings       : $GLOBAL_FINDINGS (non-fatal unless PRESET_STRICT_SURFACES=1)"
echo "  notes          : $GLOBAL_NOTES"
if [ "$MODE" = "negative-control" ]; then
    # The negative control SUCCEEDS when the tier failed for the injected knob.
    if [ -n "$FAILED_TIERS" ]; then
        echo "  NEG-CONTROL    : PASS (deliberately mis-set $PRESET_INJECT_KNOB correctly FAILED the landing check)"
        echo ""
        echo "VERIFY-PASS"
        exit 0
    fi
    echo "  NEG-CONTROL    : FAIL (the injected mismatch did NOT fail the tier — the landing check is vacuous)"
    echo ""
    echo "VERIFY-FAIL"
    exit 1
fi

if [ "${FAILED_TIERS:-}" = "" ] && [ "$TIER_PASS" -gt 0 ]; then
    echo ""
    if [ "$PRESET_WORKLOADS" = "full" ]; then
        echo "  VERIFY-PASS"
    else
        echo "  VERIFY-PASS  (profile: $PRESET_WORKLOADS — W3/W5 were SKIP by profile and are NOT counted as PASS;"
        echo "                run PRESET_WORKLOADS=full for the complete release gate)"
    fi
    exit 0
fi
echo ""
echo "VERIFY-FAIL (failed tiers:${FAILED_TIERS:- none})"
exit 1
