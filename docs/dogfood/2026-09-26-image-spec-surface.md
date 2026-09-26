# Dogfood 2026-09-26 — the image-spec / agent-tools surface (run 22)

## The angle

Twenty-one prior dogfood runs swept the CLI lifecycle, the raw REST protocol,
TLS trust, renewal/identity, ops/maintenance, agent-tools probing, and the
remote dev workflow. Never driven: **`spawn --image-spec`** — the per-agent
image customization path (GAP-064) that agent-tools names as THE remediation
for the tools it calls REQUIRED (rg, toolsd) but cannot deliver over SSH
(DF-BUNKER-57). This run drove that path end to end: write the spec the CLI
itself recommends, spawn with it, then use the agent the way agent-tools and
the remote-dev workflow say to.

Target: live fleet daemons — bunker-mvp (`bunker-mvp`, 0.1.4/509fc42, auth
enforced, 4/50 agents, /tmp: private) and cube-las-00 (0.1.4/6ea9495, 1/8,
/tmp: HOST-SHARED). CLI built from the repo checkout (a4e98ce / HEAD 4437e46
for the board commit). The skill's designated install host, bunker-las-03
(100.69.3.13), is DOWN (ssh connect timeout — same as run 21); the run used
the fleet fallback plus a fresh agent on cube-las-00 for the from-zero leg.

## What the promise said vs what happened

**Promise (README GAP-064 + agent-tools):** "spawn with `--image-spec spec.json`
to customize the agent image (base + apt/go/npm package adds)" and "deliver
rg/gopls via the image-spec package-add path" — i.e. an operator who hits the
REQUIRED rg/toolsd gap can close it by respawning with a spec.

**Reality: the path is broken in three stacked ways.**

### 1. The CLI's own remediation spec cannot build (DF-BUNKER-79)

`agent-tools --install` prints exactly this advice:

```
spawn with: {"packages":[{"manager":"apt","packages":["ripgrep"]},
 {"manager":"go","packages":["golang.org/x/tools/gopls@latest"]}]}
```

Executed verbatim → spawn FAILS exit 1 after ~27s of user/dockerd setup:

```
Step 3/3 : RUN go install golang.org/x/tools/gopls@v0.17.0
/bin/sh: 1: go: not found
The command '/bin/sh -c go install ...' returned a non-zero code: 127
```

The `go` renderer (internal/imagespec/spec.go:410) emits `RUN go install <pkg>`
against `DefaultBaseImage = docker.io/library/ubuntu:24.04` (parse.go:12),
which has no Go toolchain. The spec was re-tried twice (dfspec-b, dfspec-c) —
identical failure, agent rolled back each time.

### 2. The working apt-only spec REPLACES the agent userland (DF-BUNKER-80)

Adjusted spec → apt-only (`ripgrep + golang-1.22-go + jq`), image
`bunkerd-imagespec-ff3842c5ba6a:latest`, spawn rc=0 in 52s (cold image pull +
apt). **rg 14.1.0 IS present** — DF-57's rg gap genuinely closes. But the A/B
against a stock agent (dfspec-a) spawned the same minute shows what "package
add" actually means: the image is built from bare ubuntu:24.04 + the listed
packages, so everything the stock agent had — git, the docker client, the
docker socket wiring — is gone:

| check (agent-tools probe / exec) | stock dfspec-a | image dfspec-e |
|---|---|---|
| rg (REQUIRED) | absent | present 14.1.0 |
| git (REQUIRED) | present 2.43.0 | **absent** |
| docker client | /usr/bin/docker | **not found** |
| `env set` / env sourcing | works | **exit 2, Directory nonexistent** |
| exec runs as | agent user | **root** |

### 3. Image-backed exec is container-jail, not agent (DF-BUNKER-77, P0)

On the image agent, `exec` runs inside a FRESH `docker run` of the custom
image (GAP-069 container-mode, service.go:1322) with ONLY `$HOME` bind-mounted
(spec.go buildAgentImageExecCommand). Consequences, all live-proven:

- `/run/bunker/dfspec-e/env` does not exist in the exec view → `bunker env set`
  fails, `set -a` sourcing silently no-ops: **env is unusable**.
- `docker` is not in the container image and the agent's own
  `/run/bunker/dfspec-e/docker.sock` is not mounted in → **rootless docker
  unreachable from exec** (the thing agents are for).
- exec reports `whoami` = root, uid=0.
- The agent-tools probe runs through the same container path, so it
  classifies the CONTAINER, not the agent (git "absent").

### 4. And then CI destroyed a live agent mid-window (DF-BUNKER-78, P0)

The image-spec spawn dfspec-d (01:53:04, "agent spawned successfully") was
destroyed ONE SECOND LATER by the repo's own CI root-suite cleanup running on
bunker-mvp: snoopy logged `mv /etc/bunkerd/ssh/dfspec-d(.pub) →
/tmp/root-suite-quarantine-khpjJ7/` and `userdel -rf` at 01:53:05. User, home
and server key all gone — while `bunker list` reports `running` and
`heartbeat` ACKNOWLEDGED, extending TTL to 07:57:44. Every SSH-family verb
dies `Permission denied (publickey,password)`; registry RPCs report healthy.
The suite's cleanup snapshots keys at start and re-checks live agents at exit
(INT-SPAWN-006), but the refresh window loses to a spawn that lands between
the last `bunker list` and the sweep. A production daemon host doubling as a
CI runner makes this a standing hazard, not a one-off.

### 5. Destroy cannot finish on a docker-running agent (DF-BUNKER-81, P1)

Tearing down the image agent hit the destroy wall harder than any prior run:
five consecutive `bunker destroy` (including `--force`, 300s-class CLI)
died `deadline_exceeded`. Server truth: DestroyAgent 500 after ~29.9s each —
the CLI's 30s context (destroy.go:70) cancels the request, `exec.CommandContext`
SIGKILLs the in-flight `tar czf` ("signal: killed"), and the fail-closed
archive gate refuses deletion. Each attempt left ANOTHER partial tarball:
`/var/backups/bunker/bunker-dfspec-e-*.tar.gz` ×7 (tar -tzf: "Unexpected EOF"),
archive dir 3.4G → 3.9G during the run. The rootless docker data-root
(`.local/share/docker`, 442M of the 688M home) makes EVERY docker-running
agent a multi-hundred-MB archive. DF-BUNKER-75 filed the generic large-home
form; this run adds the partial-archive leak, the 30s-vs-28s mechanics, and
that `--force` bypasses only the live-process gate.

### What worked, honestly

- **Baseline lifecycle is still excellent**: vanilla spawn 7s, key delivered,
  exec round trip 1.04s, env set/get, cp, docker, destroy-with-archive on the
  small home (17s), byte-verified.
- **rg via image-spec genuinely delivers** — the DF-57 rg half closes when the
  spec uses apt only.
- **toolsd delivery works** with a static artifact: `agent-tools --install
  --binary dist/toolsd-linux-amd64` → delivered to `/home/bunker-<id>/bin/`,
  re-probe "present", version echoed (v0.2.0).
- **The release-asset installer is still golden**: 4s cold on a bare Debian 13
  agent including the smoke check (run 21 measured 6s; consistent).
- **Source build path works on a no-toolchain agent**: Go 1.26.5 tarball per
  the README's own instructions (adapted: no sudo → `$HOME/goroot`), then
  `scripts/install.sh --build` → 35s total, `bunker version` prints commit
  4a78f2d = HEAD. Fresh clone verified at 4a78f2d.
- **Audit trail and daemon logs are excellent forensics**: every finding above
  was pinned to a timestamped daemon log line or audit record within minutes.

## Numbers (Step 2b, coding-hermes-perf)

| operation | warm | notes |
|---|---|---|
| exec, stock agent | 1.037s ± 0.025s | hyperfine ×10 |
| exec, image agent | 1.601s ± 0.045s | +55%: container-per-exec (PERF-004) |
| vanilla spawn | 7s | bunker-mvp |
| image-spec spawn (apt, cold) | 52s | incl. base pull + apt |
| image-spec spawn (broken go spec) | ~27s to fail | rolled back |
| release installer, cold | 4s | incl. built-in smoke |
| source build, from zero | 35s | incl. Go tarball 4s |
| destroy, small home (no docker) | 17s | archive + userdel |
| destroy, docker home | UNFINISHABLE | 5 attempts, 7 partial archives |

No PERF row for anything except the exec delta — the rest is comfortably fast
or is a correctness finding wearing a stopwatch (the destroy wall is
DF-BUNKER-81, not a perf row). PERF-004 records the +55% exec penalty with
the exact commands so the future fix of DF-77 can re-measure.

## Verdict

🟡 PROMISING-BUT-ROUGH — the stock path is the best it has ever been
(baseline spawn/exec/env/docker/toolsd-delivery all clean, fast, and honestly
reported); the image-spec path and the CI coexistence are the two P0 holes.
Every prior run's P0-class findings have been fixed at HEAD; these two are
newly-discovered surfaces, not regressions.

## Cleanup

dfspec-a destroyed clean (17s). dfspec-d already destroyed by CI (registry
entry left 'running' — that IS DF-BUNKER-78). dfspec-e and dfinst-bunker
UNDESTROYABLE (DF-BUNKER-81); left running with 2h TTL, will self-reap;
operator-level cleanup for the partial archives is an owner call (they are
evidence). dfsmoke-vanilla on cube-las-00 destroyed clean. No repo
visibility/permission changes; no credentials minted or committed.
