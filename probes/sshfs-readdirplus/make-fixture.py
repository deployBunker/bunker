#!/usr/bin/env python3
"""Create Fixture A for MOUNT-015: a git tree with known modified/deleted/untracked state."""
import os
import subprocess
import sys

ROOT = sys.argv[1] if len(sys.argv) > 1 else "/tmp/m015-fixture"


def run(*args, **kw):
    return subprocess.run(args, cwd=ROOT, check=True, capture_output=True, text=True, **kw).stdout


os.makedirs(ROOT, exist_ok=True)

if not os.path.isdir(os.path.join(ROOT, ".git")):
    run("git", "init", "-q", "-b", "main")
    run("git", "config", "user.email", "fixture@example.com")
    run("git", "config", "user.name", "fixture")
    n = 0
    dirs = [""] + [f"sub{i}" for i in range(1, 11)]
    for d in dirs:
        os.makedirs(os.path.join(ROOT, d), exist_ok=True)
        for i in range(15):
            p = os.path.join(ROOT, d, f"file{i:02d}.txt")
            with open(p, "w") as f:
                f.write(f"content {d}/{i}\n" * 5)
            n += 1
    run("git", "add", "-A")
    run("git", "commit", "-q", "-m", "fixture: 150 files in 11 directories")
    print(f"fixture initialised, {n} files")

# Known dirty state: 2 modified, 2 deleted, 1 untracked file, 1 untracked dir
with open(os.path.join(ROOT, "file00.txt"), "a") as f:
    f.write("MODIFIED\n")
with open(os.path.join(ROOT, "sub3", "file07.txt"), "a") as f:
    f.write("MODIFIED\n")
for p in ("sub5/file04.txt", "sub5/file05.txt"):
    full = os.path.join(ROOT, p)
    if os.path.exists(full):
        os.unlink(full)
with open(os.path.join(ROOT, "untracked-new.txt"), "w") as f:
    f.write("untracked\n")
os.makedirs(os.path.join(ROOT, "sub9", "newdir"), exist_ok=True)
with open(os.path.join(ROOT, "sub9", "newdir", "a.txt"), "w") as f:
    f.write("x\n")

print("--- git status --porcelain (local baseline) ---")
print(run("git", "status", "--porcelain"), end="")
print("--- sha256 of that listing ---")
print(run("git", "status", "--porcelain").encode(), end="")
