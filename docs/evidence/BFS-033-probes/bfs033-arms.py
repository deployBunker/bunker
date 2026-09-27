#!/usr/bin/env python3
"""bfs033-arms.py — BFS-033: the conflict refusal must HOLD.

The defect (BFS-012, re-measured here): one `truncate(2)` syscall through the
mount produces a PUT that is REFUSED with 412 and RECORDED in conflicts.jsonl,
immediately followed by a PUT 204 THAT LANDS — and the caller sees success. The
refusal is written down but not enforced.

The interleaving that produces it (so the arm is not a guess):
  * a read through the mount fixes the client's base for the path;
  * an out-of-band edit changes the bytes while PRESERVING size and mtime, so the
    mount's poll has nothing to invalidate (mtime/ctime-based) and the conflict
    survives to the write;
  * `os.truncate()` then arrives with a stale base.

Modes
  cell        -- the interleaving cell. --expect red (the defect: a 412 AND a
                 landing 204 AND an unchanged caller verdict) or --expect green
                 (the refusal holds: the target is UNCHANGED and the caller
                 receives the failure).
  retry       -- the retry path: refuse, then RE-READ through the mount, then
                 retry. The retry must LAND, because ESTALE's contract is
                 "re-read and retry". This is the cell that proves the enforcement
                 is not a dead end.
  twowriters  -- two writers, one conflicting: a refused writer's bytes never
                 reach the target, a second writer cannot publish behind the
                 standing refusal, an innocent writer on ANOTHER path still lands
                 (the hold is per path, not a global stall), and the refused
                 writer recovers by re-reading.
  cost        -- the successful write path, timed and counted (COST_FILES files,
                 each created then resized): the enforcement must cost the writes
                 that never touch it.

Every arm asserts its own fixture before asserting anything about the write, and
the write attribution is read from the request log the counting proxy wrote
($OUT/requests.jsonl) rather than inferred from the caller's exit code.
"""
from __future__ import annotations

import argparse
import errno as errno_mod
import hashlib
import json
import os
import sys
import threading
import time

TARGET = "src/target.txt"
OTHER = "src/other.txt"
BASELINE = b"BFS-033-TARGET-CONTENT-0123456789-abcdefghijklmnopqrstuvwxyz-ABCD."[:64]


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
    out: list[dict] = []
    try:
        with open(os.path.join(cache_dir, "conflicts.jsonl")) as fh:
            for line in fh:
                if line.strip():
                    out.append(json.loads(line))
    except FileNotFoundError:
        pass
    return out


def requests(run_dir: str) -> list[dict]:
    out: list[dict] = []
    try:
        with open(os.path.join(run_dir, "requests.jsonl")) as fh:
            for line in fh:
                line = line.strip()
                if line:
                    try:
                        out.append(json.loads(line))
                    except json.JSONDecodeError:
                        pass
    except FileNotFoundError:
        pass
    return out


def puts_on(reqs: list[dict], path: str) -> list[dict]:
    return [r for r in reqs if r.get("method") == "PUT" and r.get("path", "").endswith(path)]


def try_truncate(path: str, size: int) -> tuple[bool, str, int | None]:
    try:
        os.truncate(path, size)
        return True, "returned cleanly", None
    except OSError as e:
        name = errno_mod.errorcode.get(e.errno or 0, "")
        return False, f"OSError: {e} (errno={e.errno} {name})", e.errno


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", choices=["cell", "retry", "twowriters", "cost"], required=True)
    ap.add_argument("--expect", choices=["red", "green"], default="green")
    ap.add_argument("--mount", default=os.environ.get("MNT", ""))
    ap.add_argument("--tree", default=os.environ.get("TREE", ""))
    ap.add_argument("--cache-dir", default=os.environ.get("CDIR", ""))
    ap.add_argument("--run-dir", default=os.environ.get("OUT", ""))
    args = ap.parse_args()
    if not args.mount or not args.tree or not args.cache_dir:
        raise SystemExit("--mount/--tree/--cache-dir required (or run under mount-arm.sh)")

    rc = 0

    def check(cond: bool, msg: str) -> None:
        nonlocal rc
        print(("  PASS  " if cond else "  FAIL  ") + msg)
        if not cond:
            rc = 1

    mnt_target = os.path.join(args.mount, TARGET)
    srv_target = os.path.join(args.tree, TARGET)
    mnt_other = os.path.join(args.mount, OTHER)
    srv_other = os.path.join(args.tree, OTHER)

    def seed(path: str, body: bytes) -> None:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as fh:
            fh.write(body)

    def reqs() -> list[dict]:
        return requests(args.run_dir) if args.run_dir else []

    # ------------------------------------------------------------------ cost
    if args.mode == "cost":
        # The SUCCESSFUL write path, timed and verified, in two arms:
        #   A. RESIZE — N files seeded on the server, each resized through the mount.
        #      This is the modified-file publish path, the one the enforcement sits
        #      in front of.
        #   B. CREATE — N files created through the mount (a new path, so no
        #      refusal can stand on it) and each waited for on the server side.
        # The enforcement must cost the writes that never touch it: one map lookup
        # on a path with no refusal standing.
        n = int(os.environ.get("COST_FILES", "200"))
        os.makedirs(os.path.join(args.tree, "cost"), exist_ok=True)
        os.makedirs(os.path.join(args.mount, "cost"), exist_ok=True)
        body = f"file 0000 ".encode() + b"x" * 40
        want = len(body) - 10
        for i in range(n):
            with open(os.path.join(args.tree, "cost", f"r{i:04d}.txt"), "wb") as fh:
                fh.write(body)

        t_resize = 0.0
        for i in range(n):
            p = os.path.join(args.mount, "cost", f"r{i:04d}.txt")
            s = time.monotonic()
            os.truncate(p, want)
            t_resize += time.monotonic() - s
        t_create = 0.0
        lag: list[float] = []
        for i in range(n):
            p = os.path.join(args.mount, "cost", f"c{i:04d}.txt")
            s = time.monotonic()
            with open(p, "wb") as fh:
                fh.write(body)
            t_create += time.monotonic() - s
            srv = os.path.join(args.tree, "cost", f"c{i:04d}.txt")
            s = time.monotonic()
            while time.monotonic() - s < 3.0:
                if os.path.exists(srv) and os.path.getsize(srv) == len(body):
                    break
                time.sleep(0.005)
            lag.append(time.monotonic() - s)
        ok_resize = sum(
            1 for i in range(n) if os.path.getsize(os.path.join(args.tree, "cost", f"r{i:04d}.txt")) == want
        )
        ok_create = sum(
            1 for i in range(n)
            if os.path.getsize(os.path.join(args.tree, "cost", f"c{i:04d}.txt")) == len(body)
        )
        print(f"=== cost arm: {n} files per arm ({2 * n} publications) ===")
        print(f"  RESIZE (modified path)    : {t_resize * 1000:.0f} ms total, {t_resize / n * 1000:.2f} ms/op,"
              f" {ok_resize}/{n} landed")
        print(f"  CREATE (new path)         : {t_create * 1000:.0f} ms total, {t_create / n * 1000:.2f} ms/op,"
              f" {ok_create}/{n} landed")
        print(f"  create publish lag        : max {max(lag) * 1000:.1f} ms, median"
              f" {sorted(lag)[len(lag) // 2] * 1000:.1f} ms (time until the server sees the new file)")
        st = status(args.cache_dir)
        print(f"  refusal_holds             : {json.dumps(st.get('refusal_holds', {}))}")
        print(f"  conflicts                 : {json.dumps(st.get('conflicts', {}))[:120]}")
        check(ok_resize == n, "every resize landed (the successful write path is intact)")
        check(ok_create == n, "every create landed")
        check(st.get("refusal_holds", {}).get("held_total", 0) == 0,
              "the gate held NOTHING on the success path")
        check(st.get("refusal_holds", {}).get("outstanding", 0) == 0,
              "the gate stands on no path after a clean run")
        print()
        print(f"RESULT: {n} resizes at {t_resize / n * 1000:.2f} ms/op and {n} creates at"
              f" {t_create / n * 1000:.2f} ms/op, all landed")
        return rc

    # ------------------------------------------------------------ twowriters
    if args.mode == "twowriters":
        seed(srv_target, BASELINE)
        seed(srv_other, BASELINE)
        # Both writers read the path through the mount: that read is each writer's
        # base. Then an out-of-band edit makes BOTH bases stale while leaving the
        # size and the mtime exactly as the writers saw them.
        r1 = open(mnt_target, "rb").read()
        r2 = open(mnt_target, "rb").read()
        print("=== two writers, one conflicting, on " + TARGET + " ===")
        check(sha(r1) == sha(BASELINE) and sha(r2) == sha(BASELINE),
              "FIXTURE: both writers read the baseline (each read is that writer's base)")
        st_before = os.stat(srv_target)
        edited = bytes((b ^ 0x20) if 65 <= b <= 90 else b for b in BASELINE)
        with open(srv_target, "wb") as fh:
            fh.write(edited)
        os.utime(srv_target, ns=(st_before.st_atime_ns, st_before.st_mtime_ns))
        print(f"  concurrent edit           : {len(edited)} B sha256={sha(edited)[:16]}… (same size, mtime restored)")
        check(sha(open(srv_target, "rb").read()) == sha(edited), "FIXTURE: the concurrent edit landed on the server")

        n1, n2, n3 = len(edited) - 8, len(edited) - 16, len(BASELINE) - 12
        trace_start = len(reqs())

        # Writer A: the conflicting one. Its base is stale, so the server refuses.
        a_landed, a_detail, a_err = try_truncate(mnt_target, n1)
        print(f"  writer A (truncate to {n1}) : {'LANDED' if a_landed else 'REFUSED'} — {a_detail}")
        # Writer B: a second writer with the SAME stale base. It must not be able to
        # publish behind A's standing refusal.
        b_landed, b_detail, b_err = try_truncate(mnt_target, n2)
        print(f"  writer B (truncate to {n2}) : {'LANDED' if b_landed else 'REFUSED'} — {b_detail}")
        # Writer C: an innocent writer on ANOTHER path, while the refusal stands.
        c_landed, c_detail, c_err = try_truncate(mnt_other, n3)
        print(f"  writer C on {OTHER} (to {n3}): {'LANDED' if c_landed else 'REFUSED'} — {c_detail}")
        time.sleep(1.5)
        after = open(srv_target, "rb").read()
        after_other = open(srv_other, "rb").read()
        print(f"  the target's bytes after  : {len(after)} B sha256={sha(after)[:16]}…")

        check(not a_landed and a_err == 116, "the conflicting writer was REFUSED with ESTALE")
        check(not b_landed and b_err == 116,
              "a second writer could NOT publish behind the standing refusal (it was told ESTALE)")
        check(c_landed, "an innocent writer on ANOTHER path still landed (the hold is per path, not a global stall)")
        check(after_other == BASELINE[:n3], "the innocent writer's bytes landed on its own path")
        check(after == edited, "the conflicting writers' bytes never reached the target")

        st = status(args.cache_dir)
        my_puts = puts_on(reqs()[trace_start:], TARGET)
        refused_puts = [r for r in my_puts if r.get("status") == 412]
        accepted_puts = [r for r in my_puts if isinstance(r.get("status"), int) and 200 <= r["status"] < 300]
        sizes_accepted = {r.get("req_bytes") for r in accepted_puts}
        print(f"  PUTs on {TARGET}: {len(my_puts)} total, {len(refused_puts)} x 412, {len(accepted_puts)} x 2xx")
        print(f"  refusal_holds             : {json.dumps(st.get('refusal_holds', {}))}")
        check(len(accepted_puts) == 0, "NO write landed on the target while a refusal stood")
        check(len(refused_puts) >= 1, "the conflicting write was refused (412 recorded)")
        check(n1 not in sizes_accepted and n2 not in sizes_accepted,
              "neither conflicting writer's bytes ever returned success")
        check(st.get("refusal_holds", {}).get("held_total", 0) >= 1,
              "the standing refusal is REPORTED as having held a write (refusal_holds.held_total)")
        check(len(conflicts(args.cache_dir)) == 1,
              f"one refusal recorded for the one refused write (got {len(conflicts(args.cache_dir))})")

        # The refused writer's recovery: re-read, then retry.
        open(mnt_target, "rb").read()
        b2_landed, b2_detail, b2_err = try_truncate(mnt_target, n2)
        print(f"  writer B retry (after re-read): {'LANDED' if b2_landed else 'REFUSED'} — {b2_detail}")
        time.sleep(1.5)
        after_retry = open(srv_target, "rb").read()
        check(b2_landed, "the refused writer recovered by re-reading and retrying (ESTALE's contract)")
        check(after_retry == edited[:n2], "the retry published the SERVER's current content truncated")
        print()
        print("RESULT: the refused writers never landed, the refusal held a second writer, and the "
              "innocent writer was unaffected" if rc == 0 else "RESULT: the interleaving was not contained")
        return rc

    # -------------------------------------------------------- cell / retry
    seed(srv_target, BASELINE)
    baseline = open(srv_target, "rb").read()
    st_before = os.stat(srv_target)
    trunc_to = len(baseline) - 16

    print(f"=== ARM {args.mode} (expect={args.expect}) on {TARGET} ===")
    print(f"  baseline                  : {len(baseline)} B sha256={sha(baseline)[:16]}…")

    # The read through the mount: THIS read fixes the client's base.
    served = open(mnt_target, "rb").read()
    print(f"  read through the mount    : {len(served)} B sha256={sha(served)[:16]}…  (this read fixes the base)")
    check(sha(served) == sha(baseline), "FIXTURE: the mount served the server's bytes")

    # The out-of-band edit: same size, same mtime, different bytes.
    edited = bytes((b ^ 0x20) if 65 <= b <= 90 else b for b in baseline)
    with open(srv_target, "wb") as fh:
        fh.write(edited)
    os.utime(srv_target, ns=(st_before.st_atime_ns, st_before.st_mtime_ns))
    st_after = os.stat(srv_target)
    now_bytes = open(srv_target, "rb").read()
    print(f"  concurrent edit (out-of-band): {len(now_bytes)} B sha256={sha(now_bytes)[:16]}…  "
          f"(size same={st_after.st_size == st_before.st_size}, "
          f"mtime same={st_after.st_mtime_ns == st_before.st_mtime_ns}, "
          f"bytes differ={sha(now_bytes) != sha(baseline)})")
    check(st_after.st_size == st_before.st_size, "FIXTURE: the concurrent edit kept the size")
    check(st_after.st_mtime_ns == st_before.st_mtime_ns, "FIXTURE: the concurrent edit restored the mtime")
    check(sha(now_bytes) != sha(baseline), "FIXTURE: the concurrent edit changed the bytes")

    # The write whose base is now stale.
    landed, detail, err = try_truncate(mnt_target, trunc_to)
    print(f"  truncate to {trunc_to}          : {'LANDED' if landed else 'REFUSED'} — {detail}")
    time.sleep(1.5)
    after = open(srv_target, "rb").read()
    print(f"  the target's bytes after   : {len(after)} B sha256={sha(after)[:16]}…")
    unchanged = sha(after) == sha(now_bytes)
    print(f"  target unchanged           : {unchanged}")

    cf = conflicts(args.cache_dir)
    st = status(args.cache_dir)
    print(f"  conflicts.jsonl entries    : {len(cf)}")
    for c in cf[-3:]:
        print(f"    {json.dumps(c)[:180]}")
    print(f"  status.conflicts           : {json.dumps(st.get('conflicts', {}))[:180]}")
    print(f"  status.refusal_holds       : {json.dumps(st.get('refusal_holds', {}))[:180]}")

    my_puts = puts_on(reqs(), TARGET)
    print(f"  PUTs on {TARGET} (proxy trace): {len(my_puts)}")
    for r in my_puts:
        print(f"    #{r.get('n')} status={r.get('status')} req_bytes={r.get('req_bytes')} "
              f"if_match={r.get('if_match')}")
    refused = [r for r in my_puts if r.get("status") == 412]
    accepted = [r for r in my_puts if isinstance(r.get("status"), int) and 200 <= r["status"] < 300]
    print(f"  PUT 412 count={len(refused)}  PUT 2xx count={len(accepted)}")

    print()
    if args.mode == "cell" and args.expect == "red":
        # THE DEFECT, asserted rather than narrated.
        check(landed, "DEFECT: the caller saw SUCCESS for the very write that was refused")
        check(len(refused) >= 1, "DEFECT: a refusal was recorded for the write (412)")
        check(len(accepted) >= 1, "DEFECT: a later PUT LANDED on the same path (2xx after the 412)")
        check(not unchanged, "DEFECT: the refused write's bytes are the target's new content")
        check(len(cf) >= 1, "DEFECT: conflicts.jsonl carries an entry the landing contradicts")
        print()
        print("RESULT: the refusal DID NOT HOLD — a 412 was recorded and a 204 landed after it, "
              "and the caller observed success" if rc == 0 else "RESULT: the expected defect was not reproduced")

    elif args.mode == "cell" and args.expect == "green":
        check(not landed, "the caller received the FAILURE (the refused write is not reported as success)")
        check(err == 116, f"the failure is ESTALE (116) — re-read and retry is the recovery (got {err})")
        check(unchanged, "the target is UNCHANGED by the refused write")
        check(len(refused) == 1, f"exactly one PUT reached the server and it was refused (got {len(refused)})")
        check(len(accepted) == 0, f"NO write landed on the path after the refusal (got {len(accepted)} PUT 2xx)")
        check(len(cf) >= 1, "the refusal is still recorded loudly for `bunker fs conflicts`")
        check(all(c.get("current") == "sha256:" + sha(now_bytes) for c in cf),
              "every recorded refusal names the concurrent edit's hash (none is contradicted)")
        check(st.get("refusal_holds", {}).get("held_total", 0) >= 1,
              "the held refusal is REPORTED (refusal_holds.held_total)")
        print()
        print("RESULT: the refusal HELD — one refused PUT, no landing write, the target untouched "
              "and the caller told ESTALE" if rc == 0 else "RESULT: the enforcement did not hold")

    elif args.mode == "retry" and args.expect == "green":
        check(not landed, "the first attempt was REFUSED (ESTALE)")
        check(unchanged, "the target is UNCHANGED by the refused attempt")
        # The recovery ESTALE names: re-read the path, then retry.
        re_served = open(mnt_target, "rb").read()
        print(f"  re-read through the mount : {len(re_served)} B sha256={sha(re_served)[:16]}…")
        # WHICH bytes the re-read serves is NOT this row's to assert. In this
        # interleaving (same size, mtime restored) the mount's invalidation has
        # nothing to see, so the re-read can be answered from the cache entry the
        # caller's first read made — the stale-serve class filed as BFS-024/026 and
        # QA-BUNKER-36, which BFS-033 does not touch. It is reported, not hidden: the
        # enforcement this row adds is that the refused write cannot land without an
        # intervening caller read, not that the client's cache is truthful.
        if sha(re_served) == sha(now_bytes):
            print("  re-read freshness          : FRESH — the serve is the concurrent edit's bytes")
        else:
            print("  re-read freshness          : STALE (cache) — named residual: BFS-024/026,"
                  " QA-BUNKER-36. The caller re-read; the client served the blob it still held.")
        landed2, detail2, err2 = try_truncate(mnt_target, trunc_to)
        print(f"  retry truncate to {trunc_to}    : {'LANDED' if landed2 else 'REFUSED'} — {detail2}")
        time.sleep(1.5)
        after2 = open(srv_target, "rb").read()
        print(f"  the target's bytes after   : {len(after2)} B sha256={sha(after2)[:16]}…")
        check(landed2, "the RETRY LANDED: ESTALE's contract (re-read, then retry) still works")
        check(after2 == now_bytes[:trunc_to], "the retry published the SERVER's current content truncated")
        my_puts = puts_on(reqs(), TARGET)
        refused = [r for r in my_puts if r.get("status") == 412]
        accepted = [r for r in my_puts if isinstance(r.get("status"), int) and 200 <= r["status"] < 300]
        print(f"  PUTs on {TARGET}: {len(my_puts)} total, {len(refused)} x 412, {len(accepted)} x 2xx")
        check(len(refused) == 1 and len(accepted) == 1,
              "one refusal then one sanctioned landing — no second unsanctioned write")
        print()
        print("RESULT: the enforcement is not a dead end — the refused writer recovered by re-reading"
              if rc == 0 else "RESULT: the retry path did not behave")
    else:
        raise SystemExit(f"unknown mode/expect pair: {args.mode}/{args.expect}")

    return rc


if __name__ == "__main__":
    sys.exit(main())
