#!/usr/bin/env bash
# read-all-inprocess.sh — the mount-side cost of the read path with the process
# spawn removed: ONE process reads every file through the mount with read(2) and
# sums the bytes. The per-file `cat` arm (whole-tree-read.sh) is the realistic
# reader; this one isolates the mount's own work, so a change to the read path
# shows up as a delta rather than inside 40 × fork/exec.
#
# env (set by mount-arm.sh): MNT TREE OUT
set -uo pipefail
python3 - "$MNT" <<'PY'
import os, sys, time
root = sys.argv[1]
files = []
for dirpath, _dirs, names in os.walk(root):
    for n in sorted(names):
        files.append(os.path.join(dirpath, n))
files.sort()
start = time.time()
total = 0
for f in files:
    total += len(open(f, "rb").read())
ms = int((time.time() - start) * 1000)
print("files=%d bytes=%d read_all_wall_ms=%d" % (len(files), total, ms))
PY
