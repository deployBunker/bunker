# Package: `internal/cli`

## Public API

- `NewConnectCommand()` — `bunker connect SERVER_URL` registers a bunkerd server and stores it in `~/.bunker/config.yaml`. Help documents both ports: `http://localhost:9090 (gRPC) or http://localhost:8080 (REST)` (GAP-013).
- `NewSpawnCommand()` — `bunker spawn [agent-id]` creates an agent and prints a connection bundle. The optional positional `[agent-id]` is an alias for `--agent-id` (DOGFOOD-008): it binds when the flag is empty, giving both errors with `use one or the other`, and any agent ID (flag or positional) is validated LOCALLY against `^[a-z0-9-]{1,64}$` before config load or RPC so typos fail fast without a round-trip; `Args: cobra.MaximumNArgs(1)` rejects extra positionals. Prints a `Creating agent...` progress line immediately before the SpawnAgent RPC (agent creation can take ~20s server-side: useradd, dockerd start, key write) so the user doesn't see a silent wait and Ctrl-C into a half-created agent (GAP-023).
- `NewDestroyCommand()` — `bunker destroy <agent-id>` removes an agent. Maps `connect.CodeNotFound` → `Agent <id> not found.` + exit 0 (idempotent, DOGFOOD-005).
- `NewListCommand()` — `bunker list` lists agents across servers, including a per-agent disk usage column.
- `NewStatusCommand()` — `bunker status` shows the active server's status: disk usage (>90% warning banner), CPU %, Memory used/limit, and real uptime from `ServerInfo` (DOGFOOD-006); `bunker status --all-servers` shows every registered server (MULTI-002). No-server error paths (both single and `--all-servers`) return `no servers configured; run 'bunker connect' first` → exit 1, matching list/spawn/info (GAP-037) — scripts/CI can rely on a non-zero exit in the unconfigured state.
- `NewUseCommand()` — `bunker use <server-name>` switches the active server in the CLI config (MULTI-001).
- `NewExecCommand()` — `bunker exec <agent-id> [--] <command> [args...]` executes commands via the server streaming RPC. Propagates the remote exit code: `bunker exec sh -c 'exit 7'` → CLI exits 7 (DOGFOOD-005).
- `NewRunCommand()` — `bunker run <agent-id> [--] <command> [args...]` runs a one-shot command in the agent (raw exec variant).
- `NewCpCommand()` — `bunker cp <agent-id>:<path> <local-path>` copies a file OUT of an agent via scp (UX-008, DOGFOOD-002).
- `NewSSHCommand()` — `bunker ssh <agent-id> [command...]` opens an interactive SSH session into an agent (GAP-034). Resolves user@host via `resolveUserAtHost` from the sshfs mount, key via `defaultSSHKeyPath` (`~/.bunker/keys/<id>`), flags `--server/--ssh-port/--ssh-key/--ssh-host` matching cp. The GetAgent RPC has a 30s timeout but the session itself is context-free so it lives until the user exits; streams are attached via `cmd.InOrStdin/OutOrStdout/ErrOrStderr`.
- `NewDeployCommand()` — `bunker deploy <local-path> <agent-id>:<path>` copies a file INTO an agent and chowns it to the agent user (UX-008, DOGFOOD-002).
- `NewEnvCommand()` — `bunker env <agent-id>` prints the agent's environment (DOCKER_HOST etc.).
- `NewVersionCommand()` + `PrintVersion(w io.Writer)` — `bunker version` and the root `--version` flag both print the SAME canonical 5-field block (binary name+version, commit, built, go version, platform — UX-005). `PrintVersion` (version.go, GAP-045/c3bf3ee) is the single source of truth shared by both surfaces so they can never diverge; the root command's custom `--version` flag in cmd/bunker/main.go replaced cobra's auto-added `--version` (which rendered a bare one-liner and was absent on `go install`-built binaries). Version/Commit/BuildDate come from the shared `internal/version` package, injected via Makefile LDFLAGS (`-X`) at build time with a debug.ReadBuildInfo fallback (GAP-042) — `commit: unknown` means the binary was built without ldflags (UX-005, DOGFOOD-006). Guard: `TestVersionFlagMatchesVersionSubcommand` (cmd/bunker/main_test.go) asserts byte-identical output between `--version` and the `version` subcommand.
- `NewMetricsCommand()` — `bunker metrics` shows server or per-agent metrics (CPU/memory now real, DOGFOOD-006).
- `NewHeartbeatCommand()` — `bunker heartbeat <agent-id>` extends an agent's TTL.
- `NewInfoCommand()` — `bunker info <agent-id>` shows a single agent record.
- `NewMountCommand()` — `bunker mount <agent-id> [mountpoint]` runs the SSHFS mount command from the spawn response (rewritten for client-local host/key, DOGFOOD-002).
- `NewTunnelCommand()` — `bunker tunnel <agent-id> [local-port]` prints the SSH local-forward command (key path rewritten to `~/.bunker/keys/<id>`, DOGFOOD-002).
- `NewSystemdCommand()` — `bunker systemd install/uninstall/status` for the bunkerd service (visible in `--help` since GAP-033; was `Hidden: true`).
- `NewAuditCommand()` — `bunker audit` group (audit.go, GAP-049/GAP-050) inspects the bunkerd audit trail: an append-only, SHA-256 hash-chained JSONL log of every authenticated RPC (one record per request; file mode 0600; token values never written; rotates at 5 MiB keeping backups `.1`–`.3`). Three subcommands:
  - `bunker audit verify` — local-only hash-chain verification (GAP-049). Checks every record including rotated backups (each record's hash must match its line bytes and chain to the previous record); prints `audit log <path>: OK (N records)` or exits non-zero with the first bad record index on tamper. Has only `--path` (default `/var/log/bunkerd/audit.log`); verification of a remote trail is the daemon host operator's job.
  - `bunker audit list` — matching records as a fixed-width table, oldest first (chain order `.3` → `.2` → `.1` → live file); prints `No audit records match.` when empty; `hash`/`prev_hash` are omitted from the table (they ARE in export). Method column strips the `/bunker.v1.Bunkerd/` prefix; ends with a `Total: N records` line.
  - `bunker audit export` — matching records to stdout as JSONL, one JSON object per line, same keys as the daemon log (`ts, caller, method, remote_addr, agent_id, duration_ms, outcome, summary, hash, prev_hash`). Lossless: `hash`/`prev_hash` preserved so output can be re-verified or replayed; `SetEscapeHTML(false)` keeps it byte-compatible with the on-disk format.
  - Shared query flags (`addAuditQueryFlags`) on list/export: `--server` (server alias → daemon `QueryAudit` RPC; `--path` then ignored), `--agent` (exact `agent_id` match), `--method` (procedure substring, e.g. `SpawnAgent`), `--since`/`--until` (RFC3339, inclusive), `--limit` (max records, keeps the NEWEST matches; 0 = no limit), `--path` (default `/var/log/bunkerd/audit.log`, local mode). Filters are ANDed; empty filters match everything. Local mode reads the log at `--path` plus rotated backups; `--server` mode mirrors `bunker list`'s remote pattern (resolveServer → `newBunkerdClient` → `resolveToken` → `Authorization: Bearer`; 30s context timeout). Records with unparseable `ts` are excluded only when a time filter is set. Local and remote output are byte-identical (both render `audit.Record` values via `queryAuditRecords` → `printAuditTable`/JSON encoder).
- SSH host/key resolution in `sshhost.go` (DOGFOOD-002) — makes cp/deploy/tunnel work from REMOTE clients:
  - `resolveSSHHost(entry, serverProvidedHost, flagValue)` — precedence: `--ssh-host` flag > server-entry URL hostname > server-provided hostname (the server's self-reported hostname is often unresolvable off-host).
  - `resolveUserAtHost(entry, sshfsMount, flagHost)` / `sshHostFromMount` / `sshUserHostFromTunnel` — parse host/user from the spawn bundle.
  - `rewriteTunnelCommand(tunnelCmd, serverHost, resolvedHost, clientKey)` / `rewriteSSHFSMount` / `rewriteDockerHostSsh` — rewrite server-local key paths (`/etc/bunkerd/ssh/<id>`) to the client-local `~/.bunker/keys/<id>`; no-op when hosts are equal.
  - `clientTunnelArgs` injects `-o IdentitiesOnly=yes` right after the `ssh` token if absent (GAP-009/c238df4) — without it a client with a loaded ssh-agent offers every agent key and the server's `MaxAuthTries` kills the connection before the correct `-i` key is tried.
- `ExitError` in `exit_error.go` (DOGFOOD-005) — carries the remote exit code; `main.go` checks it via `errors.As` and exits silently with the remote code (ssh-style), instead of always exiting 1 with `bunker: exit code N`.
- Disk helpers in `disk.go`: `diskWarning(pct)` returns `⚠`/`!` indicators, `formatDisk(used, total)` renders `45% (892GB/2.0TB)`, `diskUsagePercent(used, total)` computes the ratio, `diskAlert(pct)` returns true when >90% (MONITOR-001/002).
- `LoadCLIConfig()` / `SaveCLIConfig(cfg)` — load/save the CLI config file in `~/.bunker/config.yaml`.
- `newBunkerdClient(entry)` / `resolveToken(entry)` — shared HTTP client and token resolution helpers in `client.go`.
- `ServerEntry` struct and `CLIConfig` struct are defined in `config.go`.

## Conventions

- One file per command under `internal/cli/`, matching the command name.
- Commands use `cobra` for parsing and `viper` for environment binding (`BUNKER_TOKEN`).
- Every command loads `CLIConfig`, resolves the active server (or `--server`), and calls the connect-go client.
- Auth token priority: flag `--token`, server entry token, viper `token`, `BUNKER_TOKEN` env var.
- `exec` uses `DisableFlagParsing: true` so Docker flags (`--rm`, `--format`, etc.) pass through; it manually parses `--server` and `--timeout` before the command token, then skips exactly ONE `--` token (GAP-002/b35ad73) — otherwise `exec <id> --server X -- <cmd>` sends `--` AS the command (`sh: 0: Illegal option --`).
- `spawn` binds the positional `[agent-id]` to the agent ID when `--agent-id` is empty and errors when both are given (DOGFOOD-008); agent IDs are validated locally (`^[a-z0-9-]{1,64}$`, `agentIDRe` in spawn.go) as the FIRST step of RunE — before config load or RPC — so validation fires even with no server configured. Prints `Creating agent...` to stdout BEFORE the SpawnAgent RPC (GAP-023) and saves the returned private key to `~/.bunker/keys/<agent-id>` with mode `0600`.
- Every client-side ssh/scp/sshfs arg builder carries `-o IdentitiesOnly=yes` (GAP-009/c238df4): `buildSCPArgs` + cp/deploy ssh args, mount's sshfs args, `clientTunnelArgs` (inject-if-absent), and the spawn bundle's docker-host tunnel (`internal/agent/manager_spawn.go`). This pins the agent's `-i <key>` as the ONLY identity offered, defeating ssh-agent key-order races.
- `connect` REST port is the gRPC/Connect port (e.g., `:9090`) unless TLS is enabled and gRPC/REST are split; the CLI stores the URL as provided.

## Dependencies

- `proto/bunker/v1` and `proto/bunker/v1/bunkerv1connect` — generated RPC client.
- `connectrpc.com/connect` — request/response wrappers.
- `github.com/spf13/cobra` and `github.com/spf13/viper` — CLI framework and env binding.
- `internal/version` — ldflags-injected Version/Commit/BuildDate (DOGFOOD-006).
- Standard library: `context`, `crypto/tls`, `errors`, `fmt`, `net/http`, `os`, `path/filepath`, `strconv`, `time`.

## Test Patterns

- Each command has a `*_test.go` file with table-driven tests for: help output, missing args, no active server, server-not-found, success path, and server-error.
- `disk_test.go` covers `diskWarning`, `formatDisk`, `diskUsagePercent`, `diskAlert` (table-driven, incl. >90% threshold).
- `status_test.go` covers the disk column and the >90% WARNING banner, plus `TestStatusCommand_NoServersConfigured` / `TestStatusCommand_NoServersConfigured_WithAll` (single + `--all-servers` no-server error paths, GAP-037); `use_test.go` covers server switching; `cp_test.go`/`deploy_test.go` cover scp arg construction and chown logic with mocked `exec.Command`.
- `TestSpawnCommand_ProgressLineWithin2s` (spawn_test.go, GAP-023) — blocking-mock server verifies `Creating agent...` is printed within 2s of invocation while the RPC is still blocked (the anti-silent-wait regression test).
- `TestSpawnCommand_PositionalAgentID` (spawn_test.go, DOGFOOD-008) — table-driven over 5 rows: positional binds to the server-observed `AgentId` (mock records `req.Msg.AgentId` in `gotAgentID`); positional + `--agent-id` conflict errors; invalid positional AND invalid `--agent-id` are rejected with the error text containing `[a-z0-9-]{1,64}` while NO server is configured (proving validation fires before config load/RPC); >1 positional hits cobra's `MaximumNArgs(1)` usage error.
- `exec_test.go` verifies `--` passthrough and flag forwarding by inspecting the command-line arguments; `TestExecCommand_FlagSeparator` (x4) covers `--server X -- <cmd>` and `--timeout N -- <cmd>` (GAP-002).
- `exit_error_test.go` / destroy tests cover `CodeNotFound` → `Agent <id> not found.` + exit 0 and `TestExecCommand_ExitCode7` style remote-code propagation (DOGFOOD-005).
- `TestExitCode_NoServersConfigured` (status_test.go, GAP-037) — table-driven exit-code guard over status/list/spawn/info asserting every command exits non-zero with the `bunker connect` hint under `HOME=/tmp/empty` (bare `info` fails ARG validation first, so the table passes it a positional agent id; the invariant is non-zero exit + connect hint).
- `connect_test.go` uses an `httptest.Server` for the `RegisterServer` happy path.
- `sshhost_test.go` covers host resolution precedence, bundle rewriting, and `TestClientTunnelArgs` (4 cases incl. inject-if-absent and no-duplicate — GAP-009).
- `ssh_test.go` covers help, missing args, no server, agent-not-found, missing key, empty sshfs mount, `buildSSHArgs` (with and without remote command), and an end-to-end run with a fake `ssh` on PATH that records its args (GAP-034).
- `client_test.go` verifies `resolveToken` precedence and `newBunkerdClient` TLSInsecure behavior.
- `audit_test.go` (GAP-050) — table-driven filter tests write a fixture audit log to `t.TempDir()` and assert table/JSONL output (agent filter, method+time filters, invalid `--since` error); remote tests (`TestAuditListCommand_Remote`, `TestAuditExportCommand_Remote`) register a mock daemon in a temp HOME config (`t.Setenv("HOME", t.TempDir())` + `SaveCLIConfig` with the httptest server URL) and serve `QueryAudit` over connect; verify tests cover OK / tampered (first-bad-index) / missing-file paths.
- **Any test that calls `SaveCLIConfig`/`LoadCLIConfig` MUST isolate HOME** (`t.Setenv("HOME", t.TempDir())`) — without it, tests CLOBBER the real `~/.bunker/config.yaml` (DOGFOOD-002, mount_test.go regression).
- Use `cobra.Command.SetArgs`, `ExecuteC`, and `ExecuteContext` for non-streaming commands; streaming commands capture stdout/stderr with `os.Pipe` or by overriding `cmd.OutOrStdout`.
- Avoid real network calls; spin up a local connect-go server or httptest where needed.

## Pitfalls

1. **`exec` must disable cobra flag parsing.** Without `DisableFlagParsing: true`, `bunker exec <agent> -- docker run --rm` fails with `unknown flag: --rm`. The command manually parses only `--server` and `--timeout` from the raw args, then treats everything after the first non-flag token as the Docker command. The `--` separator must be SKIPPED after the manual flag loop (GAP-002).
2. **`--help` becomes a raw arg when flag parsing is disabled.** `NewExecCommand` detects `--help`/`--h` in raw args and calls `cmd.Help()` before checking argument counts.
3. **`connect` URL must include the scheme.** Registering `localhost:9090` without `http://` or `https://` produces invalid connect-go client URLs; the command stores the URL exactly as given but callers should pass a full URL.
4. **Token resolution is layered and easy to invert.** The order is: `--token` flag → server entry → viper config → `BUNKER_TOKEN` env. Tests must set each layer independently to verify precedence.
5. **`spawn` writes private keys to disk but does not return the path if writing fails.** The command silently ignores `os.WriteFile` errors for the key file; production callers should verify the key file exists before relying on it. Test cases should mock the home directory or use `t.TempDir`.
6. **`bunker systemd install` requires root.** The CLI command does not check for root; `InstallService` returns `permission denied` if run as a non-root user. The test suite redirects `UnitPath`/`LogrotatePath` to temp directories to avoid needing root.
7. **The server's self-reported hostname is often unresolvable from remote clients.** Never trust the spawn bundle's host/key paths verbatim off-host — run them through `sshhost.go` rewriting (DOGFOOD-002), or cp/deploy/tunnel will fail with connection refused / missing key.
8. **A loaded ssh-agent can kill client connections before the right key is offered.** Without `IdentitiesOnly=yes`, ssh/scp offer every agent key first; the server's `MaxAuthTries` then closes the connection (`scp: Connection closed`, exit 255) before the correct `-i` key is tried. All client ssh paths now carry it (GAP-009) — if you add a new ssh invocation, include `-o IdentitiesOnly=yes`.
9. **`bunker audit` output ordering and filter semantics have traps.** `--limit` selects the NEWEST matching records but output is always printed oldest first (chain order), so a `--limit 20` table is not the "last 20 lines" of the log — it's the last 20 matches, re-ordered. Records whose `ts` fails RFC3339 parsing are silently excluded ONLY when `--since`/`--until` is set (they appear otherwise). `--path` is ignored whenever `--server` is set — the daemon reads its own configured `audit.path`, so don't expect `--path` to point a remote query at a different file. Invalid timestamps are rejected with `invalid --since/--until %q (want RFC3339...)` before any I/O.
