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
2026-09-01 | SHIPPABLE | 28s t2fs | friction 5 | 5 findings
2026-09-07 | PROMISING-BUT-ROUGH | 40s t2fs | friction 8 | 5 findings
2026-09-16 | SHIPPABLE | 20s t2fs (named spawn→bundle; status ONLINE 1st call) | friction 6 | 5 findings (DF-BUNKER-8..12)

## 2026-09-16 run detail

- **Verdict:** ✅ SHIPPABLE (third run in the series: SHIPPABLE → PROMISING-BUT-ROUGH → SHIPPABLE). Core lifecycle solid at HEAD CLI vs v0.1.3 fleet daemon; findings are grammar/consistency and version-drift classes, not broken promises at HEAD.
- **Promise statement:** "A user can spin up isolated, resource-limited rootless-Docker agents on a remote host and drive them entirely from one CLI: connect → spawn (named, --image-spec) → exec/docker/env/cp/run --detach → tunnel → audit → destroy, with auth enforced and the audit trail queryable."
- **Method:** CLI built at HEAD 66d4150 (make build). Live daemon bunker-las-04 (v0.1.3, uptime 1h48m). Full lifecycle × 4 named agents (df0916a/b/c/d): spawn 20s/44s(image-spec)/33s/15m-TTL, info, exec (user bunker-<id>, $((6*7))=42), env set/get visible in exec, cp byte-round-trip (agent:path form), docker run alpine in-agent (rootless 29.8.1), run --detach ($HOME proof), tunnel (local docker ps via :2376), metrics (agent-scoped 376.7MB), heartbeat (+4h), audit list --agent (attributed), destroy ×4 clean (no WARN — DF-BUNKER-5 fix live). REST probes: 401/405/401/200/200. Mixed-case: new registrations round-trip (KaraCaseTest); legacy lowercase-key+mixed-name configs still fail (narrow residual). las-03 daemon + ssh dead (deadline_exceeded) — install leg done via project's own spawn (agent df0916c: clone github HEAD 0bd45e4 → make fails (no make, DF-BUNKER-12) → go build ×2 exit 0 → version OK → destroyed).
- **Time-to-first-success:** 20s (spawn→bundle); exec+42 immediately after; full spawn→destroy cycle 21s.
- **Friction count:** 6 (exec rejects --server before agent-id; --timeout position trap; cp arg-form error noise; mount Connection-reset 2/2; /tmp promise vs v0.1.3 reality; las-03 dead-but-registered).
- **Top 3 findings:** (1) DF-BUNKER-8 P1 — exec-only flag grammar, misleading not_found (exec.go:201 peeler). (2) DF-BUNKER-9 P1 — README "Private /tmp per agent" unshipped in ANY tag; daemon advertises no isolation level. (3) DF-BUNKER-10 P2 — fleet daemon a major version behind repo (v0.1.3 vs 66d4150+; las-03 unreachable).
- **Artifacts:** board rows DF-BUNKER-8..12 (tasks.jsonl + tasks.md section), docs/dogfood/2026-09-16-integration.md, docs/dogfood/diagnostics.md §11, skills/bunker-usage/SKILL.md → v1.3.0 (stale "misses exec" line fixed; 5 new pitfalls).
- **Foreman:** NOT woken, NO scheduler PUT (fleet law 21600s pin; cooldown was 43200s at pick). DF-BUNKER-5 rework worker ran in-tree during this session (HEAD moved to 358a10c mid-run) — my commit stages only my own paths.
2026-09-16 | PROMISING-BUT-ROUGH | 110s t2fs | friction 12 | 5 findings


## 2026-09-18 run detail

- **Verdict:** 🟡 PROMISING-BUT-ROUGH (series: SHIPPABLE → PROMISING-BUT-ROUGH → SHIPPABLE → SHIPPABLE → PROMISING-BUT-ROUGH). The CLI is still fine; this run rode the REST protocol surface — the promise the CLI hides.
- **Promise statement:** "An integrator can drive the whole agent lifecycle over plain HTTP using docs/integration.md as the only reference: JSON POSTs to /bunker.v1.Bunkerd/<Method>, proto snake_case in / protojson camelCase out, {\"code\",\"message\"} envelopes, and a server-streaming ExecAgent for command output."
- **Method:** Target row `bunker-pm` (PM lane stand-in, no board of its own — the lane files onto the base board via scheduler.db, proven by its own GAP-078..083 rows at 07:15Z) → dogfooded the real project `/home/kara/bunker`. Built HEAD `967c331` (make build 6.0s, v0.1.4). Wrote a stdlib-only REST client (`bunker_rest.py` + `exec_stream.py`) from docs/integration.md alone and drove: ServerInfo/ServerMetrics/ListAgents → SpawnAgent (auto id + named) → ExecAgent streaming (`id`, `sh -c 'exit 3'`, `docker run --rm alpine echo REAL-USE-REST-PASS` = 9.0s, rootless alpine pull) → AgentMetrics (401 MB/8 GB) → HeartbeatAgent (TTL 1h→2h) → QueryAudit attribution → DestroyAgent. Probed mvp/las-02/las-03/las-04/control-host daemons. TTL-hygiene probe produced the run's biggest finding (below).
- **Time-to-first-success:** ServerInfo ~0.5s after the client existed; first REST-spawned agent ~20s (cold image) / 21.8s (control spawn); first REST ExecAgent ~10 min of protocol work (undocumented connect streaming).
- **Friction count:** 8 client-visible (415-with-no-envelope; two rejected EOS forms returning HTTP 200; undocumented chunked framing; undocumented base64 stdout; exitCode-only-when-non-zero; silent `name` drop; 5-minute silent spawn hang; 300s client timeout vs 5m0.001s server deadline) + the host-hygiene findings.
- **Top 3 findings:** (1) DF-BUNKER-21 P1 — spawn on a UID with stale systemd state hangs 5 min, fails `user manager did not start`, and the rollback fails too, leaving user+home+linger residue no API can see (145 orphan linger entries on las-02; control spawn on the same uid after cleanup: 21.8s success). (2) DF-BUNKER-22 P1 — ExecAgent-over-REST is undocumented and the doc-prescribed call returns a bare `415`. (3) DF-BUNKER-23 P2 — §5's `SpawnAgent` "name"/env-vars field list does not exist; unknown fields are silently dropped.
- **Install leg:** ephemeral Debian 13 agent on bunker-las-03 (the skill's designated host, daemon v0.1.3, 29% disk): clone from the repo's own public origin 2s (HEAD 967c331) → `make build` FAILS `sh: 1: go: not found` (Error 127; make present, Go absent) → Go 1.26.5 tarball installed → `make build` 49s → `./bunker --version` 0.1.4/967c331 (smoke PASS). No repo visibility/permission/clone-setting changes; existing access only.
- **Artifacts:** board rows DF-BUNKER-21..26 (tasks.jsonl + event 526 + tasks.md section), docs/dogfood/2026-09-18-integration.md (REST integration report incl. §4.5 spawn-failure chain), docs/dogfood/diagnostics.md §12, skills/bunker-usage/SKILL.md → v1.4.0 (REST unary + streaming recipes, 3 new pitfalls).
- **Cleanup:** every scratch agent destroyed (4 via REST, 1 via CLI); las-02 back to its baseline (kara-lair), las-03 back to 0; failed-spawn residue removed at the operator level and verified gone; the linger entry the failed spawn added was removed (host still carries 145 pre-existing ones — that is the finding).
- **Foreman:** NOT woken, NO scheduler PUT (fleet law: 21600s floor; the run's instruction also forbids touching cooldowns). Rows are picked up at the project's normal cadence.
- **Note for the picker:** `bunker-pm` and other pm-lane rows point at empty stand-in workdirs by design (the pm tick resolves the base project's workdir from scheduler.db) — the dogfood briefing reads that as "no board", so manual-pick fallback to the base repo is the right move (same as the 2026-08-29 `bunker-sync` run).
2026-09-18 | PROMISING-BUT-ROUGH | 20s t2fs (REST spawn; 0.5s status) | friction 8 | 6 findings (DF-BUNKER-21..26)
2026-09-19 | PROMISING-BUT-ROUGH | 15s t2fs (spawn at HEAD e344088, scratch daemon) | friction 6 | 5 findings (DF-BUNKER-28..32)


## 2026-09-19 run detail

- **Verdict:** 🟡 PROMISING-BUT-ROUGH (series: … → SHIPPABLE → SHIPPABLE → PROMISING-BUT-ROUGH). The lifecycle promises hold; this run's angle — the TRUST promises — found two P1s in exactly the surfaces a cautious user buys the product for.
- **Promise statement:** "A user can trust that agents die when their TTL expires, survive a daemon crash via the durable registry, can be stopped and started, expose only a scoped sub-key to third parties, and leave an audit trail whose hash chain proves tampering."
- **Method (fresh angle, 13th run):** prior runs covered CLI lifecycle, REST protocol, fresh-machine install. This run exercised the durability surface never touched before: TTL expiry (real, not parse-only), kill -9 crash replay, exact port restore, stop/start, agent-scoped sub-keys, audit chain verify, adopt/destroy reconciliation. Target: scratch bunkerd at HEAD e344088 on private ports (REST :18093 / gRPC :19092), private registry/audit/ssh/CLI-home under /tmp/df0919, jwt_secret set so spawn mints sub-keys, reconciliation mode adopt as orphan-safety on a host carrying another lane's agent. Fleet daemon (/opt/bunker/bunkerd) untouched throughout.
- **What held (first live proof for each):** TTL auto-destroy (dfdf-b --ttl 2m → reaped on the next 60s tick: "TTL expired, destroying agent", user removed, registry destroy event); kill -9 durability (daemon SIGKILLed with dfdf-a+dfdf-c live → restart replayed registry, restored:2 with EXACT port ranges — dfdf-c kept 20100-20199 — exec working immediately); stop/start lifecycle; sub-key service-class rejection (401 on Bunkerd service) and GetInfo clamping to own agent; audit caller attribution (agent:<id> key:<fp> for sub-key calls); **bunker mount WORKS at HEAD — first recorded mount success in the project's dogfood history** (2/2 connection-reset on 09-16 appear fixed); docker-in-agent (rootless alpine pull+run inside dfdf-a).
- **Time-to-first-success:** 15s spawn→bundle (progress line live); exec +42s later; full battery ~55 min including two kill -9 cycles and a reconciliation restart.
- **Friction count:** 6.
- **Top 3 findings:** (1) DF-BUNKER-28 P1 — Agent-service Metrics: no sub-key scoping (a sub-key reads ANY agent's metrics) AND no existence check (any caller gets a fabricated status:running + host memory for a never-spawned id; Heartbeat/GetInfo on the same ids behave correctly). (2) DF-BUNKER-29 P1 — audit hash chain breaks on EVERY daemon restart (chain head memory-only; first post-restart record has empty prev_hash; `audit verify` says "tamper detected" on an untampered log — reproduced graceful + SIGKILL). (3) DF-BUNKER-30 P2 — adopt mode silently destroys orphans lacking readable .bunker/ports (config comment understates the precondition; behavior itself is correct fail-closed).
- **Artifacts:** board rows DF-BUNKER-28..32 (tasks.jsonl + event 610 + tasks.md section), docs/dogfood/2026-09-19-integration.md, docs/dogfood/diagnostics.md §8, skills/bunker-usage/SKILL.md → v1.5.0 (durability recipes incl. --ttl 2m canary + kill -9 replay proof, 4 new pitfalls).
- **Install leg:** SKIPPED-install-bunker — the 09-18 run already proved fresh-machine installability on an ephemeral bunker agent (clone 2s, make build 49s after Go install, version smoke PASS) and this run's fresh-scratch-daemon-from-zero (empty registry dir, first-boot replay, listeners up) re-proved the from-zero path locally; the bunker host itself was not re-provisioned this run. No repo visibility/permission/clone settings touched.
- **Cleanup:** dfdf-a/dfdf-c destroyed via CLI (keys removed, users gone); ttl-canary dfdf-b reaped by the reaper itself; synthetic orphan bunker-dforph-a1 consumed by the reconcile test (home removed); scratch daemon stopped and port pair released (curl refuses, pgrep -x clean); host verified: zero bunker-dfdf-*/dforph-* users or homes; bunker-media-hermes (other lane) untouched.
- **Foreman:** NOT woken, NO scheduler PUT (2026-09-09 fleet law: 21600s floor; run instruction forbids cooldown changes). Rows picked up at normal cadence. Board HEAD moved twice mid-run (sibling commits 8a24090, GAP-112/113 wave) — my appends verified against the live tail after each write.
2026-09-20 | DOES-NOT-DELIVER | 40s t2fs (spawn->exec) | friction 14 | 6 findings (DF-BUNKER-38..43) | install_seconds=48 spawn/49 make-build | bunker=las-03 agent=df1017i | smoke=ok

2026-09-25c | PROMISING-BUT-ROUGH | stable identity + drift report real; renew kills client SSH key, gate unsatisfiable agent-side / absent on deployed daemon, standard re-spawn fails containment gate | 3 findings (DF-BUNKER-65..70) | install=battery launched (dfda436e, las-03) | bunker=las-03 agents=df-renew-0925+dfda436e | smoke=see QA lane
2026-09-25d | PROMISING-BUT-ROUGH | ops/maintenance surface (stop/start/restart, homes/linger, agent-tools, docker tunnel) + install leg on cube-las-00 (las-03 down) | DF-BUNKER-71 P1 release binaries still lose the spawn SSH key (DF-59 fix 597 commits ahead of tag; SSH family dead on release agents) · DF-BUNKER-72 P1 uid-recycle RootlessKit stale lock kills rootless docker on an agent reporting 'running' · DF-BUNKER-73 P2 no key-recovery verb · DF-BUNKER-74 P2 README raw-URL installer 404s | exec RT 1.25s±0.04 (hyperfine ×10); spawn 16s; install_seconds=6; smoke=ok

## 2026-09-20 run detail

- **Verdict:** 🔴 DOES-NOT-DELIVER (series: … SHIPPABLE → SHIPPABLE → PROMISING-BUT-ROUGH → DOG FOOD). The lifecycle and the isolation boundary are fine; the run found a P0 whose blast radius is the README's flagship command and a P1 that makes a documented isolation feature unusable fleet-wide.
- **Angle (14th run):** the AGENT ISOLATION boundary — private `/tmp` promises + the cross-agent exchange point — and `bunker mount`, the command those docs lean on. Prior runs did CLI lifecycle, REST, fresh-machine install, durability/trust.
- **Promise statement:** "After host provisioning, a user's agents are isolated from each other and from the host (private `/tmp` per session and per detached unit); they hand files between exactly two agents through one bounded, kernel-capped directory `/srv/bunker-share`; and any agent's filesystem can be mounted locally with `bunker mount <id> /mnt/agent`."
- **Method:** CLI built at HEAD `93d7a53` (`make build` 6s). Four live agents on **bunker-las-03** (daemon 0.1.4 `6a6ad20`, `isolation-grant`): `df1017a`/`df1017b` (isolation pair), `df1017c` (G3 detached-unit test), `df1017u` (umount test), plus ephemeral `df1017i` (install leg). Read-only `status --all` against las-01/02/04/mvp/cube-las-00. Root probes via `bunker3-root` (stat/mount/`su`), an `ssh` shim on PATH to capture the CLI's exact exec, and a 20-line Go repro of the defective helper.
- **Time-to-first-success:** ~40s (spawn → `exec` returns). Full battery ~2h20m including two agent spawns per test.
- **Friction count:** 14 — 4 mount-form failures, 1 `umount` exit-1 with the same root cause, 3 `run --server`-placement refusals, 3 `env` arity refusals (a swallowed `--server`), 2 piped-exit-0 traps, 1 scratch-EACCES family.
- **Top 3 findings:** (1) **DF-BUNKER-38 P0 — `bunker mount` is completely broken at HEAD.** `runWithTimeout` (`internal/cli/mount_preflight.go:441`) calls `cmd.Start()` then `cmd.CombinedOutput()` on the same `*exec.Cmd` → `exec: already started`, empty capture, and the caller reports "remote path … does not exist or is not a directory" for a path that exists. `mount_test.go:230` stubs `remotePathCheck` and `proc_lifecycle_test.go:120` sets `BUNKER_SKIP_MOUNT_PREFLIGHT=1`, so the real function has zero coverage — this is why the suite stays green. The same helper makes `bunker umount <mountpoint>` always fail with `exec: already started`. (2) **DF-BUNKER-40 P1 — the shared scratch exchange point is unreachable on every host but bunker-mvp**: root `/srv/bunker-share` is `750 root:root` (docs: `2750 root:bunker-agents`), so the README's cross-agent `cp` example gives `Permission denied` while the daemon logs `"shared scratch ready"`; only `bunker host-provision --apply` calls the function that creates the root. (3) **DF-BUNKER-39 P1 — the documented optional-mountpoint form can never mount** (preflight called with an empty path); plus DF-BUNKER-41/42 (`run`/`env` missed the DF-BUNKER-31 flag-grammar unification) and DF-BUNKER-43 (piped failures exit 0).
- **First live proofs (hold — do not re-litigate):** **G3 detached-unit `/tmp`** — on the same agent `run --detach` sees a `/tmp` with ONE entry and cannot see the host marker the `exec` session reads; agent-to-agent `/tmp` isolation; the scratch cap as a real kernel bound (300 MiB → exactly 268435456 bytes in a 256 MiB tmpfs); DF-BUNKER-28 fabricated metrics genuinely fixed.
- **Artifacts:** board rows DF-BUNKER-38..43 (tasks.jsonl + events 626-631 + tasks.md section), `docs/dogfood/2026-09-20-integration.md`, `docs/dogfood/diagnostics.md` §13, `skills/bunker-usage/SKILL.md` → v1.6.0 (mount-is-broken + scratch-precondition + isolation-verified sections).
- **Install leg:** PASS, fresh machine = ephemeral agent `bunker-df1017i` (bare Debian 13, non-root, no Go/sshfs, sudo locked). One-command installer: SHA256-verified both binaries before writing, no sudo escalation, smoke `--version` 0.1.4; `--dry-run` clean. Source path: clone 2s @ `93d7a53`, `make build` refuses cleanly without Go (README's `check-go` guard, not `Error 127`), Go 1.26.5 → build 49s, `bunkerd caps: isolation-grant`. `scripts/install.sh --build` PASS. Not SKIPPED — the ephemeral-bunker leg ran in full and was destroyed afterwards.
- **Cleanup:** df1017a/b/c/u/i all destroyed via the CLI (keys removed); host verified: no `df1017` users, no `/home/bunker-df1017*`, no scratch mounts, `/srv/bunker-share` back to no agent dirs, host `/tmp` marker removed. No repo visibility/permission/clone setting touched.
- **Foreman:** NOT woken, NO scheduler PUT (fleet law: 21600s floor; this lane's instruction forbids touching cooldowns). Rows are picked up at the project's normal cadence. Note: the project's latest tick `bunker-2026-09-20-08-53-27` shows `status: timeout` (stale at 1h30m) — worth a look by the never-done lane, not acted on here.
2026-09-23 | PROMISING-BUT-ROUGH | ~35s spawn→bundle; key ops <1s each | friction 6 | 3 findings (DF-BUNKER-45..47) | install_seconds=battery on fresh agent (see tasks.md) | bunker=mvp+las-03 agent=ephemeral, destroyed | smoke=ok

## 2026-09-22/23 run detail (15th run; continuation of the interrupted 23:11 session)

- **Angle:** the GAP-132 key-lifecycle surface (list/rotate/revoke + audit), live on the demo daemon (bunker-mvp), because GAP-139b (Bane 09-22) demanded live deployment + acceptance there. Prior run (09-20) did isolation/mount; the 09-22 session did deploy+revoke groundwork before the gateway drop.
- **Promise statement:** "A user can list, rotate (zero-downtime overlap), and revoke API credentials on a live daemon, with audit records for every lifecycle event."
- **Continuation notes:** resumed the interrupted session's work — deploy leg was already done (daemon at 16fff6d), gitreins GAP-139b was in_progress; this session ran the acceptance legs to completion, kept its two rotation events as live evidence, and destroyed its two leftover acct agents.
- **What held:** revoke is genuinely immediate and durable (401 at t+0 on Agent/GetInfo; the previous session proved across-restart durability); audit chain records both lifecycle events with hashes; key list metadata clean; spawn→bundle ~35s; master-only scoping enforced by design (sub-key on admin RPC = scope denial, not a leak).
- **What failed (the headline):** rotate is a no-op on the live path — DF-BUNKER-45 P0, proven by minting JWTs with the boot vs returned secrets across three rotations (boot-secret JWT valid after all three; rotated-secret JWT never valid). The Tier 2 judge's criterion-3 check passed via the static master token, which bypasses the JWT secret entirely — a false negative the dogfood evidence corrects.
- **Friction count 6:** rotate-output self-lockout + recovery; --name flag trap (spawn takes positional agent-id); exec-into-agent has no token (by design, undocumented); the scope-vs-invalid 401 ambiguity (bit both this run and the interrupted one); empty `-i` in the tunnel command; PIPESTATUS trap hit once.
- **Artifacts:** board rows DF-BUNKER-45..47 (+events 715-717), tasks.md section, docs/dogfood/2026-09-22-integration.md, diagnostics §14, skills/bunker-usage/SKILL.md → v1.7.0 (credential model + rotate P0 warning).
- **Install leg:** PASS — ephemeral fresh-machine battery on bunker-las-03 agent 2b991bc9 (destroyed after; ambiguous corruption-restart cell recorded, not hidden). Not SKIPPED.
- **Cleanup:** all bg139b-* agents destroyed (mvp: 0 left); no repo visibility/permission changes; no cooldown/scheduler changes; leftover /tmp secret files shredded (a boot-secret string leaked into a tool transcript once — it is one rotation stale and the daemon was rotated twice more since, rendering it inert).
- **Off-by-one:** non-trivial debug submitted post-debug: bunker-key-rotate-output-misleading (sub_11bcdd, queued).
2026-09-23 | PROMISING-BUT-ROUGH | 0.5s exec, 6.4s deploy, 14s install | friction 5 | 5 findings (DF-BUNKER-48..52) | install_seconds=14 (fresh agent, install.sh) | bunker=las-03 agent=f0901fd3+e419d763, destroyed | smoke=ok

## 2026-09-23 remote dev workflow run (16th run)

- **Angle:** the remote development workflow surface — deploy, cp, exec, run, env, tunnel, mount — used end-to-end to build and test a Go project on a remote agent. Prior runs covered CLI/spawn, isolation/mount, and key lifecycle; this is the first run to exercise the full deploy→build→test→run loop.
- **Promise:** "A user can deploy a project to a remote agent, build it, run tests, manage env vars, and access Docker — all from the local CLI."
- **What held:** deploy (6.4s), cp (4.4s), exec (0.5s), exec --script (0.6s), run (0.6s), env persistence (set/get/list/unset all <1s), tunnel (full Docker access in 5s), mount (with HEAD CLI). The core loop works end-to-end. Docker tunnel is excellent — `docker run --rm hello-world` through the tunnel just works.
- **What failed:** (1) env PATH silently overridden (DF-BUNKER-48 P1 — user-set PATH in env file is ignored, SSH session PATH wins); (2) installed CLI binary (00c3555) predates mount fix 653d763 — `bunker mount` fails completely with the released binary (DF-BUNKER-49 P1); (3) `bunker umount` checks wrong path for custom mountpoints, says "already clean" while mount is active (DF-BUNKER-50 P2); (4) `run --detach` prints a systemd unit name but the process is orphaned to PID 1, no unit exists (DF-BUNKER-51 P2); (5) agent image lacks Go — first thing a remote dev hits (DF-BUNKER-52 P2).
- **Artifacts:** docs/dogfood/2026-09-23-remote-dev-workflow.md, diagnostics §15, board rows DF-BUNKER-48..52.
- **Install leg:** PASS — fresh agent e419d763, install.sh 14s, SHA256-verified, smoke ok. Not SKIPPED.
- **Cleanup:** f0901fd3 + e419d763 destroyed via CLI; local mount cleaned; no repo/cooldown changes.
2026-09-24 | PROMISING-BUT-ROUGH | 27.1s spawn / 0.47s probe / 0.56s warm exec | friction 2 filed + 2 minor | 2 findings (DF-BUNKER-57..58) | install_seconds=n/a (bunker-qa battery; fresh-install cell OK) | bunker=las-03 agents df-agenttools-0924+695732de, destroyed | smoke=ok

## 2026-09-24 agent-tools + lifecycle run (17th run)

- **Angle:** the surfaces runs 1-16 never touched — agent-tools (probe + --install tool delivery), stop/start/restart lifecycle, homes/linger/registry maintenance.
- **Promise:** "A fresh agent ships without the tools the remote editing verbs need; one command probes it, one more delivers the vendored tools onto the agent's own PATH and re-proves it. Stop keeps state, start resumes, restart resets TTL."
- **What held:** probe contract (missing = data, exit 0); dynamic-link refusal with the exact fix in the message; delivery re-probes on the agent (delivered toolsd ran, version echoed); state survives stop→start AND stop→restart; restart grants full default TTL (2h→4h04m measured — surprise, not a bug); homes/linger classify stale/kept with dry-run-first prune; status honestly WARNINGs HOST-SHARED /tmp; in-agent exit codes propagate (false→1, exit 7→7).
- **What failed:** DF-BUNKER-57 P1 — probe names rg+gopls REQUIRED but --install can deliver neither on SSH-based agents (image-spec path unreachable there); DF-BUNKER-58 P2 — registry has no read verb, unknown subcommand exits 0. Minor unfiled: "the toolkit repo" unnamed in the refusal error; restart TTL surprise.
- **Artifacts:** board rows DF-BUNKER-57/58 (events 750/751), tasks.md section, docs/dogfood/2026-09-24-agenttools-lifecycle.md, diagnostics §16, skills/bunker-usage/SKILL.md → v1.8.0.
- **Install leg:** PASS — bunker-qa.sh battery, fresh-install OK (go build v0.1.4), upgrade v0.1.3→HEAD clean, chaos-errorpath OK; ci-pass FAIL is harness (no act-triggerable workflow; native suite ok). Not SKIPPED.
- **Perf:** warm exec 559.7ms ± 50.7ms (hyperfine 10 runs), probe 0.47s, delivery 21.9s, homes/linger ~6ms — no user-noticeable slowness, no PERF row (skill §2b: a win nobody can feel is not a finding).
- **Cleanup:** both agents destroyed (df-agenttools-0924 manual, 695732de by collect); ~/.bunker/config.yaml md5 3a5a07ffed5c721a2219b21ddab3e042 unchanged (pre-run backup /tmp/dogfood-bunker-config-backup.yaml); no repo visibility/permission changes; no scheduler changes.

## 2026-09-25 run detail (18th run)

- **Angle:** the RAW-REST integrator surface — driving the daemon without the CLI (unary JSON, the ExecAgent streaming envelope, the GAP-128 credential-fetch path the CLI itself depends on). Runs 1-17 never skipped the CLI; this is the first run a non-CLI client wrote.
- **Promise:** "An integrator can drive the full lifecycle over plain REST using only docs/integration.md and the proto — no CLI."
- **What held:** every documented wire behavior reproduced first-try on bunker-mvp (daemon 0.1.4 f525639, auth enforced): snake_case-in/camelCase-out, int64-as-string, 401/405/404 taxonomy, the two DestroyAgent idempotency cases (200 destroyed vs 404 not_found), audit hash-chain on every call, ExecAgent envelope streaming (connect+json in/out, chunked responses, base64 stdout, exit-code-omitted-when-zero, errors INSIDE the 200), RunAgent detach-only semantics, detached process surviving the SSH session. Full lifecycle driven end-to-end over raw REST.
- **What failed (the headline):** DF-BUNKER-59 P1 — `bunker spawn` on an auth-enforced daemon cannot fetch the agent SSH key ("could not fetch SSH key: unauthenticated: missing Authorization header", no key saved). Root cause proven with a standalone Go repro: spawn.go sets Authorization only on the SpawnAgent request; the GAP-128 GetAgentKey follow-up sends none. Raw REST with the same token gets the key (200), so it is purely client-side. Blast radius: agents are ssh/mount/cp-dead from the spawning CLI, and the QA harness's ssh reachability probe then destroys healthy agents 3× (observed live). Workaround documented (GetAgentKey via curl).
- **Other findings:** DF-BUNKER-60 P2 — fresh-machine `make test-short` RED non-root (host-provision tests write /etc/pam.d for real; 23 packages ok, internal/cli fails); DF-BUNKER-61 P2 — CLI destroy deadline_exceeded while raw REST destroys the same agent fine; PERF-002 P2 — bunker-qa.sh 4-space YAML preflight grep (script-side, patched in place, recorded).
- **Install leg:** PASS — REAL public clone this time (repo is public; no tar-stream fallback needed): clone 2s @ f525639, scripts/install.sh 3s (SHA256-verified, smoke 0.1.4), make build RC=0 ~60s, make test-short FAIL (=DF-BUNKER-60, not a build problem). Battery cells not completed (harness blocked by DF-BUNKER-59's ssh probe) — recorded, not SKIPPED. All ephemeral agents destroyed and verified absent host-side.
- **Friction count 7:** connect envelope framing not copy-pasteable anywhere but docs §5 (had to reverse-engineer chunked+envelope layering), key-fetch failure, destroy deadline ×3, preflight grep, v0.1.3-vs-HEAD binary confusion, timeout-secs field name discovery, PIPESTATUS trap.
- **Artifacts:** board rows DF-BUNKER-59/60/61 + PERF-002 (tasks.jsonl tail + events 752-755, verified: 0 bad lines, no new dupes, diff = exactly 4 rows), docs/dogfood/2026-09-25-rest-credential-surface.md, docs/dogfood/2026-09-25-rest-probes/ (runnable exec_agent_rest.py + rest_lifecycle.sh), diagnostics §17, skills/bunker-usage/SKILL.md → v1.9.0.
- **Perf (Step 2b):** REST lifecycle calls ~0.35s each warm; REST spawn 18.1s (rootless-docker install, matches the documented 60-90s/first-spawn expectation on a cold host); CLI exec 1.06s; install 13s. Nothing a user would notice as slow — no PERF row for latency. The QA-harness preflight failure is a correctness row (PERF-002), not a latency one.
- **Cleanup:** 10 agents created this run (df-raw/rest/run/run2/tmp/auth ×2 + keytest* + repro-key + 3 QA orphans) — ALL destroyed via CLI or raw REST and verified by empty ListAgents + host-side user/home absence. ~/.bunker/config.yaml token refreshed from the host's own config (old local token was stale; md5 changed 3a5a07→8c1fdf — token VALUE only, no structural change; pre-run backup /tmp/dogfood-bunker-config-backup-0925.yaml). No repo visibility/permission changes; no credentials committed; scheduler untouched.

## 2026-09-25b run detail (19th run — TLS trust surface)

- **Angle:** the surfaces runs 1-18 never touched — the GAP-127 TOFU certificate pinning + GAP-141 `tls_insecure` knob (the README's "secure path is the easy path" promise) plus daemon-side TLS behavior. Nine-arm battery on a scratch HEAD daemon (a4e98ce), README-config-verbatim, private ports, fleet daemon untouched (NRestarts 0).
- **What held (first live proof for each):** TOFU fingerprint print + pin store; cert-change refusal naming BOTH fingerprints + `--accept-cert` remedy; pin-wins over `tls_insecure` (flag, one-line, AND hand-edited config) with or without ack; per-invocation ack (ack'd connect does NOT carry to the next command); `[TLS-UNVERIFIED]` stamped on RPC record AND correlated `/command` record (README claim verbatim); expired-pin refusal with notAfter + remedy and NO pin stored on refusal; POST-only REST matrix (405 GET-before-auth); audit chain verify OK (23 records incl. stamped ones); legacy-secret startup warning (GAP-129); rootless-cache warm spawn 11.5s; destroy live-process gate refuses with exact pid evidence.
- **What failed:** DF-BUNKER-63 P0 — spawn allocated uid 1001 to agent fad4b89a while an orphaned host-docker container (imhotep-backend-1, started Sep 23 by the PREVIOUS uid-1001 agent) still runs as uid 1001: agent measured `kill -0` + /proc visibility over the production process (isolation broken), destroy refuses forever (gate correct, but no spawn pre-check, no bypass, `--force` does NOT bypass), TTL reaper loops unbounded, agent zombie-'running'. DF-BUNKER-62 P1 — README quick start (connect → status → spawn, verbatim) fails 'no target bound'; mutating commands need --server/BUNKER_SESSION_TARGET every call, `bunker use` doesn't help, help text still says 'default: active server'. DF-BUNKER-64 P2 — TLS refusal UX: status exit-0 on refusals (list exits 1), TOFU banner persistence claim false on auth failure, config refusal as 'stream error: unavailable:', http-vs-TLS '400 Bad Request'.
- **Friction count 6:** 6× no-target-bound refusals following the docs (62), banner persistence claim (64), status exit-0 discovered only by checking $?, stream-error class (64), REST 415s without Content-Type (documented in morning run), transient curl:23 on the install leg (retry OK).
- **Verdict:** ✅ SHIPPABLE on the TLS surface — every trust promise held in all nine arms; the P0 lives one layer down (uid allocation + lifecycle reaping), not in TLS.
- **Artifacts:** board rows DF-BUNKER-62/63/64 (tasks.jsonl tail + events 759-761, verified: 0 bad lines, no new dupes, my rows unique), tasks.md section, docs/dogfood/2026-09-25b-tls-trust-surface.md, diagnostics §18, skills/bunker-usage/SKILL.md → v1.10.0.
- **Perf (Step 2b):** warm pinned-TLS RPC 7.5ms ± 0.7ms (hyperfine 20 runs); cold TOFU connect 0.01s; warm cross-network list 362ms ± 12ms (network RTT, not CLI); warm spawn 11.5s (rootless cache); fresh install 15s (clone 10s). Nothing user-noticeable — no PERF row (perf law: a win nobody can feel is not a finding).
- **Install leg:** PASS — ephemeral agent 5284392c on las-bunker-03 (2h TTL): public clone 10s @ a4827b2, documented one-command install (fetched to /tmp then `sh`; remote pipe-to-shell gated interactively) 15s, SHA256-verified, smoke OK (release 0.1.4 @ 235e715 — freshness note holds: tag lags HEAD a4e98ce), PATH warning correct. Agent destroyed via CLI, verified gone host-side (user + home).
- **Cleanup:** fad4b89a (reaper-zombie via uid collision) — user removed host-side with userdel after evidence capture, production container verified Up/healthy; df0925perf destroyed via CLI (exit 0, key removed); 5284392c destroyed (exit 0, key removed); host census: 0 bunker-* users, 0 orphan homes. Scratch daemon stopped, 18093/19093 released. ~/.bunker/config.yaml untouched (md5 unchanged). No repo visibility/permission changes; no credentials committed; scheduler untouched (21600s law respected); no cooldown changes, foreman not woken (active tick 540 observed mid-run).

## 2026-09-25c run detail (20th run — renewal / stable-identity surface)

- **Angle:** the never-tested `bunker renew` workflow (docs/renewal.md, DF-BUNKER-34's
  fix): drift pre-flight + destroy live-process gate + same-id re-spawn, as an
  operator renewing a long-lived agent (real footprint seeded: units, cron, configs).
- **What held:** stable identity across renew (same id/uid/home, 39s end-to-end);
  the drift report at HEAD is genuinely good (file:line hits, found the agent
  image's own rootless-docker unit paths); home-wipe honesty; the destroy gate
  fires at HEAD with exact pid evidence; `open`-preset lifecycle fully clean.
- **What broke:** DF-BUNKER-65 P1 (renew never re-fetches the client SSH key —
  ssh/cp/mount dead after every successful renew; no recovery on the deployed
  daemon, which predates GetAgentKey 325da4c); DF-BUNKER-66 P1 (destroy-gate
  remedy unsatisfiable agent-side — rootless session services respawn; gate
  ABSENT on deployed 0.1.4 → renew destroys agents with live processes);
  DF-BUNKER-67 P1 (containment landing gate: 125ms code budget vs MEASURED
  125-349ms systemd drop-in landing on las-03 → standard spawn hard-fails 3/3,
  run 19 same box/binary passed this morning; `--preset open` unaffected; renew
  has no --preset); DF-BUNKER-68 P2 (failed renew leaves agent destroyed, no
  archive/recovery hint); DF-BUNKER-69 P2 (release 0.1.4 client: bare 'unknown
  command "renew"'); DF-BUNKER-70 P2 (drift RPC 404 on old daemon degrades to
  one warn line, renew continues unscanned).
- **Board:** 6 rows + 6 events appended (tasks 436→442, events 766→772, 0 bad
  lines, my ids unique; pre-existing MOUNT-011 dupe noted, untouched). Verified
  via git diff after commit.
- **Artifacts:** docs/dogfood/2026-09-25c-renewal-identity.md (incl. version-skew
  matrix), diagnostics.md §19, skills/bunker-usage/SKILL.md → v1.11.0,
  tasks.md run-20 section.
- **Install leg:** bunker-qa.sh launch PASS (agent dfda436e @ las-03, fresh-install
  + upgrade-prep cells started) — collection RUNNING at report time; result to be
  folded into the board by the QA lane. Launch-side harness errors recorded
  (docker pull arity + '[: skip: integer expected' in upgrade-prep prep).
- **Perf (Step 2b):** renew end-to-end 39s (clean agent, fleet daemon); failed
  renew 49s to loud error; drift scan sub-second. systemd drop-in landing
  latency probe: 349ms incl. daemon-reload (the DF-BUNKER-67 number). Nothing
  user-noticeable beyond the failures themselves — no PERF row; the 125-vs-349ms
  gap IS a correctness row (67).
- **Cleanup:** df-renew-0925 destroyed via CLI (verified list-empty); scratch
  HEAD daemons: control-host one never spawned agents (stopped; uid-1001
  collision precondition avoided), remote scratch daemon on las-03 stopped and
  ports released after evidence capture. ~/.bunker/config.yaml md5 8c1fdfd7…
  unchanged (all scratch work in /tmp configs). No repo visibility/permission
  changes; no credentials committed; scheduler untouched.

## 2026-09-25d run detail (21st run — ops/maintenance surface)

- **Angle:** everything AFTER the first hour of an agent's life, which runs 1-20 never drove as one workflow:
  stop/start/restart lifecycle, host residue tools (homes/linger), agent-tools, the docker-tunnel path on a
  release daemon, and the ephemeral install leg.
- **Promise statement:** "A user can spin up isolated agents from one CLI and operate them over their whole
  life — and keep the host clean with the residue tools."
- **What held (live, bunker-mvp 0.1.4):** exec/env/docker RPC verbs on every agent incl. keyless; HEAD-CLI
  lifecycle: cp byte-verified (7.0s), ssh verb, stop→start→restart with files surviving and restart resetting
  TTL; destroy ×5 clean (local keys removed; servers back to pre-run agent counts); homes/linger classify
  honestly and refuse --server by design; agent-tools dependency report accurate; install leg (cube-las-00 —
  las-03 DOWN, ssh 100.69.3.13 timed out): release-asset installer on a bare agent → both binaries + smoke OK
  in 6s.
- **What fell apart:** (1) DF-BUNKER-71 P1 — release-channel spawn never delivers the client SSH key (DF-59 fix
  19892c3 is 597 commits past the v0.1.4 tag): warn at spawn, keyless agent, cp/ssh/mount/tunnel all dead while
  the agent looks fine. Proven both directions against the same daemon (release CLI warn+keyless; HEAD CLI key
  delivered, SSH family works). (2) DF-BUNKER-72 P1 — fresh agent dfops0925c: rootless dockerd failed (stale
  RootlessKit lock, uid 1007 recycled same-day from conctest-3-91761), agent 'running' throughout, found via the
  documented tunnel workflow. (3) DF-BUNKER-73 P2 — no recovery verb for keyless agents (GetAgentKey exists, no
  CLI surface). (4) DF-BUNKER-74 P2 — README raw-URL installer 404s; PATH export undocumented.
- **Friction count:** 4 findings, 1 dead host (las-03), 1 dirty-checkout build trap (worked around via
  git archive; lesson recorded in the usage skill).
- **Perf (Step 2b):** exec round trip 1.25s ± 0.04s (hyperfine, 10 runs, warm) — comfortably fast; spawn 16s;
  cp 7s; stop 1.6s; start/restart 0.4s; install 6s cold; homes scan 2.5s/581 entries. No PERF row filed:
  nothing was slow enough for a user to notice; the run's pain was correctness.
- **Artifacts:** docs/dogfood/2026-09-25d-ops-surface.md, diagnostics.md §20,
  skills/bunker-usage/SKILL.md → v1.12.0, board rows DF-BUNKER-71..74 (commit 9350506).
- **Cleanup:** all 5 agents destroyed + verified via list; tunnel process killed; scratch build tree in /tmp
  only. No repo visibility/permission changes; no credentials minted or committed.
- **Verdict:** 🟡 PROMISING-BUT-ROUGH — HEAD-grade operator experience, release-channel delivery gap.
