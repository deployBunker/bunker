#!/usr/bin/env bash
# dogfood 2026-11-15 (run 33) — containment disclosure real-use arm.
# Isolation: own loopback ports (19221/19222), own data/ssh/registry/audit,
# reconciliation unproven_orphan_limit 0 so the walk can destroy NOTHING.
set -u
RUN=/root/df1115
BIN="$RUN/bin"
CFG="$RUN/bunkerd.yaml"
CCFG="$RUN/cli-home/config.yaml"
EV="$RUN/evidence"
AGENT_NAME=df1115-marker
export BUNKER_HOME="$RUN/cli-home" BUNKER_ALLOW_NO_TOKEN=1
CD="$BIN/bunker"

write_cfg() { # $1 = disclosure true|false
  cat > "$CFG" <<YAML
server:
  name: df1115-containment
  grpc_addr: "127.0.0.1:19222"
  rest_addr: "127.0.0.1:19221"
  request_timeout: 300s
auth:
  enabled: false
tls:
  insecure_dev: true
containment:
  disclosure: $1
agent:
  base_data_dir: $RUN/data
  ssh_dir: $RUN/ssh
  max_agents: 2
  default_cpu_quota: 2.0
  default_memory_bytes: 4294967296
  port_range_start: 32400
  port_range_end: 32499
  port_range_per_agent: 20
  registry:
    enabled: true
    path: $RUN/agents.jsonl
    max_bytes: 1048576
    max_backups: 2
  reconciliation:
    mode: adopt
    unproven_orphan_limit: 0
  destroy_home_policy: archive
  destroy_archive_dir: $RUN/archives
  destroy_archive_keep: 2
audit:
  enabled: true
  path: $RUN/audit.log
YAML
}

start_daemon() { # $1 = disclosure
  pkill -f "df1115/bunkerd.yaml" 2>/dev/null
  for i in $(seq 1 20); do pgrep -f "df1115/bunkerd.yaml" >/dev/null || break; sleep 0.5; done
  write_cfg "$1"
  cd "$RUN" || exit 1
  nohup "$BIN/bunkerd" -c "$CFG" >> "$RUN/bunkerd.log" 2>&1 &
  for i in $(seq 1 60); do
    c=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19221/healthz)
    [ "$c" = "200" ] && { echo "daemon_up disclosure=$1 healthz=200"; return 0; }
    sleep 0.5
  done
  echo "daemon_up disclosure=$1 FAILED"; tail -5 "$RUN/bunkerd.log"; return 1
}

setup() {
  mkdir -p "$BIN" "$RUN/data" "$RUN/ssh" "$RUN/archives" "$RUN/cli-home" "$EV"
  cat > "$CCFG" <<YAML
active_server: df1115-containment
servers:
  df1115-containment:
    name: df1115-containment
    url: http://127.0.0.1:19221
    token: ""
    tls_insecure: false
YAML
  printf 'echo "SCRIPT_ENV=${BUNKER_SANDBOX:-unset}"\necho script-ok\n' > "$RUN/probe-script.sh"
  "$BIN/bunker" --version
}

arm() { # label -- args...
  local label=$1; shift
  local out="$EV/$label.out" err="$EV/$label.err"
  local s e ms rc m
  s=$(date +%s%N)
  "$@" > "$out" 2> "$err"; rc=$?
  e=$(date +%s%N); ms=$(( (e - s) / 1000000 ))
  m=$(grep -cF 'managed sandbox environment' "$out" 2>/dev/null || true)
  printf 'ARM|%-26s|rc=%-3s|ms=%-6s|marker=%s|last=%s\n' "$label" "$rc" "$ms" "${m:-0}" "$(tail -1 "$out" | cut -c1-110)"
}

spawn_agent() {
  local s e ms
  s=$(date +%s%N)
  /usr/bin/time -f "spawn_elapsed=%e" "$BIN/bunker" exec --help >/dev/null 2>&1
  "$BIN/bunker" spawn "$AGENT_NAME" --server df1115-containment --ttl 45m --cpu 1.5 --memory 3221225472 > "$EV/spawn.out" 2> "$EV/spawn.err"
  rc=$?
  e=$(date +%s%N); ms=$(( (e - s) / 1000000 ))
  echo "SPAWN rc=$rc ms=$ms"
  tail -6 "$EV/spawn.out"
  [ $rc -ne 0 ] && tail -10 "$EV/spawn.err"
}

agent_id() {
  "$BIN/bunker" list --server df1115-containment 2>/dev/null | awk -v n="$AGENT_NAME" '$1 == n {print $1; exit}'
}

teardown() {
  local id; id=$(agent_id)
  echo "TEARDOWN agent=$id"
  [ -n "$id" ] && "$BIN/bunker" destroy "$id" --server df1115-containment --force > "$EV/destroy.out" 2>&1
  tail -3 "$EV/destroy.out" 2>/dev/null
  pkill -f "df1115/bunkerd.yaml" 2>/dev/null
  sleep 1
  pgrep -f "df1115/bunkerd.yaml" >/dev/null && echo "daemon_still_up" || echo "daemon_stopped"
}

case "${1:-}" in
  setup) setup ;;
  start) start_daemon "${2:-false}" ;;
  spawn) spawn_agent ;;
  arms) # run the probe battery against the current daemon config
    id=$(agent_id)
    echo "AGENT=$id"
    [ -z "$id" ] && { echo "NO_AGENT"; exit 2; }
    E=(exec --config "$CCFG" --server df1115-containment)
    arm "shell_env"            "$CD" "${E[@]}" "$id" -- 'env | grep -F BUNKER_SANDBOX'
    arm "uname_shell"          "$CD" "${E[@]}" "$id" -- uname -a
    arm "uname_abs"            "$CD" "${E[@]}" "$id" -- /usr/bin/uname -r
    arm "uname_tmp_copy"       "$CD" "${E[@]}" "$id" -- /tmp/uname-df1115 -r
    arm "uname_pipe"           "$CD" "${E[@]}" "$id" -- 'uname -a | head -1'
    arm "uname_semicolon"      "$CD" "${E[@]}" "$id" -- 'uname -a; whoami'
    arm "cat_osrelease"        "$CD" "${E[@]}" "$id" -- 'cat /etc/os-release'
    arm "cat_passwd"           "$CD" "${E[@]}" "$id" -- 'cat /etc/passwd'
    arm "cat_n_osrelease"      "$CD" "${E[@]}" "$id" -- 'cat -n /etc/os-release'
    arm "cat_nsv_osrelease"    "$CD" "${E[@]}" "$id" -- 'cat -nsv /etc/os-release'
    arm "df_operand"           "$CD" "${E[@]}" "$id" -- 'df /etc'
    arm "free_badflag"         "$CD" "${E[@]}" "$id" -- 'free -x'
    arm "lsbrelease_missing"   "$CD" "${E[@]}" "$id" -- lsb_release
    arm "lsbrelease_a"         "$CD" "${E[@]}" "$id" -- lsb_release -a
    arm "raw_env"              "$CD" exec --config "$CCFG" --server df1115-containment --raw "$id" -- /usr/bin/env
    arm "raw_uname"            "$CD" exec --config "$CCFG" --server df1115-containment --raw "$id" -- /usr/bin/uname -a
    arm "script_upload"        "$CD" exec --config "$CCFG" --server df1115-containment --script "$RUN/probe-script.sh" "$id"
    arm "echo_plain"           "$CD" "${E[@]}" "$id" -- 'echo hello'
    arm "detach_run"           "$CD" run --config "$CCFG" --server df1115-containment --detach "$id" -- sh -c 'echo DETACHED=${BUNKER_SANDBOX:-unset} > /tmp/df1115-detach.txt'
    sleep 3
    arm "detach_read"          "$CD" "${E[@]}" "$id" -- 'cat /tmp/df1115-detach.txt'
    ;;
  teardown) teardown ;;
  *) echo "usage: $0 setup|start <true|false>|spawn|arms|teardown" ;;
esac
