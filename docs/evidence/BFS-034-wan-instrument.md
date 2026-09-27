# BFS-034 — the stall measurement made possible AT A DATACENTRE

The release rule is **0 stalls on BOTH DCs**. The topology half was already done
(`docs/evidence/DC-both-datacentres-snapshot.md`: the snapshot walk is 110 ms on Helsinki and on
Falkenstein). The **stall** half was not, and the blocker was the instrument: `probes/bunker-fs-battery.sh`
takes `--url`, so it looks like it can be aimed at a datacentre, and it cannot. This row delivers a battery
in which **every verification of a write happens on the same side of the link as the write**, a
**negative control** proving the old approach would have passed a broken write, and — because a DC happened
to be reachable — **stall counts from two real data centres**.

Everything below is from a run in this session. Nothing is extrapolated from loopback.

---

## 1. The defect, confirmed in the file (not taken from the row's line numbers)

Read at `probes/bunker-fs-battery.sh` in this worktree (`f43b613`). Each line is the current file's own:

| line | code | which side it touches |
|---|---|---|
| 53 | `TREE="$(cd "$TREE" && pwd)"` | resolves the tree **locally** |
| 134 | `rm -f "$TREE/.battery-probe.txt"` | mutates the **local** tree |
| 176–178 | section titled *"POSIX-level proof of the writes on the SERVER side"*, body `find "$TREE" -name 'renamed.txt' -o -name 'hello.txt'` | reads the **local** tree |
| 222 | `BASE="$(git -C "$TREE" rev-parse --abbrev-ref HEAD …)"` | git against the **local** tree |
| 239–243 | `git -C "$TREE" rebase --abort` … `branch -D "$BRANCH"` | cleanup against the **local** tree |
| 248–255 | `printf … > "$TREE/conflict.txt"`, `sha256sum "$TREE/conflict.txt"` | writes and hashes the **local** tree |
| 270–279 | `printf … > "$TREE/inval.txt"` | writes the **local** tree |

All seven confirmed. The consequence is the dangerous kind: against a remote server the local tree is the
**wrong side of the link**, and the verification reads a stale copy — so the battery can go green while
proving nothing, and it looks like a legitimate pass.

### The same defect, demonstrated with the committed instrument itself

`docs/evidence/BFS-034-old-instrument-run.txt`, step 5 — the **unmodified** committed script, pointed at a
served endpoint with `--tree` = a local tree that is not the served one (the only thing an operator can pass
when they have no mirror of the DC tree):

```
--- the old script's own 'server side' lines (transcript) ---
      POSIX-level proof of the writes on the SERVER side (the bytes are really there):
      server-side view of the mount's scratch file (should be empty: it was removed):
        /tmp/bfs034-old/mirror/newdir/renamed.txt
        /tmp/bfs034-old/mirror/newdir/hello.txt
--- the TRUTH, read on the side (the served tree) ---
   served  newdir/hello.txt: ABSENT
   mirror  newdir/hello.txt: present (24 bytes) ...
   served  newdir/renamed.txt: ABSENT
   mirror  newdir/renamed.txt: present (26 bytes) ...
```

The script printed **two file paths from the local tree** under the heading that claims the server's bytes,
for two files the served tree does not have. Its §5 label is the same shape:

```
     sha256 on the server: 8f9299f0efa81d3cf015a64b3d965456442b6892ccfc8adf581d83dc22d881f2
   the file's bytes on the server (must be the AGENT side edit, unchanged):
     agent side edit
```

That hash is the local tree's. `cat` through the mount had already failed
(`cat: /tmp/bfs034-old/mnt-snap/conflict.txt: No such file or directory`) because the file it hashed was
written to the wrong side.

**And it does damage, not just nothing.** After that run, the *served* tree was left dirty by the mounted
cells while the cleanup lines ran against the local tree:

```
   served status: ?? .battery-probe.txt ?? conflict.txt
   served .battery-probe.txt: PRESENT        mirror .battery-probe.txt: absent
```

The scratch file the battery creates through the mount is still on the server, and the cleanup that was
supposed to remove it ran on the mirror. The measured tree is the one that was mutated.

---

## 2. What was built, and why a second file rather than a `--mode` flag

**`probes/bunker-fs-battery-wan.sh`** (new). The choice is stated in its header and here:

1. **A flag reproduces the exact shape this row is about.** `--mode remote` keeps one command line, one
   `--tree`, and a meaning that changes invisibly at the call site. That is how the original misleads.
2. **The original must stay byte-identical** so BFS-016's 35-cell loopback numbers remain attributable to
   the instrument that produced them.
3. The contract is genuinely different — the WAN variant **requires a declared side and refuses to measure
   without one** — and that contract is easier to audit when it lives in one file.

### The rule, and how it is enforced rather than documented

* The **side** is declared: `--ssh HOST` (every read-back is `ssh`, mechanism **(b)**, the authoritative
  server-side view) or local (the server serves that directory off this filesystem, so the local read *is*
  the server-side read — mechanism (b) realised locally).
* Before any measurement, a **SIDE ASSERTION** writes a nonce **through the mount** and reads it back **on
  the declared side**. Mismatch ⇒ `exit 3`, no number reported. This is the guard that turns the original's
  silent false green into a loud refusal, and it is proved in §4.
* Every write cell is additionally cross-checked **through the mount** (mechanism **a**, what a user sees).

### Which mechanism each cell uses

| cell | the write lands… | verified by | why |
|---|---|---|---|
| mkdir / write / rename / append / rm / rmdir (13 ops) | on the side | **(b)** read of the side tree, per cell | "the bytes are really there"; the original's §2 heading claims exactly this |
| `read it back`, `ls newdir` | — | **(a)** mount read compared to the **(b)** side read | what a user actually experiences |
| git cells (13) | on the side (they run *through* the mount) | **(b)** side index / side HEAD | the cells that touch the repo are same-side by construction |
| git BASE + all cleanup | — | **(b)** `side_branch()` on the side | the fix for lines 222/239–243 |
| conflict (§5) | through the mount onto the side | **(b)** side bytes, after a 0.5 s settle | "the file's bytes on the server" is now literally that |
| invalidation (§6) | side | **(b)** side value vs **(a)** mount value | the claim is two-sided by nature; both sides are now observed |
| transport kill (§9) | — | **(b)** the side must **not** have `killed.txt` | a write that reports failure must not have landed |
| control (§8) | through the mount onto the side | **(b)** side + **(a)** fresh mount | the row's negative control |
| cache figures (§2, §7) | local by definition | **local**, and labelled as such | the client's own cache is a local resource; it makes no cross-link claim |

### Kept unchanged, so the numbers sit beside BFS-016's

The 14-operation shape (13 mount ops + 13 git cells, same names, same order), the per-op wall clock (each
op timed exactly as the original times it), the exit **class**, and the **stall classification**
(`rc=124 ⇒ STALL(timeout)`). CSV section names and op names are identical, so the measured rows line up
against BFS-016's loopback run. Same-side verification cells are recorded in their **own** `verify` section
so they cannot disturb the measured rows.

**This is checked, not asserted:** `BFS-034-probes/shape-check.sh` extracts every measured cell from both
files as `section,op` in file order, diffs the two lists, and compares the classifier body. Output in
`BFS-034-shape-check.txt`: `35` cells each, `IDENTICAL (section, op) list in the SAME ORDER : YES`,
`classify() (the stall classification) byte-identical : YES`, and the only WAN-only CSV sections are
`control` and `verify` — both new work, neither a measurement.

### Changed, deliberately, and named
* every "server side" read is a read of the **side** (ssh when remote) — the point of the file;
* each mount gets its **own** `--cache-dir`, and the counters/status/conflict log are read from that
  directory (BFS-016 found the client's counter reader picking up **another mount's** `status.json`);
* **no `pkill` at all** — mount processes are tracked by PID and killed by pid; every `fusermount` is
  bounded with `timeout` (a sibling session's mount is not this run's to kill);
* `--fresh-tree` / `--fixture-only` build the fixture **on the side** so the battery can run against a
  server whose tree this repo has no local mirror of, and record exactly how it was created.

---

## 3. Evidence

| artifact | what it shows |
|---|---|
| `BFS-034-local-run.txt` | the full battery on a local server: 13/13 ops, 3/3 tree-read, 13/13 git, 1/1 bound, 5/5 kill — **0 stalls** — with every same-side verdict |
| `BFS-034-old-instrument-run.txt` + `BFS-034-old-instrument-transcript.txt` | **negative control, part 1**: the committed old instrument pointed at a server it does not own, printing the local tree's files as the server's |
| `BFS-034-wrong-side-run.txt` | the new instrument on the same wrong-side configuration: **`exit 3`, refuses to measure** |
| `BFS-034-visibility.txt` | why a side read must poll: a write is visible on the side after **10–41 ms** (loopback) |
| `BFS-034-cache-dir-identity.txt` | the default cache directory is keyed **per endpoint**, so two mounts of one URL share one `status.json` |
| `BFS-034-cache-bound-arms.txt` | the 64 KiB bound **is** enforced when the accounting is per-mount (62,081 B ≤ 65,536; 4 entries; 36 evictions) |
| `BFS-034-dc1-dedi2-run.txt`, `BFS-034-dc2-bunker-mvp-run.txt` | real-DC runs with side-side verification and the stall counts |
| `BFS-034-probes/*.sh` | every runner and probe, re-runnable |

### 3.1 The negative control (the deliverable after the fix itself)

Section 8 of the battery, run against the DC and against a local server. The mirror is built from the
**side's own state** immediately after the write landed, so it is exactly what any local mirror of a healthy
link would contain; it is then made stale by exactly **one fault event** — the side is given different
bytes, which is what a broken write looks like from the client's point of view.

```
   8a  write through the mount      : rc=0  path=…/mnt-ctl/.wan-ctl.txt
   8b  (b) same-side read           : 'A:AC-NONCE-…'  sha=cdf0e3e42604
       (a) mount read                : 'A:AC-NONCE-…'
   8c  mirror (local, = the side now): 'A:AC-NONCE-…'  sha=cdf0e3e42604
   8d  FAULT on the side            : 'B:AC-NONCE-…'  sha=8c5a2bb657aa
       the fault changed the bytes (sha differs) — the control is not vacuous
   8e  OLD verification (local tree): 'A:AC-NONCE-…'  -> PASS(blind)
   8f  NEW verification (b) side   : 'B:AC-NONCE-…'  -> FAIL(detected)
       (a) existing mount read      : 'A:AC-NONCE-…'  <- reported, not trusted
       (a) FRESH mount read         : 'B:AC-NONCE-…'  -> FAIL(detected)
   8g  fail-closed check: run the SIDE ASSERTION with the mirror declared as the side
       MISMATCH — the guard REFUSES to measure against a mirror. Correct.

   CONTROL VERDICT: the old path PASSED a broken write, the same-side path DETECTED it,
   and the guard refused the mirror. The instrument can fail.
```

The control is **invalid unless every arm behaves as required** (fault changes the sha; old path passes;
same-side path fails; fresh mount fails; the guard refuses); otherwise the battery exits **4**. An
instrument that cannot fail is not evidence.

**A sub-finding worth keeping:** the `(a) existing mount read` is **blind** — the already-mounted client
answered `A` after the side had `B`, because a mount serves reads from its own cache/inflight view. So a
read through an *existing* mount is **not** a verification path: only the side, or a fresh mount, is. That
is why the battery prefers (b).

### 3.2 The fail-closed arm (a wrong declared side cannot produce a number)

`BFS-034-wrong-side-run.txt`: served tree and `--tree` are different trees, exactly the mis-pointed
configuration. The battery prints the mismatch, names both values, and exits 3:

```
   SIDE ASSERTION — is the declared tree really the side these writes land on?
     MISMATCH — the declared tree does NOT see the mount's writes.
     REFUSING TO MEASURE (exit 3): a number from this configuration would
     be a green that proves nothing — the exact defect this file exists for.
battery rc=3
PASS: the instrument refused to measure against a tree that is not the write side.
```

---

## 4. The DC runs

Both data centres were reached with the release's own `probes/davserve`, on a **spare high port**, started
`setsid --fork` with a pidfile and torn down by **explicit PID** — never a pattern kill. The live `bunkerd`
on 18080 was not deployed to, restarted, or reconfigured: the pre-flight and the teardown both print its
state and the listeners, and the teardown prints the probe port's listener count (0). Falkenstein keeps its
`ufw` DROP policy; its probe port is reached through an `ssh -L` tunnel rather than by opening a hole.

| | **DC 1 — Helsinki** (`dedi-2`, public path) | **DC 2 — Falkenstein** (`bunker-mvp`, `ssh -L`) |
|---|---|---|
| run 1 (instrument before the fixes) | **1 stall** — `diff --stat HEAD` **45.054 s** | not run with that revision |
| run 2 (the shipped instrument) | **1 stall** — `diff --stat HEAD` **45.061 s** | **1 stall** — `diff --stat HEAD` **45.078 s** |
| ops (13 cells) | **13/13 ok, 0 stalls** | **13/13 ok, 0 stalls** |
| per-op floor | 0.11–0.91 s | 0.11–0.81 s |
| tree-read: snapshot / PROPFIND c=25 / c=1 | 0.218 / 2.105 / 2.078 s | 0.212 / 2.356 / 1.909 s |
| git (13 cells) | **12/13 ok, 1 stall** | **12/13 ok, 1 stall** |
| `status --short` / `status --porcelain -uno` | 20.526 / 11.219 s | 37.633 / 14.169 s |
| `rebase HEAD~1` | 23.985 s | 28.240 s |
| bound: read 40 files at 64 KiB | 8.643 s | 13.767 s |
| kill (5 cells) | **5/5 ok, 0 stalls** | **5/5 ok, 0 stalls** |
| side assertion (nonce through the mount, read on the DC) | **MATCH** | **MATCH** |
| negative control | **as required** | **as required** |
| the live `bunkerd` on 18080 | untouched, `active`, both listeners | untouched, `active`, both listeners |
| artifacts | `BFS-034-dc1-dedi2-run.txt` | `BFS-034-dc2-bunker-mvp-run.txt` |

**The stall is reproducible, not a fluke of one link:** `diff --stat HEAD` hit the **45 s per-op deadline**
on every run — 45.054 s, 45.061 s (Helsinki, twice) and 45.078 s (Falkenstein, over a different path).
Three runs, two data centres, one cell, all pinned to the deadline. The **stall count at both DCs is 1**,
and it is not an artefact of a stale local mirror: every one of those cells was verified on the side the
write landed.

The rule "0 stalls on BOTH DCs" is therefore **NOT satisfied** — and for the first time it is *measured*
rather than unknown. The topology half is unchanged (110 ms snapshot walk on both DCs); the stall half is
now a number with a named cause.

### What the DC runs say

* The **side assertion passes at a DC** over a real WAN link, on both hosts — the instrument's precondition
  holds where it matters, not just on loopback. Without it, a number from either run would be unbacked.
* The **negative control passes at a DC**, in the same session, over ssh: the mirror read reports success on
  the deliberately broken write while the same-side read detects it.
* **Both DCs report the same single stall**, in the same cell, pinned to the same 45 s deadline — so the
  finding is about `diff --stat HEAD` over a mount, not about one host's link.
* The per-op floors move with the link, as expected: local ops ~0.11 s, Helsinki 0.11–0.91 s, Falkenstein
  0.11–0.81 s; and the snapshot walk costs **0.212–0.218 s at a DC against 2.08–2.36 s for the PROPFIND
  walk** — the snapshot stops paying round trips. That is the mechanism the topology half measured (110 ms
  at both DCs) seen from a second, independent run.
* `status --short` (20.5 s / 37.6 s) and `rebase HEAD~1` (24.0 s / 28.2 s) are **one deadline away from
  being stalls themselves**; they are reported as `ok` because they finished, and named here because a
  release reading "0 stalls" deserves to know how close two more cells came.

---

## 5. Instrument defects found by USING it (this is the part that only a DC run finds)

1. **A single side read is a race, not a verification.** Writes are visible on the side after
   **10–41 ms** (`BFS-034-visibility.txt`), so a one-shot `cat` reports a false MISMATCH on a healthy link.
   Every side cell now **polls with a bound** (`side_wait_eq`), and a bound that *expires* is the mismatch —
   "not visible yet" and "not there" are told apart by the bound, and the poll count is recorded in the note.
2. **`mnt-ctl` is a substring of `mnt-ctl2`.** The mount check was `grep -q "$mnt" /proc/mounts`, so the
   unmount of `ctl` waited for `ctl2` and then warned. Now `/proc/mounts` is matched **by whole field**
   (`is_mounted`). The same substring class the fleet has been bitten by before.
3. **A uid-preserving extract silently blinded every side git reading.** The fixture was pushed with
   `tar -xzf -` as root while carrying the local uid, so git on the DC answered
   `fatal: detected dubious ownership in repository` — and, with `2>/dev/null` in the cell, the reading came
   back **empty**, which read as `base=` and as a vacuous pass. Fixed three ways: push with
   `--no-same-owner`; print git's stderr verbatim in the fixture step; and a **SIDE CAPABILITY** probe that
   names a git that cannot read the side tree instead of letting it empty a cell.
4. **`verify_row` accepted an empty expectation as "ok".** The first DC run's cleanup cell passed with
   `exp= got=`. An empty expectation is now class **VACUOUS** and counted (0 in every shipped run).
5. **The old §7 cache figure and mine are not the same measurement.** The original reports
   `used_bytes=3,280,570` against `max_bytes=65,536`; this instrument reports `62,081`, 4 entries, 36
   evictions. The `BFS-034-cache-dir-identity.txt` probe shows the default cache directory is **keyed by
   endpoint** (two mounts of one URL: same directory, same `mount` id), so the original's mount shared its
   document with the earlier 256 MiB-bound mounts of the same URL, and `BFS-034-cache-bound-arms.txt` shows
   the bound **is** enforced when the accounting is per-mount. **Not fully attributed** — the exact
   contribution of the shared directory is inferred, not isolated — and it is named here rather than smoothed
   over; it belongs with BFS-031/BFS-016, not with stalls. Residual: `du` on the directory is **81,920 B >
   65,536 B** in all three arms, i.e. the directory exceeds the bound even when the client's figures do not
   (BFS-031's shape, reproduced).

---

## 6. What the same-side verification caught, immediately

Running it exposed live defects rather than green ticks. These are **reported, not fixed** here (each has
its own row):

| cell | same-side verdict | attribution |
|---|---|---|
| `append (>>)` | **MISMATCH** — the side holds the first line and nothing after it, where the appended line should be | BFS-021 (`>>` loses bytes) |
| conflict refusal | **MISMATCH** — the side holds an **empty** file where the agent's edit was; the target survives but its content is gone | BFS-030 (the truncate half commits, the write half fails) + BFS-033 (412 recorded, 204 lands) |
| invalidation window | **finding** — the mount answers `one`, the side has `two` after the declared window | BFS-016 §6 (stale read) |
| git index | the mount-side `add` returns rc=128 and the side index has nothing | BFS-020 / readdir family |
| `diff --stat HEAD` **at Helsinki** | **STALL**, 45.054 s = the per-op deadline | named, see §4 |

Those are the numbers the release wanted: they are now produced **on the side the write landed**, at a DC,
and each is attributable.

---

## 7. Cells that could NOT be made same-side, and why

* **The client's own records** — `status.json`, `conflicts.jsonl`, the cache figures and `du` (§2, §7).
  These are the **client's** state; the local side is the correct side for them, and they are labelled as
  local so they are never read as cross-link evidence. §7 says so in the output.
* **The git index cell, on a host with no usable git.** Reading an index requires git *on the side*; where
  the side has none, the cell is recorded as **not-verifiable on the side, with the reason**, and not as a
  client mismatch. The branch/HEAD cells have a git-free fallback (`.git/HEAD`), so they stay same-side
  everywhere.
* **A read through an already-mounted client** is not a verification at all (§3.1): it is reported, and the
  report says it is not trusted.
* **The transport-kill write cell** is verified by absence on the side, which is the strongest available
  statement ("the bytes are not there"); there is no byte-level positive verification of a write that by
  design never happens.

---

## 8. Named gaps (so the next row does not inherit a surprise)

* **One run per arm per DC.** No distribution, no variance, no repeated trials — the same limitation the
  topology half records. The DC numbers below are single measurements.
* **The stall half is now MEASURED, and it is 1 stall at each DC** — the rule is not satisfied. What this
  row delivers is the instrument that can say so honestly, plus the first real counts (three runs, two DCs,
  one cell, one deadline). Distribution and repeated trials remain undone.
* **`status --short` at DC 2 took 37.6 s against a 45 s deadline.** Reported as `ok`; named as an immediate
  neighbour of a stall.
* **`diff --stat HEAD` stalls at 45.05 s on Helsinki.** Whether that is the readdir defect, the 30 s
  deadline behaviour, or per-file round trips is **not** attributed here.
* **The original battery is unchanged and still misleads if pointed at a remote endpoint.** It is documented
  in its own header as local-only; making that refusal loud is a candidate for the same fix, deliberately
  not taken in this row (its file is the record of BFS-016's numbers).
* **The control's fault is an out-of-band overwrite on the side.** It is the *shape* of a broken write, not
  a server bug; it deliberately does not depend on any client behaviour.
