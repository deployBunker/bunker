# Package: `internal/tailscale`

## Public API

- `TailscaleManager` — manages per-agent Tailscale mesh VPN connections.
- `NewTailscaleManager(cfg, logger)` — creates a manager from `config.TailscaleConfig`.
- `(*TailscaleManager) Start(ctx, agentID)` — runs `tailscale up --authkey=... --hostname=bunker-<id>`, then `tailscale ip -4` to get the tailnet IP. Returns the IPv4 address.
- `(*TailscaleManager) Stop(agentID)` — runs `tailscale down` and cancels the agent's context.
- `(*TailscaleManager) GetIP(agentID)` — returns the cached tailnet IP (or empty string).
- `(*TailscaleManager) Shutdown(ctx)` — stops all active connections.

## Conventions

- Hostnames use `bunker-<agentID>` format.
- `Start` returns empty string when Tailscale is disabled (`cfg.Enabled == false`) — not an error.
- A 30-second default timeout applies to both `tailscale up` and `tailscale down`; configurable via `cfg.StartupTimeout`.
- Concurrent `Start` calls for the same agent are rejected with "already connected".
- `Stop` is a no-op if the agent was never started.
- `Shutdown` uses `context.Background()` internally since agent contexts may already be cancelled.

## Dependencies

- `internal/config` — `TailscaleConfig`: Enabled, AuthKey, BinaryPath, StartupTimeout.
- Standard library: `bufio`, `context`, `fmt`, `log/slog`, `os/exec`, `strings`, `sync`, `time`.

## Test Patterns

- `tailscale_test.go`: tests use a mock binary via `BinaryPath` override; verifies IP parsing, disabled state, duplicate Start rejection, Stop idempotency.
- Tests are safe for CI — no real tailscale binary or auth key required.

## Pitfalls

1. **`tailscale up` blocks until authenticated.** If the auth key is invalid or the coordination server is unreachable, the call hangs until the timeout (30s default). Always set a reasonable timeout.
2. **`tailscale ip -4` may return empty.** If the node hasn't received an IP assignment yet, the output is empty. The code treats this as an error.
3. **Context cancellation during `tailscale up` is best-effort.** The `exec.CommandContext` cancels the Go-side command, but the tailscale daemon may continue in the background.
4. **No multi-agent buffering.** Each agent gets its own `tailscale up` process. Spawning 50 agents simultaneously launches 50 tailscale processes — consider connection pooling for large fleets.
