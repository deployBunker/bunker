# Package: `internal/server`

## Public API

- `BunkerdServer` — the main HTTP/gRPC daemon.
- `New(cfg) *BunkerdServer` — constructor.
- `(*BunkerdServer) Run(ctx) error` — builds the chi router, mounts connect-go handlers, starts listeners, and blocks until shutdown.
- `bunkerdService` — implements `bunkerv1connect.BunkerdHandler`: `ServerInfo`, `ServerMetrics`, `SpawnAgent`, `DestroyAgent`, `ListAgents`, `GetAgent`, `AgentMetrics`, `ExecAgent`, `HeartbeatAgent`.
- `agentService` — implements `bunkerv1connect.AgentHandler`: `GetInfo`, `Metrics`, `Heartbeat` (scoped sub-key access).
- `buildTLSConfig()` (method on `BunkerdServer`) — constructs file, self-signed, certmagic AutoTLS, or mTLS configs.
- Helper functions: `readDiskStats`, `countDockerSockets`, `buildExecSSHCommand`, `shellQuoteSingle`, `buildAgentExecCommand`.

## Conventions

- Uses `chi` router with middleware: RequestID, RealIP, Logger, Recoverer, Timeout.
- Health endpoint `/healthz` returns JSON `{"status":"ok"}`.
- Hilo graph endpoints are mounted under `/graph/*` for runtime dependency analysis.
- The `Bunkerd` service handler is mounted with `NewMasterOnlyAuthInterceptor` so agent-scoped sub-keys cannot call server-level RPCs.
- The `Agent` service handler is mounted with `NewJWTAuthInterceptor` so both master and agent-scoped tokens work.
- `ExecAgent` builds an SSH command using `bunker-<agent-id>@localhost` and the persisted key in `/etc/bunkerd/ssh/<agent-id>`, passing `DOCKER_HOST` and `PATH` through a single quoted `sh -c` argument.
- Listeners run on `GRPCAddr` and optionally `RESTAddr` when they differ; both use the same chi router and TLS config.
- `Run` waits for `SIGINT`/`SIGTERM` or an error from either listener.

## Dependencies

- `connectrpc.com/connect`, `github.com/go-chi/chi/v5`, `github.com/caddyserver/certmagic`.
- `internal/agent`, `internal/apikey`, `internal/auth`, `internal/config`, `internal/hilo`, `internal/resource`, `internal/tailscale`, `internal/tlsutil`, `internal/tunnel`.
- `proto/bunker/v1` and `proto/bunker/v1/bunkerv1connect`.
- Standard library: `context`, `crypto/tls`, `fmt`, `log/slog`, `net/http`, `os`, `os/signal`, `path/filepath`, `strings`, `syscall`, `time`.

## Test Patterns

- `server_test.go` tests router construction, health endpoint, and handler mount points using `httptest`.
- `service_test.go` tests the `bunkerdService` with fake `agentManager` and tracker implementations.
- `jwt_e2e_test.go`, `tls_e2e_test.go`, `ttl_e2e_test.go` run end-to-end style tests against a real server in a temp directory.
- `ExecAgent` tests assert the SSH command string is passed as a single quoted argument to avoid the SSH env-dump bug (WI-046).
- Use `httptest.NewServer` for non-TLS tests and `httptest.NewTLSServer` for TLS tests where appropriate.

## Pitfalls

1. **The same router is used for both gRPC/Connect and REST traffic.** If `RESTAddr` differs from `GRPCAddr`, the same middleware/interceptor stack is applied to both listeners, which is usually desired but can cause double logging.
2. **Agent-scoped sub-keys must be rejected for server-level RPCs.** Mounting the `Bunkerd` service with the master-only interceptor is critical; otherwise a leaked agent key could spawn or destroy other agents.
3. **`ExecAgent` SSH quoting is subtle.** The remote command must be wrapped as `sh -c 'env PATH=... DOCKER_HOST=... <command> <args>'` passed as a single argument to `ssh`. Splitting `sh`, `-c`, and the script into separate args causes the SSH server to treat the env vars as positional parameters and echo them instead of executing the command.
4. **mTLS auth requires the base HTTP request in context.** The `MTLSAuth` interceptor reads `http.ServerContextKey`; if the server or transport layer does not inject it, mTLS auth will always fail with "no TLS connection state".
5. **`certmagic` AutoTLS requires a valid domain and outbound ACME access.** Tests use self-signed or file-based certs to avoid network dependencies.
6. **Server shutdown is signal-driven, not context-driven.** `Run` blocks on a signal channel; callers must send `SIGINT`/`SIGTERM` to stop. The context passed to `Run` is not used to cancel listeners.
