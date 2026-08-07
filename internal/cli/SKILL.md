# Package: `internal/cli`

## Public API

- `NewConnectCommand()` — `bunker connect SERVER_URL` registers a bunkerd server and stores it in `~/.bunker/config.yaml`. Help documents both ports: `http://localhost:9090 (gRPC) or http://localhost:8080 (REST)` (GAP-013).
- `NewSpawnCommand()` — `bunker spawn` creates an agent and prints a connection bundle.
- `NewDestroyCommand()` — `bunker destroy <agent-id>` removes an agent. Maps `connect.CodeNotFound` → `Agent <id> not found.` + exit 0 (idempotent, DOGFOOD-005).
- `NewListCommand()` — `bunker list` lists agents across servers, including a per-agent disk usage column.
- `NewStatusCommand()` — `bunker status` shows the active server's status: disk usage (>90% warning banner), CPU %, Memory used/limit, and real uptime from `ServerInfo` (DOGFOOD-006); `bunker status --all-servers` shows every registered server (MULTI-002).
- `NewUseCommand()` — `bunker use <server-name>` switches the active server in the CLI config (MULTI-001).
- `NewExecCommand()` — `bunker exec <agent-id> [--] <command> [args...]` executes commands via the server streaming RPC. Propagates the remote exit code: `bunker exec sh -c 'exit 7'` → CLI exits 7 (DOGFOOD-005).
- `NewRunCommand()` — `bunker run <agent-id> [--] <command> [args...]` runs a one-shot command in the agent (raw exec variant).
- `NewCpCommand()` — `bunker cp <agent-id>:<path> <local-path>` copies a file OUT of an agent via scp (UX-008, DOGFOOD-002).
- `NewDeployCommand()` — `bunker deploy <local-path> <agent-id>:<path>` copies a file INTO an agent and chowns it to the agent user (UX-008, DOGFOOD-002).
- `NewEnvCommand()` — `bunker env <agent-id>` prints the agent's environment (DOCKER_HOST etc.).
- `NewVersionCommand()` — `bunker version` prints semver, commit, Go version, build date, and platform. Version/Commit/BuildDate come from the shared `internal/version` package, injected via Makefile LDFLAGS (`-X`) at build time — `commit: unknown` means the binary was built without ldflags (UX-005, DOGFOOD-006).
- `NewMetricsCommand()` — `bunker metrics` shows server or per-agent metrics (CPU/memory now real, DOGFOOD-006).
- `NewHeartbeatCommand()` — `bunker heartbeat <agent-id>` extends an agent's TTL.
- `NewInfoCommand()` — `bunker info <agent-id>` shows a single agent record.
- `NewMountCommand()` — `bunker mount <agent-id> [mountpoint]` runs the SSHFS mount command from the spawn response (rewritten for client-local host/key, DOGFOOD-002).
- `NewTunnelCommand()` — `bunker tunnel <agent-id> [local-port]` prints the SSH local-forward command (key path rewritten to `~/.bunker/keys/<id>`, DOGFOOD-002).
- `NewSystemdCommand()` — `bunker systemd install/uninstall/status` for the bunkerd service.
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
- `spawn` saves the returned private key to `~/.bunker/keys/<agent-id>` with mode `0600`.
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
- `status_test.go` covers the disk column and the >90% WARNING banner; `use_test.go` covers server switching; `cp_test.go`/`deploy_test.go` cover scp arg construction and chown logic with mocked `exec.Command`.
- `exec_test.go` verifies `--` passthrough and flag forwarding by inspecting the command-line arguments; `TestExecCommand_FlagSeparator` (x4) covers `--server X -- <cmd>` and `--timeout N -- <cmd>` (GAP-002).
- `exit_error_test.go` / destroy tests cover `CodeNotFound` → `Agent <id> not found.` + exit 0 and `TestExecCommand_ExitCode7` style remote-code propagation (DOGFOOD-005).
- `connect_test.go` uses an `httptest.Server` for the `RegisterServer` happy path.
- `sshhost_test.go` covers host resolution precedence, bundle rewriting, and `TestClientTunnelArgs` (4 cases incl. inject-if-absent and no-duplicate — GAP-009).
- `client_test.go` verifies `resolveToken` precedence and `newBunkerdClient` TLSInsecure behavior.
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
