# Bunker README health sweep — re-measurement at tip 55c8cbdc (tick bunker-readme-2026-11-08-04-02-17, re-fire)

Boundary of this artifact:

- Published tip: `55c8cbdc112daf4510bb5dbb7df85d82f317af5a` (`refs/heads/main`), resolved with `git ls-remote --symref origin HEAD` at 2026-11-08T05:34:53Z and unchanged (`git fetch` + `git rev-parse origin/main`) before the write.
- Reader-facing probe root: the canonical checkout. It is byte-identical to the published tree for every path this sweep touches — `git show HEAD:README.md | sha256sum` and `sha256sum README.md` both print `6db7fe2565b85614f528864dd80f548596f8f754ea2363ffcf40577640e6b2b1`, and the only dirty paths in the tree are audit artifacts under `.coding-hermes/audits/`.
- Lane stand-in `<home>/.hermes/stand-in/readme/bunker` is gitless and has no README: identity check only, never a probe root.
- Consolidated audit event for this tick label: `1794015699005865993` (2026-11-08T04:41:46), written by the first execution of this tick and **reused, not duplicated** — no second audit event was appended, no id was re-minted for an existing finding.
- Frozen producer transcript (the `evidence=` target for every marker below): `.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt`, sha256 `d27b3811a82ca84015645ceb00afbf352dbc48a96ab27b06ef480d28d82f2106` (digest in the sibling `MANIFEST.txt`; this report is not the evidence).

## Why this tick is a re-fire, and what it adds

A prior execution of the same label died mid-flight after leaving residue (see "Residue" below). The earlier committed pass had already re-measured the open rows at tip `60d6795e` and recorded a matrix. This pass therefore does not recreate the sweep:

1. Re-measures **every open README row** at the moved tip (11 reproduce, 1 does not).
2. Executes the item that pass listed as UNVERIFIED and that carries the most documented claims: **the v0.2.0 release-asset install**. It now completes at rc 0 (checksum-verified binaries, `bunker 0.2.0 / commit 149411c`), so that arm is closed rather than inherited.
3. Uses the downloaded release asset plus a HEAD build as a **two-binary pair** for the release-boundary sentences — the class that flips direction at every release.
4. Files one **new** finding that the earlier passes could not see from README text alone: the shipped CLI help still names the pre-GAP-181 SSH key path (`README-021`).

## Executed matrix (this pass)

| Command | Exit | Decisive output |
| --- | --- | --- |
| `curl -fsSL …/releases/latest/download/install.sh \| HOME=<scratch> sh` | 0 | both binaries checksum-verified against `SHA256SUMS`; installed to `$HOME/.local/bin` fallback; smoke check `bunker --version -> 0.2.0` |
| `sh <asset>/install.sh --help` | 0 | full flag text + exit-code table (README:273 holds) |
| `make docs-check` | 0 | `docs OK: README/CHANGELOG/docs match v0.2.0 (35 released commands, 1183 commit(s) after the tag)` |
| `go test ./... -short` (README:1533) | 0 | all packages ok |
| `go build -o <scratch>/bunker-head ./cmd/bunker` + `--version` | 0 | `bunker 0.2.1 / commit 55c8cbd` |
| `<bin> paths` | 0 | keys dir `<home>/.config/bunker/keys`, rule `xdg` |
| 44-verb `--help` sweep (HEAD) | 0 | `HELP_FAILURES=0`; every verb documented except cobra builtins `completion`/`help` |
| v0.2.0 asset `--help` verb inventory | 0 | 38 verbs; **no** `pull`, `resource-events`, `operator`, `toolchain-check`, `paths`, `iobattery`, `reap` |
| `bash e2e-full-battery.sh --self-test` | 1 | `SELF-TEST: FAIL (1 check(s) failed)` |
| `bash regression-tests.sh --help` | 3 | runs the suite instead of printing help; `--help` not honored |
| `HOME=<scratch> <bunkerd> --config <scratch>/nonroot-config.yaml` (non-root) | 1 | `refusing to start: /var/lib/bunkerd/secrets/jwt_secret … permission denied` (before binding) |
| link/anchor census over README.md | 0 | 196 heading slugs, 15 in-page anchors, **2 dead**, 34 relative links, **0 missing** targets, 4 badges |
| `boardctl -C <repo> validate` | 0 | `RESULT: OK (1002 warning(s))` — identical before and after this sweep's writes |

Scope note on the repo's own gate: `make docs-check` grades the **released** surface only (README↔tag). It passes while the README's "released after v0.2.0" block over-labels 14 verbs the v0.2.0 asset already carries — the doc-drift gate does not check the tag→README direction, which is why `README-008` survives a green gate.

## Findings re-measured (all rows stay open; this lane files, the foreman dispatches)

Reproduced at tip 55c8cbdc (row → decisive evidence):

- **README-007 (P4)** — key path stated two ways. README:936/975 `~/.bunker/keys` vs README:1440-1441 `~/.config/bunker/keys`; `bunker paths` resolves `xdg`. **Sharpened with the release asset**: v0.2.0 has no `paths` verb and its `cp --help` names `~/.bunker/keys`, so the gate is real and README:1437-1441 (addressed to the reader *without* a HEAD build) sits on the wrong side of it. Note appended to the row.
- **README-008 (P4)** — the "Commands released after v0.2.0 … need a build from this checkout" block over-labels 14 verbs: `stop start restart renew homes linger umount guard key agent-tools surface subid-migrate host-provision fs` are all in the v0.2.0 asset. Genuinely HEAD-only: `pull resource-events operator toolchain-check paths iobattery` (+ `reap`).
- **README-009 (P4)** — the currency recipe. `## Unreleased` is no longer empty (55 lines, RELEASE-021) but names **none** of the six HEAD-only verbs; `git describe --tags --abbrev=0` → `v0.2.0` while `internal/version/version.go:25` says `0.2.1`, so a HEAD build reports 0.2.1.
- **README-012 (P4)** — exit-code "Traceable to" citations still point at unrelated lines: `exec.go:280-281` is `}` + `serverName = resolved`; `run.go:231-232` is an error wrap; `destroy.go:94` is the Authorization header; `destroy.go:80-82` is `return 0, ctx.Err()`; `spawn.go:79` is help text. The table itself moved from README:1357-1366 to README:1452-1459.
- **README-013 (P5)** — 2 dead anchors: README:582 `#the-tls-insecure-knob-2026-09`, README:657 `#the-tls_insecure-knob-2026-09`; the header at README:586 slugs to `#the-tls_insecure-knob-gap-141`.
- **README-014 (P4)** — with the README's own config example, a non-root `bunkerd` exits 1 **before binding** (`/var/lib/bunkerd/secrets/jwt_secret … permission denied`), contradicting the "starts and serves read-only endpoints" promise; README never mentions the state/secrets relocation the error message itself recommends (`BUNKER_SECRETS_DIR`, `agent.base_data_dir`).
- **README-015 (P4)** — HEAD `bunker destroy --help` documents `--force` ("Kill the agent user's live processes (SIGTERM, then SIGKILL escalation) and destroy anyway … the home is still archived first"); `grep -n 'kill-first\|--force' README.md` returns nothing.
- **README-016 (P4)** — `bash e2e-full-battery.sh --self-test` exits 1: `SELF-TEST: FAIL (1 check(s) failed)`.
- **README-017 (P4)** — documented `regression-tests.sh` still needs root and `/var/log/bunkerd-regression.log`; `--help` is not honored (the suite runs, exit 3) and section 3 connect cells fail with no servers configured.
- **README-018 (P4)** — README:748-751 still prints `bunkerd 0.1.4 / commit 923c4be` as the capability block while a HEAD build reports `bunkerd 0.2.1`; the surrounding prose calls 0.1.4 a legacy floor, but the block itself carries no historical label.
- **README-020 (P4)** — README:1447 "The CLI has no per-command exit codes of its own … plus the two ssh-style exceptions" vs the `toolchain-check` gate at README:1397-1404 and `<bin> toolchain-check --help` (0 present / 1 missing / `--report-only` = 0). Note: the quoted phrase is line-wrapped at README:1400-1401, so a literal grep for `EXIT CODE IS THE GATE` misses it — grep `-i 'exit code'`.

Not reproduced:

- **README-010 (P5)** — `bunker paths` and `bunker iobattery` are now both in the README command inventory (README:1422, README:1425) and both run at HEAD. The row is still `pending`; this artifact records that its finding is fixed upstream (0e7051a6). No closure is claimed here — closure needs the evidence-run binding a foreman holds, and this lane authored no fix.

## New finding filed

- **README-021 (P4)** — the shipped CLI help names the pre-GAP-181 key path. `cp --help` / `deploy --help` say `--ssh-key … (default: ~/.bunker/keys/<agent-id>)`, `destroy --help` says `--keep-key … (~/.bunker/keys/<id>)`, and `spawn` prints "saved to ~/.bunker/keys/", while `paths` (and `cp.go:561-565` `defaultSSHKeyPath`) resolve `${XDG_CONFIG_HOME:-~/.config}/bunker/keys`; `cp.go:70` in the same file already prints the XDG form. Six help strings across `cp.go:43`, `cp.go:256`, `deploy.go:33`, `deploy.go:241`, `spawn.go:436`, `spawn.go:458` (plus the destroy `--keep-key` string). Filed at P4, not P1: no documented command destroys data — the action is a wrong manual fallback path. Fresh evidence appended as a note on README-007 and the release-boundary arms on README-008 are the same probe set.

## Release-boundary sentences verified with BOTH binaries

The README marks `pull`, `resource-events`, `operator`, `toolchain-check`, `paths`, `iobattery`, `reap` as "requires a build from HEAD". Checked against the checksum-verified v0.2.0 asset: every one of those verbs is **absent** (`bunker: unknown command "pull" for "bunker"`, same for `paths`), and present in the HEAD build. Those caveats are correct; the drift is on the other side of the same block (README-008), where 14 released verbs are labelled unreleased.

## Residue handled (disclosed)

The tree was not clean when this pass started; both paths belong to this lane's own dead execution of the same tick label, not to a foreign writer:

- `M .coding-hermes/audits/bunker-readme-20261108T040217Z.md` — a committed artifact whose header had been rewritten (uncommitted) to claim tip `933235d7` and to point at a weak partial transcript. Restored to the committed bytes (`git checkout --`); the committed report is historical for tip `7f46d48`/`60d6795e` and is left as-is rather than edited.
- `?? .coding-hermes/audits/bunker-readme-20261108T040217Z-transcript.txt` — that execution's raw transcript, whose own text says every documented command block was **not** executed. Preserved and committed beside this report, labelled superseded; it is not cited as evidence by any marker.

Board surfaces were clean when this pass began (`git status --porcelain .coding-hermes/board/` empty), so the writes are authorized: `README-021` created, `README-007` note refreshed. No other lane's state was staged (`.gitreins/` untouched).

## UNVERIFIED

- Root daemon start and the live REST/RPC surface (`bunkerd` as root, `curl` against `:18080`, `/healthz`).
- `host-provision --apply` / `--uninstall` against a real host (root, host-mutating).
- The hosted demo (`78.46.173.180:18080`) — no token issued to this lane.
- Destructive verbs: `destroy` (incl. `--force`/`--purge`), `reap --apply`, `key revoke/rotate`.
- `bunker ssh` / `bunker mount` / `bunker tunnel` against a live agent (no agent, no docker).
- Any `systemd` verb (host service manager).
- The non-root daemon arm was run to its documented *failure* only; it was never brought up, so the "serves read-only endpoints" half of README-014 is asserted from the row's earlier evidence, not re-proved here.
- `make build` (writes `./bunker`/`./bunkerd` into the canonical checkout); the equivalent `go build` of both binaries was run instead, in scratch.

## Trace markers

ch:trace row=README-007 spec=none:README-claim-correction wave=none:serial-refire test=none:manual-command-matrix doc=README.md#L936 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=none:release-asset-and-head-binary-probe-captured-in-evidence verdict=none:open-finding commit=none:open memory=/project/bunker/readme-sweep-2026-11-08

ch:trace row=README-008 spec=none:release-boundary-claim wave=none:serial-refire test=none:two-binary-release-probe doc=README.md#L1346 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=release:v0.2.0 verdict=none:open-finding commit=none:open memory=/project/bunker/readme-sweep-2026-11-08

ch:trace row=README-009 spec=none:release-currency-recipe wave=none:serial-refire test=none:manual-command-matrix doc=README.md#L1346 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=none:changelog-and-version-probe-captured-in-evidence verdict=none:open-finding commit=none:open memory=/project/bunker/readme-sweep-2026-11-08

ch:trace row=README-010 spec=none:command-coverage wave=none:serial-refire test=none:manual-command-matrix doc=README.md#L1422 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=none:not-reproduced-at-55c8cbdc verdict=none:not-reproduced commit=none:no-closure memory=/project/bunker/readme-sweep-2026-11-08

ch:trace row=README-012 spec=none:citation-drift wave=none:serial-refire test=none:line-read-probe doc=README.md#L1452-L1459 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=none:line-citations-are-in-repo verdict=none:open-finding commit=none:open memory=/project/bunker/readme-sweep-2026-11-08

ch:trace row=README-013 spec=none:anchor-drift wave=none:serial-refire test=none:anchor-slug-census doc=README.md#L582 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=none:github-render-check-not-available verdict=none:open-finding commit=none:open memory=/project/bunker/readme-sweep-2026-11-08

ch:trace row=README-014 spec=none:documented-path-broken wave=none:serial-refire test=none:non-root-daemon-probe doc=README.md#L233-L237 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=none:non-root-daemon-exit-captured-in-evidence verdict=none:open-finding commit=none:open memory=/project/bunker/readme-sweep-2026-11-08

ch:trace row=README-015 spec=none:missing-reference wave=none:serial-refire test=none:help-vs-readme-probe doc=README.md#L1445 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=none:shipped-help-captured-in-evidence verdict=none:open-finding commit=none:open memory=/project/bunker/readme-sweep-2026-11-08

ch:trace row=README-016 spec=none:documented-command-contract wave=none:serial-refire test=none:documented-command-run doc=README.md#L1466-L1471 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=none:self-test-output-captured-in-evidence verdict=none:open-finding commit=none:open memory=/project/bunker/readme-sweep-2026-11-08

ch:trace row=README-017 spec=none:documented-command-contract wave=none:serial-refire test=none:documented-command-run doc=README.md#L1557 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=none:root-log-prerequisite-host-bound verdict=none:open-finding commit=none:open memory=/project/bunker/readme-sweep-2026-11-08

ch:trace row=README-018 spec=none:freshness-sample wave=none:serial-refire test=none:version-command-probe doc=README.md#L748-L754 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=none:local-build-output-only verdict=none:open-finding commit=none:open memory=/project/bunker/readme-sweep-2026-11-08

ch:trace row=README-020 spec=none:internal-contradiction wave=none:serial-refire test=none:help-vs-readme-probe doc=README.md#L1447 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=none:binary-help-captured-in-evidence verdict=none:open-finding commit=none:open memory=/project/bunker/readme-sweep-2026-11-08

ch:trace row=README-021 spec=none:README-claim-correction wave=none:serial-refire test=none:help-vs-resolver-probe doc=internal/cli/cp.go#L256 prompt=~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md evidence=.coding-hermes/audits/bunker-readme-20261108T0534Z-transcript.txt witness=none:help-text-vs-resolver-probe-captured-in-evidence verdict=none:open-finding commit=none:open memory=/project/bunker/readme-sweep-2026-11-08
