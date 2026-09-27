# BFS-024 — verified on the current tree: the row's scenario is SATISFIED by landed work

**Decision, up front.** The row was run as filed against current main. A path that a mount
already serves **from its own cache**, replaced by an **out-of-band shell write on the served
target**, is served **fresh inside the declared freshness window** in every writer class
measured. The worst observed latency is **1904 ms against a declared poll interval of 2000 ms**,
and the interval is stated by the mount's own record (`poll_interval_ms: 2000`).

So this is a **verification-only close**: BFS-024 is satisfied by work that has landed since it
was filed. **No fix was manufactured for it and no row was filed from it.** What follows is the
live transcript, the mechanism that covered each class, the negative controls, and the coverage
boundary stated honestly rather than as a claim of completeness.

---

## 1. What was measured, and against what

| | |
|---|---|
| repo | `/home/kara/bunker`, worktree `/home/kara/worktrees/bunker-BFS-024`, branch `wt/BFS-024` |
| measured commit | **2fb5ffb** (build stamp: `bunker 0.1.4 commit: 2fb5ffb built: 2026-09-27T13:46:26Z`) |
| origin/main at the end of the run | **6dab585** — its only advance over 2fb5ffb is `.coding-hermes/board/tasks.jsonl` (a board commit). `git diff --name-only 2fb5ffb 6dab585` lists **that one file and no code**, so the tree measured is code-identical to current main. |
| binaries | `./bunker` (CLI + FUSE client) and `probes/davserve` (the same `internal/server/webdav` handler on a loopback listener, no agent lifecycle — see its doc comment) built from that commit |
| probe | `docs/evidence/BFS-024-probe.sh` → transcript `docs/evidence/BFS-024-probe-final.txt` |
| cost probe | `docs/evidence/BFS-024-fastpath.sh` → transcript `docs/evidence/BFS-024-fastpath-final.txt` |
| guard | the probe scripts were committed under the Tier-1 guard: `Tier 1 Guards: PASS (test mode: full)` — secrets, go_build, go_lint, go_tests all green (commit 4768d24) |

**The row, as filed** (`docs/evidence/BFS-009-visibility-nongit.txt`, arm P2b): a file read once
through the mount, then replaced **on the agent** with different-length content, kept serving the
**pre-edit** bytes — "mount (post-edit) = 'arm-b-original-content' (22 chars), NATIVE control =
'arm-b-REPLACED-with-different-length-content' (45 bytes), VERDICT: STALE", with
`evictions=0 bypass=0 hits=0` and `resyncs_total=0` in the channel record.

**The scenario this probe runs**, in the row's own terms: create the path **before** the mount
binds; read it through the mount; prove it is served from cache; replace it with a **shell write
on the served directory** (never WebDAV — a WebDAV write would be the surface talking to itself);
read it again, every 100 ms, on **one clock**, until it serves the new bytes.

**Per-arm premise, proven rather than assumed.** Every arm states `cached=yes` only when two
further reads through the mount cost **zero** requests (the mount's own `transport.requests_total`
counter). An arm that cannot show that is printed `PREMISE-VACUOUS` and is **not** counted in
either direction. This matters: an earlier run of this probe (before the premise check existed)
can serve a fresh answer from a path that was never cached, and that would have been a vacuous
green.

---

## 2. The result, per out-of-band writer class

Load-bearing run (`BFS-024-probe-final.txt`), one mount, one clock:

| arm | writer class | size | cached premise | read at +400 ms | read at +500 ms | first FRESH | first refusal |
|---|---|---|---|---|---|---|---|
| W1 | replace, **different length** (the row's own shape) | 22 → 41 | **yes** (2 rereads = 0 requests) | **stale** (v1) | **stale** (v1) | **1897 ms** | none |
| W2 | replace, **same size + mtime preserved** (adversarial) | 22 → 22 | **yes** | fresh | fresh | **1382 ms** | none |
| W3 | replace, same size, new mtime | 22 → 22 | **no** → `PREMISE-VACUOUS` | stale | stale | 1904 ms | none |
| W4 | **unlink + recreate** | 22 → 37 | **yes** | fresh | fresh | **1234 ms** | none |
| W5 | **atomic rename over** | 22 → 28 | **yes** | fresh | fresh | **1164 ms** | none |

The spread in the "+400 ms" column is the poll **phase**, not a difference between classes: the
mount polls on its own 2000 ms cadence, so an edit that lands just before a boundary is seen
within ~0 ms and one that lands just after it waits nearly the whole interval. Two earlier runs
of the same probe are kept as transcripts —
`BFS-024-probe-runA-uncached-premise.txt` and `BFS-024-probe-runB-uncached-premise.txt`; they
predate the per-arm cache-premise check, so they are corroboration only. In both, the same-size,
unlink+recreate and rename-over classes (W2–W5) showed **stale** control reads at +400/+500 ms and
went fresh at `1346 / 1474 / 1703 / 1447 ms` and `1328 / 1460 / 1689 / 1422 ms`; W1 (the row's own
arm) appeared fresh at +400 ms in both with the channel counters still at zero — with no cache
premise proven, that is a **vacuous green**, and it is why the premise check was added.
Across all runs the observed maximum is **1904 ms ≤ the declared 2000 ms interval**.

**W1 is the row's own arm**, and the load-bearing run is the one where its cache premise is
proven: cached (`cached=yes`), **stale at +400 ms and +500 ms**, then **fresh at 1897 ms**, with
the mount's record moving `events_total 0 → 1`, `paths_dropped_total 0 → 1` across exactly that
window. The defect BFS-009 recorded — pre-edit bytes, nothing moved — does not reproduce on this
tree. (In the two earlier runs W1 appeared fresh at +400 ms with the channel counters still at
zero; that run had no premise proof, so under the doctrine of this project it is a **vacuous
green** and is not offered as evidence. The final run's version of that arm is.)

---

## 3. The controls that make the fresh reads attributable

1. **Native control, per arm.** After every edit, the bytes on the served target were hashed
   natively and printed; in every arm that hash equals the sha of the bytes the mount eventually
   served (`NEG native read` lines). The new bytes were really there.
2. **Fresh-mount negative control.** A **second mount**, bound **after** all the edits, with its
   own cache directory and no history, read every arm path for the first time. Every one of the
   seven paths came back with the **new** sha, each equal to the corresponding native sha
   (`BFS-024-probe-final.txt`, "NEG control" section). So the stale reads on mount 1 were **its
   cache**, not a broken fixture, and the fixture itself cannot make a stale answer look fresh.
3. **The channel's own record.** Three counters move with the work and none of them are inferred:
   `events_total 6`, `paths_dropped_total 11`, `seq 7`, `resyncs_total 0`, `results_from_gap 0`,
   `mechanism=events`, `mode=poll`, `channel_available=true`. The per-arm dumps show each arm's
   own increment (`0→1`, `1→2`, `2→4`, `4→5`).
4. **BFS-025's shape as a contrast arm (N2).** A path that was only **stat'ed**, never read, then
   replaced with longer content: `cat` **refuses loudly** — `rc=1`, `Stale file handle`,
   `cause=stale_bound: refusing to serve 48 bytes where the kernel bounds this path at 23` —
   instead of returning a fragment with rc=0. The buffered (`python`) read returns the full 48
   bytes. That is the BFS-025 behaviour, unchanged, and it is a different defect class from this
   row.
5. **In-band control (N1).** A file created **through the mount** lands on the target with the
   same sha the mount then reads back. (An in-place overwrite of an existing file is refused by
   design — BFS-030 — so the control uses a new name.)

---

## 4. Which mechanism covered it — and which did not

* **The events poll covered it.** The mount's record says so in its own words:
  `mode=poll`, `mechanism=events`, `poll_interval_ms=2000`, `channel_available=true`, with a
  `paths_dropped_total` increment per arm. This is the op landed by **BFS-026**
  (`X-Bunker-Op: events`, served from the ledger whose per-path identity is
  **(size, mtime, ctime)** — the ctime member is exactly why the same-size, mtime-preserved arm
  W2 is caught), with **BFS-063**'s rule that a client which has observed nothing is told the
  interval is unvouched (so the channel never answers a fresh client "nothing changed") and
  **BFS-060**'s rule that a channel that cannot be established is reported as such.
* **The watcher did NOT cover it — because it is not in force here.** The surface's capability
  document lists the degradation verbatim: `watch (scope=target mode=poll): no watcher is
  established on this target: the server-side watcher is not enabled in this deployment; the
  declared poll form X-Bunker-Op: events carries the channel (mode=poll)`. BFS-035's watcher is
  landed and is an **opt-in** (`webdav.Config.WatchEnabled`, off by default; the operator knob is
  BFS-043). So on every deployment today the poll is the mechanism, and the push form was **not
  measured here** — that is a statement about what this transcript covers, not a claim about the
  watcher.
* **The revision poll did NOT cover it** and is not in force: `mechanism=events`, not `rev`. Per
  BFS-048 the rev token is blind to uncommitted writes on a git tree; the client reports that gap
  when the rev tier is the one answering.

---

## 5. Coverage, stated honestly

**Caught (measured, this transcript):** an out-of-band writer that goes through the served
filesystem's normal path and changes any of **size, mtime or ctime** — different-length replace,
same-size replace with the mtime restored, same-size replace with a new mtime, unlink + recreate,
and atomic rename over. All five reach the mount's cache as a per-path drop and are served fresh
within the declared interval.

**Not caught (named, not fixed here):**

1. **A writer that restores size, mtime *and* ctime.** `internal/server/webdav/events.go` declares
   this boundary itself ("a rewriter that restores size, mtime AND ctime is invisible to any
   observer that does not read the bytes; the write path's content-hash re-validation (§6.1) is
   what covers that case; a change channel is not"). It is **not reachable from userspace** —
   `touch`, `chmod`, `mv` and ordinary writes all move ctime — so the arm cannot be built without
   filesystem-level manipulation. It is a declared gap, not a measured one.
2. **A writer landing bytes in a different superblock or container overlay upperdir**, so the
   served directory's own view does not change: BFS-051's class — invisible to push, poll **and**
   revision together. **Not re-measured here**; this probe writes to the same served directory
   the surface reads.
3. **The window itself.** A read **inside** the poll interval can still serve stale bytes — that
   is what a 2 s poll *is*, and it is the measured behaviour in arms W1/W4/W5 at +400/+500 ms.
   The window is **declared and reported** (`poll_interval_ms: 2000` in the mount's own
   `invalidation` record) and the observed worst case was 1904 ms. A bound the owner cannot see
   would not be a bound; this one is visible.
4. **If the channel is dead**, the only remaining bound is the cache's backstop TTL — and the
   status document does **not** report it: `status.cache` carries `max_bytes/entries/…/hits`, but
   no age or TTL field (`internal/fsclient/status.go` reports none, and the default is
   `fsclient.DefaultCacheMaxAge = 1 h`). BFS-026's own notes name this same residual. **Not
   measured here** (it needs a server that refuses `events`, i.e. a pre-BFS-026 build); named so
   that the coverage statement above is not read as covering it.

---

## 6. The fast path, as numbers

The brief forbids paying for this with a stat-per-file walk. Two facts:

**This row changed no Go code.** `git diff --name-only origin/main...HEAD -- '*.go'` is empty;
the only files this row adds are the two probe scripts and this document. So the fast path is
byte-identical to main by construction — and here is what it costs, measured
(`BFS-024-fastpath-final.txt`, **200 files** in the served tree, `ls -l` etc. through the mount,
counted with the **mount's own** `transport.requests_total`):

| operation | requests | wall |
|---|---|---|
| `ls -l` (cold first look) | **0** | 55 ms |
| `ls -l` (again) | **0** | 44 ms |
| `ls -lR` (a whole-tree walk) | **0** | 43 ms |
| `find -type f \| wc -l` | **0** | 21 ms |
| `stat` one file | 0–1 | 16 ms |
| read all 200 files, cold | 135 | 2822 ms |
| read all 200 files, warm | 1 | 1954 ms |
| read one file, warm | **0** | 12 ms |
| **idle 6.3 s = 3.1 poll intervals** | **3** (`208 → 211`) | — |
| after an out-of-band edit: `ls -l` | **0** | 40 ms |

Two numbers carry the argument: **metadata work on a 200-file tree costs zero requests** (the
snapshot answers `ls`, the walk and `find` in-process), and **the invalidation channel costs one
request per poll interval regardless of tree size** — 3 requests over 3.1 intervals with 200
paths, not 200 requests. The same transcript shows the round trip end to end: the edited file is
read (stale, inside the window, 0 requests), then after 2.6 s the same path serves
`EDITED-OUT-OF-BAND` — again **0 requests**, from the re-populated cache — with the channel
recording `events=1 dropped=1 seq=2`.

*(Not attributed here because I did not isolate it: the cold/warm whole-directory read cells cost
135 / 1 requests for 200 files rather than 200 / 0. The client's own `hits`/`misses` counters read
`hits=202, misses=0`. I did not read the fetch path far enough to explain the accounting, so I am
reporting the numbers and naming the non-attribution rather than inventing a mechanism. It does
not bear on the claim above: metadata ops are 0 requests, the single-file warm read is 0, and the
channel is per-interval.)*

---

## 7. For the driver

* **Close BFS-024 as verification-only.** Its scenario does not reproduce on current main; the
  mechanism is BFS-026's `events` poll (with BFS-063/BFS-060), and the cache is no longer a
  stale-copy cache for any writer class that changes size, mtime or ctime.
* **No row was filed from this run** and no code was changed for it (`wt/BFS-024`, commit 4768d24
  adds `docs/evidence/BFS-024-probe.sh`, `docs/evidence/BFS-024-fastpath.sh` and this document
  plus its two transcripts). Nothing was pushed.
* **If a follow-up is wanted**, the coverage statement above names two things worth their own
  rows, both already named by BFS-026 rather than discovered here: the cache's **unreported**
  content-age bound when the channel is dead, and BFS-051's cross-superblock class. They are
  recorded here as boundaries of this verification, not as findings to close this row.
* `.coding-hermes/board/**` and `.gitreins/tasks.yaml` were **not** touched.

## 8. Reproduce it

```sh
cd /home/kara/worktrees/bunker-BFS-024
make build && go build -o bin/davserve ./probes/davserve
bash docs/evidence/BFS-024-probe.sh   --tree "$(mktemp -d)" --bin "$PWD/bunker" --davserve "$PWD/bin/davserve" --hold 14
bash docs/evidence/BFS-024-fastpath.sh --tree "$(mktemp -d)" --bin "$PWD/bunker" --davserve "$PWD/bin/davserve" --files 200
```

Both scripts create their own scratch tree and cache directory (`mktemp -d`), bound every
`fusermount` with `timeout`, kill by explicit PID and never use `pkill -f`, and clean up with a
`trap` on exit.
