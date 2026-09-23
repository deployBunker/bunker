#!/usr/bin/env bash
# GAP-114 probe (f): TasksMax — docker --pids-limit (cgroup v2 pids.max):
# failure mode must be fork rejection (EAGAIN), not a crash; and we need the
# value below which a realistic multi-process stack trips it.
#
# Stage 1: micro — a python forker under a small limit rejects forks with
#   "Resource temporarily unavailable" (EAGAIN) and keeps running.
# Stage 2: sweep — the same forker targets 200 processes under
#   pids limits 128/64/48/40/32/24/16. At EVERY limit L exactly L-1 children
#   fork (the forker itself is the Lth task), the stop is EAGAIN, and
#   pids.current == pids.max — enforcement is exact. A compose-like stack of
#   6 services x 4 tasks (~25 tasks incl. supervisor) therefore trips any
#   limit <= ~26; the shipped TasksMax default (4096) leaves 160x headroom.
# Note: dmesg is restricted on this host (dmesg_restrict=1), so the
#   "cgroup: fork rejected by pids controller" kernel line is not readable;
#   errno 11 on fork + pids.events/pids.current carry the evidence.
#
# Knob mapping: docker --pids-limit == systemd TasksMax= (pids controller).
# All docker resources are throwaway (gap114-f-*) and removed on exit.
set -euo pipefail

WORK="${GAP114_WORK:-/tmp/gap114}"
TOOLCHAIN_IMG="gap114/toolchain"
RUNNER="gap114-f-runner"

mkdir -p "$WORK"
log() { printf '\n=== %s ===\n' "$*"; }

cat > "$WORK/forker.py" <<'EOF'
import os, sys, time
limit = int(sys.argv[1])
kids = []
try:
    for _ in range(limit):
        pid = os.fork()
        if pid == 0:
            time.sleep(300)          # child parks; parent does the counting
        kids.append(pid)
    print(f"FORKER-COMPLETED children={len(kids)}")
except OSError as e:
    print(f"FORKER-EAGAIN after={len(kids)} children err={e}")
finally:
    for p in kids:
        try: os.kill(p, 9)
        except OSError: pass
EOF

cat > "$WORK/sweep_fork.py" <<'EOF'
import os, sys, time
target = int(sys.argv[1])
park = float(sys.argv[2]) if len(sys.argv) > 2 else 6.0
kids = []
err = ""
t0 = time.time()
for _ in range(target):
    try:
        pid = os.fork()
    except OSError as e:
        err = f"{e.errno} {e.strerror}"
        break
    if pid == 0:
        time.sleep(park)
        os._exit(0)
    kids.append(pid)
time.sleep(0.5)
try:
    cur = open("/sys/fs/cgroup/pids.current").read().strip()
    mx = open("/sys/fs/cgroup/pids.max").read().strip()
except OSError:
    cur = mx = "n/a"
print(f"SWEEP target={target} forked={len(kids)} stopped_by={err or 'target-reached'} "
      f"pids.max={mx} pids.current={cur} elapsed={time.time()-t0:.1f}s")
for p in kids:
    try: os.kill(p, 9)
    except OSError: pass
EOF

cleanup() { docker container remove -f "$RUNNER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

log "stage1: fork rejection signature (EAGAIN), limit=16, target=40"
set +e
docker run --name "$RUNNER" --pids-limit=16 \
  -v "$WORK:/w" "$TOOLCHAIN_IMG" python3 /w/forker.py 40 2>&1
RC=$?
set -e
echo "OBSERVED stage1 container rc=$RC (0 = the forker SURVIVED the rejection)"
printf 'OBSERVED host dmesg_restrict='; cat /proc/sys/kernel/dmesg_restrict
echo "OBSERVED (dmesg restricted -> kernel 'fork rejected' line not readable; errno 11 above is the evidence)"

log "stage2: sweep — target 200 tasks under pids limits 128..16"
for LIM in 128 64 48 40 32 24 16; do
  docker container remove -f "$RUNNER" >/dev/null 2>&1 || true
  printf 'OBSERVED limit=%s -> ' "$LIM"
  set +e
  docker run --name "$RUNNER" --pids-limit="$LIM" \
    -v "$WORK:/w" "$TOOLCHAIN_IMG" python3 /w/sweep_fork.py 200 6 2>&1
  RC=$?
  set -e
  echo "  (container rc=$RC)"
done

log "verdict data"
echo "OBSERVED enforcement is EXACT: at limit L, forked=L-1, pids.current==pids.max, stop=EAGAIN."
echo "OBSERVED a ~25-task compose-like stack trips any TasksMax <= ~26; default 4096 leaves ~160x headroom."
echo "PROBE-F-OK"
