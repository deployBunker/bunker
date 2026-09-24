# Dogfood 2026-09-24 — agent-tools delivery + lifecycle (stop/start/restart/homes/linger/registry)

17th dogfood run. Angle: the surfaces runs 1-16 never touched — `agent-tools`
(probe + `--install` tool delivery), the lifecycle commands (`stop` / `start` /
`restart`), and the host-maintenance pair (`homes` / `linger` / `registry`).
Prior runs covered spawn/exec/env/cp/mount/tunnel/keys/REST/remote-dev; this is
the first run to exercise tool delivery onto an agent.

## Promise under test

"A fresh agent ships without the tools the remote editing verbs need; one CLI
command probes it, and one more delivers the vendored tools onto the agent's own
PATH and re-proves it. Stopping an agent keeps its state, starting resumes it,
restarting resets TTL — and the host's orphan homes/linger entries are visible
and pruneable with a dry-run first."

## What was run (all live on bunker-las-03)

| Step | Command | Result | Time |
|---|---|---|---|
| spawn | `bunker spawn --server bunker-las-03 --ttl 2h df-agenttools-0924` | agent up, rootless docker | 27.1s (cold) |
| probe | `bunker agent-tools df-agenttools-0924 --server bunker-las-03` | toolsd/rg/gopls absent, git/jq present, exit 0 | 0.47s |
| install (refused) | `bunker agent-tools ... --install` | refuses dynamic toolsd with the exact fix named | 0.03s |
| install (static) | `bunker agent-tools ... --install --binary ~/coding-hermes-tools/dist/toolsd-linux-amd64` | delivered + re-probed, version echoed | 21.9s |
| verify on agent | `bunker exec ... -- 'which toolsd && toolsd version'` | works on the agent, file written pre-stop survives | 0.5s |
| stop | `bunker stop df-agenttools-0924 --server bunker-las-03` | stopped | 1.6s |
| exec while stopped | `bunker exec ... -- 'echo alive'` | exit 1, `failed_precondition: agent_stopped` + the resume command in the message | — |
| start | `bunker start ...` | running again | 0.3s |
| state after restart | `bunker exec ... -- 'cat ~/probe.txt'` | intact | 0.5s |
| restart TTL | `bunker restart ...` | Expires At 16:37 → 20:38 (-07) | 0.3s |
| homes | `bunker homes` / `homes prune --dry-run` | 1 stale home named (bunker-media-hermes), 0 B | 0.006s |
| linger | `bunker linger` | 3 entries, 2 stale (user gone) | 0.006s |
| exec benchmark | `hyperfine --warmup 2 --runs 10 "bunker exec ... -- 'true'"` | 559.7 ms ± 50.7 ms warm | — |

## What held

1. **The probe contract is exactly right.** Missing tools are DATA (exit 0 with
   a named table), the probe itself exiting non-zero only when it could not run.
   REQUIRED-vs-optional is distinguished (toolsd/rg required, gopls optional).
2. **The dynamic-linking refusal is the best error in the CLI.** It does not
   guess: it ldd-checks the artifact, refuses, and names the exact remedy
   (`make dist` in the toolkit repo, CGO_ENABLED=0). Following that instruction
   verbatim worked first try.
3. **Delivery re-proves itself.** After copy, it re-runs the probe on the agent
   and fails if the tool is still unreachable — "copied the file" is never
   accepted as "the tool works". The delivered toolsd executed on the agent.
4. **Lifecycle semantics match their names.** stop keeps state (file survived
   stop→start AND stop→restart); start resumes; restart resets TTL (measured
   +4h04m on a 2h agent — the restart grants the full default TTL, worth
   knowing when you meant "just cycle it").
5. **Exit codes propagate honestly.** In-agent `false` → 1, `exit 7` → 7; a
   stopped agent's exec fails with a failed_precondition that tells you the
   resume command. (Caveat when scripting: `bunker exec … | tail` reports the
   pipe's status, not bunker's — use `${PIPESTATUS[0]}`.)
6. **The daemon self-reports its own gaps.** `bunker status` on las-03 prints a
   WARNING box that Private /tmp is NOT active (pam_namespace drop-in missing)
   — the README's isolation promise is flagged instead of silently unmet.

## What failed / friction

1. **DF-BUNKER-57 (P1): REQUIRED-but-undeliverable tools on SSH agents.**
   `--install` correctly refuses to ship rg/gopls ("no vendored binary; install
   via the image-spec package-add path") — but on SSH-based agents (las-03's
   class) there IS no image-spec path reachable from the CLI. The probe names
   two REQUIRED tools that no documented command can ever install there. The
   fix direction is on the board row: vendor a static ripgrep, or add an apt
   fallback into the agent's ~/bin, or scope the verdict per agent class.
2. **DF-BUNKER-58 (P2): registry has no read verb.** `bunker registry` offers
   only `compact`. The help sells it as inspectable; it is only rewriteable.
   Bare/unknown subcommand prints help and exits 0.
3. Minor (not filed): the first `--install` attempt fails if the local toolsd
   is dynamic — correct, but a fresh user has no static build and no pointer to
   which repo produces it (the error says "the toolkit repo" without naming
   it). Naming the repo, or shipping the static build as a release asset, would
   close the loop.
4. Minor (not filed): `restart` on a 2h-TTL agent granted ~4h — the reset uses
   the default TTL, not the agent's original. Surprising, not obviously wrong.

## Verdict

🟡 PROMISING-BUT-ROUGH on this angle: the lifecycle and maintenance surfaces
are solid and honest (exit codes, self-reported gaps, dry-run-first prune),
and the tool-delivery design is genuinely good — but the flagship agent-tools
flow dead-ends for half the fleet's agent classes (SSH-based), with the probe
itself as the witness.

Numbers: t2fs 27.1s spawn / 0.5s warm exec; friction 2 filed + 2 minor;
install leg PASS (see below).
