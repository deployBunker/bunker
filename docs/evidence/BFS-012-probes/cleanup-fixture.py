#!/usr/bin/env python3
"""cleanup.py — remove EXPLICIT paths under a fixed root, and say what went.

This fleet's root filesystem runs tight and BFS-012's fixture is deliberately
large, so the row requires the fixture to be cleaned up and `df` checked before and
after. `rm -rf` is not available to a worker here (and should not be: a glob in a
recursive delete is how a wave loses a sibling's work), so this refuses anything
outside the run root, takes whole paths rather than patterns, and prints the size it
reclaimed plus `df` around the removal.

usage: cleanup.py --root /tmp/bfs012 --df-before "1.6T" path [path ...]
"""
from __future__ import annotations

import os
import shutil
import subprocess
import sys


def df() -> str:
    out = subprocess.run(["df", "-h", "/"], capture_output=True, text=True).stdout.splitlines()[-1]
    return out


def main() -> int:
    if len(sys.argv) < 3:
        print(__doc__, file=sys.stderr)
        return 2
    root = os.path.realpath(sys.argv[1])
    paths = [os.path.realpath(p) for p in sys.argv[2:]]
    print(f"df before: {df()}")
    total = 0
    for p in paths:
        if not p.startswith(root + os.sep):
            print(f"REFUSED (outside {root}): {p}", file=sys.stderr)
            return 3
        if os.path.isfile(p):
            size = os.path.getsize(p)
            os.remove(p)
            print(f"  removed {p}  ({size} B, a file)")
            total += size
            continue
        size = 0
        for dirpath, _dirs, files in os.walk(p):
            for f in files:
                try:
                    size += os.path.getsize(os.path.join(dirpath, f))
                except OSError:
                    pass
        if os.path.exists(p):
            shutil.rmtree(p, ignore_errors=True)
        print(f"  removed {p}  ({size / (1 << 20):.1f} MiB)")
        total += size
    print(f"reclaimed: {total / (1 << 20):.1f} MiB")
    print(f"df after : {df()}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
