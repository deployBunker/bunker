# Bankai release report — the bunker WebDAV multi-protocol layer and its FUSE client

**Released:** 2026-09-26 · **Main:** `6b0125f` · **Rows:** 20/20 complete · **Parity:** 0 throughout
**Boundaries held:** bunker + muster (+ musterflow's two doc lanes), no other fleet project touched.

---

## 1. What was asked, and what exists now

Deliver a WebDAV multi-protocol layer inside `bunkerd` (HTTP/1.1 + HTTP/2 + HTTP/3 on one port) and our
own FUSE client (`bunker-fs`, Linux first), plus Muster's HTTP/2/HTTP/3 client transports and `.proto`
support — each of the 20 board rows closed with committed, verifiable evidence.

**All 20 rows are closed**, verified by id across both boards, by the criteria's own grouping:

| group | rows | state |
|---|---|---|
| investigation | BFS-002, BFS-003, PROTO-010, PROTO-011 | 4/4 |
| spec | BFS-004, BFS-005, PROTO-012, PROTO-013 | 4/4 |
| execution | BFS-006, BFS-007, BFS-008, BFS-009, BFS-010, PROTO-014, PROTO-015, PROTO-016 | 8/8 |
| testing | BFS-011, BFS-012, PROTO-017, PROTO-018 | 4/4 |

## 2. The headline results, and how each was verified

**The client does not stall where sshfs does.** 29 of 35 battery cells `rc=0`, 6 errors all attributed,
**0 stalls** — against measured baselines of sshfs **5 ok / 7–8 stalls** and plain WebDAV **11 ok / 1
stall**. No cell hit the 45 s deadline; no `rc=124` anywhere. (`docs/evidence/BFS-016-*`)

**Whole-tree reads are ~1000× the NFS band.** 0.013–0.051 s via the snapshot op, against NFS's 33–36 s
and native's 0.41 s — and the mechanism is measured, not assumed: **one call, 421 nodes, zero requests
during the walk**.

**The win survives WAN latency.** At dedi-2's own measured 185.2 ms RTT (via a delay relay, both arms in
one run): snapshot walk **118 ms** vs no-snapshot **1416 ms** = **12.0×**. The mechanism is why the ratio
*grows* with latency: **the tree is fetched once at mount, and every later whole-tree walk is local.**
(`docs/evidence/WAN-latency-snapshot.md`)

**The lever is concurrency, and the protocol is only the carrier.** h2 12.231→2.008 s at conc 1→8
(7.7449×); h1 on *one* connection does not degrade (0.9995×, correct); and at the same conc=8, changing
*only* the connection count moves h1 11.966→**1.847 s (6.4777×)**. BFS-016 refined this: concurrency is
the lever for independent requests, the **snapshot** is the lever for a whole-tree walk. (`BFS-011`)

**Older clients are not broken — proven with a real third-party client.** `curl`, which knows nothing of
h2/h3 or our extensions: HTTP/1.1 confirmed on the wire; GET/HEAD/OPTIONS 200; **PROPFIND 207**; plain
PUT 201 with the bytes **confirmed on the server's disk**; DELETE 204; a stale `If-Match` refused **412**
with a parseable `D:error` carrying the expected hash; a `Mozilla/4.0` UA and a no-`Accept` request both
200. (`docs/evidence/BFS-012-stock-client-*`)

**The pushed state builds and passes on a machine with no history of it** — a `--no-local` clone:
`go build` rc=0, `go vet` rc=0, **27 packages ok, 0 FAIL, 62.49 s**. This closes a real gap: the commit
guard verifies the *working tree*, not the stored commit, which already bit this release once.
(`docs/evidence/QA-fresh-clone.md`)

**Durability under injected failure.** A server killed mid-write left **0 bytes** server-side — no partial
file — and interrupted reads failed **loudly in 0 s**; after the kill the mount stayed mounted, a cached
file still read, and a never-cached file failed loudly. (`docs/evidence/` + the chaos cell in this log)

## 3. The findings — which are the most valuable output of this release

Every one of these was found by measurement, most by rows whose job was to *test* rather than to build.
They are filed as rows with their own acceptance criteria, not footnoted.

**The client is not yet safe for editing a working tree.** Reproduced independently by the driver, twice:

- **BFS-025 (P0) truncated reads.** A path that was merely `stat`ed, then replaced with longer content on
  the agent, returns a **fragment** of it with `rc=0` — e.g. **5 bytes of a 66-byte file**. Not stale
  bytes: *new* bytes, truncated, presented as success. A build or checksum consuming that gets a wrong
  answer with no error.
- **BFS-024 (P0) stale reads.** A path already read keeps serving pre-edit bytes after an agent-side
  replacement.
- **BFS-026 (P0) the root cause.** The client's invalidation depends on server ops that **do not exist**:
  the E-4 catalogue declares eight, the server implements two, and `events`/`watch` answer
  `capability_unavailable, "slice C5"`. The client detects and *reports* this honestly
  (`"mechanism": "none"`) — but with no third mechanism, nothing ever invalidates.
- **BFS-030 (P0) data loss on a failed rewrite.** `>` on an **existing** file truncates it, the O_TRUNC
  half **lands**, and then the write fails — leaving an **empty file** where the user's content was.
  In-place `r+b`/append writes are unreachable entirely.
- **BFS-033 (P0) the conflict refusal does not hold live.** A unit test passes; through the mount a **412
  is recorded in `conflicts.jsonl` and then a 204 lands**, caller sees success. Attributed below the
  client's API and specific to the ESTALE verdict (with EIO the refusal holds). The *detection* is sound
  and hash-based; the *enforcement* fails. A logged-then-bypassed refusal is worse than none, because it
  manufactures an audit trail claiming a protection the user did not get.

**The bound does not bound.** BFS-031: at a 1 KiB bound the cache directory reached 30,689 B = **29.97×**,
while the client's reported figures stayed inside — `status.json`/`conflicts.jsonl` are outside
`used_bytes`. BFS-032: `oversize_bypasses` **can never move** (pre-filtered before the counter). A counter
that cannot move is a gap, not a green check.

**Two platform-seam defects, one of which the row's own acceptance sentence found:** the `!linux` seam
**did not compile for any non-Linux platform** (fixed, with a committed cross-GOOS guard); BFS-028
`cli/umount.go` is POSIX-only and is the one site stopping `GOOS=windows go build ./...`; BFS-029
`fsclient/errors.go` reaches for `syscall.EREMOTEIO`, undefined on darwin/freebsd, despite the package's
own portable errno mechanism existing.

**Release-hygiene findings:** BFS-018 symlinks do not round-trip (a symlink appears as a regular file
containing its target — a repo with symlinks gets corrupted on first write-back); BFS-019 readdir can
return **empty while the directory has entries**; BFS-020 a created file cannot be renamed until
published, breaking git's lock-then-rename and leaving `<ref>.lock` on the tree; BFS-021 `>>` loses bytes;
BFS-022/023 the battery instrument carries a +110 ms `timeout` constant and the op deadline compounds to
60 s against a documented 30 s; BFS-017 the guard fails on host **load**, not code.

## 4. Honest ledger — what this release did NOT establish

- **The both-DCs rule is NOT satisfied.** Every client measurement is loopback or a simulated RTT. The
  rule requires 0 stalls on **both DCs**; the real DC leg is blocked on a Tailscale auth click.
- **No Windows anything.** The decision is made (WinFsp via cgofuse, opt-in; the OS redirector rejected
  because it cannot express the concurrency knob or carry the snapshot op). **No Windows code was written
  and no Windows result claimed**, because none could be run — a stub that cannot compile would make the
  repo *look* ported while nothing on Windows had ever run. Mint: **no work**, with reasoning.
- **Delegation (PRD slice C5) — the ~80× whole-tree win — is declared and honest but NOT BUILT.** Four of
  eight E-4 ops answer `capability_unavailable, "slice C5"`. This is the largest remaining gap and the
  root cause of BFS-026.
- **Nothing about stalls at WAN distance.** The WAN measurement is walk *time* only.
- **Named SKIPs, recorded rather than passed:** the live touch-only arm (it served stale bytes — the
  BFS-024/025/026 class), conflict refusal through a plain overwrite, concurrent-write bound arms, the 1 h
  TTL, whether the FUSE kernel or the VFS issues the retry, and the `>` data-loss shape reported without
  being root-caused to a line.

## 5. Release close — verified, not asserted

- **Worktrees reaped:** BFS-008, 009, 010, 012, 015, 016. Reaping was **not** a blanket sweep: the driver
  required 0 live workers, left any dirty worktree alone until its content was shown to be on main, and
  verified by content the two branches that read "unmerged" only because they had been rebased. The
  pre-existing INT-CI-045 residue was deliberately left.
- **Lanes restored:** all **21** from `/tmp/bankai-paused-lanes.txt`, read back from the DB and
  independently from the API — **0 still down**. The seven lanes paused *before* this release
  (temple-runner, rabbit-hole, ring-runner, asce, musterflow, escalation-doctrine, Kobayashi-Maru) were
  re-checked and are **untouched**.
- **Parity 0** on every landing; guard PASS on every commit.

## 6. Where the next work is

The release's own deliverable is complete; its **findings** are now the work, and five of them are P0:
**BFS-025** (truncated reads — silent corruption), **BFS-030** (`>` empties a file — data loss on a failed
rewrite), **BFS-033** (refusal logged then bypassed), **BFS-024** (stale reads) and **BFS-026** (the slice
C5 invalidation gap they share the root of). The honest summary: **`bunker-fs` is fit for reading
immutable data, and not yet safe for editing a working tree** — which is exactly the line the next wave
should cross, starting with BFS-026 because it is the root cause of three of the five.
