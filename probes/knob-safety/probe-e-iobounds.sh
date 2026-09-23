#!/usr/bin/env bash
# GAP-114 probe (e): IOWeight / bandwidth bounds — protection vs slowdown.
#
# Measured here:
#   1. --device-write-bps on the WHOLE disk (cgroup-v2 io.max wbps) — bounds a
#      large dd write; wall time and host PSI (io pressure) recorded.
#      NOTE (measured first, see doc): naming the PARTITION (/dev/nvme0n1p2)
#      makes the container fail at create with ENODEV — io.max rejects
#      partitions; scripts must pass the whole disk.
#   2. Weight contrast (--blkio-weight 900 vs 100) between two concurrent dd
#      writers — does the weight shape the split?
#   3. Unbounded control.
#
# Knob mapping: io.max bandwidth == systemd IOWriteBandwidthMax=;
# io.weight == systemd IOWeight=. All docker resources are throwaway
# (gap114-e-*) and removed on exit.
set -euo pipefail

WORK="${GAP114_WORK:-/tmp/gap114}"
TOOLCHAIN_IMG="gap114/toolchain"
DISK="$(lsblk -no PKNAME "$(findmnt -no SOURCE /)" | head -1)"
DISKDEV="/dev/$DISK"
MB="${1:-1200}"   # dd size per run in MiB

mkdir -p "$WORK"
log() { printf '\n=== %s ===\n' "$*"; }

psiread() { awk '/^full/ {print $2}' /proc/pressure/io 2>/dev/null || echo n/a; }
sample_psi() {  # until $1 pid exits; prints max avg10 seen
  local max=0 cur
  while kill -0 "$1" 2>/dev/null; do
    cur=$(psiread | sed 's/avg10=//;s/%.*//' )
    [ -n "$cur" ] && max=$(python3 -c "print(max($max,$cur))")
    sleep 1
  done
  echo "OBSERVED host io-PSI-full-max-avg10=${max}%"
}

dd_cmd() { printf 'dd if=/dev/zero of=/w/ddtest bs=1M count=%s oflag=direct 2>&1 | tail -1' "$MB"; }

run_dd() {  # $1 label, extra args = docker flags
  local label="$1"; shift
  docker container remove -f gap114-e-runner >/dev/null 2>&1 || true
  rm -f "$WORK/ddtest"
  log "run: $label (flags: $*)"
  sample_psi $$ &   # coarse: samples while THIS shell waits (approximation)
  local t0=$(date +%s.%N)
  set +e
  docker run --name gap114-e-runner --memory=512m "$@" \
    -v "$WORK:/w" "$TOOLCHAIN_IMG" sh -c "$(dd_cmd)"
  local rc=$?
  set -e
  local t1=$(date +%s.%N)
  printf 'OBSERVED label=%s exit=%s wall=%ss\n' \
    "$label" "$rc" "$(python3 -c "print(f'{$t1-$t0:.1f}')")"
  [ "$rc" -eq 0 ] || { echo "PROBE-FAIL: dd failed rc=$rc" >&2; exit 1; }
}

log "context"
echo "OBSERVED root-disk=$DISKDEV (whole-disk required for io.max; partition gave ENODEV when probed)"
docker version --format 'OBSERVED docker-server={{.Server.Version}}'

run_dd "e-unbounded"
run_dd "e-wbps-50MiBs" --device-write-bps "${DISKDEV}:52428800"
run_dd "e-wbps-20MiBs" --device-write-bps "${DISKDEV}:20971520"

log "weight contrast: two concurrent dd writers, weights 900 vs 100"
docker container remove -f gap114-e-w9 >/dev/null 2>&1 || true
docker container remove -f gap114-e-w1 >/dev/null 2>&1 || true
docker run -d --name gap114-e-w9 --blkio-weight=900 --memory=512m \
  -v "$WORK:/w" "$TOOLCHAIN_IMG" sleep 90 >/dev/null
docker run -d --name gap114-e-w1 --blkio-weight=100 --memory=512m \
  -v "$WORK:/w" "$TOOLCHAIN_IMG" sleep 90 >/dev/null
sleep 1
T0=$(date +%s.%N)
docker exec gap114-e-w9 sh -c "dd if=/dev/zero of=/w/dd9 bs=1M count=${MB} oflag=direct 2>&1 | tail -1" &
JOB9=$!
docker exec gap114-e-w1 sh -c "dd if=/dev/zero of=/w/dd1 bs=1M count=${MB} oflag=direct 2>&1 | tail -1" &
JOB1=$!
set +e
wait "$JOB9"; RC9=$?
wait "$JOB1"; RC1=$?
set -e
T1=$(date +%s.%N)
W9=$(docker exec gap114-e-w9 sh -c "cat /sys/fs/cgroup/io.stat | grep '^$(lsblk -no MAJ:MIN "$DISKDEV" | tr -d ' '): ' || cat /sys/fs/cgroup/io.stat" | head -1)
W1=$(docker exec gap114-e-w1 sh -c "cat /sys/fs/cgroup/io.stat | grep '^$(lsblk -no MAJ:MIN "$DISKDEV" | tr -d ' '): ' || cat /sys/fs/cgroup/io.stat" | head -1)
echo "OBSERVED weight-900 io.stat: $W9"
echo "OBSERVED weight-100 io.stat: $W1"
echo "OBSERVED pair wall=$(python3 -c "print(f'{$T1-$T0:.1f}')")s rc9=$RC9 rc1=$RC1"
docker container remove -f gap114-e-w9 >/dev/null 2>&1 || true
docker container remove -f gap114-e-w1 >/dev/null 2>&1 || true
rm -f "$WORK/ddtest" "$WORK/dd9" "$WORK/dd1"
docker container remove -f gap114-e-runner >/dev/null 2>&1 || true
echo "PROBE-E-OK"
