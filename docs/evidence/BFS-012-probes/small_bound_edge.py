#!/usr/bin/env python3
"""small_bound_edge.py — what the bound does and does NOT cover on disk (BFS-012).

The row states the property as "the CACHE DIRECTORY's real on-disk size (measure it
yourself) never exceeds the bound". At the default 256 MiB bound that is true with
1.5 KB to spare, because the directory holds three things and only two of them are
counted:

    blobs/          counted   (cache.blobs_bytes)
    index.json      counted   (cache.index_bytes)
    status.json     NOT counted   — the mount's own document, rewritten on a 1 s cadence
    conflicts.jsonl NOT counted   — the refusal LOG, appended once per refused write

`max_bytes` bounds `used_bytes = blobs + index`. So the directory-level claim has a
floor of about `status.json` and grows with the number of REFUSALS, not with the
tree. At a small bound that unaccounted floor is larger than the bound itself.

This probe runs one mount arm at a caller-chosen bound and measures, in order:
  1. baseline (nothing read yet),
  2. after reading every file in the tree through the mount,
  3. after N refused writes (each one appends a line to conflicts.jsonl),
reporting `du -sb` of the directory, the client's `used_bytes`/`max_bytes`, and the
ratio. It ASSERTS the client's own contract (used_bytes <= max_bytes always) and
REPORTS the directory-level claim with its measured factor, because that difference
is the finding: local storage that grows with refusals is still local storage that
grows, even though it does not grow with the tree.
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


def status(cache_dir: str, tries: int = 30) -> dict:
    return status_full(cache_dir, tries)["cache"]


def status_full(cache_dir: str, tries: int = 30) -> dict:
    for _ in range(tries):
        try:
            with open(os.path.join(cache_dir, "status.json")) as fh:
                return json.load(fh)
        except (OSError, json.JSONDecodeError):
            time.sleep(0.2)
    raise SystemExit("could not read status.json")


def measure(label: str, cache_dir: str) -> dict:
    c = status(cache_dir)
    d = du(cache_dir)
    st_size = os.path.getsize(os.path.join(cache_dir, "status.json"))
    cf = os.path.join(cache_dir, "conflicts.jsonl")
    cf_size = os.path.getsize(cf) if os.path.exists(cf) else 0
    row = {"label": label, "du": d, "used": c["used_bytes"], "max": c["max_bytes"],
           "blobs": c["blobs_bytes"], "index": c["index_bytes"], "entries": c["entries"],
           "evictions": c["evictions_total"], "status_json": st_size, "conflicts_jsonl": cf_size}
    print(f"  {label:<34} du(cache dir)={d:>9}  used_bytes={c['used_bytes']:>9}  max_bytes={c['max_bytes']:>9}  "
          f"status.json={st_size:>6}  conflicts.jsonl={cf_size:>7}  entries={c['entries']}")
    return row


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mount", default=os.environ.get("MNT", ""))
    ap.add_argument("--cache-dir", default=os.environ.get("CDIR", ""))
    ap.add_argument("--tree", default=os.environ.get("TREE", ""))
    ap.add_argument("--refusals", type=int, default=40)
    args = ap.parse_args()
    if not args.mount or not args.cache_dir or not args.tree:
        raise SystemExit("--mount/--cache-dir/--tree required (or run under mount-arm.sh)")

    rel = "src/target.txt"
    mnt_target = os.path.join(args.mount, rel)
    srv_target = os.path.join(args.tree, rel)
    body = b"REFUSED-WRITE-BODY-" + b"z" * 43 + b"\n"   # 64 B, same length as target.txt

    print(f"bound chose by the caller; files read: the whole tree through the mount")
    rows = [measure("0. baseline (nothing read)", args.cache_dir)]

    for name in sorted(os.listdir(os.path.join(args.mount, "src"))):
        with open(os.path.join(args.mount, "src", name), "rb") as fh:
            fh.read()
    time.sleep(1.5)
    rows.append(measure("1. after reading the tree", args.cache_dir))

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
    rows.append(measure("2. after the refused writes", args.cache_dir))

    c = status(args.cache_dir)
    cf_path = os.path.join(args.cache_dir, "conflicts.jsonl")
    cf_lines = 0
    if os.path.exists(cf_path):
        with open(cf_path) as fh:
            cf_lines = sum(1 for line in fh if line.strip())
    over = [(r["label"], r["du"]) for r in rows if r["du"] > c["max_bytes"]]
    print(f"  conflicts recorded        : {rows[-1]['conflicts_jsonl']} bytes / {cf_lines} lines in conflicts.jsonl "
          f"({refusals} refused writes), status.conflicts={json.dumps(status_full(args.cache_dir).get('conflicts', {}))}")

    rc = 0
    def check(cond: bool, msg: str) -> None:
        nonlocal rc
        print(("  PASS  " if cond else "  FAIL  ") + msg)
        if not cond:
            rc = 1

    # The client's own contract, in every phase.
    check(all(r["used"] <= r["max"] for r in rows),
          "the client's used_bytes never exceeded max_bytes in any phase "
          f"(max used={max(r['used'] for r in rows)}, bound={c['max_bytes']})")
    # The row's directory-level claim, measured rather than assumed.
    if over:
        f = max(r[1] for r in over) / c["max_bytes"]
        print(f"  MEASURED (finding F-C): the cache DIRECTORY exceeded the bound in {len(over)} phase(s) — "
              f"worst {max(r[1] for r in over)} B = {f:.2f}x the bound — because status.json and "
              f"conflicts.jsonl are outside used_bytes. At a bound this small the unaccounted floor "
              f"(status.json plus the refusal log) is larger than the bound itself. BFS-012 report §F-C.")
    else:
        print("  the cache DIRECTORY also stayed under the bound in every phase "
              "(status.json + conflicts.jsonl fit inside it)")
    return rc


if __name__ == "__main__":
    raise SystemExit(main())
