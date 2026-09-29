#!/usr/bin/env bash
# REV-BUNKER-005/006 — independent LIVE-daemon probe, run by a second worker.
#
# Boots the REAL bunkerd binary built from the tree under test (scratch dirs,
# loopback port, generated token) and measures, over HTTP, the two things the
# row is about:
#
#   005  what an UNAUTHENTICATED client gets from /graph/stats|related|impact,
#        and what an authenticated one gets (the acceptance pair).
#   006  whether repeated failed authentications are THROTTLED when
#        audit.enabled is false (the row's exact configuration).
#
# It is a black-box probe on purpose: it cannot see the fix, only the wire.
#
# usage: sh sec-trio-probe.sh <repo-dir> [outfile]
# exit : 0 = every cell as expected for a FIXED tree; 1 = at least one cell is
#        the FILED (defective) behaviour; 2 = infrastructure error.
#
# SAFETY: nothing outside a mktemp -d scratch dir is written; the daemon is
# killed by PID with a bounded wait (never pkill -f); no repository file is
# touched; the token is generated at run time and never written into the repo.

set -u

REPO="${1:?usage: sh sec-trio-probe.sh <repo-dir> [outfile]}"
OUT="${2:-/dev/stdout}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/sec-trio-probe.XXXXXX")"
BIN="$WORK/bunkerd"
RUN="$WORK/run"
DATA="$WORK/data"
TOKEN="probe-$(date +%s)-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
DAEMON_A=""
DAEMON_B=""
FAILED=0

say() { printf '%s\n' "$*" | tee -a "$OUT"; }

stop_proc() {
	pid="$1"
	[ -n "$pid" ] || return 0
	kill -0 "$pid" 2>/dev/null || return 0
	kill "$pid" 2>/dev/null
	i=0
	while [ "$i" -lt 60 ]; do
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.25
		i=$((i + 1))
	done
	if kill -0 "$pid" 2>/dev/null; then
		kill -KILL "$pid" 2>/dev/null
		sleep 0.3
	fi
	wait "$pid" 2>/dev/null || true
}

cleanup() {
	stop_proc "$DAEMON_B"
	stop_proc "$DAEMON_A"
	# The brief for this row forbids rm -rf in this fleet (and the harness
	# refuses it in unattended mode). The scratch dir is under mktemp -d and
	# its path is printed, so it is inspectable after the run instead of being
	# silently destroyed.
	if [ "${KEEP_SCRATCH:-1}" = "0" ]; then
		: >"$WORK/.scratch-kept"
	fi
}
trap cleanup EXIT

# pick_port prints a free loopback port.
#
# It must NOT use $RANDOM as its only source: every $( ) is a subshell that
# inherits the SAME RANDOM state, so two calls returned the same "random" port
# (observed: both arms bound :20000, which silently made the audit-off
# assertion read the audit-on daemon's log file). The candidate window comes
# from /dev/urandom and then walks upward, so consecutive calls land on
# different ports.
pick_port() {
	start=$(( $(od -An -N2 -tu2 </dev/urandom | tr -d ' ') % 15000 + 20000 ))
	i=0
	while [ "$i" -lt 200 ]; do
		c=$(( start + i ))
		if ! (exec 3<>"/dev/tcp/127.0.0.1/$c") 2>/dev/null; then
			echo "$c"
			return 0
		fi
		i=$((i + 1))
	done
	return 1
}

# pick_two_ports sets PORT_A and PORT_B, distinct and both free.
pick_two_ports() {
	PORT_A="$(pick_port)" || return 1
	PORT_B="$(pick_port)" || return 1
	if [ "$PORT_B" = "$PORT_A" ]; then
		PORT_B=$((PORT_A + 1))
		(exec 3<>"/dev/tcp/127.0.0.1/$PORT_B") 2>/dev/null && return 1
	fi
	[ "$PORT_A" = "$PORT_B" ] && return 1
	return 0
}

mkdir -p "$RUN/.vfs/graph" "$DATA"
# A deterministic 3-edge fixture so /graph/stats has a non-zero body to read.
cat >"$RUN/.vfs/graph/edges.jsonl" <<'JSONL'
{"from":"internal/server/server.go","to":"internal/config/config.go","rel":"imports"}
{"from":"internal/server/server.go","to":"pkg:connectrpc.com/connect","rel":"imports"}
{"from":"internal/config/config.go","to":"std:fmt","rel":"imports"}
JSONL

say "REV-BUNKER-005/006 live probe"
say "  date        : $(date -Is)"
say "  tree        : $REPO"
say "  head        : $(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo 'no-git')"
say "  worktree    : $(git -C "$REPO" status --porcelain 2>/dev/null | wc -l) uncommitted path(s)"
say "  scratch     : $WORK"
say

say "--- building bunkerd from the tree under test"
if ! (cd "$REPO" && go build -o "$BIN" ./cmd/bunkerd); then
	say "FATAL: go build ./cmd/bunkerd failed"
	exit 2
fi
say "    bunkerd sha256=$(sha256sum "$BIN" | cut -d' ' -f1)"
say

write_config() { # path port audit_enabled
	cat >"$1" <<YAML
server:
  grpc_addr: "127.0.0.1:$2"
  rest_addr: ""
  request_timeout: 60s
auth:
  enabled: true
  token: "$TOKEN"
audit:
  enabled: $3
  path: "$WORK/audit-$2.log"
agent:
  base_data_dir: "$DATA"
  registry:
    enabled: false
tunnel:
  enabled: false
tailscale:
  enabled: false
YAML
}

start_daemon() { # cfg port logfile -> sets STARTED_PID
	(cd "$RUN" && exec "$BIN" --config "$1") >>"$3" 2>&1 &
	STARTED_PID=$!
	i=0
	while [ "$i" -lt 120 ]; do
		if curl -sS -o /dev/null "http://127.0.0.1:$2/healthz" 2>/dev/null; then
			return 0
		fi
		if ! kill -0 "$STARTED_PID" 2>/dev/null; then
			say "FATAL: daemon exited during startup; log tail:"
			tail -15 "$3" | sed 's/^/    /' | tee -a "$OUT"
			return 1
		fi
		sleep 0.25
		i=$((i + 1))
	done
	say "FATAL: daemon never became healthy on port $2"
	return 1
}

cell() { # label got want
	if [ "$2" = "$3" ]; then
		say "  [ok]   $1: got $2, want $3"
	else
		say "  [CELL] $1: got $2, want $3"
		FAILED=1
	fi
}

# ── arm 005: the graph surface, auth enabled, audit enabled ──────────────────
say "=== ARM 005 — /graph, auth.enabled=true (audit enabled) ==="
pick_two_ports || { say "FATAL: no free port pair"; exit 2; }
CFG_A="$WORK/audit-on.yaml"
write_config "$CFG_A" "$PORT_A" true
if ! start_daemon "$CFG_A" "$PORT_A" "$WORK/daemon-a.log"; then
	exit 2
fi
DAEMON_A="$STARTED_PID"
BASE="http://127.0.0.1:$PORT_A"
say "  daemon a: pid $DAEMON_A on $BASE (audit on)"

code() { curl -s -o "$WORK/body" -w '%{http_code}' "$@" 2>/dev/null; }

c="$(code "$BASE/graph/stats")"
cell "GET /graph/stats, no credentials" "$c" "401"
say "        body: $(head -c 120 "$WORK/body")"

c="$(code -H "Authorization: Bearer wrong-token" "$BASE/graph/stats")"
cell "GET /graph/stats, wrong token" "$c" "401"

c="$(code -H "Authorization: Bearer $TOKEN" "$BASE/graph/stats")"
cell "GET /graph/stats, valid token" "$c" "200"
say "        body: $(head -c 160 "$WORK/body")"

c="$(code -H "Authorization: Bearer $TOKEN" "$BASE/graph/impact?path=internal/server/server.go")"
cell "GET /graph/impact?path=..., valid token" "$c" "200"
c="$(code "$BASE/graph/impact?path=internal/server/server.go")"
cell "GET /graph/impact?path=..., no credentials" "$c" "401"
c="$(code "$BASE/graph/related?path=internal/config/config.go")"
cell "GET /graph/related?path=..., no credentials" "$c" "401"

# Non-vacuity: /healthz is still public, so a 401 above is the gate, not a
# daemon that answers 401 to everything.
c="$(code "$BASE/healthz")"
cell "GET /healthz, no credentials (non-vacuity)" "$c" "200"

# The credential model is the daemon's own: an authenticated RPC still works
# and a BAD credential is still Unauthenticated, so the gate did not leak into
# the RPC surface.
c="$(curl -s -o "$WORK/rpc-auth" -w '%{http_code}' -X POST -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" --data '{}' "$BASE/bunker.v1.Bunkerd/ServerInfo")"
cell "POST /bunker.v1.Bunkerd/ServerInfo, valid token" "$c" "200"
c="$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' --data '{}' "$BASE/bunker.v1.Bunkerd/ServerInfo")"
cell "POST /bunker.v1.Bunkerd/ServerInfo, no credentials" "$c" "401"
stop_proc "$DAEMON_A"
DAEMON_A=""
say

# ── arm 006: the throttle with the audit sink ABSENT ─────────────────────────
say "=== ARM 006 — repeated failed auths with audit.enabled=false ==="
CFG_B="$WORK/audit-off.yaml"
write_config "$CFG_B" "$PORT_B" false
if ! start_daemon "$CFG_B" "$PORT_B" "$WORK/daemon-b.log"; then
	exit 2
fi
DAEMON_B="$STARTED_PID"
BASE="http://127.0.0.1:$PORT_B"
say "  daemon b: pid $DAEMON_B on $BASE (audit OFF — no deny sink attached)"
if grep -q 'audit' "$WORK/daemon-b.log"; then
	say "  daemon b audit-related log lines:"
	grep -i 'audit' "$WORK/daemon-b.log" | head -3 | sed 's/^/    /' | tee -a "$OUT"
fi

# ONE curl invocation, six requests: curl reuses the keep-alive connection, so
# the daemon sees ONE source address — the throttle's unit of accounting. (A
# client that opens a fresh connection per attempt is a separate, real gap;
# named in the evidence, not silently averaged away here.)
#
# `-o /dev/null` is repeated once per URL: curl pairs -o with the next URL, so
# a single -o would leave requests 2..6 printing their bodies into the transcript
# and corrupt the code sequence. `%{local_port}` is printed too, so the
# transcript itself shows the six requests rode ONE connection.
URL="$BASE/bunker.v1.Bunkerd/ServerInfo"
CODES="$(curl -s -o /dev/null -o /dev/null -o /dev/null -o /dev/null -o /dev/null -o /dev/null \
	-w '%{http_code}:%{local_port} ' \
	-X POST -H 'Content-Type: application/json' \
	-H 'Authorization: Bearer wrong-token-value' --data '{}' \
	"$URL" "$URL" "$URL" "$URL" "$URL" "$URL" 2>/dev/null)"
say "  six bad-token requests on ONE connection (http_code:local_port): $CODES"
say "  distinct local ports used: $(printf '%s' "$CODES" | tr ' ' '\n' | grep -c '^[0-9]*:[0-9]*$') requests on $(printf '%s' "$CODES" | grep -o ':[0-9]*' | sort -u | wc -l) connection(s)"

CODES="$(printf '%s' "$CODES" | tr ' ' '\n' | grep '^[0-9]*:' | cut -d: -f1 | tr '\n' ' ')"
say "  http codes in order: $CODES"

SIXTH="$(printf '%s' "$CODES" | awk '{print $6}')"
FIRST="$(printf '%s' "$CODES" | awk '{print $1}')"
cell "1st failed auth is an ordinary denial" "$FIRST" "401"
cell "6th failed auth is THROTTLED (auth on, audit off)" "$SIXTH" "503"
if printf '%s' "$CODES" | grep -q '^401 401 401 401 401 '; then
	say "  [note] the five failures before the throttle were all 401 — the"
	say "         threshold behaviour is intact, only the 6th changes code."
fi

say "=== 006 non-vacuity: a DIFFERENT source is not affected ==="
CODES2="$(curl -s -o /dev/null -w '%{http_code} ' -X POST -H 'Content-Type: application/json' --data '{}' "$URL" 2>/dev/null)"
cell "fresh connection (new source) is not throttled" "$(printf '%s' "$CODES2" | awk '{print $1}')" "401"

say "=== 006 bonus: a valid credential still authenticates on this daemon ==="
c="$(curl -s -o "$WORK/rpc-b" -w '%{http_code}' -X POST -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" --data '{}' "$URL")"
cell "valid token on the audit-off daemon" "$c" "200"

# The audit-off daemon must still write NO audit file (the sink is absent, and
# the throttle is not a reason to start writing one).
if [ -e "$WORK/audit-$PORT_B.log" ]; then
	say "  [CELL] audit.enabled=false but an audit file exists: $(wc -l <"$WORK/audit-$PORT_B.log") line(s)"
	FAILED=1
else
	say "  [ok]   audit.enabled=false wrote no audit file"
fi
stop_proc "$DAEMON_B"
DAEMON_B=""
say

if [ "$FAILED" = "0" ]; then
	say "RESULT: FIXED-BEHAVIOUR — every cell matches the row's acceptance"
	exit 0
fi
say "RESULT: FILED-BEHAVIOUR PRESENT — at least one cell is the defect"
exit 1
