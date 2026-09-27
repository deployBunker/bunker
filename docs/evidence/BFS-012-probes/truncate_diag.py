#!/usr/bin/env python3
"""truncate_diag.py — step-by-step trace of ONE truncate through the mount.

Written because the small-bound arm reported BOTH "40 conflicts recorded" AND "40
writes landed", which cannot both be true: either the truncate is refused (an error
must reach the caller) or it lands (no conflict may be recorded). A probe that
measures a contradiction has to be taken apart before any number from it is used, so
this prints, for each of a few iterations, every observable step:

  * the base the client holds (did phase 1's read record a `served` hash?),
  * the exact OSError (or none) from os.truncate,
  * the server file's size and sha256 after the attempt,
  * the conflict count and status.refusals_total after the attempt.

usage (under mount-arm.sh, which sets $MNT/$TREE/$CDIR): truncate_diag.py [--iters 3]
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import time


def sha(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def status(cache_dir: str) -> dict:
    try:
        with open(os.path.join(cache_dir, "status.json")) as fh:
            return json.load(fh)
    except Exception:
        return {}


def n_conflicts(cache_dir: str) -> int:
    p = os.path.join(cache_dir, "conflicts.jsonl")
    if not os.path.exists(p):
        return 0
    with open(p) as fh:
        return sum(1 for line in fh if line.strip())


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mount", default=os.environ.get("MNT", ""))
    ap.add_argument("--tree", default=os.environ.get("TREE", ""))
    ap.add_argument("--cache-dir", default=os.environ.get("CDIR", ""))
    ap.add_argument("--iters", type=int, default=3)
    ap.add_argument("--mode", choices=["edit", "noedit"], default="edit",
                    help="edit: change the server's bytes out of band before each truncate (the refusal case); "
                         "noedit: leave them alone (the shape of a truncate whose base is accurate)")
    args = ap.parse_args()
    if not args.mount or not args.tree or not args.cache_dir:
        raise SystemExit("--mount/--tree/--cache-dir required (or run under mount-arm.sh)")

    rel = "src/target.txt"
    mnt = os.path.join(args.mount, rel)
    srv = os.path.join(args.tree, rel)

    print("=== step 0: the read through the mount that fixes the client's base ===")
    served = open(mnt, "rb").read()
    print(f"  read through the mount : {len(served)} B sha256={sha(served)[:16]}…")

    for k in range(args.iters):
        print(f"=== iteration {k} (mode={args.mode}) ===")
        if args.mode == "edit":
            out_of_band = ((b"DIAG-%04d-" % k) + b"q" * 50)[:64].ljust(64, b"q")
            with open(srv, "wb") as fh:
                fh.write(out_of_band)
        else:
            out_of_band = open(srv, "rb").read()
        st = os.stat(srv)
        print(f"  server set out-of-band : {len(out_of_band)} B sha256={sha(out_of_band)[:16]}… "
              f"mtime_ns={st.st_mtime_ns}")
        before_conf = n_conflicts(args.cache_dir)
        err = "NONE"
        try:
            os.truncate(mnt, 32)
        except OSError as e:
            err = f"{e.__class__.__name__} errno={e.errno} ({os.strerror(e.errno) if e.errno else ''})"
        time.sleep(1.2)
        now = open(srv, "rb").read()
        st2 = os.stat(srv)
        conf = n_conflicts(args.cache_dir)
        full = status(args.cache_dir)
        print(f"  os.truncate(mnt, 32)   : {err}")
        print(f"  server file after      : {len(now)} B sha256={sha(now)[:16]}… mtime_ns={st2.st_mtime_ns}")
        print(f"  conflicts.jsonl        : {before_conf} -> {conf}   status.conflicts={json.dumps(full.get('conflicts', {}).get('refusals_total'))}")
        print(f"  reinterpretation       : "
              f"{'REFUSED (server bytes unchanged)' if now == out_of_band else 'LANDED (server bytes are now 32 B)' if now == out_of_band[:32] else 'SOMETHING ELSE'}")
    # leave the fixture in a known state for whatever runs next
    with open(srv, "wb") as fh:
        fh.write(b"diag-done\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
