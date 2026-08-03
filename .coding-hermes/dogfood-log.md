# Dogfood Log

| Date | Verdict | Promise (one-line) | Top findings | Time-to-first-success |
|------|---------|--------------------|--------------|----------------------|
| 2026-08-03 | 🟡 PROMISING-BUT-ROUGH | "Spin up isolated, resource-limited rootless-Docker environments via single CLI" — core lifecycle works, but 3 documented feature families (env, cp/deploy, tunnel) are broken from a remote client | 1) env set/get/list/unset all fail (unquoted sh -c arg join in server) 2) cp/deploy/tunnel SSH to unresolvable server hostname + server-side key path 3) invalid --ttl silently defaults to 6h | ~4 min (build + connect + spawn in 9s) |

## 2026-08-03 run detail

- **Promise statement:** "A user can spin up isolated, resource-limited Linux environments with rootless Docker on a remote host, controlled entirely through a single CLI: spawn, exec, mount, tunnel, destroy — with scoped API keys and TTL auto-expiry."
- **Method:** Built CLI from source (`go build -o bunker ./cmd/bunker`), connected to live bunker-mvp (78.46.173.180:19090), full lifecycle on scratch agent `dogfood-0803` + local non-root daemon test. All scratch agents destroyed; server back to 0 agents.
- **What held up:** spawn (9s), exec, rootless Docker run (`docker info` → rootless, 29.1.3), cgroup limits enforced exactly (memory.max=1073741824 for --memory 1GB, cpu.max=100000 for --cpu 1.0, pids.max=4096), metrics, heartbeat, destroy, no state leak on failed spawn, clean not_found for unknown agent exec/info.
- **What fell apart:** env (all 4 subcommands), cp, deploy, tunnel (remote-client SSH resolution + key path), invalid TTL validation, undocumented root requirement, raw userdel error on destroy-unknown, status metrics zeros.
- **Friction count:** 8 distinct (env family, cp, deploy, tunnel, bundle command strings, TTL validation, root docs, destroy error UX).
- **Artifacts:** tasks DOGFOOD-001..006 on board, docs/dogfood/2026-08-03-integration.md, docs/dogfood/diagnostics.md, skills/bunker-usage/SKILL.md.
- **Foreman:** active (900s cooldown, DecayRate 1, NamespaceID coding-hermes, tick #191 today) — no wake-up needed; picks up DOGFOOD tasks automatically.
