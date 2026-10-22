# MOUNT-011 closure — the direct-path remeasure refutes the PASS arm

Row MOUNT-011 (P1), 2026-10-22. Evidence: `docs/mount-drivers.md` §5, commit `c31c2e82`. Closure: confirmed-with-correction.

## What was measured

PASS arm: re-run the report script against the fixed path and show the metadata ms/op AND
the stall set both improve, requiring low mdev, not just a low average. Re-measured
2026-10-22 per §5.1 on a verified direct tailnet path (`tailscale status` → direct
`78.46.173.180:41641`; ping mdev 0.30 ms):

| quantity | 2026-09-23 relayed (`bunker-las-02`) | 2026-10-22 direct (`bunker-mvp`) |
|---|---|---|
| path | DERP(`lax`) relay | direct `78.46.173.180:41641` |
| RTT mdev | 91.1 ms | **0.30 ms** |
| sshfs metadata, uncached | 141.8 ms/op (`cache=yes`), 154.1 ms/op (`cache=no`) | **188.8–198.5 ms/op cold** (four runs) |
| sshfs metadata, cached | — | 3–7 ms/op — page cache, not the network |

`bunker-las-02` is offline (last seen 9 days; `tailscale ping` no reply), so its figures are
the retained baseline and the direct-path peer is `bunker-mvp`. The path improved in exactly
the dimension the row demanded — mdev 91.1 → 0.30 ms — while per-op cost and the stall set
did not move: cold metadata was marginally worse.

## Why the PASS arm failed

§5.3: an uncached sshfs metadata op costs exactly one round trip, and a 60 s budget divided
by 190 ms is ~315 such ops. git's whole-tree commands stat every entry — thousands of ops on
a 106-file repo — so they are bounded by `op count × RTT`. Removing the relay removes jitter
and recovers throughput; it cannot remove the round trip, because the peer is still
~168–180 ms away on a direct path. Directness is a throughput and jitter remedy, not a
latency-floor one.

## Decision

git runs on the agent (SURF-018); bulk transfer uses `bunker cp` (SURF-014); the mount remains
the inspect/grep/diff fidelity surface (§5.5). A direct path stays worth having for jitter and
interactive latency; no driver, flag, default or mount option was changed.

The single-variable latency proof — same bandwidth, 20–47x git cost, the three stalls all
completing short-range — stands. Only its fix arm is refuted.
