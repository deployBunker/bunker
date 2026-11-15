#!/usr/bin/env bash
# DF-BUNKER-122 evidence — `bunkerd --config <path that does not exist>`.
#
# Re-runnable. Writes every arm's stdout+stderr and exit code into ./out/.
# No product code is modified; nothing outside a scratch dir is touched.
#
# Usage:  bash probe.sh [scratch-dir]
# The scratch dir defaults to a fresh mktemp -d (the daemon arm needs a
# writable state tree so it does not touch /var/lib/bunkerd).
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/out"
SCRATCH="${1:-$(mktemp -d)}"
mkdir -p "$OUT" "$SCRATCH"
BUNKERD="$(command -v bunkerd || echo /home/kara/.local/bin/bunkerd)"

say() { printf '\n=== %s\n' "$*"; }

run_arm() { # run_arm <name> <cmd...>
  local name="$1"; shift
  say "$name"
  timeout 20 "$@" >"$OUT/$name.out" 2>&1
  local rc=$?
  printf 'rc=%s\n' "$rc" >>"$OUT/$name.out"
  tail -n 20 "$OUT/$name.out"
}

# A config that is valid but whose pool CANNOT hold its max_agents, so a real
# read must refuse the boot. This is the discriminator: identical command, one
# character different in the path.
cat >"$SCRATCH/overcommit.yaml" <<'YAML'
server:
  grpc_addr: 127.0.0.1:50051
  rest_addr: 127.0.0.1:8080
auth:
  enabled: false
agent:
  max_agents: 400
  port_range_start: 30000
  port_range_end: 30999
  port_range_per_agent: 100
YAML

# 1. Control: the file IS read, so the refusal is real.
run_arm 01-portpool-real-overcommit "$BUNKERD" --config "$SCRATCH/overcommit.yaml" port-pool check
echo "RC01=see file" >>/dev/null

# 2. The defect: one-character typo in the same path.
run_arm 02-portpool-typo-path "$BUNKERD" --config "$SCRATCH/overcomit.yaml" port-pool check

# 3. A path that is a directory: reported honestly (stat succeeds, read fails).
run_arm 03-portpool-directory "$BUNKERD" --config "$SCRATCH" port-pool check

# 4. A readable file with broken YAML: reported honestly.
printf 'auth:\n  enabled: [1,2\n' >"$SCRATCH/malformed.yaml"
run_arm 04-portpool-malformed "$BUNKERD" --config "$SCRATCH/malformed.yaml" port-pool check

# 5. Daemon start against the typo'd path with a token supplied by env:
#    the daemon reports "bunkerd config loaded" although the file was never read.
say "05-daemon-start-typo-path"
(
  export BUNKERD_AUTH_TOKEN=scratchtok-not-a-real-credential
  export BUNKERD_AGENT_BASE_DATA_DIR="$SCRATCH/data"
  export BUNKERD_SERVER_GRPC_ADDR=127.0.0.1:48901
  export BUNKERD_SERVER_REST_ADDR=127.0.0.1:48902
  timeout 8 "$BUNKERD" --config "$SCRATCH/absent.yaml" 2>&1
) >"$OUT/05-daemon-start-typo-path.out" 2>&1
printf 'rc=%s\n' "$?" >>"$OUT/05-daemon-start-typo-path.out"
grep -E 'config loaded|refusing to start|registry unavailable|GENERATED' "$OUT/05-daemon-start-typo-path.out" | head -5

# 6. The same daemon invocation against the REAL config, for contrast: it must
#    refuse at boot (pool overcommit) BEFORE any listener binds.
say "06-daemon-start-real-overcommit"
timeout 8 "$BUNKERD" --config "$SCRATCH/overcommit.yaml" >"$OUT/06-daemon-start-real-overcommit.out" 2>&1
printf 'rc=%s\n' "$?" >>"$OUT/06-daemon-start-real-overcommit.out"
tail -3 "$OUT/06-daemon-start-real-overcommit.out"

say "wrote $OUT"
