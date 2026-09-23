#!/usr/bin/env bash
# GAP-114 probe (c): MemoryHigh — graceful throttle (slows, no kill)?
#
# Two mechanisms measured:
#   1. docker --memory-reservation: the docker-flag "equivalent". Mechanism
#      check: does it set cgroup-v2 memory.high at all?
#   2. systemd MemoryHigh= on a user-level transient unit: the knob
#      GAP-119 would actually set. Measure wall-time delta vs unbounded,
#      kernel throttle counter (memory.events: high), survival, peak.
#
# Knob mapping: systemd MemoryHigh= == cgroup v2 memory.high.
# All docker resources are throwaway (gap114-c-*) and removed on exit.
set -euo pipefail

WORK="${GAP114_WORK:-/tmp/gap114}"
TOOLCHAIN_IMG="gap114/toolchain"
RUNNER="gap114-c-runner"
PEAK_MB="${1:-300}"

mkdir -p "$WORK"
log() { printf '\n=== %s ===\n' "$*"; }

# Workload: fast allocator ramping to target MiB; reads its OWN cgroup files
# (relative to /proc/self/cgroup) so the same code is correct both inside a
# docker cgroup namespace and under a systemd user unit (host cgroupfs).
cat > "$WORK/run_in_mem.py" <<'EOF'
import sys, time, os
target = int(sys.argv[1])          # MiB
buf = bytearray()
t0 = time.time()
while len(buf) < target * 1024 * 1024:
    buf += bytearray(16 * 1024 * 1024)
    time.sleep(0.01)
hold = time.time() - t0
rel = open("/proc/self/cgroup").read().strip().splitlines()[0].split("::")[1]
def cg(name):
    try: return open("/sys/fs/cgroup" + rel + "/" + name).read().strip().replace("\n", " ")
    except OSError as e: return f"n/a({e.errno})"
print(f"WLK-DONE peak_target={target}MiB ramp={hold:.1f}s pid={os.getpid()}")
print(f"WLK-CG rel={rel}")
print(f"WLK-CG memory.high={cg('memory.high')} memory.peak={cg('memory.peak')}")
print(f"WLK-CG memory.events={cg('memory.events')}")
EOF

run_docker() {  # $1 label, extra args = docker flags
  local label="$1"; shift
  docker container remove -f "$RUNNER" >/dev/null 2>&1 || true
  log "docker run: $label (flags: $*)"
  local t0=$(date +%s.%N)
  set +e
  docker run --name "$RUNNER" "$@" \
    -v "$WORK:/w" "$TOOLCHAIN_IMG" python3 /w/run_in_mem.py "$PEAK_MB"
  local rc=$?
  set -e
  local t1=$(date +%s.%N)
  printf 'OBSERVED label=%s exit=%s wall=%ss\n' \
    "$label" "$rc" "$(python3 -c "print(f'{$t1-$t0:.1f}')")"
  [ "$rc" -eq 0 ] || { echo "PROBE-FAIL docker rc=$rc" >&2; exit 1; }
}

rm -f "$WORK/probe-c-results.txt"
log "context"
docker version --format 'OBSERVED docker-server={{.Server.Version}}'
systemctl --user --version | sed -n 1p

log "part1: docker --memory-reservation mechanism check"
docker container remove -f "$RUNNER" >/dev/null 2>&1 || true
docker run -d --name "$RUNNER" --memory=2g --memory-reservation=100m \
  "$TOOLCHAIN_IMG" sleep 60 >/dev/null
CID=$(docker inspect -f '{{.Id}}' "$RUNNER")
printf 'OBSERVED in-container memory.high (reservation=100m requested): '
docker exec "$RUNNER" cat /sys/fs/cgroup/memory.high
printf 'OBSERVED same file, host view: '
cat "/sys/fs/cgroup/system.slice/docker-${CID}.scope/memory.high"
printf 'OBSERVED host view memory.max: '
cat "/sys/fs/cgroup/system.slice/docker-${CID}.scope/memory.max"
docker container remove -f "$RUNNER" >/dev/null 2>&1 || true

log "part2: docker --memory-reservation -> does the workload even slow down?"
run_docker "c-docker-res-none" --memory=2g
run_docker "c-docker-res-100m" --memory=2g --memory-reservation=100m

log "part3: systemd MemoryHigh= (the knob that will ship)"
run_unit() {  # $1 label, $2 MemoryHigh value or 'none'
  local label="$1" mh="$2"
  local u="gap114-c-$(echo "$label" | tr -c 'a-zA-Z0-9' '-')"
  local args=(--user -u "$u" --wait -p MemoryMax=1G -p IOSchedulingClass=idle)
  [ "$mh" != "none" ] && args+=(-p "MemoryHigh=$mh")
  log "unit run: $label (MemoryHigh=$mh)"
  local t0=$(date +%s.%N)
  set +e
  systemd-run "${args[@]}" \
    python3 "$WORK/run_in_mem.py" "$PEAK_MB" 2>&1
  local rc=$?
  set -e
  local t1=$(date +%s.%N)
  printf 'OBSERVED unit=%s exit=%s wall=%ss\n' \
    "$label" "$rc" "$(python3 -c "print(f'{$t1-$t0:.1f}')")"
  # workload stdout went to the user journal (services don't inherit the tty)
  journalctl --user -u "$u.service" --no-pager -o cat 2>/dev/null | grep '^WLK-' || true
  echo "$label exit=$rc" >> "$WORK/probe-c-results.txt"
}
run_unit "c-unit-high-none" "none"
run_unit "c-unit-high-250m" "250M"
run_unit "c-unit-high-150m" "150M"
run_unit "c-unit-high-80m"  "80M"

log "verdict data"
cat "$WORK/probe-c-results.txt"
docker container remove -f "$RUNNER" >/dev/null 2>&1 || true
echo "PROBE-C-OK"
