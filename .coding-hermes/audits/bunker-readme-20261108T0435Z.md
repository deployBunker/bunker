# Bunker README health sweep — re-fire at 60d6795 (tick bunker-readme-2026-11-08-04-02-17)

Tick label: `bunker-readme-2026-11-08-04-02-17`. This is a **re-fire addendum**: the first
execution of this same tick probed `7f46d487c3db1730314ef822c052f990afa38525`, filed
`README-020`, and ended before appending a consolidated audit event. `origin/main` then
advanced twice, so the earlier probe tree is stale for every reader-facing arm and the whole
matrix was re-run at the new tip rather than re-asserting the old one.

## Publication boundary

| read | value |
| --- | --- |
| `git ls-remote --symref https://github.com/deployBunker/bunker.git HEAD` (2026-11-08T04:35:11Z) | `ref: refs/heads/main HEAD`; `60d6795ef6980524d6b5d04da43a2190d38b2fac HEAD` |
| stranger clone `git rev-parse HEAD` (after `git fetch --unshallow origin`) | `60d6795ef6980524d6b5d04da43a2190d38b2fac` |
| canonical checkout `HEAD` / `origin/main` | `60d6795ef6980524d6b5d04da43a2190d38b2fac` (equal) |
| latest tag (`git describe --tags --abbrev=0`) | `v0.2.0` = `149411c168d1359579d948a5ee73d80222356507`, tagged 2026-10-01 |
| `README.md` last commit | `0e7051a6564081f75934a5f61ea307f33055723f`, 2026-11-07T23:32:25-05:00 |

Commits that landed between the first probe and this one: `0e7051a6` (docs: renew flags,
observability push-gaps metric, README paths/iobattery) and `60d6795e` (board: repair torn
README-020 row). The first of these changes documented claims, which is precisely why the
matrix was re-run.

Probe tree: a fresh `git clone --depth 1` of the remote URL plus `git fetch --unshallow`
(stranger path, not a clone of the local checkout). Compare: the prior run's tree
`stranger-20261108T040217Z` was not reused. Scratch `HOME` was created and verified before
any command that resolves config paths.

## Executed command matrix (all at 60d6795)

| # | command | exit | decisive output |
| --- | --- | --- | --- |
| 1 | `make docs-check` | 0 | `docs OK: README/CHANGELOG/docs match v0.2.0 (35 released commands, 1176 commit(s) after the tag)` |
| 2 | `make build` | 0 | built `./bunkerd` and `./bunker` with `Commit=60d6795e` |
| 3 | `go test ./... -short` | 0 | all packages `ok`; no FAIL lines |
| 4 | `./bunker --version` | 0 | `bunker 0.2.1`, `commit: 60d6795e` |
| 5 | `./bunker --help` | 0 | command list now includes `paths` and `iobattery` |
| 6 | `./bunker paths` | 0 | prints `COMPONENT NAME PATH MODE RULE` table plus `Precedence: flag > env > …` line |
| 7 | `./bunker iobattery --target <scratch>/iob-target` | 0 | `{"schema": "bunker.iobattery.v1", …}` with `measurements` array |
| 8 | `HOME=<scratch> BUNKER_HOME=<scratch> sh scripts/install.sh --dry-run` | 0 | `dry-run complete: nothing was downloaded, built or written` |
| 9 | `bash e2e-full-battery.sh --show-plan` | 0 | `PLAN: OK (nothing was changed)` |
| 10 | `bash e2e-full-battery.sh --self-test` | 1 | `SELF-TEST: FAIL (1 check(s) failed)` |
| 11 | `bash regression-tests.sh` | 3 | `✗ connect succeeds …` / `bunker: no servers configured; run 'bunker connect' first` |

Probe-error note (recorded so no later reader files it as a defect): a positional invocation
`./bunker iobattery <dir>` answers `bunker: unknown command "<dir>" for "bunker iobattery"`
(exit 1). The supported form is `--target` (`internal/cli/iobattery.go:30`,
`Use: "iobattery --target <dir>"`), and it works — row 7 above. This was the auditor's arm
error, not a README claim: the README table lists the verb without arguments, consistent with
every other row in that table.

`make docs-check` deliberately grades only the **released** (v0.2.0) command surface — its own
summary says `35 released commands`. It therefore cannot detect HEAD-only drift, which is the
substrate of `README-009`.

## Re-measurement of open rows

Every row below is still open on the board; none was closed by this sweep.

1. **README-007** (key path stated two ways) — REPRODUCED. `README.md:936` and `README.md:975`
   say `~/.bunker/keys/<agent-id>`; `README.md:1440-1441` gives the scp fallback as
   `~/.config/bunker/keys/<agent-id>`. `./bunker paths` at 60d6795 resolves the live location to
   `<scratch>/.config/bunker/keys` with rule `xdg`, i.e. the `:1440` form is the true one and the
   `:936`/`:975` prose is stale.
2. **README-010** (HEAD-only `paths`/`iobattery` missing from the CLI inventory) — **NOT
   REPRODUCED**. `0e7051a6` added `bunker paths` (`README.md:1422`) and `bunker iobattery`
   (`README.md:1425`). Both verbs exist in `./bunker --help` and both ran (matrix rows 5-7).
   Reported for the owning foreman to close; this lane does not close rows.
3. **README-012** (stale `file:line` citations in the exit-codes table) — REPRODUCED. Spot-check
   of 11 cited locators: `internal/cli/destroy.go:94` lands on
   `req.Header().Set("Authorization", …)`, `destroy.go:86` on a comment about a disk-usage
   snapshot cache, `spawn.go:79` on a comment about a 6h TTL window, `exec.go:137` on help prose
   about `--raw`. Only `cmd/bunker/main.go:22` still lands on the intended `run()` error site.
4. **README-013** (two dead in-page anchors) — REPRODUCED. The README still links
   `#the-tls_insecure-knob-2026-09` and `#the-tls-insecure-knob-2026-09`; the only heading that
   exists is `#### The \`tls_insecure\` knob (GAP-141)` at `README.md:586`, slug
   `#the-tls_insecure-knob-gap-141`.
5. **README-016** (`e2e-full-battery.sh --self-test` fails its own no-side-effect check) —
   REPRODUCED, same decisive line as filed:
   `✗ the harness's own scratch dir was not removed by remove_own_state_dir`, then
   `SELF-TEST: FAIL (1 check(s) failed)`, exit 1.
6. **README-017** (`regression-tests.sh` documented without its prerequisites) — REPRODUCED,
   with a different symptom than the filed run: this host answers `bunker: no servers
   configured; run 'bunker connect' first` and three `✗` cells in section 3 (`connect
   succeeds`, numeric agent count, dedicated registration name), exit 3. The suite also
   declines to manage a systemd-owned `bunkerd` (`MainPID 905538`) — the prerequisite the
   README omits is unchanged; the failing cell moved.
7. **README-018** (historical `bunkerd 0.1.4` sample presented as the current capability
   block) — REPRODUCED. `README.md:748` still prints `bunkerd 0.1.4` inside the block the
   README introduces as what "the current installer requires the daemon to prove", while a
   HEAD build reports `bunkerd 0.2.1` (`commit 60d6795e`).
8. **README-020** (generic exit-code claim contradicts `toolchain-check` gate semantics) —
   REPRODUCED, shifted by five lines by `0e7051a6`. `README.md:1447-1450`: "The CLI has no
   per-command exit codes of its own: `0` on success, `1` on any error … plus the two ssh-style
   exceptions below". `README.md:1397-1404` in the same file: "EXIT CODE IS THE GATE: 0 when
   every toolchain the class requires is present, 1 when any is missing … `--report-only`
   makes it a pure report (always exit 0)". The binary agrees with the second:
   `internal/cli/toolchain_check.go:64` repeats `EXIT CODE is the gate`.

## Bookkeeping repaired during this addendum

- **README-020 was gutted by the torn append and its repair.** `60d6795e` restored the row's
  JSON line from the surviving bytes only, leaving `created_at: null`, no
  `acceptance_criteria`, and a `reasoning` consisting of the repair note alone — a pending
  row no foreman could dispatch and no worker could execute. This addendum restored the
  finding text and an acceptance criterion onto that same row id (P4 unchanged; no new row and
  no new id minted), so the prior file's finding is not lost with its bytes.
- The uncommitted `events.jsonl` line for `README-020` (`task_created`, 2026-11-08T04:26:28)
  was this lane's own surviving write from the first execution and is published with the
  rows it describes. `.gitreins/tasks.yaml` carries a foreign lane's `BOARD-GUARD-004` entry
  and was deliberately left unstaged.

## What this addendum does not claim

UNVERIFIED: literal root `bunkerd` start and the live REST/RPC surface; `host-provision
--apply`/`--uninstall` against a real host; hosted-demo credentials; destructive commands
(`destroy`, `reap --apply`); a complete release-asset install (the v0.2.0 asset invocation
returned exit 127 in the first execution and was not re-attempted); `bunker ssh`/`mount`
against a live agent; and every other host-bound or root-required branch of the matrix. The
`make docs-check` release-only scope is recorded as a limitation, not executed away.

ch:trace row=README-007 spec=none:README-claim-correction wave=none:serial-audit test=none:manual-command-matrix doc=README.md#L936 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0435Z-transcript.txt witness=none:local-published-tip-audit verdict=none:open-finding commit=none:open memory=none:exact-key-not-found

ch:trace row=README-010 spec=none:README-claim-correction wave=none:serial-audit test=none:manual-command-matrix doc=README.md#L1422 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0435Z-transcript.txt witness=none:local-published-tip-audit verdict=none:not-reproduced commit=none:no-closure memory=none:exact-key-not-found

ch:trace row=README-012 spec=none:README-claim-correction wave=none:serial-audit test=none:manual-command-matrix doc=README.md#L1452 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0435Z-transcript.txt witness=none:local-published-tip-audit verdict=none:open-finding commit=none:open memory=none:exact-key-not-found

ch:trace row=README-013 spec=none:README-claim-correction wave=none:serial-audit test=none:manual-command-matrix doc=README.md#L553 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0435Z-transcript.txt witness=none:local-published-tip-audit verdict=none:open-finding commit=none:open memory=none:exact-key-not-found

ch:trace row=README-016 spec=none:README-claim-correction wave=none:serial-audit test=e2e-full-battery.sh::--self-test doc=README.md prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0435Z-transcript.txt witness=none:local-published-tip-audit verdict=none:open-finding commit=none:open memory=none:exact-key-not-found

ch:trace row=README-017 spec=none:README-claim-correction wave=none:serial-audit test=regression-tests.sh::section-3 doc=README.md prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0435Z-transcript.txt witness=none:local-published-tip-audit verdict=none:open-finding commit=none:open memory=none:exact-key-not-found

ch:trace row=README-018 spec=none:README-claim-correction wave=none:serial-audit test=none:manual-command-matrix doc=README.md#L748 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0435Z-transcript.txt witness=none:local-published-tip-audit verdict=none:open-finding commit=none:open memory=none:exact-key-not-found

ch:trace row=README-020 spec=none:README-claim-correction wave=none:serial-audit test=none:manual-command-matrix doc=README.md#L1447 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0435Z-transcript.txt witness=none:local-published-tip-audit verdict=none:open-finding commit=none:open memory=none:exact-key-not-found
