TICK: bunker-readme-2026-11-22-05-50-52
REPO: /home/kara/.hermes/stand-in/readme/bunker/wt-20261122
HEAD: da5f335888e9a6ed14400bb2722a73468dd6b448
ORIGIN_MAIN: da5f335888e9a6ed14400bb2722a73468dd6b448
RUN_AT_UTC: 2026-11-22T06:11:07Z
README_SHA256: 0ff354ff41b776a9919c87a40acb1eab0bf3b9a4f1dea169fc3a7faa4f8af812

================ ARM 1: freshness against release boundary ================

### CMD: git tag --sort=-creatordate
v0.3.0
v0.2.0
v0.1.4
v0.1.3
v0.1.2
v0.1.1
v0.1.0
### RC: 0

### CMD: git log -1 --format=%h_%cI_%s -- README.md
b7286e55_2026-11-15T19:39:49-05:00_feat(cli): report agent tools-readiness and document the default spawn inventory
### RC: 0

### CMD: sed -n 1,6p CHANGELOG.md
# Changelog

## Unreleased

## 0.4.0 (2026-11-16)
### RC: 0

### CMD: git describe --tags --abbrev=0
v0.3.0
### RC: 0

================ ARM 2: repo docs gate ================

### CMD: make docs-check
go run ./cmd/docs-drift
docs OK: README/CHANGELOG/docs match v0.3.0 (42 released commands, 232 commit(s) after the tag)
  released: [agent-tools audit connect cp deploy destroy env exec fs guard heartbeat homes host-provision info iobattery key linger list metrics mount operator paths pull reap registry renew resource-events restart run spawn ssh start status stop subid-migrate surface systemd toolchain-check tunnel umount use version]
### RC: 0

================ ARM 3: in-page anchors ================

### CMD: python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/check-md-anchors.py README.md
BROKEN README.md:582: #the-tls-insecure-knob-2026-09
BROKEN README.md:657: #the-tls_insecure-knob-2026-09
anchors_checked=16 broken=2
### RC: 1

================ ARM 4: structure census ================

### CMD: python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/readme-structure-census.py README.md
file=README.md bytes=96934 lines=1710

== H2 vs Contents ==
h2_total=12 contents_bullets=0
h2_without_toc=12 [(14, 'What is Bunker?'), (36, 'Features'), (92, 'Quick Start'), (1068, 'Architecture'), (1126, 'Resource Limits'), (1212, 'Can one agent see another? (security model)'), (1232, 'Agent isolation (`/tmp` and cross-agent exchange)'), (1345, 'CLI Commands'), (1490, 'CLI config & path overrides (DF-BUNKER-16)'), (1529, 'Tech Stack'), (1544, 'Development'), (1700, 'License')]
toc_entries_without_heading=[]
WARN every heading reports NO-TOC -> treat this census as a parser artifact, not a finding

== documented commands ==
table_rows=19 distinct=14
documented_more_than_once={'bunker': [1241, 1478, 1479, 1480, 1481, 1482]}
### RC: 0

### CMD: grep -n ^#\{1,4\}  README.md
1:# Bunker — Multi-Agent Coding Platform
14:## What is Bunker?
36:## Features
92:## Quick Start
94:### Run it locally (recommended first path)
106:# 1. Get the source and build both binaries (see Install: make, or the
107:#    make-free go build pair if this host has no make)
112:# 2. Configure. The daemon's default config path is /etc/bunkerd/config.yaml
113:#    (cmd/bunkerd); set auth.token to a value you choose, and pass the same
114:#    value to the CLI. Extra keys fall back to the daemon defaults
115:#    (config.example.yaml / Configure below). For a token-free local daemon
116:#    set auth.enabled: false instead — the daemon then logs a loud
117:#    WARNING: AUTH DISABLED line and accepts unauthenticated requests.
119:#    tls.self_signed: true below makes the daemon generate its own certificate
120:#    on first start, so the CLI never has to disable verification.
137:# 3. Start the daemon as root (spawn needs root). Default ports: REST :8080,
138:#    gRPC :9090 — see the non-default-ports note below if they are taken.
141:# 4. In another terminal, point the CLI at it and check it is up. The first
142:#    connect PRINTS the certificate fingerprint and pins it; every later
143:#    command verifies against that pin (see TLS (trust on first use) below).
147:# 5. First agent: spawn, run a command inside it, tear it down
148:#    (--server is required on mutating commands — see the binding note below)
153:# 6. REST probe (optional): the daemon also speaks JSON over HTTPS. /healthz is
154:#    unauthenticated by design; everything else needs the bearer token.
155:#    The -k flag is needed because the daemon uses a self-signed certificate
156:#    (tls.self_signed: true above); for production, use --cacert with your CA.
161:# Auth ordering on an RPC path is fixed, and it matters to a probe: a non-POST
162:# method returns 405, a POST whose Content-Type is neither application/json nor
163:# application/proto returns 415, and only then does auth run — an
164:# unauthenticated POST receives 401. The 405 and 415 legs stop before the auth
165:# interceptor, so they leave no audit record and no deny-sink entry: a probe
166:# rejected on method or media type is invisible in the audit trail.
207:### Or use the hosted demo (optional)
219:# Install the CLI — one command, prebuilt (see Install):
220:#   curl -fsSL https://github.com/deployBunker/bunker/releases/latest/download/install.sh | sh
221:# …or, with a Go toolchain:
222:#   go install github.com/deployBunker/bunker/cmd/bunker@latest
224:# 1. Request a demo token from the maintainers (request-access only, see above)
225:# 2. Connect with your provisioned token:
233:### Prerequisites
254:### Install
273:# Installer flags — `sh install.sh --help` prints the full text
287:# Builds ./bunkerd and ./bunker with version info baked in (ldflags)
291:#### Installing Go on a stock Debian/Ubuntu host
298:# 1. The tarball for this host: linux-amd64 (use linux-arm64 on arm64).
301:# 2. Extract under /usr/local, NOT under $HOME: a tarball unpacked into $HOME
302:#    makes GOPATH == GOROOT and every go command then prints
303:#    "warning: both GOPATH and GOROOT are the same directory".
307:# 3. Put it on PATH for this shell and for future logins.
360:### Configure
366:# edit /etc/bunkerd/config.yaml — set auth.token before first start
414:#### Control-plane secrets (GAP-129 / SEC-14)
428:# Root-only secrets location (dir 0700, file 0600). This default is the
429:# one the daemon uses for generated secrets; override with BUNKER_SECRETS_DIR.
434:# Point the daemon at it — nothing secret is written to the config file.
436:#   Environment="BUNKER_AUTH_TOKEN_FILE=/home/you/.config/bunkerd/secrets/token"
459:#### Safety presets (GAP-116/117)
472:# Per spawn (highest precedence)
475:# Daemon-wide, via the environment
476:#   Environment=BUNKERD_SAFETY_PRESET=hardened   (systemd unit)
478:# Daemon-wide, via the config file
479:#   safety:
480:#     preset: hardened
492:#### Operators and RBAC (GAP-130)
500:# All `bunker operator` verbs are part of the released CLI.
502:# An operator for a teammate: agent lifecycle, nothing else.
504:#   operator kelly created (role=operator)
505:#   <token>
506:#   # The token above is shown EXACTLY ONCE — the daemon stores only its SHA-256 hash.
508:# A machine identity for one agent: name IS the agent id.
511:# List and remove.
536:#### TLS: the secure path is the easy path (GAP-127)
544:# First connect: the certificate is observed over a REAL TLS handshake, its
545:# sha256 fingerprint is printed loudly, and it is stored on the server entry.
547:#   ================= TRUST ON FIRST USE ================
548:#   You are about to trust the certificate presented by https://bunker-host:9090 …
549:#     fingerprint      sha256:1f9c…a3
550:#   …
552:# Every later command — status, list, spawn, exec, destroy — verifies the leaf
553:# against that pin. No command ever needs --tls-insecure again.
586:#### The `tls_insecure` knob (GAP-141)
596:# Refused: the knob alone is not a decision.
598:#  Error: refusing to skip TLS verification for "…": tls_insecure requires an
599:#  explicit acknowledgement.
601:# Honored: acknowledged in the environment, warned about on stderr, marked in
602:# the daemon's audit trail.
604:#  ============== INSECURE TLS SESSION ==============
605:#  Certificate verification is DISABLED for …
606:#  every RPC in this session is marked [TLS-UNVERIFIED] in the daemon's audit trail.
718:### Provision host isolation before spawning
727:# Inspect the plan first; no host changes without --apply.
729:# After reviewing the plan, install the host-side boundary.
731:# Inspect the resulting host configuration (add --json for automation).
776:### Run the daemon
842:# teardown: bunker systemd uninstall
845:#### Daemon process contract
862:### Use the CLI
865:# Connect to a server. First connect to a self-signed daemon: the certificate
866:# fingerprint is printed and pinned, and every later command verifies against it.
869:# (Plain-HTTP loopback is still the dev case, and needs no trust decision.)
870:#   bunker connect http://127.0.0.1:8080 --token your-master-token-here
872:# Create an agent with 2 CPUs and 4 GB RAM
873:# (mutating commands need --server <alias> or BUNKER_SESSION_TARGET — see the
874:# binding note under Run it locally; the same alias you registered with connect)
876:# NOTE: the FIRST spawn on a host is slow. It installs rootless Docker into the
877:# agent's home (a ~93 MB download) and takes 60-90s+ before dockerd is ready.
878:# The CLI prints "Creating agent..." and waits under a 300s deadline, so a
879:# multi-minute first spawn is expected, not a hang. Later spawns reuse the
880:# server's cached tooling and return in ~10s.
882:# Create an agent with a customized image (GAP-064): package-add spec
892:# Rejected specs (curl|sh, base-image swaps, unknown fields, ...) fail fast
893:# with invalid_argument and build nothing. Identical specs share one cached
894:# build per agent.
896:# A spec is a package-ADD, never an image replacement: the built image keeps
897:# the stock agent userland (git, the docker client, python3, make, jq,
898:# ca-certificates, and the Go toolchain since DF-BUNKER-52) — installed from
899:# the same apt archive as the base — and adds the spec's packages on top
900:# (DF-BUNKER-80).
902:# Delivered tool set for the remote-editing verbs (TOOLS-B2): a fresh stock
903:# agent has git and jq but NOT rg or a language server (GAP-092 measurement),
904:# so search has no working mode and `toolsd lsp check` has nothing to serve.
905:# The canonical remediation spec below delivers both through their package
906:# managers (apt pins + verifies ripgrep; go installs gopls onto the agent's
907:# PATH) — it is what `bunker agent-tools` prints when a probe finds them
908:# missing:
909:#   {"packages":[{"manager":"apt","packages":["ripgrep"]},{"manager":"go","packages":["golang.org/x/tools/gopls@latest"]}]}
911:# What an agent ends up with, per delivery path:
912:#   - plain spawn (no flags):     whatever the HOST's agent userland has.
913:#                                 Measured on bunker-las-03 (2026-11-12) inside
914:#                                 a plain-spawned agent: git, node, npm, python3,
915:#                                 pip3, docker present; rg, toolsd, go, make,
916:#                                 gcc and jq ABSENT. spawn installs nothing, so
917:#                                 this inventory is host-dependent — check it
918:#                                 with `bunker info <id>` ("Spawn Tools") or
919:#                                 probe it with `bunker agent-tools <id>`.
920:#   - --tools-at-spawn:           toolsd, rg, gopls (rg/gopls via an image that
921:#                                 keeps the stock layer + adds them, toolsd from
922:#                                 a local artifact or its registry stage); the
923:#                                 delivery is verified by re-probing, and a
924:#                                 failed delivery fails the spawn loudly.
925:#   - vendored copy on install:   toolsd            (bunker agent-tools --install)
926:#   - registry via install:       rg (apt archive), gopls (Go module proxy)
927:#                                 — same command, runtime delivery (DF-BUNKER-57)
928:#   - package-add (this spec):    rg (ripgrep), gopls (Go language server)
929:#                                 inside a built image. The IMAGE's stock apt
930:#                                 layer is git, docker.io, python3, make, jq,
931:#                                 ca-certificates and golang-go (DF-BUNKER-52/
932:#                                 80) — it is not what a plain spawn has.
933:#   - build toolchains (SURF-009): OPT-IN per spawn — `bunker spawn --toolchains[=CLASS]`
934:#                                 (minimal | go | full) or `bunker toolchain-check
935:#                                 <agent> --deliver --class <class>`. Classes require
936:#                                 python3 (minimal); + go, make (go); + gcc, node, cargo
937:#                                 (full). go/gcc/make come from the userland the host
938:#                                 already has; node and cargo are delivered into the
939:#                                 agent's own $HOME from their publishers' checksummed
940:#                                 artifacts (no root), ~750 MB per agent for the Rust
941:#                                 half. Check any agent with `bunker toolchain-check
942:#                                 <agent> --class <class>` (exit 1 names what is
943:#                                 missing) — see docs/provisioning.md
945:# List agents
948:# Run a command inside the agent (including Docker)
951:# Mount the agent's filesystem locally
954:# Forward the agent's Docker socket
956:# In another terminal: DOCKER_HOST=tcp://localhost:2376 docker ps
958:# See agent details
961:# Set / read agent environment variables (KEY=VALUE as one argument)
965:# Extend TTL
968:# Tear down (also deletes the client-local SSH key ~/.bunker/keys/abc12345)
970:# Keep the local key for a spawn/destroy/spawn key-reuse cycle:
972:# Skip the home archive for this one destroy (the home is deleted with NO copy)
980:# These are mutating commands: --server required (binding note above).
981:# Pause an agent without destroying it (frees CPU, keeps user/home/container/ports)
984:# Resume a stopped agent
987:# Recover a wedged session: stop + start in one call, heartbeat TTL reset
1068:## Architecture
1092:### Services
1126:## Resource Limits
1176:### Did the agent hit its ceiling? (resource events)
1212:## Can one agent see another? (security model)
1232:## Agent isolation (`/tmp` and cross-agent exchange)
1337:# agent A hands a file to agent B
1345:## CLI Commands
1452:### Pulling files
1467:### Exit codes
1490:## CLI config & path overrides (DF-BUNKER-16)
1529:## Tech Stack
1544:## Development
1547:# Build the CLI + daemon binaries (produces ./bunker and ./bunkerd in the repo root)
1551:# Or use make build — same binaries with Version/Commit/BuildDate baked in
1554:# Run tests
1557:# Verify README/CHANGELOG against the newest release tag — documented CLI
1558:# commands must exist in that tag or be marked "requires a build from HEAD"
1561:# Run E2E battery (requires a running bunkerd)
1564:# Preview exactly what the battery would use — binaries + certification
1565:# verdict, endpoints, token source, state dirs, which sections run. No root,
1566:# nothing is changed:
1569:# The E2E battery requires root: it creates Linux users via useradd and
1570:# writes a root-owned daemon log under /var/log, so run it with sudo. The
1571:# harness prints a clear refusal (exit 42) instead of dying silently, and
1572:# any failure names the offending line and command. Verify the harness's
1573:# own diagnostics as a normal user — no root, no side effects:
1576:# Run regression suite (standalone: takes over only daemons nothing else
1577:# manages; a systemd-managed bunkerd is detected and left running, and the
1578:# operator's own CLI config is never read, written or removed)
1582:### E2E battery inputs
1603:### Who owns the daemon (standalone vs coexist)
1633:### CLI-state isolation
1647:### Running against a deployed daemon
1674:### Deploying a build for the E2E gate
1694:### Quality Gates
1700:## License
### RC: 0

### CMD: grep -nE ^[[:space:]]*[-*][[:space:]]+\[ README.md

### RC: 1

================ ARM 5: link + badge drift (local paths and external HTTP) ================

### CMD: python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/readme-link-check.py . README.md
readme=README.md local_links=32 external_links=12
line 5 OK      bunker-hero.png
line 40 OK      docs/performance.md
line 53 OK      docs/mount-drivers.md
line 70 OK      specs/agent-tmp-isolation.md
line 83 OK      specs/agent-lifecycle.md
line 84 OK      docs/egress-policy.md
line 90 OK      specs/container-mode.md
line 176 OK      docs/threat-model.md
line 231 OK      docs/integration.md
line 353 OK      CHANGELOG.md
line 411 OK      docs/threat-model.md
line 457 OK      specs/api.md
line 489 OK      docs/presets/README.md
line 490 OK      specs/configuration.md
line 628 OK      docs/audit.md#tls-verification-markers
line 1124 OK      specs/api.md
line 1155 OK      specs/safety-presets.md
line 1156 OK      internal/agent/SKILL.md
line 1210 OK      docs/resource-events.md
line 1219 OK      specs/agent-lifecycle.md
line 1220 OK      specs/agent-lifecycle.md
line 1221 OK      specs/agent-lifecycle.md
line 1222 OK      specs/agent-tmp-isolation.md
line 1223 OK      specs/agent-tmp-isolation.md
line 1224 OK      specs/agent-lifecycle.md
line 1230 OK      docs/threat-model.md
line 1343 OK      specs/agent-tmp-isolation.md
line 1465 OK      docs/both-ways.md
line 1533 OK      go.mod
line 1702 OK      LICENSE
line 1704 OK      NOTICE
line 1706 OK      CLA.md
line 7 200 https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go  <- https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go
line 8 200 https://img.shields.io/badge/License-Apache%202.0-blue.svg  <- https://img.shields.io/badge/License-Apache%202.0-blue.svg
line 9 200 https://img.shields.io/badge/gRPC-connect--go-4285F4  <- https://img.shields.io/badge/gRPC-connect--go-4285F4
line 10 200 https://img.shields.io/badge/Docker-rootless-2496ED?logo=docker  <- https://img.shields.io/badge/Docker-rootless-2496ED?logo=docker
line 210 200 https://github.com/deployBunker  <- https://github.com/deployBunker
line 1534 200 https://connectrpc.com/  <- https://connectrpc.com/
line 1535 200 https://github.com/go-chi/chi  <- https://github.com/go-chi/chi
line 1536 200 https://github.com/spf13/cobra  <- https://github.com/spf13/cobra
line 1536 200 https://github.com/spf13/viper  <- https://github.com/spf13/viper
line 1537 200 https://github.com/golang-jwt/jwt  <- https://github.com/golang-jwt/jwt
line 1538 200 https://github.com/caddyserver/certmagic  <- https://github.com/caddyserver/certmagic
local_missing=0 external_non_200=0
### RC: 0

================ ARM 6: README-024 finding — CONTRIBUTING.md never linked ================

### CMD: grep -n -i contribut README.md
1706:Contributions welcome under the [Contributor License Agreement](CLA.md).
### RC: 0

### CMD: grep -c CONTRIBUTING.md README.md
0
### RC: 1

### CMD: ls -l CONTRIBUTING.md
-rw-rw-r-- 1 kara kara 2483 Nov 22 01:10 CONTRIBUTING.md
### RC: 0

### CMD: git log -1 --format=%h_%cI_%s -- CONTRIBUTING.md
4c4a0b4d_2026-09-28T19:47:06-05:00_docs: fix CONTRIBUTING.md rot - Go version, board path, gitreins command
### RC: 0

### CMD: sed -n 1700,1708p README.md
## License

Apache 2.0 — see [LICENSE](LICENSE).

Third-party attributions in [NOTICE](NOTICE).

Contributions welcome under the [Contributor License Agreement](CLA.md).

---
### RC: 0

================ ARM 7: README-016 re-probe — self-test vs TMPDIR ================

### NOTE: inherited TMPDIR=[/mnt/ssd/tmp]

### CMD: bash -c timeout 300 bash e2e-full-battery.sh --self-test 2>&1 | tail -3
  ✓ section 8 keeps per-spawn output and waits each pid (no /dev/null'd spawn, no whole-set wait — INT-CI-080)

SELF-TEST: FAIL (1 check(s) failed)
### RC: 0

### CMD: bash -c TMPDIR=/tmp timeout 300 bash e2e-full-battery.sh --self-test 2>&1 | tail -3
  ✓ section 8 keeps per-spawn output and waits each pid (no /dev/null'd spawn, no whole-set wait — INT-CI-080)

SELF-TEST: PASS
### RC: 0

### CMD: sed -n 1311p e2e-full-battery.sh
    ST_SCRATCH_PROBE="$(mktemp -d /tmp/bunker-battery-cli-XXXXXX)"
### RC: 0

### CMD: sed -n 459,460p e2e-full-battery.sh
        "${TMPDIR:-/tmp}"/bunker-battery-cli-*|"${TMPDIR:-/tmp}"/bunker-battery-nested-cli-*)
            rm -rf "$dir" 2>/dev/null || true
### RC: 0

================ ARM 8: README-010 / README-008 premise re-probe ================

### CMD: grep -n bunker paths\|iobattery README.md
1445:bunker paths       Print every resolved secret/state path (locations and
1448:bunker iobattery   Run the I/O measurement battery (BFS-057) and emit a JSON
1449:                   report (see docs/iobattery.md)
### RC: 0

### CMD: grep -n released after v0\.\|build from this checkout README.md
276:sh install.sh --build                    # build from this checkout with go build
### RC: 0

### CMD: sed -n 1345,1350p README.md
## CLI Commands

Commands in the newest release tag (`git describe --tags --abbrev=0` → **v0.3.0**)
— these are what `go install github.com/deployBunker/bunker/cmd/bunker@latest`
gives you:
### RC: 0

================ ARM 9: README-009 / README-017 premise re-probe ================

### CMD: grep -n Unreleased README.md
353:> listed under *Unreleased* in the [CHANGELOG](CHANGELOG.md) and need a build
### RC: 0

### CMD: sed -n 347,358p README.md
> **Freshness check — what the release assets and `@latest` both carry.** The
> newest release tag (`git describe --tags --abbrev=0`) is **v0.3.0**:
> `go install ...@latest`, the release binaries the installer downloads, and a
> build of that tag all carry that tag's surface — including the lifecycle
> commands (`stop`, `start`, `restart`) and the host-maintenance commands
> (`homes`, `linger`, `host-provision`). Commands that land after a release are
> listed under *Unreleased* in the [CHANGELOG](CHANGELOG.md) and need a build
> from this checkout (`make build`) until the next tag.
> A release asset or a tag build reports the tagged commit in `commit:` by
> design, so only a HEAD build's `commit:` equals the repo's
> `git rev-parse HEAD` — a `commit:` that does not match HEAD means you are
> running a release build.
### RC: 0

### CMD: sed -n 1574,1582p README.md
bash e2e-full-battery.sh --self-test

# Run regression suite (standalone: takes over only daemons nothing else
# manages; a systemd-managed bunkerd is detected and left running, and the
# operator's own CLI config is never read, written or removed)
bash regression-tests.sh
```

### E2E battery inputs
### RC: 0

### CMD: grep -n -m5 -E \-\-help|Usage: regression-tests.sh

### RC: 1

================ ARM 10: other open-row premises (still live check) ================

### CMD: grep -c destroy --force README.md
0
### RC: 1

### CMD: grep -n bunker/keys README.md
968:# Tear down (also deletes the client-local SSH key ~/.bunker/keys/abc12345)
1007:> key to `~/.bunker/keys/<agent-id>` (fetched from the daemon via the
1462:spawn saved for the agent at `~/.config/bunker/keys/<agent-id>`:
1463:`scp -i ~/.config/bunker/keys/<agent-id> bunker-<agent-id>@<host>:<remote-path> <local-dir>`.
### RC: 0

### CMD: sed -n 747,754p README.md
$ ./bunkerd --version
bunkerd 0.1.4
  commit:     923c4be
  built:      2026-09-18T23:46:15Z
  caps:       isolation-grant
  go version: go1.26.5
  platform:   linux/amd64
```
### RC: 0

### CMD: sed -n 755,758p README.md

A daemon that does not report `caps: isolation-grant` is refused at **any**
version, because a version number cannot prove a capability: the newest release
tag predates the isolation implementation, and a plain `go build` reports the
### RC: 0

### CMD: grep -n no per-command exit codes README.md
1469:The CLI has no per-command exit codes of its own: `0` on success, `1` on any
### RC: 0

### CMD: bash -c sed -n '/^### Exit codes/,/^## CLI config/p' README.md | grep -c 'TLSRefusalExitCode\|tls_refusal'
0
### RC: 1

================ ARM 11: stranger install path ================

### CMD: sh scripts/install.sh --dry-run
install.sh: installing bunker latest for linux/amd64
install.sh: note: /usr/local/bin is not writable by kara — falling back to /home/kara/.local/bin (override with --dir)
install.sh: target directory: /home/kara/.local/bin
install.sh: dry-run: release source https://github.com/deployBunker/bunker/releases (version latest)
install.sh: dry-run: would download https://github.com/deployBunker/bunker/releases/latest/download/SHA256SUMS
install.sh: dry-run: would download https://github.com/deployBunker/bunker/releases/latest/download/bunker-linux-amd64
install.sh: dry-run: would download https://github.com/deployBunker/bunker/releases/latest/download/bunkerd-linux-amd64
install.sh: dry-run: would verify both binaries against SHA256SUMS, then install them into /home/kara/.local/bin
install.sh: dry-run: would install bunker -> /home/kara/.local/bin/bunker (mode 0755)
install.sh: dry-run: would install bunkerd -> /home/kara/.local/bin/bunkerd (mode 0755)
install.sh: dry-run complete: nothing was downloaded, built or written
### RC: 0

### CMD: sh scripts/install.sh --help
install.sh — install the bunker CLI and the bunkerd daemon.

Usage:
  install.sh [--version vX.Y.Z] [--dir PREFIX] [--dry-run]
  install.sh --from-dir DIR [--dir PREFIX] [--dry-run]
  install.sh --build [--dir PREFIX] [--dry-run]

Default: download the prebuilt release assets for this platform (linux/amd64 or
linux/arm64), verify them against the release's SHA256SUMS, then install.

sshfs is NOT installed or upgraded unless --sshfs is passed; the default run
leaves whatever sshfs the host already has exactly as it is.

Options:
  --version vX.Y.Z  install that release tag instead of the newest release
  --from-dir DIR    install the binaries in DIR (offline/air-gapped); the
                    checksums in DIR/SHA256SUMS are verified when present
  --build           build bunker and bunkerd from this checkout with go build
                    (no make, no prebuilt asset; needs a Go toolchain)
  --dir PREFIX      install prefix (default /usr/local/bin; falls back to
                    $HOME/.local/bin when the default is not writable)
  --dry-run         print every action without downloading, writing or building
  --sshfs[=MODE]    after the bunker install, also ensure the host's sshfs.
                    MODE is one of:
                      min (default)  install the newest sshfs the distro offers
                                     when none or < 3.7.6; WARN when the result
                                     is still in the affected range (< 3.7.6)
                      package        like min, but REFUSE (42) when the distro
                                     cannot provide >= 3.7.6
                      source         build sshfs 3.7.6 from the pinned upstream
                                     tag (checksum-verified) and install it to
                                     <prefix>/sshfs; needs meson, ninja and git
                    Without =MODE, --sshfs means --sshfs=min.
  -h, --help        print this help

Exit codes: 0 success, 42 refusal (unsupported platform, checksum mismatch,
missing checksum entry, no Go for --build, sshfs mode refusals), 1 any other
error.

Examples:
  curl -fsSL https://github.com/deployBunker/bunker/releases/latest/download/install.sh | sh
  sh install.sh --version v0.1.4 --dir "$HOME/.local/bin"
  sh install.sh --from-dir ./dist
  sh install.sh --build
  sh install.sh --sshfs=source --dir "$HOME/.local/bin"   # also build+install patched sshfs
### RC: 0

================ ARM 12: daemon process contract ================

### CMD: ./bunkerd version
bunkerd 0.3.0
  commit:     8b4100a2
  built:      2026-11-15T05:45:56Z
  caps:       isolation-grant
  go version: go1.26.10
  platform:   linux/amd64
### RC: 0

### CMD: bash -c ./bunkerd help 2>&1 | head -6
bunkerd — Bunker agent host daemon

Usage:
  bunkerd [flags]
  bunkerd --show-paths Print resolved secret/state paths (locations/modes only) and exit
  bunkerd port-pool check  Print the port-pool geometry vs agent.max_agents and exit
### RC: 0

PRODUCER: complete
