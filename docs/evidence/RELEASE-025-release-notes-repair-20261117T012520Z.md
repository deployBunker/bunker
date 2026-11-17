# RELEASE-025 — v0.3.0 release-notes repair (live release-body mutation)

- Board row: **RELEASE-025** (P2) — "Repair the published v0.3.0 GitHub Release notes"
- Foreman block: **RELEASE-029**; wave tick `bunker-foreman-2026-11-17-00-56-21`
- Worker: coding worker in `/home/kara/worktrees/bunker-RELEASE-025` (branch `wt/RELEASE-025`)
- Base: `origin/main` @ `91b453dde290112d87622c2a9d50537b42d19ce4`
- Repo under mutation: `deployBunker/bunker`, release **v0.3.0** (https://github.com/deployBunker/bunker/releases/tag/v0.3.0)
- Run timestamp (UTC): **2026-11-17T01:25:20Z**
- Tooling: `gh version 2.46.0 (2025-12-13 Ubuntu 2.46.0-4)`, authenticated as `totalwindupflightsystems` with `repo` scope (`gh auth status` rc=0; no credential material recorded here).
- Sibling worker on **RELEASE-024** (`/home/kara/worktrees/bunker-RELEASE-024`, Go toolchain pin) was not touched; no write, read of its tree, or git operation there.
- Scope discipline: exactly one GitHub Release body mutated; no tag created/moved, no asset replaced or uploaded, no other release touched, no push, no merge.

## 1. STEP 1 — pre-edit records

### 1.1 Tag object and peeled commit (pre-edit)

```
$ git rev-parse v0.3.0                    # rc=0
880ae8e4124c3f061cb5e2790caa038b231b5dbd
$ git rev-parse 'v0.3.0^{commit}'         # rc=0
de286fdce69cd92f9f182f3e1eecde34404d713e
```

Tag object type is annotated; the peeled commit `de286fdce69cf…` is also the baseline recorded by the readiness lane (`docs/evidence/RELEASE-readiness-2026-11-12.md`, "baseline/release surface" row).

### 1.2 Pre-edit release body (verbatim `gh release view v0.3.0 --repo deployBunker/bunker`, human output)

```markdown
title:	v0.3.0
tag:	v0.3.0
draft:	false
prerelease:	false
author:	github-actions[bot]
created:	2026-11-09T05:14:45Z
published:	2026-11-09T05:17:25Z
url:	https://github.com/deployBunker/bunker/releases/tag/v0.3.0
asset:	bunker-linux-amd64
asset:	bunker-linux-arm64
asset:	bunkerd-linux-amd64
asset:	bunkerd-linux-arm64
asset:	install.sh
asset:	SHA256SUMS
--
## What's Changed
* fix(agent-tools): deliver missing SSH-agent tools by @totalwindupflightsystems in https://github.com/deployBunker/bunker/pull/9
* fix(agent-tools): harden DF-BUNKER-57 stage scripts by @totalwindupflightsystems in https://github.com/deployBunker/bunker/pull/10

## New Contributors
* @totalwindupflightsystems made their first contribution in https://github.com/deployBunker/bunker/pull/9

**Full Changelog**: https://github.com/deployBunker/bunker/compare/v0.2.0...v0.3.0

```

Observed defects in the pre-edit body: **no Known issues section at all**, and the categorized changes covered only the two `fix(agent-tools)` PRs (#9, #10) rather than the `v0.2.0..v0.3.0` range.

### 1.3 Six assets, pre-edit (identity)

`gh release view v0.3.0 --repo deployBunker/bunker --json assets --jq '.assets[] | "\(.name) \(.digest)"'` returns `digest: null` for **every** asset on this gh/API combination — the digest field is not populated, so a digest equality check is not obtainable here. The recorded substitute (and the check actually performed) is `name id size state updatedAt`, which is byte-identity-or-stronger for replacement detection: any re-upload or replacement of an asset yields a new `id`.

```
bunker-linux-amd64 id=RA_kwDOTHcCxs4rStuv size=20792951 state=uploaded updated=2026-11-09T05:17:24Z
bunker-linux-arm64 id=RA_kwDOTHcCxs4rStur size=19362381 state=uploaded updated=2026-11-09T05:17:24Z
bunkerd-linux-amd64 id=RA_kwDOTHcCxs4rStus size=26596754 state=uploaded updated=2026-11-09T05:17:24Z
bunkerd-linux-arm64 id=RA_kwDOTHcCxs4rStuq size=24651013 state=uploaded updated=2026-11-09T05:17:24Z
install.sh id=RA_kwDOTHcCxs4rStvI size=33108 state=uploaded updated=2026-11-09T05:17:23Z
SHA256SUMS id=RA_kwDOTHcCxs4rStut size=419 state=uploaded updated=2026-11-09T05:17:23Z
```

### 1.4 Change census for `v0.2.0..v0.3.0`

```
$ git rev-list --count v0.2.0..v0.3.0                       # rc=0
1192
$ git log v0.2.0..v0.3.0 --no-merges --oneline | wc -l      # rc=0
1117
$ for p in feat fix perf; do ... --no-merges --pretty=%s | grep -c "^$p"; done
feat=33
fix=148
perf=11
```

`CHANGELOG.md:33` states the same range as "1191 commits, of which 33 `feat`, 148 `fix` and 11 `perf`"; the measured `git` count is **1192** (one commit difference between the prose and `rev-list`). Both numbers are disclosed in the new release body rather than silently reconciled. The full `git log v0.2.0..v0.3.0 --oneline` (1,192 lines) and the pre/post bodies were captured to a scratch dir during the run; the version- and enum-controlled facts used in the body were re-verified against the checkout:

```
$ git tag -l 'v0.2*' 'v0.3*'            # rc=0
v0.2.0
v0.3.0
$ grep -n '1,191\|Landed between' CHANGELOG.md
33:Range `v0.2.0..v0.3.0`: 1191 commits, of which 33 `feat`, 148 `fix` and 11
164:### Landed between v0.2.0 and the 0.2.1 version stamp (2026-10-02 → 2026-10-17)
```

No `v0.2.1` tag exists, so the "Landed between v0.2.0 and the 0.2.1 version stamp" section is genuinely inside the `v0.2.0..v0.3.0` range; the new body keeps it as its own labelled subsection instead of dropping it or re-dating it.

## 2. STEP 2 — composed body, and what each required element maps to

The repaired body was written to `RELEASE-025-v0.3.0-body.md` in the worktree (untracked, removed after the edit; its exact published text is reproduced verbatim in §4 below) and applied with `--notes-file`.

| Requirement | Where it lands in the published body |
| --- | --- |
| "## Known issues" near the top, **before** the categorized changelog | `## Known issues` at body line 18; `## What's in this release` at line 40 (verified by line-number comparison, §6) |
| Name GO-2026-6859, no exploit claim | Known issues item 1: names GO-2026-6859 / CVE-2026-97054, states "scanner-confirmed, not exploited", quotes `govulncheck` rc=3 on source at `f24428ae` and rc=3 in binary mode on the official `bunkerd-linux-amd64` asset, and states "No exploit and no reproduction is claimed." |
| Built with go1.26.10; fixed rebuild go1.26.11 in preparation under RELEASE-024; **never promise** patched artifacts | Known issues item 1: "built with **go1.26.10**", "fixed rebuild (**go1.26.11**) is in preparation under board row **RELEASE-024**", then "**No patched artifact is published.** Nothing in these notes announces, dates or promises one — the assets attached to v0.3.0 are unchanged by this notes edit." |
| Reaper/rollout residuals only as measured, with row IDs and workarounds | Known issues items 2 and 3: rollout census + RELEASE-003/RELEASE-024; reaper residual DF-BUNKER-116 with the measured `--purge` workaround. No residual is included without a row ID and a measurement. |
| QA limits stated without claiming N/A as PASS | Known issues item 4: RELEASE-028, QA-BUNKER-106, QA-BUNKER-115 ("**UNVERIFIED by that cell** — absence of a failure there is not a pass"), QA-BUNKER-096, QA-BUNKER-094, plus an explicit "unperformed check, not a pass" statement about this repair itself. |
| Categorized changes reflect the full range, not two PRs | `## What's in this release` states the range and the census (1,192 / 1,191, 33 feat, 148 fix, 11 perf), then Added (7), Changed (4), Fixed (3), and the 8-change "Landed between v0.2.0 and the 0.2.1 version stamp" subsection — the curated `CHANGELOG.md` §0.3.0 census rather than the two-PR list. |
| Preserve all existing contributor credit | `## Contributors` keeps `### New Contributors` (first contribution, PR #9) and the two original `pull/9` / `pull/10` lines verbatim, plus the `Full Changelog` compare link. |
| No retag / no asset change / no other release | §5: tag object+peeled identical; all six assets identical (id/size/state/updatedAt); only `v0.3.0`'s body was written. |

Residual wording that could not be evidenced was **dropped, not invented**: e.g. the pre-edit body's rollout numbers were re-checked against `docs/evidence/RELEASE-readiness-2026-11-12.md` (version table: 0.1.3 ×1 below-floor, 0.1.4 ×7, 0.2.1 ×5, `cube-las-00` unreachable, `hermes-fleet-01` refused on a 62-hex `cert_pin`) and the reaper figures against row DF-BUNKER-116 itself (10 registry records, 9 expired 21–25 h, 27.1 GB, 2.8 GB archive then HTTP 404).

## 3. STEP 3 — the applies (every command, with rc)

Two `gh release edit` invocations were made against the **same single release body**. The first applied the composed body. Its read-back exposed two statements that the repo records did not support exactly (the DF-BUNKER-116 workaround was recorded as executed `bunker destroy <id> --purge`, not "manual reclaim"; and QA-BUNKER-096 is "golangci-lint is never installed", not "LINT_CMD never runs"), so a **second, corrective** edit replaced those two sentences plus the certification sentence in the RELEASE-028 bullet. No invocation was retried after a failure, and no body was applied twice: `rc=0` on both, with distinct content each time.

```
$ gh release edit v0.3.0 --repo deployBunker/bunker --notes-file RELEASE-025-v0.3.0-body.md
https://github.com/deployBunker/bunker/releases/tag/v0.3.0
rc=0

$ gh release edit v0.3.0 --repo deployBunker/bunker --notes-file RELEASE-025-v0.3.0-body.md   # corrective
https://github.com/deployBunker/bunker/releases/tag/v0.3.0
rc_edit2=0

$ gh release view v0.3.0 --repo deployBunker/bunker > post2-release-view.txt
rc_view=0
$ gh release view v0.3.0 --repo deployBunker/bunker --json assets --jq '.assets[] | "\(.name) id=\(.id) size=\(.size) state=\(.state) updated=\(.updatedAt)"' > post2-assets-identity.txt
rc_assets=0
$ git rev-parse v0.3.0 ; git rev-parse 'v0.3.0^{commit}'
rc=0, rc=0
```

The correction is visible in the final body: `grep -c 'purge. cleaned all 9 zombies'` → `1`.

## 4. STEP 4 — post-edit read-back (verbatim, final state)

```markdown
title:	v0.3.0
tag:	v0.3.0
draft:	false
prerelease:	false
author:	github-actions[bot]
created:	2026-11-09T05:14:45Z
published:	2026-11-09T05:17:25Z
url:	https://github.com/deployBunker/bunker/releases/tag/v0.3.0
asset:	bunker-linux-amd64
asset:	bunker-linux-arm64
asset:	bunkerd-linux-amd64
asset:	bunkerd-linux-arm64
asset:	install.sh
asset:	SHA256SUMS
--
v0.3.0 — released 2026-11-09

## Known issues

1. **GO-2026-6859 / CVE-2026-97054 — the published binaries are built with an affected Go toolchain.** This is a Go toolchain vulnerability in the HTTP/1 server path (a `MaxBytesReader` header race in `net/http`), found in go1.26.10 and fixed in go1.26.11 / go1.27.4 (advisory published 2026-11-11T02:51:11Z, https://vuln.go.dev/ID/GO-2026-6859.json). It is **scanner-confirmed, not exploited**: `govulncheck` exits 3 on source at `f24428ae` and exits 3 in binary mode against the official `bunkerd-linux-amd64` asset downloaded from this release. No exploit and no reproduction is claimed.
   - The v0.3.0 binaries were built with **go1.26.10** (`go.mod` `toolchain=go1.26.10`; all four published release binaries embed that version).
   - A fixed rebuild (**go1.26.11**) is in preparation under board row **RELEASE-024** (scoped there to the v0.4.0 prep; the row records that the security rebuild earns a patch release).
   - **No patched artifact is published.** Nothing in these notes announces, dates or promises one — the assets attached to v0.3.0 are unchanged by this notes edit. Until a patched build is verified published, treat v0.3.0 binaries as affected and prefer an older, non-affected install over v0.3.0.
   - Evidence of the sweep: `docs/evidence/RELEASE-readiness-2026-11-12.md`.

2. **Rollout residual (measured 2026-11-12): no daemon in the fleet is running 0.3.0.** Census: 13 daemons answering — 0.1.3 ×1 (`bunker-las-04`, below the supported floor), 0.1.4 ×7, 0.2.1 ×5; `cube-las-00` unreachable; `hermes-fleet-01` refuses its 62-hex `cert_pin`. Rows: **RELEASE-003** (deployed binaries stale relative to the tag) and **RELEASE-024**.
   - Workaround: stay on the installed 0.2.1 / 0.1.4 daemons. Version skew between CLI and daemon is supported, but the **older** side must be the daemon, never the client.

3. **Reaper residual (DF-BUNKER-116, P1, open): expired agents are never reaped.** Measured on `bunker-mvp` (bunkerd 0.2.1, started 2026-11-03T20:55Z) at 2026-11-05T13:07Z: `bunker status --server bunker-mvp` reported `Agents 0/50` and "10 registered agents", while `/var/lib/bunkerd/agents.jsonl` still held 10 records with `status=running`; 9 had expired 21–25 h earlier and every one still owned a user, a home and a `/run/bunker/<id>` dir — **27.1 GB** of dead weight. A second, independent defect on the same host: `bunker destroy` archived a home (2.8 GB tarball, 2.5 min) **before** discovering the agent was gone, then returned `not found` (HTTP 404) with the home and the tarball both retained.
   - This release adds `bunker reap` (INT-HOST-007), a one-pass sweep over the planes an interrupted destroy leaves behind. It is a **remedy, not the fix**: it does not make the TTL reaper reap, and its `Probe: partial` line means at least one plane was unreadable, so its counts are a lower bound.
   - Workaround, executed and measured: `bunker destroy <id> --purge` cleaned all 9 zombies without error, so the CLI/registry teardown path works — it is the daemon's TTL reaper / reconciliation path that silently leaves them, and it emits no reap or ttl log lines at all, so the miss is invisible to operators. The durable fix must be judged by live reaper evidence on a real host, not by unit mocks.

4. **QA and certification limits — stated as limits, not as passes.**
   - **RELEASE-028:** root-suite and regression CI sit queued behind a single self-hosted runner, so CI cannot certify any candidate right now. No green candidate certification is claimed for the v0.3.0 assets: the gate that would produce one cannot currently run.
   - **QA-BUNKER-106:** `unit-tests` CI is non-deterministic across runner root shapes (gap118 tests resolve the real whole-disk device), so a red unit-tests cell is not by itself evidence of a code defect.
   - **QA-BUNKER-115:** the `chaos-disconnect` cell grades OK on a go-toolchain compile failure (any non-zero rc counts as "fails fast"), so **disconnect behaviour is UNVERIFIED by that cell** — absence of a failure there is not a pass.
   - **QA-BUNKER-096:** the battery's lint stage never runs — `bunker-qa.sh` sets `LINT_CMD='golangci-lint run'` for `.golangci.yml` repos but never installs golangci-lint, so lint grades INFO "not run" on every Go battery. **QA-BUNKER-094:** `ci-pass` runs `act -q`, so a red job's step stdout is never captured and the cell grades FAIL with no attribution. A battery "pass" therefore does not cover linting and a battery FAIL does not name the failing test.
   - **This notes repair itself** verified that the release's tag and six assets are unchanged (recorded identity before/after). It performed **no vulnerability re-scan and no rebuild** of those assets. That is an unperformed check, not a pass.

## What's in this release

Range `v0.2.0..v0.3.0`: **1,192 commits** (CHANGELOG.md records 1,191), of which **33 `feat`**, **148 `fix`** and **11 `perf`**. Board, foreman and harness bookkeeping commits are not listed below. The categorized entries are the curated census from `CHANGELOG.md` §0.3.0; the complete commit list is the compare link at the end.

### Added

- **`bunker reap` — one pass over every plane an interrupted destroy leaves behind (INT-HOST-007).** Dry run by default; `--apply` removes only entries whose user no longer exists, across rootlesskit AppArmor profiles, agent homes, systemd linger entries and the per-agent `/run/bunker/<id>` runtime dirs that no destroy revisits when the destroy never ran. User lookups run concurrently (PERF-016, 2.1x) and the home-size walk is level-synchronous (PERF-015, 5.81x at load 59–62). `Probe: partial` means at least one plane was unreadable, so counts are a lower bound.
- **`bunker key fetch` — CLI recovery for an agent whose key was lost (DF-BUNKER-73).** Re-fetches an agent's SSH key from the daemon with an operator token, so a deleted `~/.config/bunker/keys/<agent-id>` no longer means destroying a live agent. Spawn moved to the same operator-token path (DF-BUNKER-47-R2).
- **SSH key families and second-family recovery (QA-BUNKER-46).** An agent can carry a second key family so a rotated or lost primary key does not orphan it; the recovery ladder is documented in the runbook the row points at.
- **`hidepid` network mode (NET-BUNKER-011) and a namespace-aware port allocator (NET-BUNKER-009).** Procvis/hidepid mode vocabulary with its gates, units and detached-run wiring; port allocation resolves per-network namespaces instead of assuming one flat host range. The startup-reconciliation and spawn-in-flight sweep fixes (FLEET-INFRA-1) close the pool leak and the race that let a reservation be reaped mid-spawn.
- **Named destroy refusals over the wire.** `DestroyAgent` returns `FailedPrecondition` with the refusal's own message instead of a generic failure, so a client can tell "agent has live processes" from "agent not found" without parsing prose.
- **`agent-tools` delivery is automatic at spawn and reversible (TOOLS-003).** The tools an agent's editing verbs need are delivered at creation; `--install` still works ad hoc and `--uninstall` removes the delivered files and re-probes to prove absence.
- **`rest_addr` parity (QA-BUNKER-74).** `config.example.yaml` shows a reachable tailnet bind and the client's dial address derives from the same value, so the example no longer documents a listen address the CLI never dials.

### Changed

- **Observability is loopback-only by default.** `bunkerd` exposes Prometheus metrics on loopback with a `bunker_webdav_enabled` discriminator, and the textfile shim refuses a non-`bunkerd` HTTP 200 instead of reporting a stale scrape as fresh (previously the shim's own health page could satisfy its freshness probe).
- **Spawn refusals are named.** `--network` is validated and unservable ingress combinations are refused before any state is created (DF-BUNKER-105); the uid-collision precheck steps over bounded residue instead of failing (INT-CI-060); `bunker list` bounds its connect phase so an offline server cannot hang the command; `bunker tunnel` no longer pins cloudflared's metrics listener to a host-global port (DF-BUNKER-104); `bunker iobattery` classifies unsupported write shapes as their own outcome (DF-BUNKER-101); `make install` honours `PREFIX` and `DESTDIR`; persistent `exec` pins `TMPDIR` inside the agent's data dir.
- **Performance (measured, not asserted).** Canonical audit verification reads the raw stored bytes instead of re-deriving fields (PERF-017, 3.55x); bounded audit queries use an O(1) backward scan (PERF-003); `bunker reap` classifies user lookups concurrently (PERF-016, 2.1x); `bunker homes` walks sizes in parallel (PERF-015, 5.81x under load).
- **The rootless installer is pinned to an immutable commit template (`d492f85d`)**, closing the last moving-URL input in the supply-chain manifest path.

### Fixed

- **The local release gate no longer forbids the release order it documents (RELEASE-021).** `scripts/pre-release-check.sh` tree mode failed whenever `git describe --tags --abbrev=0` was not exactly `v<version>`, so the bump commit — which `docs/release-checklist.md` requires to pass the gate BEFORE its tag exists — could never be green locally. The gate now mirrors CI's INT-CI-072 policy: a newest tag OLDER than `Version` is the between-tags bump state and prints a `WARN` naming the `git tag` step, while a `Version` that has gone BEHIND the newest tag is still a hard `FAIL`. Source disagreement and the `## Unreleased` heading rule are unchanged, as is `--tag` mode.
- **A client deadline is not the daemon's verdict, and `destroy --timeout` exists (DF-BUNKER-61, DF-BUNKER-81).** On a deadline expiry `bunker destroy` now re-lists the daemon's agents before saying anything: gone → reports the agent destroyed and exits 0; still listed → reports the destroy as NOT CONFIRMED and exits non-zero; listing impossible → same fail-closed report naming the reason. It never resends a destroy that may be in flight, so the confirmation is read-only. `--timeout <duration>` sets an explicit client deadline and skips the size probe.
- **Agents can reach their own docker socket again (INT-CI-080).** The ISO-002 path-visibility fix set `/run/bunker` to `root:0700`, which sealed tenant enumeration but also removed the others-execute bit every agent needs to traverse to its own control channel; every agent-side docker command failed while every root-side check kept passing. The parent is now `root:0711` (no group/world read, unchanged threat model). `AssertAgentRunRoot` reddens on `0700` as well as `0755`/`0750`, `EnsureAgentRunRoot` repairs a host on the next spawn, and the E2E battery now probes reachability AS THE AGENT.

### Landed between v0.2.0 and the 0.2.1 version stamp (2026-10-02 → 2026-10-17)

The `0.2.1` version stamp and the CHANGELOG section it created were **never tagged** (the cut was held on the pre-release gate defect fixed above), so these changes ship in v0.3.0 and are part of the `v0.2.0..v0.3.0` range.

- **The bunker-fs mount's write shape is documented, printed and carried on its refusals (DF-BUNKER-100).** The mount publishes WHOLE FILES only: `git init` fails with `chmod on .git/config.lock failed: Operation not supported`, and truncating writes onto an existing path are refused `EOPNOTSUPP`. The contract now lives in one place (`internal/fsclient/writeshape.go`), is printed by `bunker fs mount` help and its mount banner, and is recorded in README, `docs/mount-drivers.md` §4, `docs/spec/BFS-005` §5.5 and the dogfood report. Refusals are named `write_shape_unsupported` and counted in the mount's `write_shape` status block with the refused path.
- **Supply-chain pinning (GAP-135): the installer's downloads and the base images are pinned by version + sha256/digest.** Moving URLs (`get.docker.com/rootless`, `releases/latest/download/<binary>`) and the `ubuntu:24.04` tag are replaced by a manifest checked into the repo and embedded in the binary (`internal/supplychain/manifest.json`); every consumer verifies before it executes, extracts or builds, and a mismatch is a named error (`supplychain.ErrChecksumMismatch`) that removes the artifact and fails the spawn — never a warning, never a fallback to unverified bytes. Re-pinning is a reviewed manifest edit with a documented procedure and a live upstream probe (`BUNKER_LIVE_PIN_PROBE=1`); see `docs/supply-chain-pinning.md`.
- **Per-agent resource events (GAP-121): see what an agent HIT, not just what it is using.** `bunker resource-events <agent-id>` surfaces cgroup v2 ceiling events (`memory.events`: `max`, `high`, `low`, `oom`, `oom_kill`, `oom_group_kill`; `pids.events`: `max`) plus observed peaks; `bunker metrics <agent-id>` gains a one-line `Ceiling events:` summary. Honesty rules are built in and tested: an unreadable cgroup renders `UNAVAILABLE`, a daemon predating the surface renders `NOT REPORTED`, and a sampler-derived peak is labelled `a LOWER BOUND` — neither is ever rendered as a clean zero. Wire surface: `ResourceEvents` on `AgentMetricsResponse` (field 11). Proto codegen is reproducible: `buf.gen.yaml` pins `protocolbuffers/go:v1.36.12` and `connectrpc/go:v1.19.0`. Reference: `docs/resource-events.md`.
- **Operators & RBAC (GAP-130): per-operator identities with roles.** New `bunker operator create|list|delete` (admin only) provisions named credentials with one of three roles — `admin`, `operator` (agent lifecycle only; operator management, key and secret lifecycle, audit query and host-level server info are denied with `permission_denied`), and `agent:self` (a machine identity bound to one agent). Legacy credentials keep their exact pre-GAP-130 behavior. Enforcement runs as an RBAC interceptor after auth and audit (denials are hash-chained, naming `op:<name>`); identities persist in `<base_data_dir>/operators/operators.jsonl` (0600); tokens are shown once and stored only as SHA-256 hashes.
- **Destroy gate remedy made self-satisfying for rootless agents (DF-BUNKER-66).** `bunker destroy` and `bunker renew` used to refuse with `live_processes` while counting the rootless-docker login session's own `default.target` services (dbus-daemon, pipewire, wireplumber, mpris-proxy) that no agent-side action could stop. The destroy path now stops that session infrastructure itself — the systemd user manager first, then the session services, with a bounded SIGKILL pass — and the live-process gate absorbs the remainder. A refusal now names only real operator processes.
- **`bunker agent-tools --install` delivers `rg` and `gopls` at runtime (DF-BUNKER-57).** On SSH-based agents no image-spec path exists, yet the probe names `rg` REQUIRED; the install pulls the same pinned, signature-verified apt archive and bootstraps a checksum-pinned Go toolchain for `go install golang.org/x/tools/gopls`. Every stage is re-probed and the install fails naming any tool it could not prove installed; `--uninstall` removes runtime-delivered files too.
- **Delivered tool set for the remote-editing verbs (TOOLS-B2):** `rg` via apt `ripgrep` and `gopls` ship through the image-spec package-add path, recorded canonically in `internal/imagespec` (`AgentToolPackages` / `AgentToolSpec`) and pinned by tests; see `specs/image-spec.md` §3.
- **`bunker pull <agent-id> <remote-path> [local-dir]`** copies a file FROM an agent's environment to the local machine (`bunker cp` is push-only); `--recursive` copies directories (FEAT-CLI-PULL-001, commit `558f8956`).

## Contributors

### New Contributors

* @totalwindupflightsystems made their first contribution in https://github.com/deployBunker/bunker/pull/9

### Pull requests in this release (as originally published)

* fix(agent-tools): deliver missing SSH-agent tools by @totalwindupflightsystems in https://github.com/deployBunker/bunker/pull/9
* fix(agent-tools): harden DF-BUNKER-57 stage scripts by @totalwindupflightsystems in https://github.com/deployBunker/bunker/pull/10

**Full Changelog**: https://github.com/deployBunker/bunker/compare/v0.2.0...v0.3.0

```

Post-edit body bytes: **16068**.

## 5. Tag and assets — before vs after

### 5.1 Tag object and peeled commit

```
$ diff pre-tag-object.txt post2-tag-object.txt ; echo rc=$?
rc=0   # identical

before object  880ae8e4124c3f061cb5e2790caa038b231b5dbd
after  object  880ae8e4124c3f061cb5e2790caa038b231b5dbd

before peeled  de286fdce69cd92f9f182f3e1eecde34404d713e
after  peeled  de286fdce69cd92f9f182f3e1eecde34404d713e

$ diff pre-tag-peeled.txt post2-tag-peeled.txt ; echo rc=$?
rc=0   # identical
```

**Tag unchanged (before == after): YES.** No `git tag` write of any kind was issued; `git tag -l "v0.2*" "v0.3*"` still lists exactly `v0.2.0` and `v0.3.0`.

### 5.2 Six assets — identity before (left) and after (right)

`gh release view v0.3.0 --json assets --jq ".assets[] | \"\\(.name) id=\\(.id) size=\\(.size) state=\\(.state) updated=\\(.updatedAt)\""`

```
--- BEFORE ---
bunker-linux-amd64 id=RA_kwDOTHcCxs4rStuv size=20792951 state=uploaded updated=2026-11-09T05:17:24Z
bunker-linux-arm64 id=RA_kwDOTHcCxs4rStur size=19362381 state=uploaded updated=2026-11-09T05:17:24Z
bunkerd-linux-amd64 id=RA_kwDOTHcCxs4rStus size=26596754 state=uploaded updated=2026-11-09T05:17:24Z
bunkerd-linux-arm64 id=RA_kwDOTHcCxs4rStuq size=24651013 state=uploaded updated=2026-11-09T05:17:24Z
install.sh id=RA_kwDOTHcCxs4rStvI size=33108 state=uploaded updated=2026-11-09T05:17:23Z
SHA256SUMS id=RA_kwDOTHcCxs4rStut size=419 state=uploaded updated=2026-11-09T05:17:23Z
--- AFTER ---
bunker-linux-amd64 id=RA_kwDOTHcCxs4rStuv size=20792951 state=uploaded updated=2026-11-09T05:17:24Z
bunker-linux-arm64 id=RA_kwDOTHcCxs4rStur size=19362381 state=uploaded updated=2026-11-09T05:17:24Z
bunkerd-linux-amd64 id=RA_kwDOTHcCxs4rStus size=26596754 state=uploaded updated=2026-11-09T05:17:24Z
bunkerd-linux-arm64 id=RA_kwDOTHcCxs4rStuq size=24651013 state=uploaded updated=2026-11-09T05:17:24Z
install.sh id=RA_kwDOTHcCxs4rStvI size=33108 state=uploaded updated=2026-11-09T05:17:23Z
SHA256SUMS id=RA_kwDOTHcCxs4rStut size=419 state=uploaded updated=2026-11-09T05:17:23Z
--- DIFF ---
$ diff pre-assets-identity.txt post2-assets-identity.txt ; echo rc=$?
rc=0   # byte-identical, 6 lines
```

**Assets unchanged (6/6): YES.** Same six names, same ids, same sizes, same `state=uploaded`, same `updatedAt`.

## 6. Assertion results (all measured against the final published body)

| Assertion | Result | Evidence |
| --- | --- | --- |
| Known issues positioned before the categorized changes | **MET** | `## Known issues` at line 18, `## What's in this release` at line 40, `known_before_changelog=MET` (`awk` line-number comparison) |
| GO-2026-6859 named | **MET** | `grep -q 'GO-2026-6859'` → `go_advisory=MET`; advisory ID + CVE-2026-97054 + vuln.go.dev URL present; "No exploit and no reproduction is claimed." |
| Range `v0.2.0..v0.3.0` reflected | **MET** | `grep -q 'v0.2.0..v0.3.0'` → `range=MET`; census line carries 1,192 / 1,191 / 33 feat / 148 fix / 11 perf; range also stated in the 0.2.1-stamp subsection |
| Credits preserved | **MET** | `credits=MET` — `### New Contributors` with the first-contribution PR #9 line and both original `pull/9` / `pull/10` lines verbatim, plus the `Full Changelog` compare link |
| Tag object + peeled commit unchanged | **YES** | `diff` of before/after files rc=0 for both (`obj_unchanged=YES`, `peeled_unchanged=YES`) |
| All six assets unchanged | **YES** | `diff` of the six-line identity lists rc=0 (`assets_unchanged=YES`, `asset_count=6`) |
| Published correction actually live | **MET** | `grep -c 'purge. cleaned all 9 zombies'` → `1` |

## 7. Residual / limits of this evidence

- **Asset digests are not obtainable from this gh build** (`digest: null` for all six). The before/after comparison therefore rests on `name id size state updatedAt` equality, which detects any replacement or re-upload (a replacement gets a new asset `id`). This is recorded as a substitution, not as a digest check.
- **No vulnerability re-scan and no rebuild** of the published assets was performed by this lane; the release body says so explicitly rather than implying a pass. The GO-2026-6859 finding is carried from the readiness lane's scan record (`docs/evidence/RELEASE-readiness-2026-11-12.md`), which scanned the *downloaded* v0.3.0 `bunkerd-linux-amd64`.
- **The patched rebuild is not published.** The body references RELEASE-024 as work in preparation and states plainly that no patched artifact exists yet; the v0.3.0 assets are untouched, so v0.3.0 remains the affected build until a verified rebuild is published.
- **Census count differs by one** from `CHANGELOG.md` (1,192 measured vs 1,191 recorded). Both are disclosed in the body; the discrepancy was not silently resolved.
- **Two applies, one body.** The second was a corrective replacement of three sentences after read-back; it was not a retry of a failed call (`rc=0` both times) and the same body was never applied twice.
- **Scope:** no tag created or moved, no asset uploaded or deleted, no other release touched, no push, no merge; the worktree's only committed change is this evidence file.
