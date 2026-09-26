# BFS-011 — the 14-operation battery, per protocol, at concurrency 1 and N

**Row:** BFS-011 (P1, complexity 3) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-26
**Status:** complete — the battery runs, and its answer is in §0 and §4.
**Deliverable:** the battery (`probes/webdav-battery/` + `probes/webdav-battery.sh`), this report, and the
raw transcript beside it (`BFS-011-battery-output.txt`) with the machine-readable rows
(`BFS-011-arms.csv`, `BFS-011-ops.csv`).
**Product code changed:** **none.** Everything this row adds is a probe, a driver and documents — see §8
for why that decides the E2E question.

**Why this row exists, in the row's own words.** The release rests on one proven lever: concurrent
independent requests (HTTP/2 at 25× concurrency: **0.79 s** versus **38.47 s** at `MaxConnsPerHost=1`,
`docs/prd/PRD-bunker-fs.md:60–61`). The hypothesis to falsify is therefore that **the HTTP version is a
carrier and concurrency is the actual cause**. A battery that reports a win at concurrency=N *and also*
reports a win at concurrency=1 has measured nothing; one that reports the win disappearing at
concurrency=1 has located the cause. This battery runs the same 14 operations, on the same fixture,
against the same process, over HTTP/1.1, HTTP/2 and HTTP/3, at **concurrency 1** and **concurrency 8** —
and reports whichever of the two happened.

---

## 0. Verdict in one screen

| link | protocol | conns | wall conc=1 | wall conc=8 | whole-tree ratio | verdict |
|---|---|---|---|---|---|---|
| delay20 | HTTP/1.1 | 1 | **11.969 s** | **11.966 s** | **1.0002×** | no degradation — a serialized transport has no pipelining to lose |
| delay20 | HTTP/2 | 1 | **12.231 s** | **2.008 s** | **6.0902×** | **DEGRADED** at concurrency=1 |
| delay20 | HTTP/3 | 1 | **42.902 s** | **39.004 s** | **8.9052×** (walk) / **19.0623×** (fan-out) | **DEGRADED** at concurrency=1 |
| delay20 | HTTP/1.1 | 8 | — | **1.847 s** | **6.4777×** vs the 1-connection arm | concurrency via *connections* wins the same 6.5× |
| loopback | all three | 1 | 0.166–0.213 s | 0.103–0.132 s | 1.30–3.59× | the lever is RTT-borne; sub-millisecond requests leave it nothing to recover |

**The hypothesis survives this falsification attempt, on this fixture and these links.** Concretely:

1. **The battery MUST degrade at concurrency=1 — and it does, on every multiplexed protocol.** With one
   connection and a 20 ms-per-direction link, HTTP/2's whole-tree walk goes from **11 257 ms** (concurrency=1)
   to **1 454 ms** (concurrency=8) — **7.7449×** — and its 100-request fan-out from **4 431 ms** to **586 ms**
   — **7.5677×**. HTTP/3 shows **8.9052×** on the walk and **19.0623×** on the fan-out. Both clear
   AC-11's ≥2× threshold on the whole-tree operations, which is the criterion the PRD writes down
   (`PRD-bunker-fs.md:136`).
2. **The control behaves like a control.** HTTP/1.1 on ONE connection is **1.0002×** — 11.969 s versus
   11.966 s. That is what a transport that cannot pipeline is supposed to look like, and it is the
   evidence that the battery's concurrency knob does something: two arms that differ only in the
   in-flight bound are identical precisely when the transport serializes.
3. **The carrier test says "concurrency is the cause", with the version as the carrier.** HTTP/1.1 with
   **8 connections** at the same concurrency reaches **1.847 s** — **6.4777×** faster than the
   one-connection arm (**7.6263×** on the walk). So the HTTP version does not own the win: a version that
   cannot multiplex reaches the same place by opening more connections. What the version decides is
   whether you can get there **on one connection** — which is what the release's 0.79 s figure was
   measured with (`MaxConnsPerHost=1`), and what a filesystem issuing hundreds of independent metadata
   calls actually needs.
4. **The loopback arms are the honest floor, not a contradiction.** With no round trip the mean cost of
   an independent request is **0.155–0.538 ms**, so there is almost nothing for concurrency to recover
   (totals 1.08–1.85×). The lever the study measured is *requests waiting on a round trip* — the study's
   own figure is 7.88 ms per request, 2.4 % of one 185 ms RTT — so a link with no RTT cannot show it.
   The report prints those milliseconds instead of leaving the reader to guess why.

**One number needs a caveat rather than a headline:** HTTP/3's *battery total* on the delayed link is only
**1.0999×** even though both graded whole-tree operations degrade 8.9× and 19.1×. Its large-body cells
(`read-large`, `write-large`) cost 10–28 s there, because the delay relay's per-datagram delay acts as a
throughput ceiling for multi-megabyte transfers over QUIC. That is an instrument property, named in §6.4,
and the reason the verdict is graded on the whole-tree operations rather than on the total.

---

## 1. The op list — 14 required, 1 extra, both stated

The row names fourteen operations. They are the fourteen the battery runs, one cell each:

| # | cell | request(s) | what it is |
|---|---|---|---|
| 1 | `stat` | `HEAD` | the mount's `getattr`: identity without transferring bytes |
| 2 | `read-small` | `GET` | a 1 KiB file |
| 3 | `read-large` | `GET` | the 4 MiB file (the study's own write granularity, BFS-003 Appendix A.6) |
| 4 | `write-small` | `PUT` | a 1 KiB file, verified on disk and by read-back |
| 5 | `write-large` | `PUT` | the 4 MiB file, verified on disk and by read-back |
| 6 | `list-shallow` | `PROPFIND Depth: 1` | one collection, with the identity properties named |
| 7 | `list-deep` | `PROPFIND Depth: 1` | three levels down |
| 8 | `create` | `PUT If-None-Match: *` | create-only |
| 9 | `rename` | `MOVE` | source gone, destination present |
| 10 | `delete` | `DELETE` | 404 on read-back |
| 11 | `mkdir-rmdir` | `MKCOL` + `DELETE` | collection created and removed, 404 on read-back |
| 12 | `walk-tree` | 1 × `PROPFIND Depth: 1` per directory + 1 × `HEAD` per file | **the whole-tree walk** — the READDIR + GETATTR storm, the class every baseline stalls on |
| 13 | `git-status-shaped` | 1 × `POST X-Bunker-Op: snapshot` (`depth=infinity`, `include_hash`) | the whole tree's state in **one** delegated request |
| 14 | `conflict` | `HEAD` → `PUT` (writer B) → `PUT If-Match: <stale>` | **the stale-base write, which must be REFUSED** |

**One extra arm, declared rather than smuggled: `fanout-100`.** One hundred independent `GET`s issued
concurrently, bounded only by the arm's in-flight limit. It is the shape the PRD's own A/B used to measure
the lever ("100 sequential 19.66 s / 100 concurrent 0.79 s", `PRD-bunker-fs.md:60–61`) and the shape the
release's "multiplexing replay" criterion asks a re-run to reproduce (`:267`). It is reported as an extra
arm, never folded into the 14-op table.

**Deviations the battery respects rather than trips.** The walk recurses with `Depth: 1` and never sends
`Depth: infinity`, which this surface refuses by design (BFS-004 §2.3 deviation 3 / decision D2); `GET` is
never sent to a collection (deviation 1); `MKCOL` carries no body; a collection `DELETE` carries no `Depth`.
Each of those refusals is asserted in the same run as a contract note (§4.5) — a battery that measured a
surface other than the one the spec describes would be worse than no battery.

**The `git-status-shaped` cell, stated precisely.** The row asks for a `git status`-shaped op. In *this*
build `X-Bunker-Op: status` is not implemented: it answers the structured `501 capability_unavailable`
(`scope=build, phase=C5`) that R3 requires — asserted as a note, verbatim, in every arm. What exists, and
what the cell therefore measures, is the delegated whole-tree read (`snapshot`, `depth=infinity`,
`include_hash`), which is the C5 delegation shape the PRD says makes a status-class operation affordable
(`:23–31`). Both facts are in the report; neither is smoothed over.

---

## 2. Protocol is observed, never assumed

Every request the battery issues records **three** independent observations of the version actually used:

- **`client_proto`** — Go's `resp.Proto`, what the client's own stack received;
- **`server_proto`** — the `X-Bunker-Proto` header, which this surface sets from the server's own
  `r.Proto` (BFS-004 §3 E-3, §4.3 C-6) — i.e. the anti-gaming source PROTO-014 used, on the server side;
- **the ALPN** the TLS or QUIC handshake negotiated.

A cell whose observations disagree with the arm's protocol, or that reports no protocol at all, is marked
**UNRELIABLE** and fails the arm — it is never counted as a pass and never contributes a number silently.
The run below recorded **14/14 arms MATCH, 0 unreliable cells**:

```
== protocol observed vs claimed (read from resp.Proto and X-Bunker-Proto) ==
  arm               claimed   client saw  server saw  verdict
  ----------------  --------  ----------  ----------  -------
  h1-delay20-c1x1   HTTP/1.1  HTTP/1.1    HTTP/1.1    MATCH
  h1-delay20-c1x8   HTTP/1.1  HTTP/1.1    HTTP/1.1    MATCH
  h1-delay20-c8x8   HTTP/1.1  HTTP/1.1    HTTP/1.1    MATCH
  h2-delay20-c1x1   HTTP/2.0  HTTP/2.0    HTTP/2.0    MATCH
  h2-delay20-c1x8   HTTP/2.0  HTTP/2.0    HTTP/2.0    MATCH
  h3-delay20-c1x1   HTTP/3.0  HTTP/3.0    HTTP/3.0    MATCH
  h3-delay20-c1x8   HTTP/3.0  HTTP/3.0    HTTP/3.0    MATCH
  h1-loopback-c1x1  HTTP/1.1  HTTP/1.1    HTTP/1.1    MATCH
  h1-loopback-c1x8  HTTP/1.1  HTTP/1.1    HTTP/1.1    MATCH
  h1-loopback-c8x8  HTTP/1.1  HTTP/1.1    HTTP/1.1    MATCH
  h2-loopback-c1x1  HTTP/2.0  HTTP/2.0    HTTP/2.0    MATCH
  h2-loopback-c1x8  HTTP/2.0  HTTP/2.0    HTTP/2.0    MATCH
  h3-loopback-c1x1  HTTP/3.0  HTTP/3.0    HTTP/3.0    MATCH
  h3-loopback-c1x8  HTTP/3.0  HTTP/3.0    HTTP/3.0    MATCH
```

**The control that makes the observation mean something**: the same URL, on the same daemon, driven by the
same instrument with a different transport, reports `HTTP/1.1` **and** `HTTP/2.0` — because the instrument
reads the version off the response, not off its own configuration. That is also why the h1.1 arms are
labeled as what they are: the client *asks* for `http/1.1`, and what is asserted is that the **server
observed** `HTTP/1.1` on every response (`X-Bunker-Proto`). A downgrade or an upgrade would show up as a
mismatch, not as a quiet number.

---

## 3. Method

**The run record, verbatim from the transcript:**

```
BFS-011 per-protocol WebDAV battery
date            : 2026-09-26T09:45:58-05:00
host            : karaHermes-mde-7840hs 7.0.0-31-generic x86_64
cpus            : 16
loadavg (start) : 16.31 11.92 11.17
go              : go version go1.26.5 linux/amd64
bunkerd commit  : 2567052b9aad33140924f1cde19d0026d3a99408
bunkerd sha256  : c11524ceb8eff8ffdc43f9d9d546cc1b41d4922f6a63ca493baa01b0b50471e6
fixture         : 6 dirs x 25 files + a 4 MiB file + a deep chain
results dir     : /tmp/bfs011-final (fresh: the previous run's rows are not mixed in)
daemon          : TLS + HTTP/3 on ONE port number (37410), webdav_root=…
links           : loopback,delay20 (delay = 20ms per direction on the relay link)
concurrency     : 1 and 8; fan-out arm = 100 requests
op timeout      : 45s (a cell past it is reported as a STALL)
```

**One fixture, regenerated before every arm.** Fixture A of the study (`PRD-bunker-fs.md:126`: 150 files /
6 dirs) plus exactly what the op list needs to be measurable: a 4 MiB file, a deep chain
(`d0/nested/deeper`), an empty collection as the parent for the create/mkdir/rename/delete cells, and a
dedicated conflict file. Content is deterministic (a hash chain keyed on the path), so regenerating the
fixture gives a byte-identical tree: two arms that started from different trees would not be comparable at
all. The battery **scans** the served tree to build its expectations instead of trusting the generator —
the assertions compare what the server reveals with what is on disk.

**One process, one port, three protocols.** The daemon is started once with `tls.enabled: true` and
`server.h3_enabled: true`, `rest_addr` empty, so the TCP listener (ALPN `h2` / `http/1.1`) and the QUIC
listener sit on the **same port number**; the driver asserts a UDP socket exists on it before running a
single h3 arm. Every arm therefore measures the same process, the same tree and the same handler.

**Two links.** `loopback` (no added latency — the floor) and `delay20`: a userspace **delay relay** that
forwards every chunk of every connection after 20 ms, TCP and UDP, on its own port. It exists because the
lever under test is RTT-borne and loopback cannot show it; it uses no root and touches no host setting (a
`tc netem` on a box shared with sibling agents would change every other session's measurements). It adds
≈40 ms per round trip and additionally rate-limits multi-megabyte bodies — which is why the graded
measures are the small-message whole-tree operations (§6.4).

**Arm matrix (14 arms):** for each link, `h1.1/h2/h3 × {conc=1, conc=8}` plus, on HTTP/1.1, a
`conns=8, conc=8` arm. The connection pool is the *only* difference between the h1.1 arms: `conc=8` with
`MaxConnsPerHost=1` cannot pipeline (that is the point), and the 8-connection arm is the carrier test.
QUIC serves every h3 arm on one connection, which is what QUIC is for.

**What "concurrency" means here.** One semaphore per arm bounds *every* request in flight — the cells'
own requests and their verification requests alike — so at concurrency=1 exactly one request is ever in
flight in the whole battery. Cells are dispatched per stage (reads first, against an untouched tree; then
the independent mutations; then the create → rename → delete chain) and cells inside a stage run
concurrently, bounded by that same semaphore.

**What is timed.** `work_ms` = the sum of a cell's own request durations (its cost); `wall_ms` = the
cell's elapsed time including waiting for the in-flight bound (what it cost in that run). The battery's
total is the sum of its stage walls. Verification requests are untimed, so a read-back can never flatter a
write.

**Contention is recorded, not corrected.** The row warns that contention has already corrupted one set of
numbers in this study, so every arm records `/proc/loadavg` before and after and carries a `contended`
flag when the load average reaches the core count. On this run the host was busy: **loadavg 16.31 on 16
cpus**, and 9 of 14 arms are labelled `CONTENDED` in the table below. The delayed arms are
latency-dominated (≈44 ms per request through the relay against sub-millisecond CPU work), and the
loopback arms are the ones contention can distort — which is exactly where the report claims nothing.

---

## 4. Results (raw output, verbatim)

### 4.1 The arm table

```
== arms ==
  arm               proto  link      conn  conc  wall_s  cells_ok  stalls  unreliable  notes_fail  contended  arm
  ----------------  -----  --------  ----  ----  ------  --------  ------  ----------  ----------  ---------  ----
  h1-delay20-c1x1   h1     delay20   1     1     11.969  15/15     0       0           0           true       PASS
  h1-delay20-c1x8   h1     delay20   1     8     11.966  15/15     0       0           0           false      PASS
  h1-delay20-c8x8   h1     delay20   8     8     1.847   15/15     0       0           0           false      PASS
  h2-delay20-c1x1   h2     delay20   1     1     12.231  15/15     0       0           0           false      PASS
  h2-delay20-c1x8   h2     delay20   1     8     2.008   15/15     0       0           0           false      PASS
  h3-delay20-c1x1   h3     delay20   1     1     42.902  15/15     0       0           0           false      PASS
  h3-delay20-c1x8   h3     delay20   1     8     39.004  15/15     0       0           0           false      PASS
  h1-loopback-c1x1  h1     loopback  1     1     0.166   15/15     0       0           0           true       PASS
  h1-loopback-c1x8  h1     loopback  1     8     0.154   15/15     0       0           0           true       PASS
  h1-loopback-c8x8  h1     loopback  8     8     0.117   15/15     0       0           0           true       PASS
  h2-loopback-c1x1  h2     loopback  1     1     0.191   15/15     0       0           0           true       PASS
  h2-loopback-c1x8  h2     loopback  1     8     0.103   15/15     0       0           0           true       PASS
  h3-loopback-c1x1  h3     loopback  1     1     0.213   15/15     0       0           0           true       PASS
  h3-loopback-c1x8  h3     loopback  1     8     0.132   15/15     0       0           0           true       PASS
```

`15/15` cells per arm = the 14 required ops + `fanout-100`; `stalls 0`, `unreliable 0`, `notes_fail 0`
everywhere. Every one of the 210 cells' individual status, protocol pair and work/wall time is in
`BFS-011-ops.csv`; the per-request record (3 671 requests) is written by the battery to
`requests.csv` in the results directory, which the driver prints the path of.

### 4.2 Per-operation wall times, delayed link (20 ms per direction)

```
== wall_ms — wall_ms per operation (link=delay20) ==
  op                   h1 x1 c1   h1 x1 c8   h1 x8 c8  h2 x1 c1   h2 x1 c8  h3 x1 c1   h3 x1 c8
  -------------------  ---------  ---------  --------  ---------  --------  ---------  ---------
  stat                 959.121    123.245    82.971    1034.116   41.433    41.407     42.303
  read-small           918.209    204.674    40.983    952.171    41.481    352.220    42.307
  read-large           184.616    300.800    101.347   2137.006   402.864   10193.233  10252.721
  list-shallow         42.424     83.079     84.343    43.251     126.609   10229.875  52.046
  list-deep            128.708    335.632    84.283    534.045    41.905    82.907     42.511
  walk-tree            11178.916  11184.980  1466.632  11257.278  1453.501  21001.851  2358.370
  git-status-shaped    633.165    2258.734   90.298    1980.364   216.719   144.600    381.221
  fanout-100           4389.883   4379.140   620.415   4431.272   585.548   14136.862  741.612
  write-small          329.656    364.413    86.689    496.287    90.264    9377.176   124.466
  write-large          464.726    457.741    152.880   646.964    349.367   21572.479  28542.664
  create               370.569    405.197    86.740    536.177    90.179    9418.082   124.433
  mkdir-rmdir          544.189    535.325    123.790   726.679    125.405   21652.550  145.486
  conflict             585.126    576.004    176.029   768.745    236.790   21693.791  249.945
  rename               122.276    122.304    122.365   122.883    123.216   123.253    124.208
  delete               81.521     81.743     81.628    81.766     81.787    81.773     83.686
  TOTAL(battery wall)  11968.606  11965.832  1847.224  12231.276  2008.348  42901.562  39003.673
```

Reading it: `h1 x1 c1` and `h1 x1 c8` are the same run twice (one connection cannot pipeline); `h1 x8 c8`
is the same work over 8 connections; `h2 x1 c8` is the same work multiplexed on one connection. The
whole-tree ops respond exactly as the thesis says they should.

### 4.3 Per-operation wall times, loopback (no added latency)

```
== wall_ms — wall_ms per operation (link=loopback) ==
  op                   h1 x1 c1  h1 x1 c8  h1 x8 c8  h2 x1 c1  h2 x1 c8  h3 x1 c1  h3 x1 c8
  -------------------  --------  --------  --------  --------  --------  --------  --------
  stat                 0.409     0.430     4.115     0.600     0.730     0.581     0.716
  read-small           22.461    1.065     0.744     1.102     0.631     1.731     0.797
  read-large           23.258    20.615    32.518    24.519    27.657    23.445    28.996
  list-shallow         16.387    16.196    9.048     38.751    2.009     20.288    5.160
  list-deep            16.527    5.517     3.719     18.570    1.174     21.741    0.876
  walk-tree            86.107    66.374    34.070    104.602   29.141    104.820   33.216
  git-status-shaped    46.266    23.287    22.622    34.644    12.430    33.667    11.637
  fanout-100           51.285    37.629    23.970    52.873    15.486    53.816    22.587
  write-small          21.995    21.567    5.233     23.483    5.693     29.784    8.373
  write-large          77.204    85.472    80.162    84.016    71.664    105.457   96.922
  create               22.388    21.334    5.069     24.245    8.801     29.951    7.873
  mkdir-rmdir          22.529    21.982    0.662     24.977    1.003     85.279    1.829
  conflict             23.088    22.150    9.783     25.322    7.996     85.788    8.671
  rename               0.965     0.973     1.089     1.108     1.110     1.523     1.168
  delete               0.432     0.441     0.842     0.557     0.455     0.635     0.463
  TOTAL(battery wall)  165.576   153.893   117.407   190.816   102.951   213.090   132.135
```

### 4.4 The degradation analysis (report output, verbatim)

```
== the row's MUST: does concurrency=1 degrade? ==
   link=delay20 proto=h1   conc=1 arm "h1-delay20-c1x1" vs conc=8 arm "h1-delay20-c1x8" (conns 1)
  measure                      conc=1     conc=8     ratio
  ---------------------------  ---------  ---------  --------------------
  battery total wall *         11.969     11.966     1.0002x not-degraded
  walk-tree (wall)             11178.916  11184.980  0.9995x not-degraded
  fanout-100 (wall)            4389.883   4379.140   1.0025x not-degraded
  git-status-shaped (wall) *   633.165    2258.734   0.2803x not-degraded
  fanout-100 mean per request  43.899     43.791     -

   link=delay20 proto=h2   conc=1 arm "h2-delay20-c1x1" vs conc=8 arm "h2-delay20-c1x8" (conns 1)
  measure                      conc=1     conc=8    ratio
  ---------------------------  ---------  --------  ----------------
  battery total wall *         12.231     2.008     6.0902x DEGRADED
  walk-tree (wall)             11257.278  1453.501  7.7449x DEGRADED
  fanout-100 (wall)            4431.272   585.548   7.5677x DEGRADED
  git-status-shaped (wall) *   1980.364   216.719   9.1379x DEGRADED
  fanout-100 mean per request  44.313     5.855     -

   link=delay20 proto=h3   conc=1 arm "h3-delay20-c1x1" vs conc=8 arm "h3-delay20-c1x8" (conns 1)
  measure                      conc=1     conc=8     ratio
  ---------------------------  ---------  ---------  --------------------
  battery total wall *         42.902     39.004     1.0999x not-degraded
  walk-tree (wall)             21001.851  2358.370  8.9052x DEGRADED
  fanout-100 (wall)            14136.862  741.612   19.0623x DEGRADED
  git-status-shaped (wall) *   144.600    381.221   0.3793x not-degraded
  fanout-100 mean per request  141.369    7.416     -

   CARRIER TEST (link=delay20): HTTP/1.1 with 1 connection(s) vs 8, same concurrency=8
  measure               conns=1    conns=8   ratio
  --------------------  ---------  --------  ----------------
  battery total wall *  11.966     1.847     6.4777x DEGRADED
  walk-tree (wall)      11184.980  1466.632  7.6263x DEGRADED

   link=loopback proto=h1 / h2 / h3   (conc=1 vs conc=8, one connection)
  walk-tree (wall)             86.107 / 104.602 / 104.820  ->  66.374 / 29.141 / 33.216
  ratio                        1.2973x / 3.5895x / 3.1557x
  fanout-100 mean per request  0.513 / 0.529 / 0.538 ms   ->  0.376 / 0.155 / 0.226 ms
     ^ this link has effectively no round trip, so the RTT-borne part of the lever cannot appear here.

== verdict ==
   delay20      h1: NOT DEGRADED at concurrency=1 on any whole-tree measure — FINDING
   delay20      h2: DEGRADED at concurrency=1 on every whole-tree measure (walk-tree (wall), fanout-100 (wall))
   delay20      h3: DEGRADED at concurrency=1 on every whole-tree measure (walk-tree (wall), fanout-100 (wall))
   loopback     h1: NOT DEGRADED at concurrency=1 on any whole-tree measure — FINDING
   loopback     h2: PARTIAL — degraded on walk-tree (wall); not degraded on fanout-100 (wall) — FINDING
   loopback     h3: PARTIAL — degraded on walk-tree (wall); not degraded on fanout-100 (wall) — FINDING

== failures, stalls and asymmetries ==
   CONTENDED: h1-delay20-c1x1 ran at loadavg 16.31 -> 15.18 on 16 cpus — the number is labelled, not corrected
   CONTENDED: h1-loopback-c1x1 ran at loadavg 16.31 -> 16.31 on 16 cpus — …
   (9 arms labelled CONTENDED; see the arm table)
   none: no failing cell, no stall, no unreliable protocol observation, no asymmetry

report: every arm passed its own cells and every recorded protocol matches the arm's claim
```

**The `h1` "NOT DEGRADED" line is not a finding against the thesis, it is the control.** A one-connection
HTTP/1.1 client has no pipelining: its concurrency=1 arm and its concurrency=8 arm are the same
11.97 s run. The line that separates the two readings is the **CARRIER TEST** directly above it: HTTP/1.1
with 8 connections at the *same* concurrency is **6.4777×** faster. Concurrency is the cause; the version
decides how you get it.

**The loopback "PARTIAL" lines are the link, not the protocol** — the report prints the
milliseconds-per-request behind them (0.15–0.54 ms) for exactly this reason.

### 4.5 What every arm also asserted (contract notes, untimed)

Per arm, printed in the transcript: `OPTIONS` advertises `DAV: 1` + `Allow` + `X-Bunker-Extensions`;
`PROPFIND Depth: infinity` ⇒ `403 propfind_finite_depth` (deviation 3 — the walk recursed instead);
`GET` on a collection ⇒ `405` + `Allow` (deviation 1); `X-Bunker-Op: status` ⇒ structured
`501 capability_unavailable` (what this build's git-status-shaped cell is *not*); `X-Bunker-Op: watch` ⇒
`501` with `mode=poll`; an op outside the catalogue ⇒ `400 op_unknown`. 6 notes × 14 arms, 84/84 passed.

### 4.6 The conflict cell, per the row's requirement

`conflict` runs `HEAD` (reader A takes the base hash) → `PUT` of different bytes (writer B moves the
file) → `PUT If-Match: <A's stale base>`. Every arm reports `200,204,412` in that order, and the cell
additionally asserts, before it can pass: the refusal is `412` with `X-Bunker-Verdict: hash_mismatch`; the
`X-Bunker-Current-Hash` equals writer B's bytes and `X-Bunker-Expected-Hash` equals the stale base; the
body carries both `<b:current>` and `<b:expected>`; the file on disk **still holds writer B's bytes** and
not writer A's; and the touch-without-change arm of the same rule (`If-Match` stale + *identical* bytes)
answers `204 identical_content` rather than a second refusal. A 412 without the hash pair, or a 412 that
still wrote, fails the cell.

---

## 5. Side by side with the recorded baselines

The baselines the row names are **mount-level** wall times on a ≥180 ms link, recorded before this
surface existed. This battery measures the **wire** (no FUSE client has landed — BFS-008/009 own it), so
the comparison is of *kind*, not of absolute seconds; the driver prints both together so nobody has to
remember that:

| recorded baseline (`PRD-bunker-fs.md:23–31, :84–93`) | this battery |
|---|---|
| sshfs: **5 / 14 ops, 7 stalls** (dedi-2) / **8** (bunker-mvp) | 15/15 cells, **0 stalls**, every protocol, every arm |
| NFSv4: 11 / 14, whole-tree ops **33–36 s** (native: 0.41 s) | whole-tree walk over HTTP/2 on one connection: **1.45 s** for 167 requests at 40 ms RTT |
| rclone WebDAV (no h2, 4 connections): 11 / 14, `diff --stat` still stalls at 45.09 s | HTTP/1.1 with 8 connections: **1.85 s** whole battery — the same win without h2 |
| native (git on the agent): 14 / 14, **0.41 s** flat | the delegated single-request cell (`snapshot`): **0.15–2.26 s** to describe the whole tree in one request |
| h2probe A/B: HTTP/1.1 one connection **38.47 s** / HTTP/2 one connection **0.79 s** (25.0×) at 185 ms RTT | HTTP/1.1 one connection **11.97 s** → HTTP/2 one connection **2.01 s** (6.09×) at 40 ms RTT on 167 requests |

**The 25.0× is not re-derived here and this report does not claim it.** The study's figure is 100 requests
against a 185 ms RTT; this battery's links are loopback (no RTT) and a 20 ms-per-direction relay, on a
host under loadavg 16, with a whole-tree walk in the same stage competing for the same in-flight budget.
What the battery does deliver is the thing the 25.0× was a *measurement of*: the concurrency=1 arm
degrading, per protocol, with the protocol observed from the response — and the fan-out arm's
**5.855 ms per request** at concurrency=8 over HTTP/2 sits in the same order of magnitude as the study's
7.88 ms/request, with the same serialized counterpart (**44.313 ms**) measured in the same run.

---

## 6. Instrument findings (what this row had to fix to be able to report honestly)

These are recorded because each of them could have produced a **false** answer, and two did until fixed.
They are the row's own "do not tune the battery until it produces the degradation the thesis predicts"
rule, applied to the instrument rather than to the result.

**6.1 A sequential fan-out arm cannot see concurrency.** The first version issued its 100 requests in a
loop. The semaphore then never had a second request to admit, so `fanout-100` reported ~44 ms per request
at *every* concurrency — the "100 sequential" arm of the PRD's A/B, measured four times. The fix is
goroutines bounded by the in-flight limit; the arm now reports 44.313 ms/request at concurrency=1 and
5.855 ms at concurrency=8, which is the comparison the PRD's A/B was making.

**6.2 The delay relay serialized bursts — and that alone flipped the answer for HTTP/2.** The relay's
first two versions slept `delay` *inside* the write loop, so N requests that arrived together on one
connection left N × 20 ms apart: the instrument capped concurrency at 1 no matter what the client did, per
connection. The battery's own numbers caught it — the delayed HTTP/2 arm's fan-out ratio came out
**1.122×** (17 481.9 ms at concurrency=1 versus 15 577.1 ms at concurrency=8: two nearly identical
serialized runs) while HTTP/3, whose relay path delays each datagram in its own goroutine, showed
**18.66×**. A one-connection diagnostic probe (40 requests, `MaxConnsPerHost=1`, direct vs through the
relay) attributed it to the relay: HTTP/2 at concurrency=1 took 1.686 s against 0.254 s at concurrency=8
only after the fix, and HTTP/1.1 on the same relay stayed 1.675 s at concurrency=8 on one connection while
its 8-connection arm took 0.250 s — i.e. the relay, not the client, was the bottleneck. The fix stamps
each chunk with its **arrival** time and delays from that, keeping only an ordering guarantee. After it,
the same arm reports **44.313 ms → 5.855 ms per request (7.5677×)**. Without this fix the battery would
have reported *the release's central premise as false* on HTTP/2 — the protocol the premise was measured
on — because of an accident in a test harness. **This is the single most important line in this
report's method section.**

**6.3 A results directory must hold one run.** The per-arm CSVs are appended to by design, so re-using a
directory after an interrupted run made the report read *two* runs as one (it listed "17 arms", including
an arm whose connection-refused errors named the dead run's port). The driver now truncates the three
CSVs at the start of every run and says so in the transcript.

**6.4 The relay is a latency adder, not a link emulator.** Two honest limits: (a) it delays every chunk
per direction, so a multi-megabyte body pays one delay per ≤1 MiB chunk — which is why `read-large` and
`write-large` over h3 cost 10–28 s on the delayed link and why the **battery total** for h3 shows only
1.0999× while its graded whole-tree operations show 8.9× and 19.1×; (b) it delays per datagram on the UDP
side, where QUIC spends more round trips per request than TCP does, so h3's absolute times on that link
are not comparable with h1/h2's. Neither affects the concurrency *ratio* on the graded operations, which
is measured inside one link and one protocol.

**6.5 One flaky shutdown, seen once, not chased.** During an earlier interrupted run the daemon did not
exit on `SIGTERM` while the QUIC listener had served h3 traffic. The driver now terminates its background
processes with a bounded wait (and prints a `SIGKILL` notice when it has to), so a stuck shutdown cannot
hold the battery past its own timeout. It is recorded here as an observation about `bunkerd`'s shutdown
path, not diagnosed by this row.

---

## 7. What this does NOT prove

- **No mount.** There is no FUSE client in this build (BFS-008/009), so this is **not** AC-1's or AC-2's
  "14-operation battery through the mount" replay in `PRD-bunker-fs.md:126–127`, and it does not put
  numbers next to the sshfs 5/14 baselines as if it had mounted anything. It measures the wire the mount
  will ride on.
- **No 185 ms link.** The delayed link adds 20 ms per direction (≈40 ms RTT), ~1/4.6 of the study's
  185.24 ms RTT, deliberately: a serial whole-tree walk at the study's latency would be 167 × 185 ms =
  **31 s per arm**, and 14 such arms would exceed any sane run. Absolute times scale with RTT; the
  degradation *ratios* are the transferable part, and the study's own figures let a reader scale them.
- **h3's absolute numbers on the delayed link** carry the relay artifact of §6.4. Its ratio is measured.
- **The conflict cell proves the write path refuses at the wire**, with the on-disk byte-identity check.
  It does not prove the *client's* conflict loop (BFS-005/BFS-008 own that).
- **One host, one fixture size, one run per arm.** No Windows/macOS client, no cross-DC link, no
  repetition statistics: each arm is a single run, and the two nearly identical h1.1 arms
  (11.969 s / 11.966 s) are the only repeatability evidence in the set.
- **It does not test the release's old-client or compatibility claims** (curl/rclone stock clients) —
  BFS-012's row and `probes/webdav-h1h2-probe.sh` own those.

---

## 8. Is this in the live-server E2E class? A judgement, with its reason

**No, and a LOCAL run is sufficient.** The rule (`AGENTS.md`; the row's own conventions) applies the live
`bunker-mvp` battery to changes that touch **spawn / destroy / exec / docker / SSH** behaviour. This row
changes **no product code at all**: it adds a probe, a driver, and documents. Its subject is a daemon the
harness starts itself on loopback, on ports it picks, over a fixture it generates, with a token it mints
at run time; nothing in the agent lifecycle, no remote host and no credential store is involved. Binding
loopback is not a shortcut here — it is the correct instrument: the measured variable is the protocol's
behaviour under concurrency, which is fully observable on loopback, and the one thing loopback cannot
show (RTT-borne magnitude) is supplied by the delay relay rather than by moving the test to a remote box
whose link and load this row cannot control or re-measure.

**What a remote run *would* add, and who should do it:** the same battery on `bunker-mvp` (and dedi-2)
against a real ≥180 ms link, with the FUSE client mounted, is the replay of AC-1/AC-2 that BFS-008/009/012
should run once a client exists. The battery is written to be pointed at any URL (`-url`, `-proto`,
`-conns`, `-concurrency`), so that run is one command per arm rather than a new instrument.

---

## 9. Reproducing this in one command

```
probes/webdav-battery.sh
```

That builds `bunkerd` and the battery from the checkout, generates Fixture A, starts one TLS+HTTP/3 daemon
on one port number, starts the delay relay, runs all 14 arms (the fixture is regenerated before each), and
prints the tables above followed by the verdict. Options: `--links loopback,delay20 --conc 8 --fanout 100
--delay 20 --op-timeout 45 --out DIR --binary PATH [--keep]`. Exit is 0 when every arm's cells and
contract notes pass, 1 when any arm fails, 2 on a usage/infrastructure error — and a run that finds **no**
degradation still exits 0, because that would be a finding about the release, not a broken battery.

Raw artifacts committed beside this document:

| file | what it is |
|---|---|
| `BFS-011-battery-output.txt` | the complete driver transcript (881 lines): run record, every arm's cell table with statuses and observed protocols, both per-op matrices, the degradation analysis, the verdict |
| `BFS-011-arms.csv` | one row per arm (14): protocol claimed/observed, wall times, cells ok, stalls, unreliable, notes failed, contended, exit |
| `BFS-011-ops.csv` | one row per cell per arm (210): requests, verify requests, work/wall ms, statuses, client/server protocol, ok, class, detail |
| `requests.csv` (in the run's results dir) | one row per HTTP request (3 398): method, path, status, client/server protocol, ALPN, verdict, duration, bytes |

---

## 10. REMAINING

1. **The 185 ms-link replay with a real mount** — this battery's numbers are wire-level, on loopback and a
   40 ms-RTT relay. The AC-1/AC-2 mount replay on `bunker-mvp`/dedi-2 belongs to BFS-008/009/012 once a
   FUSE client exists; the instrument is ready for it (`-url` points anywhere).
2. **Single-run arms.** No repetition or variance study: each arm ran once. The h1.1 pair (11.969 s /
   11.966 s) is the only repeatability evidence, and the host's loadavg (16.31/16) means the loopback arms
   in particular are labelled rather than trusted.
3. **h3 absolute magnitudes on the delayed link** are relay-inflated (§6.4); only its ratios are claimed.
4. **`X-Bunker-Op: status` is not in this build** (structured `501`, slice C5), so the git-status-shaped
   cell measures the delegated `snapshot` path. When C5 lands, A-12's cell and this one should be run
   together.
5. **The daemon's shutdown-after-h3 observation** (§6.5) is recorded, not diagnosed — a candidate row for
   the BFS-007 area if the agent lifecycle ever depends on a clean `SIGTERM`.
6. **The board row is not closed by this commit.** `gitreins task complete` runs from the main tree (a
   worktree cannot see `tasks.yaml`), so the foreman closes BFS-011 after the merge, as usual.
