# Bunker Threat Model

**Status:** v1, adopted from the security panel review (2026-09-20). Living document.
**Owner:** repository maintainers. **Review cadence:** on any change to the control plane,
transport, isolation path, or agent lifecycle.

This document exists because its absence was the single most-repeated finding of the
security panel: with no threat model there is nothing to approve against, no named
adversaries, and no stated residual risk. Every claim below is written to be **true of the
shipped default configuration**, verified against source at the commit noted in each
Evidence cell.

---

## 1. Scope

**In scope:** the daemon (`bunkerd`), the CLI (`bunker`), the transport between them, the
per-agent isolation boundary, the audit chain, and the host-provisioning path.

**Out of scope (stated, not hidden):**

- **A malicious host root.** Any process running as host root can read the daemon's config,
  rewrite the audit chain, and spawn agents. No sandboxing product can defend this, and
  Bunker does not claim to. The goal is to be *honest* about that and *safe* below it.
- Social engineering and physical access to the host.
- Vulnerabilities in the container runtime or the Linux kernel itself, beyond tracking them.

---

## 2. System model

```
  operator ──[network]──► bunkerd (host root) ──► agent user ──► container ──► internet
              ▲ BT1                    ▲ BT2          ▲ BT3        ▲ BT4
```

- **BT1 — Transport.** Operator to daemon. *Default today: plaintext, TLS disabled.*
- **BT2 — Authorization.** The daemon's decision of who may call what. *Default today: one
  static master token; no per-operator identity.*
- **BT3 — Agent isolation.** Host user/per-user dockerd/userns boundary. *Default today:
  strong, with one known allocation defect (see §5).*
- **BT4 — Egress.** Container to internet. *Default today: unrestricted; no policy exists.*

The central conclusion of this model: **BT3 — the boundary Bunker built well — is the
boundary least likely to be crossed. BT1, BT2 and BT4 are where the risk actually sits, and
all three are currently open.** The panel's unanimous "do not approve for team use" follows
from that sentence, not from any single bug.

---

## 3. Assets

| Asset | Where it lives | Why an attacker wants it |
|---|---|---|
| **Daemon control plane** | Host, root | Can spawn/destroy/exec agents; root-equivalent over the fleet |
| **Master credential** (`auth.token` / `auth.jwt_secret`) | `/etc/bunkerd/config.yaml` | Total control-plane access; no rotation, no revocation, no per-user attribution |
| **Agent home directories** | `/home/bunker-<id>` | Source code, artifacts, images, credentials the workload brought |
| **Per-agent SSH private keys** | Host + agent home; over the wire only via the opt-in `return_ssh_private_key` spawn flag or the master-gated `GetAgentKey` RPC (GAP-128) | Docker-host access to that agent |
| **Audit log** | Host (`internal/audit/`) | The only forensic record; its integrity *is* incident response |
| **Peer agent data** (shared scratch `/srv/bunker-share`) | Host, group `bunker-agents`, mode `2770` | Cross-tenant read/write |
| **The host** | — | Anything that escapes the agent user reaches other agents |

---

## 4. Adversaries

| # | Adversary | Capability assumed | Primary control that must exist |
|---|---|---|---|
| A1 | **Malicious or buggy agent workload** | Full root *inside its own container*; arbitrary syscalls from a container | Kernel/container boundary, egress control, resource bounds |
| A2 | **The agent operator** (a team member) | Holds agent credentials; runs the `bunker` CLI | Per-operator identity, RBAC, audit attribution |
| A3 | **Tenant vs tenant** | Agent A tries to read/affect agent B | Isolation of home, scratch and daemon; no shared writable surface by default |
| A4 | **Credential thief** | Obtained the master token (log leak, shoulder-surf, world-readable file) | Transport security, rotation, revocation, failed-auth detection |
| A5 | **Host-local unprivileged user** | A non-agent account on the same box | File permissions, secret storage |
| A6 | **Supply-chain attacker** | Compromised image or rootless installer fetch | Pinning, signatures, digest verification |
| A7 | **Malicious host root** | — | **Out of scope** (§1) |

---

## 5. Trust-boundary analysis, with defaults as shipped

### BT1 — Transport (adversary A4)

*Default:* `tls.enabled: false`, `tls.mtls: false` (`internal/config/config.go`). The daemon
will start on a non-loopback address with TLS off. **There is no transport gate** — the
config has `CheckAuth()` (which refuses to start when auth is enabled but unconfigured) but
no equivalent `CheckTLS()`, so a plaintext admin plane is the path of least resistance.

*Consequence:* every credential, every agent SSH key returned by `SpawnAgent`, and every
agent-issued RPC crosses the wire in clear unless the operator independently decides to turn
TLS on. An A4 adversary on-path can lift the master token and own the fleet.

*Control:* TLS-by-default with an explicit, warned, audited `insecure_dev` escape hatch; a
self-signed + pinning path so the secure option is the easy one. *(REQ-T1, REQ-T2.)*

### BT2 — Authorization (adversaries A2, A4)

*Default:* a single static master token (or a shared JWT secret) in the daemon config. There
is no per-operator identity and no RBAC. Roles are effectively two: "master" and "agent's
own key". An agent-scoped key that lacks an ownership check on a given RPC is an
authorization bypass — one instance of this (heartbeat TTL extension) is confirmed; see §6.

*Control:* per-operator credentials surfaced as `caller` in audit; `admin`/`operator`/
`agent:self` roles; a key lifecycle with rotation and revocation; ownership checks on every
agent-scoped RPC. *(REQ-I1…I4.)*

### BT3 — Agent isolation (adversaries A1, A3)

*Default:* per-agent Linux user, per-agent rootless dockerd, and user-namespace remapping.
This is the product's core strength and the panel agreed it is built well.

**Former defect (SEC-22), now FIXED under GAP-140:** the subordinate-ID writer used to
allocate each agent's range *starting at its own uid* with a fixed count of 65536, so
consecutive agents received **overlapping** ranges (agent 1001 → `[1001..66536]`, agent 1002
→ `[1002..66537]`). That meant the user-namespace separation between agents was weaker than
the design intends. It is now closed: ranges are allocated from a pool that skips every range
already in the database, under a host-wide flock, and the daemon **refuses to start** if an
overlap is present (remediate with `bunker subid-migrate`). Two agents' subordinate ranges are
pairwise disjoint by construction, and the guarantee is checked, not assumed.

*Also honest:* PID-namespace isolation is intentionally omitted on the current rootlesskit
(see `internal/agent/isolation.go`); rootless containers share the host kernel, so the
boundary is defence-in-depth, not a hypervisor.

### BT4 — Egress (adversary A1)

*Default:* **none.** There is no default-deny, no allowlist, and no egress code path. A
container that a dependency has compromised can exfiltrate agent home data and reach internal
services. The panel called egress control "the highest-value single addition."

*Control:* default-deny or a seeded allowlist, configurable per agent/trust tier. *(REQ-E1.)*

---

## 6. Attack scenarios (concrete, not abstract)

1. **Lift the master token → own the fleet.** A4 on-path on a plaintext link reads a token;
   with TLS off this needs no exploit. *BT1.*
2. **Extend a peer's lease.** An agent key calls heartbeat without an ownership check and
   keeps another agent alive past its TTL — a confirmed authorization bypass. *BT2.*
3. **Brute-force the static token.** No rate limiting on unauthenticated requests and no
   audit record of denials means credential-stuffing is both cheap and invisible. *BT2.*
4. **Exfiltrate via a poisoned dependency.** With no egress policy, a build-time dependency
   reads the agent home and ships it out. *BT4.*
5. **Read a peer's artifacts.** Shared scratch is on by default under a shared group; absent
   an explicit decision, agent A can read what agent B exchanged. *BT3 / A3.*
6. **Cross the agent boundary via subuid overlap.** *Formerly possible; now closed (GAP-140).* The overlapping subordinate ranges used to let a later agent's container root collide with an earlier agent's ID mapping. Ranges are now allocated disjoint under a host-wide lock and a startup gate refuses an overlapping host. *BT3.*

---

## 7. Residual risk (true even after the roadmap lands)

These remain true and are stated in `SECURITY.md` as well:

- A malicious **container** workload can attack the **host kernel**. Containers share the
  kernel; the userns/rootless design is defence-in-depth, not a hypervisor boundary.
- An agent with **legitimate exec access** can run anything the daemon would run on its
  behalf. Audit records it; it does not prevent it.
- **Cross-boot PID/tmpfs state is not preserved** — "isolation" is per-instance, not
  per-lifetime.
- The audit chain detects tampering **only** when sealed and anchored off-box. Without both,
  a host-root attacker can rewrite history undetectably, and the tamper-evidence claim must
  be stated as conditional.
- Egress control, once added, is policy, not proof: it reduces exfiltration paths, it does
  not eliminate covert channels.

---

## 8. Open work

This model is the *target* state for the controls marked above; the gap between it and the
shipped defaults is the security-readiness backlog (board rows `GAP-123`…`GAP-142`), with the
sequencing in `docs/prd/security-readiness.md` §6. The model is re-reviewed whenever a
control changes state, so it never claims a protection that has not shipped.

---

## Appendix — provenance

Derived from a six-seat, six-model-family security panel (2026-09-20): Zhipu GLM-5.3,
Moonshot Kimi-K3, Qwen3-Coder-Plus, Anthropic Claude-Opus-4.8, Google Gemini-2.5-Pro, OpenAI
gpt-5.6-sol. Unanimous verdict: **do not approve for team use** with the control plane in its
current state. Every load-bearing claim was re-verified by the coordinator against raw
source before being written here. Full ledger: `docs/prd/security-readiness.md` §4.
