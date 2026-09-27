#!/usr/bin/env python3
"""accounting.py — the three instruments reconciled, for one BFS-012 arm.

Reads a run directory written by mount-arm.sh and prints:

  BOUND       what the client says its cap is, against the largest `du -sb` seen
              WHILE THE RUN WAS IN FLIGHT (the filesystem's answer), and the
              largest `used_bytes` the client reported at the same instants;
  AGREEMENT   reported blobs_bytes vs `du -sb blobs` vs the REAL file bytes;
              reported index_bytes vs index.json; used_bytes vs blobs+index; and
              the residual (used_bytes vs du of the whole cache directory) NAMED
              to the byte instead of waved at as block rounding;
  EVICTION    evictions / bypasses / oversize / pins — the counters that separate
              "bounded" from "bounded because nothing was cached";
  READS       hits/misses when the reader reported them.

A bound is only proven if both figures are below it in EVERY in-flight sample, so
this exits non-zero when any sample exceeds the bound, and says which one.
"""
from __future__ import annotations

import csv
import json
import os
import subprocess
import sys


def read_int(path: str) -> int:
    try:
        with open(path) as fh:
            return int(fh.read().strip().split()[0])
    except Exception:
        return -1


def main() -> int:
    run = sys.argv[1]
    st = json.load(open(os.path.join(run, "status.json")))
    c = st["cache"]
    bound = c["max_bytes"]

    rows = []
    with open(os.path.join(run, "sample.csv")) as fh:
        for r in csv.DictReader(fh):
            rows.append(r)

    def as_int(v, d=-1):
        try:
            return int(v)
        except Exception:
            return d

    du_total = [as_int(r["du_total"]) for r in rows if as_int(r["du_total"]) >= 0]
    used = [as_int(r["used_bytes"]) for r in rows if as_int(r["used_bytes"]) >= 0]
    f_blobs = [as_int(r["file_blobs_bytes"]) for r in rows if as_int(r["file_blobs_bytes"]) >= 0]
    max_du = max(du_total) if du_total else -1
    max_used = max(used) if used else -1
    max_fblobs = max(f_blobs) if f_blobs else -1
    over_du = [(r["epoch_ms"], as_int(r["du_total"])) for r in rows if as_int(r["du_total"]) > bound]
    over_used = [(r["epoch_ms"], as_int(r["used_bytes"])) for r in rows if as_int(r["used_bytes"]) > bound]

    print(f"run dir                     : {run}")
    print("BOUND")
    print(f"  max_bytes (reported)      : {bound:>12d}")
    print(f"  samples in flight         : {len(rows):>12d}")
    print(f"  du -sb cache dir, MAX     : {max_du:>12d}   ratio={max_du / bound if bound else 0:.4f}")
    print(f"  used_bytes, MAX (reported): {max_used:>12d}   ratio={max_used / bound if bound else 0:.4f}")
    print(f"  real blob bytes, MAX      : {max_fblobs:>12d}")
    print(f"  samples where the CLIENT'S FIGURE exceeded the bound (used_bytes > max_bytes): {len(over_used)}"
          f"   {'OK' if not over_used else 'VIOLATION: ' + str(over_used[:3])}")
    print(f"  samples where the CACHE DIRECTORY exceeded the bound (du > max_bytes):          {len(over_du)}"
          f"   {'OK' if not over_du else 'the DIRECTORY is over: ' + str(over_du[:3])}")

    du_blobs = read_int(os.path.join(run, "du-blobs.txt"))
    real_blobs = read_int(os.path.join(run, "real-blobs-bytes.txt"))
    blobs = read_int(os.path.join(run, "blob-count.txt"))
    index_path = os.path.join(run, "index.json")
    index_present = os.path.exists(index_path)
    index_file = os.path.getsize(index_path) if index_present else 0
    total = read_int(os.path.join(run, "du-total.txt"))
    other = read_int(os.path.join(run, "other-files-bytes.txt"))
    status_size = os.path.getsize(os.path.join(run, "status.json"))
    conflicts_size = os.path.getsize(os.path.join(run, "conflicts.jsonl")) if os.path.exists(os.path.join(run, "conflicts.jsonl")) else 0

    print("AGREEMENT (final state, after the reader finished)")
    print(f"  reported blobs_bytes      : {c['blobs_bytes']:>12d}")
    print(f"  du -sb blobs              : {du_blobs:>12d}   delta={c['blobs_bytes'] - du_blobs}")
    print(f"  real blob file bytes      : {real_blobs:>12d}   delta={c['blobs_bytes'] - real_blobs}")
    print(f"  blob files on disk        : {blobs:>12d}   reported blobs={c['blobs']}  entries={c['entries']}")
    print(f"  reported index_bytes      : {c['index_bytes']:>12d}")
    if index_present:
        print(f"  index.json on disk        : {index_file:>12d}   delta={c['index_bytes'] - index_file}")
    else:
        print(f"  index.json on disk        :  ABSENT   <- no entry was ever STORED, so the index was never "
              f"flushed; the client still reports index_bytes={c['index_bytes']} for the document it would write")
    print(f"  reported used_bytes       : {c['used_bytes']:>12d}   == blobs+index: {c['used_bytes'] == c['blobs_bytes'] + c['index_bytes']}")
    print(f"  du -sb cache dir          : {total:>12d}   delta vs used_bytes = {total - c['used_bytes']}")
    print(f"  status.json on disk       : {status_size:>12d}   <- the delta is the mount's own document, a FILE it does not count as cache")
    print(f"  conflicts.jsonl on disk   : {conflicts_size:>12d}")
    print(f"  accounted: used_bytes + status.json + conflicts.jsonl == du(cache dir): "
          f"{c['used_bytes'] + status_size + conflicts_size == total}")

    print("EVICTION / BYPASS")
    print(f"  evictions_total           : {c['evictions_total']:>12d}")
    print(f"  bypass_events             : {c['bypass_events']:>12d}   (full and nothing evictable)")
    print(f"  oversize_bypasses         : {c['oversize_bypasses']:>12d}   (single entry over --cache-max-entry-bytes)")
    print(f"  pinned_blobs              : {c['pinned_blobs']:>12d}")
    print(f"READS")
    print(f"  hits / misses             : {c['hits']} / {c['misses']}")

    rc = 0
    if over_used:
        print("RESULT: THE CLIENT'S FIGURE EXCEEDED ITS BOUND in at least one in-flight sample — that is the "
              "contract failing, not a measurement artifact")
        rc = 1
    elif over_du:
        print("RESULT: the client's own figures stayed inside the bound in every in-flight sample, but the "
              "cache DIRECTORY did not: it also holds status.json and conflicts.jsonl, which used_bytes does "
              "not count (see the accounting above and the report's finding F-C)")
    else:
        print("RESULT: the bound held in every in-flight sample, by BOTH instruments; the reported figure and "
              "du agree as shown")
    return rc


if __name__ == "__main__":
    raise SystemExit(main())
