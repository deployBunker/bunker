#!/usr/bin/env python3
"""mutation_mtime_identity.py — the non-vacuity proof for the touch-only arm.

A test that cannot fail proves nothing. BFS-012 adds
`TestTouchOnlyChangeIsNotAConflict` to internal/fsclient (a change that moves ONLY
the mtime, with the bytes identical, must NOT be refused). This script makes the
defect class it guards against REAL, in the smallest way that is honest:

  MUTATION — the served content identity is made to depend on the mtime
             (internal/server/webdav/tree.go, `hashFile`: the hash that
             `ETag`/`X-Bunker-Hash`/the If-Match precondition all come from). This
             is exactly what "the conflict rule is metadata, not content" looks
             like in code — a metadata-derived identity — and it is the reason the
             row asks for the mtime-preserved case by name.

Then:

  1. the arm under test must FAIL (RED) while the mutation is in the tree;
  2. the file is restored from the bytes read before the mutation and its sha256
     is re-checked against the value recorded before the edit (an unrestored
     mutation left in a shared tree is worse than no proof at all);
  3. the same arm must PASS again (GREEN).

It also runs the neighbouring arms so the report can say WHICH tests are sensitive
to a metadata-derived identity and which are not.

Nothing here is a fix: the mutation exists only inside this script's run, is
verified byte-for-byte on the way out, and never reaches a commit.

usage: mutation_mtime_identity.py [--file PATH] [--keep]   (--keep leaves the mutation)
"""
from __future__ import annotations

import argparse
import hashlib
import os
import subprocess
import sys

ANCHOR = '''\th := "sha256:" + hex.EncodeToString(hs.Sum(nil))

\tt.mu.Lock()'''

MUTATED = '''\t// MUTATION (BFS-012 non-vacuity proof): the served content identity now depends
\t// on the mtime, which is the defect class the touch-only arm exists to catch.
\ths2 := sha256.New()
\tfmt.Fprintf(hs2, "%s|%d", hex.EncodeToString(hs.Sum(nil)), mtime)
\th := "sha256:" + hex.EncodeToString(hs2.Sum(nil))

\tt.mu.Lock()'''

ARMS = [
    "TestTouchOnlyChangeIsNotAConflict",
    "TestConflictRefusalIgnoresMtimePreservedEdit",
    "TestWriteIdenticalContentIsARecordedNoop",
]


def run_tests(go_root: str) -> dict[str, str]:
    cmd = ["go", "test", "./internal/fsclient/", "-count=1", "-v",
           "-run", "|".join(ARMS)]
    p = subprocess.run(cmd, cwd=go_root, capture_output=True, text=True, timeout=900)
    out = p.stdout + p.stderr
    res: dict[str, str] = {}
    for line in out.splitlines():
        line = line.strip()
        for arm in ARMS:
            if line.startswith(f"--- PASS: {arm}"):
                res[arm] = "PASS"
            elif line.startswith(f"--- FAIL: {arm}"):
                res[arm] = "FAIL"
        if line.startswith(("FAIL", "ok ", "build failed")) and "FAIL" in line and line.startswith("FAIL"):
            res["_suite"] = line
    if "build failed" in out or "cannot find package" in out:
        res["_suite"] = "BUILD FAILED\n" + out[-2000:]
    return res


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--file", default=None)
    ap.add_argument("--keep", action="store_true")
    args = ap.parse_args()

    here = os.path.dirname(os.path.abspath(__file__))
    go_root = os.path.abspath(os.path.join(here, "..", "..", ".."))
    path = args.file or os.path.join(go_root, "internal", "server", "webdav", "tree.go")

    original = open(path, "rb").read()
    before = hashlib.sha256(original).hexdigest()
    text = original.decode()
    if text.count(ANCHOR) != 1:
        print(f"REFUSING TO MUTATE: the anchor occurs {text.count(ANCHOR)} times in {path}; "
              f"a mutation that is not uniquely anchored is a fake RED", file=sys.stderr)
        return 2
    if MUTATED in text:
        print("REFUSING TO MUTATE: the mutation is already present", file=sys.stderr)
        return 2

    print(f"file under mutation : {path}")
    print(f"sha256 before       : {before}")
    print(f"go root             : {go_root}")
    print(f"arms                : {', '.join(ARMS)}")
    print()

    print("=== 1. GREEN baseline (unmutated tree) ===")
    base = run_tests(go_root)
    for arm in ARMS:
        print(f"  {base.get(arm, 'NOT RUN'):<5} {arm}")

    print()
    print("=== 2. RED: content identity derived from the mtime ===")
    open(path, "w").write(text.replace(ANCHOR, MUTATED, 1))
    print(f"  mutated sha256      : {hashlib.sha256(open(path, 'rb').read()).hexdigest()}")
    red = run_tests(go_root)
    for arm in ARMS:
        print(f"  {red.get(arm, 'NOT RUN'):<5} {arm}")

    print()
    print("=== 3. RESTORE (from the bytes read before the edit) ===")
    if not args.keep:
        open(path, "wb").write(original)
    after = hashlib.sha256(open(path, "rb").read()).hexdigest()
    print(f"  sha256 after restore: {after}")
    print(f"  RESTORE VERIFIED    : {after == before}")
    if after != before:
        print("STOP: the file was not restored byte-for-byte", file=sys.stderr)
        return 3
    green = run_tests(go_root)
    for arm in ARMS:
        print(f"  {green.get(arm, 'NOT RUN'):<5} {arm}")

    sensitive = [a for a in ARMS if red.get(a) == "FAIL"]
    print()
    print(f"sensitive to a metadata-derived identity: {sensitive or 'NONE — the mutation would be a fake RED'}")
    proven = (red.get("TestTouchOnlyChangeIsNotAConflict") == "FAIL"
              and green.get("TestTouchOnlyChangeIsNotAConflict") == "PASS"
              and after == before)
    print(f"NON-VACUITY: {'PROVEN' if proven else 'NOT PROVEN'}")
    return 0 if proven else 1


if __name__ == "__main__":
    raise SystemExit(main())
