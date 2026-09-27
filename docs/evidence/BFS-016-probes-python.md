# BFS-016 — the Python probes, verbatim

These four probes produced the Python-side evidence in this directory. They are committed as
TEXT rather than as `.py` files because this repo ignores `*.py` globally (`.gitignore:37`,
exception only for `tools/*.py`) — the alternative would be force-adding past a repo convention.
To run one: copy the block to a file (e.g. `/tmp/probe.py`) and run it with python3.

## `mkfixture.py` — builds the deterministic fixture tree the battery measures

```python
#!/usr/bin/env python3
"""Build the fixture tree the bunker-fs battery measures.

Layout is chosen so the COMMITTED instrument (probes/bunker-fs-battery.sh)
finds every path it names, and so the tree is comparable with BFS-011's
"Fixture A (150 files / 6 dirs)" study fixture where the instrument allows:

    go.mod          the instrument reads $M/go.mod              (section 2)
    src/            120 entries: "ls src (120 entries)"         (section 2)
    src/*.txt       120 x 15 KiB deterministic => 1.80 MiB tree (section 7)
    pkg/deep/a/b/c/ a deep chain so a recursive walk is not flat
    scratch/        an empty collection (a walk must still reveal it)

Content is a sha256 hash chain seeded by the file's own relative path, so the
same tree is reproduced byte-for-byte on every run (BFS-011's own
deterministicBytes rule).
"""
import hashlib
import os
import shutil
import stat
import subprocess
import sys

SRC_FILES = 120
SRC_BYTES = 15 * 1024
DEEP = ["pkg/deep/a/b/c/n0.txt", "pkg/deep/a/b/c/n1.txt", "pkg/deep/a/b/c/n2.txt"]
DEEP_BYTES = 512


def det_bytes(seed: str, n: int) -> bytes:
    out = bytearray()
    counter = 0
    while len(out) < n:
        out += hashlib.sha256(f"{seed}#{counter}".encode()).digest()
        counter += 1
    return bytes(out[:n])


def write(root: str, rel: str, size: int) -> None:
    p = os.path.join(root, rel)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "wb") as fh:
        fh.write(det_bytes(rel, size))


def main() -> int:
    root = sys.argv[1]
    if os.path.exists(root):
        shutil.rmtree(root)
    os.makedirs(root, mode=0o755, exist_ok=True)

    write(root, "go.mod", 0)
    with open(os.path.join(root, "go.mod"), "w") as fh:
        fh.write("module fixture.local/tree\n\ngo 1.22\n")
    with open(os.path.join(root, "README.md"), "w") as fh:
        fh.write("# fixture tree\n\nserved by probes/davserve for the bunker-fs battery.\n")

    for i in range(SRC_FILES):
        write(root, f"src/f{i:03d}.txt", SRC_BYTES)
    for rel in DEEP:
        write(root, rel, DEEP_BYTES)
    os.makedirs(os.path.join(root, "scratch"), exist_ok=True)

    env = dict(os.environ)
    env.update({
        "GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@invalid",
        "GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@invalid",
    })
    def git(*args: str) -> None:
        subprocess.run(["git", "-C", root] + list(args), check=True,
                       stdout=subprocess.DEVNULL, env=env)
    git("init", "-q", "-b", "main")
    git("add", "-A")
    git("commit", "-q", "-m", "fixture: base tree")
    write(root, "src/f000.txt", SRC_BYTES + 64)
    git("add", "-A")
    git("commit", "-q", "-m", "fixture: second commit (so rebase HEAD~1 is real)")
    with open(os.path.join(root, "CHANGELOG.md"), "w") as fh:
        fh.write("## third commit\n")
    git("add", "-A")
    git("commit", "-q", "-m", "fixture: third commit")

    total = 0
    files = 0
    for dirpath, _dirnames, filenames in os.walk(root):
        if "/.git" in dirpath or dirpath.endswith("/.git"):
            continue
        for fn in filenames:
            files += 1
            total += os.path.getsize(os.path.join(dirpath, fn))
    print(f"root={root}")
    print(f"files={files} bytes={total} ({total/1048576:.3f} MiB)")
    print(f"src_entries={len(os.listdir(os.path.join(root, 'src')))}")
    print("git log:")
    print(subprocess.run(["git", "-C", root, "log", "--oneline"],
                         capture_output=True, text=True).stdout.strip())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
```

## `counting_relay.py` — the counting/delaying relay: the independent concurrency witness

```python
#!/usr/bin/env python3
"""Counting, delaying relay in front of the bunker-fs test endpoint.

WHY: BFS-016 must show the in-flight request count the mount ACTUALLY reached,
and the instrument's own counters are unreliable (they read whichever mount last
wrote its status.json). This is an independent, server-side witness:

  * it counts requests and the concurrent in-flight high-water mark, measured at
    the socket, not self-reported by the client;
  * it can add a per-request delay, which is how BFS-011 made the concurrency
    lever visible on a link with sub-millisecond round trips.

It is a probe for one measurement, not a product surface: loopback only, no
root, no host change, and it dies with the measurement.

    python3 counting_relay.py --listen 127.0.0.1:38902 --upstream http://127.0.0.1:38901 --delay-ms 20
    curl -s http://127.0.0.1:38902/__stats     # {"requests":N,"max_in_flight":M,...}
"""
import argparse
import http.client
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

STATS_LOCK = threading.Lock()
STATS = {"requests": 0, "in_flight": 0, "max_in_flight": 0, "reset_at": time.time()}
ARGS = None


def bump(field, delta):
    with STATS_LOCK:
        STATS[field] += delta
        if field == "in_flight" and STATS["in_flight"] > STATS["max_in_flight"]:
            STATS["max_in_flight"] = STATS["in_flight"]


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "bfs016-relay"
    sys_version = ""

    def log_message(self, *a):  # keep stdout clean
        pass

    def _proxy(self):
        if self.path == "/__stats":
            body = json.dumps({**STATS, "uptime_s": round(time.time() - STATS["reset_at"], 3)}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path == "/__reset":
            with STATS_LOCK:
                STATS.update({"requests": 0, "in_flight": 0, "max_in_flight": 0, "reset_at": time.time()})
            body = b'{"ok":true}'
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        bump("requests", 1)
        bump("in_flight", 1)
        try:
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else None
            up = urlsplit(ARGS.upstream)
            conn = http.client.HTTPConnection(up.hostname, up.port or 80, timeout=ARGS.timeout)
            headers = {k: v for k, v in self.headers.items()
                       if k.lower() not in ("host", "connection", "content-length")}
            conn.request(self.command, self.path, body=body, headers=headers)
            resp = conn.getresponse()
            payload = resp.read()
            # the delay is what a real round trip costs: applied while in flight
            if ARGS.delay_ms:
                time.sleep(ARGS.delay_ms / 1000.0)
            self.send_response(resp.status)
            for k, v in resp.getheaders():
                if k.lower() in ("connection", "content-length", "transfer-encoding"):
                    continue
                self.send_header(k, v)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(payload)
            conn.close()
        except Exception as exc:  # a relay that hides failures would be a worse instrument
            try:
                msg = f"relay: {exc}".encode()
                self.send_response(502)
                self.send_header("Content-Length", str(len(msg)))
                self.end_headers()
                self.wfile.write(msg)
            except Exception:
                pass
        finally:
            bump("in_flight", -1)

    do_GET = do_HEAD = do_OPTIONS = do_PROPFIND = do_PUT = do_DELETE = do_MKCOL = do_COPY = do_MOVE = do_POST = _proxy


def main():
    global ARGS
    p = argparse.ArgumentParser()
    p.add_argument("--listen", default="127.0.0.1:38902")
    p.add_argument("--upstream", default="http://127.0.0.1:38901")
    p.add_argument("--delay-ms", type=int, default=0)
    p.add_argument("--timeout", type=float, default=30.0)
    ARGS = p.parse_args()
    host, _, port = ARGS.listen.partition(":")
    srv = ThreadingHTTPServer((host, int(port)), Handler)
    srv.daemon_threads = True
    print(f"relay listening on {ARGS.listen} -> {ARGS.upstream} (delay {ARGS.delay_ms}ms)", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
```

## `file-rows.py` — files the BFS-018 .. BFS-021 board rows (via ~/.hermes/scripts/board_append.py)

```python
#!/usr/bin/env python3
"""File the BFS-016 defect rows on the bunker board (append-only, via board_append.py).

Rows are built here as JSON and passed as argv words, so no shell quoting can
mangle them and no re-serialization can change the bytes written.
"""
import json
import subprocess
import sys

BOARD = "/home/kara/worktrees/bunker-BFS-016/.coding-hermes/board/tasks.jsonl"
APPENDER = "/home/kara/.hermes/scripts/board_append.py"
SRC = "BFS-016-measure"
TS = "2026-09-26T19:35:00+00:00"


def row(rid, title, priority, complexity, tags, reasoning, depends=None):
    r = {
        "id": rid,
        "title": title,
        "status": "pending",
        "priority": priority,
        "complexity": complexity,
        "source": SRC,
        "capability_tags": tags,
        "reasoning": reasoning,
        "created_at": TS,
        "updated_at": TS,
    }
    if depends:
        r["depends_on"] = depends
    return r


rows = []

rows.append(row(
    "BFS-018",
    "BUG: a directory listing through the mount can come back EMPTY (or short) while the directory has "
    "entries - readdir silently returns a wrong answer, and nothing ever re-reads it",
    1, 3, ["+bunker", "+mount", "+data"],
    "FOUND BY BFS-016 (the measurement row) while running the committed battery and then probing the "
    "mount's own listings. REPRODUCED ON A FRESH MOUNT with a native control, on a fixture whose server "
    "side is untouched (docs/evidence/BFS-016-readdir.txt, probe docs/evidence/BFS-016-probes/readdir-degrade.sh): "
    "fresh mount -> `ls -1` root = [CHANGELOG.md README.md go.mod pkg scratch src] (matches the server). "
    "After ONE `mkdir` through the mount -> the root listing becomes [oob.txt] - the six original entries "
    "have DISAPPEARED from the listing while still existing on the server. After a through-mount write + "
    "unlink -> the root listing is EMPTY, `find <mount> -mindepth 1` returns 0 entries against the native "
    "421, `ls -lR` through the mount prints 2 lines against 156 natively - and rc is 0 throughout: it is a "
    "SILENT wrong answer, not an error. `main` was missing from .git/refs/heads through the mount while "
    "the server held it (1 of 3 entries returned). MEANWHILE lookups still work: `stat <mount>/go.mod` = 35, "
    "`cat <mount>/src/f000.txt` = 15424 bytes, and subdirectories mostly list correctly (src 120/120, "
    ".git/objects 110/110) - so it is per-directory corruption of the cached child set, not a dead mount. "
    "CODE SITE (the mechanism, for whoever fixes it): internal/fsmount/fs_linux.go Readdir() returns "
    "`snap.Children(n.p)` whenever `snap.Known(n.p)` is true, and it re-reads the directory ONLY when Known "
    "is false; internal/fsclient/snapshot.go Children() builds its answer by intersecting the cached child "
    "NAME set (`s.children[dir]`) with the node map (`s.nodes`), silently SKIPPING any name whose node is "
    "gone; known/read is a separate flag (`s.read[dir]`). Drop (snapshot.go:371) drops nodes and the "
    "parent's child SET, while the write path's own Create/Put/Unlink/Rmdir and DropReaddir touch those "
    "maps independently - so a directory can be left 'read' (Known=true, never re-read) with a child set "
    "that no longer intersects its nodes, and the listing silently shrinks toward empty. WHY THIS IS THE "
    "WORST SHAPE OF BUG FOR THIS PROJECT: the whole-tree read is the product claim (BFS-008 acceptance 2/5); "
    "a mount that answers `ls -R`, `find` and every readdir-driven walk (git included) with an EMPTY tree "
    "while returning success is worse than one that fails loudly. NOTE the load order matters: the battery's "
    "own rows were taken while the listing was still populated (its transcript shows ls/ls -lR/find results), "
    "so the battery's numbers stand; the breakage appears later in a mount's life, after writes/mkdirs "
    "through the mount. NOT FIXED HERE: BFS-016 is a measurement row - a row that measures and fixes cannot "
    "be audited for either. FIX SHOULD COME WITH: a regression test that mkdirs through the mount, then "
    "asserts the parent listing still contains the pre-existing entries AND the new one, plus the same for "
    "a root listing after an unlink; and the invariant that Children()/Known() cannot disagree.",
    ["BFS-008"],
))

rows.append(row(
    "BFS-019",
    "BUG: a file created through the mount cannot be renamed or looked up until it is published - git's "
    "write-lock-then-rename pattern fails, leaves <ref>.lock on the server, and every later git command "
    "through the mount dies rc=128",
    1, 3, ["+bunker", "+mount", "+integration"],
    "FOUND BY BFS-016: the committed battery got 9/14 git operations through the mount and 4 errors, all "
    "rc=128 with 'fatal: Unable to create <mount>/.git/index.lock: File exists' (docs/evidence/BFS-016-battery.csv, "
    "rows 'checkout -b scratch', 'add one file', 'commit', 'commit --amend'). It is ONE cause, not four, and "
    "it reproduces by hand (docs/evidence/BFS-016-defect-repros.txt): "
    "(1) THE RACE - `printf x > <mount>/f.lock` returns rc=0, and `mv <mount>/f.lock <mount>/f.final` "
    "IMMEDIATELY after close() FAILS with ENOENT (rc=1); with a 2 s settle the same rename returns rc=0. Same "
    "shape inside .git (A3). So a caller that writes a temp file and renames it - which is EXACTLY what git "
    "does for index.lock -> index and <ref>.lock -> <ref> - loses the race and gets ENOENT, while the server "
    "already has the bytes (measured: the mount reports size 0 through getattr in the same window while the "
    "server has the 11 bytes written). The code matches the observation: internal/fsmount/fs_linux.go Create() "
    "'starts the write path: the handle buffers the arriving chunks and publishes them as ONE conditional PUT "
    "at the first publication point (Flush/Fsync/Release)' - and the kernel calls RELEASE after close() "
    "returns, asynchronously, so the caller's next syscall can arrive before the file exists server-side. "
    "(2) THE CASCADE - because the rename failed, git's cleanup left the lock on the SERVED TREE: the battery's "
    "own run left a 10320-byte /tmp/bfs016/tree/.git/index.lock behind (mtime inside the battery window), and "
    "my repro left real .git/refs/heads/repro-br.lock and .git/refs/heads/bfs016-repro.lock (41 B each) on the "
    "server. After that, EVERY lock-taking git command fails rc=128 ('cannot lock ref ... Unable to create "
    "<mount>/.git/refs/heads/X.lock: File exists. Another git process seems to be running') even though no "
    "process holds it - reproduced twice, and the failure survives removing the lock and settling, because the "
    "next attempt recreates the same broken sequence. IMPACT: the remote-editing use case is git over the mount; "
    "git's atomic-write pattern is not optional, so this is a hard blocker for the feature, and it is the "
    "difference between the sshfs-style '7-8 stalls' baseline and a working mount. NOT FIXED HERE (measurement "
    "row). FIX SHOULD COME WITH: a regression test that writes a file through the mount and renames it in the "
    "same process without any sleep, asserting rc=0; a test that runs git (init/add/commit/checkout -b) "
    "entirely through the mount and asserts rc=0 with no *.lock left in the served tree; and a decision about "
    "what a caller can rely on after close() returns (either publish synchronously or document + surface the "
    "window).",
    ["BFS-008"],
))

rows.append(row(
    "BFS-020",
    "BUG: appending through the mount fails and the bytes are LOST - '>>' returns 'I/O error' / 'Operation "
    "not supported' and the server content is unchanged (POSIX append is not implemented, and the failure "
    "is not reported as a short write)",
    1, 2, ["+bunker", "+mount"],
    "FOUND BY BFS-016 in the committed battery: ops row 'append (>>)' rc=1 class error, note "
    "'sh: 1: printf: printf: I/O error' (docs/evidence/BFS-016-battery.csv), and the very next op "
    "'read appended' returns ONLY the original line - the appended bytes are gone. REPRODUCED "
    "(docs/evidence/BFS-016-defect-repros.txt section B) on a settled file: B1 `sh -c \"printf 'second line\\n' "
    ">> file\"` -> rc=1, stderr 'printf: I/O error', server content unchanged ('payload2|'); B2 the explicit "
    "form `exec 3>>file; printf ... >&3` -> rc=0 but the data is STILL not on the server (silently dropped, "
    "which is worse than B1's rc=1); B3 the control - a full-file rewrite through the mount ('printf > file') "
    "-> rc=0 and the server has the new content. So the write path handles create/truncate-write but not "
    "O_APPEND: the early battery run reported 'Operation not supported' (EOPNOTSUPP) on the same op, the "
    "repro reports the shell's I/O-error shape. IMPACT: any tool that appends (logs, >> redirects, "
    "incremental writers, sed -i) either errors or silently loses data through the mount - and git's "
    "config/lock handling uses append in places. NOT FIXED HERE (measurement row). FIX SHOULD COME WITH: "
    "either O_APPEND support (write at the server's current size under the same conditional-PUT discipline) "
    "or a NAMED refusal (EOPNOTSUPP with a message the CLI can surface) - never a silent drop; plus a "
    "regression test that creates, appends, and reads back through the mount asserting the appended bytes "
    "are on the SERVER, and a test that the failing mode is reported as an error by close().",
    ["BFS-008"],
))

rows.append(row(
    "BFS-021",
    "INSTRUMENT: the committed battery's per-op wall times carry a +110 ms constant that is `timeout` "
    "itself, and its requests/in_flight_max columns are read from the wrong mount - BFS-008 acceptance 2/5 "
    "cannot be answered from this instrument as written",
    2, 2, ["+bunker", "+tests"],
    "FOUND BY BFS-016 while capturing the numbers the row exists for. TWO INDEPENDENT DEFECTS, both measured. "
    "(a) THE +110 ms CONSTANT: every `run_one` cell in the committed battery reports 0.109-0.119 s regardless "
    "of the operation - 13 data ops, 13 git ops, 5 kill ops, all within 10 ms of each other - while the SAME "
    "operations measured directly through the same mount take 3-10 ms. Isolated (docs/evidence/BFS-016-instrument-overhead.txt): "
    "the battery's expression `out=\"$(timeout $OP_TIMEOUT $@ 2>&1)\"` = 0.109-0.112 s; `out=\"$(cat <mount>/go.mod)\"` "
    "(substitution, no timeout) = 0.010 s; `timeout 45 cat <mount>/go.mod` (timeout, no substitution) = 0.109 s; "
    "`timeout 45 true` = 0.110 s; `timeout 45 cat /etc/hostname` (LOCAL file, no mount at all) = 0.110 s. So the "
    "constant is the `timeout` wrapper on this host (~110 ms per invocation), NOT the mount, and every per-op "
    "number in the battery is mount_cost + 110 ms. The three `tree-read` cells do not use run_one and are clean "
    "(0.013 / 0.042 / 0.051 s), which is why they look like the only real numbers in the table. It also explains "
    "the failed BFS-008 run's identical 0.109-0.111 s signature: same wrapper, same host. "
    "(b) THE COUNTERS READ THE WRONG MOUNT: `status_json()` picks `ls -td $HOME/.cache/bunker/fs/* | head -1` "
    "(newest directory mtime), but a mount's cache dir is keyed by MountID(URL) - so ALL mounts of one URL SHARE "
    "one cache dir and overwrite one status.json, and the newest dir is whichever mount last had an entry change, "
    "not the mount being measured. Result in the run: section 3's three tree-read arms all print 'requests +0' "
    "while the CSV's requests column jumps 5 -> 12 -> 19 -> 4 between unrelated ops, and in_flight_max reads 1 or "
    "2 in every row - including arms where the client really did have 8 in flight (measured independently: "
    "docs/evidence/BFS-016-concurrency.txt). WHY THIS IS A ROW AND NOT A FOOTNOTE: BFS-008's acceptance criteria "
    "2 and 5 are 'the measured per-op comparison' and 'the demonstrated in-flight count' - an instrument whose "
    "wall times are dominated by its own harness and whose counters come from another mount cannot answer either, "
    "and a future reader would believe the table. BFS-016's own report states which numbers are clean and which "
    "are not, and takes the concurrency figure from an independent server-side witness instead. NOT FIXED HERE "
    "(measurement row). FIX SHOULD COME WITH: a calibration cell (`timeout $OP_TIMEOUT true`) printed once per run "
    "so the constant is visible, or measuring inline without the wrapper; counters scoped to the mount under test "
    "(the mountpoint is in each status.json - match it, or take --mount/--cache-dir explicitly); and a test that "
    "two mounts of the same URL do not share counters.",
    ["BFS-008", "BFS-016"],
))

plan = {"board": BOARD, "appended": []}
result = subprocess.run([sys.executable, APPENDER, BOARD] + [json.dumps(r) for r in rows],
                        capture_output=True, text=True)
print("appender stdout:", result.stdout.strip())
print("appender stderr:", result.stderr.strip())
print("appender rc:", result.returncode)
plan["appended"] = [r["id"] for r in rows]
json.dump(plan, open("/tmp/bfs016/rows-filed.json", "w"), indent=2)
sys.exit(result.returncode)
```

## `file-row-022.py` — files the BFS-022 board row

```python
#!/usr/bin/env python3
"""File BFS-022 (the compounding op deadline) on the bunker board."""
import json
import subprocess
import sys

BOARD = "/home/kara/worktrees/bunker-BFS-016/.coding-hermes/board/tasks.jsonl"
APPENDER = "/home/kara/.hermes/scripts/board_append.py"

row = {
    "id": "BFS-022",
    "title": "SPEC: the op deadline compounds per internal request - a cold directory listing against a "
             "frozen server takes 60.1 s against a documented 30 s 'OpTimeout bounds any in-flight "
             "operation'",
    "status": "pending",
    "priority": 2,
    "complexity": 2,
    "source": "BFS-016-measure",
    "capability_tags": ["+bunker", "+mount"],
    "reasoning": "FOUND BY BFS-016's not-hang probe (criterion 8), arm C2: a mount whose server FREEZES "
                 "mid-flight (SIGSTOP on the endpoint, so it accepts and answers nothing), with "
                 "--no-snapshot --no-cache so no local answer can hide a wait, and a BOUNDED `timeout` "
                 "around every op. MEASURED, same frozen mount, cold paths only (docs/evidence/BFS-016-nothang-armC2.txt): "
                 "`stat src/f042.txt` (never touched) 30.041 s, ENOTCONN; `cat src/f042.txt` (never touched) "
                 "30.044 s, ENOTCONN; `ls src` (a COLD DIRECTORY LISTING) **60.095 s**, rc=2, ENOTCONN "
                 "('unknown io error', code 107); `stat .` (the root, answered locally) 0.110 s ok. "
                 "So one user-visible operation paid TWO 30 s deadlines: the deadline is per internal "
                 "request, not per operation. WHY THIS IS A ROW AND NOT A FOOTNOTE: the client documents "
                 "the number as bounding the operation - internal/fsclient/errors.go:225-226 declares "
                 "DefaultOpTimeout = 30 s with 'bounds any in-flight operation (AC-6's number)' - and the "
                 "PRD's not-hang criterion is about the caller's experience of a stalled server. A caller "
                 "that meets a stalled mount waits 2x the documented bound on a directory listing, and the "
                 "multiple is a function of how many internally-sequenced requests the operation makes "
                 "(unknown for deeper paths). The MOUNT DOES NOT HANG (no rc=124 anywhere in the run, the "
                 "mount stayed mounted, recovery after SIGCONT was 0.109 s) - this row is about the "
                 "STATED bound not being the ACTUAL bound. NOT FIXED HERE (BFS-016 measures). FIX SHOULD "
                 "COME WITH: either one deadline per user-visible operation (carry a context deadline "
                 "across the internal requests an operation makes) or a documented, per-operation bound "
                 "with a test that a cold directory listing against an unreachable endpoint returns inside "
                 "it; plus a battery cell that asserts the observed bound, since a stall in this path is "
                 "the one the release treats as the hard constraint.",
    "created_at": "2026-09-26T19:40:00+00:00",
    "updated_at": "2026-09-26T19:40:00+00:00",
    "depends_on": ["BFS-008", "BFS-016"],
}

result = subprocess.run([sys.executable, APPENDER, BOARD, json.dumps(row)],
                        capture_output=True, text=True)
print("appender stdout:", result.stdout.strip())
print("appender stderr:", result.stderr.strip())
print("appender rc:", result.returncode)
sys.exit(result.returncode)
```
