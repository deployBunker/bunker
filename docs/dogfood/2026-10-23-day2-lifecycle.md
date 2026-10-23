# 2026-10-23 — Day-2 lifecycle surface (run 29): pause/resume, env, TTL, resource ceilings

**Target:** `bunker` (CLI + `bunkerd`) — `/home/kara/deployBunker/bunker`
**Provenance:** HEAD `f062d19a6460d3c11c0a26682eb3b176f53fc2f5` (built in a detached
worktree `/tmp/df1023b/src` — the primary checkout was dirty with other lanes'
board writes, so the binary under test is HEAD and nothing else), live daemon
`bunker-mvp` 0.2.1 (`http://100.115.67.116:18080`) reached over Tailscale.
**Tick:** `bunker-dogfood-2026-10-23-14-48-42`

## The promise this run grades

README:917-984 presents a **day-2** story a real user needs right after the first
spawn:

* `bunker env set/get/list` — "the env file is automatically sourced at the start
  of every `bunker exec` … therefore persist across exec and run invocations"
  (`bunker env --help`).
* `bunker heartbeat` — "extends the TTL, it never shortens it … an existing
  longer expiry is never shortened" (README:932-946).
* `bunker stop` — "pauses an agent, it does not destroy it … keeps user/home/
  container/ports"; `bunker start` "re-arms it"; `bunker restart` "stops and
  starts in one call to recover a wedged session" (README:920-930, 969-984).
* `bunker metrics` / `bunker resource-events` — "Unavailable is not clean …
  a stopped or destroyed agent … says UNAVAILABLE/NOT REPORTED" (README:1082-1102).

So: **a user can pause an agent, come back, and keep working in it — and the
health/ceiling read-outs tell them the truth about what they have.**

## What was actually done (one agent, as a user)

```bash
cd /tmp/df1023b && go build -o bunker ./cmd/bunker          # real 4.000s (warm cache)
./bunker spawn df1023-day2 --server bunker-mvp --ttl 12h    # 19.74s
./bunker exec df1023-day2 --server bunker-mvp -- 'echo hello-from-agent; id; hostname'
./bunker exec df1023-day2 --server bunker-mvp -- 'docker ps; docker version --format "{{.Server.Version}}"'
./bunker env set  df1023-day2 --server bunker-mvp DF1023_TOKEN=day2-value
./bunker env set  df1023-day2 --server bunker-mvp DF1023_TOKEN=day2-second   # replace
./bunker env get  df1023-day2 --server bunker-mvp DF1023_TOKEN                # day2-second
./bunker env list df1023-day2 --server bunker-mvp                             # DF1023_TOKEN=day2-second
./bunker exec df1023-day2 --server bunker-mvp -- 'printenv DF1023_TOKEN'      # day2-second, rc=0
./bunker heartbeat df1023-day2 --server bunker-mvp
./bunker metrics  df1023-day2 --server bunker-mvp
./bunker resource-events df1023-day2 --server bunker-mvp
./bunker stop     df1023-day2 --server bunker-mvp ; ./bunker stop df1023-day2 ...
./bunker exec     df1023-day2 --server bunker-mvp -- hostname
./bunker heartbeat df1023-day2 --server bunker-mvp
./bunker metrics  df1023-day2 --server bunker-mvp
./bunker resource-events df1023-day2 --server bunker-mvp
./bunker start    df1023-day2 --server bunker-mvp
./bunker exec     df1023-day2 --server bunker-mvp -- 'printenv DF1023_TOKEN; hostname'
./bunker info     df1023-day2 --server bunker-mvp
./bunker restart  df1023-day2 --server bunker-mvp
./bunker ssh      df1023-day2 --server bunker-mvp -- hostname
./bunker cp ev/cp-src.txt df1023-day2:/tmp/cp-test.txt --server bunker-mvp
```

A second, disposable agent (`df1023-ctl`, `--ttl 2h`) was spawned as a
**control** for the pause/resume claim, because the first agent's docker had
already died before the pause:

```bash
./bunker spawn df1023-ctl --server bunker-mvp --ttl 2h                       # 20.94s
./bunker exec df1023-ctl --server bunker-mvp -- 'docker ps >/dev/null 2>&1; echo docker_rc=$?; ls /run/bunker/df1023-ctl/docker.sock'
#   docker_rc=0 ; srw-rw---- 1 bunker-df1023-ctl ... /run/bunker/df1023-ctl/docker.sock
./bunker stop  df1023-ctl --server bunker-mvp    # Agent df1023-ctl stopped.
./bunker start df1023-ctl --server bunker-mvp    # Agent df1023-ctl started.
#   same exec -> docker_rc=1 ; ls: cannot access '/run/bunker/df1023-ctl/docker.sock': No such file or directory
```

## What held (verified, not assumed)

| Promise | Observed |
|---|---|
| `env set/get/list` + "sourced at the start of every exec" | set 1.34s; replace semantics work; `env get` → `day2-second`; `printenv` inside exec → `day2-second` rc=0 |
| env survives a pause | `stop` → `start` → `printenv DF1023_TOKEN` → `day2-second` (the file `/run/bunker/<id>/env` is written by the daemon and not touched by stop) |
| heartbeat never shortens a longer TTL | spawned 12h (`Expires: 2026-10-24T02:52:59Z`); heartbeat 0.40s → `Expires at: 2026-10-24T02:52:59Z` **unchanged**, plus the documented semantics line |
| stop is idempotent | second `stop` → rc=0, "Agent df1023-day2 is already stopped." |
| stopped agent refuses exec but says why | rc=1 `failed_precondition: agent_stopped: agent "df1023-day2" is stopped; run 'bunker start df1023-day2'` — same for heartbeat |
| start / restart are fast and report the expiry | start 0.41s, restart 0.49s, restart printed `Expires at: 2026-10-23T20:54:33Z` (= now + the daemon's 6h default, as documented for restart) |
| metrics / resource-events read the agent's own cgroup | metrics 0.39s: 313.5 MB / 4.0 GB; resource-events: `Cgroup: /sys/fs/cgroup/user.slice/user-1494.slice`, memory peak 416.8 MB `[cgroup memory.peak]`, pids peak 72, all `*.events` counters 0, "No ceiling events recorded in this agent's cgroup." |
| agent isolation | `id` → `uid=1494(bunker-df1023-day2)`, groups `bunker-agents` |
| docker works on a fresh spawn (before any pause) | `docker ps` empty table rc=0, `docker version --format {{.Server.Version}}` → 29.1.3 |

## What broke (this tick's findings)

1. **DF-BUNKER-107 (P1) — pause/resume silently destroys the agent's Docker.**
   The control agent had `docker_rc=0` and a live socket; after
   `bunker stop` + `bunker start` (both rc=0, "started") the socket is gone and
   `docker` cannot connect. `bunker restart` does not restore it either.
   Cause, proven on the host: the agent's dockerd runs as the **transient**
   systemd-run unit `bunker-docker-<id>` (`internal/agent/manager_spawn.go:520`);
   `stop` stops that unit (`internal/agent/manager_lifecycle.go:139-182`), which
   deletes a transient unit, and `start` only calls `startUserUnit()` on that now
   non-existent name (`:271-277`), logging failure at DEBUG only. As the agent
   user: `systemctl --user start bunker-docker-df1023-ctl.service` →
   `Unit bunker-docker-df1023-ctl.service not found.`
2. **DF-BUNKER-108 (P2) — `bunker info`'s "docker degraded" is uncorrelated with
   reality.** Three agents with docker verifiably answering (`df1023-ctl`
   pre-pause, `22fbe449`, `crier-lab`) all render `running (docker degraded)`.
   The probe only classifies the installer's per-user `docker.service`
   (`internal/agent/docker_health.go:38-52, 93-135`), which on this host fails
   one second after every spawn with the DF-BUNKER-96 state-lock collision
   (`failed to lock /run/user/1494/dockerd-rootless/lock, another RootlessKit is
   running with the same state directory?` → `Start request repeated too
   quickly`), while the manager's own unit serves the socket. The same label was
   printed before the pause (docker fine) and after it (docker dead) — it cannot
   distinguish the two states.
3. **DF-BUNKER-47-R2 (P1) — a HEAD build spawns an agent whose SSH halves are
   missing.** Spawn printed `(warn: could not fetch SSH key: unauthenticated:
   agent-scoped tokens are not allowed for this endpoint)` and then published
   `SSHFS Mount:` and `Docker Tunnel:` commands with
   `-i /home/kara/.config/bunker/keys/df1023-day2`, a path that does not exist.
   `bunker ssh` and `bunker cp` both die rc=1 with "SSH key not found … spawn the
   agent first" — on an agent spawned seconds earlier. `exec` keeps working
   (API stream), so the user gets a half-working agent with no explanation.
   **The discriminating probe named the client build, not the credential or the
   daemon:** the release CLI on `PATH` (`bunker 0.2.0`, commit `9b924f0`) spawned
   `df1023-k1` against the *same* `bunker-mvp` 0.2.1 daemon with the *same* token
   and saved the key (`SSH Key: (saved to ~/.bunker/keys/)`, 411 bytes at
   `~/.config/bunker/keys/df1023-k1`). Cause: HEAD's
   `internal/cli/spawn.go:369-374` fetches the key with the spawn response's
   `ApiKey` — the new agent's own credential — and the master-gated
   `GetAgentKey` refuses agent-scoped tokens
   (`internal/auth/jwt.go:373-374`). That substitution is commit `3078a3cc`
   (2026-10-17, DF-BUNKER-47), so the original defect ("no local key is ever
   written") is back with a new mechanism. Secondary doc drift: README:494 says
   an agent sub-key may do "Everything" while README:496/`:374` say it cannot
   retrieve keys.
4. **DF-BUNKER-110 (P3) — README's honesty example is wrong for a stopped
   agent.** README:1090-1096 lists "a stopped … agent" as a case where the
   counters read UNAVAILABLE; the stopped agent served real numbers
   (`current 268.3 MB`, `peak 416.8 MB`, `pids current 2`). The behaviour is
   better than the doc; the doc would teach a reader to distrust correct output.

## Step 2b — what using it costs (measured)

| Operation | Measurement | Verdict |
|---|---|---|
| build HEAD CLI | `go build` real 4.000 s (warm module cache) | fine |
| `spawn` (warm, cached tooling) | 19.74 s and 20.94 s (two agents) | fine — README documents "later spawns reuse the server's cached tooling and return in ~10s"; 20 s is inside the documented 60-90 s first-spawn envelope |
| `exec` a trivial command | hyperfine `--warmup 2 --runs 10`: **1.294 s ± 0.030** (min 1.244 / max 1.348), user 0.003 s + sys 0.004 s | see below — NOT a product cost |
| raw `ssh <agent> true` (same host) | hyperfine `--warmup 2 --runs 8`: **1.843 s ± 0.013** | the 1.3 s is the SSH session path (PAM + pam_namespace per-session `/tmp`), and `bunker exec` is 30 % FASTER than the raw ssh it wraps |
| `env set` | 1.34 s | fine |
| `heartbeat` | 0.40 s | fine |
| `metrics` | 0.39 s | fine |
| `stop` / `start` / `restart` | 1.67 s / 0.41 s / 0.49 s | fine |
| `list` (6 agents, warm) | 426.7 ms ± 6.8 (8 runs) | fine |
| `status` | instant, includes host residue census | fine |

**No PERF row was filed.** The only number a user might feel — 1.3 s per `exec`
— is *cheaper* than a direct SSH to the same agent (1.84 s), so it is the
substrate's SSH session setup, not bunker overhead; optimizing it is not this
project's knob. Everything else is sub-second or documented envelope. A win
nobody can feel is not a finding.

## Install leg (fresh machine)

Driven with the QA harness on a fresh ephemeral agent, from the primary checkout
at the same HEAD (`dirty clone — pull skipped, testing recorded sha`; the only
dirty paths are other lanes' `.coding-hermes/board/*` writes, the Go tree is
clean):

```
bunker-qa.sh launch /home/kara/deployBunker/bunker --keep
→ agent=58495686 @ bunker-mvp, ttl 4h, evidence=/tmp/bunker-qa-evidence-20261023T145536Z-2901622.jsonl
```

**Result: install PASSES, the suite is RED from one test (attributed below).**
19 cells collected (`collect` merged them by ts; the agent was reaped before the
manual destroy — `bunker destroy 58495686` answered `Agent 58495686 not found`
and `bunker info` → `not_found`, so the leg's cleanup is complete):

| Cell | Status | What it proved |
|---|---|---|
| `repo-head` | OK | tested HEAD `f062d19a` (clone dirty → pull skipped; only `.coding-hermes/board/*` was dirty) |
| `toolchain-bootstrap` | OK ×7 | go 1.27.3, gcc/g++ wrappers, make 4.3, compose plugin, buildx 0.38.1, node v22.15.0 |
| `fresh-install` | **OK** | `make`/go build from the HEAD tree on a bare agent |
| `upgrade` | **OK** | previous release v0.1.4 tree → HEAD reinstall clean |
| `ci-pass` | **FAIL** | native suite red: exactly one test, `TestSurfaceConnectCommand_LocalSocketBusyRefused` |
| `docker-deploy` / `chaos-shutdown` / `chaos-corruption` | N/A | no compose file / no db/state files — legitimate |
| `ui-probe` | INFO | go-native start command is not a long-running server (no HTML surface) |
| `chaos-disconnect` | UNVERIFIED | derived from the failing suite; not readable on its own |
| `chaos-resource` | **FAIL** | same single test under the 3G cap |
| `chaos-errorpath` | OK | clean error (rc=1) on a missing config |

**The suite red is attributed, and it is the test, not the product:**
`internal/cli/surface_connect_test.go:437-448` binds a unix socket under
`t.TempDir()`; on this agent the path is
`/home/bunker-58495686/qa-tmp-3898522/TestSurfaceConnectCommand_LocalSocketBusyRefused…/001/busy.sock`
= **109 bytes**, over the kernel's 107-byte `sun_path` limit, so `net.Listen`
fails `bind: invalid argument` and the test `Fatalf`s. Proof, same HEAD, same
machine, only the temp base changed:

```
# kernel limit probe (python AF_UNIX bind): len=107 OK; len=108 FAIL; len=109 FAIL; len=112 FAIL; len=120 FAIL
GOTMPDIR=/tmp go test ./internal/cli -run '^TestSurfaceConnectCommand_LocalSocketBusyRefused$' -count=1
  → ok 0.542s
GOTMPDIR=/tmp/df1023b/aaa…(43 chars) go test … -count=1
  → FAIL 0.075s  listen: … /TestSurfaceConnectCommand_LocalSocketBusyRefused360697683/001/busy.sock: bind: invalid argument
```

Both the uncapped `ci-pass` leg and the capped `chaos-resource` cell show the
identical error (`native.log:2767-2768`, `res.log:2770-2771`), which is why this
run files **QA-BUNKER-076-R2**: the pending QA-BUNKER-076 attributed it to the
memory cap and its acceptance criterion says "green in the capped
chaos-resource leg" — the cap is not the variable, the temp path length is.
Because the project's own gate is `go test ./...` (AGENTS.md), every fresh-agent
battery is red until that test stops depending on `TMPDIR` length.

Install-leg note: the battery's own agent got an SSH key while this tick's
spawns did not — because `bunker-qa.sh` spawns with the release `bunker` on
`PATH` (0.2.0), which is exactly the discriminating evidence behind
DF-BUNKER-47-R2.

## Verdict

🟡 **PROMISING-BUT-ROUGH** — the day-2 *read* surface (env, heartbeat, metrics,
resource-events, stop-idempotency, stopped-agent errors) is honest, fast, and
well-worded; the day-2 *promise* that a paused agent can be resumed is false:
`stop`/`start`/`restart` leave an agent that cannot run a container while every
verb says it started, and the one health signal that could warn an operator
("docker degraded") is computed from the wrong unit and is therefore always-on
for healthy agents.

## Cleanup

Destroyed both agents this run created (`df1023-day2`, `df1023-ctl`) and the
extra `df1023-k1` spawned for the release-vs-HEAD discriminator; the install-leg
agent (`58495686`) was already gone when the manual destroy ran (`not_found` on
both `destroy` and `info`), so nothing of this tick's remains on `bunker-mvp`.
Mounts/tunnels: none opened (`ssh`/`mount` were graded broken, see
DF-BUNKER-47-R2). No repo visibility or permission changes; no credentials
minted — the CLI's existing token was used and is never reproduced in this
document.

## Trace

`ch:trace` — row=`DF-BUNKER-107,DF-BUNKER-108,DF-BUNKER-110,DF-BUNKER-47-R2,QA-BUNKER-076-R2`;
spec=`README.md:917-984,1033-1103` + `specs/agent-lifecycle.md`;
procedure=this document's command list; evidence=`.coding-hermes/dogfood-log.md`
(2026-10-23 entry) + `docs/dogfood/diagnostics.md` §25; witness=live
`bunker-mvp` 0.2.1 + HEAD CLI `f062d19a`; verdict=🟡 PROMISING-BUT-ROUGH;
commit=HEAD of this commit.
