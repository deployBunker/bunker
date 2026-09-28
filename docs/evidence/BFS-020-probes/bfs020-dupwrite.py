#!/usr/bin/env python3
"""BFS-020 probe: TWO publication points on ONE handle, where the FIRST one is the
zero-byte publication of a create.

This is the shape git produces for a ref lock it only ever deletes (it creates the
lock empty, then unlinks it), and it is the shape that made a live `git checkout -b`
print `error: couldn't close 'AUTO_MERGE.lock'` once the mount started publishing
at FLUSH. The question the probe answers is whether the SECOND publication is
refused (and if so, what the server's own view of the file was at that moment).

env: MNT, TREE
"""
import os
import sys

MNT = os.environ["MNT"]
TREE = os.environ["TREE"]

name = "dup.lock"
mpath = os.path.join(MNT, name)
spath = os.path.join(TREE, name)
for p in (spath,):
    try:
        os.unlink(p)
    except OSError:
        pass

print("== D1: create empty (close #1), then write + close #2 on the same handle ==")
fd = os.open(mpath, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
fd2 = os.dup(fd)
os.close(fd)  # close(2) #1 -> FLUSH #1 -> the zero-byte publication
print(f"   after close #1: server has it: {os.path.exists(spath)} "
      f"size={os.path.getsize(spath) if os.path.exists(spath) else 'n/a'}")
os.write(fd2, b"data-after-a-publication-point")
os.close(fd2)  # close(2) #2 -> FLUSH #2 -> the second publication
ok = os.path.exists(spath)
print(f"   after close #2: server size={os.path.getsize(spath) if ok else 'ABSENT'} "
      f"content={open(spath,'rb').read()[:40]!r}")

print("== D2: explicit fsync between two writes on ONE handle (the same shape) ==")
name2 = "sync.lock"
mpath2 = os.path.join(MNT, name2)
spath2 = os.path.join(TREE, name2)
try:
    os.unlink(spath2)
except OSError:
    pass
fd = os.open(mpath2, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
os.write(fd, b"AAAA")
try:
    os.fsync(fd)
    fs = "rc=0"
except OSError as e:
    fs = f"rc={e.errno}"
os.write(fd, b"BBBB")
try:
    os.close(fd)
    cl = "rc=0"
except OSError as e:
    cl = f"rc={e.errno}"
print(f"   fsync {fs} close {cl}; server content={open(spath2,'rb').read()!r} want b'AAAABBBB'")

print("== D3: the control — one write, one close ==")
name3 = "one.lock"
mpath3 = os.path.join(MNT, name3)
spath3 = os.path.join(TREE, name3)
try:
    os.unlink(spath3)
except OSError:
    pass
fd = os.open(mpath3, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
os.write(fd, b"one")
os.close(fd)
print(f"   server content={open(spath3,'rb').read()!r} want b'one'")
sys.exit(0)
