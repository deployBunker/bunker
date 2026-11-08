Bunker README health sweep: bunker-readme-2026-11-08-04-02-17
Canonical repository: /home/kara/deployBunker/bunker
Published remote: origin refs/heads/main
Published SHA at probe: 933235d7109c5d08c6a06fc18a1255c1d2e5e1f6
Published SHA recheck: 933235d7109c5d08c6a06fc18a1255c1d2e5e1f6
README probe tree: canonical checkout, clean before audit; README exists

COMMAND: git -C /home/kara/deployBunker/bunker ls-remote --symref origin HEAD
EXIT_STATUS=0
OUTPUT:
ref: refs/heads/main	HEAD
933235d7109c5d08c6a06fc18a1255c1d2e5e1f6	HEAD

COMMAND: git -C /home/kara/deployBunker/bunker log -1 --format='%ad %H %s' --date=iso-strict v0.2.0
EXIT_STATUS=0
OUTPUT:
2026-10-01T04:21:16-05:00 149411c168d1359579d948a5ee73d80222356507 release: v0.2.0 version-prep (RELEASE-011 step 2; closes REV-BUNKER-018)

COMMAND: git -C /home/kara/deployBunker/bunker log -1 --format='%h %ad %s' --date=short -- README.md
EXIT_STATUS=0
OUTPUT:
0e7051a6 2026-11-07 docs: renew flags, observability push-gaps metric, README paths/iobattery

COMMAND: curl -fsSL --max-time 20 https://api.github.com/repos/deployBunker/bunker/releases/latest | jq '{tag_name,name,published_at,assets:[.assets[]|.name]}'
EXIT_STATUS=0
OUTPUT:
{"tag_name":"v0.2.0","name":"v0.2.0","published_at":"2026-10-02T12:01:18Z","assets":["bunker-linux-amd64","bunker-linux-arm64","bunkerd-linux-amd64","bunkerd-linux-arm64","install.sh","SHA256SUMS"]}

COMMAND: make docs-check
EXIT_STATUS=0
OUTPUT:
go run ./cmd/docs-drift
docs OK: README/CHANGELOG/docs match v0.2.0 (35 released commands, 1181 commit(s) after the tag)
  released: [agent-tools audit connect cp deploy destroy env exec fs guard heartbeat homes host-provision info key linger list metrics mount registry renew restart run spawn ssh start status stop subid-migrate surface systemd tunnel umount use version]

COMMAND: ./bunker --version; ./bunkerd --version
EXIT_STATUS=0
OUTPUT:
bunker 0.2.1
  commit:     f7989995
  built:      2026-10-31T04:06:21Z
  go version: go1.26.10
  platform:   linux/amd64
bunkerd 0.2.1
  commit:     f7989995
  built:      2026-10-31T04:06:21Z
  caps:       isolation-grant
  go version: go1.26.10
  platform:   linux/amd64

COMMAND: go test ./... -short
EXIT_STATUS=0
OUTPUT: all listed packages passed; representative final packages included internal/server, internal/server/webdav, internal/version, and proto/bunker/v1/bunkerv1connect.

COMMAND: bash e2e-full-battery.sh --self-test
EXIT_STATUS=1
OUTPUT:
... ✗ the harness's own scratch dir was not removed by remove_own_state_dir
SELF-TEST: FAIL (1 check(s) failed)

COMMAND: bash e2e-full-battery.sh --show-plan
EXIT_STATUS=0
OUTPUT:
PLAN: OK (nothing was changed)
certification verdict: MISMATCH (binary reports bff29c84; repo HEAD 933235d7109c5d08c6a06fc18a1255c1d2e5e1f6)
root requirement: the full run needs root; --self-test/--show-plan/--bin-report never do

COMMAND: bash regression-tests.sh --help
EXIT_STATUS=3
OUTPUT:
✗ running as root ([ "$(id -u)" = "0" ])
regression-tests.sh: line 793: /var/log/bunkerd-regression.log: Permission denied
✗ bunkerd EXITED AT STARTUP

COMMAND: tracked-markdown relative-link census
EXIT_STATUS=0
OUTPUT:
MISSING: none reported by the README-local link check

COMMAND: boardctl -C /home/kara/deployBunker/bunker validate
EXIT_STATUS=0
OUTPUT:
RESULT: OK (1001 warning(s)); baseline warnings are pre-existing legacy board-shape/event warnings.

CURRENT FINDING READBACKS
README-016 remains pending and its finding reproduces: exact documented self-test exits 1 with SELF-TEST: FAIL (1 check(s) failed).
README-017 remains pending and its finding reproduces: exact documented regression-tests.sh path exits 3 with non-root/log permission failures; README block does not state the root/log prerequisites.
README-018 remains pending and its finding reproduces: README capability sample still shows bunkerd 0.1.4 / commit 923c4be / built 2026-09-18, while current built binary reports 0.2.1 / commit f7989995 / built 2026-10-31.
README-020 remains pending and its finding reproduces: README generic exit-code prose at lines 1447-1450 says only two exceptions, while the toolchain-check section documents a third command-level 0/1 gate.

UNVERIFIED
Every documented command block and prose command was not executed in this sweep. In particular, commands requiring root, a live daemon, a real token, a remote host, Docker/rootless agents, SSHFS, systemd service mutation, host provisioning, uninstall/apply, release installation, and destructive teardown were not run. The clean canonical checkout was used because the scheduler stand-in is gitless and has no README.
