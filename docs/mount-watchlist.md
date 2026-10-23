# Mount technology watch-list

**Status:** watch-list only. **None of these are to be built now.** Each entry
records what the technology is, why it is or is not relevant to bunker's mount
story, a source, and a **named trigger condition** — the concrete future fact
that would make us pick it up. The list exists so the options are re-read from
the record instead of rediscovered, and so picking one up later is a decision
against a written trigger, not a new research project.

See also: [`mount-drivers.md`](mount-drivers.md) — the drivers we actually
ship (sshfs default, rclone opt-in, FUSE-over-io_uring an opt-in verified
transport), and its §5, the measured MOUNT-017 remote-filesystem latency
decision (git on the agent, `bunker cp` for bulk transfer).

---

## 1. FUSE passthrough — kernel-side, local stacks only

**What it is.** Since Linux 6.9 (`CONFIG_FUSE_PASSTHROUGH=y`), a FUSE mount
can pass individual files through to the underlying filesystem directly: for
those files the FUSE daemon is bypassed entirely, removing the
kernel↔userspace round-trip per request.

**Relevance to bunker.** A big win for stacked **local** filesystems — but it
is **not** available for a network filesystem like sshfs, which has no local
backing file to pass through to. So it is relevant to **agent-internal**
stacked mounts, not the agent mount itself. Present on the control host
(`CONFIG_FUSE_PASSTHROUGH=y`).

**Source.** FUSE passthrough merged in Linux 6.9+; presence on the control
host confirmed in the MOUNT-005 research.

**Trigger.** *"An agent-internal stacked local mount needs per-file
performance."* Until a bunker component stacks FUSE over a local filesystem
inside the agent, this stays a note, not work.

## 2. FUSE io_uring buffer/zero-copy series — still landing upstream

**What it is.** Beyond the io_uring *transport* implemented as
[`mount-drivers.md`](mount-drivers.md) §3.3 (tracked as `MOUNT-008`), two
further series are in flight upstream: a buffer/zero-copy patch series for
FUSE over io_uring (posted to linux-fsdevel, Apr 2026) and `vfs_fadvise`
forwarding (Sep 2026). Both cut copies and latency on the kernel↔userspace
FUSE path.

**Relevance to bunker.** Same surface as MOUNT-008: it applies where the mount
happens (the client host), gated on kernel + libfuse, never on the agent.
Still landing, so today's answer is "watch" — and the pickup path is ordinary
libfuse/kernel upgrades, not new code of ours.

**Source.** linux-fsdevel, FUSE io_uring buffer/zero-copy series (Apr 2026);
`vfs_fadvise` forwarding series (Sep 2026).

**Trigger.** *"The packaged libfuse exposes the io_uring transport AND the
target kernel has the FUSE io_uring series."* **Both halves of the libfuse/
kernel precondition are already true on the control host** — re-measured
2026-10-14, the bundled `libfuse3.so.3.18.2` carries `fuse_req_is_uring` and
`-o io_uring`, and the `7.0.0-31-generic` kernel has `CONFIG_FUSE_IO_URING=y`;
the only unmet gate is the runtime switch
(`/sys/module/fuse/parameters/enable_uring` reads `N`). See
[`mount-drivers.md`](mount-drivers.md) §3.3 — that section also corrects the
earlier, wrong "libfuse exposes nothing" reading that this trigger was written
against. Each later kernel/libfuse upgrade should be re-checked against the
zero-copy and fadvise series.

## 3. SMB over QUIC — wrong problem for us

**What it is.** SMB over QUIC as a file-access transport: Samba shipped full
support and the kernel cifs/ksmbd series landed (2026-04). The transport is
UDP/443 with kernel-TLS handshaking, designed to reach file shares across
networks that block raw SMB ports.

**Relevance to bunker.** Interesting mainly for crossing hostile networks
**without a VPN** — which we do not need internally: our hosts sit behind
Tailscale/WireGuard, and the agent mount rides SSH/SFTP. It would also break
the driver rule of *no new server-side requirement* (ksmbd/Samba on the agent
side), so this is recorded as a rejection with a reason, not a TODO.

**Source.** Samba SMB-over-QUIC support; kernel cifs/ksmbd SMB-over-QUIC
series landed 2026-04.

**Trigger.** *"A deployment must cross a hostile network without a VPN."* If a
bunker ever has to serve files across an untrusted network where we control
neither the path nor a VPN, this becomes the candidate — not before.

## 4. JuiceFS / SeaweedFS — the multi-writer answer

**What it is.** Distributed filesystems that expose real POSIX semantics over
shared storage — FUSE **and** native kernel mount, hardlinks, atomic rename —
with the data on distributed backing storage. In 2026 both are explicitly
being marketed at agent workspaces.

**Relevance to bunker.** This is the answer if/when "deploy any number of
bunkers" comes to mean agents **sharing one workspace tree** with consistent
rename/lock semantics. sshfs cannot do multi-writer at all — one SFTP
connection per mount, no coherence between mounts — so a shared-workspace
requirement is not solvable by tuning the drivers we ship.

**Source.** JuiceFS and SeaweedFS project documentation and their 2026
agent-workspace positioning.

**Trigger.** *"A multi-writer shared workspace requirement lands"* — agents
sharing one workspace tree, relying on its rename/lock semantics. Until that
requirement exists, single-writer sshfs/rclone per agent is correct and
simpler.

<a id="remote-fs-latency"></a>
## 5. Remote-FS latency — DERP relay vs a direct tailnet path

**What it is.** The transport *under* any of the drivers above: a tailnet path
that is either a **direct** WireGuard endpoint pair or a **DERP relay**, plus the
NAT properties that decide which one you get (`MappingVariesByDestIP`,
`PortMapping:`). For a synchronous FUSE filesystem this — not the driver — sets
the per-operation cost, because one uncached metadata op is one round trip.

**Relevance to bunker.** Measured 2026-10-22 (MOUNT-017, recorded in
[`mount-drivers.md`](mount-drivers.md) §5): a verified **direct** path still costs
**194 ms per uncached metadata op** at 168 ms RTT (+26 ms FUSE/sshfs overhead),
so `git`'s whole-tree ops stay bounded by `op count × RTT` whether or not the
path is relayed. What directness buys is **jitter and bandwidth**: 0.30 ms mdev
direct versus 14.7 ms on a still-relayed peer, and the relay's throughput cap the
row measured at 1.9 MB/s. The `bunker-las-0x` peers cannot go direct at all
today: this host's `PortMapping:` is EMPTY with `MappingVariesByDestIP: true`
(hard NAT), so two hard NATs never punch through.

**Source.** MOUNT-017 probe, 2026-10-22 — `tailscale status`/`netcheck`/`ping`,
ICMP RTT series, cold-op timing on a live sshfs mount; reproducible via
`probes/mount017-path-probe.sh`.

**Trigger.** *"A peer must run git-heavy or many-small-file work across the wire
AND its direct-path RTT is low enough that `op count × RTT` fits the command
budget (single-digit ms), or a driver lands with request pipelining / a metadata
cache that survives a whole-tree walk."* Every peer we own measures ≥ 140 ms RTT
today, so the trigger is not met and git stays on the agent. Two narrower facts
would also land here rather than in a driver row: a **port mapping** appearing on
the local router (UPnP/NAT-PMP, or a static UDP 41641 forward), which is the one
owner action that would make the `bunker-las-0x` family direct; and any change to
Tailscale's relay selection, which today exposes **no per-peer preference**
(`tailscale debug prefs`, 1.102.3) even though the nearest relay (mia,
35.4–51.1 ms across two netchecks) is 43–74 ms closer than the lax relay
(94.5–109.5 ms) a relayed peer uses.

**2026-10-22, MOUNT-013** — the row's pipelining trigger was tested with a real
rclone mount (SFTP backend, `--sftp-concurrency 64`, `--vfs-cache-mode off`) over
the same 112-file git battery on dedi-2 (~184 ms RTT, public direct): rclone did
**not** clear the sshfs stall set — `status --short`, `diff --stat`,
`checkout -b`, and `pull --rebase` all timed out at 60 s, the stall set grew from
three named sshfs stalls to five of fourteen ops, and only the *cached*
metadata class improved (4.2–5.0 ms/op distinct-file; the uncached classes still
cost one round trip: `statfs` 208 ms, first content read 747 ms). The
mounted-workflow line stays closed; full numbers and the invocation in
[`mount-drivers.md`](mount-drivers.md) §6.

(94.5–109.5 ms) a relayed peer uses. A **sync instead of mount** strategy
(Mutagen, single static binary, no FUSE/root — MOUNT-014) sits inside this
trigger for git-heavy work: it removes the per-op round trip entirely by running
git on a local copy. Its evaluation is gated on a live `bunker-las-0x` peer
(bunker-las-02 offline 10 days as of 2026-10-22); see
[`mutagen-remote-dev.md`](mutagen-remote-dev.md).
