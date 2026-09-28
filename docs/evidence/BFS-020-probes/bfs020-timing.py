#!/usr/bin/env python3
"""BFS-020 timing probe: when does a created name become VISIBLE, measured from
close(2) returning?  Three clocks per iteration:

  1. the mount call's own cost (open+write, then close);
  2. how long AFTER close(2) returned the name appears on the served tree's own
     disk (the native view) -- if the publication ran inside the close, this is
     ~0;
  3. whether a rename through the mount, issued immediately after close(2),
     succeeds, and its errno when it does not.

env: MNT, TREE, N (iterations)
"""
import os
import time

MNT = os.environ["MNT"]
TREE = os.environ["TREE"]
N = int(os.environ.get("N", "5"))


def now() -> float:
    return time.monotonic()


def wait_exists(path: str, deadline: float = 5.0):
    t0 = now()
    end = t0 + deadline
    while now() < end:
        if os.path.exists(path):
            return now() - t0
    return None


def one(i: int) -> None:
    name = f"t{i}.lock"
    mpath = os.path.join(MNT, name)
    spath = os.path.join(TREE, name)
    dst = os.path.join(MNT, f"t{i}.final")
    if os.path.exists(spath):
        os.unlink(spath)
    t0 = now()
    fd = os.open(mpath, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
    os.write(fd, b"x")
    t_write = now()
    os.close(fd)
    t_close = now()
    # THE CALLER'S SHAPE: rename immediately, with nothing in between. This is the
    # pair the row files — the errno here is the defect.
    t_r0 = now()
    try:
        os.rename(mpath, dst)
        rc = 0
    except OSError as e:
        rc = e.errno
    t_r1 = now()
    # ...and then WHEN the name became visible to a following operation, measured
    # on the served tree's own clock (the window's length, on whichever binary).
    vis = wait_exists(spath, 5.0)
    if vis is None:
        vis = wait_exists(dst, 1.0)
        vis_s = ("moved-then-visible %.3fms" % (1e3 * vis)) if vis is not None else "NEVER(>6s)"
    else:
        vis_s = "%.3fms" % (1e3 * vis)
    print(f"  {name}: open+write={1e3*(t_write-t0):8.3f}ms close={1e3*(t_close-t_write):8.3f}ms "
          f"rename_immediately_after_close_rc={rc:<4} rename={1e3*(t_r1-t_r0):7.3f}ms "
          f"visible_from_close={vis_s}")
    time.sleep(2)
    for p in (mpath, dst):
        try:
            os.unlink(p)
        except OSError:
            pass


print("mount   =", MNT)
print("server  =", TREE)
for i in range(N):
    one(i)
