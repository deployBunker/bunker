#!/usr/bin/env python3
"""mkconflict_tree.py — the deterministic tree for BFS-012's conflict arms.

Three files, all SMALL and all of the same 64-byte length class, because the arms
turn on what a metadata comparison can and cannot see:

  src/target.txt  the file the arms read through the mount and then write back to
  src/other.txt   a file that is never read through the mount   (fetch-then-check)
  src/third.txt   a second never-read file, for the size-changed arm

Prints each file's size and sha256, which the arms assert against afterwards.
"""
from __future__ import annotations

import hashlib
import os
import sys

FILES = {
    "src/target.txt": b"ORIGINAL-CONTENT-" + b"A" * 46 + b"\n",   # 64 B
    "src/other.txt": b"other-file-untouched" + b" " * 42 + b"\n",  # 64 B
    "src/third.txt": b"third-file" + b" " * 52 + b"\n",            # 63 B
}


def main() -> int:
    root = sys.argv[1] if len(sys.argv) > 1 else "/tmp/bfs012/conflict-tree"
    if os.path.isdir(root) and any(os.scandir(root)):
        print(f"refusing to reuse non-empty {root}", file=sys.stderr)
        return 2
    for rel, data in FILES.items():
        p = os.path.join(root, rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "wb") as fh:
            fh.write(data)
        print(f"{rel}  size={len(data)}  sha256={hashlib.sha256(data).hexdigest()}")
    print(f"root: {root}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
