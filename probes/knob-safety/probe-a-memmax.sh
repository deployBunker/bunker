#!/usr/bin/env bash
# GAP-114 probe (a): MemoryMax too tight -> OOM-kill mid build-like workload.
#
# Runs a deliberate allocator that peaks at a realistic "compile-like" RSS,
# under docker -m at several tightness multiples, and records:
#   - container exit code + OOMKilled flag
#   - cgroup memory.peak (kernel-measured high-water RSS) for the run
#   - wall time
# then reports the headroom multiple that keeps the workload alive.
#
# Knob mapping: docker -m == systemd MemoryMax= (cgroup v2 memory.max).
# All docker resources are throwaway (gap114-* names) and removed on exit.
set -euo pipefail

WORK="${GAP114_WORK:-/tmp/gap114}"
TOOLCHAIN_IMG="gap114/toolchain"
RUNNER="gap114-a-runner"
PEAK_MB="${1:-300}"   # workload target peak RSS in MiB

mkdir -p "$WORK"

log() { printf '\n=== %s ===\n' "$*"; }

# --- one-time toolchain image (tiny; reused by the other probes) -------------
if ! docker image inspect "$TOOLCHAIN_IMG" >/dev/null 2>&1; then
  log "building toolchain image"
  docker build -t "$TOOLCHAIN_IMG" -f- "$WORK" <<'EOF'
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends gcc python3 && rm -rf /var/lib/apt/lists/*
EOF
fi

# --- workload: python allocator ramping to PEAK_MB then "linking" (hold) -----
cat > "$WORK/run_in_mem.py" <<'EOF'
import sys, time, os
target = int(sys.argv[1])          # MiB
step = 8                           # MiB per 50ms allocation step
buf = bytearray()
t0 = time.time()
while len(buf) < target * 1024 * 1024:
    buf += bytearray(step * 1024 * 1024)
    time.sleep(0.05)
hold = time.time() - t0
time.sleep(3)                      # emulate the link/hold phase at peak RSS
def cgread(p):
    try: return open(p).read().strip()
    except OSError: return "n/a"
peak = cgread("/sys/fs/cgroup/memory.peak")
events = cgread("/sys/fs/cgroup/memory.events")
print(f"WLK-DONE peak_target={target}MiB ramp={hold:.1f}s pid={os.getpid()}")
print(f"WLK-PEAK memory.peak={peak} memory.events={events}")
EOF

run_one() {  # $1 = label, $2 = -m value (empty = unlimited), $3 = peak MB
  local label="$1" memflag="$2" peak="$3" rc=0
  docker container remove -f "$RUNNER" >/dev/null 2>&1 || true
  log "run: $label (peak=${peak}MiB, memflag='${memflag:-none}')"
  local t0=$(date +%s.%N)
  # The run is EXPECTED to fail (exit 137 / OOMKilled) at tight limits — that is
  # the evidence. Any OTHER failure must fail this probe loudly.
  set +e
  docker run --name "$RUNNER" $memflag \
    -v "$WORK:/w" "$TOOLCHAIN_IMG" python3 /w/run_in_mem.py "$peak"
  rc=$?
  set -e
  local t1=$(date +%s.%N)
  local wall=$(python3 -c "print(f'{$t1-$t0:.1f}')")
  local oomkilled="false"
  oomkilled=$(docker inspect -f '{{.State.OOMKilled}}' "$RUNNER" 2>/dev/null || echo unknown)
  # kernel-measured peak for this container's cgroup (read BEFORE removal)
  local cid
  cid=$(docker inspect -f '{{.Id}}' "$RUNNER" 2>/dev/null || echo "")
  local cgpeak="n/a"
  if [ -n "$cid" ] && [ -r "/sys/fs/cgroup/system.slice/docker-${cid}.scope/memory.peak" ]; then
    cgpeak=$(cat "/sys/fs/cgroup/system.slice/docker-${cid}.scope/memory.peak")
  fi
  printf 'OBSERVED label=%s exit=%s oomkilled=%s wall=%ss cgroup.memory.peak=%s\n' \
    "$label" "$rc" "$oomkilled" "$wall" "$cgpeak"
  # Loud-fail contract: only exit-137/OOMKilled or clean-0 are recognized outcomes.
  if [ "$rc" -ne 0 ] && [ "$rc" -ne 137 ]; then
    echo "PROBE-FAIL: unexpected failure (exit=$rc) for $label" >&2
    docker logs "$RUNNER" 2>&1 | tail -20 || true
    exit 1
  fi
  echo "$label $rc" >> "$WORK/probe-a-results.txt"
}

rm -f "$WORK/probe-a-results.txt"
log "context"
printf 'OBSERVED host mem_avail='; awk '/MemAvailable/ {printf "%s kB\n", $2}' /proc/meminfo
docker version --format 'OBSERVED docker-server={{.Server.Version}}'

# tightness multiples: 0.75x, 1.0x, 1.25x, 1.5x of the workload peak
run_one "a-tight-075x" "--memory=$(( PEAK_MB * 3 / 4 ))m --memory-swap=$(( PEAK_MB * 3 / 4 ))m" "$PEAK_MB"
run_one "a-tight-100x" "--memory=${PEAK_MB}m --memory-swap=${PEAK_MB}m"                     "$PEAK_MB"
run_one "a-tight-125x" "--memory=$(( PEAK_MB * 5 / 4 ))m --memory-swap=$(( PEAK_MB * 5 / 4 ))m" "$PEAK_MB"
run_one "a-tight-150x" "--memory=$(( PEAK_MB * 3 / 2 ))m --memory-swap=$(( PEAK_MB * 3 / 2 ))m" "$PEAK_MB"

log "verdict data"
column -t "$WORK/probe-a-results.txt" 2>/dev/null || cat "$WORK/probe-a-results.txt"
docker container remove -f "$RUNNER" >/dev/null 2>&1 || true
echo "PROBE-A-OK"
