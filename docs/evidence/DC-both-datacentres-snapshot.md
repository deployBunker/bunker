# The both-DCs leg — the client measured against TWO real data centres

The release rule is **0 stalls on both DCs**, and every client number in the release was loopback or a
simulated RTT. This closes the *topology* half of that gap: the same client binary, running here, driving
the release's own `probes/davserve` surface in **two different data centres** over the **real internet**.

| | DC 1 | DC 2 |
|---|---|---|
| host | `dedi-2` | `bunker-mvp` |
| location | **Helsinki** | **Falkenstein** |
| path | public | ssh tunnel (see caveat) |
| link, measured | connect 0.192–0.193 s, total 0.386–0.388 s per GET | 0.37 / 0.53 / 0.53 s per GET |
| **snapshot walk** | **110 ms** (255 entries) | **110 ms** (255 entries) |
| **no-snapshot walk** | **1210 ms** (255 entries) | **1612 ms** (255 entries) |
| ratio | **11.0×** | **14.7×** |
| cold single-file read | 309 ms | 509 ms |

## The result that matters is not the ratio — it is the 110 ms twice

**The snapshot walk is 110 ms on both DCs — identical — while the per-request floor differs by 200 ms
between them** (a cold single-file read is 309 ms in Helsinki and 509 ms in Falkenstein). A whole-tree walk
that does not move when the link gets slower is the strongest evidence yet that the mechanism is what the
release claims: **the tree is fetched once at mount, and every subsequent walk is served locally.** The
walk's cost is local work, not round trips.

The ratios differ (11.0× vs 14.7×) for the same reason and in the expected direction: the no-snapshot arm
pays the RTT per request, so the *slower* DC makes the snapshot look better. That is internally consistent
rather than a discrepancy.

## How both were done without touching anything live

Both hosts run a **live `bunkerd`** (Helsinki: pid 3908125 active; Falkenstein: active, `*:18080` +
`*:19090`). **Neither was deployed to, restarted, or reconfigured.** The probe server ran as a separate
process on a spare port with its own fixture, then was stopped and removed — verified afterwards:
`no probe process left`, `18097 listeners: 0`, and the live daemon still `active` with its listener intact.
Per-host cleanup was confirmed, not asserted.

## Caveats, stated rather than buried

- **DC 2 went through an ssh tunnel, not the public path.** Falkenstein's `ufw` is `active` with **INPUT
  policy DROP**, so only 22/18080/19090 are reachable; rather than open a hole in a live host's firewall,
  the probe port was forwarded with `ssh -L`. The traffic still crosses the real WAN to Falkenstein, but
  ssh adds framing and encryption overhead on top of the real RTT, so DC 2's *absolute* link numbers are
  not directly comparable to DC 1's.
- **This is still NOT a stall measurement.** These are walk *times*; each 255-entry walk completed `rc=0`,
  but the release's "0 stalls" claim rests on BFS-016's 35-cell battery, which is loopback. The rule as
  written — 0 stalls on both DCs — is **not yet discharged at either DC**.
- **The server was the release's probe surface, not the deployed daemon.** Both hosts run an older
  `bunkerd` that predates the WebDAV surface; deploying the new build to a live host was out of bounds.
- **One run per arm per DC.** No distribution, no variance, no repeated trials.

## Two process notes worth keeping

1. **I hit the `pkill` self-match trap myself.** The first attempt to start the DC-1 probe silently did
   nothing: the setup command contained `pkill -f 'dc-davserve'`, and the pattern matched **the ssh command
   line running it**, killing my own shell before it could start the server. Fixed by abandoning pattern
   kills entirely (`setsid ... < /dev/null &`, then verify with `ss`/`curl`). Not a firewall — Helsinki's
   `iptables INPUT` policy is `ACCEPT`, `ufw inactive`.
2. **A truncated stream cost a re-run, so the retry wrote to a file.** The first DC-2 snapshot figure was
   printed to a stream that got cut, and the artifacts did not carry the timing — so the arm was re-run
   with the result written to disk. The re-run reproduced **110 ms exactly**, which is also an independent
   confirmation of the first measurement rather than a replacement for it.
