# Package: `internal/cli`

## Public API

- `NewConnectCommand()` — `bunker connect SERVER_URL` registers a bunkerd server and stores it in `~/.bunker/config.yaml`.
- `NewSpawnCommand()` — `bunker spawn` creates an agent and prints a connection bundle.
- `NewDestroyCommand()` — `bunker destroy <agent-id>` removes an agent.
- `NewListCommand()` — `bunker list` lists agents across servers.
- `NewExecCommand()` — `bunker exec <agent-id> [--] <command> [args...]` executes commands via the server streaming RPC.
- `NewMetricsCommand()` — `bunker metrics` shows server or per-agent metrics.
- `NewHeartbeatCommand()` — `bunker heartbeat <agent-id>` extends an agent's TTL.
- `NewInfoCommand()` — `bunker info <agent-id>` shows a single agent record.
- `NewMountCommand()` — `bunker mount <agent-id> [mountpoint]` runs the SSHFS mount command from the spawn response.
- `NewTunnelCommand()` — `bunker tunnel <agent-id> [local-port]` prints the SSH local-forward command.
- `NewSystemdCommand()` — `bunker systemd install/uninstall/status` for the bunkerd service.
- `LoadCLIConfig()` / `SaveCLIConfig(cfg)` — load/save the CLI config file in `~/.bunker/config.yaml`.
- `newBunkerdClient(entry)` / `resolveToken(entry)` — shared HTTP client and token resolution helpers in `client.go`.
- `ServerEntry` struct and `CLIConfig` struct are defined in `config.go`.

## Conventions

- One file per command under `internal/cli/`, matching the command name.
- Commands use `cobra` for parsing and `viper` for environment binding (`BUNKER_TOKEN`).
- Every command loads `CLIConfig`, resolves the active server (or `--server`), and calls the connect-go client.
- Auth token priority: flag `--token`, server entry token, viper `token`, `BUNKER_TOKEN` env var.
- `exec` uses `DisableFlagParsing: true` so Docker flags (`--rm`, `--format`, etc.) pass through; it manually parses `--server` and `--timeout` before the command token.
- `spawn` saves the returned private key to `~/.bunker/keys/<agent-id>` with mode `0600`.
- `connect` REST port is the gRPC/Connect port (e.g., `:9090`) unless TLS is enabled and gRPC/REST are split; the CLI stores the URL as provided.

## Dependencies

- `proto/bunker/v1` and `proto/bunker/v1/bunkerv1connect` — generated RPC client.
- `connectrpc.com/connect` — request/response wrappers.
- `github.com/spf13/cobra` and `github.com/spf13/viper` — CLI framework and env binding.
- Standard library: `context`, `crypto/tls`, `fmt`, `net/http`, `os`, `path/filepath`, `strconv`, `time`.

## Test Patterns

- Each command has a `*_test.go` file with table-driven tests for: help output, missing args, no active server, server-not-found, success path, and server-error.
- `exec_test.go` verifies `--` passthrough and flag forwarding by inspecting the command-line arguments.
- `connect_test.go` uses an `httptest.Server` for the `RegisterServer` happy path.
- `client_test.go` verifies `resolveToken` precedence and `newBunkerdClient` TLSInsecure behavior.
- Use `cobra.Command.SetArgs`, `ExecuteC`, and `ExecuteContext` for non-streaming commands; streaming commands capture stdout/stderr with `os.Pipe` or by overriding `cmd.OutOrStdout`.
- Avoid real network calls; spin up a local connect-go server or httptest where needed.

## Pitfalls

1. **`exec` must disable cobra flag parsing.** Without `DisableFlagParsing: true`, `bunker exec <agent> -- docker run --rm` fails with `unknown flag: --rm`. The command manually parses only `--server` and `--timeout` from the raw args, then treats everything after the first non-flag token as the Docker command.
2. **`--help` becomes a raw arg when flag parsing is disabled.** `NewExecCommand` detects `--help`/`--h` in raw args and calls `cmd.Help()` before checking argument counts.
3. **`connect` URL must include the scheme.** Registering `localhost:9090` without `http://` or `https://` produces invalid connect-go client URLs; the command stores the URL exactly as given but callers should pass a full URL.
4. **Token resolution is layered and easy to invert.** The order is: `--token` flag → server entry → viper config → `BUNKER_TOKEN` env. Tests must set each layer independently to verify precedence.
5. **`spawn` writes private keys to disk but does not return the path if writing fails.** The command silently ignores `os.WriteFile` errors for the key file; production callers should verify the key file exists before relying on it. Test cases should mock the home directory or use `t.TempDir`.
6. **`bunker systemd install` requires root.** The CLI command does not check for root; `InstallService` returns `permission denied` if run as a non-root user. The test suite redirects `UnitPath`/`LogrotatePath` to temp directories to avoid needing root.
