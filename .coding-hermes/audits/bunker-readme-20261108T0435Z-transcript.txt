### unshallow
From https://github.com/deployBunker/bunker
 * [new tag]           v0.1.1     -> v0.1.1
 * [new tag]           v0.1.2     -> v0.1.2
 * [new tag]           v0.1.3     -> v0.1.3
 * [new tag]           v0.1.4     -> v0.1.4
 * [new tag]           v0.2.0     -> v0.2.0
rc=0
### HEAD == published tip
60d6795ef6980524d6b5d04da43a2190d38b2fac
### make docs-check
go run ./cmd/docs-drift
docs OK: README/CHANGELOG/docs match v0.2.0 (35 released commands, 1176 commit(s) after the tag)
  released: [agent-tools audit connect cp deploy destroy env exec fs guard heartbeat homes host-provision info key linger list metrics mount registry renew restart run spawn ssh start status stop subid-migrate surface systemd tunnel umount use version]
rc=0
### make build
make: host disk space OK (110 GiB minimum free; 6% threshold).
go build -ldflags "-X github.com/deployBunker/bunker/internal/version.Version=0.2.1 -X github.com/deployBunker/bunker/internal/version.Commit=60d6795e -X github.com/deployBunker/bunker/internal/version.BuildDate=2026-11-08T04:35:43Z" -o ./bunkerd ./cmd/bunkerd
go build -ldflags "-X github.com/deployBunker/bunker/internal/version.Version=0.2.1 -X github.com/deployBunker/bunker/internal/version.Commit=60d6795e -X github.com/deployBunker/bunker/internal/version.BuildDate=2026-11-08T04:35:43Z" -o ./bunker ./cmd/bunker
rc=0
### go test -short
ok  	github.com/deployBunker/bunker/internal/iobattery	0.379s
ok  	github.com/deployBunker/bunker/internal/mountdriver	0.019s
ok  	github.com/deployBunker/bunker/internal/netmode	0.042s
ok  	github.com/deployBunker/bunker/internal/operator	0.015s
ok  	github.com/deployBunker/bunker/internal/provisioning	0.016s
ok  	github.com/deployBunker/bunker/internal/qa	0.005s
ok  	github.com/deployBunker/bunker/internal/registry	2.524s
ok  	github.com/deployBunker/bunker/internal/releasecheck	0.299s
ok  	github.com/deployBunker/bunker/internal/resource	0.027s
ok  	github.com/deployBunker/bunker/internal/server	16.152s
ok  	github.com/deployBunker/bunker/internal/server/webdav	17.801s
ok  	github.com/deployBunker/bunker/internal/sshsig	0.003s
ok  	github.com/deployBunker/bunker/internal/supplychain	0.008s
ok  	github.com/deployBunker/bunker/internal/systemd	0.004s
ok  	github.com/deployBunker/bunker/internal/tailscale	0.069s
ok  	github.com/deployBunker/bunker/internal/tlsutil	0.006s
ok  	github.com/deployBunker/bunker/internal/tunnel	21.365s
ok  	github.com/deployBunker/bunker/internal/version	0.003s
?   	github.com/deployBunker/bunker/probes/bfs025-truncated-read	[no test files]
?   	github.com/deployBunker/bunker/probes/bfs052-fuse-caps	[no test files]
?   	github.com/deployBunker/bunker/probes/davserve	[no test files]
?   	github.com/deployBunker/bunker/probes/h3client	[no test files]
ok  	github.com/deployBunker/bunker/probes/webdav-battery	0.066s
?   	github.com/deployBunker/bunker/proto/bunker/v1	[no test files]
ok  	github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect	0.008s
rc=0
### CLI help (command list)
Available Commands:
  agent-tools     Report which executables the remote editing verbs need, and which are missing on the agent
  audit           Inspect the bunkerd audit trail
  completion      Generate the autocompletion script for the specified shell
  connect         Register a bunkerd server
  cp              Copy a file to an agent's environment via SCP
  deploy          Deploy a directory to an agent's environment via SCP
  destroy         Destroy an agent
  env             Manage an agent's persistent environment variables
  exec            Execute a command in an agent's environment
  fs              bunker-fs: mount the WebDAV surface as a filesystem (opt-in; sshfs stays the default)
  guard           Do-not-build guard for SSHFS mounts
  heartbeat       Send a heartbeat to extend an agent's TTL
  help            Help about any command
  homes           Inspect and maintain orphaned agent home directories
  host-provision  Provision the per-agent isolation boundary on this host (GAP-075)
  info            Show detailed information about an agent
  iobattery       Run the I/O measurement battery (BFS-057) and emit a JSON report
  key             Manage API keys and the JWT signing secret (master only)
  linger          Inspect and maintain the systemd linger directory
  list            List agents on a bunkerd server
  metrics         Show live resource metrics for an agent or server
  mount           Mount an agent's home directory via SSHFS
  operator        Manage operator identities and their RBAC roles (admin only)
  paths           Print every resolved secret/state path (locations and modes only)
  pull            Copy a file from an agent's environment via SCP
  reap            Report (and with --apply remove) orphaned AppArmor profiles, agent homes, linger entries and runtime dirs
  registry        Inspect and maintain the durable agent registry
  renew           Renew an agent under its STABLE identity (destroy + re-spawn the same id)
  resource-events Show an agent's resource-ceiling events (memory/pids) and observed peaks
  restart         Restart an agent (stop + start, TTL reset)
  run             Run a command in an agent's environment
  spawn           Create a new agent
  ssh             Open an interactive SSH session into an agent
  start           Start a stopped agent
  status          Show status of bunkerd servers
  stop            Stop an agent (keeping its state)
  subid-migrate   Rewrite overlapping subordinate-id ranges for managed agents (GAP-140)
  surface         Manage the agent's toolsd socket surface (systemd user units)
  systemd         Manage the bunkerd systemd service
  toolchain-check Report a declared build-toolchain class (minimal, go, full) present/absent by name on the agent
  tunnel          Open an SSH tunnel to an agent's Docker socket
  umount          Unmount an agent's SSHFS mount (idempotent)
  use             Switch the active bunkerd server
  version         Print the bunker CLI version

rc=0
### paths --help
Prints, for every secret/state path bunker and bunkerd resolve:

  the resolved absolute location, the file mode (or the mode it would be
  created with), and WHICH resolution rule produced it.

The precedence chain is flag > environment variable > per-user config dir
(${XDG_CONFIG_HOME:-~/.config}/bunker, with the legacy ~/.bunker kept as a
read alias) > documented system paths (/etc/bunkerd, /var/log/bunkerd,
/var/lib/bunkerd). Resolution never consults the process working directory,
so running the binary from any folder resolves the same paths and creates
nothing in that folder.

rc=0
### iobattery --help
Run the I/O measurement battery (BFS-057) against a filesystem target and
emit a machine-readable JSON report.

The target is any directory: a bunker mount point (mount the agent home with
'bunker mount <agent-id> <dir>' first) or, when no bunker agent is available,
an ordinary local directory as a fallback. The report covers five measurement
families plus a negative control:

  throughput          single-stream sequential write + read (MB/s)
  latency_under_load  p50/p95/p99 of small ops while background load runs
  metadata_ops        create/stat/rename/unlink rate (ops/s)
  iops                random 4K read/write IOPS
rc=0
### version
bunker 0.2.1
  commit:     60d6795e
  built:      2026-11-08T04:35:43Z
  go version: go1.26.10
  platform:   linux/amd64
### install.sh --dry-run (isolated HOME)
install.sh: installing bunker latest for linux/amd64
install.sh: note: /usr/local/bin is not writable by kara — falling back to <scratch-home>/.local/bin (override with --dir)
install.sh: target directory: <scratch-home>/.local/bin
install.sh: dry-run: release source https://github.com/deployBunker/bunker/releases (version latest)
install.sh: dry-run: would download https://github.com/deployBunker/bunker/releases/latest/download/SHA256SUMS
install.sh: dry-run: would download https://github.com/deployBunker/bunker/releases/latest/download/bunker-linux-amd64
install.sh: dry-run: would download https://github.com/deployBunker/bunker/releases/latest/download/bunkerd-linux-amd64
install.sh: dry-run: would verify both binaries against SHA256SUMS, then install them into <scratch-home>/.local/bin
install.sh: dry-run: would install bunker -> <scratch-home>/.local/bin/bunker (mode 0755)
install.sh: dry-run: would install bunkerd -> <scratch-home>/.local/bin/bunkerd (mode 0755)
install.sh: dry-run complete: nothing was downloaded, built or written
rc=0
### e2e --show-plan
  root requirement      : the full run needs root (exit 42 otherwise); --self-test/--show-plan/--bin-report never do
  sections that run     : 1-11, 13, 14 (14's live-daemon registry replays are skipped in coexist mode)
  sections that may skip: 12 only when no regression-tests.sh is present; 15.1 fails without a CLI that has host-provision
  section 15 certifies  : /usr/local/bin/bunkerd (candidate daemon) via --daemon-binary on every host-provision call

PLAN: OK (nothing was changed)
rc=0
### e2e --self-test
  ✓ the battery exports BUNKER_SESSION_TARGET exactly once, AFTER the connect that registers "$BATTERY_TARGET_NAME" (line 654 < 1982)
  ✓ teardown order is destroys (1386) -> unset binding (1702) -> remove state dir (1787) — PINNED (the battery's order was already correct; the inverted one was regression-tests.sh's, fixed by INT-CI-025)
  ✓ a foreign BUNKER_SESSION_TARGET is refused up front (rc=2, naming both the foreign value and this battery's target)
  ✓ the guard accepts a binding that already names this battery's target (e2e-local|e2e-local)
  ✓ with BUNKER_E2E_TARGET unset the target name defaults to e2e-local
  ✓ port preflight refuses a stale :29091 listener naming pid + reclaim (rc=1)
  ✓ port preflight passes when neither requested port is LISTENing
  ✓ agent_docker_ready fails LOUD with path diagnostics when the agent is denied (rc=1)
  ✓ agent_docker_ready passes silently when the agent CAN reach docker
  ✓ section 8 keeps per-spawn output and waits each pid (no /dev/null'd spawn, no whole-set wait — INT-CI-080)

SELF-TEST: FAIL (1 check(s) failed)
rc=1
### bunker paths (README:1422 claim)
bunker paths — resolved secret/state locations (GAP-181)
COMPONENT  NAME                                           PATH                                                                             MODE                   RULE
cli        config.yaml (server registry, per-host tokens) <scratch-home>/.config/bunker/config.yaml 0600 (intended, missing) xdg
cli        per-host token (server entry in config.yaml)   <scratch-home>/.config/bunker/config.yaml 0600 (intended, missing) xdg
cli        agent SSH keys directory (keys/<agent-id>, ... <scratch-home>/.config/bunker/keys        0700 (intended, missing) xdg
                                                                                                                                                                     
                                                                                                                                                                     
                                                                                                                                                                     
bunkerd    daemon config file                             /etc/bunkerd/config.yaml                                                            0600                   system
                                                                                                                                                                     
                                                                                                                                                                     
                                                                                                                                                                     

Precedence: flag > env > per-user config dir (${XDG_CONFIG_HOME:-~/.config}/bunker) > legacy ~/.bunker (read alias) > system defaults (/etc/bunkerd, /var/log/bunkerd, /var/lib/bunkerd).
rc=0
### iobattery on local dir (README:1425 claim)
bunker: unknown command "<scratch-home>/iob-target" for "bunker iobattery"
rc=1
### self-test failing check
  [capture] synthetic failing command failed (exit=7)
  ✓ run_capture surfaces failing-command output (exit=7)
  ✓ ERR trap names failing line + command (line=930 cmd=false)
  [capture] synthetic failing nested suite failed (exit=1)
  ✓ a nested suite exiting non-zero flips section 12 to FAIL (FAIL: the nested suite exited 1 (tally PASS: 16 / FAIL: 14))
  ✓ section 12 reads the nested suite's OWN tally lines (16 14), not a grep of ✓/✗ and the word PASS
### iobattery --help USAGE line
21:  bunker iobattery --target /mnt/bunker/agent-dev
22:  bunker iobattery --target /tmp/io-scratch --size-mb 64 --ops 5000
23:  bunker iobattery --target /mnt/bunker/agent-dev --out report.json
25:Usage:
26:  bunker iobattery --target <dir> [flags]
28:Flags:
36:      --target string      directory to measure (bunker mount point or local dir; required)
38:Global Flags:
### iobattery --target attempt
{
  "schema": "bunker.iobattery.v1",
  "write_shape": {
    "append_supported": true,
    "random_write_supported": true,
    "metadata_supported": true
  },
  "target": "<scratch-home>/iob-target",
  "host": "karaHermes-mde-7840hs",
  "os": "linux/amd64",
  "started": "2026-11-08T04:40:27Z",
  "command": "bunker iobattery",
  "measurements": [
    {
      "name": "throughput",
      "kind": "bytes_moved",
      "count": 67108864,
      "elapsed_ms": 17.869,
      "derived": 2086.4256171337274,
      "derived_unit": "MB/s",
      "detail": {
        "chunk_bytes": 1048576,
        "payload_mb": 32,
        "read_bytes": 33554432,
        "read_mb_s": 12636.573719401722,
rc=0
### self-test FAIL detail
30:  ✓ section 12 reads the nested suite's OWN tally lines (16 14), not a grep of ✓/✗ and the word PASS
33:  ✓ remove_own_state_dir REFUSES a dir that is not the harness's own scratch (rc=1, the config file is untouched)
35:  ✗ the harness's own scratch dir was not removed by remove_own_state_dir
### regression-tests.sh (README-017)
── 3. Connect ──
  [0;31m✗[0m connect succeeds (echo "$OUT" | grep -q "Connected")
  [0;31m✗[0m connect reports a numeric agent count (pristine host reads 'Agents: 0/'; residue reads higher and is baselined in section 3b) (echo "$OUT" | grep -q "Agents: [0-9]*/")
  [0;31m✗[0m registered under the dedicated name (no fleet clobber) (echo "$OUT" | grep -q "Server registered as \"regression-local\"")
  [0;32m✓[0m session target bound: BUNKER_SESSION_TARGET=regression-local
  [0;31m✗[0m runner residue baseline: could not read a numeric agent count from 'bunker status' (first line: bunker: no servers configured; run 'bunker connect' first)

[1;33m=== Cleanup ===[0m
  operator config server names BEFORE: bunker-las-01 bunker-las-02 bunker-las-03 bunker-las-04 bunker-mvp cube-las-00 dedi-2 hermes-fleet-01 hermes-fleet-02 hermes-fleet-03 hermes-fleet-04 hermes4friends judgebox karahermes-mde-7840hs hermes-fleet-05 
  operator config server names AFTER:  bunker-las-01 bunker-las-02 bunker-las-03 bunker-las-04 bunker-mvp cube-las-00 dedi-2 hermes-fleet-01 hermes-fleet-02 hermes-fleet-03 hermes-fleet-04 hermes4friends judgebox karahermes-mde-7840hs hermes-fleet-05 
  ⚠ NOT killing bunkerd processes: systemd manages the running bunkerd (MainPID 905538) — this suite only manages what it creates
  ⚠ NOT sweeping /run/bunker/* and /etc/bunkerd/ssh/t*: systemd manages the running bunkerd (MainPID 905538) — this suite only manages what it creates
rc=3
