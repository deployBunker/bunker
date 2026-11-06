# Dogfood run 31 — the observability surface (2026-11-06)

Lane: `bunker-dogfood-2026-11-06-12-41-09` · Target: `/home/kara/deployBunker/bunker`
Measured HEAD: **a9c1db80** (pinned; the tip advanced to fb6058a7 mid-run with **0 non-board
paths changed**, so every number below stands for both) · daemon and CLI built from that SHA
with `make build` in a detached worktree (`bunker 0.2.1 / commit a9c1db80 / go1.26.10`).

## The promise being tested

`docs/observability-spec.md` (tier 1 implemented 2026-11-05) promises an operator a real
answer to the 3am questions: `/metrics` on the REST listener behind
`server.metrics_enabled` (loopback-only), five tier-1 gauges, tier-2 RPC counters/histogram
from the audit middleware, tier-3 lifecycle outcomes (spawn/destroy/refusals/archive), tier-4
WebDAV depth — plus a **textfile shim** (`bunker metrics --prometheus-textfile <path>
--metrics-url <url>`) that ports a scrape into the node_exporter textfile directory, appending
`bunker_metrics_last_success_timestamp_seconds` "only on success … so the freshness alert can
fire". README and the shipped release binary do not mention any of this (see "surfaces not
ready for users" below).

## How it was driven (a real operator's task, not a test script)

1. Build the documented artifact from a clean copy of the published tip (`make build`, worktree,
   `HOME` preserved so the Go toolchain resolver is the operator's).
2. Stand up a **coexist scratch daemon** on `bunker-mvp` — own loopback REST/gRPC ports
   (19201/19202), own `agent.base_data_dir`, `agent.ssh_dir`, `agent.registry.path`,
   `audit.path`, `destroy_archive_dir`, and `agent.reconciliation.unproven_orphan_limit: 0` so
   the startup orphan walk can destroy nothing. Daemon up in 500 ms.
3. Enable metrics, scrape it, spawn real agents, run real RPCs, destroy one, and watch every
   counter move.
4. Run the shim as an operator would (host-local, atomic publish into a textfile path) and then
   **break each thing the shim is supposed to protect against**.
5. Check the far end: what the deployed Prometheus actually receives, and what it alerts on.

## What held (verified, not asserted)

| Claim | Evidence |
|---|---|
| `/metrics` serves tier 1–4 on a loopback REST bind | `GET /metrics` → **200**, 1.5 ms, 38 `bunker_` series at 0 agents |
| `metrics_enabled` is genuinely off by default and returns 404 | second scratch daemon without the key: `http=404` |
| the loopback gate is enforcement, not decoration | `metrics_enabled: true` + `rest_addr: 0.0.0.0:19205` → daemon **refuses to start**: `server.metrics_enabled requires server.rest_addr to be an IP loopback address` (rc=1) |
| tier-1 gauges track reality | after spawn: `bunker_agents_current 1`, `bunker_ttl_expiring_soon 1` for a 5m-TTL agent; with a 5m **and** a 20m agent alive: `agents_current 2`, `ttl_expiring_soon 1` (the 10-minute window is honest) |
| tier-2 comes free from the audit middleware | after exec/list/info: `bunker_rpc_requests_total{code="ok",op="ExecAgent"} 1`, `…{op="SpawnAgent"} 1`, and `bunker_rpc_duration_seconds_sum{op="SpawnAgent"} 24.94` — the histogram measures what the CLI actually waited |
| tier-3 lifecycle closes the loop | `spawn_total{outcome="ok"} 2`; `destroy_total{outcome="archived"} 2` (one TTL reap + one explicit destroy), `{error|refused|ok|skipped_archive} 0`; `archive_bytes` 0 → 9.84e7 → 1.97e8; `registry_events` 1 → 4; `portpool_available` 5 → 4 → 5 |
| label law holds | every label set in the scrape is bounded: `op`, `code`, `reason`, `le`, `version`, `rest_addr` — no agent id, user, path or error text |
| the shim publishes atomically with a true stamp | 39 series / 5,102 B, `bunker_metrics_last_success_timestamp_seconds 1793969727` == `date +%s`; mode 0644; **zero** `.tmp` leftovers |
| shim failure keeps the last good file | dead port → **rc=1**, destination md5 **unchanged** (`44fd88f2…` before and after) |
| the textfile is a legal exposition | strict line check: 151 value lines / **0 malformed**, **0 duplicate series** |
| the collection path is live on the fleet | `bunker_agents_total{host="bunker-mvp",job="integrations/unix"}` has `count_over_time[1h] = 120` samples (30 s scrape) — a published `.prom` would be collected with no stack change |
| auth posture is consistent with the spec | on an `auth.enabled: true` daemon, `/metrics` answers 200 **without** a token (loopback-only, by design) while the same daemon still warns loudly about the unauthenticated hilo graph routes |

## What broke — four findings, all filed

**OBS-BUNKER-007 (P3) — the freshness guarantee has no consumer.**
The deployed Prometheus (`fleet-monitor:9090/api/v1/rules?type=alert`) carries exactly two
`bunker` rules — `BunkerLogindWedge`, `BunkerSpawnFailures` — and neither reads
`bunker_metrics_last_success_timestamp_seconds`; a query for that series returns an empty
vector. The shim, endpoint, atomic write and stamp all work, and the collector would accept the
file tomorrow — but nothing pages. The OBS-005 commit message and row text claim "the freshness
alert can fire"; on this stack it cannot, because no rule exists.
OBS-BUNKER-006 is blocked on *deployment*; deploying does not create the alert either.

**OBS-BUNKER-008 (P3) — the shim certifies any HTTP 200.**
`writePrometheusTextfile` checks only `resp.StatusCode == 200` and appends the stamp
unconditionally (`internal/cli/metrics.go`, the `if resp.StatusCode != http.StatusOK` guard).
Measured: a stub server answering `200` with a **0-byte** body → the shim exits **0** and
replaces a valid 10,234-byte / 119-series scrape with a **256-byte stamp-only** file whose stamp
equals the current time. A misrouted `--metrics-url`, a proxy error page, or any middleware
answering 200 therefore destroys the last good scrape *and* certifies freshness in the same
write; a 200 HTML page makes node_exporter drop the whole file (series *and* stamp) silently.
This is the one failure class the shim's own safety story does not cover.

**OBS-BUNKER-009 (P4) — "off" is indistinguishable from "healthy" for WebDAV.**
With `webdav_enabled` unset the scrape still carries 13 `bunker_webdav_*` series, all 0,
including `bunker_webdav_watch_heartbeat_age_seconds 0` — an alert on heartbeat age reports a
perfectly fresh watcher on a host that has none. Not a regression (OBS-BUNKER-004 pinned the
zero-mode deliberately), but there is no `bunker_webdav_enabled` gauge to tell the two states
apart, and the spec's tier-4 table names `bunker_webdav_push_subscribers` while the code emits
`bunker_webdav_push_active` / `push_active_max` plus five counters the catalogue omits.

**QA-BUNKER-099 (P3) — the clean-machine gate is red by default (found by the install leg).**
The ephemeral agent's `go test ./...` produced **19** failing test lines for four host reasons:
the borrowed-registry guard fails 8 `cmd/bunkerd` server tests once the installer has created
`/var/lib/bunkerd` (`agent.registry.path resolves to /var/lib/bunkerd/agents.jsonl inside the
default tree … but this daemon's agent.base_data_dir is /tmp/…`); egress tests hard-fail with
`neither nft nor iptables is available on this host`; AppArmor destroy tests hard-fail with
`apparmor_parser: executable file not found in $PATH`; and
`TestBuildAgentPersistentExecCommand` fails `sh: 1: go: not found`. The guard is correct — the
tests leave `agent.registry.path` at its default while pointing `base_data_dir` at a temp dir,
so they cannot pass on an installed host; the same test passes at the same HEAD on a host
without that directory (`ok 0.116s`).

## Surfaces not ready for a user (observation, not a row — already tracked)

- The **shipped release binary** (`/home/kara/deployBunker/bunker/bunker`, `0.2.1` / `f7989995`
  / built 2026-10-31) has **no `/metrics` and no `--prometheus-textfile`** — `bunker metrics
  --help` there lists only `--server`. HEAD reports the *same* `0.2.1` while shipping the whole
  observability surface. Release/version prep is RELEASE-019's job and deployment is
  OBS-BUNKER-006's; cited here so the gap is measurable, not duplicated.
- **README says nothing** about `metrics_enabled`, `/metrics` or the shim (0 grep hits); the
  only user-facing text is a comment block in `config.example.yaml`. That matches the PM's
  deliberate "propose, don't claim released" call (`d1129d7d`), so no doc row was filed.
- **The fleet's own census series is wrong about the live host**: `bunker_agents_total
  {host="bunker-mvp"} = 12` while the daemon reports `3/50` agents (and `list` says
  `Total: 3`). The census lives in the fleet-monitoring repo (spec §1 already traced it), so it
  is reported here rather than filed on bunker's board — but it is exactly the drift bunkerd's
  own `bunker_agents_current` now makes visible.

## Step 2b — what using it costs (nothing a user would feel)

| Operation | Warm | Note |
|---|---|---|
| `GET /metrics` (2 agents) | **2.0 ms** mean (n=10, min 1.3, max 2.7) | HTTP scrape, loopback |
| `bunker metrics --prometheus-textfile …` | **17 ms** mean (n=10, min 11, max 21), rc=0 ×10 | whole process: scrape + atomic publish |
| `bunker metrics` (RPC, server-wide) | **18 ms** mean (n=5, min 17, max 20), rc=0 ×5 | connect RPC path |
| spawn (real agent) | 24.96 s cold (first on a fresh daemon) / 18.72 s warm | not changed by this surface |
| destroy with archive (248 MB home) | 21.65 s | idem |

The observability path is milliseconds end to end, three orders below the ~1.3 s a `bunker exec`
already costs. **No PERF row filed** — there is no wait here for a user to notice, and profiling
a 17 ms path would be ceremony.

## Install leg (explicit, never silent)

`bunker-qa.sh` against `bunker-las-03` (designated ephemeral host, daemon `ONLINE`, 1/8 agents in
use): `launch` **OK** at 2026-11-06T13:09:19Z → agent `b661d74a`, ttl 4h, battery started
detached. Agent-side cells pulled before the tick's deadline: **9× toolchain-bootstrap OK**,
**`fresh-install` OK** (the documented installer ran inside the agent), `lint` INFO
(golangci-lint absent on a bare agent), `docker_build` / `compose_config` N/A, **`tests` FAIL**
(19 test lines, four host-environment causes — QA-BUNKER-099), **`ci-pass` FAIL** (`native
rc=1`, same suite; the battery was still inside the act cell at the deadline, so the chaos cells
never ran and are not claimed).

`bunker-qa.sh collect` **aborted twice** with `COLLECT-ABORT … cannot reach agent b661d74a
(ssh probe returned no state)` while a direct `ssh -i ~/.bunker/keys/b661d74a` into the same
agent succeeded in the same window — a recurrence of the pending QA-BUNKER-077-R2. A second,
smaller wrapper wart: `BUNKER_QA_EVIDENCE` is honoured by `launch` but `collect` requires
`--evidence` and exits 0 with a usage line. Evidence was therefore harvested directly from the
agent (its `qa-evidence.jsonl` plus the 765-line `tests.log`), and the agent was destroyed and
verified `not_found` — installability is graded from that evidence, and no cell is reported as
passing that did not pass.

## Teardown

Every run-owned thing destroyed or stopped and verified: agents `df1106-obs1` (TTL reaped),
`df1106-obs2` (explicit destroy, archived), the metrics/`nometrics`/`auth` scratch daemons and
the stub HTTP server stopped, `/root/df1106` left for inspection (evidence, configs, logs). No
production daemon restarted, no repo visibility or permission changed, no credential minted —
the scratch auth token is a literal throwaway string in a scratch config on a scratch host.
