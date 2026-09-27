# The snapshot win at WAN latency (185 ms RTT) — the "both DCs" risk, partially de-risked

The release rule says bunker-fs must show **0 stalls on both DCs**, and it is not satisfied: every client
measurement in this release is **loopback**. dedi-2's measured RTT is **185.24 ms**, and the whole-tree
read is the client's headline win, so the obvious question is whether that win survives real distance or
is a loopback artefact.

The real DC leg is blocked (dedi-2 awaits a Tailscale auth click), so this measures the same client over
a **delayed TCP relay** at **92.62 ms one-way = ~185.2 ms RTT** — dedi-2's actual measured figure, not a
guess.

Reproduce: `docs/evidence/WAN-latency-walk.sh` + `docs/evidence/WAN-latency-delayrelay-python.md`.

## Result — both arms, one run, one fixture, one relay

| arm | `ls -lR` over 185.2 ms RTT | entries |
|---|---|---|
| **snapshot** (default) | **118 ms** | 259 |
| **no-snapshot** (`--no-snapshot`) | **1416 ms** | 259 |

**12.0× at realistic WAN latency.** The delay was verified in the path before either arm was trusted:
three raw `GET`s through the relay measured 0.15–0.19 s each against a loopback floor of ~1 ms.

## Why the snapshot walk is FASTER than one round trip — and why that is the real finding

118 ms is *less* than the 185.2 ms RTT, which looks wrong until you know the mechanism: **the snapshot is
fetched at mount time, not during the walk.** The mount log from the release's dogfood run shows it
happening — `snapshot: snapshot-op, 6 nodes in 1 call(s)` — and BFS-016 measured the same thing
independently: **zero requests during the walk** once the snapshot is held.

So the honest statement of the win is not "the walk is one call" but:

> **the whole tree is fetched once, at mount, and every subsequent whole-tree walk is served locally.**

That is a stronger property than a per-walk speedup, and it is why the ratio *grows* with latency: the
no-snapshot arm pays the RTT per request, and the snapshot arm pays it once at mount.

## What this establishes, and what it does not

**Establishes:** the snapshot win is **not** a loopback artefact. At dedi-2's own measured latency the
gap is 12.0×, and the mechanism (fetch-once-at-mount) is understood rather than merely observed. The
release's central performance claim survives distance.

**Does NOT establish:** this is a **simulated** RTT, not a real DC. A relay reproduces the *per-request
latency* and nothing else — not packet loss, not a real link's jitter or bufferbloat, not the remote
box's own disk. The "both DCs" rule therefore remains **unsatisfied**, and this file says so rather than
being reported as if the rule were met.

**Also does NOT establish** anything about stalls: this measured walk time only. The 0-stall claim rests
on BFS-016, which is loopback, and on nothing at WAN distance yet.
