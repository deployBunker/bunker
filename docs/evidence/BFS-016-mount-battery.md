# BFS-016 — the bunker-fs mount battery, measured (BFS-008 acceptance 2, 5, 8)

**Row:** BFS-016 (P1, complexity 3) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-26
**Status:** complete with named residuals — the battery ran, the numbers are below, and four defects it
exposed are FILED (BFS-018 … BFS-021) rather than fixed, because this row measures.
**Product code changed:** **none.** This row adds evidence and two probes; it changes no Go file.

**What this row owns, in its own words.** BFS-008's implementation merged and verified, but it closed
PARTIAL: (2) the per-op comparison against the sshfs numbers, (5) the demonstrated in-flight request
count, and (8) the not-hang proof were never captured — the previous worker ran the battery, then hung
in cleanup (`pkill -f 'bunker-fs-battery'` matched its own shell) and no report came back. The
instrument is committed, so the numbers are taken from the SAME instrument rather than a rival one.

---

## 0. Verdict in one screen

| acceptance criterion | answer | where |
|---|---|---|
| **2 — per-op table through the mount, concurrency > 1** | **35/35 cells, 0 stalls, ~17 s total** — but the table is NOT clean: 6 cells failed (4 git rc=128, `append (>>)` rc=1, the expected write-against-dead-endpoint rc=2), and every wall time carries a **+110 ms constant that is `timeout` itself**, not the mount (§2, §6-BFS-021) | §2 |
| **5 — concurrency demonstrated, not asserted** | **YES, measured at the socket: 8 requests in flight** for 8 parallel cold reads at `--concurrency 8` (1 at `--concurrency 1`), **3.6× faster** (0.584 s → 0.162 s) on a 20 ms link. **BUT a serial whole-tree walk does not drive it**: `find` over 127 dirs reaches **1** in flight at c=1 and **2** at c=8, and is **1.03× faster** (8.208 s → 8.118 s). The whole-tree read's real win is the **snapshot op: 0.035 s and ZERO requests during the walk** (1 call, 421 nodes) | §3 |
| **8 — not-hang proof** | **NO HANG.** A dead endpoint: named refusal in **0.109 s** (`ENOTCONN`, `unreachable_connect`). A *stalled* server (accepts, answers nothing): the bind refusal is bounded at the declared **BindTimeout, 5.113 s**; a mounted client whose server freezes mid-flight fails every op with `ENOTCONN` and a named cause, **stays mounted**, and recovers in 0.109 s once the server answers. **The residual: the bound is 30 s per internal request, so a cold directory listing takes 60.1 s — and paths already read keep answering from local state while the server is silent** | §4 |

**The two headline findings this row exists to surface:**

1. **A whole-tree read through the mount can silently return an EMPTY listing** while the tree has 421
   entries and lookups still succeed — reproduced on a fresh mount with a native control (§6, BFS-018).
   The battery's own numbers were taken while the listing was populated (its transcript shows the
   listings and the walks), so the table below stands; the breakage appears later in a mount's life.
2. **Concurrency is real and reachable but the SERIAL walker does not use it** — the lever the release
   rests on (BFS-011) is not the lever that makes the whole-tree read fast; the **snapshot** is (§3).
   Stated as a finding, which is what the row asked for, not smoothed into a footnote.

---

## 1. What was measured, on what, and on what config

**The instrument** — `probes/bunker-fs-battery.sh` from the merged tree, unmodified:
sha256 `e87c56d4a2bde807f36b0f7dd20e2e214a9fea3b3f9d0d70ff219c1703fec4d2` — byte-identical to the copy at
`37920ec` (= main) because the worktree was **clean at start of session** (`git status --short` empty)
and `37920ec` is the only commit it contains. It ran with `--concurrency 25 --timeout 45` and a
`--stop-endpoint` that kills the endpoint by explicit PID (`kill $(cat srv.pid)`) — never `pkill -f`.

**Shared fixture.** Deterministic (a sha256 hash chain seeded by each file's own path, so a re-run
reproduces the tree byte-for-byte) at `/tmp/bfs016/tree`, built by the probe
`BFS-016-probes/mkfixture.py`:

| property | value |
|---|---|
| working-tree files / bytes | 126 files, 1 844 920 B (**1.759 MiB**) |
| `src/` | exactly **120 entries**, 15 KiB each — the instrument's own `ls src (120 entries)` |
| deeper structure | `pkg/deep/a/b/c/*.txt`, an empty `scratch/` |
| whole tree incl. `.git` | 421 entries, 127 directories |
| git state | own repo, **3 commits**, branch `main` (so `rebase HEAD~1` is a real operation), clean |

**The endpoint** — `probes/davserve` (the repo's own client-facing endpoint: the same
`internal/server/webdav` handler bunkerd mounts, with no registry and no agent lifecycle, so it cannot
destroy live agents the way a locally-started `bunkerd` with an empty registry does) on loopback, plain
**HTTP/1.1**, no TLS, no h2c. Nothing about the client required anything else (§1.4).

**Client under test** — `go build ./cmd/bunker` from the same tree, `/tmp/bfs016/bin/bunker`.
**Host** — 16 cores, loadavg 9.79 at battery start and 11.61 at end (sibling workers were running; every
number below is therefore a wall time under load, which inflates rather than flatters).

**The run** — 2026-09-26T19:21:19-05:00, exit 0, whole battery in **~17 s**, 35 measurement cells.

### 1.1 `bunker-fs` is OPT-IN and did NOT become the default — CONFIRMED

* `internal/mountdriver/mountdriver.go:25` — `const DefaultDriver = "sshfs"`; the constant is what
  `internal/cli/mount.go:206,323,329` falls back to when a request names no driver.
* `internal/mountdriver/bunkerfs.go:1-18` — the driver registers itself as `DriverBunkerFS = "bunkerfs"`,
  and its own doc says "NEVER DEFAULT … nothing in this file touches DefaultDriver".
* The CLI's own text (live): `bunker fs mount --help` → *"It is NOT the default and does not become one
  until a battery on both DCs shows zero stalls."*
* A mount is reached only by asking for it: `bunker fs mount <mp> --url …` (or `--driver bunkerfs`).

### 1.2 The mountpoint is PRIVATE 0700 — CONFIRMED (with the reading trap named)

`PrepareMountpoint` (`internal/fsmount/options.go:168-190`) creates the directory 0700 and TIGHTENS an
existing wider mode. MEASURED: a fresh mount is left at **mode 700** after unmount.

**The trap, because a careless check reports a false failure:** *while the mount is up*, `stat` on the
mountpoint goes THROUGH the FUSE filesystem and reports the **server's** root directory mode (755 here) —
the local private mode is only observable once the mount is detached. Verified the only way it can be:
`stat` before mount (0700) / through the mount (755, the server's) / after `fusermount -u` (700, the
local dir).

### 1.3 `allow_other` is STRIPPED, never honoured — CONFIRMED live

* `/proc/mounts` for a live mount: `bunker-fs /tmp/bfs016/mnt-r1 fuse.bunker-fs
  rw,nosuid,nodev,relatime,user_id=1000,group_id=1000,max_read=1048576 0 0` — **no `allow_other`**.
* `internal/fsmount/fs_linux.go:232` hardcodes `AllowOther: false` with the comment "STRIPPED: always
  private, whatever was requested"; the flag is accepted and the CLI prints
  `AllowedOtherStripped()` (`options.go:204-208`) rather than refusing.

### 1.4 No new server-side requirement — CONFIRMED

The battery's own preflight against the merged client and the davserve endpoint:

```
proto           : HTTP/1.1 (HTTP/1.1 is supported; nothing here requires h2/h3)
extensions      : identity,if_match_refuse,rev,tree,op,watch
degradation     : watch (scope=target mode=poll) no inotify watcher on this target; poll with HEAD/ETag
                  or an X-Bunker-Op: snapshot diff
bind preflight  : OK in 2ms
```

Every `X-Bunker-*` extension is optional and has a standard fallback, and the client **declares** the
degradation instead of demanding the capability. The fallback is exercised in the same run: the
`--no-snapshot` arms took the **PROPFIND Depth: 1** path and worked end to end (§2, §3). No server
setting, config file or daemon was modified for any measurement here.

---

## 2. Acceptance 2 — the per-op table through the mount, at concurrency 25

Raw transcript: `BFS-016-battery-output.txt`; machine-readable rows: `BFS-016-battery.csv`.
Below is the CSV verbatim in the instrument's own column order, with the instrument's own class column.

```
section,op,elapsed_s,rc,class
ops,cat go.mod (read),0.110,0,ok
ops,ls -l (list+stat),0.109,0,ok
ops,ls src (120 entries),0.110,0,ok
ops,stat go.mod,0.110,0,ok
ops,mkdir newdir,0.110,0,ok
ops,write file (printf>),0.111,0,ok
ops,read it back,0.114,0,ok
ops,mv (rename),0.110,0,ok
ops,ls newdir (after rename),0.112,0,ok
ops,append (>>),0.110,1,error(rc=1)          <-- FAILED, bytes lost (BFS-020)
ops,read appended,0.110,0,ok
ops,rm (unlink),0.111,0,ok
ops,rmdir,0.115,0,ok
tree-read,snapshot-op ls -lR,0.013,0,ok
tree-read,propfind-walk ls -lR c=25,0.042,0,ok
tree-read,propfind-walk ls -lR c=1,0.051,0,ok
git,rev-parse HEAD,0.116,0,ok
git,log -1,0.119,0,ok
git,log --oneline -20,0.116,0,ok
git,ls-files (count),0.113,0,ok
git,diff --stat HEAD,0.916,0,ok
git,status --short,0.216,0,ok
git,status --porcelain -uno,0.214,0,ok
git,checkout -b scratch,0.113,128,error(rc=128)   <-- FAILED (BFS-019)
git,add one file,0.115,128,error(rc=128)          <-- FAILED (BFS-019)
git,commit,0.112,128,error(rc=128)                <-- FAILED (BFS-019)
git,commit --amend,0.110,128,error(rc=128)        <-- FAILED (BFS-019)
git,rebase HEAD~1,1.317,0,ok
git,symbolic-ref / branch,0.114,0,ok
bound,read 40 files (bound exceeded),0.211,0,ok
kill,stat . (endpoint up),0.122,0,ok
kill,stat . (endpoint DOWN),0.111,0,ok
kill,cat go.mod (endpoint DOWN),0.110,0,ok
kill,ls (endpoint DOWN),0.111,0,ok
kill,write (endpoint DOWN),0.110,2,error(rc=2)    <-- EXPECTED: the bounded transport error
```

**How that reads against the recorded baselines** (the comparison the row asks for):

| | sshfs (recorded) | WebDAV (recorded) | **bunker-fs (this run)** |
|---|---|---|---|
| 14-op shape | 5 ok / **7-8 stalls** | 11 ok / 1 stall | **13/14 cells rc=0, 0 stalls** — the one non-zero is `append (>>)` |
| whole-tree op | — | — | **0.013 s** (snapshot) / 0.042 s (PROPFIND c=25) / 0.051 s (c=1) — against **NFS 33-36 s** and native **0.41 s** |

**Three caveats that belong WITH the table, not after it** (all measured, all filed):

1. **Every `run_one` wall time carries a +110 ms constant that is `timeout` itself** — `timeout 45 true`
   measures 0.110 s on this host, and `timeout 45 cat /etc/hostname` (a LOCAL file, no mount in the
   path) also measures 0.110 s. The same operations measured directly through the same mount take
   3-10 ms. So the 32 `run_one` cells are `mount_cost + 110 ms`, and only the three `tree-read` cells
   (measured inline, no wrapper) are clean. **The battery's `0.013 s` whole-tree read is the real
   number; `0.110 s` for `cat go.mod` is not.** Isolated in `BFS-016-instrument-overhead.txt`.
2. **The instrument's `requests` / `in_flight_max` columns are read from the wrong mount**
   (`status_json()` picks `ls -td $HOME/.cache/bunker/fs/* | head -1`, but all mounts of one URL share
   one cache dir keyed by `MountID(URL)`). In this run section 3's three arms all printed
   `requests +0`, and the CSV's `requests` column jumps 5 → 12 → 19 → 4 between unrelated rows while
   `in_flight_max` reads 1-2 everywhere. **§3 answers criterion 5 from an independent witness instead.**
3. **The summary's `13/13 ok` counts non-stalls, not successes**: the per-row `class` column is honest
   (`error(rc=128)` ×4, `error(rc=1)`), but the per-section line counts any non-STALL row as ok. A
   reader who takes `[git] -> 13/13 ok` at face value would miss four real failures.

**Stalls, stated as stalls:** there were **none** — 0 of 35 cells hit the instrument's 45 s per-op
deadline. The failures above are errors, not stalls, and their wall times are the +110 ms constant.
What the instrument CANNOT see is a stall in a slow path it does not time per-op; §4 tests the stall
case directly, with a frozen server, and reports a hard 30.0 s bound per operation with no hang.

---

## 3. Acceptance 5 — CONCURRENCY, demonstrated with a server-side witness

**Why the witness.** The mounted client's own counters are the ones §2.2 shows are read from another
mount's `status.json`, and a self-reported in-flight count is an assertion, not a demonstration. So the
measurement runs through `BFS-016-probes/counting_relay.py` — a loopback counting relay in front of the
endpoint that counts **requests** and the **concurrent in-flight high-water mark at the socket**, and can
inject a per-request delay (the technique BFS-011 used to make the lever visible on a link whose round
trip is otherwise sub-millisecond; here **20 ms**). Fixture: the same 421-entry / 127-directory tree.

| arm | workload | concurrency | wall | requests | **max in flight (relay)** | client's own report |
|---|---|---|---|---|---|---|
| 1 | `find` whole tree, `--no-snapshot` | **1** | **8.208 s** | 132 | **1** | 1 |
| 2 | `find` whole tree, `--no-snapshot` | **8** | **8.118 s** | 132 | **2** | 2 |
| 3 | 8 parallel cold `cat` (15 KiB, `--no-cache`) | **1** | **0.584 s** | 9 | **1** | 1 |
| 4 | 8 parallel cold `cat` (15 KiB, `--no-cache`) | **8** | **0.162 s** | 17 | **8** | 1 |
| 5 | `find` whole tree, **snapshot on** | 8 | **0.035 s** | **0** | 0 | 1 |

**What this demonstrates, and what it refutes.**

1. **Concurrency is reachable through the mount, and it is the client's knob.** Eight independent
   readers put **8 requests in flight** at `--concurrency 8` — measured at the socket, not claimed —
   and the same eight readers at `--concurrency 1` reach **1** and take **3.6× longer** (0.584 s →
   0.162 s). That is the criterion's "demonstrated, not asserted", on the same fixture, the same
   endpoint and the same client build as §2.
2. **The serial whole-tree walk does NOT drive concurrency — this is the finding the row anticipated.**
   `find` over 127 directories issues 132 requests either way, but raising the knob from 1 to 8 moves
   the in-flight high-water mark only from **1 to 2** and the wall time only from **8.208 s to
   8.118 s (1.03×)**. The client does not prefetch or pipeline per-directory PROPFINDs for a walker
   that asks for one directory at a time; with the snapshot disabled there is nothing to prefetch with.
   In BFS-011's terms: the *carrier* is present (HTTP/1.1 with 8 connections allowed) and the *cause*
   is present (independent concurrent requests do overlap — arm 4), but a serial walker never produces
   the concurrent requests, so the win does not appear. **Concurrency helps a fan-out, not a walk.**
3. **The whole-tree read's win is the SNAPSHOT, and it is structural.** The same `find`, with the
   snapshot on, finishes in **0.035 s** — **232× faster than the c=8 no-snapshot arm** — and issues
   **zero requests during the walk**, because the whole tree arrived in **one** `X-Bunker-Op: snapshot`
   call at mount time (**421 nodes in 1 call**). This is also why the battery's `tree-read` row reads
   0.013 s against the `propfind-walk` rows' 0.042 / 0.051 s on a sub-millisecond link: it is one call
   versus 127, not 25 streams versus 1.

**So, for the release claim:** the mount DOES reach concurrency when the workload is concurrent (8 in
flight, 3.6×), and the whole-tree read is fast for a different, stronger reason (one call). A release
note that attributes the whole-tree win to concurrency would be wrong on this evidence; BFS-011's
conclusion (concurrency is the cause, the version only the carrier) is not contradicted — it is
**scoped**: it holds for fan-out, and the walk is served by the snapshot path instead.

---

## 4. Acceptance 8 — the NOT-HANG proof

The criterion is the PRD's hard constraint: point the client at a server that is stalled or unreachable
and get a **bounded** error; the mount must not hang. Three arms; every op wrapped in `timeout`, so
`rc=124` would have been reported as a STALL. Raw: `BFS-016-nothang.txt`.

**Arm A — nothing listening (`connection refused`), no mount involved**

| op | wall | rc | errno / cause |
|---|---|---|---|
| `fs probe` on a dead port | **0.110 s** | 1 | `errno=ENOTCONN cause=unreachable_connect` — `dial tcp 127.0.0.1:38999: connect: connection refused` |
| `fs snapshot` on a dead port | 0.110 s | 1 | same, `Post … connection refused` |
| `fs mount` on a dead port | 0.109 s | 1 | `bind refused (ENOTCONN, cause unreachable_connect)` |

**Nothing was left at the refused mountpoint** (0 entries) — the "silent empty tree" failure mode the
durability spec records did not occur.

**Arm B — a STALLED listener (accepts TCP, never answers; the relay is SIGSTOPped)**

| op | wall | rc | errno / cause |
|---|---|---|---|
| `fs probe` against the stalled endpoint | **5.113 s** | 1 | `errno=ENOTCONN cause=unreachable_deadline` — `context deadline exceeded` |
| `fs mount` against the stalled endpoint | **5.116 s** | 1 | `bind refused (ENOTCONN, cause unreachable_deadline)` |

5.1 s is the client's declared **BindTimeout (5 s)**: the mount's bind preflight is bounded, and a
stalled server does not hang the mount.

**Arm C — mounted and healthy, then the server FREEZES mid-flight** (`SIGSTOP`; `--no-snapshot
--no-cache`, so no local answer can hide a hang)

| op | wall | rc | result |
|---|---|---|---|
| control: `cat go.mod` (server answering) | 0.108 s | 0 | `module fixture.local/tree` |
| control: `stat go.mod` | 0.109 s | 0 | `35` |
| `stat go.mod` (frozen) | 0.109 s | 0 | **35 — answered from local state while the server was silent** |
| `cat go.mod` (frozen) | 0.109 s | 0 | **content returned from local state** |
| `ls` (frozen) | **30.038 s** | 1 | `ENOTCONN: Transport endpoint is not connected` |
| `write` (frozen) | **30.036 s** | 2 | `ENOTCONN: Transport endpoint is not connected` |
| `stat src/f042.txt` — **never touched by this mount** (frozen) | **30.041 s** | 1 | `ENOTCONN: Transport endpoint is not connected` |
| `cat src/f042.txt` — never touched (frozen) | **30.044 s** | 1 | `ENOTCONN` |
| `ls src` — a COLD directory listing (frozen) | **60.095 s** | 2 | `ENOTCONN` (`unknown io error`, code 107) — **two internal requests, two deadlines** |
| `stat .` — the ROOT, after two timeouts (frozen) | 0.110 s | 0 | `ok` — the root is always in the tree |
| **recovery**: `cat go.mod` after `SIGCONT` | 0.109 s | 0 | content returned — **no remount needed** |

* **The mount stayed mounted throughout** — present in `/proc/mounts` with the same options, not a
  phantom and not silently unmounted, and the client re-binds the same tree afterwards.
* **30.0 s is the declared `OpTimeout`** (`internal/fsclient/errors.go:226`), and it is what bounds a
  file operation — to the millisecond, three separate ops.
* **The bound COMPOUNDS: it is per internal request, not per operation.** A cold directory listing
  (`ls src`, which must issue a PROPFIND and then read) took **60.095 s** — two requests, two 30 s
  deadlines — while the client's documentation says the deadline "bounds any in-flight operation"
  (`errors.go:225`). The mount never hangs; it does take **2× the declared bound** for one user-visible
  operation. Filed as **BFS-022**.
* **The bound is real but it is only "fast" when the failure is a refusal (0.11 s at arm A) rather than
  a stall.** The PRD's phrase "fails loudly and fast" is met for a dead server; against a *stalled* one
  the honest numbers are 30 s per internal request (60 s for a directory listing), with a named errno
  and no hang.
* **A stall is not uniformly visible:** the root and paths already touched in the mount kept answering
  from local state (0.109-0.110 s, correct bytes) while the server answered nothing, because
  publication/lookup had already happened. A never-touched path paid the full 30 s. That is a staleness
  property to state in the release notes rather than a hang.

**Verdict: no hang, in any arm.** No `rc=124`, no unbounded wait, no stuck unmount (every unmount
returned in ≪1 s), and the one stall-shaped case is bounded by the client's own declared deadline.

---

## 5. Stalls: none found, and how a wrong "0 stalls" could still be read

* The instrument's stall definition is a cell hitting the per-op deadline (`rc=124`). **0 of 35 cells**
  did. The four git failures are errors returned in 0.11 s, not stalls.
* The instrument cannot see a stall in a path it does not time per-op (e.g. the 30 s a frozen server
  costs an op) — §4 measures that directly. **The worst case measured anywhere in this row is 30.0 s
  per operation against a frozen server, bounded and named**, against the NFS baseline of 33-36 s for a
  single whole-tree op and sshfs's 7-8 stalls out of 14.
* Reproducible stalls WERE found in the instrument itself, not in the mount: the previous run's
  `pkill -f` self-kill (§0 of the row) and, in this run, **`timeout` costing 110 ms per invocation** —
  a fixed cost per measurement cell that a reader could mistake for mount latency.

---

## 6. Defects found — FILED, not fixed (a row that measures and fixes can be audited for neither)

| row | one line | evidence |
|---|---|---|
| **BFS-018** | A directory listing through the mount can come back **EMPTY or short while the directory has entries**, silently (rc=0), and nothing ever re-reads it — `ls -R`, `find` and every readdir-driven walk (git) see an empty tree | `BFS-016-readdir.txt`, probe `BFS-016-probes/readdir-degrade.sh`: fresh mount root = 6 entries; after one `mkdir` through the mount the root lists `[oob.txt]`; after a write+unlink it lists `[]` while the native tree has 421 entries and `stat`/`cat` of the same paths still succeed. Code sites: `internal/fsmount/fs_linux.go` `Readdir`, `internal/fsclient/snapshot.go` `Children`/`Known`/`Drop` |
| **BFS-019** | A file created through the mount **cannot be renamed until it is published** (immediate `mv` → ENOENT, same `mv` after 2 s → rc=0), so git's `write .lock` → `rename` pattern fails, **leaves `<ref>.lock`/`index.lock` on the served tree**, and every later lock-taking git command dies rc=128 | `BFS-016-defect-repros.txt` (A1/A2/A3, D1/D2/D3) + the battery's 4 rc=128 rows + a 10320-byte `.git/index.lock` left in the served tree by the run |
| **BFS-020** | **Appending through the mount fails and loses the bytes** — `>>` returns `Operation not supported` / `I/O error`, the server content is unchanged, and the explicit `exec 3>>` form returns rc=0 while still dropping the data | battery row `ops,append (>>)` + `BFS-016-defect-repros.txt` section B (B1/B2 fail, B3 control passes) |
| **BFS-021** | The committed battery's per-op wall times carry a **+110 ms constant from `timeout` itself**, and its `requests`/`in_flight_max` columns are read **from whichever mount wrote status.json last** — BFS-008 acceptance 2/5 cannot be answered from the instrument as written | `BFS-016-instrument-overhead.txt` (`timeout 45 true` = 0.110 s, local file = 0.110 s, direct op = 0.010 s) + §2.2 + §2.3 |
| **BFS-022** | The op deadline **compounds**: a cold directory listing against a frozen server took **60.095 s**, two internal requests × the declared 30 s `OpTimeout`, while the client documents the deadline as bounding "any in-flight operation" | `BFS-016-nothang-armC2.txt`: `ls src` 60.095 s `ENOTCONN`, vs 30.041 s for a cold file `stat` and 30.044 s for a cold `cat` on the same frozen mount |

**What I did NOT touch:** no product file, no probe that shipped with the client, no server/daemon
setting. `/tmp` scratch only, plus `docs/evidence/` in the worktree, plus five board rows (BFS-018 …
BFS-022) filed through the repo's `board_append.py`.

---

## 7. REMAINING — what this row could not demonstrate, by name

1. **A fully green 14-op git battery through the mount.** The four git failures are attributed to one
   reproduced cause (BFS-019) with a hand reproduction, but the "all 14 green after the fix" state was
   never observed on this tree, because BFS-016 does not fix it. Do not read `13/14 ok` as "git works
   over the mount": `checkout -b`, `add`, `commit`, `amend` all failed.
2. **A whole-tree read measured after the readdir defect bites (BFS-018).** The §2/§3 walk numbers were
   taken while listings were populated — the transcripts show the correct listings and 420/421 entries
   — but the same mount later returned an empty root listing. A release claim for acceptance 2 needs a
   re-measure after BFS-018 is fixed, with the mkdir-then-walk sequence included.
3. **Both DCs.** This battery ran on loopback only. The release rule ("sshfs stays default until a
   battery on BOTH DCs shows zero stalls") is therefore not satisfied by this row, and §1.1 confirms the
   opt-in default is respecting it.
4. **"Fails loudly and FAST" for a stall.** Demonstrated for a refused connection (0.109 s). Against a
   frozen-but-listening server the bound is the client's declared 30 s per op. If the PRD's "fast" is
   meant to be better than 30 s, that is a design question this row cannot answer; if 30 s is the
   intent, the criterion is met.
5. **Ordering/consistency between the local cache and a stalled transport** beyond the single
   observation in §4 (already-touched paths answer locally; never-touched paths time out). No
   systematic matrix of "which paths answer stale while the server is silent" was run.
6. **Attribution of the 30 s cost to a single mechanism** (no circuit breaker vs a per-request deadline).
   The measurement shows two consecutive ops each paying the full 30 s; I did not instrument the client
   to prove why.

---

## 8. Reproducing this row

Everything needed is committed beside this report:

```
docs/evidence/BFS-016-probes-python.md          mkfixture.py + counting_relay.py (shipped as TEXT: this
                                                repo gitignores *.py globally, .gitignore:37)
docs/evidence/BFS-016-probes/repro-defects.sh     the write→rename race + git lock-cascade repros
docs/evidence/BFS-016-probes/readdir-check.sh     readdir vs native, per directory
docs/evidence/BFS-016-probes/readdir-degrade.sh   the empty-listing reproduction
docs/evidence/BFS-016-probes/settle-sweep.sh      the write→rename window sweep
docs/evidence/BFS-016-probes/concurrency-probe.sh the five concurrency arms
docs/evidence/BFS-016-probes/nothang-probe.sh     arms A and B (dead / stalled at bind)
docs/evidence/BFS-016-probes/nothang-armC.sh      arm C (freeze mid-flight)
docs/evidence/BFS-016-probes/nothang-armC2.sh     cold reads against a frozen server
docs/evidence/BFS-016-probes/overhead-check.sh    the +110 ms `timeout` isolation
```

Shape of a re-run: `mkfixture.py /tmp/tree` → `go build -o /tmp/bin/{bunker,davserve} ./cmd/bunker
./probes/davserve` → `davserve --root /tmp/tree --addr 127.0.0.1:PORT` → `bash
probes/bunker-fs-battery.sh --url http://127.0.0.1:PORT/dav --tree /tmp/tree --mnt /tmp/mnt --bin
/tmp/bin/bunker --concurrency 25 --csv /tmp/battery.csv`. **Two rules from the run that died:** never
`pkill -f <pattern>` where the pattern is in your own command line (the committed script now uses
`pkill -x`; this run used an explicit PID through `--stop-endpoint`), and put a bounded `timeout` around
every unmount — no unmount in this session needed more than a second, but the bound is what keeps a
stuck FUSE daemon from becoming a stuck session.

**Operational note, not part of the measurement:** the worktree
`/home/kara/worktrees/bunker-BFS-016` was deleted mid-run by something outside this session (its
directory vanished while `git worktree list` still showed the registration; it was restored from
`wt/BFS-016`, which had no commits at that moment). All measurement artifacts lived in `/tmp` and were
unaffected because of that. A worktree reaper that classifies a commit-less worktree as merged will do
this to any evidence-only row.
