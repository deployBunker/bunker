Bunker README health sweep — re-measurement at a moved tip
Tick: bunker-readme-2026-11-08-04-02-17 (re-fire). Consolidated audit event for this tick: 1794015699005865993.
Lane skill: ~/.hermes/skills/coding-hermes/coding-hermes-readme/SKILL.md
Lane stand-in: <home>/.hermes/stand-in/readme/bunker (gitless, no README — identity check only, never a probe root)
Canonical repository: <home>/deployBunker/bunker (git rev-parse --show-toplevel)
Probe root: the canonical checkout; reader-facing tree byte-identical to the published tip (README sha256 equal to HEAD blob, board surfaces clean)
Scratch/probe prefix: <scratch>/bunker-readme-20261108T0534

Paths are redacted to <home>/<scratch> and one operator-config line is redacted; the raw transcript is kept in the lane stand-in.

COMMAND: git -C <home>/deployBunker/bunker ls-remote --symref origin HEAD
EXIT_STATUS=0
OUTPUT:
ref: refs/heads/main	HEAD
55c8cbdc112daf4510bb5dbb7df85d82f317af5a	HEAD
(tip re-read at 2026-11-08T05:34:53Z; HEAD == origin/main == 55c8cbdc112daf4510bb5dbb7df85d82f317af5a)

COMMAND: git status --porcelain .coding-hermes/board/
EXIT_STATUS=0
OUTPUT: (empty — no foreign writer on the board surfaces)

COMMAND: git status --porcelain
EXIT_STATUS=0
OUTPUT:
 M .coding-hermes/audits/bunker-readme-20261108T040217Z.md
?? .coding-hermes/audits/bunker-readme-20261108T040217Z-transcript.txt
(dead-execution residue of this same tick label; see the report's residue section)

COMMAND: boardctl -C <home>/deployBunker/bunker validate
EXIT_STATUS=0
OUTPUT: RESULT: OK (1002 warning(s))
(baseline census before any write; warnings are pre-existing legacy board-shape/event warnings)

COMMAND: git show HEAD:README.md | sha256sum ; sha256sum README.md
EXIT_STATUS=0
OUTPUT:
6db7fe2565b85614f528864dd80f548596f8f754ea2363ffcf40577640e6b2b1  -
6db7fe2565b85614f528864dd80f548596f8f754ea2363ffcf40577640e6b2b1  README.md

COMMAND: git log -1 --format='%h %ad %s' --date=short -- README.md
EXIT_STATUS=0
OUTPUT: 0e7051a6 2026-11-07 docs: renew flags, observability push-gaps metric, README paths/iobattery

COMMAND: git describe --tags --abbrev=0
EXIT_STATUS=0
OUTPUT: v0.2.0

COMMAND: curl -fsSL --max-time 25 https://api.github.com/repos/deployBunker/bunker/releases/latest | jq -c '{tag_name,published_at,assets:[.assets[]|{name,size}]}'
EXIT_STATUS=0
OUTPUT:
{"tag_name":"v0.2.0","published_at":"2026-10-02T12:01:18Z","assets":[{"name":"bunker-linux-amd64","size":19009020},{"name":"bunker-linux-arm64","size":17730053},{"name":"bunkerd-linux-amd64","size":24237115},{"name":"bunkerd-linux-arm64","size":22482842},{"name":"install.sh","size":33108},{"name":"SHA256SUMS","size":419}]}

COMMAND: test -w /usr/local/bin
EXIT_STATUS=1
OUTPUT: NOT_WRITABLE (so the documented fallback to $HOME/.local/bin is the arm a stranger sees on a stock host)

COMMAND: curl -fsSL https://github.com/deployBunker/bunker/releases/latest/download/install.sh | HOME=<scratch>/bunker-readme-20261108T0534/home sh
EXIT_STATUS=0
OUTPUT:
install.sh: installing bunker latest for linux/amd64
install.sh: note: /usr/local/bin is not writable by <user> — falling back to <scratch>/bunker-readme-20261108T0534/home/.local/bin (override with --dir)
install.sh: target directory: <scratch>/bunker-readme-20261108T0534/home/.local/bin
install.sh: downloaded https://github.com/deployBunker/bunker/releases/latest/download/SHA256SUMS
install.sh: downloaded https://github.com/deployBunker/bunker/releases/latest/download/bunker-linux-amd64
install.sh: downloaded https://github.com/deployBunker/bunker/releases/latest/download/bunkerd-linux-amd64
install.sh: verified bunker-linux-amd64 (sha256 982c57fcf2290168c82b24b89e32cf3477c9ecf33c9c66dc2ab401ee3e278088)
install.sh: verified bunkerd-linux-amd64 (sha256 3fb29c67146fa5b365f72dd3434eff740721ff836f2157b8be4ab3ddad506a1f)
install.sh: installed <scratch>/.../home/.local/bin/bunker
install.sh: installed <scratch>/.../home/.local/bin/bunkerd
bunker 0.2.0
  commit:     149411c
  built:      2026-10-02T12:00:36Z
  go version: go1.26.5
  platform:   linux/amd64
install.sh: smoke check OK: .../bunker --version -> 0.2.0
install.sh: warning: .../.local/bin is not on your PATH — add it, e.g. export PATH="...:$PATH"
install.sh: done: bunker and bunkerd are installed in .../.local/bin
(The install arm the 0435Z pass recorded as UNVERIFIED — "release asset invocation returned exit 127" — completes at rc 0 with a checksum-verified, working binary at the documented tag.)

COMMAND: sh <scratch>/bunker-readme-20261108T0534/assets/install.sh --help
EXIT_STATUS=0
OUTPUT: full flag text printed (--version/--from-dir/--build/--dir/--dry-run/--sshfs, exit codes 0/42/1, examples) — README.md:273 claim "sh install.sh --help prints the full text" holds.

COMMAND: go build -o <scratch>/bunker-readme-20261108T0534/bunker-head ./cmd/bunker
EXIT_STATUS=0
OUTPUT: (no output; 0m00.965s)

COMMAND: <scratch>/bunker-readme-20261108T0534/bunker-head --version
EXIT_STATUS=0
OUTPUT:
bunker 0.2.1
  commit:     55c8cbd
  built:      2026-11-08T05:32:21Z
  go version: go1.26.10
  platform:   linux/amd64

COMMAND: <scratch>/bunker-readme-20261108T0534/bunker-head paths
EXIT_STATUS=0
OUTPUT (decisive rows):
cli        agent SSH keys directory (keys/<agent-id>, ... <home>/.config/bunker/keys   0700   xdg
bunkerd    daemon config file                         /etc/bunkerd/config.yaml      0600   system
Precedence: flag > env > per-user config dir (${XDG_CONFIG_HOME:-~/.config}/bunker) > legacy ~/.bunker (read alias) > system defaults (/etc/bunkerd, /var/log/bunkerd, /var/lib/bunkerd).

COMMAND: grep -n '\.bunker/keys\|\.config/bunker/keys\|keys/<agent-id>' README.md
EXIT_STATUS=0
OUTPUT:
936:# Tear down (also deletes the client-local SSH key ~/.bunker/keys/abc12345)
975:> key to `~/.bunker/keys/<agent-id>` (fetched from the daemon via the
1440:for the agent at `~/.config/bunker/keys/<agent-id>`:
1441:`scp -i ~/.config/bunker/keys/<agent-id> bunker-<agent-id>@<host>:<remote-path> <local-dir>`.

COMMAND: <release v0.2.0 asset> paths ; <release v0.2.0 asset> pull
EXIT_STATUS=0 (message captured)
OUTPUT:
bunker: unknown command "paths" for "bunker"
bunker: unknown command "pull" for "bunker"
(the v0.2.0 asset has neither verb — so a v0.2.0 reader cannot run `bunker paths` to learn the key path, and README.md:1437-1441's "Without that build" arm describes a v0.2.0 reader)

COMMAND: <scratch>/.../bunker-head --help | grep -n pull
EXIT_STATUS=0
OUTPUT: 48:  pull            Copy a file from an agent's environment via SCP

COMMAND: <scratch>/.../bunker-head --help  (verb inventory) and per-verb `--help` sweep
EXIT_STATUS=0
OUTPUT: 44 top-level verbs: agent-tools audit completion connect cp deploy destroy env exec fs guard heartbeat help homes host-provision info iobattery key linger list metrics mount operator paths pull reap registry renew resource-events restart run spawn ssh start status stop subid-migrate surface systemd toolchain-check tunnel umount use version
HELP_FAILURES=0 (every verb's --help exits 0)
README documents every verb except the cobra builtins `completion` and `help` — not a finding.

COMMAND: make docs-check
EXIT_STATUS=0
OUTPUT:
go run ./cmd/docs-drift
docs OK: README/CHANGELOG/docs match v0.2.0 (35 released commands, 1183 commit(s) after the tag)
  released: [agent-tools audit connect cp deploy destroy env exec fs guard heartbeat homes host-provision info key linger list metrics mount registry renew restart run spawn ssh start status stop subid-migrate surface systemd tunnel umount use version]
(scope note: this gate grades the RELEASED surface only; the HEAD-only verbs are covered by the verb inventory above)

COMMAND: go test ./... -short
EXIT_STATUS=0
OUTPUT: all packages ok (internal/..., probes/webdav-battery, proto/bunker/v1/bunkerv1connect); [no test files] for probes/bfs025-truncated-read, probes/bfs052-fuse-caps, probes/davserve, probes/h3client, proto/bunker/v1

COMMAND: bash e2e-full-battery.sh --self-test
EXIT_STATUS=1
OUTPUT (decisive tail):
  ✓ teardown order is destroys (1386) -> unset binding (1702) -> remove state dir (1787) — PINNED
  ...
SELF-TEST: FAIL (1 check(s) failed)
(the failing cell is the harness's own scratch-dir removal check; full list of passing cells elided)

COMMAND: bash regression-tests.sh --help
EXIT_STATUS=3
OUTPUT (decisive tail):
── 3. Connect ──
  ✗ connect succeeds (echo "$OUT" | grep -q "Connected")
  ✗ connect reports a numeric agent count ...
  ✗ registered under the dedicated name (no fleet clobber) ...
  ✗ runner residue baseline: could not read a numeric agent count from 'bunker status' (first line: bunker: no servers configured; run 'bunker connect' first)
=== Cleanup ===
  operator config server names BEFORE: <15 operator-configured host names redacted — private fleet hosts>
  operator config server names AFTER:  <same 15 names — no mutation>
  ⚠ NOT killing bunkerd processes: systemd manages the running bunkerd (MainPID <pid>) — this suite only manages what it creates
RT_HELP_EXIT=3
(`--help` is not honored: the suite runs instead of printing help, and a non-root operator cannot satisfy its /var/log prerequisite)

COMMAND: python3 link_census.py  (script kept in <scratch>/bunker-readme-20261108T0534/link_census.py; slugs headings GitHub-style, then matches every ](#...) and every relative target)
EXIT_STATUS=0
OUTPUT:
HEADING_SLUGS=196
IN_PAGE_ANCHORS=15 UNIQUE=11
DEAD_ANCHORS=2
  DEAD README.md:582 #the-tls-insecure-knob-2026-09
  DEAD README.md:657 #the-tls_insecure-knob-2026-09
RELATIVE_LINKS=34 UNIQUE_TARGETS=22
MISSING_TARGETS=0
BADGES: README.md:7 Go 1.26+, :8 License Apache 2.0, :9 gRPC connect-go, :10 Docker rootless

COMMAND: head -3 LICENSE ; grep -n -i 'INSERT\|YOUR NAME\|\[TODO\|<contact' CONTRIBUTING.md CODE_OF_CONDUCT.md SECURITY.md
EXIT_STATUS=0 / non-zero (files absent)
OUTPUT:
                                 Apache License
                           Version 2.0, January 2004
(no governance docs are linked or claimed, so nothing to template-check; LICENSE/NOTICE/CLA.md all exist)

COMMAND: sed -n '1452,1459p' README.md ; sed -n '280,281p' internal/cli/exec.go ; sed -n '231,232p' internal/cli/run.go ; sed -n '94p' internal/cli/destroy.go ; sed -n '80,82p' internal/cli/destroy.go ; sed -n '79p' internal/cli/spawn.go
EXIT_STATUS=0
OUTPUT (cited line vs what it actually contains):
README:1456 cites internal/cli/exec.go:280-281  -> "\t\t\t}" / "\t\t\tserverName = resolved"
README:1457 cites internal/cli/run.go:231-232   -> "if err != nil {" / "return decorateDeadline(\"run\", ...)"
README:1458 cites internal/cli/destroy.go:94    -> "req.Header().Set(\"Authorization\", \"Bearer \"+token)"
README:1459 cites internal/cli/destroy.go:80-82 -> "return 0, ctx.Err()" / "}" / "}"
README:1459 cites internal/cli/destroy.go:90-92 -> (destroyHomeSizeProbe block)
README:1456/1459 cite spawn.go:79 / spawn.go:107 -> help text about --ttl; "}"
(exit codes table still cites unrelated lines; table moved from README:1357-1366 to README:1452-1459)

COMMAND: sed -n '1397,1404p' README.md ; grep -n 'no per-command exit codes' README.md
EXIT_STATUS=0
OUTPUT:
1397:bunker toolchain-check  Report a DECLARED build-toolchain CLASS present or
1400:                   (go + gcc + make + node + cargo — the default). EXIT
1401:                   CODE IS THE GATE: 0 when every toolchain the class
1402:                   requires is present, 1 when any is missing ...
1404:                   --report-only makes it a pure report (always exit 0).
1447:The CLI has no per-command exit codes of its own: `0` on success, `1` on any
(the quoted phrase is line-wrapped, so a literal grep for "EXIT CODE IS THE GATE" misses it; grep -i 'exit code' matches)

COMMAND: <scratch>/.../bunker-head toolchain-check --help | grep -i -c 'exit code'
EXIT_STATUS=0
OUTPUT: 1 (the shipped binary documents the same 0/1/--report-only gate)

────────────────────────────────────────────────────────────────────────────
PART 2 — remaining open rows re-measured, plus one new in-binary finding
────────────────────────────────────────────────────────────────────────────

COMMAND: <scratch>/bunker-readme-20261108T0534/home/.local/bin/bunker --help   (v0.2.0 RELEASE ASSET verb inventory)
EXIT_STATUS=0
OUTPUT (Available Commands): agent-tools audit completion connect cp deploy destroy env exec fs guard heartbeat help homes host-provision info key linger list metrics mount registry renew restart run spawn ssh start status stop subid-migrate surface systemd tunnel umount use version
(a 38-verb inventory; NO pull, resource-events, operator, toolchain-check, paths, iobattery, reap)

COMMAND: for v in stop start restart renew homes linger umount guard key agent-tools surface subid-migrate host-provision fs pull resource-events operator toolchain-check paths iobattery; do <release v0.2.0 asset> --help | grep -qE "^  $v " && echo IN || echo ABSENT; done
EXIT_STATUS=0
OUTPUT:
IN_v0.2.0: stop, start, restart, renew, homes, linger, umount, guard, key, agent-tools, surface, subid-migrate, host-provision, fs   (14)
ABSENT_v0.2.0: pull, resource-events, operator, toolchain-check, paths, iobattery   (6)
REAP: not present in the v0.2.0 inventory either — genuinely post-release, as the README says.
(The README's "Commands released after v0.2.0 … need a build from this checkout" block therefore over-labels 14 verbs.)

COMMAND: head -8 CHANGELOG.md ; git describe --tags --abbrev=0 ; grep -n 'Version = ' internal/version/version.go
EXIT_STATUS=0
OUTPUT:
# Changelog
## Unreleased
- **The local release gate no longer forbids the release order it documents (RELEASE-021).** …
(55 lines under Unreleased)
v0.2.0
internal/version/version.go:25:	Version = "0.2.1"

COMMAND: awk '/^## Unreleased/,/^## 0\.2\.1/' CHANGELOG.md | grep -cE 'pull|resource-events|operator|toolchain-check|paths|iobattery'
EXIT_STATUS=1
OUTPUT: 0
(The Unreleased section is no longer EMPTY — it carries RELEASE-021 text — but it names none of the six HEAD-only verbs, so the README's currency recipe still cannot tell a reader that pull/resource-events/operator/toolchain-check/paths/iobattery exist on main; and the version is still an untagged 0.2.1 while the newest tag is v0.2.0.)

COMMAND: cp config.example.yaml <scratch>/nonroot-config.yaml ; sed -i 's/127.0.0.1:9090/127.0.0.1:19391/' … ; HOME=<scratch>/nonroot-home timeout 20 <scratch>/bunkerd-head --config <scratch>/nonroot-config.yaml
EXIT_STATUS=1
OUTPUT:
bunkerd: refusing to start: refusing to start: /var/lib/bunkerd/secrets/jwt_secret exists but cannot be read (auth.jwt_secret: read secret file "/var/lib/bunkerd/secrets/jwt_secret": open /var/lib/bunkerd/secrets/jwt_secret: permission denied) — if this file is yours (or you are root), fix its permissions (mode 0600, owner-readable) or remove it to have a new secret generated; if it belongs to another user (typically the root daemon's state under /var/lib/bunkerd), do not touch it — run this daemon against a secrets location you own instead: set BUNKER_SECRETS_DIR=<dir> owned by this user, or set agent.base_data_dir (or agent.registry.path) in the config so the secret resolves inside your own state tree
BUNKERD_EXIT=1
(A non-root daemon given the README's own config example exits 1 BEFORE binding. The message names the relocation the README never mentions, and README's own step 1 for that config — `cp config.example.yaml /etc/bunkerd/config.yaml` — needs root anyway.)

COMMAND: <scratch>/bunker-head destroy --help | grep -n -B2 -A6 '\-\-force' ; grep -n 'kill-first\|destroy --force\|--force' README.md
EXIT_STATUS=0 / 1
OUTPUT:
55:      --force              Kill the agent user's live processes (SIGTERM, then SIGKILL escalation) and destroy anyway — the kill-first exit hatch for a live_processes refusal; the home is still archived first
57:      --keep-key           Keep the local SSH key (~/.bunker/keys/<id>) after destroy (key rotation)
(README grep: no match — README never mentions --force or the kill-first contract)

COMMAND: <scratch>/bunker-head paths ; <scratch>/bunker-head cp --help | grep -n 'ssh-key' ; <scratch>/bunker-head deploy --help | grep -n 'ssh-key' ; sed -n '560,566p' internal/cli/cp.go ; grep -n '\.bunker/keys' internal/cli/cp.go internal/cli/deploy.go
EXIT_STATUS=0
OUTPUT:
paths: cli  agent SSH keys directory (keys/<agent-id>, ...)  <home>/.config/bunker/keys  0700  xdg
cp --help:  --ssh-key string  SSH private key path (default: ~/.bunker/keys/<agent-id>)
deploy --help:  --ssh-key string  SSH private key path (default: ~/.bunker/keys/<agent-id>)
cp.go:561-565 func defaultSSHKeyPath(agentID) { cfgDir, _ := configFilePath(); return filepath.Join(filepath.Dir(cfgDir), "keys", agentID) }
cp.go:43   docs the key as "~/.bunker/keys/<agent-id>"
cp.go:70   (same command's pull remedy) "scp -i ~/.config/bunker/keys/<agent-id> …"
cp.go:256  flag help "default: ~/.bunker/keys/<agent-id>"
deploy.go:33 / deploy.go:241  same two shapes
spawn.go:436 "SSH Key: (saved to ~/.bunker/keys/)" ; spawn.go:458 "(not fetched — expected at ~/.bunker/keys/)"
(resolved default is <home>/.config/bunker/keys; the shipped help names the pre-GAP-181 legacy path in three files and contradicts itself inside cp.go)

COMMAND: jq -r 'select((.title|tostring)|test("help text|--ssh-key|cp --help|flag default"))' <board>/tasks.jsonl | wc -l
EXIT_STATUS=0
OUTPUT: existing hits are DOGFOOD-013 (audit help, complete), DF-BUNKER-84 (cp argument contract, complete), DF-BUNKER-46 (key rotate, complete), DOC-35/DOC-37/DOC-051 (config/mount/exec surfaces) — none covers the SSH-key default path in the shipped help.

COMMAND: grep -c 'paths\|iobattery' README.md ; <scratch>/bunker-head paths >/dev/null ; <scratch>/bunker-head iobattery --help >/dev/null
EXIT_STATUS=0
OUTPUT: README.md:1422 documents `bunker paths`, README.md:1425 documents `bunker iobattery`; both verbs run — README-010's finding (both verbs undocumented) no longer reproduces.
