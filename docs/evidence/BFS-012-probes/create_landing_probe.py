#!/usr/bin/env python3
"""create_landing_probe.py — does a file CREATED through the mount reach the server?

`write_shape_probe.py` reported the create shape as accepted but its own "reached the
server's disk?" check said NO, and that check ran IMMEDIATELY after close. Whether the
mount's create path delivers the bytes at all is a different (and much larger) claim
than this row is making, so it is measured rather than left ambiguous — and the answer
has two parts: the bytes DO reach the server, and (without an fsync) the PUBLICATION
LAGS THE CLOSE, so a check that runs too early sees nothing.

Four shapes, each checked against the server's own bytes, with a settle:

  1. python `open('wb')` on a NEW path, checked IMMEDIATELY after close;
  2. the same, with an explicit fsync and a settle  -> the control that shows the bytes;
  3. the shell shape `printf > newfile` (a real subprocess, O_CREAT|O_TRUNC);
  4. `open('wb')` a SECOND time on a path that now exists — the shape a build tool
     uses to REWRITE a file, which is the case F-A is about.

usage (under mount-arm.sh, which sets $MNT/$TREE):
  create_landing_probe.py [--settle 3]
"""
from __future__ import annotations

import argparse
import hashlib
import os
import sys
import time


def sha(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mount", default=os.environ.get("MNT", ""))
    ap.add_argument("--tree", default=os.environ.get("TREE", ""))
    ap.add_argument("--settle", type=float, default=3.0)
    args = ap.parse_args()
    if not args.mount or not args.tree:
        raise SystemExit("--mount/--tree required (or run under mount-arm.sh)")

    summary: list[str] = []

    def check(label: str, rel: str, body: bytes) -> None:
        """Report where the bytes are, on the server and through the mount."""
        srv = os.path.join(args.tree, rel)
        mnt = os.path.join(args.mount, rel)
        print(f"--- {label} ---")
        print(f"  server path exists right now : {os.path.exists(srv)}")
        if os.path.exists(srv):
            got = open(srv, "rb").read()
            print(f"  server bytes                 : {len(got)} B sha256={sha(got)[:16]}…  "
                  f"(want {len(body)} B {sha(body)[:16]}…)")
            print(f"  MATCHES                      : {got == body}")
            summary.append(f"{rel}: exists=True matches={got == body}")
        else:
            summary.append(f"{rel}: exists=False matches=ABSENT")
        try:
            print(f"  the mount lists it           : {os.path.exists(mnt)}")
        except OSError as e:
            print(f"  the mount stat raised        : {e}")

    def remove_through_mount(rel: str) -> None:
        mnt = os.path.join(args.mount, rel)
        try:
            os.remove(mnt)
            print(f"  (removed {rel} through the mount)")
        except OSError as e:
            print(f"  (removing {rel} through the mount raised: {e.__class__.__name__} errno={e.errno})")

    # 1. python open('wb') on a NEW path — checked IMMEDIATELY after close.
    rel1 = "src/created-python.txt"
    body1 = b"created by python open(wb)\n"
    with open(os.path.join(args.mount, rel1), "wb") as fh:
        fh.write(body1)
    check("1. python open('wb') on a NEW path — check IMMEDIATELY after close", rel1, body1)

    # 2. the same shape, published explicitly: fsync + settle.
    rel2 = "src/created-fsync.txt"
    body2 = b"created with an explicit fsync\n"
    fh = open(os.path.join(args.mount, rel2), "wb")
    fh.write(body2)
    fh.flush()
    os.fsync(fh.fileno())
    fh.close()
    time.sleep(args.settle)
    check(f"2. python open('wb') + fsync, checked after a {args.settle}s settle", rel2, body2)

    # 3. the shell shape on a NEW path (a real subprocess: O_CREAT|O_TRUNC).
    rel3 = "src/created-shell.txt"
    payload3 = b"created by printf"
    rc = os.system(f"printf '%s' '{payload3.decode()}' > '{os.path.join(args.mount, rel3)}'")
    time.sleep(args.settle)
    print(f"--- 3. printf > newfile — rc={rc}, checked after a {args.settle}s settle ---")
    check("3. printf > newfile", rel3, payload3)

    # 4. the REWRITE shape: the path exists now, so this is the case F-A is about.
    print(f"--- 4. the REWRITE shape: open('wb') on a path that EXISTS ---")
    rel4 = rel3
    try:
        with open(os.path.join(args.mount, rel4), "wb") as fh:
            fh.write(b"rewritten\n")
        print("  open('wb') on an existing path : returned cleanly (no error)")
        summary.append(f"{rel4} rewrite: accepted")
    except OSError as e:
        print(f"  open('wb') on an existing path : REFUSED — {e.__class__.__name__} "
              f"errno={e.errno} ({os.strerror(e.errno) if e.errno else ''})")
        summary.append(f"{rel4} rewrite: refused errno={e.errno}")
    time.sleep(1.0)
    check("4. after the rewrite attempt", rel3, b"rewritten\n")

    # leave the fixture clean: remove everything this probe created
    for rel in (rel1, rel2, rel3):
        remove_through_mount(rel)
        srv = os.path.join(args.tree, rel)
        if os.path.exists(srv):
            os.remove(srv)

    print()
    print("SUMMARY (the server's own disk, per shape):")
    for s in summary:
        print(f"  {s}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
