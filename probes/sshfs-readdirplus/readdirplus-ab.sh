#!/usr/bin/env bash
# Fixture A battery, three arms across the sshfs readdirplus patch:
#
#   base      unpatched sshfs, pinned upstream commit (see apply-patch.sh)
#   plus      patched sshfs, readdirplus active (default)
#   plus-off  patched sshfs with -o readdirplus=no (the runtime opt-out)
#
# One mountpoint, one remote tree, a fresh mount per arm, so the only variable
# is the flag. Each arm records:
#   * wall time (or STALL, 120 s ceiling) per Fixture A op
#   * SFTP request counts that sshfs logs under -o sshfs_debug - the round trips
#     that actually cost latency, bucketed by request type
#   * the bytes of `find`'s metadata walk, `git status --porcelain`,
#     `git diff --stat HEAD` and `git status --short`, for byte comparison
#
# Mount options default to the fleet's durable sshfs option set
# (internal/cli/mount_preflight.go: reconnect, bounded ServerAlive,
# ConnectTimeout, auto_unmount, dir_cache=no). Override with MOUNT_EXTRA, e.g.
#   MOUNT_EXTRA="" reads the same tree with the sshfs defaults (dir_cache=yes).
#
# Usage:
#   readdirplus-ab.sh <base-binary> <plus-binary> <host> <remote-dir> \
#                     <mountpoint> [outdir]
set -u

BASE=${1:?base sshfs binary}
PLUS=${2:?patched sshfs binary}
HOST=${3:?remote host}
RDIR=${4:?remote directory}
MP=${5:?mountpoint}
OUT=${6:-/tmp/mount015-runs}
OP_TIMEOUT=${OP_TIMEOUT:-120}
MOUNT_CMD_TIMEOUT=${MOUNT_CMD_TIMEOUT:-30}
: "${MOUNT_EXTRA:=-o reconnect -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ConnectTimeout=15 -o auto_unmount -o dir_cache=no}"

SSH="ssh -o BatchMode=yes -o ConnectTimeout=15 $HOST"
mkdir -p "$OUT" "$MP"
: > "$OUT/timings.csv"

ARM=""
REPORT() { printf '%-9s %-28s %9ss %s\n' "$1" "$2" "$3" "$4"; }

timed() { # $1=label $2=shell function
	local t0 t1 dt rc
	t0=$EPOCHREALTIME
	"$2"
	rc=$?
	t1=$EPOCHREALTIME
	dt=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.3f", b-a}')
	if [ "$rc" -eq 124 ]; then
		REPORT "$ARM" "$1" "$dt" "STALL(>${OP_TIMEOUT}s)"
		echo "$ARM,$1,$dt,STALL" >> "$OUT/timings.csv"
	else
		REPORT "$ARM" "$1" "$dt" "ok"
		echo "$ARM,$1,$dt,ok" >> "$OUT/timings.csv"
	fi
}

op_find()    { ( cd "$MP" && timeout "$OP_TIMEOUT" find . -path ./.git -prune \
                 -o -type f -printf '%p %s %T@\n' | sort ) > "$OUT/$ARM.walk.txt" 2>&1; }
op_lslr()    { ( cd "$MP" && timeout "$OP_TIMEOUT" ls -lR ) > /dev/null 2>&1; }
op_revparse(){ ( cd "$MP" && timeout "$OP_TIMEOUT" git rev-parse --show-toplevel ) > /dev/null 2>&1; }
op_log()     { ( cd "$MP" && timeout "$OP_TIMEOUT" git log -1 --oneline ) > /dev/null 2>&1; }
op_lsfiles() { ( cd "$MP" && timeout "$OP_TIMEOUT" git ls-files | wc -l ) > "$OUT/$ARM.lsfiles.txt" 2>&1; }
op_status()  { ( cd "$MP" && timeout "$OP_TIMEOUT" git status --porcelain ) > "$OUT/$ARM.status.txt" 2>&1; }
op_diffstat(){ ( cd "$MP" && timeout "$OP_TIMEOUT" git diff --stat HEAD ) > "$OUT/$ARM.diffstat.txt" 2>&1; }
op_shortst() { ( cd "$MP" && timeout "$OP_TIMEOUT" git status --short ) > "$OUT/$ARM.statusshort.txt" 2>&1; }
op_add()     { ( cd "$MP" && timeout "$OP_TIMEOUT" git add -A ); }
op_branch()  { ( cd "$MP" && timeout "$OP_TIMEOUT" git checkout -b "probe-$ARM" && \
                 timeout "$OP_TIMEOUT" git -c user.email=fixture@example.com \
                   -c user.name=fixture commit --allow-empty -q -m probe ); }

mount_arm() { # $1=arm $2=binary $3=extra opts
	# shellcheck disable=SC2086
	"$2" "$HOST:$RDIR" "$MP" $MOUNT_EXTRA $3 -o sshfs_debug \
		< /dev/null > "$OUT/$1.sshfs.log" 2>&1 &
	for _ in $(seq 1 $((MOUNT_CMD_TIMEOUT * 2))); do
		mount | grep -q " on $MP " && return 0
		sleep 0.5
	done
	echo "FATAL: $1 did not mount (see $OUT/$1.sshfs.log)" >&2
	return 1
}

unmount_arm() {
	fusermount3 -u "$MP" 2>/dev/null || umount "$MP" 2>/dev/null
	for _ in $(seq 1 40); do
		mount | grep -q " on $MP " || return 0
		sleep 0.25
	done
	echo "WARN: still mounted: $MP" >&2
}

sftp_report() { # $1=arm
	local log="$OUT/$1.sshfs.log" f="$OUT/$1.sftp.txt"
	{
		echo "arm=$1 mount_extra=${MOUNT_EXTRA:-none}"
		echo "-- SFTP requests by type (count over the whole arm)"
		grep -E '^\[[0-9]+\] +[A-Z_]+$' "$log" | awk '{print $2}' |
			sort | uniq -c | sort -rn
		echo "-- readdir handler calls: total=$(grep -c '^readdir' "$log" || true)" \
		     "plus=$(grep -c '^readdirplus:' "$log" || true)" \
		     "plain=$(grep -c '^readdir:' "$log" || true)"
		echo "-- entries whose attrs were withheld (pending write):" \
		     "$(grep -c 'no attrs for' "$log" || true)"
	} | tee "$f"
}

battery() {
	ARM=$1
	timed "find metadata walk"    op_find
	timed "ls -lR"                op_lslr
	timed "git rev-parse"         op_revparse
	timed "git log -1"            op_log
	timed "git ls-files"          op_lsfiles
	timed "git status --porcelain" op_status
	timed "git diff --stat HEAD"  op_diffstat
	timed "git status --short"    op_shortst
	timed "git add -A"            op_add
	$SSH "cd $RDIR && git reset -q" > /dev/null 2>&1
	timed "git checkout -b + commit" op_branch
}

ORIG=$($SSH "cd $RDIR && git rev-parse --abbrev-ref HEAD" 2>/dev/null)
echo "remote=$HOST:$RDIR mountpoint=$MP original-branch=${ORIG:-unknown}"
echo "mount options: $MOUNT_EXTRA"
echo "op ceiling: ${OP_TIMEOUT}s"
echo "arm,op,seconds,status" > "$OUT/timings.csv"

for arm in ${ARMS:-base plus plus-off}; do
	case $arm in
	base)     BIN=$BASE; EXTRA="" ;;
	plus)     BIN=$PLUS; EXTRA="" ;;
	plus-off) BIN=$PLUS; EXTRA="-o readdirplus=no" ;;
	esac
	echo "================ arm: $arm ($BIN ${EXTRA:-<default>})"
	mount_arm "$arm" "$BIN" "$EXTRA" || continue
	battery "$arm"
	sftp_report "$arm"
	unmount_arm
	$SSH "cd $RDIR && git checkout -q '${ORIG:-master}' 2>/dev/null || git checkout -q '${ORIG:-main}' 2>/dev/null; git branch -D 'probe-$arm' 2>/dev/null; git reset -q" > /dev/null 2>&1
	$SSH "cd $RDIR && git status --porcelain" > "$OUT/$arm.after.txt" 2>&1
	sleep 2
done

echo
echo "================ byte comparison of captured outputs"
for f in status.txt diffstat.txt walk.txt statusshort.txt; do
	for other in plus plus-off; do
		[ -f "$OUT/$other.$f" ] || continue
		if cmp -s "$OUT/base.$f" "$OUT/$other.$f"; then
			echo "base vs $other $f: IDENTICAL ($(wc -c < "$OUT/base.$f") bytes)"
		else
			echo "base vs $other $f: DIFFERS"
			diff -u "$OUT/base.$f" "$OUT/$other.$f" | sed -n '1,15p'
		fi
	done
done
echo
echo "================ post-arm tree state (must be identical, proves the battery left no residue)"
for arm in ${ARMS:-base plus plus-off}; do echo "-- $arm"; cat "$OUT/$arm.after.txt" 2>/dev/null; done
echo
echo "timings: $OUT/timings.csv"
