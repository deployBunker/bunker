# Per-Agent Egress Policy (GAP-134)

Every agent's outbound network traffic is governed by a per-agent egress
policy. The design authorities are REQ-E1 / SEC-06
([docs/prd/security-readiness.md](prd/security-readiness.md) §5.4) and the
"BT4 — Egress" boundary ([docs/threat-model.md](threat-model.md)): before
GAP-134 there was no egress code path at all — a container compromised
through a poisoned dependency could exfiltrate agent-home data and reach
internal services freely.

## Modes

Configured daemon-wide under `agent.egress.mode`, or per spawn with the
`--egress-mode` flag of the spawn command (the per-spawn value wins). The vocabulary is
owned by `internal/egress`; unknown names refuse at config load, at the RPC
boundary (`CodeInvalidArgument`), and in the spawn path — a typo never
silently resolves to a different boundary.

| Mode | Behavior |
| --- | --- |
| `open` (default) | Unrestricted outbound. **Zero behavior change**: no firewall command is ever invoked — a fleet that never configures egress runs byte-identically to a pre-GAP-134 daemon (pinned by test). |
| `allowlist` | A default-deny chain keyed on the agent's uid. Accepted: loopback, return traffic of already-established connections, and the destinations in `agent.egress.allowlist`. Everything else is dropped (and counted, so `nft list chain` shows how much policy blocked). |
| `none` | Deny-all: loopback and established/related return traffic only. |

## When to use which

- `open` — trusted workloads, hosts without root firewall access, and any
  deployment that has not made an explicit egress decision yet. It is the
  safe default precisely because it changes nothing.
- `allowlist` — untrusted or supply-chain-risky workloads: CI runners,
  agents executing model- or user-authored code, anything that handles
  secrets it must not be able to ship out. Seed the allowlist with what the
  workload genuinely needs (package mirrors, artifact registries, your
  proxy).
- `none` — the strongest containment: fully offline agents (local build
  sandboxes, data-processing over mounts, blast-radius containment after an
  incident).

## How enforcement works

- The **root daemon** (bunkerd) owns every firewall mutation. No agent
  receives any nftables/iptables capability, unit property, env var, or
  profile line — an agent cannot see, alter, or bypass its chain short of a
  kernel exploit.
- Rules install at spawn (after the dockerd start is verified, before the
  agent is registered as running) and are removed at destroy. The chain is
  named deterministically per uid: `bunker-egress-<uid>` (nftables) /
  `BUNKER-EGRESS-<uid>` (iptables), inside the shared `bunker_egress`
  table; the uid match lives in the jump rule (`meta skuid <uid> jump …` /
  `-m owner --uid-owner <uid>`), so policy follows the agent's uid, not its
  network namespace.
- A stale-chain sweep runs at daemon start and every minute thereafter
  (same grace as the TTL reaper: it waits for reconciliation). It removes
  chains whose uid no longer belongs to a live managed agent. A daemon
  whose config is `open` and whose records carry no enforced mode never
  touches the firewall at all.
- **Failure is loud.** If rule installation fails in `allowlist`/`none`
  mode, the spawn FAILS and rolls back — an agent is never left running
  unenforced while its config claims it is restricted.

## Verifying enforcement

The daemon owns every rule, so verification runs on the daemon host as root.
The chain name is a pure function of the agent's uid — `id -u
bunker-<agent-id>` gives the uid, and the same uid names the chain in both
backends' spellings: `bunker-egress-<uid>` (nftables, the default backend)
or `BUNKER-EGRESS-<uid>` (the iptables fallback, used only when the `nft`
binary is absent from the daemon's PATH).

nftables host — confirm the shared table, the jump rule, and the per-agent
chain (the `counter drop` line's counters are the "how much did policy
block" read):

```bash
# Shared table: shows the output_hook chain and every per-agent chain.
sudo nft list table ip bunker_egress

# The jump that routes this agent's uid — look for
# `meta skuid <uid> jump bunker-egress-<uid>` inside output_hook:
sudo nft list chain ip bunker_egress output_hook

# The per-agent chain: loopback + established/related accepts, any
# allowlist accepts, then the final `counter drop`.
sudo nft list chain ip bunker_egress bunker-egress-<uid>
```

iptables-fallback host:

```bash
sudo iptables -S | grep BUNKER-EGRESS      # chain declarations + the OUTPUT jump
sudo iptables -nL BUNKER-EGRESS-<uid>      # per-agent rules with packet counters
```

Nothing listed means nothing is enforced. In `open` mode that is the correct
state (open never invokes a firewall command); in `allowlist`/`none` mode a
missing chain means the agent was destroyed, the stale-chain sweep removed
it, or the spawn that should have installed it failed loudly — check the
daemon log before trusting the boundary.

Prove the policy actually bites from inside the agent. `bunker exec` runs
its command as the agent's uid, so its traffic is exactly the traffic the
jump routes into the chain. Spawn an enforced agent first
(`--egress-mode none` for the strongest demo), then run a deny/allow pair:

```bash
# DENY: a non-loopback destination hangs and times out (packets are DROPPED,
# not refused) — use an IP literal so the probe fails at connect, not at the
# also-dropped DNS step; the chain's drop counter above ticks up while it hangs
# (192.0.2.1 is RFC 5737 TEST-NET-1: nothing answers it anywhere):
bunker exec demo-agent --server bunker-host -- curl -m 5 -sS http://192.0.2.1/ -o /dev/null; echo "exit=$?"   # exit=28 (timeout)

# ALLOW: loopback is accepted in every enforced mode, so a local target
# answers immediately — the daemon's own REST endpoint is a convenient one:
bunker exec demo-agent --server bunker-host -- curl -m 5 -sS http://127.0.0.1:8080/healthz; echo   # {"status":"ok"}

# Under `allowlist`, the allow side is a DESTINATION you allowlisted
# (deny a non-allowlisted one instead); under `none`, loopback is the only
# allow target. Remember outbound DNS is dropped unless your resolver is
# allowlisted — probe IP literals so the deny side fails at connect.
```

A denied `curl` timing out (exit 28) plus an answering loopback probe, with
the chain's drop counter advancing between the two, is end-to-end proof that
the policy is installed, hooked to this agent's uid, and enforcing.

## The control channel stays open

In every enforced mode, the agent's connection back to the bunker control
plane keeps working by construction: the agent's dockerd control is a
**unix socket** (filesystem, not network), and the daemon-to-agent SSH
session is an **inbound** connection whose replies match the
`ct state established,related` accept in the per-agent chain. `none` mode
therefore means "no *outbound* traffic", not "unreachable".

## DNS caveat

Allowlist entries may be hostnames; they are **resolved to IP addresses at
rule-install time** and the durable rules carry IPs only. Consequences:

- A hostname whose DNS answer rotates (CDN, load balancer) goes STALE —
  traffic to the new address is dropped until the policy is reinstalled
  (respawn, or daemon restart — the daemon-start sweep does not re-resolve
  rules; re-spawn the agent). Prefer stable IPs/CIDRs, or your own
  allowlisted resolver plus pinned destinations, for long-lived agents.
- **DNS itself must be allowed explicitly.** A default-deny chain drops
  outbound DNS: add your resolver's addresses (e.g. `10.0.0.1/32`,
  `1.1.1.1/32`) to `agent.egress.allowlist`, or resolution — including the
  install-time resolution of every other entry — fails and the spawn
  refuses loudly. Domain-name rules are a convenience at install time, not
  a DNS-filtering feature: there is no SNI/Host inspection, so an allowlisted
  hostname also allows every other name sharing its IP.

## iptables fallback

nftables is preferred; when the `nft` binary is absent from the daemon's
PATH, the iptables path installs the equivalent rule set (chain
`BUNKER-EGRESS-<uid>`, jump inserted at position 1 of OUTPUT). The
fallback is **IPv4-only**: on a host without nftables, IPv6 egress stays
unrestricted — this is a loud, documented gap of the fallback path. Hosts
with dual-stack requirements must run nftables.

## Container mode (GAP-065) — future note

When container-mode agents land (GAP-065), they will run inside a Docker
network namespace rather than as host users, and **Docker network policy
(custom networks, `--icc`, per-network egress filtering) replaces the
nftables chain for them** — a uid source-match cannot address a container
whose outbound traffic leaves through the rootless daemon's own uid. The
`agent.egress.*` config surface and mode vocabulary stay exactly as
documented here; only the enforcement mechanism for container-mode agents
changes. Host-user agents (today's default) keep the mechanism above.
