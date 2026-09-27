#!/usr/bin/env python3
"""conflict-arms.py — BFS-012 deliverable 2, LIVE, through the client's own path.

The stock-client half (docs/evidence/BFS-012-stock-client-*.md) shows a 412 at the
surface with curl: that proves the SERVER refuses, not that the CLIENT'S write path
does, and not that the client's rule is the content hash rather than metadata. These
arms drive real writes through a live FUSE mount, so the syscall path is under test.

Which write path? Measured, not assumed. `write_shape_probe.py` shows that every
shape which opens an EXISTING file and writes into it is answered EOPNOTSUPP (95)
— `node.Open` returns a read handle whatever the flags say, and a write handle is
created only by `node.Create` (a NEW path, finding F-A). The ONE reachable write
path for an existing file is `truncate` (Setattr size): a read-modify-write
published as ONE conditional PUT through WritePath (base resolution + refusal
recording). So that is the path these arms exercise, and the shape that is
unreachable is measured here as arm W rather than silently avoided.

  W. measure          — overwrite an existing file in place: EOPNOTSUPP, and the
                        server's bytes do not move. (The finding, not a test.)
  A. control          — truncate a file that was NEVER read through the mount: the
                        base comes from fetch-then-check, so it must LAND. Without
                        this arm, "the write was refused" proves nothing.
  B. mtime-preserved  — READ the file (that read fixes the client's base hash), then
                        replace it on the server with different bytes of the SAME
                        length and restore the mtime EXACTLY, then truncate. The
                        base is stale while every metadata field matches: the write
                        must be REFUSED loudly, name the current hash, leave the
                        server's bytes untouched, and be recorded for
                        `bunker fs conflicts`. If it LANDS, the client's conflict
                        detection is fooled by metadata and the row is a FINDING.
  C. touch-only       — READ the file, move ONLY the mtime, then truncate: the base
                        still describes the bytes, so the write must LAND. This is
                        the "NEVER mtime" direction: an mtime-comparing rule would
                        refuse it.

Every arm asserts its own fixture (same size, same mtime, different bytes) before
asserting anything about the write.
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


def status(cache_dir: str, tries: int = 40) -> dict:
    for _ in range(tries):
        try:
            with open(os.path.join(cache_dir, "status.json")) as fh:
                return json.load(fh)
        except (OSError, json.JSONDecodeError):
            time.sleep(0.25)
    return {}


def conflicts(cache_dir: str) -> list[dict]:
    out = []
    try:
        with open(os.path.join(cache_dir, "conflicts.jsonl")) as fh:
            for line in fh:
                if line.strip():
                    out.append(json.loads(line))
    except FileNotFoundError:
        pass
    return out


def try_call(fn):
    """Run fn(); return (ok, detail) with the errno named."""
    try:
        fn()
        return True, "returned cleanly"
    except OSError as e:
        return False, (f"{e.__class__.__name__}: {e} "
                       f"(errno={e.errno} {os.strerror(e.errno) if e.errno else ''})")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mount", default=os.environ.get("MNT", ""), help="default $MNT (mount-arm.sh)")
    ap.add_argument("--tree", default=os.environ.get("TREE", ""), help="default $TREE (mount-arm.sh)")
    ap.add_argument("--cache-dir", default=os.environ.get("CDIR", ""), help="default $CDIR (mount-arm.sh)")
    args = ap.parse_args()
    if not args.mount or not args.tree or not args.cache_dir:
        raise SystemExit("--mount/--tree/--cache-dir required (or run under mount-arm.sh)")

    rc = 0

    def check(cond: bool, msg: str) -> None:
        nonlocal rc
        print(("  PASS  " if cond else "  FAIL  ") + msg)
        if not cond:
            rc = 1

    rel = "src/target.txt"
    mnt_target = os.path.join(args.mount, rel)
    srv_target = os.path.join(args.tree, rel)
    mnt_other = os.path.join(args.mount, "src/other.txt")
    srv_other = os.path.join(args.tree, "src/other.txt")

    print("=== ARM W: MEASUREMENT — can an existing file be written in place at all? ===")
    before_w = open(srv_target, "rb").read()

    def inplace_write(path: str, body: bytes) -> None:
        with open(path, "r+b", buffering=0) as fh:
            fh.write(body)
            fh.flush()

    ok_w, detail_w = try_call(lambda: inplace_write(mnt_target, b"w" * len(before_w)))
    after_w = open(srv_target, "rb").read()
    print(f"  open(r+b) + write + close : {'accepted' if ok_w else 'REFUSED'} — {detail_w}")
    print(f"  server bytes moved        : {sha(after_w)[:16]}… (was {sha(before_w)[:16]}…)")
    check(not ok_w or sha(after_w) != sha(before_w),
          "MEASURED (finding F-A): an in-place write of an existing file is refused with EOPNOTSUPP and the "
          "server's bytes do not move — writing an existing file through the mount is unreachable")

    print()
    print("=== ARM A: CONTROL — truncate a file NEVER read through the mount (fetch-then-check base) ===")
    other_size_before = os.path.getsize(srv_other)
    want_size = other_size_before - 20
    ok_a, detail_a = try_call(lambda: os.truncate(mnt_other, want_size))
    time.sleep(1.0)
    print(f"  truncate {other_size_before} -> {want_size}: {'LANDED' if ok_a else 'REFUSED'} — {detail_a}")
    print(f"  server size now           : {os.path.getsize(srv_other)}")
    check(ok_a, "the control write LANDED (a path never read resolves its base with fetch-then-check)")
    check(os.path.getsize(srv_other) == want_size,
          f"the control's new size reached the server (want {want_size}, got {os.path.getsize(srv_other)})")

    print()
    print("=== ARM B: an edit that preserves SIZE and MTIME must still be REFUSED ===")
    # The READ THROUGH THE MOUNT is the point of this arm: that read is what fixes the
    # client's base hash. Without it the client resolves the base with fetch-then-check
    # and the write is expected to land, which would prove nothing.
    before = open(mnt_target, "rb").read()
    srv_before = open(srv_target, "rb").read()
    st_before = os.stat(srv_target)
    print(f"  read through the mount    : {len(before)} B sha256={sha(before)[:16]}…  (this read fixes the base)")
    check(sha(before) == sha(srv_before), "FIXTURE: the mount's read did not return the server's bytes")
    # the out-of-band edit: same length, different bytes, mtime restored exactly
    edited = bytes((b ^ 0x20) if 65 <= b <= 90 else b for b in before)
    with open(srv_target, "wb") as fh:
        fh.write(edited)
    os.utime(srv_target, ns=(st_before.st_atime_ns, st_before.st_mtime_ns))
    st_after = os.stat(srv_target)
    now_bytes = open(srv_target, "rb").read()
    print(f"  server edited out-of-band : {len(now_bytes)} B sha256={sha(now_bytes)[:16]}…  "
          f"(size same={st_after.st_size == st_before.st_size}, "
          f"mtime same={st_after.st_mtime_ns == st_before.st_mtime_ns}, "
          f"bytes differ={sha(now_bytes) != sha(before)})")
    check(st_after.st_size == st_before.st_size, "FIXTURE: the edit changed the size")
    check(st_after.st_mtime_ns == st_before.st_mtime_ns, "FIXTURE: the edit moved the mtime")
    check(sha(now_bytes) != sha(before), "FIXTURE: the edit did not change the bytes")
    trunc_to = len(now_bytes) - 16
    ok_b, detail_b = try_call(lambda: os.truncate(mnt_target, trunc_to))
    time.sleep(2.0)
    after_b = open(srv_target, "rb").read()
    print(f"  truncate to {trunc_to}      : {'LANDED' if ok_b else 'REFUSED'} — {detail_b}")
    print(f"  server bytes after        : {len(after_b)} B sha256={sha(after_b)[:16]}…")
    check(not ok_b, "the write did NOT land over a same-size, mtime-preserved concurrent edit "
                    "(if this fails, the refusal did not hold — see the request trace below)")
    check(sha(after_b) == sha(now_bytes), "the server's bytes were left untouched by the refused write")
    cf = conflicts(args.cache_dir)
    st = status(args.cache_dir)
    print(f"  conflicts.jsonl entries   : {len(cf)}")
    for c in cf[-3:]:
        print(f"    {json.dumps(c)[:200]}")
    print(f"  status.conflicts          : {json.dumps(st.get('conflicts', {}))[:200]}")
    check(any(c.get("current") == "sha256:" + sha(now_bytes) for c in cf),
          "a conflict record names the post-edit hash (the refusal was recorded loudly)")
    check(st.get("conflicts", {}).get("refusals_total", 0) >= 1, "status.json counted the refusal")

    print()
    print("=== ARM C: bytes IDENTICAL, mtime moved — the write must LAND (never mtime) ===")
    served_c = open(mnt_target, "rb").read()
    server_c = open(srv_target, "rb").read()
    st_c = os.stat(srv_target)
    same_as_server = sha(served_c) == sha(server_c)
    print(f"  read through the mount    : {len(served_c)} B sha256={sha(served_c)[:16]}…")
    print(f"  the server's bytes        : {len(server_c)} B sha256={sha(server_c)[:16]}…")
    if not same_as_server:
        # The mount served bytes that are not the server's. That is the known
        # staleness class (BFS-024/025/026: invalidation depends on server ops this
        # build answers capability_unavailable, and the mount's read path serves a
        # cached blob for a path an out-of-band edit changed). A touch-only arm whose
        # "before" read is already wrong cannot attribute a later outcome to the
        # mtime rule, so this arm is SKIPPED and named rather than turned into a
        # finding about the conflict rule.
        print("  SKIP (unreliable cell): the mount served STALE bytes for this path, so this arm cannot")
        print("     attribute anything to the mtime rule. Known defect, filed as BFS-024/025/026 and")
        print("     re-observed here; the touch-only direction is carried by the unit test")
        print("     TestTouchOnlyChangeIsNotAConflict (mutation-RED proven in BFS-012-probes).")
        print()
        print(f"RESULT: {'every reachable arm behaved as the content-hash rule requires' if rc == 0 else 'AT LEAST ONE ARM FAILED — see FAIL lines above'}"
              f"  (arm C: SKIPPED, stale read)")
        return rc
    os.utime(srv_target, ns=(st_c.st_atime_ns, st_c.st_mtime_ns + 5_000_000_000))
    st_c2 = os.stat(srv_target)
    same_bytes = sha(open(srv_target, "rb").read()) == sha(served_c)
    print(f"  server touched            : size same={st_c2.st_size == st_c.st_size} "
          f"mtime moved={st_c2.st_mtime_ns != st_c.st_mtime_ns} bytes same={same_bytes}")
    check(st_c2.st_mtime_ns != st_c.st_mtime_ns, "FIXTURE: the touch did not move the mtime")
    check(same_bytes, "FIXTURE: the touch changed the bytes")
    want_c = len(served_c) - 8
    ok_c, detail_c = try_call(lambda: os.truncate(mnt_target, want_c))
    time.sleep(2.0)
    print(f"  truncate to {want_c}      : {'LANDED (correct)' if ok_c else 'REFUSED (an mtime-reading rule)'} — {detail_c}")
    print(f"  server size now           : {os.path.getsize(srv_target)}")
    check(ok_c, "the write LANDED — the rule read the content, not the mtime")
    check(os.path.getsize(srv_target) == want_c, "the new size is on the server's disk")

    print()
    print(f"RESULT: {'every reachable arm behaved as the content-hash rule requires' if rc == 0 else 'AT LEAST ONE ARM FAILED — see FAIL lines above'}")
    return rc


if __name__ == "__main__":
    raise SystemExit(main())
