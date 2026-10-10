# Mount technology watch-list

**Status:** watch-list only. **None of these are to be built now.** Each entry
records what the technology is, why it is or is not relevant to bunker's mount
story, a source, and a **named trigger condition** — the concrete future fact
that would make us pick it up. The list exists so the options are re-read from
the record instead of rediscovered, and so picking one up later is a decision
against a written trigger, not a new research project.

See also: [`mount-drivers.md`](mount-drivers.md) — the drivers we actually
ship (sshfs default, rclone opt-in, FUSE-over-io_uring planned).

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

**What it is.** Beyond the io_uring *transport* already planned as
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
target kernel has the FUSE io_uring series."* That is exactly MOUNT-008's
verification probe going green on a stock install — at that point this entry
graduates into the driver work, and each later kernel/libfuse upgrade should
be re-checked against the zero-copy and fadvise series.

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
