#!/usr/bin/env bash
# Repeat one operation N times inside a single mount and report the SFTP round-trip
# cost of each repetition (MOUNT-015). A cold/cold/cold pattern means the operation
# always pays per file; cold/warm/warm means readdirplus paid off from the second
# invocation on, which is the pattern that matters for repeated whole-tree scans
# (rsync loops, git watch, editor file-tree refreshes).
#
# Usage: repeat-op.sh <binary> <host> <remote-dir> <mountpoint> <iterations> -- <command...>
set -u

BIN=${1:?binary}; HOST=${2:?host}; RDIR=${3:?remote dir}; MP=${4:?mount}
ITERS=${5:?iterations}
shift 5
[ "${1:-}" = "--" ] && shift
CMD=("$@")
OP_TIMEOUT=${OP_TIMEOUT:-120}
LOG=$(mktemp /tmp/m015-repeat.XXXXXX.log)
OUT=${OUT:-/tmp/m015-repeat}

mkdir -p "$MP" "$OUT"
MOUNT_EXTRA=${MOUNT_EXTRA:--o reconnect -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ConnectTimeout=15 -o auto_unmount -o dir_cache=no}

count() { grep -cE "^\[[0-9]+\] +$1\$" "$LOG" 2>/dev/null || true; }

# shellcheck disable=SC2086
"$BIN" "$HOST:$RDIR" "$MP" $MOUNT_EXTRA ${ARM_EXTRA:-} -o sshfs_debug \
	< /dev/null > "$LOG" 2>&1 &
for _ in $(seq 1 60); do
	mount | grep -q " on $MP " && break
	sleep 0.5
done
mount | grep -q " on $MP " || { echo "FATAL: did not mount (see $LOG)" >&2; exit 1; }

prev=$(count LSTAT)
echo "mount,iteration,lstat_delta,seconds,rc"
i=0
while [ "$i" -lt "$ITERS" ]; do
	i=$((i + 1))
	t0=$EPOCHREALTIME
	( cd "$MP" && timeout "$OP_TIMEOUT" "${CMD[@]}" ) > "$OUT/iter-$i.out" 2>&1
	rc=$?
	t1=$EPOCHREALTIME
	now=$(count LSTAT)
	dt=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.3f", b-a}')
	printf 'mount,%d,%d,%s,%s\n' "$i" "$((now - prev))" "$dt" "$rc"
	prev=$now
done
echo "log: $LOG"
fusermount3 -u "$MP" 2>/dev/null || umount "$MP" 2>/dev/null
for _ in $(seq 1 40); do mount | grep -q " on $MP " || break; sleep 0.25; done
