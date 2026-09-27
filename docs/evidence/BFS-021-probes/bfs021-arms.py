#!/usr/bin/env python3
"""bfs021-arms.py — BFS-021: an append through the mount must land its bytes, or
refuse LOUDLY without losing any.

THE ROW. `>>` against a file on the mount fails ('I/O error' / 'Operation not
supported') and the appended bytes are gone. Two different defects look alike
here and the arms separate them:

  * the appended bytes never land (the caller lost what it wrote), while the
    ORIGINAL survives. That is this row.
  * the original is destroyed by the failed operation. That is BFS-030's class
    (already fixed) and these arms assert it does NOT happen, so a green here
    cannot be bought by moving the loss one byte earlier.

So every arm asserts the FULL byte-level outcome — read the file back through the
mount AND on the server's own disk, compare sha256 — rather than the caller's
exit code. An arm whose expectation is `loss` PASSES when the bytes are provably
absent and the original is provably intact; an arm whose expectation is `ok`
PASSES only when the read-back is byte-for-byte `original + tail`.

The shapes are the ones a caller actually uses (each is measured, not assumed):
  sh       — the shell's `>>` (the shape the row names)
  fd       — os.open(O_WRONLY|O_APPEND) + write + close
  ab       — python's builtin `open(path,'ab')`
  rdwr     — os.open(O_RDWR|O_APPEND), write, then read back through the same fd
  multi    — THREE writes inside ONE open (`exec 3>>f`), i.e. one handle, three
             chunks: the multi-chunk append a naive one-shot fix would break
  new      — `>>` a path that does not exist: the control that must always land
             (the mount creates it), so the arms cannot be green by refusing
             everything
  pwrite   — a second write at the SAME offset (the caller's retry). Reported,
             not asserted: what the kernel sends is the kernel's business, and
             the idempotence cell lives in the package tests where the offset is
             exact.
  reader   — a reader reading WHILE appends land: every read must be one of the
             complete states, never a torn mixture.

usage: bfs021-arms.py --mode MODE --expect {loss,ok}   (run under mount-arm.sh)
"""
from __future__ import annotations

import argparse
import errno as errno_mod
import hashlib
import json
import os
import subprocess
import sys
import threading
import time

TARGET = "src/target.txt"
NEWFILE = "src/fresh.txt"
BASE = b"BFS-021-BASE-CONTENT-" + b"A" * 60 + b"\n"
TAIL = b"APPENDED-TAIL-0123456789\n"
# EXTRA_CHUNKS: the chunks a mode writes AFTER the first one. Only `multi` writes
# more than one inside a single open, and it is the mode that measures a later
# publication on the SAME handle.
EXTRA_CHUNKS = {"multi": [b"SECOND-CHUNK\n", b"THIRD-CHUNK\n"]}


def sha(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def errname(e: int | None) -> str:
    return errno_mod.errorcode.get(e or 0, "")


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


def status(cache_dir: str, tries: int = 40) -> dict:
    for _ in range(tries):
        try:
            with open(os.path.join(cache_dir, "status.json")) as fh:
                return json.load(fh)
        except (OSError, json.JSONDecodeError):
            time.sleep(0.25)
    return {}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", required=True,
                    choices=["sh", "fd", "ab", "rdwr", "multi", "new", "pwrite", "reader",
                             "rplus", "trunc", "cost"])
    ap.add_argument("--expect", required=True, choices=["loss", "silent", "ok"])
    ap.add_argument("--mount", default=os.environ.get("MNT", ""))
    ap.add_argument("--tree", default=os.environ.get("TREE", ""))
    ap.add_argument("--cache-dir", default=os.environ.get("CDIR", ""))
    ap.add_argument("--run-dir", default=os.environ.get("OUT", ""))
    ap.add_argument("--appends", default="12", help="reader arm: how many appends to interleave")
    args = ap.parse_args()
    if not args.mount or not args.tree:
        raise SystemExit("--mount/--tree required (or run under mount-arm.sh)")

    rc = 0

    def check(cond: bool, msg: str) -> None:
        nonlocal rc
        print(("  PASS  " if cond else "  FAIL  ") + msg)
        if not cond:
            rc = 1

    def report(k: str, v) -> None:
        print(f"BFS021-MEASURE {k}={v}")

    mnt_target = os.path.join(args.mount, TARGET)
    srv_target = os.path.join(args.tree, TARGET)
    mnt_new = os.path.join(args.mount, NEWFILE)
    srv_new = os.path.join(args.tree, NEWFILE)

    def seed(path: str, body: bytes) -> None:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as fh:
            fh.write(body)

    def server() -> bytes:
        with open(srv_target, "rb") as fh:
            return fh.read()

    def read_back(limit: int = 1 << 20) -> bytes:
        with open(mnt_target, "rb") as fh:
            return fh.read(limit)

    # --------------------------------------------------------- the fixture, first
    # The fixture is written on the server's own disk and then READ THROUGH THE
    # MOUNT. That read is what fixes the client's base for the path (BFS-005
    # §5.3) and it is also what publishes i_size to the kernel — i.e. what makes
    # the append offset well defined. Both are asserted, not assumed.
    seed(srv_target, BASE)
    print(f"=== mode={args.mode}  expect={args.expect} ===")
    print(f"  server fixture            : {len(BASE)} B sha256={sha(BASE)[:16]}… (written on the server's own disk)")
    r0 = read_back()
    check(r0 == BASE, "FIXTURE: the first read through the mount serves the baseline byte-for-byte")
    report("baseline_size", len(BASE))
    report("baseline_sha", sha(BASE))

    # --------------------------------------------------------- the new-path control
    if args.mode == "new":
        if os.path.exists(srv_new):
            os.unlink(srv_new)
        print(f"=== mode=new  `>>` a path that does not exist ({NEWFILE}) ===")
        got, detail, err = run_shape("sh", mnt_new, TAIL)
        report("mode", "new")
        report("landed", got)
        report("detail", json.dumps(detail))
        report("errno", err)
        print(f"  shape returned            : {'ok' if got else 'ERROR'} — {detail}")
        # the mount answers the create from its own snapshot, so the server file
        # may appear a moment later; wait for it before comparing.
        for _ in range(200):
            if os.path.exists(srv_new):
                break
            time.sleep(0.01)
        srv = b""
        try:
            with open(srv_new, "rb") as fh:
                srv = fh.read()
        except OSError:
            pass
        print(f"  server {NEWFILE}: {len(srv)} B sha256={sha(srv)[:16]}…")
        if args.expect == "ok":
            check(got, "`>>` to a NEW path must land (the create path works — this is the control)")
            check(srv == TAIL, f"the new file must hold exactly the appended bytes, got {srv!r}")
            print()
            print(f"RESULT: new-path append landed, {len(srv)} B")
        return rc

    # ---------------------------------------------------------------- every other mode
    if args.mode == "reader":
        return reader_arm(args, check, report, read_back, server, seed)

    if args.mode == "cost":
        # THE SUCCESSFUL PATH, as a number: N appends of one small tail to the same
        # file, each its own open/write/close — the `>>` shape a log or a shell uses.
        # An append is served by read-modify-publish, so the figure to read beside
        # the latency is the BYTES PUBLISHED: the whole file per append.
        n = int(os.environ.get("COST_APPENDS", "50"))
        t_first = time.monotonic()
        times: list[float] = []
        for _ in range(n):
            t0 = time.monotonic()
            fd = os.open(mnt_target, os.O_WRONLY | os.O_APPEND)
            os.write(fd, TAIL)
            os.close(fd)
            times.append((time.monotonic() - t0) * 1000)
        total = (time.monotonic() - t_first) * 1000
        time.sleep(0.5)
        srv = server()
        reqs = [r for r in requests(args.run_dir) if r.get("path", "").endswith(TARGET)]
        puts = [r for r in reqs if r.get("method") == "PUT"]
        gets = [r for r in reqs if r.get("method") == "GET"]
        st = times[len(times) // 2]
        report("mode", "cost")
        report("appends", n)
        report("file_bytes_start", len(BASE))
        report("file_bytes_final", len(srv))
        report("ms_total", f"{total:.0f}")
        report("ms_median", f"{st:.2f}")
        report("ms_mean", f"{sum(times) / len(times):.2f}")
        report("ms_p95", f"{sorted(times)[int(len(times) * 0.95)]:.2f}")
        report("ms_min", f"{min(times):.2f}")
        report("ms_max", f"{max(times):.2f}")
        report("requests_put", len(puts))
        report("requests_get", len(gets))
        report("bytes_put", sum(int(r.get("req_bytes") or 0) for r in puts))
        report("bytes_get", sum(int(r.get("resp_bytes") or 0) for r in gets))
        print(f"  {n} appends to one file   : {total:.0f} ms total, {st:.2f} ms median, {sum(times)/len(times):.2f} ms mean")
        print(f"  file                      : {len(BASE)} B -> {len(srv)} B, sha256={sha(srv)[:16]}…")
        print(f"  wire                      : {len(puts)} PUT ({sum(int(r.get('req_bytes') or 0) for r in puts)} B published), {len(gets)} GET")
        check(len(srv) == len(BASE) + n * len(TAIL), "every append landed: the file grew by exactly N tails")
        check(srv.count(TAIL) == n, f"each tail landed exactly once, saw {srv.count(TAIL)}")
        check(len(puts) == n, f"one publication per append, saw {len(puts)}")
        print()
        print(f"RESULT: append costs ONE GET + ONE whole-file PUT, {st:.2f} ms median per append "
              f"({sum(int(r.get('req_bytes') or 0) for r in puts)} B published for {n * len(TAIL)} B appended)")
        return rc

    if args.mode == "pwrite":
        # a second write at the SAME offset: the caller's retry. Reported.
        fd = os.open(mnt_target, os.O_WRONLY | os.O_APPEND)
        try:
            n1 = os.write(fd, TAIL)
            n2 = 0
            err2 = None
            try:
                n2 = os.pwrite(fd, TAIL, len(BASE))
            except OSError as e:
                err2 = e.errno
        finally:
            os.close(fd)
        time.sleep(0.5)
        srv = server()
        report("mode", "pwrite")
        report("first_write", n1)
        report("retry_write", n2)
        report("retry_errno", err2)
        print(f"  server after              : {len(srv)} B sha256={sha(srv)[:16]}…")
        print(f"  tail occurrences          : {srv.count(TAIL)}")
        print(f"RESULT: the retry-at-the-same-offset arm is REPORTED, not asserted "
              f"(n1={n1} n2={n2} errno={err2} tail_count={srv.count(TAIL)})")
        return 0

    before_reqs = len(requests(args.run_dir)) if args.run_dir else 0
    t0 = time.monotonic()
    landed, detail, err = run_shape(args.mode, mnt_target, TAIL)
    dt = (time.monotonic() - t0) * 1000
    time.sleep(0.6)  # let the publication reach the server
    srv = server()
    srv_sha = sha(srv)
    # The chunks this mode writes, in order. `multi` writes three inside ONE open,
    # and each of them is closed (a `printf >&3` duplicates fd 3 onto fd 1 and
    # closes it) — which is ALSO a publication point, so this mode is the one that
    # measures that a later publication on the same handle carries the earlier
    # one's bytes instead of refusing on a stale precondition.
    extra = EXTRA_CHUNKS.get(args.mode, [])
    want = BASE + TAIL + b"".join(extra)
    want_publications = 1 + len(extra)
    tail_present = srv.find(TAIL) >= 0
    original_intact = srv.startswith(BASE)
    tail_count = srv.count(TAIL)

    print(f"  shape returned            : {'ok' if landed else 'ERROR'} — {detail}")
    report("mode", args.mode)
    report("landed", str(landed).lower())
    report("errno", err)
    report("errno_name", errname(err))
    report("detail", json.dumps(detail))
    report("elapsed_ms", f"{dt:.1f}")
    report("server_bytes", len(srv))
    report("server_sha", srv_sha)
    report("want_sha", sha(want))
    report("tail_present", str(tail_present).lower())
    report("original_intact", str(original_intact).lower())
    report("tail_count", tail_count)

    # The LOST BYTES, stated as a claim the arm decides rather than a remark:
    # the appended tail is absent from the file, read back, after the operation.
    if args.expect == "silent":
        # THE WORST SHAPE, and a distinct defect from `loss`: the operation
        # REPORTS SUCCESS and the bytes are still gone. A caller that checks the
        # exit status cannot know. Measured on the unfixed tree for the
        # one-open/three-writes shape (`exec 3>>f`), where the shell reports 0.
        check(landed, "the shape REPORTS SUCCESS to the caller (this is the silent shape)")
        check(not tail_present, "THE LOST BYTES: the shape reported success and the tail is NOT in the file")
        check(original_intact, "the original prefix survives")
        check(srv_sha == sha(BASE), "the server file is byte-identical to the fixture")
        print()
        print(f"RESULT: SILENT LOSS — the shape returned success, the server still holds "
              f"{len(BASE)} B sha256={sha(BASE)[:16]}…, and the {len(TAIL)} B it claimed to write are nowhere")
        return rc

    if args.expect == "loss":
        check(not landed, "the shape FAILS on this tree (the filed failure)")
        check(not tail_present, "THE LOST BYTES: the appended tail is NOT in the file read back")
        check(original_intact, "the original prefix survives the failed append (BFS-030's class does not recur)")
        check(srv_sha == sha(BASE), f"the server file is byte-identical to the fixture, not merely prefix-equal")
        print()
        print(f"RESULT: append LOST its bytes — shape reported an error, server still holds "
              f"{len(BASE)} B sha256={sha(BASE)[:16]}…, appended {len(TAIL)} B nowhere")
        return rc

    # expect == ok
    check(landed, "the append SUCCEEDS (no error reaches the caller)")
    check(original_intact, "the original prefix is intact byte-for-byte")
    check(tail_present, "the appended bytes ARE in the file")
    check(srv == want, f"the read-back is byte-for-byte original+every chunk: got {len(srv)} B {srv_sha[:16]}… want {len(want)} B {sha(want)[:16]}…")
    for c in [TAIL] + extra:
        check(srv.count(c) == 1, f"the chunk {c!r} appears exactly once (a retry must not double-apply it), saw {srv.count(c)}")
    # and the same bytes must be visible THROUGH the mount, not only on disk
    try:
        back = read_back()
    except OSError as e:
        back = b"<read failed: %s>" % str(e).encode()
    check(back == want, f"the mount serves the appended content read-back too ({len(back)} B)")
    if args.run_dir:
        mine = [r for r in requests(args.run_dir)[before_reqs:]
                if r.get("path", "").endswith(TARGET)]
        puts = [r for r in mine if r.get("method") == "PUT"]
        gets = [r for r in mine if r.get("method") == "GET"]
        print(f"  requests for this append  : {len(puts)} PUT, {len(gets)} GET")
        print(f"  PUT details               : {json.dumps([{k: r.get(k) for k in ('status','req_bytes','if_match','noop','verdict')} for r in puts])}")
        check(len(puts) == want_publications,
              f"ONE publication per close ({want_publications} here), never one request per chunk — saw {len(puts)}")
        check(all(isinstance(r.get("status"), int) and 200 <= r["status"] < 300 for r in puts),
              f"every publication landed (no 412 on a handle's own later publication): {[r.get('status') for r in puts]}")
    print()
    print(f"RESULT: append landed — {len(srv)} B sha256={srv_sha[:16]}… , original+every chunk byte-for-byte, each chunk once, in {dt:.0f} ms")
    return rc


def run_shape(mode: str, path: str, tail: bytes):
    """Perform one real append shape. Returns (landed, detail, errno or None)."""
    if mode in ("sh", "new"):
        # the shell's `>>`, verbatim: the shape the row filed
        p = subprocess.run(["/bin/sh", "-c", 'printf %s "$1" >> "$2"', "sh", tail.decode(), path],
                           capture_output=True)
        if p.returncode == 0:
            return True, "sh -c 'printf ... >> file' returned 0", None
        detail = (p.stderr.decode(errors="replace").strip() or p.stdout.decode(errors="replace").strip())
        return False, f"shell rc={p.returncode}: {detail}", None
    if mode == "multi":
        # THREE writes inside ONE open: one handle, three chunks. Each `printf >&3`
        # duplicates fd 3 onto fd 1 and closes it again, so the kernel asks for a
        # publication point PER close — which is why this shape also measures that
        # a second publication on the same handle carries the FIRST one's bytes.
        script = 'exec 3>>"$1"; printf %s "$2" >&3; printf %s "$3" >&3; printf %s "$4" >&3; exec 3>&-'
        p = subprocess.run(["/bin/sh", "-c", script, "sh", path, tail.decode(),
                            "SECOND-CHUNK\n", "THIRD-CHUNK\n"], capture_output=True)
        if p.returncode == 0:
            return True, "three writes inside one `exec 3>>` returned 0", None
        detail = (p.stderr.decode(errors="replace").strip() or p.stdout.decode(errors="replace").strip())
        return False, f"shell rc={p.returncode}: {detail}", None
    if mode == "fd":
        try:
            fd = os.open(path, os.O_WRONLY | os.O_APPEND)
        except OSError as e:
            return False, f"open(O_WRONLY|O_APPEND) failed: {e}", e.errno
        try:
            os.write(fd, tail)
            os.close(fd)
            return True, "os.open(O_WRONLY|O_APPEND)+write returned cleanly", None
        except OSError as e:
            try:
                os.close(fd)
            except OSError:
                pass
            return False, f"write failed: {e}", e.errno
    if mode == "ab":
        try:
            with open(path, "ab") as fh:
                fh.write(tail)
                fh.flush()
            return True, "open(path,'ab')+write returned cleanly", None
        except OSError as e:
            return False, f"open('ab') write failed: {e}", e.errno
    if mode == "rdwr":
        try:
            fd = os.open(path, os.O_RDWR | os.O_APPEND)
        except OSError as e:
            return False, f"open(O_RDWR|O_APPEND) failed: {e}", e.errno
        try:
            os.write(fd, tail)
            back = os.pread(fd, 4096, 0)
            os.close(fd)
            return True, f"read-back through the same fd: {len(back)} B", None
        except OSError as e:
            try:
                os.close(fd)
            except OSError:
                pass
            return False, f"write failed: {e}", e.errno
    if mode == "rplus":
        # a whole-file in-place rewrite through an EXISTING file: `open('r+b')`
        # then a write at offset 0. BFS-012 measured this as refused and BFS-030
        # kept it refused; this arm is the regression guard for that.
        try:
            with open(path, "r+b") as fh:
                fh.write(tail)
                fh.flush()
            return True, "open('r+b')+write returned cleanly", None
        except OSError as e:
            return False, f"r+b write failed: {e}", e.errno
    if mode == "trunc":
        # the shell's `>` (open O_WRONLY|O_CREAT|O_TRUNC) on an EXISTING file:
        # BFS-030's shape, refused at the kernel's O_TRUNC half.
        try:
            with open(path, "wb") as fh:
                fh.write(tail)
                fh.flush()
            return True, "open('wb')+write returned cleanly", None
        except OSError as e:
            return False, f"wb write failed: {e}", e.errno
    return False, f"unknown mode {mode}", None


def reader_arm(args, check, report, read_back, server, seed) -> int:
    """A reader reading the file WHILE appends land.

    The guarantee under test: a FUSE read is a snapshot of one whole content —
    the old complete content or the new complete content, never a torn mixture.
    The complete states are known exactly (BASE + T1..Tk), so a torn read is a
    sha256 that is in no state at all. A refusal (ESTALE) is a legitimate,
    documented outcome (BFS-025's read bound) and is counted, not failed; what is
    forbidden is a read that returns a FRAGMENT with no error.
    """
    n = int(args.appends)
    tails = [f"T{i:04d}\n".encode() for i in range(1, n + 1)]
    states: list[bytes] = [BASE]
    for t in tails:
        states.append(states[-1] + t)
    allowed = {sha(s): len(s) for s in states}

    read_back()  # fix the kernel's i_size for the path before the writer starts
    stop = threading.Event()
    written = [0]
    writer_err: list[str] = []

    def writer():
        for t in tails:
            if stop.is_set():
                break
            try:
                fd = os.open(os.path.join(args.mount, TARGET), os.O_WRONLY | os.O_APPEND)
                os.write(fd, t)
                os.close(fd)
            except OSError as e:
                writer_err.append(f"append {t!r}: {e}")
                break
            # wait until the server really holds the new state, so the allowed
            # set is the server's truth and not the writer's belief
            for _ in range(400):
                if len(server()) == len(states[written[0] + 1]):
                    break
                time.sleep(0.005)
            written[0] += 1
        stop.set()

    th = threading.Thread(target=writer, daemon=True)
    th.start()
    torn: list[str] = []
    refused = 0
    served = 0
    deadline = time.monotonic() + 60
    while not stop.is_set() or time.monotonic() < deadline:
        try:
            got = read_back()
            served += 1
            h = sha(got)
            if h not in allowed:
                torn.append(f"len={len(got)} sha256={h[:16]}…")
        except OSError as e:
            if e.errno in (116, 5, 11):
                refused += 1
                time.sleep(0.005)
                continue
            torn.append(f"read raised {e}")
        if stop.is_set() and time.monotonic() > deadline:
            break
        if stop.is_set() and served > 4:
            break
    th.join(timeout=30)
    srv = server()
    report("mode", "reader")
    report("appends", written[0])
    report("reads_served", served)
    report("reads_refused", refused)
    report("writer_errors", json.dumps(writer_err))
    report("torn_reads", json.dumps(torn))
    print(f"  appends landed            : {written[0]}/{n}  {('(writer stopped: ' + writer_err[0] + ')') if writer_err else ''}")
    print(f"  reads through the mount   : {served} served, {refused} refused (ESTALE — the documented re-open recovery)")
    print(f"  server final              : {len(srv)} B sha256={sha(srv)[:16]}…")
    print(f"  torn reads                : {len(torn)} {torn[:3]}")
    check(written[0] == n, f"every append landed ({written[0]}/{n})")
    check(srv == states[n], "the server holds exactly BASE+T1..Tn (no append was lost or doubled)")
    check(served > 0, "a concurrent reader was actually served (the arm is not vacuous)")
    check(not torn, f"NO torn read: every read was one of the {len(states)} complete states")
    print()
    print(f"RESULT: {written[0]} appends against {served} concurrent reads, {len(torn)} torn")
    return 0


if __name__ == "__main__":
    sys.exit(main())
