# Bunker dogfood — the public-networking / ingress surface (2026-10-20, run 28)

**Promise tested:** README:85 — *"Networking — Cloudflare tunnels (named or
TryCloudflare), Tailscale mesh, or direct port ranges"* — driven through the
documented flag pair `bunker spawn <id> --network cloudflare --trycloudflare`
(README quick-start and `bunker spawn --help`), i.e. "give my agent a public
URL so somebody outside can reach the service running in it".

**Why this angle:** 27 prior runs touched lifecycle, mounts (sshfs, rclone,
bunker-fs), egress, TLS, renewal, ops, image-spec, isolation, release-channel
and agent-tools. The ingress/tunnel surface had **zero** real-use coverage —
`grep -ci cloudflare .coding-hermes/dogfood-log.md` returned 0 across every
earlier run.

**Environment / provenance**
- HEAD-built binaries (in-tree version 0.2.1, commit `dccaf31`) for both CLI
  and daemon: `go build -o /tmp/dftun-1020/bunker ./cmd/bunker` etc.
- Two **isolated scratch daemons** on the control host, full state isolation
  (`agent.base_data_dir` + `agent.registry.path` + `audit.path` + `ssh_dir`
  under `/tmp/dftun-1020/var*`, REST/gRPC on loopback-only 39080/39090 and
  39081/39091). Each daemon replayed 0 live agents and its orphan sweep
  destroyed nothing (see "What held").
- Live deployment touched for the documented-path check: `bunker-mvp`
  (daemon 0.2.0 commit `365ac56`, `tunnel.enabled: false` in its config).
- cloudflared 2026.5.2 on the control host, 1 cloudflared on mvp.

## 1. What happened (the honest sequence)

1. **Documented command against the live daemon** (`--server bunker-mvp
   --network cloudflare --trycloudflare`): rc=0 in **30.5s**, bundle printed
   **no Public URL row at all**, `bunker info` has no URL row, `bunker list`
   says `(no URL)`. The daemon's config has `tunnel.enabled: false`, and
   nothing in the response says the capability is off — the user who asked
   for public networking gets silence. → **DF-BUNKER-105**
2. **Same command on a daemon with tunnels enabled** (default
   `tunnel.enabled: true`): rc=0 in **21.8s**, bundle prints
   `Public URL: https://humans-preserve-brief-subtle.trycloudflare.com`.
   Fetching that URL from outside returned Cloudflare **error 1033 / HTTP
   530** ("the host is configured as a Cloudflare Tunnel, and Cloudflare is
   currently unable to resolve it") — with a service listening on the
   agent's first port and serving content. The daemon's own log said
   `TryCloudflare tunnel started ... public_url=...` for a `cloudflared`
   child that had **already exited rc=1** and was sitting in the process
   table as `[cloudflared] <defunct>`. → **DF-BUNKER-104**, **DF-BUNKER-106**
3. **Root cause, isolated with two control arms.** The daemon builds
   `cloudflared tunnel --url http://localhost:<port> --metrics
   localhost:<cfg.TunnelPort> --no-autoupdate` (`internal/tunnel/tunnel.go:90-91`),
   and `TunnelPort` defaults to **8080** (`internal/config/config.go:1472`).
   The port is **host-global** while the tunnel is **per-agent**:
   - *Control A — free metrics port.* `bunker spawn … --trycloudflare` on a
     daemon with `tunnel.tunnel_port: 39801` (isolated second daemon):
     first agent's URL served **200** with the agent's own directory listing;
     the second agent's URL was published and returned **530/1033**, and its
     `cloudflared` was a zombie. Same code, same host, only the port differs.
   - *Control B — same argv by hand.* Identical to the daemon's argv except
     `--metrics localhost:39999`: tunnel stays up, URL serves the agent's
     `marker.txt` (`public-networking-leg-2026-10-20`) **200 in 0.53s**
     through the Cloudflare edge. Drop `--metrics` entirely and cloudflared
     picks its own random metrics port (127.0.0.1:20243) and also works.
   - Exact child failure, reproduced verbatim with the daemon's argv:
     `ERR Error opening metrics server listener error="failed to bind to
     address (localhost:8080): listen tcp 127.0.0.1:8080: bind: address
     already in use"` → exit 1. cloudflared prints the quick-tunnel URL
     *before* opening the metrics listener, which is why the daemon's URL
     regex matches and `Start()` returns success.
   - On the control host (three unrelated long-lived cloudflared processes
     already hold `:8080`) **even the first** agent's URL was dead — two
     independent scratch daemons, two dead URLs, two zombies.
4. **Not a HEAD-only regression.** `--metrics`/`TunnelPort: 8080` date from
   the original TryCloudflare commit `6efcae8c` (2026-06-28);
   `internal/tunnel/tunnel.go` was last touched `a5180596` (2026-09-18),
   i.e. **before the latest release v0.2.0 (2026-10-02)** — and the install
   leg below proves that is exactly what a fresh user installs.
5. **The install leg installs a release that ships the defect.** README
   Install Option 1 (`curl -fsSL …/releases/latest/download/install.sh | sh`)
   on a fresh ephemeral agent: **install_seconds=4**, both binaries
   sha256-verified against the release `SHA256SUMS`, smoke check OK
   (`bunker 0.2.0`, commit `149411c`, built 2026-10-02), installed to
   `~/.local/bin` with the documented not-on-PATH warning. In-tree version is
   0.2.1, so the release lags the checkout (already tracked by README-009).

## 2. What held (do not regress these)

- The documented release installer works end-to-end on a bare agent and
  verifies checksums **before** writing; the source path is reachable too
  (`git clone` 2s on the public repo, `go`/`make` present).
- With a free metrics port the whole feature works: URL issued in ~3-4s
  inside the spawn, first external response **200 in 0.70-0.95s**, agent
  content served through the Cloudflare edge.
- `bunker destroy` stops a live tunnel cleanly (its cloudflared disappeared
  and the URL fell to 502) and reaps a dead child.
- The orphan-sweep guard behaved exactly as designed: a scratch daemon with a
  fresh registry found 4 unknown `bunker-*` users and **refused** to destroy
  anything, printing the reason and the remedy (this is the DF-BUNKER-102
  hazard now defended).
- `--network` flag local validation: absent (see DF-BUNKER-105) — recorded as
  a finding, not as "held".

## 3. Performance (Step 2b) — measured, no row filed

| Operation (exact command) | Cold | Warm |
|---|---|---|
| `bunker spawn <id> --network cloudflare --trycloudflare` (scratch daemon, agent create + tunnel) | 21.8s (first agent) | 18.8s (second call, same command) |
| same, against `bunker-mvp` (release daemon) | 30.5s | 23.9s (no tunnel) |
| TryCloudflare bring-up inside the spawn (daemon log: spawn start → `TryCloudflare tunnel started`) | ~4s | ~3s |
| first external fetch through the Cloudflare edge (working tunnel) | — | 0.70s / 0.95s (200); dead URL 0.25-0.78s (530) |

Nothing here is slow enough for a user to notice — the dominant cost is the
agent spawn (18.8-23.9s, against the documented 60-90s envelope, warm page
cache and a cached rootless installer), and the tunnel adds ~3-4s. **No PERF
row filed**: the defect in this run is correctness (dead URL), not latency,
and a fabricated perf win would devalue the real rows.

## 4. Verdict

**🔴 DOES-NOT-DELIVER for the public-networking promise as a multi-agent
feature; ✅ for the single-first-tunnel case.** A user with one agent on a
daemon whose metrics port is free gets a working public URL. Any second agent,
or any host where another process holds `:8080`, gets a URL that answers
Cloudflare 1033/530 while `spawn`, `bunker list` and `bunker info` all report
success. The install path (README Option 1) is solid — 4 seconds, checksum
verified — and it installs exactly the release that carries this defect.

## 5. Rows filed (bunker board `tasks.jsonl`, `boardctl validate` → RESULT: OK)

| Row | Pri | Finding |
|---|---|---|
| **DF-BUNKER-104** | P1 | Per-agent tunnel launched with a fixed host-global metrics port → second+ tunnel dies on bind, daemon reports success and publishes a dead URL (1033/530). Repro + two control arms + provenance in the row. |
| **DF-BUNKER-105** | P2 | Ingress flags accepted with no local validation and no capability report: `--network bogus` creates an agent; `--domain` and `--network tailscale` are silently discarded; on `tunnel.enabled: false` the documented command yields nothing with rc=0. |
| **DF-BUNKER-106** | P3 | Exited cloudflared children are never detected: zombie with PPID=daemon for the agent's lifetime, and the dead URL stays in the bundle, the durable registry and list/info. |

Related, already on the board and **not** duplicated here: DOC-41 (tunnel CLI
flags undocumented), DF-BUNKER-47 / -73 / -76 (spawn key-fetch / keyless-agent
class, which is why the bundle's per-agent key path does not exist for this
CLI's server token), README-009 (release-currency procedure stale),
RELEASE-003 / -008 (deployed-binary freshness).

## 6. Evidence paths (survive the tick)

- Scratch root: `/tmp/dftun-1020/` — `bunkerd.yaml`, `bunkerd2.yaml`,
  `cli.yaml`, `cli2.yaml`, `daemon.log`, `daemon2.log`, `reproA.log`
  (daemon argv, busy port), `reproB.log` (default, works), `controlA.log`
  (free port, works), `leg1-spawn.out`, `leg2-spawn.out`, `leg-ab1.out`,
  `leg-ab2.out`, `ab1.html` (200 listing), `ab2.html` (530/1033),
  `leg2-before.txt`, `install-leg.sh` output.
- Live-deployment leg: `leg1-spawn.out` (no URL row, 30.5s, mvp).
- Install leg: run on ephemeral agent `dftun-install` @ bunker-mvp
  (destroyed at the end of the run).

## 7. Cleanup

Everything this run created is temporary and was removed or is explicitly
listed: 6 agents (`dftun-1020`, `dftun-install` on bunker-mvp; `dftun-df1`,
`dftun-df2`, `dftun-df3`, `dftun-df4`, `dftun-probe`, `dftun-ab1`,
`dftun-ab2` on the scratch daemons) destroyed; both scratch daemons stopped;
no repo visibility/permission change; no credential minted. The three
unrelated cloudflared processes on the control host were left alone (not
mine), and the CI battery running on bunker-mvp was not disturbed.
