#!/usr/bin/env bash
# bfs009-bound-accounting.sh — DELIVERABLE 1's arithmetic, from the artifacts a
# finished arm left behind: du vs the figure the client reports, with the delta
# NAMED rather than waved at as "block rounding".
#
# usage: bfs009-bound-accounting.sh <run-dir>
set -uo pipefail
D="$1"
CDIR=$(ls -td "$D"/xdg/bunker/fs/* 2>/dev/null | head -1)
S="$CDIR/status.json"
[ -f "$S" ] || { echo "no status.json under $D"; exit 1; }

python3 - "$CDIR" "$S" "$D/sample.csv" <<'PY'
import json, os, sys, subprocess
cdir, spath, samp = sys.argv[1], sys.argv[2], sys.argv[3]
st = json.load(open(spath))
c = st["cache"]

def du(p):
    try:
        return int(subprocess.check_output(["du", "-sb", p]).split()[0])
    except Exception:
        return -1

def size(p):
    try:
        return os.path.getsize(p)
    except Exception:
        return 0

du_total = du(cdir)
du_blobs = du(os.path.join(cdir, "blobs"))
du_index = du(os.path.join(cdir, "index.json"))
real_blobs = sum(os.path.getsize(os.path.join(cdir, "blobs", f))
                 for f in os.listdir(os.path.join(cdir, "blobs")))
status_size = size(spath)
blob_files = len(os.listdir(os.path.join(cdir, "blobs")))

mx_du = mx_used = mx_ev = mx_byp = mx_over = 0
rows = 0
with open(samp) as f:
    next(f, None)
    for line in f:
        p = line.rstrip("\n").split(",")
        if len(p) < 15:
            continue
        rows += 1
        def num(i):
            try: return int(p[i])
            except Exception: return 0
        mx_du = max(mx_du, num(1)); mx_used = max(mx_used, num(6))
        mx_ev = max(mx_ev, num(10)); mx_byp = max(mx_byp, num(11)); mx_over = max(mx_over, num(12))

bound = c["max_bytes"]
print("cache dir                       : %s" % cdir)
print()
print("BOUND       max_bytes (reported) : %16d" % bound)
print("            du -sb cache dir     : %16d   %s" % (du_total, "OK" if du_total <= bound else "EXCEEDED"))
print("            du -sb cache, MAX during the run : %10d   %s" % (mx_du, "OK" if mx_du <= bound else "EXCEEDED"))
print("            used_bytes, MAX during the run  : %10d   %s" % (mx_used, "OK" if mx_used <= bound else "EXCEEDED"))
print()
print("AGREEMENT   reported blobs_bytes : %16d" % c["blobs_bytes"])
print("            du -sb blobs         : %16d   delta=%d" % (du_blobs, du_blobs - c["blobs_bytes"]))
print("            real blob file bytes : %16d   delta=%d" % (real_blobs, real_blobs - c["blobs_bytes"]))
print("            reported index_bytes : %16d" % c["index_bytes"])
print("            du -sb index.json    : %16d   delta=%d" % (du_index, du_index - c["index_bytes"]))
print("            reported used_bytes  : %16d  (= blobs_bytes + index_bytes: %s)"
      % (c["used_bytes"], c["blobs_bytes"] + c["index_bytes"] == c["used_bytes"]))
print("            du -sb cache dir     : %16d   delta vs used_bytes = %d" % (du_total, du_total - c["used_bytes"]))
print("            status.json on disk  : %16d   <- the delta, EXACTLY (a file, not the cache)" % status_size)
print("            accounted: used_bytes + status.json == du(cache dir): %s"
      % (c["used_bytes"] + status_size == du_total))
print()
print("EVICTION    reported evictions   : %16d   bypass=%d oversize=%d" % (c["evictions_total"], c["bypass_events"], c["oversize_bypasses"]))
print("            entries / blob files : %16d / %d" % (c["entries"], blob_files))
print("            entries == blob files: %s" % (c["entries"] == blob_files))
print("            max evictions sampled: %16d" % mx_ev)
print()
print("READS       hits / misses        : %16d / %d" % (c["hits"], c["misses"]))
print("            samples in the run   : %16d" % rows)
PY
