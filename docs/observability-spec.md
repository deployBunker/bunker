# bunker — Observability Spec

Status: proposed (audit 2026-11-03). No service code changed yet.
Audience: the `monitoring` lane worker who implements this, and the operator who pages at 3am.

---

## 0. Verdict (measured, not assumed)

| Question | Answer | Evidence |
|---|---|---|
| Does bunkerd expose `/metrics`? | **No.** | `grep -rn '"/metrics"' --include=*.go` → 0 hits (764 Go files, `third_party/` excluded) |
| Does it use a Prometheus client library? | **No.** | `grep prometheus/client_golang` → 0 hits; `grep -c prometheus go.mod` → 0 |
| Any other metrics surface? | **No push, no expvar.** | `expvar.Handler` / `/debug/vars` → 0 hits |
| What is the only monitoring surface? | **An authenticated, pull-only gRPC RPC pair** — `ServerMetrics` and `AgentMetrics`. | `proto/bunker/v1/bunker.proto:12`, `:41`, `:86`; `internal/server/service.go:328`, `:741` |
| So where do the 8 `bunker_*` families come from? | **Not from this binary and not from this repo.** They are written by an out-of-repo SSH cron into a node_exporter **textfile** on each host, which Alloy remote-writes. | §1 below |

**Verdict before:** *blind by construction.* bunkerd is a long-running, multi-tenant,
state-mutating daemon that owns host users, cgroups, Docker containers, ports and archives —
and it emits **zero** Prometheus series. The only `bunker_*` series in Prometheus describe the
*hosts it runs on*, scraped from the outside, and do not even come from the code in this repo.

**Verdict after (target):** `/metrics` on the REST listener, 4 core gauges + 3 counters live in
week 1 (row `OBS-BUNKER-001`), then the RPC histogram and subsystem depth.

---

## 1. Provenance of the existing `bunker_*` series — traced

Prometheus currently receives 8 `bunker_*` families (read from
`/home/kara/fleet-monitoring/scripts/raw/metrics.json`, the metric-name inventory captured
2026-11-02):

`bunker_agents_total`, `bunker_agent_cpu_usage`, `bunker_agent_memory_usage_bytes`,
`bunker_agent_disk_usage_bytes`, `bunker_agent_online`, `bunker_containers_per_host`,
`bunker_logind_up`, `bunker_spawn_failures_30m`.

They arrive through **two different textfile writers**, neither in the bunker repo:

**(a) Tenant census — `scripts/fleet-inventory.sh` in the fleet-monitoring repo.**
`git show 8d45156:scripts/fleet-inventory.sh` lines 32-105 (`collect_bunker_metrics`):
it SSHes to each host, loops `/home/bunker-*` and `/sys/fs/cgroup/system.slice/bunker-docker-*.service`,
and prints `bunker_agents_total`, `bunker_agent_online`, `bunker_agent_{cpu,memory,disk}_usage`,
`bunker_agent_ttl_seconds`, `bunker_containers_per_host`, then atomically writes
`bunker_agents.prom` into the host's textfile dir (line 109).

**This script is NOT on the live checkout.** The live fleet-monitoring checkout is on branch
`main` (`git log -1` → `1684c62`, "fix(host-detail): default the host variable …"); on `main`
`scripts/fleet-inventory.sh` is a 77-line software-census script with **zero** `bunker_` hits.
The 152-line tenant-census version exists only on `master` / `wt/mon-gap014-tenant-census-v2`
(`git merge-base --is-ancestor 8d45156 main` → false; `… master` → true).
Consequence: whichever branch drives the cron is what keeps those 5 families alive. A
deployment from `main` silently stops them. `bunker_agent_ttl_seconds` is emitted by that
script but **is absent from the Prometheus inventory** — it has never landed.

**(b) Logind / spawn-failure — `bunker-health.sh` textfile exporter.**
Referenced only by a comment in `alerts/rules/host-health.yml` at the `BunkerLogindWedge`
alert ("bunker-health.sh textfile exporter publishes logind liveness and the 30m spawn-failure
count from the bunkerd journal"). That script is **not in the fleet-monitoring repo and not on
this machine** — no copy is readable from this audit's vantage point. The two series it
publishes (`bunker_logind_up`, `bunker_spawn_failures_30m`) are alert sources in
`host-health.yml:104` and `:118`, so an unreadable emitter is an unmonitored alert path.

**Implication for the catalogue below:** these 8 are *host-observation* proxies. They are not
emitted by the process that owns the state, so they cannot answer anything about a *specific
bunkerd* (uptime, request rate, TTL countdown, refusal backoff). Nothing below duplicates them
where they overlap; the overlap rows are marked *supersedes* with the retirement condition.

---

## 2. Role and what "healthy" means

`bunkerd` is a root-privileged per-host agent manager. On one host it: creates Linux users and
homes, systemd units (`bunkerd.service`, the unit name is fixed at
`internal/systemd/systemd.go:13`), per-agent Docker cgroups, allocates a port slice
(defaults 10000-19999 at 100/agent, `internal/config/config.go:1499-1501`), issues scoped API
keys, and reaps agents whose TTL has expired (`internal/agent/manager.go:333`).

Three listeners are possible; the HTTP one is what matters here:
REST `:8080` and gRPC `:9090` by default (`internal/config/config.go:1478-1479`), plus optional
HTTP/3/UDP.

**Healthy means, in the operator's language:**

1. The daemon is running, has been up for a while, and is reachable on `:8080`.
2. It is *absorbing* agents: `agents_current` stays under `agents_max` and spawns keep succeeding.
3. It is *releasing* agents: expiry and destroy succeed. A rising count of destroy refusals
   (`home_retained`, `live_processes`, `userdel_failed`) is the leak signature — the host fills
   with homes that will never go away.
4. Nothing is accumulating invisibly: registry JSONL growth, archive-dir growth, port-pool
   exhaustion.
5. Failure is visible per-reason, not as one opaque error rate.

---

## 3. Why the status quo cannot answer the 3am questions

Every fact below **already exists in memory** and is thrown away on restart or buried in a log
line. This is the blind-spot table; the "where it lives" column is the citation.

| 3am question | What exists today | Where it lives | Why it fails |
|---|---|---|---|
| Is this bunkerd up? | `/healthz` returns a *constant* `{"status":"ok"}` | `internal/server/server.go:180-184` | Always 200, even when the registry was restored empty or the reaper is stuck. No uptime, no identity. |
| How long has it been up? | `UptimeSeconds` computed per RPC from `serverStartTime` | `internal/server/service.go:134-142` | Gated behind master-token gRPC; a host-scoped exporter cannot call it. |
| How many agents are live vs allowed? | `s.tracker.Count()` / `s.tracker.MaxAgents()` | `internal/resource/tracker.go:136-145`, served at `service.go:140-141` | Same problem — pull-only and authenticated. |
| Is it doing work? | Only `middleware.Logger` writing one unstructured chi line per request | `internal/server/server.go:175` | Not aggregation, not scrapeable, no counters. (The **audit log** is better — see next row.) |
| Request rate and latency per RPC? | **`audit.Record` already carries `method`, `duration_ms`, `outcome`, `remote_addr`** | `internal/audit/audit.go:36-43` | Shipped to a 5 MiB rotating local file (`MaxSize`, `audit.go:23`), consumed by forensics tools. Perfect histogram source, zero Prometheus series. |
| Which RPCs fail, and with what code? | `Outcome` = connect code, e.g. `"not_found"` | same | Tail-the-file archaeology only. |
| How many spawns failed in the last hour, split by cause? | `s.logger.Error("spawn agent failed", …)` | `internal/server/service.go:443-447` | Free-text log; the *cause* is only in `err`. Requires journalctl + regex. |
| Are destroys failing to archive (homes leaking)? | `s.logger.Error("agent home archive failed; home RETAINED, userdel NOT run", …)` plus durable refusal state on the registry record | `internal/agent/manager_destroy.go:895-906`; backoff in `internal/agent/manager.go:380-407` | A refusal with exponential backoff up to **1 hour** is invisible unless you grep. This is the single most expensive silent failure. |
| How many agents are waiting out a destroy-refusal backoff? | `reaperBackoffRemaining(rec.AgentID)` computed per agent, then logged one line | `internal/agent/manager.go:395-401` | A gauge-shaped quantity that exists only as N log lines per minute. |
| Is any agent's TTL about to expire? | `rec.ExpiresAt` per record | `internal/registry/registry.go:88-91` (Event), `manager.go:391` | Expiry sweeps at 1-minute ticks (`manager.go:335`) with no "expiring soon" count exposed. |
| Is the port pool running out? | `m.portAlloc.Available()` computed inside a warn-only branch | `internal/agent/manager.go:367-377` | Emitted *only when drift released something*. A pool draining normally is silent. |
| Is the registry file growing without bound? | `Compact` computes `BeforeEvents`, `Files` | `internal/registry/compact.go:41`, `:162` | Compaction stats are returned to the caller and dropped. |
| Is the WebDAV watcher healthy? | **A complete counter set already exists**: `overflows`, `unvouched`, `rescans`, `installFailures`, `backendErrors`, `watchedChanges`, `heartbeats`, `loopTicks`, `lastHeartbeatAt` | `internal/server/webdav/watch.go:409-421`, snapshotted into `watchCounters` at `watch.go:1330-1342` | Exposed only as a JSON document on the WebDAV surface. This is the closest thing in the repo to a metrics endpoint and it still is not one. |
| Are push subscribers dropping events? | `active`, `activeMax`, `drops`, `gaps` atomics | `internal/server/webdav/push.go:163-166` | Same — not scrapeable. |
| Is there leaked residue (orphan users/homes/linger)? | `residueInventory()` computed for `ServerInfo.Residue` | `internal/server/service.go:148-153` | Authenticated RPC only. |

**Suspected bug, fix alongside row 003:** the tenant census matches a cgroup directory
`/sys/fs/cgroup/system.slice/bunker-docker-<suffix>.service`
(`git show 8d45156:scripts/fleet-inventory.sh:63`), while bunkerd installs the unit as
`bunkerd.service` (`internal/systemd/systemd.go:13`). If per-agent units are not named
`bunker-docker-*`, the census silently falls back to the `user-<uid>.slice` branch (line 70) and
the `bunker_agent_cpu_usage` figures describe a different cgroup than the name suggests. This
audit did not execute the script and cannot confirm; it is an *unverified* item flagged for the
row, not a finding.

---

## 4. Metric catalogue

Naming: `bunker_<subsystem>_<thing>_<unit>`. `_total` on counters, `_seconds` / `_bytes` on
gauges and histograms. Every family carries `host` (from the Alloy/collector layer, never
appended by the app) and `product="bunker"`.

**Label law (hard):** never a value that can multiply without bound. That means **no** `agent_id`,
`user`, `session_id`, `remote_addr`, `path`, `error` string, or timestamp as a label. Bounded
vocabularies only — connect `code` (16 values), `op` (the fixed RPC short name), `reason` (the
registry refusal enum). Everything unbounded belongs in logs, where the audit log already has it.

### 4.1 Tier 1 — minimal viable emission (row `OBS-BUNKER-001`)

Served by a `/metrics` handler on the existing REST listener. These five answer "up, working,
how much, and is it failing".

| Metric | Type | Unit | Labels | The one question it answers |
|---|---|---|---|---|
| `bunker_info` | gauge (always 1) | — | `version`, `rest_addr` | What build is this, and is it the one I deployed? |
| `bunker_uptime_seconds` | gauge | seconds | — | Is it up and stable, or restarting every few minutes? |
| `bunker_agents_current` | gauge | count | — | How many agents are live right now? |
| `bunker_agents_max` | gauge | count | — | What is this host's capacity? (Divide the two → saturation.) |
| `bunker_ttl_expiring_soon` | gauge | count | — | Will agents disappear in the next 10 minutes (expected) or is the host about to empty (unexpected)? |

### 4.2 Tier 2 — request behaviour (row `OBS-BUNKER-002`)

Sourced from the audit middleware that already computes `duration_ms` and `outcome`; no new
measurement, just a different sink.

| Metric | Type | Unit | Labels | Question |
|---|---|---|---|---|
| `bunker_rpc_requests_total` | counter | requests | `op`, `code` | What is the request rate, and which RPCs are failing with which code? |
| `bunker_rpc_duration_seconds` | histogram | seconds | `op` | How slow is spawn vs list vs exec? (Buckets 0.05 … 600) |

### 4.3 Tier 3 — lifecycle outcomes (row `OBS-BUNKER-003`)

| Metric | Type | Unit | Labels | Question |
|---|---|---|---|---|
| `bunker_spawn_total` | counter | spawns | `outcome` ∈ {`ok`,`error`} | Is the host accepting new agents? |
| `bunker_destroy_total` | counter | destroys | `outcome` ∈ {`ok`,`refused`,`error`,`archived`,`skipped_archive`} | Are destroys completing? |
| `bunker_destroy_refused_total` | counter | refusals | `reason` ∈ {`live_processes`,`home_retained`,`userdel_failed`} | Why are homes leaking? |
| `bunker_agent_expiring_soon` | gauge | agents | — | (dup of tier 1 `ttl_expiring_soon`; keep one name — see Note A) |
| `bunker_registry_destroy_refused` | gauge | agents | `reason` | How many agents are currently stuck in refusal backoff? |
| `bunker_archive_bytes` | gauge | bytes | — | Is the archive directory growing without bound? |
| `bunker_archive_failures_total` | counter | archives | `reason` ∈ {`archive`,`prune`,`verify`} | Is `home_retained` about to become a leak? |
| `bunker_registry_events` | gauge | events | — | Is the registry JSONL growing toward its compaction bound? |
| `bunker_portpool_available` | gauge | ports | — | Is the port range exhausting (and will the next spawn fail)? |
| `bunker_portpool_drift_released_total` | counter | reservations | — | Are teardown paths leaking port reservations? |

### 4.4 Tier 4 — subsystem depth (row `OBS-BUNKER-004`, only when WebDAV/subscription is on)

| Metric | Type | Unit | Labels | Question |
|---|---|---|---|---|
| `bunker_webdav_watch_backend_errors_total` | counter | errors | — | Is the filesystem watcher degrading? (`watch.go:415`) |
| `bunker_webdav_watch_install_failures_total` | counter | failures | — | Did watcher install fail (watch is degraded)? (`watch.go:414`) |
| `bunker_webdav_watch_overflows_total` | counter | events | — | Are events being dropped on overflow? (`watch.go:412`) |
| `bunker_webdav_watch_unvouched_total` | counter | paths | — | Is the watch set drifting from truth? (`watch.go:413`) |
| `bunker_webdav_watch_heartbeat_age_seconds` | gauge | seconds | — | Is the watch loop alive? (`watch.go:419`, `loopTicks` at `:418`) |
| `bunker_webdav_push_subscribers` | gauge | subscribers | — | Are push streams attached? (`push.go:163`) |
| `bunker_webdav_push_drops_total` | counter | events | — | Are subscribers losing events? (`push.go:165`) |

### 4.5 Deliberately NOT instrumented (and why)

- **Per-agent series** (`agent_id` label) — cardinality is unbounded by design (agents are
  ephemeral). Per-agent CPU/memory already arrives from the cgroup path (§1a) and from cAdvisor.
  Per-agent *lifecycle* questions are answered by `bunker_destroy_refused_total{reason}` plus the
  audit log.
- **User identity / remote address** — an unbounded label and a privacy surface. Already in the
  audit `Record` (`audit.go:38-43`), which is the correct place for it.
- **Error message text as a label** — unbounded strings. Use the bounded `reason`/`code` enums
  plus the log line.
- **Timestamps as labels** — use a gauge (`*_timestamp_seconds`) instead; a label timestamp
  creates a new series per second.
- **Exec command content** — redacted by design upstream (`docs/exec-audit.md`); never a metric.
- **SLA/error-budget series** — derived in PromQL, not emitted by the app.

---

## 5. Exposure mechanism

**Primary: `/metrics` on the existing REST listener via `prometheus/client_golang` +
`promhttp`.** Not a second listener — `server.go:527-556` already builds `http.Server`s around
one `rootHandler`, and `/healthz` (`server.go:180`) is already an unauthenticated plain-HTTP
route on chi. `/metrics` goes beside it in exactly the same way.

Constraints the implementation must respect:

1. **Default-off or loopback-only binding.** `/healthz` is currently reachable by anyone who can
   reach `:8080`, and the config explicitly warns about that posture
   (`server.go:265`: "*anything that can reach the port can map the host*"). `/metrics` must NOT
   follow that example blindly — gate it behind `server.metrics_enabled` (new key) that ALSO
   requires the REST listener to bind loopback, or require the master bearer token. Default off,
   like `webdav_enabled` (`internal/config/config.go:370-372`).
2. **JWT guardrail.** `cmd/bunker/exec_flaggrammar_test.go:246` pins the root command's
   persistent flags to exactly `{config, daemon-config}`. This work must not add a root flag; the
   config key is a `mapstructure` field only.
3. **Do not make scrape latency depend on the RPC plane.** `/metrics` must not call
   `AgentMetrics`; it reads in-process counters and the tracker, which are already cheap.
4. **Spawn a registry of collectors once**; no per-scrape goroutine fan-out into Docker.

For the *existing* outer signals that only a host shell can see (logind liveness, spawn-failure
count from the journal), the right answer remains a **textfile** (`bunker-health.prom`), because
those facts live outside the process. That is a fleet-monitoring change, out of scope here — see
row `OBS-BUNKER-005`.

**Short-lived CLI lanes (`bunker` CLI, `scripts/*.sh`): not instrumented.** They are not
resident; the correct record for them is the exit status and the audit line they already
produce. Do not add a scrape target for them and do not add a pushgateway.

---

## 6. Collection path

The fleet's posture is **push**, not scrape: every host runs the Alloy container
(`/home/kara/fleet-monitoring/ansible/templates/alloy.service.j2:25-38`), which scrapes local
exporters at a **30s** interval (`stack/alloy/config.alloy:54`) and
`remote_write`s to the monitoring host (`:9090/api/v1/write`, `config.alloy:58-66`).

Two supported wirings, in order of preference:

**(A) Textfile shim — zero monitoring-stack change, fastest to ship.**
A per-host collector (systemd timer, or a proposed opt-in textfile mode on the metrics command,
whose option name would be prometheus-textfile and which does not exist yet) calls
`curl -s http://127.0.0.1:8080/metrics` and writes the body to
`/var/lib/node_exporter/textfile/bunkerd.prom`. The `prometheus.exporter.unix "host"` textfile
collector (`config.alloy:30-32`) picks it up on the next 30s scrape and remote-writes it. Labels
arriving for free: `instance` (relabelled to the SSH alias — `config.alloy:44-51`), `job`,
`host`. This reuses the same directory the existing `bunker_agents.prom` lives in.

*Staleness is the cost:* a dead bunkerd leaves the last textfile on disk and the series keep
reporting a plausible number forever. The writer MUST stamp `bunker_metrics_last_success_timestamp_seconds`
and the alert MUST be `time() - bunker_metrics_last_success_timestamp_seconds > 180` — a
freshness alert, not a presence alert. This is exactly why (A) is a bridge, not the destination.

**(B) New scrape job on the monitoring host — the real answer, requires an owner-approved change
to `/root/fleet-monitoring/prometheus/prometheus.yml`.** The monitoring host has no inbound
service ports by design, so this means either a Tailscale-addressable target per bunker host
with a `host` relabel, or a push gateway — and the stack doc explicitly chose push to avoid the
former. Record the decision; do not invent it in a worker row.

Labels required on every series regardless of path: `host` (the SSH alias, not `$(hostname)` —
`config.alloy:10-12`), `product="bunker"`, `service="bunkerd"`.

---

## 7. Verification — what "done" looks like

Run from the bunker host, after deploy. Every check must be reproducible by a reviewer.

**V1 — endpoint is live (row 001).**
```sh
curl -fsS http://127.0.0.1:8080/metrics | grep -c '^bunker_'
```
Expect `>= 5`. A `404` means the handler is not mounted; a `401` means the gate is on and the
reviewer must pass `-H "Authorization: Bearer <master-token>"`.

**V2 — the two capacity gauges agree with the CLI.** `bunker metrics` reports server-level
resource usage (`internal/cli/metrics.go:24`), and `ServerInfo.AgentCount` is the same
`tracker.Count()`:
```sh
curl -fsS http://127.0.0.1:8080/metrics | grep -E '^bunker_agents_(current|max) '
bunker server info            # AgentCount must equal bunker_agents_current
```

**V3 — PromQL, in the fleet Prometheus.**
```promql
# rows 001: uptime and capacity
bunker_uptime_seconds{product="bunker"}
bunker_agents_current / bunker_agents_max

# row 002: per-RPC error ratio over 5m
sum by (op) (rate(bunker_rpc_requests_total{code!="ok"}[5m]))
  / sum by (op) (rate(bunker_rpc_requests_total[5m]))

# row 002: p95 spawn latency
histogram_quantile(0.95, sum by (le) (rate(bunker_rpc_duration_seconds_bucket{op="SpawnAgent"}[5m])))

# row 003: the leak signature
increase(bunker_destroy_refused_total[6h]) > 0
bunker_registry_destroy_refused{reason="home_retained"} > 0

# row 003: port pool about to exhaust
bunker_portpool_available < 200
```
Each query must return at least one series with a plausible value.

**V4 — freshness (required for path 6A).**
```promql
time() - bunker_metrics_last_success_timestamp_seconds{host="bunker-mvp"} < 180
```

**V5 — negative test.** Stop `bunkerd` (`systemctl stop bunkerd`). Within 5 minutes
`bunker_uptime_seconds` must go stale under path (B), or `bunker_metrics_last_success_timestamp_seconds`
must age past 180s under path (6A). If a stopped daemon keeps reporting healthy numbers, the
signal is worse than none.

**V6 — the alert exists.** The tier-1 and tier-3 series are worthless without a rule. "Done"
includes at least these three in the alerting tree (fleet-monitoring, owner-gated):
`BunkerDaemonStale` (V4 inverted), `BunkerDestroyRefusalLeak` (`bunker_registry_destroy_refused > 0` for 30m),
`BunkerPortPoolLow` (`bunker_portpool_available < 200` for 10m).

---

## 8. Open questions this audit could NOT resolve

Stated plainly, not guessed:

1. **Which branch of fleet-monitoring drives the live cron?** `main` lacks the tenant census and
   `master` has it; the answer decides whether `bunker_agent_*` keeps flowing.
2. **Where does `bunker-health.sh` live?** It is cited by an alert comment and exists in neither
   the repo nor a readable path on this machine. `bunker_logind_up` and
   `bunker_spawn_failures_30m` are therefore alerting on a producer nobody in this audit can
   inspect.
3. **Is `bunker_agent_ttl_seconds` really absent?** It is printed by the census script but is not
   in the 2026-11-02 metric inventory. Either the census was never run against a host with TTL
   metadata, or the series was dropped. Unverified.
4. **Are per-agent units actually named `bunker-docker-<suffix>`?** §3's suspected cgroup mismatch
   is unconfirmed — it needs one `systemctl list-units 'bunker*'` on a live host.
5. **Does any host have `bunkerd` listening on a non-loopback REST address?** Not checked; it
   changes whether path (6A) is safe without the token gate.

---

## Note A — name collisions to resolve before coding

`bunker_agents_total` (existing, host census, meaning "tenant home directories on this host") and
`bunker_agents_current` (proposed, meaning "agents in bunkerd's tracker") will disagree whenever a
home outlives its registry record — which is precisely the leak we want to detect. **Keep both,
do not rename one onto the other**, and let the gap be a metric:

```promql
bunker_agents_total - bunker_agents_current > 0   # leaked homes on this host
```
