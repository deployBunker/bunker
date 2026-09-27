#!/usr/bin/env python3
"""bypass_check.py — the "there is nothing left to evict => BYPASS, not fail" arm.

The client's Insert() has three ways out when a new entry does not fit: it can
evict (the normal path), it can refuse the ENTRY as oversize, or — when the cache
is full of bytes it may not reclaim — it must BYPASS and still serve the read.
BFS-009 measured `bypass=0 oversize=0` in every arm it ran, because a serial
reader against a big tree never pins anything, so eviction always succeeds and
neither bypass class is ever reached AT THE MOUNT LEVEL.

This script drives both classes through a real FUSE mount and checks the property
the owner's constraint actually needs — local storage must not grow with the tree:

  arm PIN  (--hold N) — hold N read handles open (each pins its blob, and a pinned
            blob is never an eviction candidate), then read the remaining files.
            The cache is full of unpinned-but-unevictable data: `bypass_events`
            must move, the bypassed bytes must NOT be on disk, and every read must
            still return the right bytes.
  arm OVER (--hold 0) — --cache-max-entry-bytes below the file size: no bulk read
            may be cached at all, and every read must still return the right bytes.

Every file is read through the mount EXACTLY ONCE: the digest it is compared
against is computed from the SERVER's copy ($TREE), never from a second mount read
(a second read would double the insert attempts and make the counters misleading).

usage (normally run BY mount-arm.sh, which sets $MNT/$CDIR/$TREE):
  bypass_check.py --hold N [--dir src/bulk] [--settle 3]
Prints one PASS/FAIL line per assertion and exits non-zero if any fails.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import time


def blob_path(cache_dir: str, hexdigest: str) -> str:
    return os.path.join(cache_dir, "blobs", hexdigest)


def status(cache_dir: str, tries: int = 30) -> dict:
    for _ in range(tries):
        try:
            with open(os.path.join(cache_dir, "status.json")) as fh:
                return json.load(fh)
        except (OSError, json.JSONDecodeError):
            time.sleep(0.2)
    raise SystemExit("could not read status.json")


def sha256_path(p: str) -> str:
    h = hashlib.sha256()
    with open(p, "rb") as fh:
        while True:
            b = fh.read(1 << 20)
            if not b:
                break
            h.update(b)
    return h.hexdigest()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mount", default=os.environ.get("MNT", ""), help="default: $MNT (set by mount-arm.sh)")
    ap.add_argument("--cache-dir", default=os.environ.get("CDIR", ""), help="default: $CDIR (set by mount-arm.sh)")
    ap.add_argument("--tree", default=os.environ.get("TREE", ""), help="the server's own copy, for the wanted digest")
    ap.add_argument("--dir", default="src/bulk", help="directory IN the mount to read")
    ap.add_argument("--hold", type=int, default=0, help="how many handles to keep open (pins)")
    ap.add_argument("--settle", type=float, default=3.0, help="seconds to hold the pins, so the sampler sees them")
    args = ap.parse_args()
    if not args.mount or not args.cache_dir or not args.tree:
        raise SystemExit("--mount/--cache-dir/--tree are required (or run me under mount-arm.sh, which sets them)")

    blob_dir = os.path.join(args.cache_dir, "blobs")
    target_dir = os.path.join(args.mount, args.dir)
    src_dir = os.path.join(args.tree, args.dir)
    names = sorted(os.listdir(target_dir))
    if not names:
        raise SystemExit(f"no files under {target_dir}")
    print(f"files under {args.dir}: {len(names)}   hold={args.hold}")

    before = status(args.cache_dir)["cache"]
    print(f"status before: entries={before['entries']} blobs={before['blobs']} used={before['used_bytes']} "
          f"evictions={before['evictions_total']} bypass={before['bypass_events']} "
          f"oversize={before['oversize_bypasses']} pinned={before['pinned_blobs']}")

    held: list[tuple[str, object]] = []
    uncached: list[str] = []
    read_bad: list[str] = []
    read_ok = 0

    def read_once(name: str, keep_open: bool) -> None:
        """One mount read: digest the bytes the MOUNT returned against the server's."""
        nonlocal read_ok
        want = sha256_path(os.path.join(src_dir, name))
        mnt_path = os.path.join(target_dir, name)
        if keep_open:
            fh = open(mnt_path, "rb")  # noqa: SIM115 — held open on purpose: the pin IS the fixture
            data = fh.read()
            held.append((name, fh))
        else:
            with open(mnt_path, "rb") as fh:
                data = fh.read()
        got = hashlib.sha256(data).hexdigest()
        if got == want:
            read_ok += 1
        else:
            read_bad.append(name)
        if got == want and not os.path.exists(blob_path(args.cache_dir, got)):
            uncached.append(name)
        return

    for name in names[: args.hold]:
        read_once(name, keep_open=True)
        print(f"  held open : {name} (sha256 {sha256_path(os.path.join(src_dir, name))[:12]}…)")
    for name in names[args.hold :]:
        read_once(name, keep_open=False)

    print(f"  reads through the mount: {len(names)} (one per file)  ok={read_ok}  mismatched={read_bad}")

    during = None
    if held:
        # Hold the pins across more than one status-document cadence (1 s), so
        # `pinned_blobs > 0` is OBSERVED and not merely inferred from the code.
        time.sleep(args.settle)
        during = status(args.cache_dir)["cache"]
        print(f"status while pinned: entries={during['entries']} blobs={during['blobs']} used={during['used_bytes']} "
              f"bypass={during['bypass_events']} oversize={during['oversize_bypasses']} "
              f"pinned={during['pinned_blobs']} evictions={during['evictions_total']}")
    for _name, fh in held:
        fh.close()
    time.sleep(1.5)

    after = status(args.cache_dir)["cache"]
    print(f"status after : entries={after['entries']} blobs={after['blobs']} used={after['used_bytes']} "
          f"evictions={after['evictions_total']} bypass={after['bypass_events']} "
          f"oversize={after['oversize_bypasses']} pinned={after['pinned_blobs']}")

    on_disk = sorted(os.listdir(blob_dir)) if os.path.isdir(blob_dir) else []
    on_disk_bytes = sum(os.path.getsize(os.path.join(blob_dir, n)) for n in on_disk)
    print(f"blob files on disk : {len(on_disk)} ({on_disk_bytes} B)   bound={after['max_bytes']} used={after['used_bytes']}")

    rc = 0

    def check(cond: bool, msg: str) -> None:
        nonlocal rc
        print(("  PASS  " if cond else "  FAIL  ") + msg)
        if not cond:
            rc = 1

    # 1. a bypass is never a failure: every read returned the right bytes.
    check(not read_bad, f"every read returned the right bytes ({read_ok}/{len(names)} matched; mismatched={read_bad})")
    # 2. nothing refused a cache entry is on disk.
    expected_uncached = len(names) - args.hold
    check(len(uncached) == expected_uncached,
          f"{len(uncached)}/{expected_uncached} reads that could not be cached left NO blob on disk "
          f"(uncached={uncached[:6]}{'…' if len(uncached) > 6 else ''})")
    # 3. the bound held, by both instruments.
    check(after["used_bytes"] <= after["max_bytes"], f"used_bytes {after['used_bytes']} <= max_bytes {after['max_bytes']}")
    check(on_disk_bytes <= after["max_bytes"], f"real blob bytes {on_disk_bytes} <= max_bytes {after['max_bytes']}")
    # 4. the arm's own mechanism was reached, and was reported.
    if args.hold:
        check(after["bypass_events"] >= 1,
              f"bypass_events moved ({after['bypass_events']}) — a full cache of pinned blobs bypassed instead of failing")
        check(during is not None and during["pinned_blobs"] >= 1,
              f"pins were real and visible while held (during={during['pinned_blobs'] if during else 'n/a'}, "
              f"after={after['pinned_blobs']})")
        check(after["entries"] == args.hold and len(on_disk) == args.hold,
              f"no entry was stored for a bypassed read (entries={after['entries']}, blobs on disk={len(on_disk)}, "
              f"held={args.hold})")
    else:
        check(after["entries"] == 0 and after["blobs"] == 0 and len(on_disk) == 0,
              f"nothing over the entry cap was cached (entries={after['entries']} blobs={after['blobs']} files={len(on_disk)})")
        # MEASURED, NOT ASSERTED — a defect this arm found. A probe that turned red
        # the day someone fixed it would be a probe nobody could keep. The cache's
        # own Insert() counts this case (internal/fsclient/cache.go:312
        # OversizeBypasses, covered by TestCacheOversizeEntryIsNeverCached) but the
        # mount's read path never calls Insert for an entry over the cap
        # (internal/fsmount/fs_linux.go:1053 pre-filters), so the counter that exists
        # to make "the bound is respected by not caching anything" VISIBLE stays 0
        # while every large read is refused caching. See BFS-012 report §F-B.
        print(f"  MEASURED (finding F-B): oversize_bypasses={after['oversize_bypasses']} while "
              f"{len(uncached)} reads exceeded --cache-max-entry-bytes and were not cached — the mount "
              f"pre-filters at internal/fsmount/fs_linux.go:1053, so the counter can never move")
    return rc


if __name__ == "__main__":
    raise SystemExit(main())
