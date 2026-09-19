# Changelog

## Unreleased

Commits after the `v0.1.4` release tag. Everything below is in the tree but not
in the newest release tag, so it is what the README marks as *requires a build
from HEAD*.

### Added

- Agent lifecycle commands: `bunker stop <agent-id>` pauses an agent without
  destroying it (its Linux user, home, container and allocated port range
  survive), `bunker start <agent-id>` re-arms it, and `bunker restart <agent-id>`
  does both in one call to recover a wedged session while resetting the heartbeat
  expiry
- Local host-maintenance commands: `bunker homes` / `bunker homes prune` report
  and remove orphaned `bunker-*` home directories, and `bunker linger` /
  `bunker linger prune` do the same for stale `systemd` linger entries
  (`--dry-run` first; fail-closed on an inconclusive user lookup)
- `bunker host-provision` installs, inspects (`--status`, `--json`) and removes
  (`--uninstall`) the host-side per-agent `/tmp` isolation boundary — dry run by
  default, and it requires the daemon to report the `isolation-grant` capability
- `go run ./cmd/docs-drift` (also `make docs-check`, wired into CI): a
  deterministic offline check that the README's documented CLI surface exists in
  the newest release tag or is marked as requiring a build from HEAD, that no
  release tag older than the newest one is presented as current, and that this
  section exists while commits sit after the tag

- `make test-sh` (DF-BUNKER-25): runs `scripts/install_test.sh`, a network-free
  suite of 49 assertions — unsupported platform refuses with exit 42, checksum
  mismatch refuses with exit 42 and installs nothing, a missing SHA256SUMS entry
  refuses, `--from-dir` installs both binaries for the plain and
  `bunker-<os>-<arch>` layouts and is idempotent, the default release path
  installs from a `file://` mirror for both the latest and the tag-pinned asset
  URLs, `--dry-run` writes nothing, and `--build` without a Go toolchain names
  the README section instead of surfacing Error 127
- `scripts/install.sh` (DF-BUNKER-25): one-command install for a fresh host —
  the default mode downloads the prebuilt release assets for linux/amd64 or
  linux/arm64 and verifies both binaries against the release's `SHA256SUMS`
  before anything is written; `--from-dir` installs local binaries
  (offline/air-gapped), `--build` builds from the checkout with `go build` and no
  `make`, plus `--version`, `--dir`, `--dry-run` and `--help`. It refuses with
  exit 42 on an unsupported platform, a checksum mismatch or a missing checksum
  entry, falls back to `$HOME/.local/bin` when `/usr/local/bin` is not writable
  instead of escalating privileges, and smoke-checks the installed binary's
  `--version` (warning when the version stamp is missing)
- `make release-binaries` (DF-BUNKER-25): the single source of truth for the
  release artifacts — cross-compiles linux/amd64 and linux/arm64 for both
  `bunker` and `bunkerd` with the same ldflags as `build`, then writes
  `SHA256SUMS` and copies `scripts/install.sh` into `dist/`
- `.github/workflows/release.yml` (DF-BUNKER-25): on a `v*` tag it calls
  `make release-binaries` (the build commands are not duplicated in YAML) and
  publishes `bunker-linux-amd64`, `bunkerd-linux-amd64`, `bunker-linux-arm64`,
  `bunkerd-linux-arm64`, `SHA256SUMS` and `install.sh` as a GitHub Release,
  idempotently: an existing release for the tag has its assets replaced
  (`--clobber`) and the workflow asserts all six names are attached
- Makefile `check-go` guard (DF-BUNKER-25): `build`, `build-daemon`,
  `build-cli`, `install` and `release-binaries` refuse with the documented
  recovery steps (README Install section, tarball URL, PATH export, and the
  `$HOME` extraction pitfall) when no Go toolchain is on PATH, instead of
  surfacing `sh: 1: go: not found` (exit 127)

### Docs

- README: the documented CLI surface is split into "in the newest release tag"
  and "requires a build from HEAD" instead of documenting commands the
  `go install ...@latest` path cannot run, and the freshness note no longer calls
  `v0.1.3` the newest release — it is `v0.1.4` (GAP-081)
- README (DF-BUNKER-25): the one-command installer is the first documented
  install path, the build-from-source path documents obtaining Go on a stock
  Debian/Ubuntu host (tarball under `/usr/local/go` plus the PATH export, with
  the `GOPATH == GOROOT` pitfall called out for a `$HOME` extraction), and the
  former "the repo does not ship prebuilt binaries" note now describes the
  release-asset path

## 0.1.4 (2026-09-13)

### Added
- Durable agent registry (GAP-070): append-only JSONL lifecycle log (spawn/heartbeat/destroy) replayed at startup so a `bunkerd` restart never orphans live agents; startup reconciliation of unmanaged `bunker-*` users (`destroy` default, `adopt` opt-in via `agent.reconciliation.mode`); `bunker registry compact` for rotation; the daemon refuses to start when the registry is enabled but its file cannot be opened

## 0.1.3 (2026-08-20)

### Fixed
- `go install github.com/deployBunker/bunker/cmd/bunker@latest` serves HEAD again — v0.1.3 cut as a fresh version at HEAD (97 commits past v0.1.2) so fresh installs get the DOGFOOD-008 `spawn` positional agent-id binding and the README/SKILL.md-documented behavior; tag==version==CHANGELOG parity restored (GAP-046)

## 0.1.2 (2026-08-15)

### Added
- E2E VERIFY-PASS artifact for the release — `docs/dogfood/2026-08-15-gap-044-e2e.md` runs the full battery against the tagged tree on bunker-mvp in coexist mode (GAP-044)

### Fixed
- `go install github.com/deployBunker/bunker/cmd/bunker@latest` serves the current release again — v0.1.2 cut as a fresh version because the force-moved v0.1.1 tag left the module proxy permanently serving the stale pre-bump zip; the tag-build CI gate now derives the expected version from the tag name (GAP-043)
- `bunker --version` prints the same 5-field block as `bunker version` — consistent UX-005 version contract (GAP-045)

## 0.1.1 (2026-08-10)

### Added
- `bunker ssh <agent-id> [command...]` — interactive session into an agent (GAP-034)
- `bunker --version` via cobra (GAP-035)
- `bunker systemd` helpers no longer hidden in `--help` (GAP-033)
- CI: CLI-surface smoke (`--version` + `ssh`/`systemd` in `--help`) and a version-authority check (latest git tag == `bunker version` == CHANGELOG top entry) on every push (GAP-036/GAP-038)

### Fixed
- `go install github.com/deployBunker/bunker/cmd/bunker@latest` works from a fresh checkout — generated protobuf code is now committed (GAP-027)
- `go install @latest` ships the full CLI again — the v0.1.1 tag was re-cut at HEAD after being cut from a pre-bump commit that built a 0.1.0 binary without `ssh`/`systemd`; CI now builds from the tag itself (GAP-041)
- `bunker version` reports a real commit and build time on `go install` builds — VCS metadata from the embedded build info (`debug.ReadBuildInfo`) is used when ldflags are absent, with the module version as a last-resort commit (GAP-042)
- Deterministic agent PATH — exec builders no longer inherit the daemon's ambient `$PATH` (GAP-030)
- Version defaults aligned across source, tags, and docs (GAP-031)
- `bunker status` exits non-zero when no servers are configured, matching `list`/`spawn`/`info` (GAP-037)

### Docs
- Agent port range, `max_agents` default, and demo port corrections across docs (GAP-028/GAP-029/GAP-032)

## 0.1.0 (2026-07-06)

### Features
- Multi-server support: `bunker use <server>` and `bunker status --all-servers`
- Rootless Docker container support with user namespace remapping
- Cloudflare tunnel auto-provisioning per container
- mTLS between CLI and server with certificate generation
- JWT-based API key authentication with scope enforcement
- cgroup resource enforcement (CPU, memory, PID limits)
- Tailscale integration for private networking
- Hermes Agent auto-provisioning inside containers
- Hilo code intelligence integration
- systemd unit generation for bunkerd
- CLI commands: spawn, destroy, list, info, exec, connect, status, use, mount, metrics, heartbeat

### Tests
- 459 test functions across 50 test files (532 test cases)
- 14 packages, all passing
- Live E2E battery on bunker-mvp

### Infrastructure
- Go 1.26.5
- ConnectRPC (gRPC-compatible)
- Docker SDK
- Cloudflare API
