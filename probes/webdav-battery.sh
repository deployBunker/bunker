#!/usr/bin/env bash
# webdav-battery.sh — run the 14-operation battery against ONE live bunkerd
# serving the SAME fixture over HTTP/1.1, HTTP/2 and HTTP/3, at concurrency=1
# AND at concurrency>1, and publish the per-protocol numbers side by side.
#
# WHY (BFS-011): the release rests on one proven lever — CONCURRENT REQUESTS
# (HTTP/2 at 25x concurrency: 0.79 s versus 38.47 s at MaxConnsPerHost=1,
# docs/prd/PRD-bunker-fs.md:60-61). The hypothesis is that the protocol VERSION
# is a carrier and concurrency is the cause. This driver runs every protocol at
# both concurrency levels so that hypothesis is falsifiable: if a protocol does
# NOT degrade at concurrency=1, the report says so as a MAJOR finding and does
# not tune the battery until it produces the answer the thesis predicts.
#
# WHAT IT DOES
#   1. builds bunkerd from this tree and the battery instrument (probes/webdav-battery)
#   2. generates Fixture A (150 files / 6 dirs + a 4 MiB file + a deep chain)
#      deterministically, and regenerates it before EVERY arm so two arms start
#      from byte-identical trees
#   3. starts ONE daemon: TLS + HTTP/3 on the same port number, so http/1.1, h2
#      and h3 are served by one process on one fixture
#   4. optionally starts a delay relay (no host change, no root) so the battery
#      can also be measured with a round trip on the wire — the read the study's
#      185 ms figures came from
#   5. runs the arm matrix, then reports: per-protocol tables, the concurrency
#      ratios, and the AC-11 verdict
#
# SAFETY: everything is scratch — tree, config, certificate, results — under
# $TMPDIR on loopback ports, with a token generated at run time. No repository
# file is written, no host setting is touched, and the delay relay is a
# userspace process that dies with this script.
#
# USAGE:
#   probes/webdav-battery.sh [--binary PATH] [--port N] [--conc N] [--fanout N]
#                            [--links loopback,delay20] [--delay MS] [--out DIR]
#                            [--op-timeout S] [--keep]
#
#   --binary PATH   reuse an existing bunkerd binary (default: build one)
#   --port N        first loopback port to bind (default: a free one)
#   --conc N        the high concurrency arm (default 8)
#   --fanout N      requests in the fan-out arm (default 100)
#   --links LIST    loopback and/or delay<MS> (default "loopback,delay20")
#   --delay MS      delay per direction for the relay link (default 20)
#   --out DIR       results directory (default: a scratch dir; the path is printed)
#   --op-timeout S  per-cell deadline (default 45; a cell past it is a STALL)
#   --keep          keep the scratch tree, the daemon log and the results
#
# EXIT: 0 when every arm passed its cells and its contract notes, 1 when any arm
#       failed, 2 on usage or infrastructure error. A battery that ran correctly
#       and found NO degradation still exits 0 — that outcome is a finding about
#       the release, not a broken test.

set -uo pipefail

BIN=""
PORT=""
CONC=8
FANOUT=100
LINKS="loopback,delay20"
DELAY_MS=20
OUT=""
OP_TIMEOUT=45
KEEP=0
PER_DIR=25
DIRS=6
BIG_MIB=4

while [ $# -gt 0 ]; do
  case "$1" in
    --binary)     BIN="${2:-}"; shift 2 ;;
    --port)       PORT="${2:-}"; shift 2 ;;
    --conc)       CONC="${2:-}"; shift 2 ;;
    --fanout)     FANOUT="${2:-}"; shift 2 ;;
    --links)      LINKS="${2:-}"; shift 2 ;;
    --delay)      DELAY_MS="${2:-}"; shift 2 ;;
    --out)        OUT="${2:-}"; shift 2 ;;
    --op-timeout) OP_TIMEOUT="${2:-}"; shift 2 ;;
    --dirs)       DIRS="${2:-}"; shift 2 ;;
    --per-dir)    PER_DIR="${2:-}"; shift 2 ;;
    --big-mib)    BIG_MIB="${2:-}"; shift 2 ;;
    --keep)       KEEP=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/webdav-battery.XXXXXX")"
ROOT="$WORK/tree"
OUT="${OUT:-$WORK/results}"
mkdir -p "$OUT"

# A results directory holds ONE run. The per-arm and per-op CSVs are appended to
# by design (every arm appends its rows), so re-using a directory from an earlier
# run would put two runs' rows in one table — which the report would then read as
# a single run and compare across. Truncate up front; say so in the transcript.
#
# (Learned the hard way: an interrupted run left three arms of a dead relay in
# arms.csv, and the next run's report listed "17 arms" — including a
# connection-refused arm whose port number belonged to the previous process.)
: > "$OUT/arms.csv"
: > "$OUT/ops.csv"
: > "$OUT/requests.csv"

pick_port() {
  local candidate
  for _ in $(seq 1 60); do
    candidate=$(( (RANDOM % 20000) + 20000 ))
    if ! (exec 3<>"/dev/tcp/127.0.0.1/$candidate") 2>/dev/null; then
      echo "$candidate"
      return 0
    fi
  done
  return 1
}

if [ -z "$PORT" ]; then
  PORT="$(pick_port)" || { echo "no free loopback port found" >&2; exit 2; }
fi
RELAY_PORT="$(pick_port)" || { echo "no free relay port found" >&2; exit 2; }
while [ "$RELAY_PORT" = "$PORT" ]; do RELAY_PORT="$(pick_port)"; done

TOKEN="$(date +%s)-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
DAEMON_PID=""
RELAY_PID=""
DAEMON_LOG="$WORK/bunkerd.log"
RELAY_LOG="$WORK/relay.log"
RAW="$OUT/battery-run.txt"

cleanup() {
  stop_proc "$RELAY_PID" relay
  stop_proc "$DAEMON_PID" bunkerd
  if [ "$KEEP" = "1" ]; then
    echo "scratch kept: $WORK"
    echo "raw output   : $RAW"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

# stop_proc terminates a background process with a BOUNDED wait. An unbounded
# `wait` after a signal hung the first version of this driver past its own
# timeout (the cleanup never returned), so the bound is not politeness: a battery
# that cannot exit reports nothing.
stop_proc() { # pid label
  local pid="$1" label="$2" i
  [ -n "$pid" ] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill "$pid" 2>/dev/null
  for i in $(seq 1 40); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "cleanup: $label (pid $pid) did not exit on SIGTERM; sending SIGKILL"
    kill -KILL "$pid" 2>/dev/null
    sleep 0.2
  fi
  wait "$pid" 2>/dev/null || true
}

# ── build ────────────────────────────────────────────────────────────────────
if [ -z "$BIN" ]; then
  BIN="$WORK/bunkerd"
  echo "building bunkerd from $REPO_ROOT ..."
  if ! (cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/bunkerd); then
    echo "go build ./cmd/bunkerd failed" >&2
    exit 2
  fi
fi
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }

BAT="$WORK/webdav-battery"
echo "building the battery instrument ..."
if ! (cd "$REPO_ROOT" && go build -o "$BAT" ./probes/webdav-battery); then
  echo "go build ./probes/webdav-battery failed" >&2
  exit 2
fi

# ── the run record ───────────────────────────────────────────────────────────
{
  echo "BFS-011 per-protocol WebDAV battery"
  echo "date            : $(date -Is)"
  echo "host            : $(uname -n) $(uname -r) $(uname -m)"
  echo "cpus            : $(nproc)"
  echo "loadavg (start) : $(cut -d' ' -f1-3 /proc/loadavg 2>/dev/null || echo n/a)"
  echo "go              : $(cd "$REPO_ROOT" && go version)"
  echo "bunkerd commit  : $(cd "$REPO_ROOT" && git rev-parse HEAD)"
  echo "bunkerd sha256  : $(sha256sum "$BIN" | awk '{print $1}')"
  echo "fixture         : $DIRS dirs x $PER_DIR files + a ${BIG_MIB} MiB file + a deep chain"
  echo "results dir     : $OUT (fresh: the previous run's rows are not mixed in)"
  echo "daemon          : TLS + HTTP/3 on ONE port number ($PORT), webdav_root=$ROOT"
  echo "links           : $LINKS (delay = ${DELAY_MS}ms per direction on the relay link)"
  echo "concurrency     : 1 and $CONC; fan-out arm = $FANOUT requests"
  echo "op timeout      : ${OP_TIMEOUT}s (a cell past it is reported as a STALL)"
  echo
  echo "RECORDED BASELINES (docs/prd/PRD-bunker-fs.md:23-31, :84-93) — mount-level wall times on a"
  echo ">=180 ms link, recorded before this surface existed. They are the numbers the release is judged"
  echo "against; they are NOT directly comparable to the loopback arms below, which have no link latency,"
  echo "and the report says so where it matters:"
  echo "  sshfs (current default)          5 / 14 ok, 7 stalls (dedi-2) / 8 (bunker-mvp)"
  echo "  NFSv4                           11 / 14 ok, whole-tree ops 33-36 s (native: 0.41 s)"
  echo "  rclone WebDAV (no h2, 4 conns)  11 / 14 ok, diff --stat stalls at 45.09 s"
  echo "  native (git on the agent)       14 / 14 ok, 0.41 s flat"
  echo "  h2probe A/B, one connection     HTTP/1.1: 100 sequential 38.37 s / 100 concurrent 38.47 s (1.0x)"
  echo "                                  HTTP/2  : 100 sequential 19.66 s / 100 concurrent  0.79 s (25.0x)"
  echo
} | tee "$RAW"

# ── fixture ──────────────────────────────────────────────────────────────────
gen_fixture() {
  "$BAT" -mode fixture -root "$ROOT" -dirs "$DIRS" -per-dir "$PER_DIR" -big-mib "$BIG_MIB" >>"$RAW" 2>&1
}
if ! gen_fixture; then
  echo "fixture generation failed" >&2
  exit 2
fi

# ── the daemon: TLS + h3 on the SAME port number ─────────────────────────────
cat > "$WORK/tls-h3.yaml" <<YAML
server:
  grpc_addr: "127.0.0.1:$PORT"
  rest_addr: ""
  request_timeout: 120s
  webdav_enabled: true
  webdav_root: "$ROOT"
  h3_enabled: true
tls:
  enabled: true
  self_signed: true
  hosts: ["127.0.0.1", "localhost"]
  cert_file: "$WORK/cert.pem"
  key_file: "$WORK/key.pem"
auth:
  enabled: true
  token: "$TOKEN"
agent:
  base_data_dir: "$WORK/data"
  registry:
    enabled: false
YAML

"$BIN" --config "$WORK/tls-h3.yaml" >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!

health_wait() {
  local i
  for i in $(seq 1 80); do
    if curl -sS -k -o /dev/null "https://127.0.0.1:$PORT/healthz" 2>/dev/null; then
      return 0
    fi
    if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
      echo "daemon exited during startup:" | tee -a "$RAW"
      tail -20 "$DAEMON_LOG" >>"$RAW"
      return 1
    fi
    sleep 0.25
  done
  return 1
}

if ! health_wait; then
  echo "the daemon did not become healthy" >&2
  exit 2
fi

# A UDP socket on the same port number is what makes "h3 on this port" true; the
# probe asserts it once here so the h3 arms are not measuring a fallback.
PORT_HEX="$(printf '%04X' "$PORT")"
{
  if awk -v p=":$PORT_HEX" 'NR>1 && $2 ~ p "$" { found=1 } END { exit !found }' /proc/net/udp; then
    echo "socket check: UDP bound on the SAME port number as TCP (:$PORT)"
  else
    echo "socket check: WARNING — no UDP socket on :$PORT; the h3 arms will fail loudly"
  fi
  echo "daemon log: $(head -3 "$DAEMON_LOG" | tr '\n' ' ')"
  echo
} | tee -a "$RAW"

# ── the delay relay, for the link with a round trip on it ────────────────────
start_relay() {
  local ms="$1"
  "$BAT" -mode relay -relay-listen "127.0.0.1:$RELAY_PORT" -relay-target "127.0.0.1:$PORT" \
    -relay-delay "${ms}ms" >>"$RELAY_LOG" 2>&1 &
  RELAY_PID=$!
  local i
  for i in $(seq 1 60); do
    if grep -q "relay ready" "$RELAY_LOG" 2>/dev/null; then
      echo "relay up: $(cat "$RELAY_LOG")" | tee -a "$RAW"
      return 0
    fi
    sleep 0.25
  done
  echo "the relay did not start: $(cat "$RELAY_LOG" 2>/dev/null)" >&2
  return 1
}

DELAY_ACTIVE=0
case ",$LINKS," in
  *",delay${DELAY_MS},"*)
    if start_relay "$DELAY_MS"; then
      DELAY_ACTIVE=1
    else
      exit 2
    fi
    ;;
esac

link_base() { # link -> base URL
  case "$1" in
    loopback) echo "https://127.0.0.1:$PORT" ;;
    delay*)   echo "https://127.0.0.1:$RELAY_PORT" ;;
    *)        echo "" ;;
  esac
}

# ── the arms ─────────────────────────────────────────────────────────────────
ARMS_OK=0
ARMS_FAILED=0

run_arm() { # proto conns conc link
  local proto="$1" conns="$2" conc="$3" link="$4"
  local base label
  base="$(link_base "$link")"
  if [ -z "$base" ]; then
    echo "no base URL for link '$link'" >&2
    return 2
  fi
  case "$link" in
    loopback) label="${proto}-loopback-c${conns}x${conc}" ;;
    *)        label="${proto}-${link}-c${conns}x${conc}" ;;
  esac

  # A fresh, byte-identical tree for every arm: Fixture A plus the four root
  # files, regenerated deterministically. Two arms that started from different
  # trees would not be comparable at all.
  if ! gen_fixture >>"$RAW" 2>&1; then
    echo "failure: could not regenerate the fixture before $label" | tee -a "$RAW"
    ARMS_FAILED=$((ARMS_FAILED + 1))
    return 1
  fi

  {
    echo "----------------------------------------------------------------"
    echo "ARM $label  base=$base  proto=$proto conns=$conns concurrency=$conc"
    echo "----------------------------------------------------------------"
  } >>"$RAW"

  if "$BAT" -mode battery -url "$base" -proto "$proto" -conns "$conns" -concurrency "$conc" \
      -root "$ROOT" -out "$OUT" -label "$label" -link "$link" \
      -user bunker -token "$TOKEN" -op-timeout "${OP_TIMEOUT}s" -fanout "$FANOUT" >>"$RAW" 2>&1; then
    ARMS_OK=$((ARMS_OK + 1))
  else
    ARMS_FAILED=$((ARMS_FAILED + 1))
    echo "ARM FAILED: $label (see $RAW)" | tee -a "$RAW"
  fi
  # The arm's own table goes to the console too, so the run is readable live.
  sed -n "/^== arm ${label} ==/,/^   ARM RESULT/p" "$RAW"
  echo
}

for LINK in ${LINKS//,/ }; do
  case "$LINK" in
    loopback) ;;
    delay*)   [ "$DELAY_ACTIVE" = "1" ] || { echo "link '$LINK' requested but the relay is not up" >&2; exit 2; } ;;
    *)        echo "unknown link '$LINK'" >&2; exit 2 ;;
  esac
  for P in h1 h2 h3; do
    run_arm "$P" 1 1 "$LINK"
    run_arm "$P" 1 "$CONC" "$LINK"
    # HTTP/1.1 cannot multiplex, so MORE CONNECTIONS is its only route to
    # concurrency — the arm that separates "the protocol version" from
    # "concurrency" in the verdict.
    if [ "$P" = "h1" ]; then
      run_arm "$P" "$CONC" "$CONC" "$LINK"
    fi
  done
done

# ── the report ───────────────────────────────────────────────────────────────
{
  echo
  echo "================================================================================"
  echo "REPORT — per-protocol, per-concurrency tables and the AC-11 verdict"
  echo "================================================================================"
} | tee -a "$RAW"

set +e
"$BAT" -mode report -in "$OUT" | tee -a "$RAW"
REPORT_RC="${PIPESTATUS[0]}"
set -e

{
  echo
  echo "================================================================================"
  echo "RUN SUMMARY"
  echo "  arms ok            : $ARMS_OK"
  echo "  arms failed        : $ARMS_FAILED"
  echo "  report exit        : $REPORT_RC"
  echo "  loadavg (end)      : $(cut -d' ' -f1-3 /proc/loadavg 2>/dev/null || echo n/a)"
  echo "  results dir        : $OUT"
  echo "  raw output         : $RAW"
  echo "  per-request record : $OUT/requests.csv"
  echo "  reproduce with     : $0 --links $LINKS --conc $CONC --fanout $FANOUT"
  echo "================================================================================"
} | tee -a "$RAW"

if [ "$ARMS_FAILED" -ne 0 ] || [ "$REPORT_RC" -ne 0 ]; then
  echo "BATTERY: FAIL"
  exit 1
fi
echo "BATTERY: PASS (every arm's cells and contract notes passed; the concurrency verdict is above)"
exit 0
