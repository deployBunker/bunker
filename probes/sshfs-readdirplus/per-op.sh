#!/usr/bin/env bash
# Per-operation SFTP round-trip attribution for the sshfs readdirplus patch (MOUNT-015 D2).
#
# The whole-arm battery (readdirplus-ab.sh) mixes ten operations into one request
# count, which cannot say *which* operation readdirplus accelerates. This harness
# mounts fresh for every (arm, operation) pair - a fresh mount is a fresh FUSE
# superblock, so the kernel's inode/dentry caches start cold - runs exactly one
# operation, and counts the SFTP requests sshfs logged for it. The count for a
# no-op operation gives the mount's own overhead, which is subtracted so the
# numbers are per-operation, not per-mount.
#
# What to look for: LSTAT. Without readdirplus every metadata read is one
# SSH_FXP_LSTAT round trip per file; with readdirplus the attributes arrive with
# the directory listing and the LSTATs disappear.
#
# Usage: per-op.sh <arm...> -- <host> <remote-dir> <mount-point> <outdir>
#   An arm is "label=path" or "label=path|-o extra -o opts".
#   e.g. per-op.sh base=./base/sshfs plus=./plus/sshfs 'plus-off=./plus/sshfs|-o readdirplus=no' \
#          -- localhost /tmp/fix /tmp/mnt /tmp/out
set -u

BINS=()
while [ $# -gt 0 ] && [ "$1" != "--" ]; do BINS+=("$1"); shift; done
[ "${1:-}" = "--" ] && shift
HOST=${1:?host}
RDIR=${2:?remote dir}
MP=${3:?mountpoint}
OUT=${4:-/tmp/m015-perop}
OP_TIMEOUT=${OP_TIMEOUT:-120}
MOUNT_EXTRA=${MOUNT_EXTRA:--o reconnect -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ConnectTimeout=15 -o auto_unmount -o dir_cache=no}

mkdir -p "$OUT" "$MP"
CSV=$OUT/per-op.csv
echo "arm,op,seconds,lstat,stat,fstat,readdir,opendir,total" > "$CSV"

count() { # $1=log $2=request type
	grep -cE "^\[[0-9]+\] +$2\$" "$1" 2>/dev/null || true
}

mount_arm() { # $1=label $2=binary $3=log
	# shellcheck disable=SC2086
	"$2" "$HOST:$RDIR" "$MP" $MOUNT_EXTRA $ARM_EXTRA -o sshfs_debug \
		< /dev/null > "$3" 2>&1 &
	for _ in $(seq 1 60); do
		mount | grep -q " on $MP " && return 0
		sleep 0.5
	done
	echo "FATAL: $1 did not mount (see $3)" >&2
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

run_op() { # $1=arm $2=op-name $3=command
	local log="$OUT/$1.$2.log" t0 t1 dt rc
	mount_arm "$1" "$BIN" "$log" || return 1
	# Baseline for this mount: the requests sshfs makes on its own
	local base_lstat; base_lstat=$(count "$log" LSTAT)
	t0=$EPOCHREALTIME
	( cd "$MP" && timeout "$OP_TIMEOUT" bash -c "$3" ) > "$OUT/$1.$2.out" 2>&1
	rc=$?
	t1=$EPOCHREALTIME
	dt=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.3f", b-a}')
	local lstat stat fstat readdir opendir total
	lstat=$(count "$log" LSTAT); stat=$(count "$log" STAT)
	fstat=$(count "$log" FSTAT); readdir=$(count "$log" READDIR)
	opendir=$(count "$log" OPENDIR)
	total=$(grep -cE '^\[[0-9]+\] +[A-Z_]+$' "$log" || true)
	unmount_arm
	printf '%-10s %-26s %8ss lstat=%-5s (raw %-5s) stat=%-4s readdir=%-4s total=%s rc=%s\n' \
		"$1" "$2" "$dt" "$((lstat - base_lstat))" "$lstat" "$stat" "$readdir" "$total" "$rc"
	echo "$1,$2,$dt,$((lstat - base_lstat)),$stat,$fstat,$readdir,$opendir,$total" >> "$CSV"
}

OPS=(
	"mount-overhead|true"
	"find-metadata-walk|find . -path ./.git -prune -o -type f -printf '%p %s %T@\\n' | sort"
	"git-status-porcelain|git status --porcelain"
	"git-diff-stat|git diff --stat HEAD"
	"git-ls-files|git ls-files"
	"ls-lR|ls -lR"
)

for entry in "${BINS[@]}"; do
	LABEL=${entry%%=*}
	rest=${entry#*=}
	BIN=${rest%%|*}
	ARM_EXTRA=""
	[ "${rest#*|}" != "$rest" ] && ARM_EXTRA=${rest#*|}
	echo "================ arm: $LABEL ($BIN ${ARM_EXTRA:-<default opts>})"
	for op in "${OPS[@]}"; do
		if [ -n "${OPS_FILTER:-}" ] && ! [[ ${op%%|*} =~ $OPS_FILTER ]]; then
			continue
		fi
		run_op "$LABEL" "${op%%|*}" "${op#*|}"
	done
done

echo
echo "per-op table: $CSV"
