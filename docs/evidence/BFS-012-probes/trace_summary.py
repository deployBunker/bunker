#!/usr/bin/env python3
"""trace_summary.py — read a countproxy request log and answer the question it exists for.

Returns, in order: every request as a table, the counts per method/status, and then
the specific attribution this row needs — for each path, whether a REFUSED write
(412) was followed by a LANDED write (201/204) on the same path. That sequence is
the difference between "the refusal stopped the mutation" and "the refusal was
recorded and then the mutation went through anyway", and it cannot be seen from the
client's own counters.

usage: trace_summary.py <requests.jsonl>
"""
from __future__ import annotations

import json
import sys
from collections import Counter


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__, file=sys.stderr)
        return 2
    recs = []
    with open(sys.argv[1]) as fh:
        for line in fh:
            line = line.strip()
            if line:
                recs.append(json.loads(line))
    print(f"{'#':>4} {'method':<9} {'status':<7} {'req_bytes':>9}  {'if-match':<14} path")
    print("-" * 100)
    for r in recs:
        im = (r.get("if_match") or "").replace("sha256:", "")[:12] or "-"
        print(f"{r['n']:>4} {r['method']:<9} {r['status']:<7} {r['req_bytes']:>9}  {im:<14} {r['path']}")
    print()
    print("counts by method/status:")
    for key, n in sorted(Counter(f"{r['method']} {r['status']}" for r in recs).items()):
        print(f"  {n:>4} x {key}")
    refused = {r["path"]: r["n"] for r in recs if r["method"] in ("PUT",) and r["status"] == 412}
    landed = {r["path"]: r["n"] for r in recs if r["method"] in ("PUT",) and r["status"] in (201, 204)}
    print()
    print("write attribution (the question this trace exists for):")
    if not refused:
        print("  no refused (412) write in this trace")
    for path, n in refused.items():
        after = [m for p, m in landed.items() if p == path and m > n]
        if after:
            print(f"  {path}: REFUSED at request #{n}, then LANDED at request #{min(after)} — "
                  f"a refused write did NOT stop the mutation")
        else:
            print(f"  {path}: REFUSED at request #{n}, never landed afterwards — the refusal held")
    for path, n in landed.items():
        if path not in refused:
            print(f"  {path}: landed at request #{n} with no refusal before it (an ordinary write)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
