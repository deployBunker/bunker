#!/usr/bin/env bash
# GAP-114 probe (b): MemorySwapMax=0 equivalent — docker --memory-swap == --memory
# (swap allowance = memory-swap minus memory; equal values bar swap entirely).
#
# Question: does a build-like workload still complete when swap absorbs the
# peak, and where exactly does swap-barred execution die? Quantify wall time
# and kernel swap counters (memory.swap.current / memory.swap.peak).
#
# Knob mapping: docker --memory-swap=--memory == systemd MemorySwapMax=0.
# All docker resources are throwaway (gap114-b-*) and removed on exit.
set -euo pipefail

WORK="${GAP114_WORK:-/tmp/gap114}"
TOOLCHAIN_IMG="gap114/toolchain"
RUNNER="gap114-b-runner"
PEAK_MB="${1:-300}"

mkdir -p "$WORK"

log() { printf '\n=== %s ===\n' "$*"; }

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
    try: return open(p).read().strip().replace("\n", " ")
    except OSError: return "n/a"
print(f"WLK-DONE peak_target={target}MiB ramp={hold:.1f}s pid={os.getpid()}")
print(f"WLK-PEAK memory.peak={cgread('/sys/fs/cgroup/memory.peak')} "
      f"swap.current={cgread('/sys/fs/cgroup/memory.swap.current')} "
      f"swap.peak={cgread('/sys/fs/cgroup/memory.swap.peak')}")
print(f"WLK-EV memory.events={cgread('/sys/fs/cgroup/memory.events')}")
EOF

run_one() {  # $1 label, $2 memory, $3 memory-swap
  local label="$1" mem="$2" memswap="$3" rc=0
  docker container remove -f "$RUNNER" >/dev/null 2>&1 || true
  log "run: $label (memory=${mem}m memory-swap=${memswap}m, peak=${PEAK_MB}MiB)"
  local t0=$(date +%s.%N)
  set +e
  docker run --name "$RUNNER" --memory=${mem}m --memory-swap=${memswap}m \
    -v "$WORK:/w" "$TOOLCHAIN_IMG" python3 /w/run_in_mem.py "$PEAK_MB"
  rc=$?
  set -e
  local t1=$(date +%s.%N)
  local wall=$(python3 -c "print(f'{$t1-$t0:.1f}')")
  local oomkilled
  oomkilled=$(docker inspect -f '{{.State.OOMKilled}}' "$RUNNER" 2>/dev/null || echo unknown)
  printf 'OBSERVED label=%s exit=%s oomkilled=%s wall=%ss\n' "$label" "$rc" "$oomkilled" "$wall"
  if [ "$rc" -ne 0 ] && [ "$rc" -ne 137 ]; then
    echo "PROBE-FAIL: unexpected failure (exit=$rc) for $label" >&2
    docker logs "$RUNNER" 2>&1 | tail -20 || true
    exit 1
  fi
  echo "$label exit=$rc wall=${wall}s" >> "$WORK/probe-b-results.txt"
}

rm -f "$WORK/probe-b-results.txt"
log "context"
docker version --format 'OBSERVED docker-server={{.Server.Version}}'
printf 'OBSERVED host SwapTotal='; awk '/SwapTotal/ {print $2 " kB"}' /proc/meminfo

# 1.0x of workload peak, swap barred (MemorySwapMax=0 equivalent) — expect OOM.
run_one "b-swapbarred-100x" 300 300
# 1.0x with 300MiB swap allowance — does swap absorb the ~17MiB overshoot?
run_one "b-swapallowed-100x" 300 600
# 0.5x with 300MiB swap allowance — heavy swap use: complete, and at what cost?
run_one "b-swapallowed-050x" 150 450
# 0.5x swap barred — control: must die.
run_one "b-swapbarred-050x" 150 150

log "verdict data"
cat "$WORK/probe-b-results.txt"
docker container remove -f "$RUNNER" >/dev/null 2>&1 || true
echo "PROBE-B-OK"
