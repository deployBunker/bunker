# Dogfood 2026-10-12 — private-/tmp isolation chain re-proof + install leg

**Run:** bunker-dogfood tick 2026-10-12T23:42Z · target daemon bunker-mvp v0.2.0
(3f66c4e6, built 2026-10-10, uptime 2d5h) · CLI/daemon HEAD ae821785.

## Promise tested

README:55 + specs/agent-tmp-isolation.md claim: after `bunker host-provision
--apply` on a root daemon reporting `caps: isolation-grant`, every agent SSH
session (all `bunker exec`/cp/mount transports) gets its own private `/tmp`,
persists between that agent's sessions, and is invisible to peers and the host.

## What was driven (fresh evidence, first drive of this exact composition)

1. `bunker status --server bunker-mvp` → `/tmp: private (per-session
   pam_namespace instance)`; `bunkerd version` → `caps: isolation-grant`. ✅
2. `bunker exec df-tick1012 -- sh -c 'ls -ld /tmp; touch …; mkdir …'` →
   `/tmp` is `drwx------ bunker-df-tick1012:bunker-df-tick1012`, NOT the host
   `/tmp`. ✅ (1.26s warm)
3. **Cross-session persistence (spec line 182, never proven before):** a file
   and a directory created in exec #1 were both present in exec #2 on the same
   agent. ✅
4. **Install leg (PASSED, not skipped):** fresh ephemeral agent `df-tick1012b`
   (spawn 22.8s warm) on bunker-mvp; clone of the documented origin
   `https://github.com/deployBunker/bunker.git`; documented `make build`
   verbatim → rc=0; `./bunker version` → 0.2.0 commit ae82178 with
   `caps: isolation-grant`. Warm rebuild 5.8s; second cold tree `go build
   ./cmd/bunker ./cmd/bunkerd` 15.2s (26.3s user). No sudo needed inside the
   agent (Go toolchain present on the agent image). Destroy clean; residue
   counter 1 orphan user/home at +5s → 0 at +80s (60s reconciler swept it).
5. **Egress zero-chain re-proof (DF-BUNKER-86 premise):** `nft list ruleset` +
   `iptables -t filter -S` on bunker-mvp v0.2.0 → 0 `bunker-egress` chains
   with the daemon in open mode. The 2026-10-09 finding's environment now
   runs the fixed binary; the open-mode zero-firewall law holds on it.
6. Ephemeral-bunker leg host substitution: las-03 and las-02 offline
   (tailscale shows both offline; ssh timeout), bunker-mvp reachable — leg run
   there. Hostnames probed: mvp up, las-01/02/03/04 timed out.

## Verdict

🟡 PROMISING-BUT-ROUGH → the isolation chain itself is ✅ SHIPPABLE on v0.2.0;
the residual roughness is evidence-shaped, not functional: from inside the
agent, `drwx------ /tmp` is indistinguishable between a real pam_namespace
instance and a plain per-agent directory, so user-side proof still leans on
the daemon-side `status` line (DF-BUNKER-89).

Board rows filed: DF-BUNKER-89 (P2), DF-BUNKER-90 (P3, sudo-ladder docs gap).
No code or visibility changes; no credentials minted; all spawned agents
destroyed and verified absent.
