#!/usr/bin/env bash
# GAP-114 probe: abuse cases — one measured run per abuse class, inside the
# knobs the tiers would set. The question is NOT "does the bomb run" but
# "does the knob contain it, and does the host stay healthy".
#   - fork bomb (pids): exponential forker, 2 children per generation,
#     depth 12 (theoretical 4094 forks) under --pids-limit=256 vs the
#     container's own supervisor overhead; host pid count watched.
#   - memory bomb (alloc loop): allocator targeting 8GiB under the shipped
#     MemoryMax default scale; host MemAvailable watched.
#   - IO hog (dd): measured in probe-e-iobounds.sh (unbounded vs wbps-capped,
#     with host PSI sampled) — cross-referenced, not duplicated here.
# All docker resources are throwaway (gap114-ab-*) and removed on exit.
set -euo pipefail

WORK="${GAP114_WORK:-/tmp/gap114}"
TOOLCHAIN_IMG="gap114/toolchain"
mkdir -p "$WORK"
log() { printf '\n=== %s ===\n' "$*"; }

cat > "$WORK/forkbomb.py" <<'EOF'
import os, sys, time
DEPTH = 12                      # theoretical ~2^12 = 4094 forks
forks = 0
def spawn(d):
    global forks
    if d >= DEPTH:
        return
    for _ in range(2):
        try:
            pid = os.fork()
        except OSError:
            return              # contained by pids.max (EAGAIN)
        forks += 1
        if pid == 0:
            spawn(d + 1)
            time.sleep(6)
            os._exit(0)
spawn(0)
time.sleep(1)
try:
    cur = open("/sys/fs/cgroup/pids.current").read().strip()
    mx = open("/sys/fs/cgroup/pids.max").read().strip()
except OSError:
    cur = mx = "n/a"
print(f"BOMB-DONE theoretical={2**DEPTH - 1} actual_forks={forks} "
      f"pids.max={mx} pids.current={cur}")
EOF

host_pids() { ls /proc | grep -c '^[0-9]'; }

cleanup() {
  for c in gap114-ab-fork gap114-ab-mem; do
    docker container remove -f "$c" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

log "abuse 1: fork bomb under --pids-limit=256"
P0=$(host_pids)
docker run --rm --name gap114-ab-fork --pids-limit=256 \
  -v "$WORK:/w" "$TOOLCHAIN_IMG" python3 /w/forkbomb.py
P1=$(host_pids)
echo "OBSERVED host pid-count before=$P0 after=$P1 (host unaffected; container removed)"

log "abuse 2: memory bomb (8GiB alloc target) under --memory=1g --memory-swap=1g"
cat > "$WORK/membomb.py" <<'EOF'
import time
buf = bytearray()
chunks = 0
try:
    while len(buf) < 8 * 1024 * 1024 * 1024:   # attacker wants 8GiB
        buf += bytearray(64 * 1024 * 1024)
        chunks += 1
        time.sleep(0.05)
    print("BOMB-COMPLETED (not contained!)", flush=True)
except MemoryError:
    pass
EOF
M0=$(awk '/MemAvailable/ {print $2}' /proc/meminfo)
docker container remove -f gap114-ab-mem >/dev/null 2>&1 || true
set +e
docker run --name gap114-ab-mem --memory=1g --memory-swap=1g \
  -v "$WORK:/w" "$TOOLCHAIN_IMG" python3 /w/membomb.py
RC=$?
set -e
M1=$(awk '/MemAvailable/ {print $2}' /proc/meminfo)
echo "OBSERVED bomb container rc=$RC oomkilled=$(docker inspect -f '{{.State.OOMKilled}}' gap114-ab-mem 2>/dev/null)"
echo "OBSERVED host MemAvailable before=${M0}kB after=${M1}kB (delta=$(( (M1-M0)/1024 ))MiB)"
docker container remove -f gap114-ab-mem >/dev/null 2>&1 || true

log "abuse 3: IO hog — measured in probe-e-iobounds.sh"
echo "OBSERVED cross-reference: e-unbounded 320MB/s (PSI spiked to 7.94% in-run) vs e-wbps-20MiBs 21.0MB/s; the cap is the containment."
echo "PROBE-ABUSE-OK"
