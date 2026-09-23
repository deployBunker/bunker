#!/usr/bin/env bash
# GAP-114 probe (d): OOM-group semantics.
#
# WHAT IS MEASURED HERE (unprivileged-runnable):
#   B. Docker pair under independent memory pressure: the OOM kill takes the
#      victim container only; the sibling keeps running (the kernel's kill
#      granularity is the pressured cgroup — a compose-like pair has NO
#      group semantics at the docker level).
#   B-control. --oom-kill-disable is bypassed on cgroup v2 (no-op flag) —
#      measured, because GAP-119 must not rely on it for protection.
#
# WHAT IS UNMEASURED (recorded loudly, re-run on a permissive host):
#   A. memory.oom.group=1 atomic subtree reaping under systemd. On THIS host,
#      all three unprivileged paths are denied (captured live 2026-09-22):
#        1) `systemd-run --user -p MemoryOOMGroup=yes` -> "Unknown assignment:
#           MemoryOOMGroup=yes" (user manager 259 refuses the property, with
#           or without Delegate=yes);
#        2) delegated scope (Delegate=yes): mkdir under the scope works, but
#           every controller write fails EACCES — `echo 1 > .../memory.oom.group`
#           -> Permission denied; enabling +memory in cgroup.subtree_control
#           also fails — this host's kernel denies knob writes on
#           user-delegated cgroups even when the files are owned by $UID;
#        3) docker exposes no oom.group equivalent, and host cgroupfs is
#           out of scope by the probe's own safety rule.
#      => memory.oom.group stays UNMEASURED-here; it must NOT default on
#      until measured on a host where path (1) or (2) works (root manager
#      unit or permissive kernel).
set -euo pipefail

WORK="${GAP114_WORK:-/tmp/gap114}"
TOOLCHAIN_IMG="gap114/toolchain"
mkdir -p "$WORK"
log() { printf '\n=== %s ===\n' "$*"; }

cat > "$WORK/oom_victim.py" <<'EOF'
import time
buf = bytearray()
while len(buf) < 300 * 1024 * 1024:      # ramp far past the 64M cap
    buf += bytearray(16 * 1024 * 1024)
    time.sleep(0.01)
print("VICTIM-HELD", flush=True)
time.sleep(120)
EOF

cleanup() {
  for c in gap114-d-victim gap114-d-sibling gap114-d-protect; do
    docker container remove -f "$c" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

log "mechanism B: docker pair — OOM takes the victim, sibling stays Up"
docker run -d --name gap114-d-victim --memory=64m --memory-swap=64m \
  -v "$WORK:/w" "$TOOLCHAIN_IMG" python3 /w/oom_victim.py >/dev/null
docker run -d --name gap114-d-sibling --memory=64m --memory-swap=64m \
  "$TOOLCHAIN_IMG" sh -c 'for i in $(seq 1 30); do echo SIB-UP $i; sleep 1; done' >/dev/null
sleep 12
echo "OBSERVED victim: state=$(docker inspect -f '{{.State.Status}}' gap114-d-victim) oomkilled=$(docker inspect -f '{{.State.OOMKilled}}' gap114-d-victim) exit=$(docker inspect -f '{{.State.ExitCode}}' gap114-d-victim)"
echo "OBSERVED sibling: state=$(docker inspect -f '{{.State.Status}}' gap114-d-sibling) (still Up despite the pair)"
echo "OBSERVED sibling last liveness lines:"
docker logs gap114-d-sibling 2>&1 | tail -2

log "mechanism B-control: --oom-kill-disable on cgroup v2 (expected: bypassed)"
set +e
docker run -d --name gap114-d-protect --memory=64m --memory-swap=64m \
  --oom-kill-disable -v "$WORK:/w" "$TOOLCHAIN_IMG" python3 /w/oom_victim.py \
  >/dev/null 2>"$WORK/oomdis.err"
RC=$?
set -e
echo "OBSERVED docker-run-with-oom-kill-disable rc=$RC stderr=$(head -1 "$WORK/oomdis.err" 2>/dev/null)"
if [ "$RC" -eq 0 ]; then
  sleep 12
  echo "OBSERVED protected-victim: state=$(docker inspect -f '{{.State.Status}}' gap114-d-protect) oomkilled=$(docker inspect -f '{{.State.OOMKilled}}' gap114-d-protect) exit=$(docker inspect -f '{{.State.ExitCode}}' gap114-d-protect)"
fi

log "mechanism A re-check (recorded loudly; the exact refusals)"
echo "OBSERVED systemd-run --user -p MemoryOOMGroup=yes ->"
systemd-run --user -u gap114-d-og-probe --scope -p MemoryOOMGroup=yes true 2>&1 | tail -1 || true
echo "OBSERVED delegated-scope controller write ->"
systemd-run --user -u gap114-d-og-probe2 --scope -p Delegate=yes \
  sh -c 'R=$(grep 0:: /proc/self/cgroup | cut -d: -f3); mkdir /sys/fs/cgroup$R/leafog 2>&1; echo 1 > /sys/fs/cgroup$R/leafog/memory.oom.group 2>&1 && echo "OG-WRITE-OK" || echo "OG-WRITE-DENIED"' 2>&1 | tail -2 || true

echo "PROBE-D-OK (measured: B + B-control; A recorded as UNMEASURED with captured refusals)"
