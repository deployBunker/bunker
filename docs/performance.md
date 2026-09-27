# Performance on a long path: the socket buffer is a ceiling, and HTTP/2 walks straight into it

**Authored 2026-09-27.** Everything below is either a **measured** number (with the command that produced it)
or **arithmetic on measured numbers** — and each one says which. The distinction is not decoration here:
the arithmetic is a *ceiling*, and this page would be false if it presented it as a result.

**Rows:** `BFS-055` (lift the ceiling, and measure it) · `BFS-057` (the harness every lever is judged by) ·
`BFS-059` (the cheap tier-1 levers). Design detail: [`prd/SPEC-linux-io-max.md`](prd/SPEC-linux-io-max.md).

---

## 0. The one-screen answer

| Layer | What is capped | The number |
|---|---|---|
| **One TCP connection's send buffer** | the *bytes* one connection can keep in flight, and therefore its throughput on a long path | **4 MiB** ceiling → **~21.8 MB/s (~175 Mbps)** at a 192 ms RTT — arithmetic |
| **HTTP/2** | every stream shares that one connection, so the whole session inherits the per-connection number | one 4 MiB buffer for N concurrent streams |
| **HTTP/3 / QUIC** | its UDP socket buffers are *set explicitly*, and both DC hosts cap those at **208 KiB** | **~1.1 MB/s (~8.9 Mbps)** per socket — arithmetic, **capped by `net.core.{r,w}mem_max`**, not by `tcp_wmem` |
| **Concurrency** | buys *independent requests in flight*, not bandwidth beyond the connection's byte ceiling | measured 7.7449× on a walk, 25.0× per request — both RTT-born, both small-payload |

**If you take one thing away:** on a long path, *concurrency is the lever for requests and the socket buffer
is the ceiling for bytes.* They are different problems with different fixes, and a mount can be excellent at
the first while being stuck on the second.

---

## 1. The measured inputs

Measured with `sysctl -n <key>` on 2026-09-27, on the **control host** (this box) and on the two DC hosts
the client is measured against. Read them all; three of them are not what you would assume.

| key | control host (kernel 7.0.0-31) | dedi-2, Helsinki (6.8.0-90) | bunker-mvp, Falkenstein (6.8.0-117) |
|---|---|---|---|
| `net.ipv4.tcp_wmem` | `4096 16384 4194304` | `4096 16384 4194304` | `4096 16384 4194304` |
| `net.core.wmem_max` | `4194304` | **`212992`** | **`212992`** |
| `net.ipv4.tcp_rmem` | `4096 131072 33554432` | `4096 131072 6291456` | `4096 131072 6291456` |
| `net.core.rmem_max` | `4194304` | **`212992`** | **`212992`** |
| `net.ipv4.tcp_notsent_lowat` | `4294967295` (disabled) | same | same |
| `net.ipv4.tcp_congestion_control` | `cubic` | `cubic` | `cubic` |
| `net.core.default_qdisc` | `fq_codel` | `fq_codel` | `fq_codel` |

Reading the two numbers that matter:

- **`tcp_wmem`'s third value is the ceiling TCP auto-tuning may grow the SEND buffer to: 4 MiB**, on all three
  hosts. That is a *hard per-connection cap on bytes in flight*.
- **`net.core.wmem_max` / `rmem_max` are 4 MiB here but only 208 KiB on both DC hosts.** These are the caps on
  socket buffers that *software sets explicitly* — which is exactly what a QUIC implementation does for its UDP
  socket. So the HTTP/3 path is capped at **208 KiB**, not 4 MiB, on the hosts that serve it.

And the distance, which is the other half of the arithmetic:

| path | measured RTT | how |
|---|---|---|
| control host → dedi-2 (Helsinki) | **185.24 ms** (mdev 0.16 ms) | `ping -c 20 95.216.12.55` |
| the same leg, inside a real transfer | **192–193 ms** | TCP connect times `0.1920 / 0.1929 / 0.1927 s` through the public path, measured before the client was trusted (`evidence/DC-real-latency-snapshot.md`) |
| control host → bunker-mvp (Falkenstein) | **~198 ms** | `ping -c 20 78.46.173.180` |

This page uses **192 ms** for the worked numbers because it is the figure measured *on the path a transfer
actually takes*, and it is the conservative one of the two Helsinki numbers.

---

## 2. The arithmetic — stated as arithmetic

To move data at rate `T` over a path of round-trip time `R`, you must keep `T × R` bytes **outstanding**
(the bandwidth-delay product). If the send buffer cannot hold that many bytes, the send buffer is the limit:

```
per-connection ceiling  ~=  send_buffer_ceiling / RTT
```

Numbers, from the two measured inputs in §1:

| RTT | ceiling with a 4 MiB send buffer | ceiling with a 208 KiB UDP socket |
|---|---|---|
| 185.24 ms | **22.6 MB/s ≈ 181 Mbps** | 1.15 MB/s ≈ 9.2 Mbps |
| **192 ms** | **21.8 MB/s ≈ 175 Mbps** | **1.11 MB/s ≈ 8.9 Mbps** |
| 198 ms | 21.2 MB/s ≈ 169 Mbps | 1.08 MB/s ≈ 8.6 Mbps |

At the 192 ms figure: **4 MiB / 0.192 s ≈ 21.8 MB/s ≈ 175 Mbps, no matter how much bandwidth the link has.**
As a bulk-transfer consequence: **1 GiB cannot cross a single connection in less than ≈ 49 s** on this path
while that ceiling binds.

**Three honest qualifications, because this is arithmetic and not a measurement:**

1. It is a **ceiling, not a prediction.** It binds only if the link would otherwise carry more than ~175 Mbps.
   What this link actually carries **has not been measured**, and a measurement could legitimately come back
   *below* this number (in which case the ceiling is real but not the thing in your way) or *above* it (in
   which case something else — the receive window, congestion control, the disk — is binding instead).
2. **Every input above is measured today, on these hosts.** If you are reading this on a different host, re-read
   the sysctls before reusing any number here. Nodes differ: the two DC hosts in the table disagree with the
   control host on `wmem_max` by a factor of 20.
3. **The throughput itself is not measured.** Measuring it is `BFS-055`, against the harness in `BFS-057`.
   Presenting this arithmetic as a measurement is exactly the failure mode this project spends its time
   removing.

---

## 3. Why this is an HTTP/2 problem specifically

HTTP/2 multiplexes every stream onto **one** TCP connection. That is the whole point of it — and it means
**all 25 of your concurrent requests share one 4 MiB send buffer**. The concurrency is real, the pipelining is
real, and the session's *byte* throughput is still capped by a single socket.

The lever itself is measured, and it is real:

| measurement | result |
|---|---|
| 100 concurrent 4 KiB requests, h2, one connection, 185 ms link | **0.79 s** (7.88 ms/req) vs **38.47 s** serialized on h1 — **25.0×**, 49× per request |
| whole-tree walk, h2, `--concurrency 1 → 8` | **11 257 ms → 1 454 ms — 7.7449×** (HTTP/1.1 on one connection: 1.0002×, i.e. no gain, correctly) |

**Both of those arms are latency-bound, not bandwidth-bound** — small independent requests waiting on a round
trip. That is why they are unaffected by the buffer ceiling, and it is why the ceiling cannot be used to
"explain away" the win. The two regimes are separate:

- **Many small independent requests** (metadata, a walk, `git status`) → *concurrency* is the lever, and the
  buffer is irrelevant. This is the case the FUSE client was built for.
- **Bytes** (a multi-megabyte `read-large` / `write-large`, a build artifact, a tarball) → *concurrency does
  not lift the ceiling*: N streams on one connection share one buffer, so N streams together cannot exceed
  what one connection can hold in flight. Adding streams past that point splits a fixed budget more finely.
- **N connections** (HTTP/1.1-style or `MaxConnsPerHost=N`) → genuinely lifts it, by buying N buffers. That is
  the honest trade: it costs N sockets, N handshakes, and gives up the multiplexing that made the metadata
  path fast. `BFS-011` measured exactly this shape: h1 with **8 connections** reached **1.847 s** where h1 on
  one connection reached 11.966 s.

---

## 4. Which box's buffer — the direction matters

The 4 MiB in §1 is a *send* buffer, so it caps whoever is sending:

| direction through the mount | whose buffer caps it |
|---|---|
| a **write** into the mount (upload → agent) | the **client's** `tcp_wmem` — this box: 4 MiB |
| a **read** from the mount (download ← agent) | the **agent's** `tcp_wmem` — dedi-2 / mvp: 4 MiB |

Both are 4 MiB today, so both directions land on the same ~21.8 MB/s arithmetic — but they are set on
**different hosts**, and a fix applied to one of them fixes one direction only. This is worth knowing before
"raising the sysctl" and finding half the problem still there. For the *editing* path the client's buffer is
the one that matters, because the mount's write path is what the working tree depends on.

---

## 5. HTTP/3 has a sharper version of the same ceiling

A QUIC implementation sets its UDP socket buffers **explicitly** (`SO_RCVBUF`/`SO_SNDBUF`, or the `FORCE`
variants). Explicit sets are clamped to `net.core.rmem_max` / `wmem_max`, which on **both** DC hosts is
**212992 = 208 KiB**, not 4 MiB. Arithmetic from §1: **~1.1 MB/s ≈ 8.9 Mbps per socket at 192 ms**.

- If the daemon requests buffers without the privileged `FORCE` variants, **208 KiB is the ceiling** and the h3
  path is bounded ~20× below the h2 path *on the same link*.
- If it uses the `FORCE` variants (available to it — `bunkerd` runs as root), the 208 KiB cap does not apply.
  **Which one it does is a code question, not measured here** — and it is the first thing to check before
  drawing any conclusion about h3 on these hosts.
- Independently of the socket, `quic-go` has its own connection- and stream-level flow-control windows. Those
  are separate knobs from the socket buffer, and on a 192 ms path the defaults are almost certainly small.
- An earlier observation is consistent with a per-datagram ceiling without proving this one: on the *delay
  relay* used by `BFS-011`, the large-body QUIC cells cost 10–28 s while its small-request cells behaved. That
  was named there as an instrument property, and it stays an instrument property.

---

## 6. What to change, in the order worth doing it

Each of these lands with a before/after number from the harness (`BFS-057`), on **both DCs**. A lever whose
absence cannot be detected in the numbers is not a lever.

1. **Raise the TCP socket-buffer ceilings** — `net.ipv4.tcp_wmem` (and `net.core.wmem_max`) to 16–32 MiB,
   on **both** the client and the agent hosts. Trivial, reversible, and the direct fix for §2. Do the agent
   hosts too, or you have fixed one direction.
2. **`tcp_notsent_lowat` → 64–256 KiB.** This is *not* a throughput knob: it attacks **interactive latency
   under load**, which for a working tree matters more than peak throughput. A bulk transfer twice as fast
   with doubled interactive latency is a regression for anyone editing over the mount.
3. **Congestion control: BBR is installed and unused.** The module is on disk, the running algorithm is
   `cubic`, and on a 192 ms path with measured bufferbloat CUBIC's loss-based back-off is the wrong strategy.
   Two caveats before you switch: it is a **kernel-level change on a shared box** (it affects traffic that is
   not ours), and BBR wants **pacing**, i.e. an `fq` qdisc — today's is `fq_codel`.
4. **The protocol's own flow-control windows** (Go `http2.Transport` stream/connection windows; quic-go's
   receive windows). These are *separate* from the TCP buffer: raising the sysctl does not raise them, and on
   a 192 ms path a default window can be the binding constraint on its own.
5. **Range-pipelined large reads** — issue N parallel range GETs for one large read and assemble. A win the
   kernel cannot give you, but note it spends the *same* concurrency budget, and it lives *under* the byte
   ceiling: it does not make a 4 MiB buffer carry more bytes per second.
6. **Cheap, no-correctness-exposure levers** (`BFS-059`): readahead, locks, inode retention, symlink caching,
   and verifying whether readdirplus is actually in use. These target the metadata path, which is the one the
   buffer ceiling does not touch.

**Rules that make this safe rather than a wall of knobs:** every value is explicit and reported; an invalid
value fails loudly rather than falling back; a knob the kernel cannot honour is reported as a
**requested-vs-effective mismatch**; and anything kernel-level on a shared box is reversible and *said out
loud*.

---

## 7. What this page does NOT establish

- **The single-connection throughput at 192 ms has not been measured.** §2 is arithmetic on a measured RTT and
  a measured sysctl. `BFS-055` is the row that measures it.
- **The link's actual bandwidth is unknown**, so it is not established that this ceiling binds *here* at all.
  A measurement that comes back above ~175 Mbps would show the ceiling is real but not currently the limit;
  one that comes back well below it would show the limit is elsewhere (receive window, congestion control, the
  daemon, the disk).
- **That the measured concurrency win is plateauing because of this buffer.** The 7.7449× and 25.0× arms are
  latency-bound small requests (§3); nothing in them demonstrates a byte ceiling, and no measurement yet
  shows a byte-bound arm hitting 21.8 MB/s.
- **Which QUIC socket-buffer path `bunkerd` takes** (§5) — the sysctl caps are measured; the code's request
  (plain or `FORCE`) is not read.
- **Anything about jitter or loss on these paths.** The Helsinki leg measured mdev 0.16 ms, i.e. stable; this
  page says nothing about a lossy path, where the picture is different.
- **A distribution.** The RTTs are `ping -c 20` figures and three connect samples, not repeated trials with
  variance reported.
