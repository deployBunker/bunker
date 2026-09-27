#!/usr/bin/env python3
"""mkfixture.py — build a DETERMINISTIC source tree and print its exact byte total.

Written for BFS-012 because the BFS-009 fixture generator was not committed with
its evidence, so the 400 MiB arm BFS-009 measured could not be rebuilt by anyone
who was not there. This one is self-contained: give it a directory, get the same
tree, the same bytes and the same sha256 manifest every time, on any host.

Every byte derives from the file's own path by a sha256 chain, so two runs on two
hosts produce identical content and the manifest verifies it:

    python3 mkfixture.py /tmp/tree --bulk 640 --bulk-bytes 1048576 --small 200
    (cd /tmp/tree && sha256sum -c ../manifest-sha256.txt)

The printed total is RECOMPUTED from a fresh walk of what was just written, so a
generator that mis-states its own fixture fails loudly instead of quietly
(C-1's arithmetic slip is the reason this is a check and not a print).
"""
from __future__ import annotations

import argparse
import hashlib
import os
import sys
from pathlib import Path


def chunk_bytes(seed: str, n: int, block: int = 1 << 20) -> bytes:
    """Deterministic pseudo-random bytes of length n, from a path seed."""
    out = bytearray()
    counter = 0
    while len(out) < n:
        h = hashlib.sha256(f"{seed}:{counter}".encode()).digest()
        out.extend(h * (len(h) // 1))  # 32 bytes per digest
        counter += 1
    del out[n:]
    return bytes(out)


def write_file(root: Path, rel: str, data: bytes, manifest: list[str]) -> int:
    p = root / rel
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_bytes(data)
    manifest.append(f"{hashlib.sha256(data).hexdigest()}  {rel}")
    return len(data)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("root", help="fixture root (created if absent)")
    ap.add_argument("--bulk", type=int, default=640, help="number of 1-MiB-class files")
    ap.add_argument("--bulk-bytes", type=int, default=1 << 20)
    ap.add_argument("--small", type=int, default=200)
    ap.add_argument("--small-bytes", type=int, default=300)
    ap.add_argument("--manifest", default=None, help="manifest path (default <root>/../manifest-sha256.txt)")
    ap.add_argument("--force", action="store_true", help="write into a directory that already has files")
    args = ap.parse_args()

    root = Path(args.root)
    if root.exists() and any(root.iterdir()) and not args.force:
        print(f"refusing to reuse non-empty {root} (deterministic fixture, fresh directory per run)", file=sys.stderr)
        return 2
    root.mkdir(parents=True, exist_ok=True)
    manifest: list[str] = []

    total = 0
    for i in range(args.bulk):
        rel = f"src/bulk/f{i:04d}.dat"
        total += write_file(root, rel, chunk_bytes(rel, args.bulk_bytes), manifest)
    for i in range(args.small):
        rel = f"src/small/s{i:04d}.txt"
        total += write_file(root, rel, chunk_bytes(rel, args.small_bytes), manifest)
    total += write_file(root, "README.md", b"# bfs-012 fixture\n\ndeterministic tree\n", manifest)

    man = Path(args.manifest) if args.manifest else root.parent / "manifest-sha256.txt"
    man.write_text("\n".join(sorted(manifest)) + "\n")

    # Recompute from what is on disk rather than trusting the accumulator.
    walked = 0
    files = 0
    for dirpath, _dirnames, filenames in os.walk(root):
        for fn in filenames:
            walked += os.path.getsize(os.path.join(dirpath, fn))
            files += 1

    print(f"root          : {root}")
    print(f"bulk files    : {args.bulk} x {args.bulk_bytes} B")
    print(f"small files   : {args.small} x {args.small_bytes} B")
    print(f"files (walk)  : {files}")
    print(f"total_bytes   : {walked}")
    print(f"MiB           : {walked / (1 << 20):.1f}")
    print(f"manifest      : {man} ({len(manifest)} lines)")
    if walked != total:
        print(f"FIXTURE MIS-STATEMENT: accumulator {total} != walk {walked}", file=sys.stderr)
        return 3
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
