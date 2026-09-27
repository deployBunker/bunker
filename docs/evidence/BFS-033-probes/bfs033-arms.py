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
  cell   -- the interleaving cell. --expect red (the defect: a 412 AND a landing
            204 AND an unchanged caller verdict) or --expect green (the refusal
            holds: the target is UNCHANGED and the caller receives the failure).
  retry  -- the retry path: refuse, then RE-READ through the mount, then retry.
            The retry must LAND, because ESTALE's contract is "re-read and retry".
            This is the cell that proves the enforcement is not a dead end.

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
    ap.add_argument("--mode", choices=["cell", "retry"], required=True)
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

    rel = "src/target.txt"
    mnt_target = os.path.join(args.mount, rel)
    srv_target = os.path.join(args.tree, rel)

    # --- fixture: one file of known content, created on the SERVER side ---------
    os.makedirs(os.path.dirname(srv_target), exist_ok=True)
    body = bytes(("BFS-033-TARGET-CONTENT-0123456789-abcdefghijklmnopqrstuvwxyz-ABCD.").encode())
    body = body[:64]
    with open(srv_target, "wb") as fh:
        fh.write(body)
    baseline = open(srv_target, "rb").read()
    st_before = os.stat(srv_target)
    trunc_to = len(baseline) - 16  # 48

    print(f"=== ARM {args.mode} (expect={args.expect}) on {rel} ===")
    print(f"  baseline                  : {len(baseline)} B sha256={sha(baseline)[:16]}…")

    # --- the read through the mount: THIS read fixes the client's base ---------
    served = open(mnt_target, "rb").read()
    print(f"  read through the mount    : {len(served)} B sha256={sha(served)[:16]}…  (this read fixes the base)")
    check(sha(served) == sha(baseline), "FIXTURE: the mount served the server's bytes")

    # --- the out-of-band edit: same size, same mtime, different bytes ----------
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

    # --- the write whose base is now stale -------------------------------------
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

    reqs = requests(args.run_dir) if args.run_dir else []
    my_puts = puts_on(reqs, rel)
    print(f"  PUTs on {rel} (proxy trace): {len(my_puts)}")
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
        print()
        print("RESULT: the refusal HELD — one refused PUT, no landing write, the target untouched "
              "and the caller told ESTALE" if rc == 0 else "RESULT: the enforcement did not hold")

    elif args.mode == "retry" and args.expect == "green":
        check(not landed, "the first attempt was REFUSED (ESTALE)")
        check(unchanged, "the target is UNCHANGED by the refused attempt")
        # The recovery ESTALE names: re-read the path, then retry.
        re_served = open(mnt_target, "rb").read()
        print(f"  re-read through the mount : {len(re_served)} B sha256={sha(re_served)[:16]}…")
        check(sha(re_served) == sha(now_bytes),
              "the re-read serves the concurrent edit's bytes (the base is now truth)")
        landed2, detail2, err2 = try_truncate(mnt_target, trunc_to)
        print(f"  retry truncate to {trunc_to}    : {'LANDED' if landed2 else 'REFUSED'} — {detail2}")
        time.sleep(1.5)
        after2 = open(srv_target, "rb").read()
        print(f"  the target's bytes after   : {len(after2)} B sha256={sha(after2)[:16]}…")
        check(landed2, "the RETRY LANDED: ESTALE's contract (re-read, then retry) still works")
        check(after2 == now_bytes[:trunc_to], "the retry published the freshly-read content truncated")
        reqs = requests(args.run_dir) if args.run_dir else []
        my_puts = puts_on(reqs, rel)
        refused = [r for r in my_puts if r.get("status") == 412]
        accepted = [r for r in my_puts if isinstance(r.get("status"), int) and 200 <= r["status"] < 300]
        print(f"  PUTs on {rel}: {len(my_puts)} total, {len(refused)} x 412, {len(accepted)} x 2xx")
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
