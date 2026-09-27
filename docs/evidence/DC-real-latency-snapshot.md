# The both-DCs leg — the client measured against a REAL data centre over the real WAN

The release's own rule is **0 stalls on both DCs**, and every client number in the release was loopback or
a simulated RTT. This closes the first half of that gap: the client, running **here**, driving the
release's own `probes/davserve` surface running on **dedi-2 in Helsinki**, over the **public path**.

## How it was done without touching anything live

dedi-2 runs a **live `bunkerd`** (pid 3908125, active, listener on `*:18080`) against a 2 TB volume. It was
**not** deployed to, restarted, or reconfigured. Instead the release's own probe server was run as a
**separate process on a spare port (18099)** with its own fixture, exactly as BFS-016 did locally — the
measurement moved to a real DC rather than the DC being changed to suit the measurement. The fixture
(255 entries, the same shape as the local arms) was created on the DC and removed afterwards.

## The link, measured before the client was trusted

Three raw `GET`s through the public path:

```
connect=0.1920 s  total=0.3878 s
connect=0.1929 s  total=0.3864 s
connect=0.1927 s  total=0.3858 s
```

That is a **193 ms RTT** — consistent with this host's previously measured 185.24 ms for dedi-2 — with the
request/response overhead on top, so the delay is real and in the path rather than assumed.

## Result — both arms, one run, one fixture, one DC

| arm | `ls -lR` on a real DC | entries | cold single-file read |
|---|---|---|---|
| **snapshot** (default) | **110 ms** | 255 | 309 ms |
| **no-snapshot** | **1210 ms** | 255 | 207 ms |

**11.0× on real hardware over the public internet.**

## Why this matters: it validates the simulation

The earlier delay-relay run at the same nominal RTT predicted **12.0×** (118 ms vs 1416 ms). The real DC
measured **11.0×** (110 ms vs 1210 ms). A relay reproduces per-request latency and nothing else — no
loss, no jitter, no real routing — so the two agreeing to within ~9% is the evidence that the relay is a
**usable surrogate** for this particular property, and that the snapshot win is a property of the design
rather than of a simulated link.

It also re-confirms the mechanism at real distance: the snapshot walk (110 ms) is **less than one round
trip** (386 ms for a plain GET), because the tree is fetched **once at mount** and the walk is then served
locally. The no-snapshot arm pays the RTT per request and lands around 6 round trips for this fixture.

## What this does NOT establish — stated plainly

- **It is ONE real DC (dedi-2, Helsinki).** The rule says *both*. The second DC leg is still open.
- **It is not a stall measurement.** These are walk *times*. The 0-stall claim still rests on BFS-016,
  which is loopback. The rule is "0 stalls on both DCs" and neither half of that is discharged at a DC.
- **The server was the release's probe surface, not the deployed daemon.** The deployed dedi-2 `bunkerd`
  is an OLDER build whose `/dav/` answers `405` for PROPFIND — it predates the WebDAV surface
  (BFS-006), and deploying the new build to a live host was explicitly out of bounds.
- **One run per arm**, not a distribution. No variance, no outlier trim, no repeated trials.

## A process note worth keeping

The first attempt to start the DC probe server silently did nothing, and the cause was **my own hand in
the trap this release kept hitting**: the setup command contained `pkill -f 'dc-davserve'`, and that
pattern matched **the ssh command line running it**, killing my own shell before it could start the
server. The fix was to stop using a pattern kill entirely — `setsid ... < /dev/null &` start, and
`ss`/`curl` verification. Same failure mode that hung a worker for nine hours earlier in this release,
and it was not a firewall: `iptables INPUT policy ACCEPT`, `ufw inactive`.
