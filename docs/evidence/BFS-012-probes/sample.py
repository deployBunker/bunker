#!/usr/bin/env python3
"""sample.py — sample the CLIENT'S REAL on-disk footprint while a run is in flight.

BFS-012's first acceptance item is "the cache directory's real on-disk size
(measure it yourself) never exceeds the bound" AND "the figure the client reports
commands agreement with your measurement". Those are two different instruments, so
this samples both, every interval, from the same instant:

  * `du -sb` of the cache directory (the filesystem's own answer), plus the REAL
    byte sizes of blobs/ and index.json (no block rounding), so the difference
    between "files" and "du" is visible instead of assumed;
  * the mount's own status.json (`cache.used_bytes`, `blobs_bytes`,
    `index_bytes`, the counters).

JSON, not sed: the doc is machine-written, so it is read as JSON, and a sample
whose status.json is mid-rename (the mount writes it atomically) is retried
rather than recorded as a zero.

usage: sample.py <cache-dir> <out.csv> [interval_s]
columns: epoch_ms,du_total,du_blobs,du_index,file_blobs_bytes,file_index_bytes,
         used_bytes,blobs_bytes,index_bytes,entries,blobs,evictions_total,
         bypass_events,oversize_bypasses,pinned_blobs,hits,misses
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import time

COLS = [
    "epoch_ms", "du_total", "du_blobs", "du_index", "file_blobs_bytes", "file_index_bytes",
    "used_bytes", "blobs_bytes", "index_bytes", "entries", "blobs", "evictions_total",
    "bypass_events", "oversize_bypasses", "pinned_blobs", "hits", "misses",
]


def du(path: str) -> int:
    try:
        out = subprocess.run(["du", "-sb", path], capture_output=True, text=True, timeout=20).stdout
        return int(out.split()[0])
    except Exception:
        return -1


def real_bytes(path: str) -> int:
    total = 0
    try:
        for name in os.listdir(path):
            p = os.path.join(path, name)
            if os.path.isfile(p):
                total += os.path.getsize(p)
    except FileNotFoundError:
        return -1
    return total


def stat_size(path: str) -> int:
    try:
        return os.path.getsize(path)
    except OSError:
        return -1


def read_status(path: str) -> dict | None:
    for _ in range(3):
        try:
            with open(path) as fh:
                return json.load(fh)
        except (OSError, json.JSONDecodeError):
            time.sleep(0.02)
    return None


def main() -> int:
    if len(sys.argv) < 3:
        print(__doc__.split("usage:")[-1].strip(), file=sys.stderr)
        return 2
    cdir, out = sys.argv[1], sys.argv[2]
    interval = float(sys.argv[3]) if len(sys.argv) > 3 else 0.5
    with open(out, "w") as fh:
        fh.write(",".join(COLS) + "\n")
        while os.path.isdir(cdir):
            now = int(time.time() * 1000)
            st = read_status(os.path.join(cdir, "status.json")) or {}
            cache = st.get("cache", {}) if isinstance(st, dict) else {}
            row = [
                now,
                du(cdir), du(os.path.join(cdir, "blobs")), stat_size(os.path.join(cdir, "index.json")),
                real_bytes(os.path.join(cdir, "blobs")), stat_size(os.path.join(cdir, "index.json")),
                cache.get("used_bytes"), cache.get("blobs_bytes"), cache.get("index_bytes"),
                cache.get("entries"), cache.get("blobs"), cache.get("evictions_total"),
                cache.get("bypass_events"), cache.get("oversize_bypasses"), cache.get("pinned_blobs"),
                cache.get("hits"), cache.get("misses"),
            ]
            fh.write(",".join("" if v is None else str(v) for v in row) + "\n")
            fh.flush()
            time.sleep(interval)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
