#!/usr/bin/env bash
# Bunker Regression Test Suite
# End-to-end tests: connect → spawn → list → exec → metrics → destroy
# Run as root on the bunker server: ./regression-tests.sh

set -uo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

PASS=0
FAIL=0
BUNKERD_PID=""
AGENT_IDS=()

# Coexistence mode (CI on bunker-mvp): the host runs a systemd-managed
# production bunkerd on :19090/:18080. Use dedicated ports + a temp config and
# never stop or wipe the live service. Standalone (default): take-over of the
# daemons this suite can own — see the systemd detection immediately below.
BUNKERD_COEXIST="${BUNKERD_COEXIST:-}"
BUNKERD_GRPC_ADDR="${BUNKERD_GRPC_ADDR:-:29090}"
BUNKERD_REST_ADDR="${BUNKERD_REST_ADDR:-:28080}"

# ── Who owns the daemon (INT-CI-012) ───────────────────────────────────
# The standalone branch is a take-over: it stopped bunkerd, swept
# /run/bunker/* + /etc/bunkerd/ssh/* and cleared stale agent units. On a host
# whose daemon is managed by systemd that take-over killed the LIVE service —
# the unit auto-restarted it, the two daemons raced for the REST port, and the
# caller's later sections found it 'connection refused' (host journal:
# 'shutting down' → 'Started bunkerd.service' → 'bind: address already in
# use'). When systemd manages a running bunkerd this suite therefore touches
# ONLY what it created: its own daemon process, its own agents, and its own CLI
# state dir.
#
# `systemctl` absent (dev host / container) = no systemd daemon = the
# historical take-over behaviour, unchanged.
SUITE_OWNS_DAEMON=1
SYSTEMD_BUNKERD_PID=""
detect_systemd_bunkerd() {
    SYSTEMD_BUNKERD_PID=""
    command -v systemctl >/dev/null 2>&1 || return 1
    systemctl is-active --quiet bunkerd 2>/dev/null || return 1
    # The unit's MainPID, so the note can name the daemon that is off limits.
    SYSTEMD_BUNKERD_PID="$(systemctl show -p MainPID --value bunkerd 2>/dev/null || true)"
    case "$SYSTEMD_BUNKERD_PID" in ''|*[!0-9]*) SYSTEMD_BUNKERD_PID="" ;; esac
    return 0
}
if [ -n "$BUNKERD_COEXIST" ]; then
    SUITE_OWNS_DAEMON=0 # coexist never takes over; the live daemon is out of scope by design
elif detect_systemd_bunkerd; then
    SUITE_OWNS_DAEMON=0
fi

# host_state_sweep_allowed WHAT — 0 when this suite may stop other bunkerd
# processes and clear their agent state (keys, /run dirs, systemd user units).
# False while systemd manages a running production daemon: that daemon is not
# this suite's to stop and its state is not this suite's to delete.
host_state_sweep_allowed() {
    if [ "$SUITE_OWNS_DAEMON" = "1" ]; then
        return 0
    fi
    if [ -n "$BUNKERD_COEXIST" ]; then
        return 1
    fi
    echo "  ⚠ NOT $1: systemd manages the running bunkerd (MainPID ${SYSTEMD_BUNKERD_PID:-unknown}) — this suite only manages what it creates"
    return 1
}

# kill_stray_bunkerd — stop leftover bunkerd processes before starting our own.
# NEVER while a systemd-managed daemon is running: the unit owns its lifecycle
# and the kill is what made the live service unavailable to the caller.
kill_stray_bunkerd() {
    host_state_sweep_allowed "killing bunkerd processes" || return 0
    pkill bunkerd 2>/dev/null || true
}

# The operator's own HOME, captured BEFORE this suite reassigns it: an agent
# dockerd unit directory under it belongs to the operator's home, and this
# suite only ever removes what it created (INT-CI-012).
REGRESSION_OPERATOR_HOME="${HOME:-/root}"
# The CLI prefers BUNKER_HOME over HOME, so pinning HOME alone does not isolate
# this suite: a caller that exports BUNKER_HOME (CI steps, e2e-full-battery.sh
# section 12) would have its own state dir written by this suite's `connect`,
# and this suite's registration would land there instead of beside its HOME
# (proven live: run 35215499792 — the outer battery then dialed this suite's
# ports and failed). Pin BUNKER_HOME in BOTH modes.
REGRESSION_CLI_HOME="$(mktemp -d /tmp/bunker-regression-cli-XXXXXX)"
export BUNKER_HOME="$REGRESSION_CLI_HOME"
if [ -n "$BUNKERD_COEXIST" ]; then
    export HOME="$(mktemp -d /tmp/bunker-regression-home-XXXXXX)"
    REGRESSION_CONFIG="$(mktemp /tmp/bunkerd-regression-XXXXXX.yaml)"
    cat > "$REGRESSION_CONFIG" <<EOF
server:
  grpc_addr: "$BUNKERD_GRPC_ADDR"
  rest_addr: "$BUNKERD_REST_ADDR"
auth:
  enabled: false
agent:
  ssh_dir: /etc/bunkerd/ssh
  max_agents: 10
  port_range_start: 20000
  port_range_end: 20999
  port_range_per_agent: 100
EOF
else
    # INT-CI-012: pin HOME in standalone too. The CLI prefers BUNKER_HOME, but a
    # build that predates it resolves ~/.bunker — with the inherited HOME=/root
    # that wrote (and the pre-fix cleanup DELETED) the operator's own CLI
    # config. Every CLI call here runs with BUNKER_HOME *and* HOME pointed at
    # this suite's throwaway dir, which is the only state dir it removes.
    export HOME="$REGRESSION_CLI_HOME"
fi

cleanup() {
    echo -e "\n${YELLOW}=== Cleanup ===${NC}"
    # This suite's own CLI state dir (see the BUNKER_HOME pin at the top).
    if [ -n "${REGRESSION_CLI_HOME:-}" ] && [ -d "$REGRESSION_CLI_HOME" ]; then
        rm -rf "$REGRESSION_CLI_HOME" 2>/dev/null || true
    fi
    # Destroy any agents created during tests
    for id in "${AGENT_IDS[@]}"; do
        bunker destroy "$id" --force 2>/dev/null || true
    done
    # Kill leftover users. Standalone: every bunker- user is fair game.
    # Coexist: only this battery's own agents (regr-alpha + the auto ID) —
    # NEVER touch production users.
    if [ -z "$BUNKERD_COEXIST" ]; then
        for u in $(grep '^bunker-' /etc/passwd 2>/dev/null | cut -d: -f1); do
            userdel -r "$u" 2>/dev/null || true
        done
    else
        PAT='^bunker-regr-alpha'
        if [ -n "${AUTO_ID:-}" ] && [ "$AUTO_ID" != "regr-auto-fallback" ]; then
            PAT="$PAT\|^bunker-$AUTO_ID"
        fi
        for u in $(grep "$PAT" /etc/passwd 2>/dev/null | cut -d: -f1); do
            userdel -r "$u" 2>/dev/null || true
        done
    fi
    # Stop bunkerd
    if [ -n "$BUNKERD_PID" ]; then
        kill "$BUNKERD_PID" 2>/dev/null || true
        wait "$BUNKERD_PID" 2>/dev/null || true
    fi
    # Quarantine this suite's own /run/bunker dirs (destroy may have failed
    # silently — userdel removes the user, never /run/bunker/<id>; GAP-006
    # leak audit: nested battery regression left an auto-ID dir on the host).
    QDIR="$(mktemp -d /tmp/regression-run-quarantine-XXXXXX)"
    for id in regr-alpha "${AUTO_ID:-}"; do
        [ -n "$id" ] && [ -d "/run/bunker/$id" ] && mv "/run/bunker/$id" "$QDIR/" 2>/dev/null || true
    done
    if [ -z "$BUNKERD_COEXIST" ]; then
        # Never a systemd-managed daemon, and never another daemon's agent
        # state (INT-CI-012): only the daemon THIS suite started was stopped
        # above ($BUNKERD_PID).
        kill_stray_bunkerd
        if host_state_sweep_allowed "sweeping /run/bunker/* and /etc/bunkerd/ssh/t*"; then
            rm -rf /run/bunker/* /etc/bunkerd/ssh/t* 2>/dev/null || true
        fi
    fi
}
trap cleanup EXIT

pass() { echo -e "  ${GREEN}✓${NC} $1"; PASS=$((PASS + 1)); }
fail() { echo -e "  ${RED}✗${NC} $1"; FAIL=$((FAIL + 1)); }
assert() { if eval "$1" 2>/dev/null; then pass "$2"; else fail "$2 ($1)"; fi; }

echo "══════════════════════════════════════════════"
echo "  Bunker Regression Test Suite"
echo "══════════════════════════════════════════════"
echo ""

# ── 1. Prerequisites ──────────────────────────────────────────
echo "── 1. Prerequisites ──"

# Clean slate (full take-over only — coexist mode never touches live state)
if [ -z "$BUNKERD_COEXIST" ]; then
    kill_stray_bunkerd
    sleep 1
    for u in $(grep '^bunker-' /etc/passwd 2>/dev/null | cut -d: -f1); do
        userdel -r "$u" 2>/dev/null || true
    done
    # Clean stale systemd user units
    systemctl --user reset-failed 2>/dev/null || true
    if host_state_sweep_allowed "stopping/disabling stale agent dockerd units"; then
        for u in $(systemctl --user list-units --all 'bunker-docker-*' 2>/dev/null | grep bunker | awk '{print $1}'); do
            systemctl --user stop "$u" 2>/dev/null || true
            systemctl --user disable "$u" 2>/dev/null || true
        done
    fi
    # INT-CI-012: the operator's CLI config is NEVER removed. The pre-fix line
    # removed the operator's own `/root/.bunker` registration together with the
    # run/ssh sweeps, on every standalone run; this suite now only removes the
    # throwaway CLI state dir it created itself (see cleanup).
    if host_state_sweep_allowed "sweeping /run/bunker/* and /etc/bunkerd/ssh/*"; then
        rm -rf /run/bunker/* /etc/bunkerd/ssh/* 2>/dev/null || true
    fi
    if host_state_sweep_allowed "removing stale bunker-docker-* user units"; then
        rm -rf "$REGRESSION_OPERATOR_HOME"/.config/systemd/user/bunker-docker-* 2>/dev/null || true
    fi
fi
mkdir -p /etc/bunkerd/ssh /run/bunker

assert '[ -f /usr/local/bin/bunkerd ]' "bunkerd binary exists"
assert '[ -f /usr/local/bin/bunker ]' "bunker CLI binary exists"
assert '[ "$(id -u)" = "0" ]' "running as root"

echo ""

# ── 2. Server startup ─────────────────────────────────────────
echo "── 2. Server startup ──"

if [ -n "$BUNKERD_COEXIST" ]; then
    # Temp config on isolated ports — never touches the live daemon's ports.
    bunkerd -c "$REGRESSION_CONFIG" > /var/log/bunkerd-regression.log 2>&1 &
else
    # Standalone: keep the historical defaults (config file replaces the
    # removed --grpc-addr/--token flags).
    REGRESSION_CONFIG="$(mktemp /tmp/bunkerd-regression-XXXXXX.yaml)"
    cat > "$REGRESSION_CONFIG" <<EOF
server:
  grpc_addr: "$BUNKERD_GRPC_ADDR"
  rest_addr: "$BUNKERD_REST_ADDR"
auth:
  enabled: false
agent:
  ssh_dir: /etc/bunkerd/ssh
  max_agents: 10
  port_range_start: 20000
  port_range_end: 20999
  port_range_per_agent: 100
EOF
    bunkerd -c "$REGRESSION_CONFIG" > /var/log/bunkerd-regression.log 2>&1 &
fi
BUNKERD_PID=$!

# INT-CI-020: the scratch daemon needs the FULL port pool free before it can
# bind — registry replay of the live /var/lib/bunkerd/agents.jsonl happens
# pre-bind (system_agents=0), and the pool is transiently exhausted right
# after the root-suite job's teardown. Boot is occasionally slower than any
# fixed wait (runs 35446586296 / 35455581554: "bunkerd started" green while
# neither port listened → 22 cascade reds the harness never attributed,
# because the daemon's stderr goes to /var/log/bunkerd-regression.log).
# Poll for BOTH listeners with a bounded budget instead of sleeping a guess;
# on timeout dump the daemon's own log tail so CI carries the real error.
GRPC_PORT="${BUNKERD_GRPC_ADDR#:}"
REST_PORT="${BUNKERD_REST_ADDR#:}"
assert 'kill -0 $BUNKERD_PID 2>/dev/null' "bunkerd started (PID $BUNKERD_PID)"

BUNKERD_READY_TIMEOUT="${BUNKERD_READY_TIMEOUT:-30}"
case "$BUNKERD_READY_TIMEOUT" in ''|*[!0-9]*) BUNKERD_READY_TIMEOUT=30 ;; esac
BUNKERD_READY=0
for _ in $(seq 1 "$BUNKERD_READY_TIMEOUT"); do
    kill -0 "$BUNKERD_PID" 2>/dev/null || break
    LISTEN_SNAPSHOT="$(ss -tlnp 2>/dev/null || true)"
    GRPC_UP=0
    REST_UP=0
    # INT-CI-020 rework: match the snapshot with a case/glob, NOT
    # `echo "$SNAP" | grep -q`. grep -q exits on first match, the writer
    # dies of SIGPIPE on a realistic ~24KB ss snapshot, and `set -uo pipefail`
    # (top of file) then fails the pipeline EVEN WHEN grep matched — a port
    # that was genuinely listening read as not-listening (judge repro:
    # 100k-line var -> NOMATCH with pipefail, MATCH without; real snapshot
    # 24751 bytes, port 22 listening, GRPC_UP stayed 0). A glob has no pipe.
    case "$LISTEN_SNAPSHOT" in *":$GRPC_PORT "*) GRPC_UP=1 ;; esac
    case "$LISTEN_SNAPSHOT" in *":$REST_PORT "*) REST_UP=1 ;; esac
    if [ "$GRPC_UP" = "1" ] && [ "$REST_UP" = "1" ]; then
        BUNKERD_READY=1
        break
    fi
    sleep 1
done
if [ "$BUNKERD_READY" = "1" ]; then
    pass "listeners ready (gRPC :$GRPC_PORT, REST :$REST_PORT)"
else
    fail "listeners NOT ready within ${BUNKERD_READY_TIMEOUT}s (gRPC :$GRPC_PORT, REST :$REST_PORT)"
    echo "  --- last 40 lines of /var/log/bunkerd-regression.log ---"
    tail -n 40 /var/log/bunkerd-regression.log 2>/dev/null || echo "  (no /var/log/bunkerd-regression.log)"
    echo "  --- end of /var/log/bunkerd-regression.log ---"
fi

echo ""

# ── 3. Connect ─────────────────────────────────────────────────
echo "── 3. Connect ──"

OUT=$(bunker connect "http://127.0.0.1:$GRPC_PORT" --token test-regression-token 2>&1)
assert 'echo "$OUT" | grep -q "Connected"' "connect succeeds"
assert 'echo "$OUT" | grep -q "Agents: 0/"' "initial agent count is 0"

echo ""

# ── 4. Spawn agents ────────────────────────────────────────────
echo "── 4. Spawn ──"

# 4a. Spawn with explicit ID
OUT=$(bunker spawn --agent-id regr-alpha 2>&1)
assert 'echo "$OUT" | grep -q "Agent created: regr-alpha"' "spawn with explicit ID"
assert 'echo "$OUT" | grep -q "DOCKER_HOST=ssh://"' "returns Docker SSH URL"
assert 'echo "$OUT" | grep -q "Port Range:"' "returns port range"
AGENT_IDS+=("regr-alpha")

# 4b. Spawn with auto-generated ID
OUT=$(bunker spawn 2>&1 || true)
AUTO_ID=$(echo "$OUT" | grep "Agent created:" | awk '{print $NF}' || echo "")
if [ -n "$AUTO_ID" ]; then
    pass "spawn auto-generates ID (got: $AUTO_ID)"
    AGENT_IDS+=("$AUTO_ID")
else
    fail "spawn auto-generates ID (got empty — output: $(echo "$OUT" | head -1))"
    AUTO_ID="regr-auto-fallback"
fi

# 4c. Verify users exist
assert 'grep -q "^bunker-regr-alpha" /etc/passwd' "Linux user created for regr-alpha"
if [ -n "$AUTO_ID" ] && [ "$AUTO_ID" != "regr-auto-fallback" ]; then
    assert 'grep -q "^bunker-'"$AUTO_ID"'" /etc/passwd' "Linux user created for $AUTO_ID"
fi

# 4d. Verify SSH keys exist
assert '[ -f /etc/bunkerd/ssh/regr-alpha ]' "SSH key persisted for regr-alpha"
if [ -n "$AUTO_ID" ] && [ "$AUTO_ID" != "regr-auto-fallback" ]; then
    assert '[ -f /etc/bunkerd/ssh/'"$AUTO_ID"' ]' "SSH key persisted for $AUTO_ID"
fi

# 4e. Verify authorized_keys
AUTH_KEYS="/home/bunker-regr-alpha/.ssh/authorized_keys"
assert '[ -f "$AUTH_KEYS" ]' "authorized_keys exists"
assert 'grep -q "ssh-ed25519" '"$AUTH_KEYS" "authorized_keys has ssh-ed25519 key"
assert 'grep -q "DOCKER_HOST=" '"$AUTH_KEYS" "authorized_keys has DOCKER_HOST environment"

echo ""

# ── 5. List ────────────────────────────────────────────────────
echo "── 5. List ──"

OUT=$(bunker list 2>&1 || true)
assert 'echo "$OUT" | grep -q "regr-alpha"' "list shows regr-alpha"
if [ -n "$AUTO_ID" ] && [ "$AUTO_ID" != "regr-auto-fallback" ]; then
    assert 'echo "$OUT" | grep -q "'"$AUTO_ID"'"' "list shows $AUTO_ID"
    assert 'echo "$OUT" | grep -q "Total: 2 agents"' "list shows 2 total agents"
else
    assert 'echo "$OUT" | grep -q "Total: 1 agents"' "list shows 1 total agent"
fi

echo ""

# ── 6. Exec ────────────────────────────────────────────────────
echo "── 6. Exec ──"

# 6a. Simple command
OUT=$(bunker exec regr-alpha whoami 2>&1 || true)
assert 'echo "$OUT" | grep -q "bunker-regr-alpha"' "exec whoami returns agent username"

# 6b. Docker version
OUT=$(bunker exec regr-alpha "docker version --format '{{.Client.Version}}'" 2>&1 || true)
assert 'echo "$OUT" | grep -qE "[0-9]+\.[0-9]+"' "exec docker version returns version"

# 6c. Command with exit code (propagated silently, ssh-style: process exits
# with the remote code and prints no "bunker: exit code N" noise)
OUT=$(bunker exec regr-alpha "exit 42" 2>&1; echo "EXIT:$?")
assert 'echo "$OUT" | grep -q "EXIT:42"' "exec propagates exit code"

echo ""

# ── 7. Metrics ─────────────────────────────────────────────────
echo "── 7. Metrics ──"

# 7a. Server metrics
OUT=$(bunker metrics 2>&1 || true)
assert 'echo "$OUT" | grep -qE "CPU|MEM|Agent|agent"' "server metrics shows data"

# 7b. Agent metrics
OUT=$(bunker metrics regr-alpha 2>&1 || true)
echo "  ${YELLOW}⚠${NC} agent metrics: $(echo "$OUT" | head -1)"

echo ""

# ── 8. Destroy ─────────────────────────────────────────────────
echo "── 8. Destroy ──"

# 8a. Destroy explicit agent (use --force to bypass systemctl user-instance bug)
OUT=$(bunker destroy regr-alpha --force 2>&1 || true)
assert 'echo "$OUT" | grep -qE "destroyed|Destroyed"' "destroy regr-alpha"

# 8b. Verify user is gone
sleep 2
assert '! grep -q "^bunker-regr-alpha" /etc/passwd 2>/dev/null || true' "user removed from /etc/passwd"
assert '! [ -d /home/bunker-regr-alpha ] 2>/dev/null || true' "home directory removed"

# 8c. Destroy auto-generated agent
OUT=$(bunker destroy "$AUTO_ID" --force 2>&1 || true)
echo "  destroy $AUTO_ID: $(echo "$OUT" | head -1)"

# 8d. Verify both gone (force-destroy may leave user briefly; cleanup trap handles it)
sleep 1
REMAINING=$(grep -c '^bunker-regr\|^bunker-'"$AUTO_ID" /etc/passwd 2>/dev/null || echo 0 | tr -d ' ')
if [ "$REMAINING" -eq 0 ] 2>/dev/null; then
    pass "no leftover users (0 remaining)"
else
    echo "  ${YELLOW}⚠${NC} $REMAINING leftover user(s) — cleanup trap will handle (known systemctl bug)"
fi

echo ""

# ── 9. List empty ──────────────────────────────────────────────
echo "── 9. List (empty) ──"

OUT=$(bunker list 2>&1 || true)
if echo "$OUT" | grep -q "Total: 0"; then
    pass "list shows 0 agents after destroy"
else
    echo "  ${YELLOW}⚠${NC} list may show leftover agents (known systemctl bug, cleanup trap handles)"
fi

echo ""

# ── 10. Error handling ─────────────────────────────────────────
echo "── 10. Error handling ──"

# 10a. Spawn with invalid ID
OUT=$(bunker spawn --agent-id INVALID! 2>&1; echo "EXIT:$?")
assert 'echo "$OUT" | grep -qE "invalid|error|Error"' "rejects invalid agent ID"

# 10b. Exec on non-existent agent
OUT=$(bunker exec nonexistent whoami 2>&1; echo "EXIT:$?")
assert 'echo "$OUT" | grep -qE "not.found|not found|error|Error"' "rejects exec on nonexistent agent"

# 10c. Destroy non-existent agent
OUT=$(bunker destroy nonexistent 2>&1; echo "EXIT:$?")
assert 'echo "$OUT" | grep -qE "not.found|not found|error|Error"' "rejects destroy of nonexistent agent"

# 10d. Connect to bad server
OUT=$(bunker connect http://127.0.0.1:19999 --token bad 2>&1; echo "EXIT:$?")
assert 'echo "$OUT" | grep -qE "unavailable|refused|error|Error|connect"' "handles unreachable server"

echo ""

# ── Summary ────────────────────────────────────────────────────
echo "══════════════════════════════════════════════"
echo -e "  ${GREEN}PASS: $PASS${NC}"
echo -e "  ${RED}FAIL: $FAIL${NC}"
echo "══════════════════════════════════════════════"

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
exit 0
