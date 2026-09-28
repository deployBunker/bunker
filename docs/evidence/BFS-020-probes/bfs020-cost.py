#!/usr/bin/env python3
"""BFS-020 cost probe: what does publishing INSIDE the close(2) reply cost?

The fix moves the publication from RELEASE (which the kernel does not wait for, so
the round trip used to overlap the caller's next work) to FLUSH (which it does).
The price is therefore one request round trip's latency inside every close(2) of a
created file. This measures it, as a number, on whichever binary it is given:

  N create+write+close cycles, each reporting the CLOSE's own cost, plus the total,
  the median and p95, and a read-back that asserts every file really landed with
  the right bytes (a fast wrong answer is not a measurement).

env: MNT, TREE, N (default 30)
"""
import os
import statistics
import time

MNT = os.environ["MNT"]
TREE = os.environ["TREE"]
N = int(os.environ.get("N", "30"))


def now() -> float:
    return time.monotonic()


closes = []
unclean = 0
total0 = now()
for i in range(N):
    name = f"cost{i:03d}.txt"
    body = (f"payload {i}\n" * 4).encode()
    mp = os.path.join(MNT, name)
    t = now()
    fd = os.open(mp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
    os.write(fd, body)
    t1 = now()
    os.close(fd)
    t2 = now()
    closes.append((t2 - t1) * 1e3)
    if not os.path.exists(os.path.join(TREE, name)):
        print(f"  {name}: NOT ON THE SERVED TREE AFTER CLOSE (this binary does not publish at FLUSH)")
    else:
        with open(os.path.join(TREE, name), "rb") as f:
            got = f.read()
        if got != body:
            print(f"  {name}: WRONG BYTES on the served tree ({len(got)} of {len(body)})")
    # The host-side cleanup is best-effort: on a binary that does not publish at
    # FLUSH the name is not on the served tree yet, so the mount's own unlink can
    # answer ENOENT — which is itself part of this row's measurement.
    for p in (mp, os.path.join(TREE, name)):
        try:
            os.unlink(p)
        except OSError as e:
            if p == mp:
                unclean += 1
total = (now() - total0) * 1e3

closes_sorted = sorted(closes)
p95 = closes_sorted[min(len(closes_sorted) - 1, int(0.95 * len(closes_sorted)))]
print(f"cost       = {N} create+write+close cycles to the served tree")
print(f"close      : median {statistics.median(closes):.3f} ms, mean {statistics.mean(closes):.3f} ms, "
      f"p95 {p95:.3f} ms, min {closes_sorted[0]:.3f} ms, max {closes_sorted[-1]:.3f} ms")
print(f"the whole  : {total:.1f} ms for {N} files ({total/N:.2f} ms per file, including the host-side unlinks)")
print(f"unlink     : {unclean} of {N} names could not be removed through the mount "
      f"(ENOENT: the name is not on the served tree yet)")
