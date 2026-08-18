# Bunker — Real-Use Integration Report (2026-08-18)

Dogfood field test by coding-hermes-dogfood (cron). Verdict: **✅ SHIPPABLE**.
This report is the accurate, current usage reference — note that
`skills/bunker-usage/SKILL.md` is STALE (2026-08-03; see task DOGFOOD-007) and
contradicts this document in places (auth, env, cp/deploy/tunnel). When they
disagree, **this document wins** — every claim below was verified live on
2026-08-18 against the auth-enforced demo server.

## 1. What was tested / environment

- **Client:** kara's workstation (Linux, uid 1000, go 1.26.5).
- **Server:** live demo `bunker-mvp` — 78.46.173.180, gRPC :19090 / REST :18080,
  auth ENFORCED (Bearer token from `~/.bunker/config.yaml`), server binary v0.1.1
  (repo HEAD is v0.1.2 — see DOGFOOD-010).
- **CLI:** built from source (`make build`, 1.08s incremental, version 0.1.2
  baked in) AND `go install github.com/deployBunker/bunker/cmd/bunker@latest`
  (GAP-027 fix verified — v0.1.2 installs and runs).
- **Scope:** two full agent lifecycles + REST surface probe. Scratch agents
  only (`c31d8ee8`, `e8134667`); the pre-existing production agent
  `dexdat-dogfood` was never touched. Server left with exactly 1 agent (the
  pre-existing one), 0 leaked users/keys/run-dirs.

## 2. The verified workflow (copy-paste runnable)

```bash
make build                                  # or: go install github.com/deployBunker/bunker/cmd/bunker@latest
bunker connect http://78.46.173.180:18080 --token <token>   # writes ~/.bunker/config.yaml
bunker status                               # ONLINE, version, agents, CPU/mem/disk — 0.56s
bunker list                                 # current agents
bunker spawn --ttl 1h                       # ~10s → connection bundle (key, ports, TTL, sshfs/tunnel cmds)
bunker info <id>                            # status, expires, limits
bunker exec <id> -- uname -a                # isolated user: bunker-<id>
bunker exec <id> -- docker run --rm alpine echo hi     # rootless Docker works (3.4s first pull)
bunker env set <id> FOO=bar                 # NOTE: KEY=VALUE as ONE arg
bunker env list <id>                        # FOO=bar
bunker env get <id> FOO                     # bar (get needs the KEY arg)
bunker exec <id> -- sh -c 'echo $FOO'       # env visible in exec (sourced at start)
bunker cp /local/file <id>:/tmp/file        # scp round-trip
bunker metrics <id>                         # real memory/disk usage
bunker heartbeat <id>                       # TTL extended (acknowledged + new expiry)
bunker run <id> --detach --name job -- sh -c 'sleep 3; echo done >> /tmp/j.log'   # systemd transient unit, survives disconnect
bunker destroy <id>                         # 1.9s; server back to prior agent count
```

REST (same surface, JSON over HTTP):

```bash
curl -X POST http://78.46.173.180:18080/bunker.v1.Bunkerd/ListAgents \
  -H 'Authorization: Bearer <token>' -H 'Content-Type: application/json' -d '{}'
curl -X POST http://78.46.173.180:18080/bunker.v1.Bunkerd/ServerInfo \
  -H 'Authorization: Bearer <token>' -H 'Content-Type: application/json' -d '{}'
```

## 3. Errors hit and their resolutions (all user-side, none product bugs)

1. `bunker env set c31d8ee8 FOO bar42` → `bunker: env set requires exactly one
   KEY=VALUE argument` — my syntax guess was wrong; correct form is
   `env set <id> KEY=VALUE` (one arg). Help text documents it; README doesn't
   (DOGFOOD-009).
2. `bunker env get c31d8ee8` (no KEY) → `env get requires exactly one KEY
   argument` — same class; `env get <id> KEY` is correct.
3. REST probe `POST /bunkerd.v1.Bunkerd/ListAgents` → **404** — guessed the
   service name wrong. The connect-go path is `POST /bunker.v1.Bunkerd/<Method>`
   (proto package `bunker.v1`, service `Bunkerd`). Documented in
   docs/integration.md §3; not in README. No-auth probe with no Content-Type →
   415; with auth + correct path → 200.
4. `bunker spawn --ttl 1h demo-agent` → succeeded but created `c31d8ee8`
   (positional arg silently dropped). README's quick-start command implies a
   named agent; reality: use `--agent-id` (DOGFOOD-008).

## 4. What held up (promises kept)

- **Spawn → destroy lifecycle**: flawless twice, ~10s spawn incl. rootless
  dockerd bring-up, 1.9s destroy, TTL expiry honored (invalid `banana` →
  clean `invalid_argument: invalid ttl "banana": must match "\d+[hmd]"`).
- **Isolation**: `whoami` inside agent = `bunker-<id>`; limits visible in
  `bunker info` (CPU 2.0, mem 4 GB, disk 20 GB, 10 containers).
- **Rootless Docker**: `docker run --rm alpine echo REAL-USE-DOCKER-PASS` ✅
  (image pulled inside the agent's own daemon).
- **env**: set/get/list round-trip; `FOO=bar42` visible in subsequent exec
  (sourced from /run/bunker/<id>/env — DOGFOOD-001 fix holds).
- **cp**: file copied to agent, `cat` verified byte-identical.
- **metrics / heartbeat / run --detach**: real values; TTL extended; detached
  job ran to completion under its systemd unit (`bunker-run-<id>-<uuid>`).
- **Auth**: token required; wrong service path 404s; unauthenticated requests
  rejected. **destroy <unknown-id>** → `Agent no-such-agent-xyz not found.`
  exit 0 (idempotent, DOGFOOD-005 fix holds).
- **Version skew**: client 0.1.2 vs server 0.1.1 — no protocol breakage.

## 5. Frictions (→ board tasks)

| # | Friction | Evidence | Task |
|---|----------|----------|------|
| 1 | Repo usage SKILL.md teaches the opposite of reality (auth disabled, env broken, cp/deploy/tunnel broken) | skill claims vs live verification; stale since 2026-08-03 | DOGFOOD-007 (P1) |
| 2 | `spawn <name>` positional arg silently ignored | created c31d8ee8 for `demo-agent` | DOGFOOD-008 (P2) |
| 3 | README documents `bunker env` with no example | 2 usage errors during session | DOGFOOD-009 (P2) |
| 4 | Demo server v0.1.1 vs HEAD v0.1.2 | `bunker status` Version field | DOGFOOD-010 (P2) |

Non-tasks observed: REST path `bunker.v1.Bunkerd` is easy to guess wrong
(404 instead of a helpful error) — integration.md documents it, README
doesn't; minor.

## 6. Judgment

- **Does it work?** Yes — every promised workflow completed, twice, with
  clean teardown and 0 leaks.
- **Is it useful?** Yes — real utility: throwaway isolated Linux+Docker
  environments with enforced limits, driven remotely. The request-only demo
  token model is a deliberate access gate (README says so), not a defect.
- **Is it usable?** High — first success (`bunker status`) in 0.56s; full
  working agent in ~10s; 4 minor frictions over a 40-min session, none
  blocking. Never had to read source to proceed.
- **Is it trustworthy?** Yes — destroy idempotent, TTL honored, no leaked
  users/keys/run-dirs, auth enforced, invalid input rejected with clean
  connect codes.
