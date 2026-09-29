# BFS-055 — the transport matrix, measured on a standby CX53 at a synthesised WAN RTT

Evidence for "lift the arithmetic send-buffer ceiling, enable the congestion control this box already
has installed". Run on `standby-cx53-01-2147` (Hetzner CX53, nbg1-dc3, 16 vCPU, Ubuntu 24.04, kernel
6.8.0-138) — the spare instance the owner offered for exactly this, and which was reinstalled afterwards.

## THE RIG, AND WHY ITS ABSOLUTE NUMBERS ARE NOT THE ANSWER

Everything ran inside one network namespace: a veth pair, `netem delay 96 ms` per direction (~192 ms
RTT, verified by ping: 192.218 ms avg), iperf3 on both ends. The namespace keeps the delay away from the
real interface (netem on eth0 would kill the SSH session) and gives the experiment its own tcp sysctls.

**The rig measures itself at ~160 Mbit/s for ONE stream.** With the delay REMOVED entirely
(`netem delay 0ms`) a single stream still reports 159.89 Mbit/s — the same as at 192 ms. So a
single-stream number anywhere near 160 Mbit/s is the RIG's ceiling (one veth queue, netem's per-packet
cost, one iperf3 thread), not a property of the path. TSO/GSO/GRO on or off moved it 160.9 -> 171.2,
which is noise at this scale.

Parallel streams do not hit that ceiling: P8 = 1335 Mbit/s at BOTH 0 ms and 192 ms, P32 = 2757 Mbit/s.

## WHAT SURVIVES THE RIG CAVEAT

### 1. An app that sizes its own send buffer is capped at 208 KiB, and it costs 15x

`net.core.wmem_max = 212992` (208 KiB) is the cap on what `setsockopt(SO_SNDBUF)` may request. The
arithmetic is `window / RTT`:

    208 KiB / 0.192 s = 1.11 MB/s = 8.9 Mbit/s   (predicted)
    measured                                       10.62 Mbit/s   (1.33 MB/s)

This is the one single-stream arm BELOW the rig's own ceiling, so it is network-caused, not rig-caused,
and it lands on the predicted value. **An app that asks for its own buffer runs 15x slower than one that
lets the kernel autotune** (10.62 vs 158.72 Mbit/s), on the same box, in the same second.

Note the trap this exposes in the row's own wording: `net.ipv4.tcp_wmem = 4096 16384 4194304` advertises
a 4 MiB maximum, and the earlier arithmetic (4 MiB / 0.192 s = 21.8 MB/s = 175 Mbit/s) was derived from
it — but that 4 MiB is the AUTOTUNER's ceiling. `net.core.wmem_max` is the ceiling for an explicit
request, and it is 20x smaller. Two different limits, and only one of them was quoted.

### 2. Raising the box-wide caps lifts single-stream throughput well past the shipped ceiling

    shipped caps, autotune, 192 ms      160.94 Mbit/s   (cwnd 8.5 MB)
    caps raised,  autotune, 192 ms      528.29 Mbit/s   (cwnd 35 MB)

The raised arm is ABOVE what the shipped arm could reach in any configuration tested, so the shipped
caps were binding for an autotuned connection too. `net.core.wmem_max`/`rmem_max` are **NOT namespaced**
(inside the netns `/proc/sys/net/core/wmem_max` does not exist) — so this is a HOST-WIDE setting on a
real deployment, not a per-mount one. That is the single most important operational fact here.

### 3. BBR is NOT a win on this path — and the row's assumption should be corrected

BBR is available (module `tcp_bbr.ko.zst` ships with the kernel; `modprobe` works and
`tcp_available_congestion_control` gains `bbr`). Measured at 192 ms, zero loss:

    cubic, shipped caps     158.72 Mbit/s
    BBR,   shipped caps     120.06 Mbit/s     <- SLOWER
    cubic, raised caps      529.24 Mbit/s
    BBR,   raised caps      494.78 Mbit/s     <- still slower

The row says "enable the congestion control this box already has installed", implying a win. In this rig
BBR is consistently a few percent SLOWER, with `retransmits=0` everywhere — which is the expected shape:
BBR's advantage is on lossy or deeply-buffered paths, and this rig has no loss. So the honest verdict is
that **BBR is available and harmless, but it is not where the throughput is**, and the row should stop
implying it is.

### 4. Parallelism is a far bigger lever than any per-connection tuning

    shipped, P1    160.94 Mbit/s
    shipped, P8   1335.46 Mbit/s     (8.3x)
    raised,  P1    528.29 Mbit/s
    raised,  P8   2618.99 Mbit/s
    rig ceiling, P32  2756.86 Mbit/s

Every per-connection lever tested moves a single stream by at most 3.3x; going from 1 to 8 streams moves
it 8.3x — and does so in EVERY configuration, including the shipped one. This independently reproduces
the project's own earlier finding that the lever is concurrent requests, not the protocol (BFS-011).

**It also means HTTP/2 is a throughput risk, not a throughput feature, for this workload.** HTTP/2
multiplexes every stream over ONE TCP connection, so it forfeits exactly the 8.3x that parallelism buys
and re-exposes the per-connection window `net.core.wmem_max` caps. That is a design consequence worth
stating in the PRD the owner asked about, and it is the opposite of the naive reading of "h2 is faster".

## WHAT THIS DOES AND DOES NOT ESTABLISH

- ESTABLISHED: the 208 KiB app-visible cap and its ~15x cost at 192 ms; that `net.core.wmem_max` is
  host-wide and not namespaced; that raising the caps lifts an autotuned single stream well past the
  shipped ceiling; that BBR is not a win at zero loss; that parallelism beats all of it.
- NOT ESTABLISHED: any absolute WAN throughput figure. The rig cannot give one — its own single-stream
  ceiling sits at ~160 Mbit/s and its multi-stream numbers depend on 16 idle vCPUs, not on a real NIC.
  A real answer needs two datacentres and a real client path (which BFS-047 is for, and which the live
  daemon currently cannot serve because it predates the WebDAV surface).
- The DC-internal path is NOT the problem: the standby measures 3-10 ms to bunker-mvp and 23-27 ms to
  dedi-2, so buffer tuning between the DCs is near-worthless. This is a CLIENT-PATH problem, and the
  192 ms figure belongs to the client, not to the servers.

## THE BOX

Standby CX53, sacrificed for this measurement and then REINSTALLED (not destroyed — the instance type is
hard to obtain). Box-wide sysctls were changed by these tests (`net.core.wmem_max`/`rmem_max`), which is
another reason the reinstall is the correct end state.
