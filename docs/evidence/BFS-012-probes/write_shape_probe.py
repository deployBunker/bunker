#!/usr/bin/env python3
"""write_shape_probe.py — WHICH writes the mount actually accepts (BFS-012).

BFS-012's conflict arms need to write an EXISTING file through the mount, so the
first question is whether the mount accepts that at all. It does not: every shape
that opens an existing file, writes into it and closes it comes back EOPNOTSUPP
(errno 95), because `node.Open` hands back a read handle whatever the open flags
say, and a write handle is only ever created by `node.Create` (a NEW path). The
only reachable write path for an existing file is `truncate` (Setattr size), which
is a read-modify-write published as ONE conditional PUT.

This probe measures each shape on a live mount and reports, for every one, the
errno and whether the bytes actually reached the server's disk — so the finding is
a table rather than an impression, and so the conflict arms rest on a path that is
demonstrably reachable.

usage (run BY mount-arm.sh, which sets $MNT/$TREE):
  write_shape_probe.py [--file src/probe.txt]
"""
from __future__ import annotations

import argparse
import hashlib
import os
import sys


def sha_path(p: str) -> str:
    h = hashlib.sha256()
    with open(p, "rb") as fh:
        for b in iter(lambda: fh.read(1 << 20), b""):
            h.update(b)
    return h.hexdigest()


def attempt(label: str, fn) -> dict:
    try:
        fn()
        return {"label": label, "rc": "ok", "errno": "", "detail": ""}
    except OSError as e:
        return {"label": label, "rc": "error", "errno": f"{e.errno} {os.strerror(e.errno) if e.errno else ''}",
                "detail": e.__class__.__name__}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mount", default=os.environ.get("MNT", ""))
    ap.add_argument("--tree", default=os.environ.get("TREE", ""))
    args = ap.parse_args()
    if not args.mount or not args.tree:
        raise SystemExit("--mount/--tree required (or run under mount-arm.sh)")

    rel = "src/probe.txt"
    mnt = os.path.join(args.mount, rel)
    srv = os.path.join(args.tree, rel)
    print(f"mount {args.mount}")
    print(f"server tree {args.tree}")
    print()
    print(f"{'shape':<44} {'rc':<6} {'errno':<22} reached the server's disk?")
    print("-" * 110)

    rows = []

    def inplace_write(path: str, body: bytes) -> None:
        # The shape a shell's `>` or an editor's save uses on an existing file:
        # open without O_CREAT-only, write into it, close (the publication point).
        with open(path, "r+b", buffering=0) as fh:
            fh.write(body)
            fh.flush()

    def append_write(path: str, body: bytes) -> None:
        with open(path, "ab") as fh:
            fh.write(body)

    def trunc_file(path: str) -> None:
        with open(path, "wb") as fh:
            fh.write(b"z" * 32)

    def report(r: dict, landed: str) -> None:
        rows.append((r, landed))
        print(f"{r['label']:<44} {r['rc']:<6} {r['errno']:<22} {landed}")
        if r["detail"]:
            print(f"    ({r['detail']})")

    # 1. CREATE a new file (the path does not exist): this is node.Create.
    body_new = b"created-through-the-mount\n"
    r = attempt("open(new,'wb') -> write -> close",
                lambda: open(mnt, "wb").write(body_new))
    landed = "yes" if os.path.exists(srv) and open(srv, "rb").read() == body_new else "NO"
    report(r, landed)
    if os.path.exists(srv):
        try:
            os.remove(srv)   # server-side cleanup of the fixture we just made
        except OSError:
            pass
    if os.path.exists(mnt):
        try:
            os.remove(mnt)
        except OSError:
            pass

    # 2. OVERWRITE an existing file in place (the conflict path's shape).
    open(srv, "wb").write(b"x" * 64)
    r = attempt("open(existing,'r+b') -> write(same len) -> close",
                lambda: inplace_write(mnt, b"y" * 64))
    landed = "yes" if open(srv, "rb").read() == b"y" * 64 else "no"
    report(r, landed)

    # 3. OVERWRITE an existing file with O_TRUNC (printf > file, the shell's shape).
    open(srv, "wb").write(b"x" * 64)
    r = attempt("open(existing,'wb') -> write -> close", lambda: trunc_file(mnt))
    landed = "yes" if open(srv, "rb").read() == b"z" * 32 else "no"
    report(r, landed)

    # 4. APPEND to an existing file.
    open(srv, "wb").write(b"x" * 64)
    r = attempt("open(existing,'ab') -> write -> close", lambda: append_write(mnt, b"appended"))
    landed = "yes" if open(srv, "rb").read().endswith(b"appended") else "no"
    report(r, landed)

    # 5. TRUNCATE an existing file (Setattr size — the one reachable write path).
    open(srv, "wb").write(b"x" * 64)
    r = attempt("truncate(existing, 32)", lambda: os.truncate(mnt, 32))
    landed = "yes" if os.path.getsize(srv) == 32 else "no"
    report(r, landed)

    # 6. the same truncate, back up (read-modify-write grows too)
    r = attempt("truncate(existing, 48)  (grow back)", lambda: os.truncate(mnt, 48))
    landed = "yes" if os.path.getsize(srv) == 48 else "no"
    report(r, landed)

    print()
    ok = [r["label"] for r, _ in rows if r["rc"] == "ok"]
    bad = [r["label"] for r, _ in rows if r["rc"] != "ok"]
    print(f"shapes accepted  : {ok}")
    print(f"shapes refused   : {bad}")
    print(f"sha256 of the server's file after the truncates: {sha_path(srv)[:16]}…")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
