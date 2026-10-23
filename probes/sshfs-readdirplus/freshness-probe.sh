#!/usr/bin/env bash
# Freshness / staleness probe harness for the sshfs readdirplus patch (MOUNT-015 D3).
#
# Mounts <binary> against <remote-dir> with max_conns>1 - which is what makes
# sshfs.conntab exist and therefore what makes the pending-write caveat live -
# runs freshness-probe.py through the mount, and reports:
#   * the per-iteration size agreement (stat while open / directory listing /
#     stat after close)
#   * whether the patch's suppression actually fired ("no attrs for ..." in the
#     sshfs debug log)
#   * a git-level check: a tracked file appended through the mount must show up
#     as modified in `git status --porcelain`
#
# Usage: freshness-probe.sh <binary> <host> <remote-dir> <mountpoint> [outdir]
set -u

BIN=${1:?sshfs binary}
HOST=${2:?remote host}
RDIR=${3:?remote directory}
MP=${4:?mountpoint}
OUT=${5:-/tmp/mnt015-fresh}
ITERS=${ITERS:-8}
MAX_CONNS=${MAX_CONNS:-4}
HERE=$(cd "$(dirname "$0")" && pwd)

mkdir -p "$MP" "$OUT"
LOG=$OUT/freshness.sshfs.log

"$BIN" "$HOST:$RDIR" "$MP" -o reconnect -o ConnectTimeout=15 -o auto_unmount \
	-o dir_cache=no -o "max_conns=$MAX_CONNS" -o sshfs_debug \
	< /dev/null > "$LOG" 2>&1 &
for _ in $(seq 1 60); do
	mount | grep -q " on $MP " && break
	sleep 0.5
done
if ! mount | grep -q " on $MP "; then
	echo "FATAL: did not mount (see $LOG)" >&2
	exit 1
fi

rc=0
echo "=== freshness probe (max_conns=$MAX_CONNS, $ITERS iterations)"
python3 "$HERE/freshness-probe.py" "$MP" "$ITERS" || rc=1

echo
echo "=== suppression evidence in the sshfs debug log"
supp=$(grep -c 'no attrs for' "$LOG" || true)
plus=$(grep -c '^readdirplus:' "$LOG" || true)
plain=$(grep -c '^readdir:' "$LOG" || true)
echo "readdir calls: plus=$plus plain=$plain"
echo "entries whose attrs were withheld (pending write): $supp"
if [ "${EXPECT_SUPPRESS:-1}" = 1 ] && [ "$supp" -eq 0 ]; then
	echo "FAIL: the caveat path never fired - suppression unproven"
	rc=1
fi

echo
echo "=== git-level check: append through the mount, then git status"
GIT_REL=${GIT_REL:-sub1/file01.txt}
printf 'appended through the mount %s\n' "$(date -Is)" >> "$MP/$GIT_REL"
( cd "$MP" && git status --porcelain ) > "$OUT/git-status-after-write.txt" 2>&1
cat "$OUT/git-status-after-write.txt"
grep -q "M $GIT_REL" "$OUT/git-status-after-write.txt" || {
	echo "FAIL: the appended file is not reported modified"; rc=1; }

fusermount3 -u "$MP" 2>/dev/null || umount "$MP" 2>/dev/null
for _ in $(seq 1 40); do mount | grep -q " on $MP " || break; sleep 0.25; done

echo
echo "RESULT: $([ $rc -eq 0 ] && echo PASS || echo FAIL)"
exit "$rc"
