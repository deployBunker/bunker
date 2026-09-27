#!/usr/bin/env python3
"""estale_retry_experiment.py — is the mount-level retry specific to the ESTALE refusal?

The trace arm measured this: ONE `truncate(2)` syscall (strace: exactly one, rc=0)
produces TWO conditional PUTs through the mount — a 412 refusAL that is recorded in
conflicts.jsonl, and then a second PUT that LANDS (204). In the accurate-base arm
the same syscall produces exactly one PUT. So something below the client's own API
re-issues the operation after a refusal, and the client's rule 3 (adopt the refused
base) means that re-issued operation succeeds.

The client returns ESTALE (116) for a conflict, and its own comment names the class
"write precondition refused: re-read and retry". This experiment tests the hunch
that the re-issue is tied to that errno: it builds a binary whose conflict refusal
carries EIO (5) instead of ESTALE, runs the SAME trace arm against it, and compares
the request sequences. If the second PUT disappears, the retry is ESTALE-specific
and the errno choice is the lever; if it survives, the retry is generic and the
finding is bigger. The file is restored byte-for-byte and verified afterwards.

usage: estale_retry_experiment.py [--work /tmp/bfs012] [--repo .]
"""
from __future__ import annotations

import argparse
import hashlib
import os
import subprocess
import sys

TARGET_REL = os.path.join("internal", "fsclient", "client.go")
ANCHOR = "\t\te.Errno = ErrnoESTALE\n\t\te.Cause = CauseConflict"
MUTATED = "\t\te.Errno = ErrnoEIO // EXPERIMENT ONLY: the ESTALE-specific-retry test\n\t\te.Cause = CauseConflict"


def run(cmd, **kw):
    return subprocess.run(cmd, capture_output=True, text=True, timeout=900, **kw)


def trace_essence(run_dir: str) -> str:
    import json
    out = []
    with open(os.path.join(run_dir, "requests.jsonl")) as fh:
        for line in fh:
            if not line.strip():
                continue
            r = json.loads(line)
            if r["method"] in ("PUT", "GET") and "target.txt" in r["path"]:
                out.append(f"{r['method']} {r['status']} if-match={(r.get('if_match') or '-')[:14]}")
    return "\n".join(out)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--work", default="/tmp/bfs012")
    ap.add_argument("--repo", default=os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..")))
    args = ap.parse_args()

    path = os.path.join(args.repo, TARGET_REL)
    original = open(path, "rb").read()
    before = hashlib.sha256(original).hexdigest()
    text = original.decode()
    if text.count(ANCHOR) != 1:
        print(f"REFUSING: the anchor occurs {text.count(ANCHOR)} times", file=sys.stderr)
        return 2
    print(f"client.go sha256 before : {before}")

    mb = os.path.join(args.repo, "docs", "evidence", "BFS-012-probes", "mount-arm.sh")
    tree = os.path.join(args.work, "conflict-tree9")
    run(["python3", os.path.join(os.path.dirname(os.path.abspath(__file__)), "mkconflict_tree.py"), tree])

    try:
        open(path, "w").write(text.replace(ANCHOR, MUTATED, 1))
        print(f"mutated client.go sha256: {hashlib.sha256(open(path, 'rb').read()).hexdigest()}")
        b = run(["go", "build", "-o", os.path.join(args.work, "bin", "bunker-eio"), "./cmd/bunker"], cwd=args.repo)
        if b.returncode != 0:
            print("build failed:\n" + b.stderr[-2000:], file=sys.stderr)
            return 3
        print()
        print("=== arm with the conflict errno = EIO (5) instead of ESTALE (116) ===")
        r = run(["bash", mb, "--label", "M-eio", "--tree", tree,
                 "--bin", os.path.join(args.work, "bin", "bunker-eio"),
                 "--davserve", os.path.join(args.work, "bin", "davserve"),
                 "--trace", "--trace-port", "18474",
                 "--reader", os.path.join(os.path.dirname(mb), "truncate_diag.py"),
                 "--reader-args", "--iters 1 --mode edit", "--mount-timeout-s", "120"])
        tail = r.stdout[-3000:]
        print(tail)
        print("--- PUT/GET sequence on the target (mutated build) ---")
        try:
            print(trace_essence(os.path.join(args.work, "run-M-eio")))
        except Exception as e:  # noqa: BLE001
            print(f"(could not read the trace: {e})")
    finally:
        open(path, "wb").write(original)
        after = hashlib.sha256(open(path, "rb").read()).hexdigest()
        print()
        print(f"client.go sha256 restored: {after}")
        print(f"RESTORE VERIFIED         : {after == before}")
        if after != before:
            print("STOP: not restored byte-for-byte", file=sys.stderr)
            return 4
        run(["go", "build", "-o", os.path.join(args.work, "bin", "bunker"), "./cmd/bunker"], cwd=args.repo)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
