#!/usr/bin/env python3
"""mkfixture.py — the deterministic fixture tree for the BFS-026 arms.

The tree is small on purpose: this row measures a CHANNEL, not a bound, so what
matters is that every path is known and reproducible, not that the tree is big.
A second mode (`--burst N`) creates N tiny files, which the cap arm uses: the
4096-path spill has to be driven with the real declared cap, not a lowered one.

usage:
  mkfixture.py <dir>                    # the plain tree (7 files, 3 dirs)
  mkfixture.py <dir> --burst 2000       # the plain tree plus N tiny files
  mkfixture.py <dir> --add-burst 4100   # N more burst files into an EXISTING tree
                                        # (the cap arm needs the real 4096 cap driven
                                        #  after a baseline poll, not a lowered one)
"""
import os
import sys


def write(path, body):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(body)


def main(argv):
    if len(argv) < 2:
        print(__doc__)
        return 2
    root = os.path.abspath(argv[1])
    burst = 0
    add_only = False
    if "--burst" in argv:
        burst = int(argv[argv.index("--burst") + 1])
    if "--add-burst" in argv:
        burst = int(argv[argv.index("--add-burst") + 1])
        add_only = True
    if not add_only and os.path.exists(root) and os.listdir(root):
        print("mkfixture: refusing to reuse a non-empty %s" % root)
        return 2
    os.makedirs(root, exist_ok=True)

    if not add_only:
        write(os.path.join(root, "src", "main.go"), "package main\n\nfunc main() {}\n")
        write(os.path.join(root, "src", "util.go"), "package main\n\nfunc util() {}\n")
        write(os.path.join(root, "src", "deep", "nested.go"), "package deep\n")
        write(os.path.join(root, "README.md"), "# bfs026 fixture\n")
        write(os.path.join(root, "cached.txt"), "cached-version-1\n")
        write(os.path.join(root, "never.txt"), "never-version-1\n")
        write(os.path.join(root, "snap.txt"), "snap-version-1\n")

    if burst:
        for i in range(burst):
            write(os.path.join(root, "burst", "f%05d.txt" % i), "b\n")

    count = sum(len(files) for _, _, files in os.walk(root))
    print("fixture %s: %d files%s" % (root, count, (" (+%d burst)" % burst) if burst else ""))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
