Bunker README health sweep evidence
Tick: bunker-readme-2026-11-15-05-16-17
Canonical checkout: /home/kara/deployBunker/bunker
Published tip: 8b4100a274e1c56ff7ab2ba8a8ba069a8c27ef31 (origin/main)
Frozen producer artifacts:
- final-producer-20261115T0516Z-transcript.txt sha256 aeb33a5ea3582e79b8475baa4cebcb20af16c0ddd304b65babde502ecc6d3394

RELEASE/FRESHNESS
Command: curl -fsSL --max-time 60 https://github.com/deployBunker/bunker/releases/latest/download/bunkerd-linux-amd64 -o <scratch>/release/final-bunkerd-linux-amd64 && chmod +x <scratch>/release/final-bunkerd-linux-amd64 && <scratch>/release/final-bunkerd-linux-amd64 --version
Exit: 0
Output: bunkerd 0.3.0; commit: de286fdc; built: 2026-11-09T05:16:34Z; caps: isolation-grant; go version: go1.26.10; platform: linux/amd64.
Command: curl -fsSL --max-time 60 https://github.com/deployBunker/bunker/releases/latest/download/SHA256SUMS ... && sha256sum -c final-SHA256SUMS --ignore-missing
Exit: 0
Output: bunker-linux-amd64: OK; bunkerd-linux-amd64: OK.
Command: grep -nE 'newest release tag predates|TLSRefusal|exit 2|exit code 2|per-command exit codes|EXIT CODE IS THE GATE' README.md
Exit: 0
Output: README.md:1454: The CLI has no per-command exit codes of its own: `0` on success, `1` on any.
The isolation paragraph at README.md:300-307 still says the newest release tag predates the isolation implementation.
Command: make docs-check
Exit: 0
Output: docs OK: README/CHANGELOG/docs match v0.3.0 (42 released commands, 117 commit(s) after the tag).

COMMAND MATRIX / INSTALL
Command: go build -o <scratch>/bunker-final ./cmd/bunker; <scratch>/bunker-final --version
Exit: 0
Output: bunker 0.3.0; commit: 8b4100a; go version: go1.26.10; platform: linux/amd64.
Command: go build -o <scratch>/bunkerd-final ./cmd/bunkerd; <scratch>/bunkerd-final --version
Exit: 0
Output: bunkerd 0.3.0; commit: 8b4100a; caps: isolation-grant; go version: go1.26.10; platform: linux/amd64.
Command: go test ./... -short -count=1
Exit: 0. The transcript records all packages passing, including internal/cli, internal/docscheck, internal/server, internal/server/webdav, internal/tunnel, and generated proto packages.
Command: bash e2e-full-battery.sh --show-plan
Exit: 0. Output: PLAN: OK (nothing was changed).
Command: bash e2e-full-battery.sh --self-test
Exit: 1. Output: SELF-TEST: FAIL (1 check(s) failed).
Command: HOME=<fresh-home> BUNKER_HOME=<fresh-home> sh scripts/install.sh --dry-run
Exit: 0. Output: dry-run complete: nothing was downloaded, built or written; target directory is <fresh-home>/.local/bin.
Command: bash regression-tests.sh --help
Exit: 3. Output includes: running as root check failed; regression-tests.sh: line 793: /var/log/bunkerd-regression.log: Permission denied; suite attempted startup and did not behave as help-only.

ANCHORS / LINKS / BADGES
Command: python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/check-md-anchors.py README.md
Exit: 1.
Output: BROKEN README.md:582: #the-tls-insecure-knob-2026-09; BROKEN README.md:657: #the-tls_insecure-knob-2026-09; anchors_checked=16 broken=2.
Command: python3 <stand-in>/link-check.py
Exit: 0. Output: LOCAL_LINKS=33 MISSING=0; every 33 repository-relative targets checked resolved. External URL checks returned HTTP 200 for go.dev, connectrpc.com, Docker rootless docs, github.com/deployBunker, and the latest install.sh asset.
Badges: README.md:7-10 are Go, License, gRPC, Docker shields; no Actions badge is present.

READER STRUCTURE / FRESHNESS DETAILS
README headings provide What is Bunker, Features, Quick Start, Prerequisites, Install, Configure, Provision host isolation, Run the daemon, Use the CLI, Architecture, Resource Limits, security model, CLI Commands, Exit codes, config/path overrides, Tech Stack, Development, License.
The README CLI help inventory includes the command rows, but the documented command matrix has the existing failures above. The exit-code prose at README.md:1454 still excludes command-specific gates; the toolchain-check block documents a 0/1 gate and --report-only=0. Source grep confirms internal/cli/tls_refusal.go:75 defines TLSRefusalExitCode = 2 and cmd/bunker/main.go:34 exits with the refusal code.
The HEAD-built cp/deploy/destroy help still includes ~/.bunker/keys in the --ssh-key/--keep-key text, while README's pull section uses ~/.config/bunker/keys.

UNVERIFIED
A live root daemon start, host-provision --apply/--uninstall, live agent spawn/exec/mount/tunnel/SSHFS paths, hosted-demo token path, destructive teardown, and an independent mismatched-certificate TLS refusal reproduction were not run. The README-023 exit-2 claim is source-supported but its acceptance-required second-daemon reproduction remains for the worker. The local regression suite's root/log failure is retained as observed behavior, not converted into a clean pass.

FILED ROWS
README-022: ch:trace row=README-022 spec=README.md#provision-host-isolation-before-spawning wave=none:serial-audit test=none:worker-must-rerun-release-acceptance doc=README.md evidence=.coding-hermes/audits/bunker-readme-20261115T0516Z-evidence.txt witness=release:v0.3.0 verdict=none:open-finding commit=none:open memory=none:exact-key-not-found
README-023: ch:trace row=README-023 spec=README.md#exit-codes wave=none:serial-audit test=none:worker-must-run-mismatched-certificate-reproduction doc=README.md evidence=.coding-hermes/audits/bunker-readme-20261115T0516Z-evidence.txt witness=none:worker-must-reproduce-mismatched-certificate verdict=none:open-finding commit=none:open memory=none:exact-key-not-found
