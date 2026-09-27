#!/usr/bin/env python3
"""small_bound_edge.py — what the bound does and does NOT cover on disk (BFS-012).

The row states the property as "the CACHE DIRECTORY's real on-disk size (measure it
yourself) never exceeds the bound". When this probe was written that was false: the
directory held four things and only two of them were counted:

    blobs/          counted   (cache.blobs_bytes)
    index.json      counted   (cache.index_bytes)
    status.json     NOT counted   — the mount's own document, rewritten on a 1 s cadence
    conflicts.jsonl NOT counted   — the refusal LOG, appended once per refused write

`max_bytes` bounded `used_bytes = blobs + index`, so the directory-level claim had a
floor of about `status.json` and grew with the number of REFUSALS, not with the tree.
At a 1 KiB bound that unaccounted floor was larger than the bound itself: measured at
30,689 B = 29.97x the bound (BFS-012 report §F-C), and 7,965 B = 7.78x when this
reproducer was re-run on the tree BFS-031 was written against.

BFS-031 moved the two uncounted files OUT of the directory the bound names (they now
live in the mount directory, beside `cache/`, with their own enforced bound), so this
probe now ASSERTS the row's property instead of merely reporting it:

  1. `du -sb` of the CACHE DIRECTORY <= max_bytes, in every phase;
  2. the reported `cache.dir_bytes` agrees with that `du` within a stated tolerance;
  3. the state the cache bound does NOT name (status.json, the refusal log) is inside
     ITS own bound, reported as `state.*` in the same document.

It still runs the same three phases and the same ground-truth check as the RED run:
  1. baseline (nothing read yet),
  2. after reading every file in the tree through the mount,
  3. after N refused writes (each one appends a line to conflicts.jsonl),
reporting `du -sb` of the directory, the client's `used_bytes`/`max_bytes`, and the
ratio — the numbers the row quoted.
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import time


def du(path: str) -> int:
    try:
        out = subprocess.run(["du", "-sb", path], capture_output=True, text=True, timeout=30).stdout
        return int(out.split()[0])
    except Exception:
        return -1


def status_full(mount_dir: str, cache_dir: str, tries: int = 30) -> dict:
    """The status document, from wherever the mount keeps it.

    BFS-031: the document lives in the MOUNT directory (beside the cache directory),
    not inside the directory the byte bound names. The fallback keeps this probe
    usable against the pre-BFS-031 layout, which is what the RED run used.
    """
    for d in (mount_dir, cache_dir):
        for _ in range(tries):
            try:
                with open(os.path.join(d, "status.json")) as fh:
                    return json.load(fh)
            except (OSError, json.JSONDecodeError):
                time.sleep(0.2)
    raise SystemExit("could not read status.json")


def status(mount_dir: str, cache_dir: str) -> dict:
    return status_full(mount_dir, cache_dir)["cache"]


def measure(label: str, mount_dir: str, cache_dir: str) -> dict:
    doc = status_full(mount_dir, cache_dir)
    c = doc["cache"]
    d = du(cache_dir)
    st_size = os.path.getsize(os.path.join(mount_dir, "status.json")) if os.path.exists(
        os.path.join(mount_dir, "status.json")) else 0
    cf = os.path.join(mount_dir, "conflicts.jsonl")
    cf_size = os.path.getsize(cf) if os.path.exists(cf) else 0
    row = {"label": label, "du": d, "used": c["used_bytes"], "max": c["max_bytes"],
           "blobs": c["blobs_bytes"], "index": c["index_bytes"], "entries": c["entries"],
           "evictions": c["evictions_total"], "status_json": st_size, "conflicts_jsonl": cf_size,
           "dir_bytes": c.get("dir_bytes"), "peak": c.get("dir_peak_bytes"),
           "state": doc.get("state") or {}}
    print(f"  {label:<34} du(cache dir)={d:>9}  reported dir_bytes={str(c.get('dir_bytes')):>8}  "
          f"used_bytes={c['used_bytes']:>9}  max_bytes={c['max_bytes']:>9}  peak={str(c.get('dir_peak_bytes')):>8}  "
          f"entries={c['entries']}")
    print(f"  {'':<34} (the mount's own state, which the cache bound does NOT name: "
          f"status.json={st_size} conflicts.jsonl={cf_size})")
    return row


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mount", default=os.environ.get("MNT", ""))
    ap.add_argument("--cache-dir", default=os.environ.get("CDIR", ""))
    ap.add_argument("--mount-dir", default=os.environ.get("MDIR", ""),
                    help="where the mount keeps status.json/conflicts.jsonl (BFS-031); "
                         "defaults to the parent of --cache-dir when that is named 'cache'")
    ap.add_argument("--tree", default=os.environ.get("TREE", ""))
    ap.add_argument("--refusals", type=int, default=40)
    args = ap.parse_args()
    if not args.mount or not args.cache_dir or not args.tree:
        raise SystemExit("--mount/--cache-dir/--tree required (or run under mount-arm.sh)")
    mount_dir = args.mount_dir
    if not mount_dir:
        parent, base = os.path.split(os.path.abspath(args.cache_dir))
        mount_dir = parent if base == "cache" else args.cache_dir

    rel = "src/target.txt"
    mnt_target = os.path.join(args.mount, rel)
    srv_target = os.path.join(args.tree, rel)
    body = b"REFUSED-WRITE-BODY-" + b"z" * 43 + b"\n"   # 64 B, same length as target.txt

    print(f"bound chose by the caller; files read: the whole tree through the mount")
    print(f"cache dir  : {args.cache_dir}")
    print(f"mount dir  : {mount_dir}")
    rows = [measure("0. baseline (nothing read)", mount_dir, args.cache_dir)]

    for name in sorted(os.listdir(os.path.join(args.mount, "src"))):
        with open(os.path.join(args.mount, "src", name), "rb") as fh:
            fh.read()
    time.sleep(1.5)
    rows.append(measure("1. after reading the tree", mount_dir, args.cache_dir))

    refusals = 0
    landed = 0
    errno_hist: dict[str, int] = {}
    t0 = time.time()
    for k in range(args.refusals):
        # out of band: the server's bytes change while the client's base does not,
        # so every write below carries a stale base and must be refused.
        content = ((b"SERVER-EDIT-%04d-" % k) + b"q" * 47 + b"\n")[:64].ljust(64, b"q")
        with open(srv_target, "wb") as fh:
            fh.write(content)
        errno_seen = ""
        try:
            # `truncate` is the one reachable write path for an EXISTING file: it is
            # a read-modify-write published as ONE conditional PUT through WritePath
            # (see write_shape_probe.py and finding F-A — in-place writes are answered
            # EOPNOTSUPP because node.Open returns a read handle whatever the flags say).
            os.truncate(mnt_target, 32)
        except OSError as e:
            errno_seen = f"errno={e.errno} ({os.strerror(e.errno) if e.errno else ''})"
            errno_hist[errno_seen] = errno_hist.get(errno_seen, 0) + 1
        # GROUND TRUTH, not the return value: are the server's bytes still the
        # out-of-band content (refused), or truncated to 32 (landed)?
        with open(srv_target, "rb") as fh:
            now = fh.read()
        if now == content[:32]:
            landed += 1
        else:
            refusals += 1
    time.sleep(1.5)
    print(f"  write attempts (truncate): {args.refusals}   refused={refusals}   LANDED={landed}   in {time.time() - t0:.1f}s")
    for k, v in sorted(errno_hist.items(), key=lambda kv: -kv[1]):
        print(f"    errno histogram: {v:>4} x {k}   (ESTALE is the conflict refusal the client raises)")
    rows.append(measure("2. after the refused writes", mount_dir, args.cache_dir))

    doc = status_full(mount_dir, args.cache_dir)
    c = doc["cache"]
    state = doc.get("state") or {}
    cf_path = os.path.join(mount_dir, "conflicts.jsonl")
    cf_lines = 0
    if os.path.exists(cf_path):
        with open(cf_path) as fh:
            cf_lines = sum(1 for line in fh if line.strip())
    over = [(r["label"], r["du"]) for r in rows if r["du"] > c["max_bytes"]]
    print(f"  conflicts recorded        : {rows[-1]['conflicts_jsonl']} bytes / {cf_lines} lines in conflicts.jsonl "
          f"({refusals} refused writes), status.conflicts={json.dumps(doc.get('conflicts', {}))[:200]}")

    rc = 0
    def check(cond: bool, msg: str) -> None:
        nonlocal rc
        print(("  PASS  " if cond else "  FAIL  ") + msg)
        if not cond:
            rc = 1

    # 1. THE ROW'S PROPERTY (BFS-031): the cache DIRECTORY is inside the bound the
    #    client reports. This is the claim the RED run measured at 29.97x.
    check(all(r["used"] <= r["max"] for r in rows),
          "the client's used_bytes never exceeded max_bytes in any phase "
          f"(max used={max(r['used'] for r in rows)}, bound={c['max_bytes']})")
    if over:
        f = max(r[1] for r in over) / c["max_bytes"]
        print(f"  MEASURED: the cache DIRECTORY exceeded the bound in {len(over)} phase(s) — "
              f"worst {max(r[1] for r in over)} B = {f:.2f}x the bound. This is the RED shape "
              f"(BFS-012 §F-C); with the layout of BFS-031 the directory holds only cache bytes.")
    check(not over,
          f"THE CACHE DIRECTORY IS INSIDE THE BOUND in every phase (worst du={max(r['du'] for r in rows)}, "
          f"bound={c['max_bytes']})")
    # 2. THE AGREEMENT: the reported figure against `du`, with the tolerance stated.
    tol = 4096
    deltas = [(r["label"], abs((r["dir_bytes"] or 0) - r["du"])) for r in rows]
    worst = max(d for _, d in deltas)
    check(all(d <= tol for _, d in deltas),
          f"the reported dir_bytes agrees with du of the cache directory within {tol} B in every phase "
          f"(worst delta={worst} B)")
    # 3. THE PEAK the bound is ENFORCED against is inside the bound too.
    check(all((r["peak"] or 0) <= r["max"] for r in rows),
          f"the peak the bound is enforced against (dir_peak_bytes) is inside it in every phase "
          f"(worst={max((r['peak'] or 0) for r in rows)})")
    # 4. The state whose bytes are NOT the cache is bounded and reported.
    if state:
        check(state.get("bytes", 0) <= state.get("max_bytes", 0),
              "the mount's own state (status.json + the refusal log) is inside its own bound: "
              f"{state.get('bytes')} <= {state.get('max_bytes')}")
        check(state.get("footprint_bytes", 0) <= state.get("footprint_max_bytes", 0),
              "the whole local footprint is inside its bound: "
              f"{state.get('footprint_bytes')} <= {state.get('footprint_max_bytes')}")
    else:
        print("  NOTE  this document carries no state block (a pre-BFS-031 build): the state's own "
              "bound is not reported, which is the accounting gap the row names")
    return rc


if __name__ == "__main__":
    raise SystemExit(main())

