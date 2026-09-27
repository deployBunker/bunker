#!/usr/bin/env bash
# strace-truncate.sh — count the USERSPACE syscalls behind one truncate through the mount.
#
# The request trace (countproxy.py) proved the mount sends, for a single
# `os.truncate()`, a PUT that is REFUSED (412) and then a second PUT that LANDS
# (204). Two PUTs can only come from two SETATTR executions. The remaining
# question is whether the second one is a USERSPACE retry (python, or the client)
# or a SECOND FUSE REQUEST the kernel issued for one syscall. This wrapper answers
# it: the process under test runs under strace, so the syscall count is measured
# rather than argued.
#
# usage (under mount-arm.sh, which sets $MNT/$TREE/$CDIR/$OUT): strace-truncate.sh [--mode edit|noedit]
set -uo pipefail
MODE="${1:---mode}"; MODE="${2:-edit}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "strace: python3 truncate_diag.py --iters 1 --mode $MODE"
strace -f -tt -e trace=truncate,ftruncate -o "$OUT/strace.txt" \
  python3 "$HERE/truncate_diag.py" --iters 1 --mode "$MODE"
echo
echo "=== strace lines mentioning a truncate syscall (from $OUT/strace.txt) ==="
grep -E "truncate\(" "$OUT/strace.txt" | head -20
echo "  truncate/ftruncate syscalls: $(grep -cE "truncate\(" "$OUT/strace.txt")"
echo "  of which failed (returned -1): $(grep -E "truncate\(" "$OUT/strace.txt" | grep -c "= -1")"
