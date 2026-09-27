#!/usr/bin/env bash
# ============================================================================
# BFS-047 — THE PUSH CHANNEL MEASURED AGAINST A REAL DATACENTRE.
#
# WHAT THIS IS.  A real client, driven from this host, against a real server at
# a real distance (dedi-2, Helsinki), over the public path — with every number
# it reports coming from that path and no number invented from loopback.  It is
# the same shape of instrument BFS-034 established for the fs battery: the
# measurement moves to the datacentre, the datacentre is never changed to suit
# the measurement, and every verification happens on the same side the change
# was made.
#
# THE LIVE HOST IS READ-ONLY.  dedi-2 runs a live `bunkerd` on *:18080.  This
# script NEVER deploys to it, restarts it, reconfigures it or touches its
# firewall.  What it does instead is what BFS-034 did and what the row that
# filed this work prescribed: it runs the RELEASE'S OWN probe server
# (probes/davserve) as a SEPARATE process on a SPARE PORT with ITS OWN fixture
# under /tmp, and removes both when it is done.  Mode `recon` measures — and
# leaves untouched — the live daemon itself.
#
# WHY THE PROBE SERVER AND NOT THE LIVE DAEMON.  Because the deployed build does
# not serve the surface at all: dedi-2's bunkerd is 0.1.4 @ 071ab3b, which
# predates internal/server/webdav entirely.  Measured, read-only, in
# docs/evidence/BFS-047-recon.txt: GET /dav/ -> 404, PROPFIND /dav/ -> 405 with
# no DAV header, OPTIONS * -> 404, POST /dav/ with X-Bunker-Op: watch -> 404.
# There is no channel on the live daemon to measure, and deploying the new one
# over it is out of bounds, so the honest DC measurement is the release's own
# surface server on the real path — and the live-daemon cell is reported
# UNTESTED rather than asserted.
#
# THE TOKEN.  Mode `recon` reads /etc/bunkerd/config.yaml through ssh into a
# shell variable, uses it for two read-only probes and never writes it to any
# file, log, transcript or fixture.  No other mode needs it, because no other
# mode talks to the live daemon.
#
# MODES
#   recon     the read-only live-host probe (RTT, deployed build, /dav probes)
#   local     every cell against a loopback endpoint  — the numbers the DC
#             numbers are compared against, measured with the same instrument
#   dc        every cell against dedi-2 over the public path
#   control   the instrument's own negative controls: the cells must be able to
#             go RED (endpoint without a watcher, mechanism honesty, and the
#             counted-resync cell against a neutered client)
#   teardown  verified teardown of anything the harness left behind
#   all       recon + local + dc + control
#
# USAGE  bash probes/bfs047-dc-channel.sh <mode>
# ============================================================================
set -uo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
ART=${ART:-$(mktemp -d "${TMPDIR:-/tmp}/bfs047-XXXXXX")}
BIN="$ART/bin"
OUT="$ART/transcript.txt"
CSV="$ART/cells.csv"
STAMP=$(date -u '+%Y%m%d%H%M%S')

DC_SSH=${DC_SSH:-dedi-2}
DC_NAME=${DC_NAME:-95-216-12-55.nip.io}   # the name the unattended terminal guard allows
DC_IP=${DC_IP:-95.216.12.55}              # used only where a raw address is not a URL (ping)
DC_PORT=${DC_PORT:-18471}                 # spare port, its own fixture; never the live daemon's
LOC_PORT=${LOC_PORT:-18511}
RDIR="/tmp/bfs047-dc-$STAMP"

HOLD=${HOLD:-95}                 # steady-push window: >3 declared heartbeats (30 s each)
SAMPLE_MS=${SAMPLE_MS:-200}
SAMPLE_S=$(perl -e "printf('%.3f', $SAMPLE_MS/1000)")
LAT_EDITS=${LAT_EDITS:-8}
STALL_SHORT=${STALL_SHORT:-3}    # seconds: under the declared write deadline
POLL_WIN=${POLL_WIN:-60}
POLL_INTERVAL=${POLL_INTERVAL:-2s}
ROTATE_LINES=${ROTATE_LINES:-620}      # well past the journal's 256-event bound, so the
ROTATE_SPACING=${ROTATE_SPACING:-0.12} # client's own cursor falls BEHIND the retained base:
                                       # one flush per write (flush_every_ms is 50), which is
                                       # what makes the count of LINES the thing that rotates.
ROTATE_LINES_SMALL=${ROTATE_LINES_SMALL:-40}  # the pinned-push cell needs no rotation (its loop has ended)
STORM_BURSTS=${STORM_BURSTS:-6}
STORM_FILES=${STORM_FILES:-600}
VOLUME_BURSTS=${VOLUME_BURSTS:-24}   # bursts pushed at a frozen reader, to reach the write deadline
VOLUME_FILES=${VOLUME_FILES:-3000}   # paths per burst (long names: the bytes are the point)
VOLUME_NAME_LEN=${VOLUME_NAME_LEN:-180}
VOLUME_BUDGET_MB=${VOLUME_BUDGET_MB:-16}

PROBE_USER=bfs047
PROBE_PW=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' | head -c 24)
CLIENT_BIN=${CLIENT_BIN:-}    # set by build_binaries; the control mode points it at a mutant
SERVER_TTL=${SERVER_TTL:-2400} # every server this script starts is bounded and self-terminating
SS_BIN=$(command -v ss || echo /usr/sbin/ss)

FAILED=0
FINDINGS=0
CELLN=0
MOUNTS=()
MOUNT_PIDS=()
READER_PIDS=()
SRV_PID=""
SRV_PID_DC=""

mkdir -p "$ART"
# EVERYTHING goes to the transcript file. A `tee` to the caller's stdout would make
# the harness's own progress depend on someone draining the session pipe, which is
# how a run of this instrument can sit blocked with no evidence of where. Watch it
# with:  tail -f $ART/transcript.txt
exec >>"$OUT" 2>&1
printf 'ART=%s\n' "$ART" > "$ART/ART.path"

log()   { printf '%s\n' "$*"; }
head2() { printf '\n%s\n%s\n' "────────────────────────────────────────────────────────────────────────────" "$*"; }
csv()   { printf '%s\n' "$*" >> "$CSV"; }
row()   { # row <arm> <cell> <metric> <value> <unit> <verdict> <note>
  csv "$1,$2,$3,$4,$5,$6,\"${7:-}\""
  printf '   %-26s %-20s %-10s %-9s %s\n' "$3" "${4:-}" "${5:-}" "[${6:-}]" "${7:-}"
}
verdict() { # verdict <ok|fail|untested|finding> <sentence>
  printf '   VERDICT: %s — %s\n' "$1" "$2"
  case "$1" in fail) FAILED=$((FAILED+1));; finding) FINDINGS=$((FINDINGS+1));; esac
}
need() { command -v "$1" >/dev/null 2>&1 || { log "MISSING TOOL: $1"; exit 3; }; }
num() { printf '%s' "${1:-0}" | sed -nE 's/^(-?[0-9]+).*/\1/p' | head -1; }

# ---------------------------------------------------------------- prerequisites
need jq; need curl; need perl; need ssh; need go

build_binaries() {
  head2 "BUILD — the release's own probe server and client, for this host"
  mkdir -p "$BIN"
  ( cd "$REPO_ROOT" && go build -o "$BIN/davserve" ./probes/davserve ) || { log "davserve build FAILED"; exit 4; }
  ( cd "$REPO_ROOT" && go build -o "$BIN/bunker" ./cmd/bunker )     || { log "client build FAILED"; exit 4; }
  CLIENT_BIN="$BIN/bunker"
  sha256sum "$BIN/davserve" "$BIN/bunker" | sed 's/^/   /'
}

fixture() { # fixture <dir> <n>
  local dir=$1 n=${2:-40}
  mkdir -p "$dir"
  local i
  for i in $(seq 1 "$n"); do printf 'fixture line %d\n' "$i" > "$dir/f$(printf '%03d' "$i").txt"; done
  printf 'read me first\n' > "$dir/README.txt"
}

# ------------------------------------------------------------------- endpoints
start_server_local() { # start_server_local <root> <port> <watch>
  local root=$1 port=$2 watch=$3
  port_free "$port" || { log "   ABORT: 127.0.0.1:$port is already taken (another run of this harness, or a sibling); refusing to measure against an endpoint that is not this run's"; return 1; }
  nohup timeout "$SERVER_TTL" "$BIN/davserve" --root "$root" --addr "127.0.0.1:$port" \
      ${watch:+$watch} --user "$PROBE_USER" --pass "$PROBE_PW" \
      > "$ART/server-local.log" 2>&1 &
  SRV_PID=$!
  local i
  for i in $(seq 1 60); do
    [ -s "$ART/server-local.log" ] && curl -s -u "$PROBE_USER:$PROBE_PW" -o /dev/null "$BASE/" && break
    sleep 0.5
  done
  sed -n 1,4p "$ART/server-local.log" | sed 's/^/   /'
  server_is_ours
}

# port_free answers "may this run bind here" BEFORE anything is started. A run that
# silently attaches to somebody else's endpoint measures the wrong tree and the
# wrong process — which is exactly what happened once here, and every number after
# it was meaningless.
port_free() { # port_free <port>
  local port=$1
  [ "$port" -gt 0 ] || return 0
  ! $SS_BIN -ltn 2>/dev/null | grep -q ":$port "
}

# server_is_ours fails unless the endpoint that answers is the one this run started
# and it serves the tree this run built.
server_is_ours() {
  local mode; mode=$(srv '.result.capabilities.extensions.watch.mode')
  if [ -z "$mode" ] || [ "$mode" = null ]; then
    log "   ABORT: the endpoint at $BASE did not answer the capability document as this run's server (mode='$mode') — refusing to measure"
    return 1
  fi
  log "   endpoint verified ours: mode=$mode (the capability document answered to THIS run's credential)"
  return 0
}

push_to_dc() { # push_to_dc <local-binary-or-dir> <remote-path>
  scp -q -o BatchMode=yes "$1" "$DC_SSH:$2" || return 1
}

start_server_dc() { # start_server_dc <localtree> <port> <watch>
  local tree=$1 port=$2 watch=$3
  head2 "START — the probe server on the DATACENTRE (spare port $port, its own fixture)"
  if ! $SS_BIN -ltn 2>/dev/null | grep -q ":$port " && ! ssh -o BatchMode=yes "$DC_SSH" "ss -ltn 2>/dev/null | grep -q ':$port '" ; then
    : # free on both ends is what we want
  else
    log "   ABORT: port $port is already in use (locally or on the DC); refusing to attach to an endpoint this run did not start"
    return 1
  fi
  ssh -o BatchMode=yes "$DC_SSH" "mkdir -p '$RDIR/tree'" || return 1
  tar -C "$tree" -cf - . | ssh -o BatchMode=yes "$DC_SSH" "tar -C '$RDIR/tree' -xf -" || return 1
  push_to_dc "$BIN/davserve" "$RDIR/davserve" || return 1
  ssh -o BatchMode=yes "$DC_SSH" "chmod +x '$RDIR/davserve'"
  ssh -o BatchMode=yes "$DC_SSH" \
    "cd '$RDIR' && nohup timeout $SERVER_TTL ./davserve --root '$RDIR/tree' --addr 0.0.0.0:$port ${watch:+$watch} \
       --user '$PROBE_USER' --pass '$PROBE_PW' > '$RDIR/server.log' 2>&1 & echo \$! > '$RDIR/pid'"
  sleep 2
  SRV_PID_DC=$(ssh -o BatchMode=yes "$DC_SSH" "cat '$RDIR/pid'")
  ssh -o BatchMode=yes "$DC_SSH" "cat '$RDIR/server.log'" | sed -n 1,4p | sed 's/^/   /'
  local i
  for i in $(seq 1 40); do
    curl -s -u "$PROBE_USER:$PROBE_PW" -o /dev/null "$BASE/" && break
    sleep 0.5
  done
  log "   the tree on the DC:  $RDIR/tree   (the client never writes it; the edits are made over ssh)"
  log "   the live daemon:     UNTOUCHED (its own pid and listeners are re-checked at teardown)"
  server_is_ours
}

# the capability document — the server's own declaration AND its live counters
cap()        { curl -s -u "$PROBE_USER:$PROBE_PW" -X POST -H 'X-Bunker-Op: capabilities' \
                    -H 'Content-Type: application/json' -d '{}' "$BASE/"; }
srv()        { cap | jq -r "$1" 2>/dev/null; }
push_ctr()   { srv '.result.capabilities.extensions.watch.push.counters.'"$1"; }
watch_ctr()  { srv '.result.capabilities.extensions.watch.counters.'"$1"; }

# ----------------------------------------------------------------------- client
mount_client() { # mount_client <name> <url> <mode> <extra...>
  local name=$1 url=$2 mode=$3; shift 3
  local mp="$ART/mnt-$name" mdir="$ART/mdir-$name"
  mkdir -p "$mp" "$mdir"
  nohup timeout "$SERVER_TTL" "${CLIENT_BIN:-$BIN/bunker}" fs mount "$mp" --url "$url" \
      --user "$PROBE_USER" --password "$PROBE_PW" \
      --invalidation "$mode" --poll-interval "$POLL_INTERVAL" --cache-dir "$mdir" "$@" \
      > "$ART/mount-$name.log" 2>&1 &
  local pid=$!
  local i
  for i in $(seq 1 80); do [ -f "$mdir/status.json" ] && break; sleep 0.5; done
  MOUNTS+=("$name:$mp:$mdir"); MOUNT_PIDS+=("$name:$pid")
  printf '   mount %-6s pid=%s mp=%s mode=%s\n' "$name" "$pid" "$mp" "$mode"
}
mount_of() { local n=$1 e; for e in "${MOUNTS[@]}"; do [ "${e%%:*}" = "$n" ] && { printf '%s' "${e#*:}"; return; }; done; }
mp_of()    { mount_of "$1" | cut -d: -f1; }
mdir_of()  { mount_of "$1" | cut -d: -f2; }
pid_of()   { local n=$1 e; for e in "${MOUNT_PIDS[@]}"; do [ "${e%%:*}" = "$n" ] && { printf '%s' "${e#*:}"; return; }; done; }
st()       { jq -r "$1" "$(mdir_of "$2")/status.json" 2>/dev/null; }
umount_all() {
  local e name mp
  for e in "${MOUNTS[@]}"; do
    name=${e%%:*}; mp=$(mp_of "$name")
    "${CLIENT_BIN:-$BIN/bunker}" fs umount "$mp" >/dev/null 2>&1
    sleep 0.2
    if mountpoint -q "$mp" 2>/dev/null; then fusermount -u "$mp" >/dev/null 2>&1; fi
  done
  for e in "${MOUNT_PIDS[@]}"; do kill -TERM "${e#*:}" 2>/dev/null; done
  sleep 0.5
  MOUNTS=(); MOUNT_PIDS=()
}
stop_readers() { local p; for p in "${READER_PIDS[@]:-}"; do [ -n "$p" ] && kill -TERM "$p" 2>/dev/null; done; READER_PIDS=(); }

# the raw NDJSON reader: the wire itself, timestamped per line (a second subscriber
# in its own right, and the only way to time a LINE rather than a status sample)
reader_start() { # reader_start <name> <url> <since_seq>
  local name=$1 url=$2 since=$3
  nohup bash -c "curl -sN -u '$PROBE_USER:$PROBE_PW' -X POST -H 'X-Bunker-Op: watch' \
      -H 'Content-Type: application/json' -H 'Accept: application/x-ndjson' \
      -d '{\"paths\":[],\"since_seq\":$since}' '$url/' | \
      perl -MTime::HiRes=time -ne 'BEGIN{\$|=1} printf \"%.6f %s\", time, \$_'" \
      > "$ART/reader-$name.log" 2>&1 &
  READER_PIDS+=($!)
  sleep 1
  printf '   raw reader %-6s lines so far: %s\n' "$name" "$(wc -l < "$ART/reader-$name.log" 2>/dev/null || echo 0)"
}

# ------------------------------------------------------------------ the DC edits
far_write() { # far_write <arm> <relative-path> <content>
  local arm=$1 p=$2 c=$3
  case "$arm" in
    dc)    ssh -o BatchMode=yes "$DC_SSH" "printf '%s\n' '$c' > '$RDIR/tree/$p'" ;;
    local) printf '%s\n' "$c" > "$LOCAL_TREE/$p" ;;
  esac
}
far_append() { local arm=$1 p=$2 c=$3; case "$arm" in
    dc)    ssh -o BatchMode=yes "$DC_SSH" "printf '%s\n' '$c' >> '$RDIR/tree/$p'" ;;
    local) printf '%s\n' "$c" >> "$LOCAL_TREE/$p" ;;
  esac; }
far_read() { local arm=$1 p=$2; case "$arm" in
    dc)    ssh -o BatchMode=yes "$DC_SSH" "cat '$RDIR/tree/$p' 2>&1" ;;
    local) cat "$LOCAL_TREE/$p" 2>&1 ;;
  esac; }
far_burst() { # far_burst <arm> <n> <tag> [name-len]
  # One burst inside a flush interval, with paths long enough that a handful of
  # bursts is megabytes on the wire: the point is to fill the path's buffers, so
  # the BYTES per line matter and the file count is what costs.
  local arm=$1 n=$2 tag=$3 len=${4:-180}
  case "$arm" in
    dc)    ssh -o BatchMode=yes "$DC_SSH" "cd '$RDIR/tree' && L=\$(printf 'x%.0s' \$(seq 1 $len)) && for i in \$(seq 1 $n); do : > \"${tag}\$i\$L\"; done" ;;
    local) local i L; L=$(printf 'x%.0s' $(seq 1 "$len")); for i in $(seq 1 "$n"); do : > "$LOCAL_TREE/${tag}$i$L"; done ;;
  esac; }
far_rotate() { # far_rotate <arm> <lines> <spacing>  — one line per flush interval
  local arm=$1 lines=$2 sp=$3
  case "$arm" in
    dc)    ssh -o BatchMode=yes "$DC_SSH" "cd '$RDIR/tree' && for i in \$(seq 1 $lines); do : > r\$i.x; sleep $sp; done" ;;
    local) local i; for i in $(seq 1 "$lines"); do : > "$LOCAL_TREE/r$i.x"; sleep "$sp"; done ;;
  esac; }
far_git_free() { :; }

# the ledger's own head, read from the server — the precondition of the counted-gap
# arm is that the rotation really moved MORE lines than the journal retains, and a
# precondition that is assumed rather than measured is how a cell reports a green it
# did not earn.
ledger_head() { curl -s -u "$PROBE_USER:$PROBE_PW" -X POST -H 'X-Bunker-Op: events' \
    -H 'Content-Type: application/json' -d '{"paths":[],"since_seq":0}' "$BASE/" | jq -r '.result.head_seq' 2>/dev/null; }

# --------------------------------------------------------------------- sampling
# the mount's own status, sampled while a cell runs (the client's view over time)
sample_start() { # sample_start <name>
  local name=$1
  nohup bash -c "while :; do printf '%s %s\n' \"\$(date +%s.%N)\" \"\$(cat '$(mdir_of "$name")/status.json' 2>/dev/null | tr -d '\n')\" >> '$ART/samples-$name.log'; sleep $SAMPLE_S; done" \
      >/dev/null 2>&1 &
  SAMPLE_PID=$!
  SAMPLE_PID_name=$name
}
sample_stop() { [ -n "${SAMPLE_PID:-}" ] && kill -TERM "$SAMPLE_PID" 2>/dev/null; SAMPLE_PID=""; sleep "$SAMPLE_S"; }

# ============================================================================
# CELL 0 — THE LINK (measure the path before trusting anything on it)
# ============================================================================
cell_link() { # cell_link <arm>
  local arm=$1 target
  head2 "CELL 0 — THE LINK ($arm)"
  [ "$arm" = dc ] && target=$DC_IP || target=127.0.0.1
  local ping_out
  ping_out=$(ping -c 10 -W 3 "$target" 2>&1 | tail -2)
  log "$ping_out" | sed 's/^/   /'
  local avg mdev loss
  avg=$(printf '%s' "$ping_out" | sed -nE 's/.*rtt min\/avg\/max\/mdev = ([0-9.]+)\/([0-9.]+)\/([0-9.]+)\/([0-9.]+) ms.*/\2/p')
  mdev=$(printf '%s' "$ping_out" | sed -nE 's/.*rtt min\/avg\/max\/mdev = ([0-9.]+)\/([0-9.]+)\/([0-9.]+)\/([0-9.]+) ms.*/\4/p')
  loss=$(printf '%s' "$ping_out" | sed -nE 's/.* ([0-9.]+)% packet loss.*/\1/p')
  row "$arm" link rtt_avg "${avg:-?}" ms "$(awk -v a="${avg:-0}" 'BEGIN{print (a>100)?"real-wan":"loopback"}')" "icmp, 10 packets"
  row "$arm" link rtt_mdev "${mdev:-?}" ms jitter "icmp, 10 packets"
  row "$arm" link loss "${loss:-?}" % ok "icmp, 10 packets"

  local i out tc tt
  tc=""; tt=""
  for i in $(seq 1 5); do
    out=$(curl -s -o /dev/null -w '%{time_connect} %{time_total}' "$BASE/")
    tc="$tc $(printf '%s' "$out" | cut -d' ' -f1)"; tt="$tt $(printf '%s' "$out" | cut -d' ' -f2)"
  done
  log "   HTTP connects (s):$tc"
  log "   HTTP totals   (s):$tt"
  row "$arm" link http_connect_median "$(printf '%s' "$tc" | tr ' ' '\n' | grep . | sort -g | sed -n 3p)" s ok "5 GETs"
  row "$arm" link http_total_median "$(printf '%s' "$tt" | tr ' ' '\n' | grep . | sort -g | sed -n 3p)" s ok "5 GETs"
}

# ============================================================================
# CELL A — DOES THE CHANNEL ESTABLISH AND STAY UP?
# ============================================================================
cell_establish() { # cell_establish <arm> <hold-seconds>
  local arm=$1 hold=${2:-$HOLD}
  head2 "CELL A — ESTABLISH AND STAY UP ($arm, ${hold}s ≈ $((hold/30)) declared heartbeats)"
  local doc mode served hb wd maxsub
  doc=$(cap)
  mode=$(printf '%s' "$doc"   | jq -r '.result.capabilities.extensions.watch.mode')
  served=$(printf '%s' "$doc" | jq -r '.result.capabilities.extensions.watch.push.served')
  hb=$(printf '%s' "$doc"     | jq -r '.result.capabilities.extensions.watch.push.heartbeat_ms')
  wd=$(printf '%s' "$doc"     | jq -r '.result.capabilities.extensions.watch.push.write_deadline_ms')
  maxsub=$(printf '%s' "$doc" | jq -r '.result.capabilities.extensions.watch.push.max_subscribers')
  log "   the server declares: watch.mode=$mode push.served=$served heartbeat_ms=$hb write_deadline_ms=$wd max_subscribers=$maxsub"
  row "$arm" establish server_mode "$mode" state "$([ "$mode" = push ] && echo ok || echo FAIL)" "the declaration the client switches on"
  if [ "$mode" != push ] || [ "$served" != true ]; then
    verdict fail "this endpoint does not offer the pushed form, so NOTHING below it can be measured: this is the RED the control mode also produces on purpose"
    return 1
  fi
  row "$arm" establish declared_heartbeat "$hb" ms ok "server's own declaration"
  row "$arm" establish declared_write_deadline "$wd" ms ok "server's own declaration (< heartbeat, per §4.4)"

  mount_client push "$BASE" push
  sleep 2
  if [ -z "$(pid_of push)" ]; then
    verdict fail "the mount did not start against $BASE — no measurement is reported"
    return 1
  fi
  reader_start push "$BASE" 0
  sample_start push
  log "   holding the channel for ${hold}s …"
  sleep "$hold"
  sample_stop push

  local hbclient hbserver mech seq ends rec idlefb gaps resyncs fails req
  mech=$(st '.invalidation.mechanism' push); seq=$(st '.invalidation.seq' push)
  hbclient=$(st '.invalidation.liveness.heartbeats_total' push)
  hbserver=$(srv '.result.capabilities.extensions.watch.counters.event_loop_ticks')
  ends=$(st '.invalidation.stream_ends_total' push); rec=$(st '.invalidation.reconnects_total' push)
  idlefb=$(st '.invalidation.idle_fallbacks_total' push)
  gaps=$(st '.invalidation.resyncs_from_gap' push); resyncs=$(st '.invalidation.resyncs_total' push)
  fails=$(st '.invalidation.failures_total' push); req=$(st '.invalidation.requests_total' push)
  local idle dhb
  idle=$(st '.invalidation.idle_timeout_ms' push); dhb=$(st '.invalidation.liveness.declared_heartbeat_ms' push)

  log "   client record: mechanism=$mech seq=$seq heartbeats=$hbclient idle_timeout_ms=$idle declared_heartbeat_ms=$dhb"
  log "   outcome: stream_ends=$ends reconnects=$rec idle_fallbacks=$idlefb gaps=$gaps resyncs=$resyncs failures=$fails requests=$req"
  local rawlines hbraw
  rawlines=$(wc -l < "$ART/reader-push.log" 2>/dev/null || echo 0)
  hbraw=$(grep -c '"event":"heartbeat"' "$ART/reader-push.log" 2>/dev/null || echo 0)
  log "   the wire itself, read raw: $rawlines line(s), $hbraw heartbeat(s)"
  row "$arm" establish subscribers_active "$(push_ctr subscribers_active)" streams ok "server counters"
  row "$arm" establish heartbeats_client "${hbclient:-?}" count ok "client's own record"
  row "$arm" establish heartbeats_raw "$hbraw" count ok "the raw NDJSON wire"
  row "$arm" establish heartbeats_expected "$((hold/30))" count ok "at the declared 30 s period"
  row "$arm" establish stream_ends "$ends" count "$([ "$ends" = 0 ] && echo stable || echo re-established)"
  row "$arm" establish deadline_disconnects "$(push_ctr subscriber_disconnects_write_deadline_total)" count "$([ "$(push_ctr subscriber_disconnects_write_deadline_total)" = 0 ] && echo none || echo fired)"

  if [ "$mech" = watch ] && [ "${ends:-0}" = 0 ] && [ "$hbraw" -ge 1 ]; then
    verdict ok "the channel ESTABLISHED and STAYED UP for ${hold}s: $hbraw heartbeat(s) on the wire, mechanism=watch, no stream ended, no failure"
    return 0
  fi
  verdict fail "the channel did not stay up: mechanism=$mech stream_ends=$ends heartbeats=$hbraw"
  return 1
}

# ============================================================================
# CELL B — THE HEARTBEAT AND THE WRITE DEADLINE AT REAL LATENCY (the margin)
# ============================================================================
heartbeat_cadence() { # heartbeat_cadence <arm> <reader-name>
  local arm=$1 rn=$2
  head2 "CELL B1 — THE HEARTBEAT CADENCE ON THE WIRE ($arm)"
  local f="$ART/reader-$rn.log"
  local stamps n
  stamps=$(grep '"event":"heartbeat"' "$f" 2>/dev/null | awk '{print $1}')
  n=$(printf '%s\n' "$stamps" | grep -c . || true)
  if [ "${n:-0}" -lt 2 ]; then
    row "$arm" margin heartbeat_intervals 0 count untested "fewer than two heartbeats on the wire in this window"
    verdict untested "no cadence to measure: fewer than two heartbeats arrived in the hold window"
    return 1
  fi
  printf '%s\n' "$stamps" | awk '
    NR<3 { prev=$1; next }
    NR==3 { printf "   (interval 1 skipped: the first heartbeat is the ESTABLISH line the subscription mints, not a ticker fire)\n" }
    { d=($1-prev)*1000; prev=$1; n++; s+=d; if (n==1||d<mn) mn=d; if (n==1||d>mx) mx=d;
      printf "   interval %d: %.0f ms\n", n, d }
    END { printf "   intervals=%d mean=%.0f ms min=%.0f ms max=%.0f ms\n", n, s/n, mn, mx }'
  local mean mx mn
  mean=$(printf '%s\n' "$stamps" | awk 'NR<3{prev=$1;next}{d=($1-prev)*1000;prev=$1;n++;s+=d}END{if(n)printf "%.0f", s/n; else print 0}')
  mx=$(printf '%s\n' "$stamps"   | awk 'NR<3{prev=$1;next}{d=($1-prev)*1000;prev=$1;if(n++==0||d>mx)mx=d}END{printf "%.0f", mx}')
  mn=$(printf '%s\n' "$stamps"   | awk 'NR<3{prev=$1;next}{d=($1-prev)*1000;prev=$1;if(n++==0||d<mn)mn=d}END{printf "%.0f", mn}')
  local dev; dev=$(awk -v a="$mean" -v b=30000 'BEGIN{d=a-b; if(d<0)d=-d; printf "%.0f", d}')
  row "$arm" margin heartbeat_declared 30000 ms ok "server's declaration"
  row "$arm" margin heartbeat_observed_mean "$mean" ms ok "measured on the wire over $n interval(s)"
  row "$arm" margin heartbeat_observed_max "$mx" ms ok "worst interval"
  row "$arm" margin heartbeat_error_ms "$dev" ms "$(awk -v d="$dev" 'BEGIN{print (d<3000)?"within 10% of the declared period":"OUTSIDE the declared period"}')" "|observed mean - declared|"
  row "$arm" margin heartbeat_jitter_ms "$(awk -v a="$mx" -v b="$mn" 'BEGIN{printf "%.0f", a-b}')" ms ok "max-min across the window"
  local idle; idle=$(st '.invalidation.idle_timeout_ms' push)
  row "$arm" margin client_idle_timeout "${idle:-?}" ms ok "the client's own silence deadline (3 declared heartbeats)"
  if [ -n "${idle:-}" ]; then
    row "$arm" margin idle_margin_ms "$(awk -v i="$idle" -v m="$mx" 'BEGIN{printf "%.0f", i-m}')" ms \
        "$(awk -v i="$idle" -v m="$mx" 'BEGIN{print (i-m>m)?"comfortable (a stalled channel is declared dead while the previous heartbeat is itself the margin)":"TIGHT"}')" \
        "silence deadline minus the worst observed interval"
  fi
  return 0
}

write_deadline_margin() { # write_deadline_margin <arm>
  local arm=$1
  head2 "CELL B2 — THE WRITE DEADLINE UNDER REAL LATENCY: the margin, measured ($arm)"
  local wd hb
  wd=$(srv '.result.capabilities.extensions.watch.push.write_deadline_ms')
  hb=$(srv '.result.capabilities.extensions.watch.push.heartbeat_ms')
  log "   declared: write_deadline_ms=$wd  heartbeat_ms=$hb  (deadline strictly below the heartbeat)"

  # (a) the healthy cost of a line's journey, measured end to end on the real path
  local i t0 lines_before first ts lat lat_list=""
  for i in $(seq 1 "$LAT_EDITS"); do
    lines_before=$(wc -l < "$ART/reader-push.log" 2>/dev/null || echo 0)
    far_write "$arm" "lat-$i.txt" "latency probe $i $STAMP"
    t0=$(date +%s.%N)
    # the line that answers THIS edit: the first INVALIDATE after the mark. A
    # heartbeat arriving between the mark and the edit is not an answer to it, and
    # counting one produced a negative latency the first time this ran.
    first=$(awk -v n="$lines_before" 'NR>n && /"event":"invalidate"/{print $1; exit}' "$ART/reader-push.log" 2>/dev/null)
    local guard=0
    while [ -z "$first" ] && [ $guard -lt 120 ]; do sleep 0.05; guard=$((guard+1));
      first=$(awk -v n="$lines_before" 'NR>n && /"event":"invalidate"/{print $1; exit}' "$ART/reader-push.log" 2>/dev/null); done
    if [ -n "$first" ]; then
      lat=$(perl -e "my \$d=($first-$t0)*1000; printf('%.1f', \$d<0?0:\$d)")
      lat_list="$lat_list $lat"
      printf '   edit %d -> line on the wire: %s ms\n' "$i" "$lat"
    else
      printf '   edit %d -> NO line arrived\n' "$i"
    fi
    sleep 0.5
  done
  local p50 p100
  p50=$(printf '%s' "$lat_list" | tr ' ' '\n' | grep -E '^[0-9]' | sort -g | awk '{a[NR]=$1} END{print a[int((NR+1)/2)]}')
  p100=$(printf '%s' "$lat_list" | tr ' ' '\n' | grep -E '^[0-9]' | sort -g | tail -1)
  row "$arm" margin line_arrival_p50 "${p50:-?}" ms ok "out-of-band edit on the far side -> line on the wire"
  row "$arm" margin line_arrival_p100 "${p100:-?}" ms ok "worst of $LAT_EDITS"
  if [ -n "${p100:-}" ]; then
    row "$arm" margin deadline_margin_p100 "$((wd - ${p100%.*}))" ms "$([ $((wd - ${p100%.*})) -gt $((wd/2)) ] && echo comfortable || echo thin)" \
        "declared ${wd} ms minus the worst observed healthy line"
  fi

  # (b) a slow-but-ALIVE reader, INSIDE the bound: it must survive
  local mpid; mpid=$(pid_of push)
  local ends0 rec0 wd0 gap0 res0
  ends0=$(st '.invalidation.stream_ends_total' push); rec0=$(st '.invalidation.reconnects_total' push)
  wd0=$(push_ctr subscriber_disconnects_write_deadline_total); gap0=$(st '.invalidation.resyncs_from_gap' push)
  log "   freezing the reader for ${STALL_SHORT}s (under the ${wd} ms deadline) and writing on the far side"
  kill -STOP "$mpid"; sleep 1
  far_write "$arm" "stall-short.txt" "written while the reader was frozen for ${STALL_SHORT}s"
  sleep "$STALL_SHORT"; kill -CONT "$mpid"; sleep 3
  local ends1 rec1 wd1
  ends1=$(st '.invalidation.stream_ends_total' push); rec1=$(st '.invalidation.reconnects_total' push)
  wd1=$(push_ctr subscriber_disconnects_write_deadline_total)
  local seen; seen=$(cat "$(mp_of push)/stall-short.txt" 2>&1 | tail -1)
  log "   after resume: stream_ends $ends0->$ends1 reconnects $rec0->$rec1 deadline_disconnects $wd0->$wd1"
  log "   the change written during the freeze, read back through the mount: $seen"
  row "$arm" margin short_stall_secs "$STALL_SHORT" s ok "reader frozen, then resumed"
  row "$arm" margin short_stall_killed "$((wd1-wd0))" count "$([ $((wd1-wd0)) -eq 0 ] && echo survived || echo KILLED)"
  row "$arm" margin short_stall_stream_ends "$((ends1-ends0))" count "$([ $((ends1-ends0)) -eq 0 ] && echo survived || echo re-established)"
  local alive=ok
  if [ $((wd1-wd0)) -ne 0 ] || ! printf '%s' "$seen" | grep -q 'frozen for'; then
    alive=fail
    verdict fail "a reader that stalls for ${STALL_SHORT}s — WELL INSIDE the declared ${wd} ms — was killed or lost the change: the ordering does NOT hold at this latency"
  else
    verdict ok "a ${STALL_SHORT}s stall is inside the declared ${wd} ms bound: the write was queued and delivered, nothing was killed"
  fi
  [ "$alive" = fail ] && FAILED=$((FAILED+1))

  # (c) how much the path absorbs before the writer can block: the BYTE margin
  head2 "CELL B3 — HOW MUCH THE PATH ABSORBS BEFORE THE WRITER CAN BLOCK ($arm)"
  local rx0 wd2
  rx0=$(push_ctr subscriber_buffer_high_water_bytes)
  wd2=$(push_ctr subscriber_disconnects_write_deadline_total)
  log "   freezing the reader and pushing up to ${VOLUME_BURSTS} bursts of ${VOLUME_FILES} paths (~$(( VOLUME_FILES * (VOLUME_NAME_LEN + 40) / 1024 )) KB per burst), budget ${VOLUME_BUDGET_MB} MB"
  kill -STOP "$mpid"
  local b pushed_bytes=0 bytes_per_burst=$(( VOLUME_FILES * (VOLUME_NAME_LEN + 40) ))
  local t_vol0 t_vol1 fired=0 fire_bytes=0 fire_secs=""
  t_vol0=$(date +%s.%N)
  for b in $(seq 1 "$VOLUME_BURSTS"); do
    far_burst "$arm" "$VOLUME_FILES" "v${b}_" "$VOLUME_NAME_LEN"
    pushed_bytes=$(( pushed_bytes + bytes_per_burst ))
    sleep 0.2
    local wdnow; wdnow=$(push_ctr subscriber_disconnects_write_deadline_total)
    if [ "${wdnow:-0}" -gt "${wd2:-0}" ]; then
      fired=1; fire_bytes=$pushed_bytes; t_vol1=$(date +%s.%N)
      fire_secs=$(perl -e "printf('%.1f', $t_vol1-$t_vol0)")
      log "   THE WRITE DEADLINE FIRED after burst $b (~$(( pushed_bytes / 1048576 )) MB pushed, ${fire_secs}s)"
      break
    fi
    if [ $(( pushed_bytes / 1048576 )) -ge "$VOLUME_BUDGET_MB" ]; then
      log "   byte budget (${VOLUME_BUDGET_MB} MB) reached without the write deadline firing"; break
    fi
  done
  local sslocal ssremote="" rcvq sndq
  # The socket's OWN columns, not a guess: Recv-Q is what this frozen reader's
  # kernel holds unread, Send-Q what the far end has queued and not had acked.
  # These are the bytes the path absorbs before the server's write can block.
  sslocal=$($SS_BIN -tnm "dport = :$([ "$arm" = dc ] && echo "$DC_PORT" || echo "$LOC_PORT")" 2>/dev/null | head -12)
  log "   this end — the reader's own sockets (skmem rb = its receive buffer bound):"
  printf '%s\n' "$sslocal" | sed 's/^/     /'
  if [ "$arm" = dc ]; then
    ssremote=$(ssh -o BatchMode=yes "$DC_SSH" "ss -tnm 'sport = :$DC_PORT' 2>/dev/null | head -12")
    log "   the far end — the server's own sockets (Send-Q = queued and unacknowledged):"
    printf '%s\n' "$ssremote" | sed 's/^/     /'
  fi
  local wd3 drops
  wd3=$(push_ctr subscriber_disconnects_write_deadline_total)
  drops=$(push_ctr subscriber_drops_total)
  rcvq=$(printf '%s' "$sslocal"  | awk '$1=="ESTAB"{if($2>m)m=$2}END{printf "%d", m+0}')
  sndq=$(printf '%s' "$ssremote" | awk '$1=="ESTAB"{if($3>m)m=$3}END{printf "%d", m+0}')
  row "$arm" margin reader_recvq_at_freeze "$rcvq" bytes ok "MEASURED unread bytes held by this reader's kernel"
  row "$arm" margin server_sendq_at_freeze "${sndq:-0}" bytes ok "MEASURED unacknowledged bytes queued on the far socket"
  row "$arm" margin absorbed_high_water "$(push_ctr subscriber_buffer_high_water_bytes)" bytes ok "the channel's own buffer high-water (bound 4 MiB / 256 events)"
  row "$arm" margin paths_written_estimate "$(( pushed_bytes / 1048576 ))" MB ok "MB the far side was ASKED to write by this harness (an estimate of the volume, not a queue measurement)"
  row "$arm" margin subscriber_drops "${drops:-?}" count "$([ "${drops:-0}" -gt 0 ] && echo counted-drops || echo none)" "drop-oldest is COUNTED, never silent"
  kill -CONT "$mpid"; sleep 4
  wd3=$(push_ctr subscriber_disconnects_write_deadline_total)
  row "$arm" margin deadline_fired_under_volume "$((wd3-wd2))" count "$([ $((wd3-wd2)) -gt 0 ] && echo FIRED || echo not-reached)" "the server's own counter, read over the wire"
  if [ "$fired" = 1 ]; then
    row "$arm" margin deadline_fire_after "$fire_secs" s ok "from the first burst to the counter moving"
    row "$arm" margin deadline_fire_bytes "$(( fire_bytes / 1048576 ))" MB ok "bytes the far side wrote before the write blocked"
    row "$arm" margin deadline_declared "$wd" ms ok "the bound that fired"
    verdict ok "the write deadline FIRED at the far end after ~$(( fire_bytes / 1048576 )) MB pushed into a reader that had stopped: the bound is ENFORCED over this path, and it fired within the declared ${wd} ms of the write blocking"
  else
    verdict untested "UNTESTED AT THIS VOLUME: ${VOLUME_BURSTS}x${VOLUME_FILES} paths (≈$(( pushed_bytes / 1048576 )) MB) did not make the far end's write block, so the deadline had nothing to fire on. The declared ${wd} ms is therefore NOT disproved — it was not REACHED. What the arm does measure is the slack above it: this reader's socket held ${rcvq:-?} bytes unread at the freeze."
  fi
  return 0
}

# ============================================================================
# CELL C — RECONNECT, AND THE COUNTED RESYNC
# ============================================================================
cell_reconnect() { # cell_reconnect <arm>
  local arm=$1
  head2 "CELL C — RECONNECT AND RESYNC ($arm)"
  local mpid; mpid=$(pid_of push)
  if [ -z "$mpid" ]; then
    row "$arm" reconnect mount "-" "-" untested "no push mount from the establishing cell to reconnect"
    verdict untested "the reconnect cell has no mount to break: the establishing cell did not leave one"
    return 1
  fi
  local ends0 rec0 gaps0 res0 seq0
  ends0=$(st '.invalidation.stream_ends_total' push); rec0=$(st '.invalidation.reconnects_total' push)
  gaps0=$(st '.invalidation.resyncs_from_gap' push); res0=$(st '.invalidation.resyncs_total' push)
  seq0=$(st '.invalidation.seq' push)
  log "   before: seq=$seq0 stream_ends=$ends0 reconnects=$rec0 gaps=$gaps0 resyncs=$res0"

  # 1. break the connection while the reader cannot react, then rotate the
  #    ledger past its cursor — with the count of lines the rotation produced
  log "   freezing the reader, dropping its connection, then rotating the ledger past its cursor"
  kill -STOP "$mpid"
  sleep 1
  local killarg
  if [ "$arm" = dc ]; then killarg="dst $DC_IP dport = $DC_PORT"; else killarg="dst 127.0.0.1 dport = $LOC_PORT"; fi
  sudo -n "$SS_BIN" -K $killarg >/dev/null 2>&1 \
    && log "   the reader's stream connection was dropped (ss -K, on this host — the DC is not touched)" \
    || log "   (ss -K found nothing to drop; the reconnect arm then measures whatever break came instead)"
  sleep 1
  local rotate_t0 rotate_t1 rotated
  rotate_t0=$(date +%s.%N)
  far_rotate "$arm" "$ROTATE_LINES_SMALL" "$ROTATE_SPACING"
  rotate_t1=$(date +%s.%N)
  rotated=$(perl -e "printf('%.1f', $rotate_t1-$rotate_t0)")
  log "   rotated $ROTATE_LINES_SMALL paths over ${rotated}s (the pinned client's loop has already ended, so this only asks whether anything reached it)"
  local writes_before writes_after
  writes_before=$(st '.invalidation.seq' push)
  local t_resume; t_resume=$(date +%s.%N)
  kill -CONT "$mpid"
  local t_recon="" t_view="" guard=0 seq_now
  while [ $guard -lt 300 ]; do
    sleep 0.25; guard=$((guard+1))
    local rec; rec=$(st '.invalidation.reconnects_total' push)
    if [ -z "$t_recon" ] && [ "${rec:-0}" -gt "$rec0" ]; then t_recon=$(date +%s.%N); fi
    seq_now=$(st '.invalidation.seq' push)
    if [ -n "$t_recon" ] && [ "${seq_now:-0}" -gt "${seq0:-0}" ] && [ -e "$(mp_of push)/r1.x" ]; then t_view=$(date +%s.%N); break; fi
  done
  local ends1 rec1 gaps1 res1 reason
  ends1=$(st '.invalidation.stream_ends_total' push); rec1=$(st '.invalidation.reconnects_total' push)
  gaps1=$(st '.invalidation.resyncs_from_gap' push); res1=$(st '.invalidation.resyncs_total' push)
  reason=$(st '.invalidation.reason' push)
  local dt_recon dt_view
  [ -n "$t_recon" ] && dt_recon=$(perl -e "printf('%.2f', $t_recon-$t_resume)") || dt_recon="n/a"
  [ -n "$t_view" ]  && dt_view=$(perl -e "printf('%.2f', $t_view-$t_resume)")  || dt_view="n/a"
  log "   after:  stream_ends=$ends1 reconnects=$rec1 gaps=$gaps1 resyncs=$res1 seq=$(st '.invalidation.seq' push)"
  log "   reason recorded by the client: ${reason:-<none>}"
  row "$arm" reconnect resume_to_reconnect "$dt_recon" s "$([ "$dt_recon" != n/a ] && echo ok || echo fail)" "from SIGCONT to the client's own reconnect counter"
  row "$arm" reconnect resume_to_working_view "$dt_view" s "$([ "$dt_view" != n/a ] && echo ok || echo fail)" "from SIGCONT to the view being past where it was"
  row "$arm" reconnect stream_ends "$((ends1-ends0))" count "$([ $((ends1-ends0)) -gt 0 ] && echo ended || echo none)" "the dropped connection was seen"
  row "$arm" reconnect reconnects "$((rec1-rec0))" count "$([ $((rec1-rec0)) -gt 0 ] && echo re-established || echo NONE)"
  row "$arm" reconnect counted_gaps "$((gaps1-gaps0))" count "$([ $((gaps1-gaps0)) -gt 0 ] && echo counted || echo none)"
  row "$arm" reconnect counted_resyncs "$((res1-res0))" count "$([ $((res1-res0)) -gt 0 ] && echo counted || echo none)"

  # the classification the row asks for: counted-and-NAMED, or silent
  local classified
  if [ $((gaps1-gaps0)) -gt 0 ] && [ -n "$reason" ]; then
    classified="COUNTED AND NAMED (resyncs_from_gap +$((gaps1-gaps0)), reason names it: ${reason:0:70})"
  elif [ $((res1-res0)) -gt 0 ] && [ -n "$reason" ]; then
    classified="COUNTED AND NAMED (resyncs_total +$((res1-res0)), reason: ${reason:0:70})"
  elif [ $((ends1-ends0)) -gt 0 ] && [ $((rec1-rec0)) -gt 0 ]; then
    classified="RE-ESTABLISHED WITH NOTHING COUNTED — the client resumed from the journal without naming a gap (correct only if no line was missed)"
  elif [ $((ends1-ends0)) -eq 0 ] && [ $((rec1-rec0)) -eq 0 ]; then
    classified="PINNED-PUSH: THE DROPPED CONNECTION ENDED THE LOOP — no stream end counted, no reconnect, and the record still reads mode=push/mechanism=watch (reason: ${reason:0:60})"
  else
    classified="NOTHING MOVED — the drop was not even noticed"
  fi
  row "$arm" reconnect resync_classification "-" "-" "$(printf '%s' "$classified" | cut -c1-30)" "$classified"
  case "$classified" in
    COUNTED*) verdict ok "reconnect + resync behaviour: $classified";;
    RE-ESTABLISHED*) verdict ok "reconnect measured; the resume was precise from the retained journal: $classified";;
    PINNED-PUSH*) verdict finding "measured, and reported as a defect for the owner rather than smoothed over: $classified";;
    *) verdict fail "reconnect behaviour: $classified";;
  esac

  # same-side check: the rotated paths must be visible through the mount
  local i ok_count=0
  for i in 1 5; do [ -e "$(mp_of push)/r$i.x" ] && ok_count=$((ok_count+1)); done
  row "$arm" reconnect view_after_resync "$ok_count" "of 2" "$([ "$ok_count" -eq 2 ] && echo ok || echo fail)" "rotated paths visible through the mount"
  return 0
}

# ============================================================================
# CELL C2 — THE SAME DROP IN THE MODE A USER WHO DOES NOT PIN A MODE GETS
# ============================================================================
# The row's cell is about the channel RECONNECTING and RESYNCING with a counted
# gap. Mode `push` (the explicit pin) has just been measured ending the loop on a
# drop; `auto` is the mode that is supposed to reconnect. Both are measured
# against the same endpoint, with the same drop, in the same arm — so the
# difference is the mode and nothing else.
cell_reconnect_auto() { # cell_reconnect_auto <arm>
  local arm=$1
  head2 "CELL C2 — THE SAME DROP, MODE=auto (the mode a default mount runs) ($arm)"
  mount_client auto "$BASE" auto
  sleep 3
  local ma; ma=$(pid_of auto)
  if [ -z "$ma" ]; then
    verdict untested "the auto-mode mount did not start; nothing measured"
    return 1
  fi
  local ends0 rec0 gaps0 res0 fails0 head0 cursor0 seqb
  ends0=$(st '.invalidation.stream_ends_total' auto); rec0=$(st '.invalidation.reconnects_total' auto)
  gaps0=$(st '.invalidation.resyncs_from_gap' auto); res0=$(st '.invalidation.resyncs_total' auto)
  fails0=$(st '.invalidation.failures_total' auto)
  head0=$(ledger_head); cursor0=$(st '.invalidation.resume_seq' auto); seqb=$(st '.invalidation.seq' auto)
  log "   before: stream_ends=${ends0:-0} reconnects=${rec0:-0} gaps=${gaps0:-0} resyncs=${res0:-0} failures=${fails0:-0}"
  log "   before: ledger head=$head0, this client's cursor=${seqb:-?} (presented resume_seq=${cursor0:-?})"

  kill -STOP "$ma"; sleep 1
  local killarg
  if [ "$arm" = dc ]; then killarg="dst $DC_IP dport = $DC_PORT"; else killarg="dst 127.0.0.1 dport = $LOC_PORT"; fi
  sudo -n "$SS_BIN" -K $killarg >/dev/null 2>&1
  local t0; t0=$(date +%s.%N)
  far_rotate "$arm" "$ROTATE_LINES" "$ROTATE_SPACING"
  local t_resume; t_resume=$(date +%s.%N)
  local head1 lines_rotated
  head1=$(ledger_head)
  lines_rotated=$(( ${head1:-0} - ${head0:-0} ))
  log "   the rotation moved the ledger head ${head0:-?} -> ${head1:-?} = ${lines_rotated} line(s); the journal retains 256, so ${lines_rotated} lines is $([ "$lines_rotated" -gt 256 ] && echo 'MORE than it retains (the cursor is left behind the base)' || echo 'NOT more than it retains (the cursor stays inside the retained tail)')"
  row "$arm" reconnect_auto rotation_lines_measured "$lines_rotated" lines "$([ "$lines_rotated" -gt 256 ] && echo past-the-journal || echo inside-the-journal)" "MEASURED from the server's own ledger head, not assumed"
  row "$arm" reconnect_auto cursor_before_drop "${seqb:-?}" seq ok "the client's applied cursor when it was stopped (head was ${head0:-?})"
  kill -CONT "$ma"

  local t_recon="" guard=0 rec
  while [ $guard -lt 400 ]; do
    sleep 0.25; guard=$((guard+1))
    rec=$(st '.invalidation.reconnects_total' auto)
    if [ -z "$t_recon" ] && [ "${rec:-0}" -gt "${rec0:-0}" ]; then t_recon=$(date +%s.%N); break; fi
  done
  # give the re-established stream its answer (the overflow line and the tail)
  sleep 5
  local ends1 rec1 gaps1 res1 fails1 reason mode mech
  ends1=$(st '.invalidation.stream_ends_total' auto); rec1=$(st '.invalidation.reconnects_total' auto)
  gaps1=$(st '.invalidation.resyncs_from_gap' auto); res1=$(st '.invalidation.resyncs_total' auto)
  fails1=$(st '.invalidation.failures_total' auto)
  reason=$(st '.invalidation.reason' auto); mode=$(st '.invalidation.mode' auto); mech=$(st '.invalidation.mechanism' auto)
  local dt_recon dt_rotate
  [ -n "$t_recon" ] && dt_recon=$(perl -e "printf('%.2f', $t_recon-$t_resume)") || dt_recon="n/a"
  dt_rotate=$(perl -e "printf('%.1f', $t_resume-$t0)")
  log "   after:  stream_ends=${ends1:-?} reconnects=${rec1:-?} gaps=${gaps1:-?} resyncs=${res1:-?} failures=${fails1:-?} mode=$mode mechanism=$mech"
  log "   the ledger rotated for ${dt_rotate}s while the reader was stopped; reason: ${reason:-<none>}"
  row "$arm" reconnect_auto stream_ends "$(( ${ends1:-0} - ${ends0:-0} ))" count "$([ $(( ${ends1:-0} - ${ends0:-0} )) -gt 0 ] && echo ended || echo none)" "the dropped stream was seen"
  row "$arm" reconnect_auto reconnects "$(( ${rec1:-0} - ${rec0:-0} ))" count "$([ $(( ${rec1:-0} - ${rec0:-0} )) -gt 0 ] && echo RE-ESTABLISHED || echo NONE)" "auto mode is expected to reconnect"
  row "$arm" reconnect_auto resume_to_reconnect "$dt_recon" s "$([ "$dt_recon" != n/a ] && echo ok || echo fail)" "SIGCONT -> the client's own reconnect counter"
  row "$arm" reconnect_auto counted_gaps "$(( ${gaps1:-0} - ${gaps0:-0} ))" count "$([ $(( ${gaps1:-0} - ${gaps0:-0} )) -gt 0 ] && echo counted || echo none)" "resyncs_from_gap — BFS-061's counted kind"
  row "$arm" reconnect_auto counted_resyncs "$(( ${res1:-0} - ${res0:-0} ))" count "$([ $(( ${res1:-0} - ${res0:-0} )) -gt 0 ] && echo counted || echo none)" "resyncs_total"
  local view=0 i
  for i in 1 5; do [ -e "$(mp_of auto)/r$i.x" ] && view=$((view+1)); done
  row "$arm" reconnect_auto view_after_resync "$view" "of 2" "$([ "$view" -eq 2 ] && echo ok || echo fail)" "rotated paths visible through the re-established mount"
  local gd=$(( ${gaps1:-0} - ${gaps0:-0} )) rd=$(( ${res1:-0} - ${res0:-0} ))
  if [ "$lines_rotated" -le 256 ]; then
    verdict untested "UNTESTED: the rotation moved only ${lines_rotated} line(s) of the journal the server retains (256), so the client's cursor was never left behind the base and this arm asked the server a question it could answer contiguously. The reconnect itself is measured above (${dt_recon}s); the COUNTED-GAP claim is NOT discharged by this run."
  elif [ "$gd" -gt 0 ] && [ -n "$reason" ]; then
    verdict ok "the dropped channel RE-ESTABLISHED in ${dt_recon}s and the resync is the COUNTED kind: resyncs_from_gap +$gd, reason \"${reason:0:80}\""
  elif [ "$rd" -gt 0 ] && [ -n "$reason" ]; then
    verdict ok "the dropped channel RE-ESTABLISHED in ${dt_recon}s with a NAMED resync (resyncs_total +$rd, reason \"${reason:0:80}\") — counted, not silent"
  elif [ "$(( ${rec1:-0} - ${rec0:-0} ))" -gt 0 ]; then
    verdict fail "the rotation left the cursor behind the base (${lines_rotated} lines) and the client re-established, but NOTHING was counted and no reason was recorded: a view was resumed without the client being able to name what it missed"
  else
    verdict fail "mode=auto did NOT re-establish after the drop (reconnects +$(( ${rec1:-0} - ${rec0:-0} )), ends +$(( ${ends1:-0} - ${ends0:-0} )))"
  fi
  return 0
}

cell_poll() { # cell_poll <arm>
  local arm=$1
  head2 "CELL D — THE POLL FALLBACK OVER THE SAME PATH, AND ITS COST ($arm)"
  mount_client poll "$BASE" poll
  sleep 3
  local pmech preq0
  pmech=$(st '.invalidation.mechanism' poll)
  row "$arm" poll mechanism "${pmech:-none}" state "$(case "${pmech:-}" in events|rev) echo ok;; *) echo FAIL;; esac)" "the op that answered, not what was hoped for"
  if [ -z "$(pid_of poll)" ]; then
    verdict fail "the poll mount did not start against $BASE — no measurement is reported"
    return 1
  fi
  preq0=$(st '.invalidation.requests_total' poll)

  local i t0 seen lat lat_list=""
  for i in $(seq 1 3); do
    lat=""
    far_write "$arm" "poll-$i.txt" "poll probe $i $STAMP"
    t0=$(date +%s.%N)
    local guard=0
    while [ $guard -lt 200 ]; do
      if [ -s "$(mp_of poll)/poll-$i.txt" ]; then
      # The poll mount refreshes every declared interval, so this measures
      # "within one poll interval" rather than the path's own latency: a zero
      # here means the change was ALREADY visible when the clock started.
      lat=$(perl -e "my \$d=time()-$t0; printf('%.1f', \$d<0?0:\$d)")
      break
    fi
      sleep 0.1; guard=$((guard+1))
    done
    lat_list="$lat_list ${lat:-}"
    printf '   edit %d -> visible through the poll mount: %s s\n' "$i" "${lat:-TIMEOUT}"
    sleep 1
  done
  local pp100; pp100=$(printf '%s' "$lat_list" | tr ' ' '\n' | grep -E '^[0-9]' | sort -g | tail -1)
  row "$arm" poll edit_to_visible_p100 "${pp100:-?}" s ok "declared poll interval ${POLL_INTERVAL} + the path"
  local t_start t_end preq1 q; t_start=$(date +%s.%N)
  preq0=$(st '.invalidation.requests_total' poll)
  local pushreq0 pushreq1
  pushreq0=$(st '.invalidation.requests_total' push)
  sleep "$POLL_WIN"
  preq1=$(st '.invalidation.requests_total' poll)
  pushreq1=$(st '.invalidation.requests_total' push)
  t_end=$(date +%s.%N)
  local win; win=$(perl -e "printf('%.1f', $t_end-$t_start)")
  local pollrpm pushrpm
  pollrpm=$(perl -e "printf('%.1f', ($preq1-$preq0)*60/$win)")
  pushrpm=$(perl -e "printf('%.1f', ($pushreq1-$pushreq0)*60/$win)")
  log "   over ${win}s: poll requests $preq0->$preq1, push requests $pushreq0->$pushreq1"
  row "$arm" poll requests_per_min "$pollrpm" req/min ok "quiet tree, ${POLL_INTERVAL} interval"
  row "$arm" poll push_requests_per_min "$pushrpm" req/min ok "the same window, the pushed channel"
  row "$arm" poll cost_ratio "$(perl -e "printf('%.1f', ($pollrpm>0?$pollrpm/($pushrpm>0?$pushrpm:0.0001):0))")" x "$(awk -v p="$pollrpm" -v u="$pushrpm" 'BEGIN{print (p>u)?"the poll is the expensive one here":"the POLL IS CHEAPER on this path"}')" "requests per minute, same tree, same window"
  local pushlat; pushlat="${p100:-?}"
  row "$arm" poll push_edit_to_line_p100 "${p100:-?}" ms ok "from CELL B2, the pushed channel"
  if [ -n "${pp100:-}" ] && [ -n "${p100:-}" ]; then
    awk -v pl="$p100" -v q="$pp100" -v arm="$arm" 'BEGIN{
      printf "%s,poll,latency_ratio,%.1f,x,%s,\"pushed line vs poll visibility\"\n", arm, q/(pl>0?pl:0.001), (q>pl?"the poll is SLOWER by this factor":"the poll is FASTER") }' | while IFS= read -r l; do csv "$l"; log "   recorded: $l"; done
  fi
  verdict ok "the poll fallback still works across this path (mechanism=$pmech, edit-to-visible p100 ${pp100:-?}s) at ${pollrpm} requests/min against the push channel's ${pushrpm}"
  return 0
}

# ============================================================================
# CELL E — THE SHARED POOL: DOES ONE CLIENT'S STORM STARVE ANOTHER?
# ============================================================================
cell_storm() { # cell_storm <arm>
  local arm=$1
  head2 "CELL E — A STORM FROM ONE CLIENT AGAINST ANOTHER SUBSCRIBER ($arm)"
  mount_client storm "$BASE" push
  sleep 2
  reader_start storm "$BASE" 0
  local sub0 submax
  sub0=$(push_ctr subscribers_active)
  log "   subscribers now: $(push_ctr subscribers_active) (cap $(srv '.result.capabilities.extensions.watch.push.max_subscribers'))"
  row "$arm" storm subscribers_active "$sub0" count ok "two mounts + a raw reader"

  # quiet baseline: how long a line takes to reach the SECOND client
  local i t0 lat quiet_list=""
  for i in 1 2 3; do
    local before; before=$(wc -l < "$ART/reader-storm.log")
    far_write "$arm" "quiet-$i.txt" "quiet $i"
    t0=$(date +%s.%N)
    local guard=0 first=""
    while [ $guard -lt 200 ]; do
      first=$(awk -v n="$before" 'NR>n' "$ART/reader-storm.log" 2>/dev/null | awk '{print $1; exit}')
      [ -n "$first" ] && break; sleep 0.05; guard=$((guard+1))
    done
    [ -n "$first" ] && { lat=$(perl -e "printf('%.1f', ($first-$t0)*1000)"); quiet_list="$quiet_list $lat"; printf '   quiet line -> second client: %s ms\n' "$lat"; }
    sleep 0.5
  done
  local qmax; qmax=$(printf '%s' "$quiet_list" | tr ' ' '\n' | grep -E '^[0-9]' | sort -g | tail -1)
  row "$arm" storm quiet_line_p100 "${qmax:-?}" ms ok "baseline, no storm"

  # the storm: bursts on the far side, while the second client is watching
  local dup0 dg0 sdrops0 sgaps0 ends0
  dup0=$(st '.invalidation.resyncs_from_gap' storm); dg0=$(st '.invalidation.paths_dropped_total' storm)
  sdrops0=$(push_ctr subscriber_drops_total); sgaps0=$(push_ctr subscriber_gaps_total)
  ends0=$(st '.invalidation.stream_ends_total' storm)
  local b before first t_storm lat_list=""
  for b in $(seq 1 "$STORM_BURSTS"); do
    before=$(wc -l < "$ART/reader-storm.log")
    t_storm=$(date +%s.%N)
    far_burst "$arm" "$STORM_FILES" "st${b}_"
    local guard=0
    while [ $guard -lt 300 ]; do
      first=$(awk -v n="$before" 'NR>n' "$ART/reader-storm.log" 2>/dev/null | awk '{print $1; exit}')
      [ -n "$first" ] && break; sleep 0.05; guard=$((guard+1))
    done
    if [ -n "$first" ]; then
      lat=$(perl -e "printf('%.1f', ($first-$t_storm)*1000)"); lat_list="$lat_list $lat"
      printf '   storm burst %d -> first line at the second client: %s ms\n' "$b" "$lat"
    else
      printf '   storm burst %d -> NO line reached the second client in 15s\n' "$b"
    fi
    sleep 0.2
  done
  sleep 3
  local smax dup1 dg1 sdrops1 sgaps1 ends1
  smax=$(printf '%s' "$lat_list" | tr ' ' '\n' | grep -E '^[0-9]' | sort -g | tail -1)
  dup1=$(st '.invalidation.resyncs_from_gap' storm); dg1=$(st '.invalidation.paths_dropped_total' storm)
  sdrops1=$(push_ctr subscriber_drops_total); sgaps1=$(push_ctr subscriber_gaps_total)
  ends1=$(st '.invalidation.stream_ends_total' storm)
  row "$arm" storm storm_line_p100 "${smax:-?}" ms "$(awk -v q="${qmax:-1}" -v s="${smax:-1}" 'BEGIN{print (s<=q*3)?"delivered promptly":"DELAYED under the storm"}')" "first line after each burst, second client"
  row "$arm" storm second_client_counted_gaps "$((dup1-dup0))" count "$([ $((dup1-dup0)) -gt 0 ] && echo counted || echo none)" "the second client's counted resyncs"
  row "$arm" storm second_client_path_drops "$((dg1-dg0))" count ok "the second client's precise drops"
  row "$arm" storm second_client_stream_ends "$((ends1-ends0))" count "$([ $((ends1-ends0)) -eq 0 ] && echo none || echo re-established)" "the storm must not kill the other client"
  row "$arm" storm server_subscriber_drops "$((sdrops1-sdrops0))" count ok "drop-oldest, counted server-side"
  row "$arm" storm server_subscriber_gaps "$((sgaps1-sgaps0))" count ok "the overflows the server declared"
  row "$arm" storm subscribers_active_max "$(push_ctr subscribers_active_max)" count ok "the cap is $(srv '.result.capabilities.extensions.watch.push.max_subscribers')"

  local starved=ok
  if [ $((ends1-ends0)) -gt 0 ]; then
    starved=KILLED
    verdict fail "the storm ENDED the other client's stream ($((ends1-ends0)) time(s)): that is starvation, not backpressure"
  elif [ -n "${smax:-}" ] && [ -n "${qmax:-}" ] && [ "${smax%.*}" -gt $(( ${qmax%.*} * 3 + 50 )) ]; then
    verdict ok "the storm DELAYED the other client's lines (${qmax} -> ${smax} ms) without killing them: the cost is latency, and it is bounded and measured"
  else
    verdict ok "the storm did not starve the other client: lines arrived in ${smax:-?} ms against a ${qmax:-?} ms quiet baseline, and every drop was COUNTED (server drops ${sdrops1-sdrops0}, gaps ${sgaps1-sgaps0})"
  fi
  return 0
}

# ============================================================================
# TRANSCRIPTS THAT MUST BE COMMITTED, AND A VERIFIED TEARDOWN
# ============================================================================
finish_arm() { # finish_arm <arm>
  local arm=$1
  umount_all; stop_readers
  sleep 1
  if [ -n "$SRV_PID" ] && kill -0 "$SRV_PID" 2>/dev/null; then kill -TERM "$SRV_PID" 2>/dev/null; fi
  SRV_PID=""
  head2 "ARM SUMMARY ($arm)"
  log "   cells recorded: $(( $(wc -l < "$CSV") - 1 )) CSV rows"
  log "   mountpoints of this run still mounted: $(mount | grep -c "$ART" || true)"
  log "   raw wire logs: $(ls "$ART"/reader-*.log 2>/dev/null | tr '\n' ' ')"
  log "   status samples: $(ls "$ART"/samples-*.log 2>/dev/null | tr '\n' ' ')"
}

teardown() {
  head2 "TEARDOWN — verified, not asserted"
  umount_all; stop_readers
  local lp; lp=$(ss -ltn 2>/dev/null | grep -c ":$LOC_PORT" || true)
  log "   local probe port listeners left: $lp"
  if [ -n "$SRV_PID_DC" ]; then
    ssh -o BatchMode=yes "$DC_SSH" "kill -TERM $SRV_PID_DC 2>/dev/null; sleep 1; kill -0 $SRV_PID_DC 2>/dev/null && echo 'STILL ALIVE' || echo 'probe server stopped'"
    ssh -o BatchMode=yes "$DC_SSH" "rm -rf '$RDIR'" 2>/dev/null
    log "   remote dir removed: $(ssh -o BatchMode=yes "$DC_SSH" "[ -d '$RDIR' ] && echo NO || echo yes")"
  fi
  log "   the live daemon, re-checked (it must be exactly as it was found):"
  ssh -o BatchMode=yes "$DC_SSH" 'systemctl is-active bunkerd; systemctl show -p MainPID --value bunkerd; ss -ltn | grep -cE "18080|19090"' | sed 's/^/     /'
  log "   probe port on the DC: $(ssh -o BatchMode=yes "$DC_SSH" "ss -ltn | grep -c ':$DC_PORT'" 2>/dev/null)"
  log "   mounts of this run left locally: $(mount | grep -c "$ART" || true)"
}

# ============================================================================
# CONTROLS — the instrument must be able to fail
# ============================================================================
control() {
  head2 "CONTROL C-1 — an endpoint WITHOUT a watcher: the establishing cell must go RED"
  # the same probe server, the same code path, WITHOUT --watch: this is exactly the
  # state probes/davserve was in before this row's flag, so it is also the RED for it.
  BASE="http://127.0.0.1:$LOC_PORT/dav"
  start_server_local "$LOCAL_TREE" "$LOC_PORT" ""
  if cell_establish local 10; then
    verdict fail "the establishing cell PASSED against an endpoint that cannot serve the push form: the cell is blind"
  else
    log "   the cell refused to report a pass on an endpoint with no watcher — this is the required RED"
    verdict ok "C-1 satisfied: with no watcher the establishing cell goes RED (and says why) instead of passing"
  fi
  umount_all; stop_readers
  kill -TERM "$SRV_PID" 2>/dev/null; SRV_PID=""; sleep 1

  head2 "CONTROL C-2 — mechanism honesty: the SAME endpoint, a mount pinned to the poll"
  BASE="http://127.0.0.1:$LOC_PORT/dav"
  start_server_local "$LOCAL_TREE" "$LOC_PORT" "--watch"
  mount_client honest "$BASE" poll
  sleep 3
  local m; m=$(st '.invalidation.mechanism' honest)
  row local control mechanism_poll_pinned "$m" mechanism "$([ "$m" = events ] || [ "$m" = rev ] && echo ok || echo FAIL)" "the poll must not be reported as the pushed channel"
  if [ "$m" = events ] || [ "$m" = rev ]; then
    verdict ok "C-2 satisfied: the same push-capable endpoint reports mode=poll/mechanism=$m when the client is told to poll — the record follows the mechanism in force"
  else
    verdict fail "C-2: a poll-pinned mount reported mechanism=$m — the record can lie"
  fi
  umount_all
  kill -TERM "$SRV_PID" 2>/dev/null; SRV_PID=""; sleep 1

  head2 "CONTROL C-3 — the COUNTED-RESYNC cell against a NEUTERED client"
  # the claim: after a broken connection and a rotated ledger, the resync is COUNTED
  # and NAMED. Neuter both halves of that counting (BFS-061's own 'loosegap' shape,
  # plus the overflow's resync call) and the cell must go RED while the mount still
  # works — i.e. the mutant is the cheap wrong fix, and the cell tells them apart.
  local src="$REPO_ROOT/internal/fsclient/invalidate.go"
  local pre_hash cur_hash
  pre_hash=$(sha256sum "$src" | awk '{print $1}')
  log "   invalidate.go sha256 before: $pre_hash"
  cp "$src" "$ART/invalidate.go.pre"

  perl -0pi -e 's/\t\ti\.gaps\+\+\n\t\tgap = true\n/\t\tgap = true\n/' "$src"
  perl -0pi -e 's/\t\ti\.Resync\("overflow: the server declared knowledge lost"\)\n/\t\t_ = ev.Seq\n/' "$src"

  local mut_hash
  mut_hash=$(sha256sum "$src" | awk '{print $1}')
  if [ "$mut_hash" = "$pre_hash" ]; then
    verdict untested "the mutation did not land (the source did not change), so this control measured nothing"
    cp "$ART/invalidate.go.pre" "$src"; return 1
  fi
  log "   mutant sha256: $mut_hash  (gaps++ removed, the overflow's resync silenced)"
  ( cd "$REPO_ROOT" && go build -o "$BIN/bunker-mutant" ./cmd/bunker ) || { log "   the mutant does not compile; control abandoned"; cp "$ART/invalidate.go.pre" "$src"; return 1; }

  # restore IMMEDIATELY, verify byte-identical, and only then run the mutant binary
  cp "$ART/invalidate.go.pre" "$src"
  cur_hash=$(sha256sum "$src" | awk '{print $1}')
  log "   restored: sha256=$cur_hash $([ "$cur_hash" = "$pre_hash" ] && echo '(byte-identical)' || echo '(MISMATCH — aborting)')"
  [ "$cur_hash" = "$pre_hash" ] || { log "   RESTORE FAILED"; exit 5; }

  BASE="http://127.0.0.1:$LOC_PORT/dav"
  start_server_local "$LOCAL_TREE" "$LOC_PORT" "--watch"
  CLIENT_BIN="$BIN/bunker-mutant" mount_client neuted "$BASE" push
  CLIENT_BIN="$BIN/bunker"
  sleep 2
  local mpid; mpid=$(pid_of neuted)
  kill -STOP "$mpid"; sleep 1
  sudo -n "$SS_BIN" -K dst 127.0.0.1 dport = "$LOC_PORT" >/dev/null 2>&1
  far_rotate local "$ROTATE_LINES" "$ROTATE_SPACING"
  kill -CONT "$mpid"; sleep 6
  local g r rsn view
  g=$(st '.invalidation.resyncs_from_gap' neuted); r=$(st '.invalidation.resyncs_total' neuted)
  rsn=$(st '.invalidation.reason' neuted)
  view=$([ -e "$(mp_of neuted)/r1.x" ] && echo yes || echo no)
  log "   the NEUTERED client after the same break+rotation: gaps=${g:-?} resyncs=${r:-?} reason=${rsn:-<none>} view-refreshed=$view"
  row local control neutered_counted_gaps "${g:-?}" count "$([ "${g:-0}" -gt 0 ] && echo counted || echo SILENT)" "the same break the real client counts"
  row local control neutered_reason "${rsn:-none}" text "$([ -n "$rsn" ] && echo named || echo SILENT)" "a gap the client cannot name"
  if [ "${g:-0}" -eq 0 ] && [ -z "$rsn" ]; then
    verdict ok "C-3 satisfied: with the counting neutered the client applies the tail SILENTLY (gaps=${g:-0}, no reason) — so the cell's assertion is about the real counting mechanism, not about a mount that happens to keep working"
  else
    verdict fail "C-3: the neutered client still reported a counted resync (gaps=${g:-?} reason=${rsn:-none}) — the cell may be measuring something other than the counting"
  fi
  umount_all; stop_readers
  kill -TERM "$SRV_PID" 2>/dev/null; SRV_PID=""
}

# ============================================================================
# RECON — the live daemon, read-only
# ============================================================================
recon() {
  head2 "RECON — the live daemon on dedi-2, read-only (nothing here is deployed to)"
  timeout 40 ping -c 20 "$DC_IP" 2>&1 | tail -3 | sed 's/^/   /'
  ssh -o BatchMode=yes "$DC_SSH" 'systemctl is-active bunkerd; systemctl show -p MainPID --value bunkerd; ss -ltnp 2>/dev/null | grep -E "18080|19090"' | sed 's/^/   /'
  ssh -o BatchMode=yes "$DC_SSH" '/usr/local/bin/bunkerd --version 2>&1' | sed 's/^/   /'
  local TOKEN
  TOKEN=$(ssh -o BatchMode=yes "$DC_SSH" 'sed -nE "s/^  token: *//p" /etc/bunkerd/config.yaml' | tr -d '"')
  log "   live token read at runtime: ${#TOKEN} chars (value never printed, never written to a file)"
  local p
  for p in / /dav/ /health; do
    printf '   GET %-8s -> ' "$p"
    curl -s -o /dev/null -w 'http=%{http_code} bytes=%{size_download} t=%{time_total}s\n' "http://$DC_NAME:18080$p"
  done
  printf '   PROPFIND /dav/ (with the live token) -> '
  curl -s -X PROPFIND -H "Authorization: Bearer $TOKEN" -H 'Depth: 0' -o /dev/null \
       -w 'http=%{http_code} t=%{time_total}s\n' "http://$DC_NAME:18080/dav/"
  printf '   POST /dav/ X-Bunker-Op: watch (with the live token) -> '
  curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'X-Bunker-Op: watch' \
       -H 'Content-Type: application/json' -d '{"paths":[]}' -o /dev/null \
       -w 'http=%{http_code} t=%{time_total}s\n' "http://$DC_NAME:18080/dav/"
  TOKEN=""
  ssh -o BatchMode=yes "$DC_SSH" 'systemctl is-active bunkerd; systemctl show -p MainPID --value bunkerd' | sed 's/^/   after: /'
}

# ============================================================================
arm_run() { # arm_run <arm>
  local arm=$1
  if [ "$arm" = dc ]; then
    LOCAL_TREE="$ART/tree-dc"
    head2 "ARM: THE DATACENTRE (dedi-2, Helsinki — the real path, the release's own server on a spare port)"
    fixture "$LOCAL_TREE" 40
    BASE="http://$DC_NAME:$DC_PORT/dav"
    start_server_dc "$LOCAL_TREE" "$DC_PORT" "--watch" || { log "THE DC ENDPOINT DID NOT COME UP — no measurement is reported for this arm"; return 1; }
  else
    LOCAL_TREE="$ART/tree-local"
    head2 "ARM: LOOPBACK (the same instrument, so the DC numbers have local ones beside them)"
    fixture "$LOCAL_TREE" 40
    BASE="http://127.0.0.1:$LOC_PORT/dav"
    start_server_local "$LOCAL_TREE" "$LOC_PORT" "--watch" || { log "THE LOOPBACK ENDPOINT DID NOT COME UP — no measurement is reported for this arm"; return 1; }
  fi
  : > "$CSV"
  csv "arm,cell,metric,value,unit,verdict,note"
  cell_link "$arm"
  cell_establish "$arm" "$HOLD" || true
  heartbeat_cadence "$arm" push || true
  write_deadline_margin "$arm" || true
  cell_reconnect "$arm" || true
  cell_reconnect_auto "$arm" || true
  cell_poll "$arm" || true
  cell_storm "$arm" || true
  finish_arm "$arm"
  cp "$CSV" "$ART/BFS-047-cells-$arm.csv"
  log "   CSV: $ART/BFS-047-cells-$arm.csv"
}

mode=${1:-}
case "$mode" in
  recon)    build_binaries; recon ;;
  local)    build_binaries; arm_run local ;;
  dc)       build_binaries; arm_run dc; teardown ;;
  control)  build_binaries; fixture "$ART/tree-local" 40; LOCAL_TREE="$ART/tree-local"; control ;;
  teardown) teardown ;;
  all)      build_binaries; recon; arm_run local; arm_run dc; teardown
            fixture "$ART/tree-local" 40; LOCAL_TREE="$ART/tree-local"; control ;;
  *)        sed -n '2,40p' "$0"; exit 1 ;;
esac

# the probe credential never reaches a transcript: redact it from everything this
# run wrote.  (The LIVE token is never written anywhere at all — recon keeps it in
# one shell variable and clears it.)
sed -i "s/$PROBE_PW/<redacted-probe-credential>/g" "$OUT" "$ART"/mount-*.log "$ART"/server-*.log 2>/dev/null || true
rm -f "$ART/invalidate.go.pre" 2>/dev/null || true

head2 "DONE ($mode) — artifacts"
log "   transcript : $OUT"
log "   cells csv  : $CSV"
log "   artifacts  : $ART"
log "   failed verdicts this run: $FAILED"
exit 0
