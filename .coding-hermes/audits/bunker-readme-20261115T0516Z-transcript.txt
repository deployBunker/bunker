
=== COMMAND ===
sh -c date -u +%Y-%m-%dT%H:%M:%SZ
=== CWD ===
/home/kara/deployBunker/bunker
2026-11-15T06:04:11Z
=== EXIT_STATUS=0 ===

=== COMMAND ===
git -C /home/kara/deployBunker/bunker rev-parse HEAD
=== CWD ===
/home/kara/deployBunker/bunker
8b4100a274e1c56ff7ab2ba8a8ba069a8c27ef31
=== EXIT_STATUS=0 ===

=== COMMAND ===
git -C /home/kara/deployBunker/bunker ls-remote --symref origin HEAD
=== CWD ===
/home/kara/deployBunker/bunker
ref: refs/heads/main	HEAD
8b4100a274e1c56ff7ab2ba8a8ba069a8c27ef31	HEAD
=== EXIT_STATUS=0 ===

=== COMMAND ===
git -C /home/kara/deployBunker/bunker status --short
=== CWD ===
/home/kara/deployBunker/bunker
=== EXIT_STATUS=0 ===

=== COMMAND ===
git -C /home/kara/deployBunker/bunker diff --quiet 31f842f -- README.md
=== CWD ===
/home/kara/deployBunker/bunker
=== EXIT_STATUS=0 ===

=== COMMAND ===
git -C /home/kara/deployBunker/bunker rev-parse 31f842f:README.md
=== CWD ===
/home/kara/deployBunker/bunker
7ec2043235c46dc9ee3ef119674e6d3c81f4dea3
=== EXIT_STATUS=0 ===

=== COMMAND ===
git -C /home/kara/deployBunker/bunker rev-parse HEAD:README.md
=== CWD ===
/home/kara/deployBunker/bunker
7ec2043235c46dc9ee3ef119674e6d3c81f4dea3
=== EXIT_STATUS=0 ===

=== COMMAND ===
git -C /home/kara/deployBunker/bunker tag --sort=-version:refname
=== CWD ===
/home/kara/deployBunker/bunker
v0.3.0
v0.2.0
v0.1.4
v0.1.3
v0.1.2
v0.1.1
v0.1.0
=== EXIT_STATUS=0 ===

=== COMMAND ===
git -C /home/kara/deployBunker/bunker show -s --format=%H %cI %D %s v0.3.0
=== CWD ===
/home/kara/deployBunker/bunker
tag v0.3.0
Tagger: totalwindupflightsystems <totalwindupflightsystems@gmail.com>

Release v0.3.0
de286fdce69cd92f9f182f3e1eecde34404d713e 2026-11-09T00:14:38-05:00 tag: v0.3.0 release: bump to v0.3.0 — promote the CHANGELOG and align the README with the released surface
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c grep -nE "^#{1,4} |^```|Exit codes|toolchain-check|install|test|deploy|Known|Limit|Uninstall|agent-tools|regression-tests|docs-check" /home/kara/deployBunker/bunker/README.md
=== CWD ===
/home/kara/deployBunker/bunker
sh: 1: Syntax error: EOF in backquote substitution
=== EXIT_STATUS=2 ===

=== COMMAND ===
sh -c grep -nE "newest release tag predates|TLSRefusal|exit 2|exit code 2|per-command exit codes|EXIT CODE IS THE GATE" /home/kara/deployBunker/bunker/README.md
=== CWD ===
/home/kara/deployBunker/bunker
1454:The CLI has no per-command exit codes of its own: `0` on success, `1` on any
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c make docs-check
=== CWD ===
/home/kara/deployBunker/bunker
go run ./cmd/docs-drift
docs OK: README/CHANGELOG/docs match v0.3.0 (42 released commands, 117 commit(s) after the tag)
  released: [agent-tools audit connect cp deploy destroy env exec fs guard heartbeat homes host-provision info iobattery key linger list metrics mount operator paths pull reap registry renew resource-events restart run spawn ssh start status stop subid-migrate surface systemd toolchain-check tunnel umount use version]
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c go build -o /home/kara/.hermes/stand-in/readme/bunker/bunker-head ./cmd/bunker && /home/kara/.hermes/stand-in/readme/bunker/bunker-head --version
=== CWD ===
/home/kara/deployBunker/bunker
bunker 0.3.0
  commit:     8b4100a
  built:      2026-11-15T05:42:40Z
  go version: go1.26.10
  platform:   linux/amd64
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c go build -o /home/kara/.hermes/stand-in/readme/bunker/bunkerd-head ./cmd/bunkerd && /home/kara/.hermes/stand-in/readme/bunker/bunkerd-head --version
=== CWD ===
/home/kara/deployBunker/bunker
bunkerd 0.3.0
  commit:     8b4100a
  built:      2026-11-15T05:42:40Z
  caps:       isolation-grant
  go version: go1.26.10
  platform:   linux/amd64
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c go test ./... -short -count=1
=== CWD ===
/home/kara/deployBunker/bunker
ok  	github.com/deployBunker/bunker/cmd/bunker	0.038s
ok  	github.com/deployBunker/bunker/cmd/bunkerd	0.179s
?   	github.com/deployBunker/bunker/cmd/docs-drift	[no test files]
ok  	github.com/deployBunker/bunker/internal/agent	165.126s
ok  	github.com/deployBunker/bunker/internal/apikey	0.011s
ok  	github.com/deployBunker/bunker/internal/audit	4.037s
ok  	github.com/deployBunker/bunker/internal/auth	0.320s
ok  	github.com/deployBunker/bunker/internal/cli	39.125s
ok  	github.com/deployBunker/bunker/internal/config	0.127s
?   	github.com/deployBunker/bunker/internal/davserve	[no test files]
ok  	github.com/deployBunker/bunker/internal/docscheck	7.831s
ok  	github.com/deployBunker/bunker/internal/egress	0.023s
ok  	github.com/deployBunker/bunker/internal/fleet	0.013s
ok  	github.com/deployBunker/bunker/internal/fsclient	41.564s
ok  	github.com/deployBunker/bunker/internal/fsmount	1.665s
ok  	github.com/deployBunker/bunker/internal/hermes	192.524s
ok  	github.com/deployBunker/bunker/internal/hilo	0.017s
ok  	github.com/deployBunker/bunker/internal/hostsetup	2.598s
ok  	github.com/deployBunker/bunker/internal/imagespec	0.038s
ok  	github.com/deployBunker/bunker/internal/invalidation	0.005s
ok  	github.com/deployBunker/bunker/internal/iobattery	0.467s
ok  	github.com/deployBunker/bunker/internal/mountdriver	0.006s
ok  	github.com/deployBunker/bunker/internal/netmode	0.039s
ok  	github.com/deployBunker/bunker/internal/operator	0.020s
ok  	github.com/deployBunker/bunker/internal/provisioning	0.007s
ok  	github.com/deployBunker/bunker/internal/qa	0.004s
ok  	github.com/deployBunker/bunker/internal/registry	2.871s
ok  	github.com/deployBunker/bunker/internal/releasecheck	0.206s
ok  	github.com/deployBunker/bunker/internal/resource	0.022s
ok  	github.com/deployBunker/bunker/internal/server	16.841s
ok  	github.com/deployBunker/bunker/internal/server/webdav	17.708s
ok  	github.com/deployBunker/bunker/internal/sshsig	0.003s
ok  	github.com/deployBunker/bunker/internal/supplychain	0.009s
ok  	github.com/deployBunker/bunker/internal/systemd	0.004s
ok  	github.com/deployBunker/bunker/internal/tailscale	0.073s
ok  	github.com/deployBunker/bunker/internal/tlsutil	0.033s
ok  	github.com/deployBunker/bunker/internal/tunnel	21.333s
ok  	github.com/deployBunker/bunker/internal/version	0.003s
?   	github.com/deployBunker/bunker/probes/bfs025-truncated-read	[no test files]
?   	github.com/deployBunker/bunker/probes/bfs052-fuse-caps	[no test files]
?   	github.com/deployBunker/bunker/probes/davserve	[no test files]
?   	github.com/deployBunker/bunker/probes/h3client	[no test files]
ok  	github.com/deployBunker/bunker/probes/webdav-battery	0.108s
?   	github.com/deployBunker/bunker/proto/bunker/v1	[no test files]
ok  	github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect	0.010s
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c bash e2e-full-battery.sh --show-plan
=== CWD ===
/home/kara/deployBunker/bunker
==========================================
 BUNKER END-TO-END TEST BATTERY
 Sun Nov 15 01:07:28 AM -05 2026
==========================================


=== E2E BATTERY PLAN (no root required, nothing was changed) ===
  battery CLI binary    : /usr/local/bin/bunker
  battery daemon binary : /usr/local/bin/bunkerd
  certification verdict : MISMATCH (binary reports bff29c84; repo HEAD 8b4100a274e1c56ff7ab2ba8a8ba069a8c27ef31)
  mode                  : standalone — talks to the daemon already on the ports above; starts/stops no daemon itself (INT-CI-012)
  nested suite (sec 12) : its own ports :29092 :28082 and its own CLI state dir — it never shares this battery's ports; a systemd-managed bunkerd and its agent state are left alone
  sections 13/14 policy : probe http://localhost:18080 first — an unreachable daemon fails ONE cell naming the endpoint, not a cascade of 'not found'
  daemon URL (connect)  : http://localhost:18080  [default]
  REST port             : :18080  (BUNKERD_REST_ADDR=:18080)
  gRPC port             : :19090  (BUNKERD_GRPC_ADDR=:19090)
  token source          : default (fingerprint sha256:1096116d; the token itself is never printed)
  session target        : e2e-local  (registered by the section-2 connect --name, bound as BUNKER_SESSION_TARGET; every mutating call also passes --server explicitly — GAP-093)
  verdict host identity : the VERIFY verdict will name the host the DAEMON reports (bcli status Hostname, QA-BUNKER-B14); expected on this runner: karaHermes-mde-7840hs
  nested suite binding  : the child runs with BUNKER_SESSION_TARGET cleared (env -u) and binds its own target name
  battery CLI state dir : /var/tmp/hermes-scratch/bunker-battery-cli-FN9Dbl  (BUNKER_HOME and HOME for every CLI call; removed on exit)
  battery CLI config    : /var/tmp/hermes-scratch/bunker-battery-cli-FN9Dbl/config.yaml  (created on the first CLI call)
  operator CLI config   : /home/kara/.bunker/config.yaml  (exists; sha256:f98ddb9d9d7ce44fa8624a5165e9a9fed808e95ad5c5db40d2f1d41b8e351ad2)
  operator config policy: never read or written by this battery — the fingerprint is re-checked at the end of the run
  root requirement      : the full run needs root (exit 42 otherwise); --self-test/--show-plan/--bin-report never do
  sections that run     : 1-11, 13, 14 (14's live-daemon registry replays are skipped in coexist mode)
  sections that may skip: 12 only when no regression-tests.sh is present; 15.1 fails without a CLI that has host-provision
  section 15 certifies  : /usr/local/bin/bunkerd (candidate daemon) via --daemon-binary on every host-provision call

PLAN: OK (nothing was changed)
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c bash e2e-full-battery.sh --self-test
=== CWD ===
/home/kara/deployBunker/bunker
==========================================
 BUNKER END-TO-END TEST BATTERY
 Sun Nov 15 01:07:28 AM -05 2026
==========================================

=== 0. Self-test (no root, no side effects) ===
  [capture] synthetic failing command failed (exit=7)
  ✓ run_capture surfaces failing-command output (exit=7)
  ✓ ERR trap names failing line + command (line=930 cmd=false)
  ✓ preflight would refuse a non-root uid (rc=1, reason printed)
  ✓ token refusal would refuse an unauthenticated connect (rc=0, reason printed)
  ✓ token refusal accepts a successful connect (rc=0)
  ✓ bin_commit_verdict: short/full SHA prefixes MATCH both ways
  ✓ bin_commit_verdict: two different real SHAs are a MISMATCH
  ✓ bin_commit_verdict: unknown/dev/empty commit and missing HEAD are SKIP (never MISMATCH)
  ✓ bin_commit_verdict: case/whitespace normalized before compare
  ✓ bin_commit_verdict: a <7-char prefix never MATCHes (not a real git abbreviation)
  ✓ isolated CLI state: the operator's config is byte-identical after a CLI invocation (HOME=/tmp/bunker-battery-selftest-op-yH4Wn9)
  ✓ isolated CLI state: the CLI wrote config.yaml under the battery's own state dir (/var/tmp/hermes-scratch/bunker-battery-cli-OUVzrL), not the operator's
  ✓ bcli pins ONE stable state dir for every call (BUNKER_HOME=HOME=/var/tmp/hermes-scratch/bunker-battery-cli-OUVzrL)
  ✓ explicit inputs honored: BUNKERD_REST_ADDR=:18080 + BUNKER_TOKEN exported → REST :18080, token source=env
  ✓ unset BUNKER_TOKEN resolves to token source=default (not silently inherited)
  ✓ the nested suite is handed its own isolated ports in both modes (:29092 :28082)
  ✓ no production/battery port is handed to the nested suite
  ✓ section 15 has no bare bcli host-provision calls and uses gap075_bcli (4 helper call site(s))
  ✓ no bare bcli destroy calls remain (every teardown destroy carries || true or run_capture — INT-CI-049)
  ✓ the single nested-suite invocation carries explicit non-production ports (line 2474)
  [capture] synthetic failing nested suite failed (exit=1)
  ✓ a nested suite exiting non-zero flips section 12 to FAIL (FAIL: the nested suite exited 1 (tally PASS: 16 / FAIL: 14))
  ✓ section 12 reads the nested suite's OWN tally lines (16 14), not a grep of ✓/✗ and the word PASS
  ✓ a green nested suite reports PASS with its own tally (PASS: the nested suite exited 0 with tally PASS: 21 / FAIL: 0)
  ✓ a nested suite that printed no tally is UNVERIFIED (FAIL), never a silent pass
  ✓ remove_own_state_dir REFUSES a dir that is not the harness's own scratch (rc=1, the config file is untouched)
  ⚠ refusing to remove '/tmp/bunker-battery-cli-MHtulo' — not a scratch dir this harness created (nothing was deleted; operator CLI config: /home/kara/.bunker/config.yaml)
  ✗ the harness's own scratch dir was not removed by remove_own_state_dir
  ✓ regression-tests.sh removes no path under /root (no operator CLI config deletion)
  ✓ regression-tests.sh pins BOTH BUNKER_HOME and HOME to its own scratch dir (standalone included)
  ✓ regression-tests.sh funnels its single pkill bunkerd through kill_stray_bunkerd (never a systemd-managed daemon)
  ✓ every mutating call site carries an explicit --server "$BATTERY_TARGET_NAME" (defense in depth, GAP-093 binding contract)
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
=== EXIT_STATUS=1 ===

=== COMMAND ===
sh -c HOME=/home/kara/.hermes/stand-in/readme/bunker/fresh-home BUNKER_HOME=/home/kara/.hermes/stand-in/readme/bunker/fresh-home sh scripts/install.sh --dry-run
=== CWD ===
/home/kara/deployBunker/bunker
install.sh: installing bunker latest for linux/amd64
install.sh: note: /usr/local/bin is not writable by kara — falling back to /home/kara/.hermes/stand-in/readme/bunker/fresh-home/.local/bin (override with --dir)
install.sh: target directory: /home/kara/.hermes/stand-in/readme/bunker/fresh-home/.local/bin
install.sh: dry-run: release source https://github.com/deployBunker/bunker/releases (version latest)
install.sh: dry-run: would download https://github.com/deployBunker/bunker/releases/latest/download/SHA256SUMS
install.sh: dry-run: would download https://github.com/deployBunker/bunker/releases/latest/download/bunker-linux-amd64
install.sh: dry-run: would download https://github.com/deployBunker/bunker/releases/latest/download/bunkerd-linux-amd64
install.sh: dry-run: would verify both binaries against SHA256SUMS, then install them into /home/kara/.hermes/stand-in/readme/bunker/fresh-home/.local/bin
install.sh: dry-run: would install bunker -> /home/kara/.hermes/stand-in/readme/bunker/fresh-home/.local/bin/bunker (mode 0755)
install.sh: dry-run: would install bunkerd -> /home/kara/.hermes/stand-in/readme/bunker/fresh-home/.local/bin/bunkerd (mode 0755)
install.sh: dry-run complete: nothing was downloaded, built or written
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c HOME=/home/kara/.hermes/stand-in/readme/bunker/fresh-home BUNKER_HOME=/home/kara/.hermes/stand-in/readme/bunker/fresh-home /home/kara/.hermes/stand-in/readme/bunker/bunker-head --help
=== CWD ===
/home/kara/deployBunker/bunker
bunker is the command-line tool for managing Bunker agent hosts.

Manage servers, deploy bunkerd instances, connect to remote hosts,
and control ephemeral development environments — all from the CLI.

Client-side path overrides (persistent flags):
  --config <path>         CLI config file. Precedence: --config >
                          $BUNKER_HOME/config.yaml > $HOME/.bunker/config.yaml.
                          (bunker systemd install's local --config means the
                          DAEMON config file and shadows this flag.)
  --daemon-config <path>  DAEMON config file consulted for the default paths
                          of the local-file commands (audit list/verify/export/
                          status --path, registry compact --path). Precedence:
                          --daemon-config > $BUNKERD_CONFIG > /etc/bunkerd/config.yaml.
                          Missing or unreadable files fall back silently to the
                          documented constants. An explicit --path on those
                          commands always wins.

Usage:
  bunker [flags]
  bunker [command]

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

Flags:
      --config string          CLI config file. Precedence: --config > $BUNKER_HOME/config.yaml > $HOME/.bunker/config.yaml. NOTE: 'bunker systemd install --config' means the DAEMON config and shadows this flag
      --daemon-config string   DAEMON config file for the local-file command defaults (audit --path, registry compact --path). Precedence: --daemon-config > $BUNKERD_CONFIG > /etc/bunkerd/config.yaml. An explicit --path on those commands always wins
  -h, --help                   help for bunker
      --version                Print the bunker version (same as 'bunker version')

Use "bunker [command] --help" for more information about a command.
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c bash regression-tests.sh --help
=== CWD ===
/home/kara/deployBunker/bunker
══════════════════════════════════════════════
  Bunker Regression Test Suite
══════════════════════════════════════════════

── 1. Prerequisites ──
  ⚠ NOT killing bunkerd processes: systemd manages the running bunkerd (MainPID 905538) — this suite only manages what it creates
  ⚠ NOT stopping/disabling stale agent dockerd units: systemd manages the running bunkerd (MainPID 905538) — this suite only manages what it creates
  ⚠ NOT sweeping /run/bunker/* and /etc/bunkerd/ssh/*: systemd manages the running bunkerd (MainPID 905538) — this suite only manages what it creates
  ⚠ NOT removing stale bunker-docker-* user units: systemd manages the running bunkerd (MainPID 905538) — this suite only manages what it creates
  [0;32m✓[0m bunkerd binary exists
  [0;32m✓[0m bunker CLI binary exists
  [0;31m✗[0m running as root ([ "$(id -u)" = "0" ])

── 2. Server startup ──
  [0;32m✓[0m bunkerd started (PID 2274324)
regression-tests.sh: line 793: /var/log/bunkerd-regression.log: Permission denied
  [0;31m✗[0m bunkerd EXITED AT STARTUP (PID 2274324) — no recognised refusal line in the log; see the tail below
  --- last 40 lines of /var/log/bunkerd-regression.log ---
2026/09/20 21:27:22 [karaHermes-mde-7840hs/zZjhadaurY-000002] "POST http://127.0.0.1:29090/bunker.v1.Bunkerd/SpawnAgent HTTP/1.1" from 127.0.0.1:57916 - 500 155B in 189.14µs
{"time":"2026-09-20T21:27:22.729843159-05:00","level":"INFO","msg":"allocated port range","agent_id":"f495d286","range":"20200-20299"}
{"time":"2026-09-20T21:27:22.729872934-05:00","level":"INFO","msg":"spawning agent","agent_id":"f495d286"}
{"time":"2026-09-20T21:27:22.72988151-05:00","level":"INFO","msg":"creating user","username":"bunker-f495d286"}
{"time":"2026-09-20T21:27:22.884104685-05:00","level":"INFO","msg":"agent isolation group membership ensured","agent_id":"f495d286","group":"bunker-agents"}
{"time":"2026-09-20T21:27:22.900011172-05:00","level":"INFO","msg":"shared scratch ready","agent_id":"f495d286"}
{"time":"2026-09-20T21:27:22.905593015-05:00","level":"INFO","msg":"private /tmp instance ready","agent_id":"f495d286"}
{"time":"2026-09-20T21:27:22.905624964-05:00","level":"INFO","msg":"generating SSH keypair","keyfile":"/tmp/bunker-key-f495d286"}
{"time":"2026-09-20T21:27:22.909315677-05:00","level":"INFO","msg":"setting up authorized_keys","user":"bunker-f495d286"}
{"time":"2026-09-20T21:27:22.915610991-05:00","level":"INFO","msg":"found host SSH public key, appending to agent authorized_keys","path":"/home/kara/.ssh/id_ed25519.pub","agent_user":"bunker-f495d286"}
{"time":"2026-09-20T21:27:22.915666033-05:00","level":"INFO","msg":"provisioned host SSH key into agent authorized_keys","agent_user":"bunker-f495d286"}
{"time":"2026-09-20T21:27:22.918355111-05:00","level":"INFO","msg":"persisted SSH private key","path":"/etc/bunkerd/ssh/f495d286"}
{"time":"2026-09-20T21:27:22.91837609-05:00","level":"INFO","msg":"starting rootless dockerd","unit":"bunker-docker-f495d286","sock":"/run/bunker/f495d286/docker.sock"}
{"time":"2026-09-20T21:27:22.918384836-05:00","level":"INFO","msg":"spawn entering stage","agent_id":"f495d286","stage":"rootless-install"}
{"time":"2026-09-20T21:27:22.930334999-05:00","level":"INFO","msg":"installed rootlesskit apparmor profile","profile":"home.bunker-f495d286.bin.rootlesskit"}
{"time":"2026-09-20T21:27:22.933664913-05:00","level":"INFO","msg":"installing rootless docker","user":"bunker-f495d286"}
{"time":"2026-09-20T21:27:27.035719974-05:00","level":"WARN","msg":"rootless installer hit the unit-not-found race; reloading the user manager and retrying once","user":"bunker-f495d286","unit":"user@1003.service","output":"# Installing stable version 29.8.1 | # Executing docker rootless install script, commit: 2b32480025b223ebfddae9a3a8bef09027680f53 | % Total % Received % Xferd Average Speed Time Time Time Current | Dload Upload Total Spent Left Speed | 0 0 0 0 0 0 0 0 0 80 82.06M 80 66.15M 0 0 66.18M 0 00:01 00:01 66.15M 100 82.06M 100 82.06M 0 0 71.87M 0 00:01 00:01 66.15M 100 82.06M 100 82.06M 0 0 71.86M 0 00:01..."}
{"time":"2026-09-20T21:27:27.547806153-05:00","level":"INFO","msg":"destroying agent","agent_id":"regr-alpha"}
{"time":"2026-09-20T21:27:27.5589215-05:00","level":"WARN","msg":"agent container rm failed (continuing)","agent_id":"regr-alpha","error":"exit status 1","output":"failed to connect to the docker API at unix:///run/bunker/regr-alpha/docker.sock; check if the path is correct and if the daemon is running: dial unix /run/bunker/regr-alpha/docker.sock: connect: no such file or directory"}
{"time":"2026-09-20T21:27:27.558956985-05:00","level":"WARN","msg":"agent container cleanup incomplete","agent_id":"regr-alpha","error":"remove agent container bunker-regr-alpha: exit status 1 (output: failed to connect to the docker API at unix:///run/bunker/regr-alpha/docker.sock; check if the path is correct and if the daemon is running: dial unix /run/bunker/regr-alpha/docker.sock: connect: no such file or directory)"}
{"time":"2026-09-20T21:27:27.559004624-05:00","level":"INFO","msg":"removed user slice drop-in","slice":"user-1001.slice"}
{"time":"2026-09-20T21:27:27.97535431-05:00","level":"INFO","msg":"rootless installer succeeded on the daemon-reload retry","user":"bunker-f495d286"}
{"time":"2026-09-20T21:27:27.978851103-05:00","level":"INFO","msg":"spawn entering stage","agent_id":"f495d286","stage":"dockerd-start"}
{"time":"2026-09-20T21:27:28.387582676-05:00","level":"WARN","msg":"direct dockerd stop failed","unit":"bunker-docker-regr-alpha","error":"pgrep dockerd for user bunker-regr-alpha: exit status 1 (output: )"}
{"time":"2026-09-20T21:27:28.390168984-05:00","level":"WARN","msg":"systemctl stop failed (may not exist)","unit":"bunker-docker-regr-alpha","error":"exit status 1","output":"Failed to connect to user scope bus via local transport: $DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not defined (consider using --machine=<user>@.host --user to connect to bus of other user)\n"}
{"time":"2026-09-20T21:27:28.454367101-05:00","level":"INFO","msg":"archiving agent home before delete","agent_id":"regr-alpha","policy":"archive","home":"/home/bunker-regr-alpha","archive_dir":"/var/backups/bunker"}
{"time":"2026-09-20T21:27:28.750515063-05:00","level":"INFO","msg":"dockerd ready","user":"bunker-f495d286","sock":"/run/bunker/f495d286/docker.sock"}
{"time":"2026-09-20T21:27:29.068138774-05:00","level":"INFO","msg":"wrote user slice drop-in","slice":"user-1003.slice","path":"/etc/systemd/system/user-1003.slice.d/50-bunker.conf"}
{"time":"2026-09-20T21:27:29.851026132-05:00","level":"INFO","msg":"spawn entering stage","agent_id":"f495d286","stage":"session-probe"}
{"time":"2026-09-20T21:27:30.037035422-05:00","level":"INFO","msg":"session probe succeeded","agent_id":"f495d286","stage":"session-probe","attempts":1}
{"time":"2026-09-20T21:27:30.037061991-05:00","level":"INFO","msg":"agent registered","agent_id":"f495d286","total":3}
{"time":"2026-09-20T21:27:30.040930182-05:00","level":"INFO","msg":"agent spawned successfully","agent_id":"f495d286"}
2026/09/20 21:27:30 [karaHermes-mde-7840hs/zZjhadaurY-000003] "POST http://127.0.0.1:29090/bunker.v1.Bunkerd/SpawnAgent HTTP/1.1" from 127.0.0.1:57922 - 200 329B in 7.312728011s
2026/09/20 21:27:30 [karaHermes-mde-7840hs/zZjhadaurY-000005] "POST http://127.0.0.1:29090/bunker.v1.Bunkerd/GetAgentKey HTTP/1.1" from 127.0.0.1:57922 - 200 312B in 684.297µs
{"time":"2026-09-20T21:27:38.822054078-05:00","level":"INFO","msg":"agent home archived and verified","agent_id":"regr-alpha","policy":"archive","archive_path":"/var/backups/bunker/bunker-regr-alpha-20260921T022728Z.tar.gz"}
{"time":"2026-09-20T21:27:39.014995701-05:00","level":"INFO","msg":"agent unregistered","agent_id":"regr-alpha","total":2}
{"time":"2026-09-20T21:27:39.015015037-05:00","level":"INFO","msg":"freed port range","agent_id":"regr-alpha"}
{"time":"2026-09-20T21:27:39.018455676-05:00","level":"INFO","msg":"agent destroyed","agent_id":"regr-alpha"}
2026/09/20 21:27:39 [karaHermes-mde-7840hs/zZjhadaurY-000004] "POST http://127.0.0.1:29090/bunker.v1.Bunkerd/DestroyAgent HTTP/1.1" from 127.0.0.1:57936 - 200 47B in 11.471508574s
{"time":"2026-09-20T21:27:39.025699507-05:00","level":"INFO","msg":"shutting down","signal":"terminated"}
  --- end of /var/log/bunkerd-regression.log ---

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
=== EXIT_STATUS=3 ===

=== COMMAND ===
sh -c python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/check-md-anchors.py README.md
=== CWD ===
/home/kara/deployBunker/bunker
BROKEN README.md:582: #the-tls-insecure-knob-2026-09
BROKEN README.md:657: #the-tls_insecure-knob-2026-09
anchors_checked=16 broken=2
=== EXIT_STATUS=1 ===

=== COMMAND ===
sh -c python3 - <<"PY"
import re
from pathlib import Path
p=Path("README.md")
s=p.read_text()
local=[]
for target in re.findall(r"\]\(([^)#]+)(?:#[^)]*)?\)", s):
    if target.startswith(("http://","https://","mailto:")): continue
    q=(p.parent/target)
    local.append((target,q.exists()))
for target,ok in local:
    print(f"{target}\t{\"OK\" if ok else \"MISSING\"}")
print(f"LOCAL_LINKS={len(local)} MISSING={sum(not ok for _,ok in local)}")
PY
=== CWD ===
/home/kara/deployBunker/bunker
  File "<stdin>", line 11
    print(f"{target}\t{\"OK\" if ok else \"MISSING\"}")
                        ^
SyntaxError: unexpected character after line continuation character
=== EXIT_STATUS=1 ===

=== COMMAND ===
sh -c for u in "https://go.dev/" "https://connectrpc.com/" "https://docs.docker.com/engine/security/rootless/" "https://github.com/deployBunker" "https://github.com/deployBunker/bunker/releases/latest/download/install.sh"; do printf "URL %s\\n" "$u"; curl -L -sS -o /dev/null -w "HTTP %{http_code} FINAL %{url_effective}\\n" --max-time 30 "$u"; done
=== CWD ===
/home/kara/deployBunker/bunker
URL https://go.dev/
HTTP 200 FINAL https://go.dev/
URL https://connectrpc.com/
HTTP 200 FINAL https://connectrpc.com/
URL https://docs.docker.com/engine/security/rootless/
HTTP 200 FINAL https://docs.docker.com/engine/security/rootless/
URL https://github.com/deployBunker
HTTP 200 FINAL https://github.com/deployBunker
URL https://github.com/deployBunker/bunker/releases/latest/download/install.sh
HTTP 200 FINAL https://release-assets.githubusercontent.com/github-production-release-asset/1282867910/81fabce2-d73d-468e-81d8-d66f539e3088?sp=r&sv=2018-11-09&sr=b&spr=https&se=2026-11-15T06%3A46%3A16Z&rscd=attachment%3B+filename%3Dinstall.sh&rsct=application%2Foctet-stream&skoid=96c2d410-5711-43a1-aedd-ab1947aa7ab0&sktid=398a6654-997b-47e9-b12b-9515b896b4de&skt=2026-11-15T05%3A45%3A20Z&ske=2026-11-15T06%3A46%3A16Z&sks=b&skv=2018-11-09&sig=PXwU4Pdoov8XVSlynFvVIbwroU4q4CYobuIxM5VQ%2BFY%3D&jwt=eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzI1NiJ9.eyJpc3MiOiJnaXRodWIuY29tIiwiYXVkIjoicmVsZWFzZS1hc3NldHMuZ2l0aHVidXNlcmNvbnRlbnQuY29tIiwia2V5Ijoia2V5MSIsImV4cCI6MTc5NDcyMzE1MywibmJmIjoxNzk0NzIyODUzLCJwYXRoIjoicmVsZWFzZWFzc2V0cHJvZHVjdGlvbi5ibG9iLmNvcmUud2luZG93cy5uZXQifQ.uZj1v-3Mh4b_iNMi_oRGlOeOSwzfWoOJsLhj-PTmYM0&response-content-disposition=attachment%3B%20filename%3Dinstall.sh&response-content-type=application%2Foctet-stream
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c curl -fsSL --max-time 60 https://github.com/deployBunker/bunker/releases/latest/download/bunkerd-linux-amd64 -o /home/kara/.hermes/stand-in/readme/bunker/release/bunkerd-linux-amd64 && chmod +x /home/kara/.hermes/stand-in/readme/bunker/release/bunkerd-linux-amd64 && /home/kara/.hermes/stand-in/readme/bunker/release/bunkerd-linux-amd64 --version
=== CWD ===
/home/kara/deployBunker/bunker
bunkerd 0.3.0
  commit:     de286fdc
  built:      2026-11-09T05:16:34Z
  caps:       isolation-grant
  go version: go1.26.10
  platform:   linux/amd64
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c curl -fsSL --max-time 60 https://github.com/deployBunker/bunker/releases/latest/download/bunker-linux-amd64 -o /home/kara/.hermes/stand-in/readme/bunker/release/bunker-linux-amd64 && chmod +x /home/kara/.hermes/stand-in/readme/bunker/release/bunker-linux-amd64 && /home/kara/.hermes/stand-in/readme/bunker/release/bunker-linux-amd64 --version
=== CWD ===
/home/kara/deployBunker/bunker
bunker 0.3.0
  commit:     de286fdc
  built:      2026-11-09T05:16:34Z
  go version: go1.26.10
  platform:   linux/amd64
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c curl -fsSL --max-time 60 https://github.com/deployBunker/bunker/releases/latest/download/SHA256SUMS -o /home/kara/.hermes/stand-in/readme/bunker/release/SHA256SUMS && cd /home/kara/.hermes/stand-in/readme/bunker/release && sha256sum -c SHA256SUMS --ignore-missing
=== CWD ===
/home/kara/deployBunker/bunker
bunker-linux-amd64: OK
bunkerd-linux-amd64: OK
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c grep -nE "^bunker status|^bunker (connect|use|spawn|list|info|env|exec|run|mount|tunnel|ssh|cp|deploy|systemd|metrics|heartbeat|destroy|audit|registry|version|stop|start|restart|resource-events|renew|homes|linger|reap|umount|pull|guard|host-provision|key|operator|agent-tools|toolchain-check|surface|subid-migrate|paths|iobattery)" README.md | sort | uniq -c
=== CWD ===
/home/kara/deployBunker/bunker
      1 1169:bunker resource-events abc12345            # memory.events + pids.events + peaks
      1 1323:bunker exec a -- sh -c 'cp build.tar /srv/bunker-share/a/build.tar'
      1 1324:bunker exec b -- cp /srv/bunker-share/a/build.tar ./build.tar
      1 1337:bunker connect     Register a bunkerd server (--tls self-signed pins the daemon's
      1 1339:bunker use         Select the active server
      1 1340:bunker status      Show server status (CPU/memory/disk/uptime)
      1 1341:bunker spawn       Create a new agent (--image-spec <file> for package-add image customization)
      1 1342:bunker list        List agents
      1 1343:bunker info        Show agent details
      1 1344:bunker env         Manage agent environment variables
      1 1345:bunker exec        Run a command inside an agent
      1 1346:bunker run         Run a command in an agent's environment (with --detach for background)
      1 1347:bunker mount       Mount agent filesystem via SSHFS
      1 1348:bunker tunnel      Forward agent Docker socket
      1 1349:bunker ssh         Open an interactive SSH session into an agent
      1 1350:bunker cp          Copy a file into an agent's environment (push-only; see
      1 1352:bunker deploy      Deploy a directory into an agent's environment
      1 1353:bunker systemd     Manage the bunkerd systemd service (install/uninstall/status)
      1 1354:bunker metrics     Show resource usage
      1 1355:bunker heartbeat   Extend agent TTL
      1 1356:bunker destroy     Tear down an agent (removes the local key unless --keep-key)
      1 1357:bunker audit       Inspect the audit trail (verify / list / export / status — see docs/audit.md)
      1 1358:bunker registry    Maintain the durable agent registry (compact)
      1 1359:bunker version     Print version/commit/build metadata (also --version)
      1 1360:bunker stop        Pause an agent without destroying it (start/restart resume it)
      1 1361:bunker start       Resume a stopped agent
      1 1362:bunker restart     Stop + start in one call and reset the heartbeat TTL
      1 1363:bunker resource-events  Show an agent's resource-ceiling events and observed peaks
      1 1366:bunker renew       Renew an agent under its STABLE identity (destroy + re-spawn the
      1 1368:bunker homes       Inspect orphaned agent home directories (prune removes the stale ones)
      1 1369:bunker linger      Inspect the systemd linger directory (prune removes the stale entries)
      1 1370:bunker reap        One-pass residue scan: orphan AppArmor profiles, agent homes,
      1 1373:bunker umount      Unmount a mountpoint (idempotent, clears stranded mounts)
      1 1374:bunker pull        Copy a file FROM an agent's environment (cp is push-only);
      1 1377:bunker guard       Do-not-build guard for SSHFS mounts (check/install — see docs/both-ways.md)
      1 1378:bunker host-provision  Provision the per-agent isolation boundary on this host
      1 1381:bunker key         Manage API keys and the JWT signing secret (master only):
      1 1387:bunker operator    Manage operator identities and their RBAC roles (admin
      1 1392:bunker agent-tools  Probe the agent (not the client) for the executables the
      1 1405:bunker toolchain-check  Report a DECLARED build-toolchain CLASS present or
      1 1423:bunker surface     Manage the agent's toolsd socket surface (systemd user
      1 1427:bunker subid-migrate  Rewrite overlapping subordinate-id (subuid/subgid)
      1 1430:bunker paths       Print every resolved secret/state path (locations and
      1 1433:bunker iobattery   Run the I/O measurement battery (BFS-057) and emit a JSON
      1 226:bunker connect --name bunker-mvp http://78.46.173.180:18080 --token <your-demo-token>
      1 227:bunker status
      1 228:bunker spawn --server bunker-mvp --ttl 1h demo-agent
      1 473:bunker spawn build-1 --ttl 2h --preset hardened
      1 503:bunker operator create kelly --role operator
      1 509:bunker operator create agent-77 --role agent:self --ttl-seconds 2592000
      1 512:bunker operator list
      1 513:bunker operator list --agent-id agent-77
      1 514:bunker operator delete kelly
      1 546:bunker connect --tls self-signed https://bunker-host:9090 --token <token>
      1 597:bunker connect --tls-insecure https://throwaway:9090
      1 840:bunker systemd install --binary /usr/local/bin/bunkerd --config /etc/bunkerd/config.yaml --user root
      1 841:bunker systemd status
      1 867:bunker connect --tls self-signed https://bunker-host:9090 --token your-master-token-here
      1 875:bunker spawn --server bunker-host --cpu 2.0 --memory 4294967296 --ttl 6h
      1 891:bunker spawn --server bunker-host --image-spec spec.json --ttl 6h
      1 931:bunker list
      1 934:bunker exec abc12345 --server bunker-host -- docker run --rm alpine echo hello
      1 937:bunker mount abc12345 --server bunker-host /mnt/my-agent
      1 940:bunker tunnel abc12345 --server bunker-host
      1 944:bunker info abc12345
      1 947:bunker env set abc12345 --server bunker-host KEY=VALUE
      1 948:bunker env get abc12345 --server bunker-host KEY
      1 951:bunker heartbeat abc12345 --server bunker-host
      1 954:bunker destroy abc12345 --server bunker-host
      1 956:bunker destroy abc12345 --server bunker-host --keep-key
      1 958:bunker destroy abc12345 --server bunker-host --archive=false
      1 967:bunker stop abc12345 --server bunker-host
      1 970:bunker start abc12345 --server bunker-host
      1 973:bunker restart abc12345 --server bunker-host
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c git -C /home/kara/deployBunker/bunker ls-files README.md docs | wc -l
=== CWD ===
/home/kara/deployBunker/bunker
618
=== EXIT_STATUS=0 ===

=== COMMAND ===
sh -c grep -nE "^## |^### " README.md
=== CWD ===
/home/kara/deployBunker/bunker
14:## What is Bunker?
36:## Features
92:## Quick Start
94:### Run it locally (recommended first path)
207:### Or use the hosted demo (optional)
233:### Prerequisites
254:### Install
360:### Configure
718:### Provision host isolation before spawning
776:### Run the daemon
862:### Use the CLI
1053:## Architecture
1077:### Services
1111:## Resource Limits
1161:### Did the agent hit its ceiling? (resource events)
1197:## Can one agent see another? (security model)
1217:## Agent isolation (`/tmp` and cross-agent exchange)
1330:## CLI Commands
1437:### Pulling files
1452:### Exit codes
1475:## CLI config & path overrides (DF-BUNKER-16)
1514:## Tech Stack
1529:## Development
1567:### E2E battery inputs
1588:### Who owns the daemon (standalone vs coexist)
1618:### CLI-state isolation
1632:### Running against a deployed daemon
1659:### Deploying a build for the E2E gate
1679:### Quality Gates
1685:## License
=== EXIT_STATUS=0 ===


=== COMMAND ===
date -u +%Y-%m-%dT%H:%M:%SZ
=== CWD ===
/home/kara/deployBunker/bunker
2026-11-15T06:14:43Z
=== EXIT_STATUS=0 ===

=== COMMAND ===
git -C /home/kara/deployBunker/bunker rev-parse HEAD
=== CWD ===
/home/kara/deployBunker/bunker
8b4100a274e1c56ff7ab2ba8a8ba069a8c27ef31
=== EXIT_STATUS=0 ===

=== COMMAND ===
bash e2e-full-battery.sh --self-test
=== CWD ===
/home/kara/deployBunker/bunker
==========================================
 BUNKER END-TO-END TEST BATTERY
 Sun Nov 15 01:14:43 AM -05 2026
==========================================

=== 0. Self-test (no root, no side effects) ===
  [capture] synthetic failing command failed (exit=7)
  ✓ run_capture surfaces failing-command output (exit=7)
  ✓ ERR trap names failing line + command (line=930 cmd=false)
  ✓ preflight would refuse a non-root uid (rc=1, reason printed)
  ✓ token refusal would refuse an unauthenticated connect (rc=0, reason printed)
  ✓ token refusal accepts a successful connect (rc=0)
  ✓ bin_commit_verdict: short/full SHA prefixes MATCH both ways
  ✓ bin_commit_verdict: two different real SHAs are a MISMATCH
  ✓ bin_commit_verdict: unknown/dev/empty commit and missing HEAD are SKIP (never MISMATCH)
  ✓ bin_commit_verdict: case/whitespace normalized before compare
  ✓ bin_commit_verdict: a <7-char prefix never MATCHes (not a real git abbreviation)
  ✓ isolated CLI state: the operator's config is byte-identical after a CLI invocation (HOME=/tmp/bunker-battery-selftest-op-H5oK1z)
  ✓ isolated CLI state: the CLI wrote config.yaml under the battery's own state dir (/var/tmp/hermes-scratch/bunker-battery-cli-pLsKXf), not the operator's
  ✓ bcli pins ONE stable state dir for every call (BUNKER_HOME=HOME=/var/tmp/hermes-scratch/bunker-battery-cli-pLsKXf)
  ✓ explicit inputs honored: BUNKERD_REST_ADDR=:18080 + BUNKER_TOKEN exported → REST :18080, token source=env
  ✓ unset BUNKER_TOKEN resolves to token source=default (not silently inherited)
  ✓ the nested suite is handed its own isolated ports in both modes (:29092 :28082)
  ✓ no production/battery port is handed to the nested suite
  ✓ section 15 has no bare bcli host-provision calls and uses gap075_bcli (4 helper call site(s))
  ✓ no bare bcli destroy calls remain (every teardown destroy carries || true or run_capture — INT-CI-049)
  ✓ the single nested-suite invocation carries explicit non-production ports (line 2474)
  [capture] synthetic failing nested suite failed (exit=1)
  ✓ a nested suite exiting non-zero flips section 12 to FAIL (FAIL: the nested suite exited 1 (tally PASS: 16 / FAIL: 14))
  ✓ section 12 reads the nested suite's OWN tally lines (16 14), not a grep of ✓/✗ and the word PASS
  ✓ a green nested suite reports PASS with its own tally (PASS: the nested suite exited 0 with tally PASS: 21 / FAIL: 0)
  ✓ a nested suite that printed no tally is UNVERIFIED (FAIL), never a silent pass
  ✓ remove_own_state_dir REFUSES a dir that is not the harness's own scratch (rc=1, the config file is untouched)
  ⚠ refusing to remove '/tmp/bunker-battery-cli-wsGCvX' — not a scratch dir this harness created (nothing was deleted; operator CLI config: /home/kara/.bunker/config.yaml)
  ✗ the harness's own scratch dir was not removed by remove_own_state_dir
  ✓ regression-tests.sh removes no path under /root (no operator CLI config deletion)
  ✓ regression-tests.sh pins BOTH BUNKER_HOME and HOME to its own scratch dir (standalone included)
  ✓ regression-tests.sh funnels its single pkill bunkerd through kill_stray_bunkerd (never a systemd-managed daemon)
  ✓ every mutating call site carries an explicit --server "$BATTERY_TARGET_NAME" (defense in depth, GAP-093 binding contract)
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
=== EXIT_STATUS=1 ===

=== COMMAND ===
HOME=/home/kara/.hermes/stand-in/readme/bunker/fresh-home BUNKER_HOME=/home/kara/.hermes/stand-in/readme/bunker/fresh-home sh scripts/install.sh --dry-run
=== CWD ===
/home/kara/deployBunker/bunker
install.sh: installing bunker latest for linux/amd64
install.sh: note: /usr/local/bin is not writable by kara — falling back to /home/kara/.hermes/stand-in/readme/bunker/fresh-home/.local/bin (override with --dir)
install.sh: target directory: /home/kara/.hermes/stand-in/readme/bunker/fresh-home/.local/bin
install.sh: dry-run: release source https://github.com/deployBunker/bunker/releases (version latest)
install.sh: dry-run: would download https://github.com/deployBunker/bunker/releases/latest/download/SHA256SUMS
install.sh: dry-run: would download https://github.com/deployBunker/bunker/releases/latest/download/bunker-linux-amd64
install.sh: dry-run: would download https://github.com/deployBunker/bunker/releases/latest/download/bunkerd-linux-amd64
install.sh: dry-run: would verify both binaries against SHA256SUMS, then install them into /home/kara/.hermes/stand-in/readme/bunker/fresh-home/.local/bin
install.sh: dry-run: would install bunker -> /home/kara/.hermes/stand-in/readme/bunker/fresh-home/.local/bin/bunker (mode 0755)
install.sh: dry-run: would install bunkerd -> /home/kara/.hermes/stand-in/readme/bunker/fresh-home/.local/bin/bunkerd (mode 0755)
install.sh: dry-run complete: nothing was downloaded, built or written
=== EXIT_STATUS=0 ===

=== COMMAND ===
bash regression-tests.sh --help
=== CWD ===
/home/kara/deployBunker/bunker
══════════════════════════════════════════════
  Bunker Regression Test Suite
══════════════════════════════════════════════

── 1. Prerequisites ──
  ⚠ NOT killing bunkerd processes: systemd manages the running bunkerd (MainPID 905538) — this suite only manages what it creates
  ⚠ NOT stopping/disabling stale agent dockerd units: systemd manages the running bunkerd (MainPID 905538) — this suite only manages what it creates
  ⚠ NOT sweeping /run/bunker/* and /etc/bunkerd/ssh/*: systemd manages the running bunkerd (MainPID 905538) — this suite only manages what it creates
  ⚠ NOT removing stale bunker-docker-* user units: systemd manages the running bunkerd (MainPID 905538) — this suite only manages what it creates
  [0;32m✓[0m bunkerd binary exists
  [0;32m✓[0m bunker CLI binary exists
  [0;31m✗[0m running as root ([ "$(id -u)" = "0" ])

── 2. Server startup ──
  [0;32m✓[0m bunkerd started (PID 2401827)
regression-tests.sh: line 793: /var/log/bunkerd-regression.log: Permission denied
  [0;31m✗[0m bunkerd EXITED AT STARTUP (PID 2401827) — no recognised refusal line in the log; see the tail below
  --- last 40 lines of /var/log/bunkerd-regression.log ---
2026/09/20 21:27:22 [karaHermes-mde-7840hs/zZjhadaurY-000002] "POST http://127.0.0.1:29090/bunker.v1.Bunkerd/SpawnAgent HTTP/1.1" from 127.0.0.1:57916 - 500 155B in 189.14µs
{"time":"2026-09-20T21:27:22.729843159-05:00","level":"INFO","msg":"allocated port range","agent_id":"f495d286","range":"20200-20299"}
{"time":"2026-09-20T21:27:22.729872934-05:00","level":"INFO","msg":"spawning agent","agent_id":"f495d286"}
{"time":"2026-09-20T21:27:22.72988151-05:00","level":"INFO","msg":"creating user","username":"bunker-f495d286"}
{"time":"2026-09-20T21:27:22.884104685-05:00","level":"INFO","msg":"agent isolation group membership ensured","agent_id":"f495d286","group":"bunker-agents"}
{"time":"2026-09-20T21:27:22.900011172-05:00","level":"INFO","msg":"shared scratch ready","agent_id":"f495d286"}
{"time":"2026-09-20T21:27:22.905593015-05:00","level":"INFO","msg":"private /tmp instance ready","agent_id":"f495d286"}
{"time":"2026-09-20T21:27:22.905624964-05:00","level":"INFO","msg":"generating SSH keypair","keyfile":"/tmp/bunker-key-f495d286"}
{"time":"2026-09-20T21:27:22.909315677-05:00","level":"INFO","msg":"setting up authorized_keys","user":"bunker-f495d286"}
{"time":"2026-09-20T21:27:22.915610991-05:00","level":"INFO","msg":"found host SSH public key, appending to agent authorized_keys","path":"/home/kara/.ssh/id_ed25519.pub","agent_user":"bunker-f495d286"}
{"time":"2026-09-20T21:27:22.915666033-05:00","level":"INFO","msg":"provisioned host SSH key into agent authorized_keys","agent_user":"bunker-f495d286"}
{"time":"2026-09-20T21:27:22.918355111-05:00","level":"INFO","msg":"persisted SSH private key","path":"/etc/bunkerd/ssh/f495d286"}
{"time":"2026-09-20T21:27:22.91837609-05:00","level":"INFO","msg":"starting rootless dockerd","unit":"bunker-docker-f495d286","sock":"/run/bunker/f495d286/docker.sock"}
{"time":"2026-09-20T21:27:22.918384836-05:00","level":"INFO","msg":"spawn entering stage","agent_id":"f495d286","stage":"rootless-install"}
{"time":"2026-09-20T21:27:22.930334999-05:00","level":"INFO","msg":"installed rootlesskit apparmor profile","profile":"home.bunker-f495d286.bin.rootlesskit"}
{"time":"2026-09-20T21:27:22.933664913-05:00","level":"INFO","msg":"installing rootless docker","user":"bunker-f495d286"}
{"time":"2026-09-20T21:27:27.035719974-05:00","level":"WARN","msg":"rootless installer hit the unit-not-found race; reloading the user manager and retrying once","user":"bunker-f495d286","unit":"user@1003.service","output":"# Installing stable version 29.8.1 | # Executing docker rootless install script, commit: 2b32480025b223ebfddae9a3a8bef09027680f53 | % Total % Received % Xferd Average Speed Time Time Time Current | Dload Upload Total Spent Left Speed | 0 0 0 0 0 0 0 0 0 80 82.06M 80 66.15M 0 0 66.18M 0 00:01 00:01 66.15M 100 82.06M 100 82.06M 0 0 71.87M 0 00:01 00:01 66.15M 100 82.06M 100 82.06M 0 0 71.86M 0 00:01..."}
{"time":"2026-09-20T21:27:27.547806153-05:00","level":"INFO","msg":"destroying agent","agent_id":"regr-alpha"}
{"time":"2026-09-20T21:27:27.5589215-05:00","level":"WARN","msg":"agent container rm failed (continuing)","agent_id":"regr-alpha","error":"exit status 1","output":"failed to connect to the docker API at unix:///run/bunker/regr-alpha/docker.sock; check if the path is correct and if the daemon is running: dial unix /run/bunker/regr-alpha/docker.sock: connect: no such file or directory"}
{"time":"2026-09-20T21:27:27.558956985-05:00","level":"WARN","msg":"agent container cleanup incomplete","agent_id":"regr-alpha","error":"remove agent container bunker-regr-alpha: exit status 1 (output: failed to connect to the docker API at unix:///run/bunker/regr-alpha/docker.sock; check if the path is correct and if the daemon is running: dial unix /run/bunker/regr-alpha/docker.sock: connect: no such file or directory)"}
{"time":"2026-09-20T21:27:27.559004624-05:00","level":"INFO","msg":"removed user slice drop-in","slice":"user-1001.slice"}
{"time":"2026-09-20T21:27:27.97535431-05:00","level":"INFO","msg":"rootless installer succeeded on the daemon-reload retry","user":"bunker-f495d286"}
{"time":"2026-09-20T21:27:27.978851103-05:00","level":"INFO","msg":"spawn entering stage","agent_id":"f495d286","stage":"dockerd-start"}
{"time":"2026-09-20T21:27:28.387582676-05:00","level":"WARN","msg":"direct dockerd stop failed","unit":"bunker-docker-regr-alpha","error":"pgrep dockerd for user bunker-regr-alpha: exit status 1 (output: )"}
{"time":"2026-09-20T21:27:28.390168984-05:00","level":"WARN","msg":"systemctl stop failed (may not exist)","unit":"bunker-docker-regr-alpha","error":"exit status 1","output":"Failed to connect to user scope bus via local transport: $DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not defined (consider using --machine=<user>@.host --user to connect to bus of other user)\n"}
{"time":"2026-09-20T21:27:28.454367101-05:00","level":"INFO","msg":"archiving agent home before delete","agent_id":"regr-alpha","policy":"archive","home":"/home/bunker-regr-alpha","archive_dir":"/var/backups/bunker"}
{"time":"2026-09-20T21:27:28.750515063-05:00","level":"INFO","msg":"dockerd ready","user":"bunker-f495d286","sock":"/run/bunker/f495d286/docker.sock"}
{"time":"2026-09-20T21:27:29.068138774-05:00","level":"INFO","msg":"wrote user slice drop-in","slice":"user-1003.slice","path":"/etc/systemd/system/user-1003.slice.d/50-bunker.conf"}
{"time":"2026-09-20T21:27:29.851026132-05:00","level":"INFO","msg":"spawn entering stage","agent_id":"f495d286","stage":"session-probe"}
{"time":"2026-09-20T21:27:30.037035422-05:00","level":"INFO","msg":"session probe succeeded","agent_id":"f495d286","stage":"session-probe","attempts":1}
{"time":"2026-09-20T21:27:30.037061991-05:00","level":"INFO","msg":"agent registered","agent_id":"f495d286","total":3}
{"time":"2026-09-20T21:27:30.040930182-05:00","level":"INFO","msg":"agent spawned successfully","agent_id":"f495d286"}
2026/09/20 21:27:30 [karaHermes-mde-7840hs/zZjhadaurY-000003] "POST http://127.0.0.1:29090/bunker.v1.Bunkerd/SpawnAgent HTTP/1.1" from 127.0.0.1:57922 - 200 329B in 7.312728011s
2026/09/20 21:27:30 [karaHermes-mde-7840hs/zZjhadaurY-000005] "POST http://127.0.0.1:29090/bunker.v1.Bunkerd/GetAgentKey HTTP/1.1" from 127.0.0.1:57922 - 200 312B in 684.297µs
{"time":"2026-09-20T21:27:38.822054078-05:00","level":"INFO","msg":"agent home archived and verified","agent_id":"regr-alpha","policy":"archive","archive_path":"/var/backups/bunker/bunker-regr-alpha-20260921T022728Z.tar.gz"}
{"time":"2026-09-20T21:27:39.014995701-05:00","level":"INFO","msg":"agent unregistered","agent_id":"regr-alpha","total":2}
{"time":"2026-09-20T21:27:39.015015037-05:00","level":"INFO","msg":"freed port range","agent_id":"regr-alpha"}
{"time":"2026-09-20T21:27:39.018455676-05:00","level":"INFO","msg":"agent destroyed","agent_id":"regr-alpha"}
2026/09/20 21:27:39 [karaHermes-mde-7840hs/zZjhadaurY-000004] "POST http://127.0.0.1:29090/bunker.v1.Bunkerd/DestroyAgent HTTP/1.1" from 127.0.0.1:57936 - 200 47B in 11.471508574s
{"time":"2026-09-20T21:27:39.025699507-05:00","level":"INFO","msg":"shutting down","signal":"terminated"}
  --- end of /var/log/bunkerd-regression.log ---

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
=== EXIT_STATUS=3 ===

=== COMMAND ===
python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/check-md-anchors.py README.md
=== CWD ===
/home/kara/deployBunker/bunker
BROKEN README.md:582: #the-tls-insecure-knob-2026-09
BROKEN README.md:657: #the-tls_insecure-knob-2026-09
anchors_checked=16 broken=2
=== EXIT_STATUS=1 ===

=== COMMAND ===
python3 /home/kara/.hermes/stand-in/readme/bunker/link-check.py
=== CWD ===
/home/kara/deployBunker/bunker
bunker-hero.png	OK
LICENSE	OK
docs/performance.md	OK
docs/mount-drivers.md	OK
specs/agent-tmp-isolation.md	OK
specs/agent-lifecycle.md	OK
docs/egress-policy.md	OK
specs/container-mode.md	OK
docs/threat-model.md	OK
docs/integration.md	OK
CHANGELOG.md	OK
docs/threat-model.md	OK
specs/api.md	OK
docs/presets/README.md	OK
specs/configuration.md	OK
docs/audit.md	OK
specs/api.md	OK
specs/safety-presets.md	OK
internal/agent/SKILL.md	OK
docs/resource-events.md	OK
specs/agent-lifecycle.md	OK
specs/agent-lifecycle.md	OK
specs/agent-lifecycle.md	OK
specs/agent-tmp-isolation.md	OK
specs/agent-tmp-isolation.md	OK
specs/agent-lifecycle.md	OK
docs/threat-model.md	OK
specs/agent-tmp-isolation.md	OK
docs/both-ways.md	OK
go.mod	OK
LICENSE	OK
NOTICE	OK
CLA.md	OK
LOCAL_LINKS=33 MISSING=0
=== EXIT_STATUS=0 ===

=== COMMAND ===
grep -nE "newest release tag predates|TLSRefusal|exit 2|exit code 2|per-command exit codes|EXIT CODE IS THE GATE" README.md
=== CWD ===
/home/kara/deployBunker/bunker
1454:The CLI has no per-command exit codes of its own: `0` on success, `1` on any
=== EXIT_STATUS=0 ===

=== COMMAND ===
grep -nE "^## |^### " README.md
=== CWD ===
/home/kara/deployBunker/bunker
14:## What is Bunker?
36:## Features
92:## Quick Start
94:### Run it locally (recommended first path)
207:### Or use the hosted demo (optional)
233:### Prerequisites
254:### Install
360:### Configure
718:### Provision host isolation before spawning
776:### Run the daemon
862:### Use the CLI
1053:## Architecture
1077:### Services
1111:## Resource Limits
1161:### Did the agent hit its ceiling? (resource events)
1197:## Can one agent see another? (security model)
1217:## Agent isolation (`/tmp` and cross-agent exchange)
1330:## CLI Commands
1437:### Pulling files
1452:### Exit codes
1475:## CLI config & path overrides (DF-BUNKER-16)
1514:## Tech Stack
1529:## Development
1567:### E2E battery inputs
1588:### Who owns the daemon (standalone vs coexist)
1618:### CLI-state isolation
1632:### Running against a deployed daemon
1659:### Deploying a build for the E2E gate
1679:### Quality Gates
1685:## License
=== EXIT_STATUS=0 ===

=== COMMAND ===
sed -n "720,775p;1437,1475p" README.md
=== CWD ===
/home/kara/deployBunker/bunker
**Required for private `/tmp`:** building or starting `bunkerd` does not install
its host-side SSH/PAM configuration. A fresh unprovisioned host leaves agent SSH
sessions sharing the host `/tmp`. Run these commands on the **daemon host**, as
root, from the checkout where `make build` produced both binaries. This is local
host administration, not an RPC to the server selected by `bunker connect`.

```bash
# Inspect the plan first; no host changes without --apply.
sudo ./bunker host-provision --daemon-binary "$(pwd)/bunkerd"
# After reviewing the plan, install the host-side boundary.
sudo ./bunker host-provision --daemon-binary "$(pwd)/bunkerd" --apply
# Inspect the resulting host configuration (add --json for automation).
sudo ./bunker host-provision --daemon-binary "$(pwd)/bunkerd" --status
```

Use the **same daemon binary** when starting the service below. For an installed
service, substitute its actual executable path in `--daemon-binary` (the default
is `/usr/local/bin/bunkerd`). Upgrade/restart an existing service before applying
new host isolation rules: an older daemon can create agents without the required
`bunker-agents` membership, and the fail-closed PAM rules then deny their SSH
sessions (including exec, cp and mount).

The current installer requires the daemon to prove the spawn-side isolation
grant by **reporting the `isolation-grant` capability** in its own version block
(printed by both `bunkerd --version` and `bunkerd version`):

```text
$ ./bunkerd --version
bunkerd 0.1.4
  commit:     923c4be
  built:      2026-09-18T23:46:15Z
  caps:       isolation-grant
  go version: go1.26.5
  platform:   linux/amd64
```

A daemon that does not report `caps: isolation-grant` is refused at **any**
version, because a version number cannot prove a capability: the newest release
tag predates the isolation implementation, and a plain `go build` reports the
package version whatever the tree contains — a binary built from a tagged
release self-reports that release's version (e.g. `0.1.4`) while carrying no
grant. The **0.1.4** version floor is a legacy minimum kept as a
secondary check (it still applies when the capability *is* reported) and is not a
capability guarantee. Build CLI and daemon from the same current checkout.
An absent, unreadable, timed-out or unparseable daemon version produces an
**UNKNOWN warning**, not proof of compatibility. Resolve that warning rather
than bypassing it with `--allow-daemon-skew`.

`host-provision --status` inspects static host configuration; it is not an
end-to-end SSH-session test. After starting/restarting the matching daemon and
connecting, run `bunker status` **before spawning**. Require the `/tmp:` line to
report `private`; `HOST-SHARED`, `unknown`, or `not reported` is not evidence of isolation.
Do not put isolation-sensitive workloads on that host until the setup and
running daemon agree. See [Agent isolation](#agent-isolation-tmp-and-cross-agent-exchange)
for the mechanisms and teardown precautions.

### Pulling files

`bunker cp` and `bunker deploy` are push-only: they copy a LOCAL source into an
agent, so `<agent-id>:/path` is the *destination* form and a pull-shaped
invocation — `<agent-id>:<path>` as the first argument, a local path as the
second — is refused. The refusal names the three remedies below rather than
only the destination format.

The reverse direction is `bunker pull <agent-id> <remote-path> [local-dir]`
(`--recursive` copies a directory tree). The equivalent over scp uses the key
spawn saved for the agent at `~/.config/bunker/keys/<agent-id>`:
`scp -i ~/.config/bunker/keys/<agent-id> bunker-<agent-id>@<host>:<remote-path> <local-dir>`.
The other read path is the sshfs mount (`bunker mount <agent-id>`), compared
against the verb layer in [docs/both-ways.md](docs/both-ways.md).

### Exit codes

The CLI has no per-command exit codes of its own: `0` on success, `1` on any
error (printed as `bunker: <error>` on stderr), plus the two ssh-style
exceptions below — a propagated remote exit code from `exec`/`run`, and a
reported not-found outcome from `destroy`.

| Situation | Exit code | Traceable to |
| --- | --- | --- |
| Any command succeeds | `0` | `cmd/bunker/main.go:22` (nil error ⇒ no explicit exit) |
| Any command fails (`spawn`, `list`, `info`, `heartbeat`, `cp`, `mount`, `audit`, `registry`, …) | `1`, with `bunker: <error>` on stderr | `cmd/bunker/main.go:28-29` |
| `bunker exec <id> <cmd>` — the remote command exits non-zero | the remote code verbatim (e.g. `bunker exec … -- sh -c 'exit 7'` ⇒ `7`), printed silently ssh-style | `internal/cli/exec.go:280-281` → `cmd/bunker/main.go:25-27` |
| `bunker run <id> <cmd>` — the remote command exits non-zero | same as `exec` (verbatim remote code) | `internal/cli/run.go:231-232` → `cmd/bunker/main.go:25-27` |
| `bunker destroy <id>` — agent destroyed | `0` | `internal/cli/destroy.go:94` |
| `bunker destroy <id>` — agent **not found** (never spawned, or already destroyed): a REPORTED outcome, not an error | `0` (prints `Agent <id> not found.`) | `internal/cli/destroy.go:80-82` (RPC `CodeNotFound`) and `internal/cli/destroy.go:90-92` (in-band `not_found`) |
| `bunker destroy <id>` — real RPC failure (agent may still exist; local key kept) | `1` | `internal/cli/destroy.go:86` |
| Invalid arguments/flags rejected locally (missing positional args, bad agent id, invalid `--ttl`, unknown exec flag) | `1`, with no RPC attempted | `internal/cli/spawn.go:79` (agent id), `internal/cli/spawn.go:107` (`--ttl`), `internal/cli/exec.go:137` (exec flag grammar) |

`bunker exec` and `bunker run` are the only commands that propagate a remote
exit code; a `not_found` destroy is the only failure-shaped outcome that exits
`0` on purpose. Both exceptions are deliberate and documented in
`internal/cli/SKILL.md`.

## CLI config & path overrides (DF-BUNKER-16)
=== EXIT_STATUS=0 ===

=== COMMAND ===
grep -nE "TLSRefusalExitCode|os.Exit|return 2|ExitCode" internal/cli/tls_refusal.go cmd/bunker/main.go
=== CWD ===
/home/kara/deployBunker/bunker
internal/cli/tls_refusal.go:36://     code: TLSRefusalExitCode (2) — distinct from the generic 1 so a
internal/cli/tls_refusal.go:68:	// TLSRefusalExitCode is the process exit code for a refused TLS/trust
internal/cli/tls_refusal.go:75:	TLSRefusalExitCode = 2
internal/cli/tls_refusal.go:180:// (TLSRefusalExitCode) and whether err is one. Any other error reports
internal/cli/tls_refusal.go:185:		return TLSRefusalExitCode, true
internal/cli/tls_refusal.go:191:// must terminate the process with TLSRefusalExitCode instead of the generic
internal/cli/tls_refusal.go:210:// ExitCode is the process exit status for this refusal.
internal/cli/tls_refusal.go:211:func (e *TLSRefusalExitError) ExitCode() int { return TLSRefusalExitCode }
cmd/bunker/main.go:24:		// TLSRefusalExitCode (2), not the generic 1, so a monitoring gate
cmd/bunker/main.go:34:			os.Exit(refusal.ExitCode())
cmd/bunker/main.go:39:			os.Exit(code)
cmd/bunker/main.go:42:		os.Exit(1)
cmd/bunker/main.go:76:	// passes through to the existing %w printing and os.Exit(1) below
=== EXIT_STATUS=0 ===

=== COMMAND ===
grep -nE "exec.go|run.go|destroy.go|spawn.go|main.go" README.md | tail -30
=== CWD ===
/home/kara/deployBunker/bunker
1150:`internal/agent/manager_spawn.go`). Two consequences: **post-spawn container
1461:| Any command succeeds | `0` | `cmd/bunker/main.go:22` (nil error ⇒ no explicit exit) |
1462:| Any command fails (`spawn`, `list`, `info`, `heartbeat`, `cp`, `mount`, `audit`, `registry`, …) | `1`, with `bunker: <error>` on stderr | `cmd/bunker/main.go:28-29` |
1463:| `bunker exec <id> <cmd>` — the remote command exits non-zero | the remote code verbatim (e.g. `bunker exec … -- sh -c 'exit 7'` ⇒ `7`), printed silently ssh-style | `internal/cli/exec.go:280-281` → `cmd/bunker/main.go:25-27` |
1464:| `bunker run <id> <cmd>` — the remote command exits non-zero | same as `exec` (verbatim remote code) | `internal/cli/run.go:231-232` → `cmd/bunker/main.go:25-27` |
1465:| `bunker destroy <id>` — agent destroyed | `0` | `internal/cli/destroy.go:94` |
1466:| `bunker destroy <id>` — agent **not found** (never spawned, or already destroyed): a REPORTED outcome, not an error | `0` (prints `Agent <id> not found.`) | `internal/cli/destroy.go:80-82` (RPC `CodeNotFound`) and `internal/cli/destroy.go:90-92` (in-band `not_found`) |
1467:| `bunker destroy <id>` — real RPC failure (agent may still exist; local key kept) | `1` | `internal/cli/destroy.go:86` |
1468:| Invalid arguments/flags rejected locally (missing positional args, bad agent id, invalid `--ttl`, unknown exec flag) | `1`, with no RPC attempted | `internal/cli/spawn.go:79` (agent id), `internal/cli/spawn.go:107` (`--ttl`), `internal/cli/exec.go:137` (exec flag grammar) |
=== EXIT_STATUS=0 ===

=== COMMAND ===
git status --short
=== CWD ===
/home/kara/deployBunker/bunker
=== EXIT_STATUS=0 ===

=== TRANSCRIPT_SHA256 ===
fcdc2ae1f544424524e60a2533e5023fd9e3baac47a092274ab60b0ee23a135a  /home/kara/.hermes/stand-in/readme/bunker/continuation-20261115T0516Z-transcript.txt
