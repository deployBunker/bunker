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

## 2026-08-18 run detail

- **Verdict:** ✅ SHIPPABLE — full lifecycle verified live twice; 4 minor follow-ups.
- **Promise statement:** "A user can spin up isolated, resource-limited Linux environments with rootless Docker on a remote host, driven entirely from a single CLI (or connect-go gRPC/REST): connect → spawn → exec/docker → env → cp → metrics → heartbeat → run --detach → destroy, with `go install @latest` or `make build` as install paths and a live demo at 78.46.173.180."
- **Method:** Built CLI (make build, 1.08s) + `go install @latest` (GAP-027 verified: v0.1.2 installs/runs). Connected to auth-enforced demo server with stored token. Two full lifecycles on scratch agents c31d8ee8 + e8134667: spawn (10s) → info → exec (rootless docker run alpine REAL-USE-DOCKER-PASS) → env set/get/list (FOO=bar42 visible in exec) → cp round-trip → metrics (real values) → heartbeat (TTL extended) → run --detach (systemd transient unit, job completed) → destroy (1.9s). REST surface probed: `/bunker.v1.Bunkerd/ListAgents` + `ServerInfo` 200 with auth. All scratch agents destroyed; server back to exactly 1 pre-existing agent (dexdat-dogfood untouched); 0 leaked users/keys/run-dirs.
- **Time-to-first-success:** 0.56s (`bunker status`); full working agent ~10s.
- **Friction count:** 4 (stale usage SKILL.md teaching wrong reality; spawn positional name silently ignored; README lacks env examples — 2 usage errors; demo server version lag v0.1.1 vs v0.1.2). Plus 1 non-task: REST path `bunker.v1.Bunkerd` easy to guess wrong.
- **Top 3 findings:** (1) DOGFOOD-007 P1 — skills/bunker-usage/SKILL.md 15 days stale, says auth disabled + env/cp/tunnel broken (all fixed and re-verified); (2) DOGFOOD-008 P2 — `bunker spawn --ttl 1h demo-agent` silently drops the name arg; (3) DOGFOOD-009 P2 — README documents env with no examples.
- **Artifacts:** tasks DOGFOOD-007..010 on board (md + tasks.jsonl), docs/dogfood/2026-08-18-integration.md (current accurate usage reference), docs/dogfood/diagnostics.md §9 snapshot.
- **Foreman:** active (3600s cooldown, Enabled true) — no wake-up needed; picks up DOGFOOD tasks automatically.
- **Note:** tasks.md 2026-08-10 "Stand-In Gap Findings" section still shows GAP-027/028/029 without ✅ markers while tasks.jsonl marks them complete — cosmetic md/jsonl drift; validator counts those 3 rows, worth folding into a future board-hygiene pass.

## 2026-08-29 run detail

- **Verdict:** ✅ SHIPPABLE — same as 2026-08-18; two P1 findings on 3-day-old audit surface + metrics.
- **Promise statement:** "A user can spin up isolated, resource-limited rootless-Docker agent environments on a remote Linux host and drive them entirely from a single CLI: connect → spawn (named) → exec/docker → env → cp → run --detach → metrics → heartbeat → audit (new) → destroy, with auth enforced and the audit trail queryable remotely."
- **Method:** Built CLI HEAD (./bunker v0.1.3 commit 4af949d). Two full lifecycles on **bunker-las-03** (bunker-las-03:10002, v0.1.3): `dogfood-0829` (--agent-id, spawn 21.3s) → info → exec (isolated user, `$((6*7))=42` via rootless docker 29.7.2) → env set/get (visible in exec) → cp byte-round-trip → run --detach (systemd transient, /tmp/j.log) → heartbeat → metrics → **audit list/export (GAP-050) + verify --help** → destroy 1.96s. Second spawn `pos-test-0829` positional (DOGFOOD-008 fix confirmed, 49.8s < 300s). REST: 401 no-auth / 404 wrong path / 200 auth. Cleanup: 0 agents remaining; no leaked users.
- **Time-to-first-success:** ~0.5s (`bunker status` ONLINE on first call); full working named agent 21.3s.
- **Friction count:** 5 (metrics shows host memory — P1; audit exec records lack agent_id+remote_addr — P1; audit group help advertises --server on verify which rejects it — P2; destroy leaves client SSH key — P2; + 1 non-task: metrics "2.4 GB / 1.0 GB" incoherent output was the clue that triggered the metrics investigation).
- **Top 3 findings:** (1) DOGFOOD-011 P1 — `bunker metrics` reports HOST memory (2.4 GB used on a 1.0 GB-limit agent); cgroup.go reads host root cgroup/meminfo instead of the agent's user.slice. (2) DOGFOOD-012 P1 — audit trail: every ExecAgent record has empty agent_id AND remote_addr (streaming RPC, msg invisible to interceptor) → `--agent` filter misses exec records, the key forensics surface. (3) DOGFOOD-013 P2 — `bunker audit --help` promises --server on all subcommands; verify rejects it (local-only).
- **Artifacts:** tasks DOGFOOD-011..014 (tasks.md + tasks.jsonl, event 306), docs/dogfood/2026-08-29-integration.md (this run), docs/dogfood/diagnostics.md §10, skills/bunker-usage/SKILL.md v1.2.0 refresh.
- **Foreman:** active (21600s cooldown pin, Enabled true, tick #407 latest, board had 77/77 complete before this run) — picks up DOGFOOD-011..014 automatically on next tick. No scheduler PUT needed.
- **Note:** picker selected `bunker-sync` (DuckBrain sync entry, empty workdir, placeholder URL) — dogfooded the real `bunker` project it mirrors, per Step-0 manual-pick fallback.
