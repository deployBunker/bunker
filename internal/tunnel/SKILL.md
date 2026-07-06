# Package: `internal/tunnel`

## Public API

- `TunnelManager` — manages Cloudflare TryCloudflare anonymous and named tunnels.
- `NewTunnelManager(cfg, logger)` — creates a manager for anonymous tunnels only.
- `NewTunnelManagerWithNamed(cfg, namedCfg, logger)` — creates a manager with both anonymous and named tunnel support.
- `(*TunnelManager) Start(ctx, agentID, localPort) (publicURL, error)` — launches an anonymous `cloudflared tunnel --url http://localhost:<port>` and returns the TryCloudflare URL.
- `(*TunnelManager) StartNamed(ctx, agentID, localPort) (domain, error)` — launches a named `cloudflared tunnel run <name>` and returns the configured domain.
- `(*TunnelManager) Stop(agentID) error` — terminates the cloudflared process for the agent.
- `(*TunnelManager) GetURL(agentID) string` — returns the cached public URL or empty string.
- `(*TunnelManager) Shutdown(ctx)` — stops all active tunnels.
- `(*TunnelManager) HasNamedTunnel() bool` — reports whether named tunnel support is configured and enabled.

## Conventions

- Tunnels are disabled when `cfg.Enabled` is false; `Start` returns an empty string without error.
- The TryCloudflare URL is parsed from stdout using `tryCloudflareRe`, which matches `https://<subdomain>.trycloudflare.com`.
- cloudflared processes run under a `context.WithCancel(context.Background())` so they outlive the RPC request.
- Stdout and stderr are merged; after the URL is found, a goroutine drains stdout to `io.Discard` so cloudflared never blocks on a full pipe.
- Named tunnels are considered started after a timeout if the process is still running; the domain is taken from `NamedTunnelConfig.Domain` because the tunnel does not print the URL.
- `Stop` cancels the context and waits for the process to exit; it returns nil even if the tunnel was already stopped.

## Dependencies

- `internal/config` — `TunnelConfig` and `NamedTunnelConfig`.
- Standard library: `bufio`, `context`, `fmt`, `io`, `log/slog`, `os`, `os/exec`, `regexp`, `sync`, `time`.
- No external Go dependencies beyond the standard library.

## Test Patterns

- `tunnel_test.go` tests disabled mode, duplicate start, invalid binary, timeout, context cancellation, and concurrent start/stop.
- Tests use a fake `cloudflared` script that prints the expected URL or sleeps, placed in `PATH` via `t.Setenv`.
- Regex parsing tests feed strings directly to `tryCloudflareRe` to verify URL extraction.
- Named tunnel tests use a fake binary that sleeps to simulate a long-running process.
- Tests assert that the process is cleaned up after `Stop` and that `GetURL` returns empty after stop.

## Pitfalls

1. **cloudflared must be on PATH or `BinaryPath` must be absolute.** The default `BinaryPath` is `"cloudflared"`; if the binary is not installed, `Start` fails with `exec: "cloudflared": executable file not found`.
2. **TryCloudflare URL extraction depends on stdout format.** If cloudflared changes its banner output, `tryCloudflareRe` will fail and `Start` will time out. The regex is intentionally narrow to avoid false positives.
3. **Named tunnels may never print a URL.** The code waits for `StartupTimeout` and assumes success if the process is still running; if the process crashes immediately, the scanner goroutine reports an error and `Start` returns that error.
4. **Leaked cloudflared processes after bunkerd restart.** `TunnelManager` tracks processes in memory; if bunkerd crashes, cloudflared processes may remain running. Server restart does not recover them; use `pkill cloudflared` or implement persistent tunnel tracking if this is a concern.
5. **Draining stdout is a best-effort goroutine.** If the draining goroutine is slow and `Stop` is called immediately after `Start`, the process may be cancelled before the drain finishes, causing a harmless `io.Copy` error that is ignored.
