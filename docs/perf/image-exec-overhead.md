# Image-backed exec overhead: +55% per call — container-per-exec is the price (PERF-004)

Status: measurement record + design note. No behavior change is proposed here;
the mitigation already shipped (SURF-012) and the remaining trade-offs are
documented so the next change can be judged against a fixed baseline.

## 0. The one-screen answer

`bunker exec` on an agent spawned with an image spec (GAP-069 path) costs
~+0.56s per call over a stock host-context exec: 1.601s vs 1.037s warm. The
entire delta is the `docker run --rm` container lifecycle (create, start,
exec, remove) that GAP-069 deliberately adds per call. Single interactive
calls do not feel it. A 100-call scripted loop pays ~+56s and does.

For scripted loops, the answer today is SURF-012's opt-in persistent
container: `bunker exec --persistent ...` keeps one long-lived container per
agent and rides `docker exec` instead of a fresh `docker run --rm` each time.

## 1. The measurement

Source: dogfood run 22, 2026-09-26, host `bunker-mvp` (78.46.173.180),
hyperfine `--warmup 3 --runs 10`, warm cache, same host and same minute, both
agents idle, `-- true` as the command so shell/work cost is negligible.

| path | agent | mean | stddev |
|---|---|---|---|
| image-spec exec (GAP-069 container per call) | `dfspec-e` | 1.601s | ± 0.045s |
| stock host-context exec (plain SSH shell) | `dfspec-a` | 1.037s | ± 0.025s |

Delta: +0.564s per call = **+55%**.

Scope limits, stated plainly: warm (not cold-start), N=10 runs, one host, one
day. The absolute number will drift with the host and the image; the *ratio*
(image path ≈ 1.55× host-context path) is the durable fact, and it was stable
across all 10 runs.

## 2. Where the +0.56s goes

`containerRunPrefix` (internal/server/service.go) builds
`docker run --rm ... <image> sh -lc <cmd>` — executed remotely by the agent
user against the agent's own rootless dockerd. Every call:

1. creates a container (OCI spec, mount namespace, the home bind-mount, the
   runtime-dir bind, identity/env injection),
2. starts it and execs `sh -lc`,
3. tears it down (`--rm`) so nothing is left behind.

No profile was taken because none is needed: the two paths differ by exactly
this lifecycle, and the measured delta is the same order as a plain
`docker run --rm ... true` on the same daemon. There is no hotspot to hunt
inside the exec itself — the stock path's command runs the same way once the
container is up.

## 3. What it costs in practice

- Single interactive call: invisible (~half a second on top of an SSH
  round-trip).
- Scripted loop (the case PERF-004 was filed for): 100 execs ≈ +56s of pure
  container churn.
- It is also a component of DF-BUNKER-77's total container-isolation penalty;
  that row tracks the broader cost, this row the per-exec slice.

## 4. What already exists: SURF-012 persistent container

The "reuse the container" mitigation is shipped, opt-in, per call:

- `bunker exec --persistent ...` — ensure a container named
  `bunker-exec-<agent_id>` exists (create `... sleep infinity` only when
  missing or when its image ref no longer matches the agent's current image),
  then run the command via `docker exec` in it. The writable layer — Go
  module caches, apt-installed toolchains — survives between calls, which is
  usually worth more than the latency itself.
- `--list-persistent` / `--cleanup-persistent` — list/clean up these
  containers by their `bunker.persistent=true` label. The marker lives on the
  container inside the agent's own rootless dockerd, so it survives a bunkerd
  restart by construction.
- The default (no flag) is byte-identical to the pre-SURF-012 one-shot path.
  Persistent exec is refused loudly for agents without an image spec (nothing
  to keep alive) and is incompatible with raw mode (reuse requires an ensure
  step; raw's contract is execve with no shell in between).

So the design decision recorded here is: **the fallback for loop-heavy
callers is `--persistent`, not a code change.** What a caller gives up is the
fresh-container guarantee — a persistent container accumulates state across
calls, by design.

## 5. Options considered

- **Persistent container (SHIPPED, SURF-012)** — amortizes the lifecycle to
  one create per agent; see §4.
- **Host-context fallback flag (REJECTED)** — a flag that runs an image-spec
  agent's exec on the bare host would erase exactly the isolation GAP-069 was
  built for (and that DF-BUNKER-77-IMGEXEC hardened: identity resolution,
  runtime-dir bind, socket env). An operator who wants host context can spawn
  the agent without an image spec; that is the explicit, visible choice.
- **Batch multiple commands into one exec (CALLER-SIDE)** — a script can ship
  a compound command in a single call; zero code, but no shared filesystem
  state between batches and awkward error handling.
- **Pre-warmed container pool (NOT PURSUED)** — would shave create cost only
  (start/remove remain), adds lifecycle state bunkerd would have to own, and
  SURF-012 already covers the loop case better.

## 6. Re-measure protocol

When anything changes this path (DF-BUNKER-77 follow-ups, dockerd version
bumps, exec builder rewrites), re-run the §1 hyperfine pair on the same
agent pair (`dfspec-e` image-spec vs `dfspec-a` stock) and compare the
*ratio*, not just the milliseconds. If the ratio drops materially below
~1.5×, update §1 and the comment on `containerRunPrefix` in the same commit.
