# Bunker documentation gap audit — scheduler tick bunker-docs-2026-11-07-00-08-38

Lane: `bunker-docs` (documentation audit; files rows, never edits the docs).
Target: `/home/kara/deployBunker/bunker` (`git@github.com:deployBunker/bunker.git`, branch `main`).
Canonical board: `.coding-hermes/board/tasks.jsonl`.
Audit host: karaHermes-mde-7840hs, 2026-11-07T00:26Z.

Target revision at probe: `062e8e6c30fb2349b85f5bd54a8f20e7b95e2927`
(`board: tick 00-10-36 OBS-BUNKER-008 recovered with real comm...`). HEAD moved one board-only
commit during the sweep (from `330d682a`); no Go file differs between them, so the surface below
is HEAD-true.

Surface oracle: a binary built from this checkout at the audited revision —
`go build -o <stand-in>/bunker-audit-2026-11-07 ./cmd/bunker` (rc=0), reporting
`bunker 0.2.1 / commit: 330d682 / built: 2026-11-06T16:10:50Z / go1.26.10 / linux/amd64`.
The checked-in `./bunker` (built 2026-10-30) was NOT used: a stale binary can make a registered
command look absent.

## Ownership and write preflight

- `git status --porcelain` (board paths): clean — `.coding-hermes/` shows no modification before
  the write. The only dirty path in the tree is `M .gitreins/tasks.yaml` (GitReins lifecycle
  bookkeeping, not a board path); this audit did not read, stage, or commit it.
- `boardctl validate -C /home/kara/deployBunker/bunker` BEFORE: `RESULT: OK (977 warning(s))`,
  rc=0. Warnings are inherited legacy-shape (`event has no id field`), not this sweep's.
- Minter (SCHED-GAP-1690): `python3 ~/.hermes/scripts/board_next_id.py <board> DOC --count 5`
  → `DOC-075 DOC-076 DOC-077 DOC-078 DOC-079`; each confirmed absent by exact-id grep of the
  board (five `0` counts).

## Surface derivation (what was derived, and from where)

1. CLI: `bunker --help` on the fresh binary lists **44 registered commands**
   (agent-tools, audit, completion, connect, cp, deploy, destroy, env, exec, fs, guard,
   heartbeat, help, homes, host-provision, info, iobattery, key, linger, list, metrics, mount,
   operator, paths, pull, reap, registry, renew, resource-events, restart, run, spawn, ssh,
   start, status, stop, subid-migrate, surface, systemd, toolchain-check, tunnel, umount, use,
   version). Each verb was grepped for `bunker <verb>` across README + `docs/**/*.md` +
   `specs/*.md` (evidence/dogfood trees excluded). Only two verbs have zero README coverage —
   `iobattery` and `paths` — and both are already the subject of open row **README-010**;
   no duplicate was filed.
2. Config keys: derived from `internal/config/config.go` mapstructure tags (§13 recipe class):
   **115 keys**. Diffed against `specs/configuration.md` (self-described "the complete surface")
   and `config.example.yaml`: **3** keys absent from the reference — `agent.hardened_unit_properties`
   (open row DOC-052), `agent.proc_group` (open row DOC-063), and
   `server.metrics_enabled` (finding F2 below, no prior row).
3. Environment variables: 59 `BUNKER*`/`BUNKERD*` names read in `cmd`/`internal`; each classified
   production-vs-test. Only two survive as production reads with zero mentions in any tracked
   `.md`: `BUNKER_CONFIG_HOME` (internal/cli/paths.go:48) and `BUNKER_UID_COLLISION_MAX_RETRIES`
   (internal/agent/df_bunker_63.go:148 — an internal retry bound, deliberately not filed).
   The rest are `_test.go`-only fixtures and were excluded by classification, not by assumption.
4. Docs rot: markdown link census over 107 tracked markdown files (evidence/history trees
   excluded), 193 relative links checked → **2 dangling** (finding F5). A "documented verb does
   not exist" census over the same corpus produced only prose false positives (e.g. "bunker
   thread" in an author line, "bunker agent" in ordinary text); no removed command is still
   taught for the audience axis.
5. Audience axis: front-door/discoverability (end user + operator) is where the findings landed;
   integrator coverage was checked at `specs/api.md` existence only (not re-audited this tick —
   open rows DOC-034/065 already own that surface).

## Findings filed (5)

### F1 → DOC-075 (P4, rot) — `docs/observability-spec.md` declares `/metrics` absent and tiers 2–4 unshipped

    $ grep -n '^Status:' docs/observability-spec.md
    3:Status: tier 1 implemented (2026-11-05); tiers 2–4 remain proposed.
    $ sed -n '12,16p' docs/observability-spec.md
    | Does bunkerd expose `/metrics`? | **No.** | `grep -rn '"/metrics"' --include=*.go` → 0 hits ...
    $ grep -rn --include='*.go' '"/metrics"' cmd internal | grep -v _test
    internal/server/metrics.go:30:  r.Get("/metrics", newMetricsHandler(...).ServeHTTP)
    $ grep -c prometheus go.mod
    4
    $ grep -rl --include='*.go' bunker_rpc_requests_total internal | grep -v _test
    internal/audit/interceptor.go
    $ grep -rl --include='*.go' bunker_webdav_watch_heartbeats_total internal | grep -v _test
    internal/server/metrics.go

Tier 2 (RPC counters/histogram) landed in `internal/audit/interceptor.go`; tiers 3 and 4
(destroy/refusal counters, WebDAV watchdog + push series — 27 distinct `bunker_*` series in
`internal/server/metrics.go`) are in `internal/server/metrics.go`. Board rows OBS-BUNKER-001..004
are `complete`. The doc's §0 table is `docs/observability-spec.md:12-16` and its status line is
line 3.

### F2 → DOC-076 (P4, missing-reference) — `server.metrics_enabled` absent from the complete-surface config reference

    $ grep -c metrics_enabled specs/configuration.md
    0            (rc=1)
    $ grep -n 'MetricsEnabled bool\|server.metrics_enabled' internal/config/config.go
    375:  MetricsEnabled bool `mapstructure:"metrics_enabled"`
    1626: v.BindEnv("server.metrics_enabled")
    1776: return fmt.Errorf("server.metrics_enabled requires server.rest_addr to be an IP loopback address ...")

`specs/configuration.md` states "Every key on this page is read from the Go source" and "this page
is the complete surface" (lines 1-17), Last Updated 2026-10-09 — before the key existed. The only
prose that names the key is `docs/observability-spec.md:220` and dogfood evidence.

### F3 → DOC-077 (P4, missing-reference / discoverability) — the observability surface has no front door

    $ grep -c observability README.md ; grep -c '/metrics' README.md
    0
    0
    $ grep -rln --include='*.md' observability-spec . \
        | grep -vE '(\.gitreins/|docs/dogfood/|docs/evidence/|.coding-hermes/)' | wc -l
    0

`docs/observability-spec.md` is the only operator reference for `/metrics`,
`bunker metrics --prometheus-textfile` and the textfile collection path, and no other tracked
markdown file links to it (it is orphaned), while the README never mentions the surface at all.
Sibling of DOC-075/DOC-076 (different artifact: the entry point, not the reference content).

### F4 → DOC-078 (P4, wrong-reference) — README still documents the pre-GAP-181 config-file ladder

    $ sed -n '1467,1476p' README.md
    **CLI config file** (server registry + active server):
    ```
    --config /path/to/config.yaml     # highest precedence (persistent root flag)
    BUNKER_HOME=/path/to/state        # config at $BUNKER_HOME/config.yaml
    $HOME/.bunker/config.yaml         # default (unchanged)
    ```
    $ grep -c BUNKER_CONFIG_HOME README.md specs/configuration.md docs/backup-set.md
    README.md:0 specs/configuration.md:0 docs/backup-set.md:0
    $ grep -n 'CLI config file. Precedence' cmd/bunker/main.go
    104/131: "(... ) Precedence: --config > $BUNKER_HOME/config.yaml > $HOME/.bunker/config.yaml"

Live proof from the audited binary, in a throwaway HOME with the env unset:

    $ env -u BUNKER_HOME -u BUNKER_CONFIG_HOME -u XDG_CONFIG_HOME HOME=<tmp>/home <bin> paths
    cli  config.yaml (server registry, per-host tokens)  <tmp>/home/.config/bunker/config.yaml  0600  xdg
    ...
    Precedence: flag > env > per-user config dir (${XDG_CONFIG_HOME:-~/.config}/bunker) > legacy ~/.bunker (read alias) > system defaults

    $ env -u BUNKER_HOME -u XDG_CONFIG_HOME BUNKER_CONFIG_HOME=<tmp>/cfg HOME=<tmp>/home <bin> paths
    cli  config.yaml ... <tmp>/cfg/bunker/config.yaml  0600 (intended, missing)  env     # BUNKER_CONFIG_HOME is live

So at HEAD the default is `${XDG_CONFIG_HOME:-~/.config}/bunker/config.yaml`, `~/.bunker` is a read
alias only, and `BUNKER_CONFIG_HOME` is a live knob documented in **no** tracked markdown file.
Stated sibling of open row README-007 (same GAP-181 lag, different artifact: README-007 owns the
agent-key path lines, this row owns the config-file block + the undocumented env var + the flag
help string at `cmd/bunker/main.go:131`).

### F5 → DOC-079 (P5, rot) — two relative markdown links that cannot resolve

    $ sed -n '126p' docs/prd/SPEC-bunker-native-file-tools.md
    Related: [docs/both-ways.md](../../both-ways.md) — when to use the verbs vs the SSHFS mount.
    $ sed -n '174p' specs/architecture.md
    [specs/safety-presets.md](specs/safety-presets.md) ("Per-file size") and
    $ ls -1 both-ways.md specs/specs 2>&1
    ls: cannot access 'both-ways.md': No such file or directory
    ls: cannot access 'specs/specs': No such file or directory
    $ ls docs/both-ways.md specs/safety-presets.md
    docs/both-ways.md   specs/safety-presets.md

Both links are written repo-relative inside a file that resolves links file-relative: the PRD one
needs `../both-ways.md`, the architecture one needs `safety-presets.md`. Census basis: 107 tracked
markdown files, 193 relative links, 2 misses (no other miss in the corpus).

## Board write boundary

Writes performed by this sweep: the five `boardctl create` rows above plus one `boardctl update`
re-triage (GAP-177 priority P2 → P4, doc-severity policy only) and one `boardctl event` audit
record. Not touched: README, `docs/**`, `specs/**` (this lane never edits documentation), the
`iobattery`/`paths` README inventory gap (already open as README-010), `.gitreins/tasks.yaml`
(foreign dirty path). If any further board mutation is performed after this artifact is frozen it
is recorded in a separately named addendum, never by editing this file.

## What was exercised, and what was not

- Exercised (T3): the freshly built CLI — `--help`, `metrics --help`, and four `paths` runs under
  isolated `HOME`/`XDG_CONFIG_HOME`/`BUNKER_CONFIG_HOME` environments.
- Exercised (T2): source/docs census (grep, link census, config-key derivation).
- NOT exercised: no `bunkerd` was started, so the live `/metrics` boundary (HTTP 200 with
  `server.metrics_enabled: true`) was not re-observed this tick — DOC-075's acceptance is
  documentation-vs-source, and the live arm is already owned by OBS-BUNKER-006/008 (blocked) and
  the 2026-11-06 dogfood record `docs/dogfood/2026-11-06-observability.md:44-45`.
- NOT verified: the released 0.2.1 binary's observability surface (stale-deploy arm) — owned by
  OBS-BUNKER-006; this audit only asserts HEAD.
