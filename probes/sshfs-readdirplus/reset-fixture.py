#!/usr/bin/env python3
"""Reset Fixture A to its known dirty state (undo what a probe wrote through the mount)."""
import os
import shutil
import subprocess
import sys

ROOT = sys.argv[1] if len(sys.argv) > 1 else "/tmp/m015-fixture"


def git(*args):
    return subprocess.run(("git",) + args, cwd=ROOT, capture_output=True, text=True)


# undo probe additions
git("checkout", "--", "sub1/file01.txt")
for extra in ("freshness",):
    p = os.path.join(ROOT, extra)
    if os.path.isdir(p):
        shutil.rmtree(p)

out = git("status", "--porcelain")
print(out.stdout, end="")
if out.returncode != 0:
    print("git status failed:", out.stderr, file=sys.stderr)
    sys.exit(1)
