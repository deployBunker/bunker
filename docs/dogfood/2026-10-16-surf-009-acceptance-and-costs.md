# SURF-009 — acceptance re-verification + the two costs it did not measure

**Task:** SURF-009 — *toolchain provisioning for agent build capability*.
**Date:** 2026-10-16 (all timestamps UTC unless marked).
**Branch:** `wt/SURF-009` @ `2d84a536` (the judged commit; unchanged by this document —
this file is the only thing it adds).
**Why this doc exists:** SURF-009 was dispatched **twice**. The first dispatch wrote
the implementation (`internal/provisioning`, `internal/cli/toolchain_check.go`,
`internal/cli/spawn.go`, `docs/provisioning.md`) and ran the tier-2 judge, which
returned **`passed: true`** on 3/3 criteria (`.gitreins/verdicts/verdict.json`,
merge-gate). The second dispatch's brief asked for four acceptance criteria, two of
which were **in neither brief** — *"toolchain does not bloat the base image"* and
*"spawn latency impact measured"*. This document is the second dispatch doing the two
things a duplicate owes: **independent verification of the landed commit** (§1) and
**the criteria nobody owned** (§2, §3).

The proof of the *feature* is
[`2026-10-16-surf-009-toolchain-proof.md`](2026-10-16-surf-009-toolchain-proof.md)
(the first dispatch's live run: a real Go module built and tested on an agent, the
gate, the delivery transcript). Nothing here repeats it — this doc measures the
*price* and re-derives the headline claims from a separately built binary.

---

## 1. Independent verification of the landed commit

```
$ git -C <worktree> rev-parse HEAD
2d84a53644fcb02ad6048a22697e4f2b69cc6dd3
$ git -C <worktree> status --short          # before the build
?? .gitreins/verdicts/                      # the judge's own artifact, untracked
$ go build -o /tmp/surf009-measure/bunker ./cmd/bunker
$ sha256sum /tmp/surf009-measure/bunker
d41b62d934c29fb133d2fcdf4c720c1032ec7560439051f7cd313e199830ebb1
```

The hash is **byte-identical** to the `d41b62d9…` recorded in the proof doc's header:
the judged tree rebuilds to the same binary, so the measurements below were taken with
the same code the judge graded (and the binary is reproducible).

### A fresh agent has `go`, `gcc`, `make` — the brief's exact commands

Fresh vanilla agent `surf009-m-plain` (created 2026-10-16T04:07:35Z, uid 1361) on
**bunker-mvp**:

```
$ bunker exec surf009-m-plain 'go version'
go version go1.22.2 linux/amd64
rc=0

$ bunker exec surf009-m-plain 'gcc --version'
gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0
Copyright (C) 2023 Free Software Foundation, Inc.
This is free software; see the source for copying conditions.  There is NO
warranty; not even for MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
rc=0

$ bunker exec surf009-m-plain 'make --version'
GNU Make 4.3
Built for x86_64-pc-linux-gnu
Copyright (C) 1988-2020 Free Software Foundation, Inc.
License GPLv3+: GNU GPL version 3 or later <http://gnu.org/licenses/gpl.html>
This is free software: you are free to change and redistribute it.
There is NO WARRANTY, to the extent permitted by law.
rc=0

$ bunker exec surf009-m-plain 'hostname; id -un; id -u; uname -m'
bunker-mvp
bunker-surf009-m-plain
1361
x86_64

$ for t in go gcc make python3 node cargo; do printf "%s -> " $t; \
    bunker exec surf009-m-plain "command -v $t || echo ABSENT"; done
go -> /usr/bin/go
gcc -> /usr/bin/gcc
make -> /usr/bin/make
python3 -> /usr/bin/python3
node -> ABSENT
cargo -> ABSENT
```

`node`/`cargo` absent on the same agent is exactly the state the row predicted — and
what `--toolchains=full` / `--deliver` closes (§3).

### The check and its gate, independently re-run

```
$ bunker toolchain-check surf009-m-plain --class go
  TOOL     STATUS   REQ  VERSION / WHAT IT BUILDS
  python3  PRESENT  yes  Python 3.12.3
  go       PRESENT  yes  go version go1.22.2 linux/amd64
  gcc      PRESENT  no   gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0
  make     PRESENT  yes  GNU Make 4.3
  node     ABSENT   no   JS/TS builds (npm run build) and npm-installed toolchains
  cargo    ABSENT   no   Rust binaries (cargo build / cargo test)

  class go: every required toolchain is PRESENT (python3, go, make) — this agent can build Go projects
exit=0

$ bunker toolchain-check surf009-m-plain --class full
  ... (same six lines; node and cargo now REQ yes) ...
  class full: MISSING on the agent: cargo, node
bunker: toolchain class "full" incomplete on agent "surf009-m-plain": missing cargo, node
exit=1

$ bunker toolchain-check surf009-m-plain --class full --report-only
exit=0
```

One line per tool, `PRESENT`/`ABSENT` **by name**, the class requirement marked, and the
exit code doing the gating — reproduced from a binary built by a different session than
the one that wrote the code.

---

## 2. Criterion: the toolchain does not bloat the base image

Three separate questions, each measured.

### 2a. What this change adds to the image: **zero bytes**

No `Dockerfile`, no image build step, no default image change. Provisioning is
client-side and opt-in (a spawn without `--toolchains` is byte-identically the old
spawn), and delivery lands in the **agent's own `$HOME`**. The only image-facing
artifact in the change is a *printed* directive an operator may paste into an
image-spec file (opt-in by construction):

```
{"packages":[{"manager":"apt","packages":["gcc","g++","make"]}]}
```

So the honest answer to "does it bloat the base image" is: it does not touch the base
image at all. The costs below are per-agent or host-shared, not image layers.

### 2b. The `go`/`gcc`/`make` half is the **host's single copy**, not per-agent and not in the image

Same device:inode for the file in the agent's namespace and for the file on the
bunker-mvp host itself — one inode, one copy on disk, visible to every agent:

```
$ bunker exec surf009-m-plain 'stat -c "%d:%i %n" /usr/lib/go-1.22/bin/go /usr/bin/gcc-13 /usr/bin/make'
2049:279052 /usr/lib/go-1.22/bin/go
2049:12686 /usr/bin/gcc-13
2049:7090 /usr/bin/make

$ ssh bunker-mvp 'stat -c "%d:%i %n" /usr/lib/go-1.22/bin/go /usr/bin/gcc-13 /usr/bin/make'
2049:279052 /usr/lib/go-1.22/bin/go
2049:12686 /usr/bin/gcc-13
2049:7090 /usr/bin/make
```

What those shared bytes cost on this host:

| path | KiB | MiB |
|---|---|---|
| `/usr/lib/go-1.22` (the Go toolchain) | 98,972 | 96.7 |
| `/usr/lib/gcc` + `/usr/libexec/gcc` + `/usr/share/gcc` (the C compiler family) | 116,980 | 114.2 |
| `/usr/bin/make` | 256 | 0.3 |
| **total, once per host** | **216,208** | **211.1** |

`dpkg`'s `Installed-Size` on the same agent is deliberately **not** used as the number
here: for `golang-go`, `gcc` and `python3` it reports the *metapackage* (69 / 37 /
81 KiB) — the real payload is the dependency closure the `du` above measures
(`make` 404 KiB and `git` 21,692 KiB are single-package and match).

### 2c. The delivered half (`node` + `cargo`), per agent, in `$HOME`

The full class on an agent where they are absent (`surf009-m-full`, delivered by
`--toolchains=full`):

| path under `$HOME` | KiB | MiB |
|---|---|---|
| `.bunker/toolchains/node` | 170,492 | 166.5 |
| `.bunker/toolchains/rustup` (the 1.89.0 toolchain) + `cargo` | 594,896 | 580.9 |
| **`.bunker/toolchains` total** | **765,392** | **747.5** |
| `$HOME/bin` (dockerd/containerd/ctr proxies — present on *every* agent) + symlinks | 253,612 | 247.7 |

Cross-checked against the daemon's own measured figure, which is independent of `du`:

```
plain agent (no delivery)   Disk Used 248.0 MB      $HOME du 254,088 KiB = 248.1 MiB
the SAME agent, --toolchains=full      981.2 MB      $HOME du 1,019,492 KiB = 995.6 MiB
delta                                    +733.2 MB   delta 765,404 KiB = 747.5 MiB
```

Two independent measures agree within ~2%. **Tradeoff, stated plainly:** the `full`
class costs ~750 MB **per agent** when node and cargo are actually missing (the Rust
toolchain is 78% of that), because `$HOME` is per-agent and nothing dedupes it today.
The `go` class costs **0 extra bytes** where the stock userland already carries
`go`/`make`/`gcc` (§2b), and the same delivery path installs them where it does not.

### 2d. The apt-served image route, on this host

```
$ bunker exec surf009-m-plain 'apt-get -s install --no-install-recommends gcc g++ make | tail -1'
0 upgraded, 0 newly installed, 0 to remove and 64 not upgraded.
```

The bake directive adds nothing where the host already has the compiler family; the
~211 MiB of §2b is what those packages occupy where they exist.

---

## 3. Criterion: spawn latency impact, measured

Every row is a **fresh** `bunker spawn` on bunker-mvp, sequential (no concurrent
spawn), wall-clock around the whole command including the connection bundle. TTL 30m;
each agent was destroyed after the run (§4.7).

| spawn | n | wall (s) | mean |
|---|---|---|---|
| no `--toolchains` (baseline) | 3 | 19.49 / 18.11 / 19.82 | 19.14 |
| `--toolchains=go` (all present → probe only) | 2 | 22.33 / 19.75 | 21.04 |
| `--toolchains=full` (node + cargo delivered) | 1 | 36.28 | 36.28 |

The provisioning overhead isolates cleanly, because one probe is one `exec` round trip
(measured directly, three runs):

```
$ /usr/bin/time -f "wall_s=%e" bunker toolchain-check surf009-m-go --class go
check(class go) run1 wall_s=1.27
check(class go) run2 wall_s=1.26
check(class go) run3 wall_s=1.31
```

* **Already-satisfied class → +1 probe ≈ 1.3 s**, inside the baseline spread (the
  `go`-class samples straddle the baseline band: 18.1–22.3 s vs 18.1–19.8 s). On this
  host `--toolchains=go` therefore provisions *nothing* and only verifies.
* **Real delivery of the missing half → +17.1 s** for ~750 MB (node + rustup
  download + extract) on bunker-mvp's link. That is the worst case measured.
* **Bounded by design, not by luck:** the delivery runs *client-side after* creation,
  so the server spawn path is untouched; one probe plus a per-stage budget
  (`toolchainStageBudget` 600 s/stage, the spawn delivery context 30 min) cap it, and
  a failed delivery is a **warning, not a spawn failure** — the agent is up and the
  same delivery is resumable with `bunker toolchain-check <id> --deliver`.

Verdict: acceptable for both classes on this host — no measurable penalty when the
class is already satisfied, and a cost proportional to what is actually missing (and
one-time per agent, since the stages cache under `$HOME/.bunker/toolchains` so a
re-run is a no-op probe).

---

## 4. Honest notes, deviations and findings

1. **Two dispatches, one row.** The implementation, the proof doc and the tier-2
   judge (`passed: true`, 3/3, verdict at `.gitreins/verdicts/verdict.json`) are the
   **first** dispatch's work; this document is the **second** dispatch's, which
   deliberately wrote no code in the first one's files. The two acceptance lists were
   diffed: everything the first brief asked for is verified above from the landed
   commit; the second brief's *footprint* and *latency* criteria were owned by nobody
   and are measured here (§2, §3).
2. **The premise is host-dependent — again.** §1 shows a fresh vanilla agent on
   bunker-mvp already has `go 1.22.2`, `gcc 13.3.0` and `make 4.3` from the host's own
   userland, so `--toolchains=go` provisions *nothing there*. That does not contradict
   the row: `node`/`cargo` are still absent, and where a host lacks the compilers the
   same stages install them into `$HOME` (the first dispatch proved that half on the
   same agent). The check reporting what is really present is the point.
3. **No cross-agent dedupe.** §2c: node + cargo land in each agent's own `$HOME`, so N
   full-class agents cost N × ~750 MB. A shared read-only cache is not implemented.
4. **The image path still cannot bake node or cargo** (no image-spec directive installs
   a node/rustup *runtime*) — the first dispatch named this gap in
   `docs/provisioning.md`; §2d shows the apt half is bakeable and, on this host,
   already satisfied.
5. **Small samples, one host, one time.** Latency is n=3 / n=2 / n=1 and the full-class
   row is a single observation; report the spread, not a benchmark. Agent-creation cost
   on this host was stable (18.1–19.8 s) so the deltas are credible, but a busier host
   will move the absolute numbers.
6. **The CLI's `Disk Used` is a lagging metric.** Immediately after creation it reads
   `0 B`; the same idle plain agent read `248.0 MB` minutes later (and `surf009-goclass`
   read `1.0 GB`). Use it as a before/after delta, never as an instantaneous size.
7. **Cleanup.** All six measurement agents were destroyed
   (`bunker destroy --archive=false`: `surf009-m-plain`, `-plain2`, `-plain3`, `-m-go`,
   `-m-go2`, `-m-full`), returning ~2.2 GB to bunker-mvp. The only agent left running
   from the SURF-009 work is the first dispatch's `surf009-goclass`.
8. **Finding, unrelated to this row but measured on the way: `jq` is not on a vanilla
   agent.** On a fresh spawn on bunker-mvp, `command -v jq` → `sh: 1: jq: not found`
   and `dpkg-query -W jq` → `no packages found matching jq`, while `git 2.43.0` and the
   docker client are present. README's delivery-path list and
   `imagespec.StockToolchainPackages` both name `jq` as stock, but the stock *host*
   userland it mirrors may not carry it (`StockToolchainPackages` is the image-spec
   bake list, not a guarantee for a vanilla agent). This is a documentation-vs-reality
   gap in the same family as SURF-009 and belongs in its own row, not in this task.
