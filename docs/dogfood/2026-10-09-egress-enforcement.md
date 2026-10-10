# Dogfood 2026-10-09 — egress enforcement surface (run 25)

**Angle:** the egress policy (GAP-134, landed 2026-10-07) had never been exercised
live — its own close note says "live battery deferred". This run drove the
documented flag (`bunker spawn --egress-mode allowlist`), the config surface
(`agent.egress.*`), and the fresh-machine install path, on ephemeral and
coexist setups.

**Promise tested:** "allowlist/none modes install a default-deny nftables/iptables
chain keyed on the agent's uid; a failed install fails the spawn — the agent is
never left unenforced."

## What held up

- **Fail-loud contract.** Every broken enforcement path refused visibly:
  allowlist with zero destinations → `spawn failed at stage egress: allowlist
  resolved to zero destinations`, rc=1, agent rolled back (no user, no
  registry entry). A rule-install failure also rolled the spawn back — no
  unenforced agent was ever left running, exactly as the docs promise.
- **Open-mode law.** Two open-mode spawns + one destroy on bunker-mvp
  (HEAD daemon) left `nft list tables` with NO bunker_egress table — the
  "zero behavior change" pin holds in production behavior, not just tests.
- **Lifecycle on a fresh machine.** Clone → `go build ./cmd/bunker` (55s cold,
  Debian 13, module cache empty) → daemon up → invalid `--ttl banana` rc=1
  with an actionable message → `exec` of a missing agent rc=1 not_found →
  `destroy` of a missing agent rc=0 idempotent (DOGFOOD-005 fix holds).
- **Perf (Step 2b):** spawn 16.3s cold / 16.3s warm; destroy 18.1s (includes
  home archive). Far inside the documented 60-90s cold envelope — nothing a
  user feels, so no PERF row.

## What broke (rows DF-BUNKER-86/87/88)

1. **P0 — egress install is dead on nft hosts.** `nft -f` receives the rule
   content as the FILE argument (internal/egress/manager.go:251, executor at
   :162 has no stdin path). Every allowlist/none spawn fails: `Could not open
   file "table ip bunker_egress...".` Reproduced twice against a HEAD
   (ae13f7c8) root daemon with a seeded allowlist. The fail-loud contract is
   the only thing standing between this bug and unenforced agents.
2. **P1 — older daemons silently swallow the flag.** v0.1.4 (bunker-las-02)
   and bunker-mvp's daemon (built 04:50 UTC 10-07, ~8h before the egress
   merge) accept `--egress-mode allowlist`, exit 0, install nothing. A user
   who wires containment gets a fully open agent and no signal. Both fleet
   daemons predate the feature; no host has ever run enforced egress.
3. **P2 — docs/ergonomics.** docs/egress-policy.md ships no user-verification
   recipe (no `nft list table bunker_egress` check, no deny/allow curl pair);
   a non-root daemon start on a host where a root bunkerd ran collides with
   /var/lib/bunkerd/secrets and the error tells the user to chmod ROOT's file
   instead of naming the `agent.base_data_dir` / BUNKERD_SECRETS_DIR override.

## The working recipe (once DF-BUNKER-86 lands)

```bash
# daemon config (root daemon, nft host)
agent:
  egress:
    mode: open            # fleet default; per-spawn flag wins
    allowlist: ["1.1.1.1/32", "8.8.8.8/32"]   # + your resolver

bunker spawn myagent --egress-mode allowlist

# verify enforcement (user-visible recipe this run had to invent):
nft list table ip bunker_egress                 # table + output_hook + per-agent chain
nft list chain ip bunker_egress bunker-egress-$(id -u bunker-myagent)
# deny: curl http://8.8.8.8/ from inside the agent -> timeout/dropped (counter increments)
# allow: curl https://1.1.1.1/ -> 200
```

Non-root daemon alongside a system one: set `agent.base_data_dir` (and
`agent.registry.path`) under your home — the default /var/lib/bunkerd is
root-owned and the start will refuse on its secrets dir.

## Provenance

- Fresh install leg: ephemeral agent `6b3a0ca5` on bunker-las-02 (las-03 was
  offline; sibling-host substitution per the skill), clone of
  github.com/deployBunker/bunker at HEAD 7b3fecb, 55s cold CLI build,
  14s daemon build, lifecycle probes above.
- Enforcement probe: HEAD daemon (ae13f7c8, built 7s in coexist mode — scratch
  ports 19340/19341, /tmp state) run as root on bunker-mvp per the repo's own
  E2E pattern; production binaries/units untouched.
- All scratch agents destroyed and verified absent; HEAD daemon killed; /tmp
  scratch wiped; 0 leaked users/keys/chains on both hosts. No repo
  visibility/permission changes; no credentials minted or committed.
