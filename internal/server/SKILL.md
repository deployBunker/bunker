# Package: `internal/server`

## Public API

- `BunkerdServer` — the main HTTP/gRPC daemon. `New(cfg)` opens the daemon-side audit trail when `cfg.Audit.Enabled` (GAP-047, cc66105); an open failure is NON-FATAL — it warns `"audit logging disabled"` and runs without audit so a missing/unwritable `/var/log/bunkerd` never blocks daemon startup. `Run` closes the audit log (defer) when the daemon exits.
- `New(cfg) *BunkerdServer` — constructor.
- `(*BunkerdServer) Run(ctx) error` — validates config, logs the effective config on startup, builds the chi router, mounts connect-go handlers, starts listeners, and blocks until shutdown. Since GAP-028 (5b3fe56) it emits `slog.Info("bunkerd config loaded", "max_agents", ...)` right after `cfg.Validate()` so the ACTIVE agent cap is always visible in the daemon log (README/config.example/code were misaligned at 50 vs 100 before the fix).
- `bunkerdService` — implements `bunkerv1connect.BunkerdHandler`: `ServerInfo`, `ServerMetrics`, `SpawnAgent`, `DestroyAgent`, `ListAgents`, `GetAgent`, `AgentMetrics`, `ExecAgent`, `RunAgent`, `HeartbeatAgent`, `QueryAudit`. `ServerInfo` reports real uptime (package `serverStartTime`, set at process start) and the real build version from `internal/version` — no more hardcoded `0.1.0`/`UptimeSeconds: 0` (DOGFOOD-006). `SpawnAgent` validates the TTL as step 1a (BEFORE port allocation/side effects) and maps invalid TTLs to `CodeInvalidArgument` via a service-level pre-check (DOGFOOD-003).
- `QueryAudit` (GAP-050, 3d435e0) — the remote query surface for the audit trail: read-only, returns matching records oldest-first from the daemon's audit log (live file + rotated backups `.1`–`.3`, chain order), the exact same read path as `bunker audit list/export --path` uses locally. Filters (all ANDed): `agent_id` exact match, `method` substring match on the full connect procedure, `since`/`until` RFC3339 bounds (inclusive), `limit` capped at the NEWEST matches (0 = no limit). Error mapping: invalid `since`/`until` → `CodeInvalidArgument` (a typo must not silently widen the query to "everything"), audit logging disabled on the server → `CodeUnavailable`, read failure → `CodeInternal`. The handler never writes records itself — the record for the query itself is appended by the audit interceptor like every other authenticated RPC.
- `agentService` — implements `bunkerv1connect.AgentHandler`: `GetInfo`, `Metrics`, `Heartbeat` (scoped sub-key access).
- `buildTLSConfig()` (method on `BunkerdServer`) — constructs file, self-signed, certmagic AutoTLS, or mTLS configs.
- Helper functions: `readDiskStats`, `agentDiskUsage(agentID)`, `countDockerSockets`, `buildExecSSHCommand`, `buildExecSSHRawCommand`, `buildExecSSHScriptCommand`, `buildAgentExecCommand`, `buildAgentRawExecCommand`, `buildAgentScriptCommand`, `buildSSHBaseCommand`, `shellQuoteSingle`.
- Remote exec construction (DOGFOOD-001 chain, 9896c99→956d307): `buildAgentExecCommand` shell-quotes EVERY argument with `shellQuoteSingle` and wraps the joined command in `sh -c '<joined>'` — this fixes compound snippets passed as the COMMAND token (exec-style) and `bunker env set/get/list/unset`. The server-side env injection then (a) guards the env-file source with `[ -f ]` (a fresh agent has no env file and dash exits 2 on a failed dot-source, which killed ALL exec — live-E2E-only regression) and (b) sources with `set -a` (allexport) so injected `KEY=VALUE` lines reach the child `sh -c` as real env vars, not shell-only locals.
- Deterministic agent PATH (GAP-030, 2026-08-11): all three builders (`buildAgentExecCommand`, `buildAgentRawExecCommand`, `buildAgentScriptCommand`) set `PATH=<agentBin>:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin` via the `agentExecBasePath` constant — they NO LONGER inherit the daemon's ambient `$PATH` (`PATH=...:$PATH`). A polluted/long daemon PATH (scheduler shells accumulate ~16KB of duplicated entries) was embedded verbatim into every agent command and broke exec with exit 126 under uutils env(1) ("unknown error: execvp failed" / 127 "No such file or directory").
- Disk monitoring (MONITOR-001/002): `SpawnAgent` warns in the server log when host disk usage exceeds 90% (`diskAlert`); `readDiskStats` reads host used/total; `agentDiskUsage` walks an agent's home directory for the per-agent figure exposed via `AgentSummary.DiskUsedBytes`.
- `ServerMetrics` uses `resource.CPUSampler` (delta of `cpu.stat`) for live CPU% and `ReadCgroupMetrics` with the `/proc/meminfo` fallback for memory — the server never hardcodes `CPU: 0.0%` / `Memory: 0 B` (DOGFOOD-006).

## Conventions

- Uses `chi` router with middleware: RequestID, RealIP, Logger, Recoverer, Timeout.
- Health endpoint `/healthz` returns JSON `{"status":"ok"}`.
- Hilo graph endpoints are mounted under `/graph/*` for runtime dependency analysis.
- The `Bunkerd` service handler is mounted with `NewMasterOnlyAuthInterceptor` so agent-scoped sub-keys cannot call server-level RPCs.
- The `Agent` service handler is mounted with `NewJWTAuthInterceptor` so both master and agent-scoped tokens work.
- Audit interceptor composition (GAP-047): the audit interceptor (`audit.NewInterceptor`) is appended AFTER the auth interceptor on BOTH services (`bunkerdInterceptors` and `agentInterceptors`), with auth listed first in `connect.WithInterceptors` so it runs OUTERMOST — failed-auth attempts never produce audit records. Caller identity is read from the Claims the auth interceptor placed in the context; the raw Authorization header / token value is never touched.
- Audit record shape: append-only JSONL, one record per authenticated RPC with `ts` (RFC3339Nano UTC), `caller` (identity derived from Claims: `"master"` for static token / unscoped master JWT, `"agent:<id>"`, `"agent:<id> key:<kid>"`, or the subject), `method` (full connect procedure), `remote_addr`, `agent_id` (target agent from the request message, else the caller's scope; empty for master callers on streaming RPCs where the request message is invisible to interceptors), `duration_ms`, `outcome` (`"ok"` or the connect error code), `summary` (procedure + `agent_id=` only — deliberately NO message contents, since commands/script bodies can carry credentials). Token values are never written.
- Audit log lifecycle (GAP-049, 75e0add): the log rotates at 5 MiB keeping 3 backups (`audit.log` → `.1` → `.2` → `.3`); every record carries its own SHA-256 `hash` plus the previous record's `hash` as `prev_hash`, and the chain spans rotations (first record of a fresh file chains to the last record of `.1`). `bunker audit verify` (CLI) checks the chain via `internal/audit.Verify`. `bunker audit list/export` (CLI) and `QueryAudit` are the read surfaces.
- Snoopy exec-level audit provision (GAP-048, 333c084/5f32cb7): `scripts/install-exec-audit.sh` + `docs/exec-audit.md` — an optional per-agent OS-level command-logging provision (rsyslog + snoopy) that complements the RPC-level audit trail. It is a provisioning script, NOT code in this package.
- `ExecAgent` builds an SSH command using `bunker-<agent-id>@localhost` and the persisted key in `/etc/bunkerd/ssh/<agent-id>`, passing `DOCKER_HOST` and `PATH` through a single quoted `sh -c` argument.
- Listeners run on `GRPCAddr` and optionally `RESTAddr` when they differ; both use the same chi router and TLS config.
- `Run` waits for `SIGINT`/`SIGTERM` or an error from either listener.

## Dependencies

- `connectrpc.com/connect`, `github.com/go-chi/chi/v5`, `github.com/caddyserver/certmagic`.
- `internal/agent`, `internal/apikey`, `internal/audit`, `internal/auth`, `internal/config`, `internal/hilo`, `internal/resource`, `internal/tailscale`, `internal/tlsutil`, `internal/tunnel`, `internal/version`.
- `proto/bunker/v1` and `proto/bunker/v1/bunkerv1connect`.
- Standard library: `context`, `crypto/tls`, `fmt`, `log/slog`, `net/http`, `os`, `os/signal`, `path/filepath`, `strings`, `syscall`, `time`.

## Test Patterns

- `server_test.go` tests router construction, health endpoint, and handler mount points using `httptest`.
- `service_test.go` tests the `bunkerdService` with fake `agentManager` and tracker implementations.
- `audit_query_test.go` (GAP-050) tests `QueryAudit` against a real server with a real audit log: filter combinations (agent/method/since/until/limit), invalid RFC3339 `since`/`until` → `CodeInvalidArgument`, disabled audit → `CodeUnavailable`, and that the query RPC itself lands in the trail.
- `jwt_e2e_test.go`, `tls_e2e_test.go`, `ttl_e2e_test.go` run end-to-end style tests against a real server in a temp directory.
- `ExecAgent` tests assert the SSH command string is passed as a single quoted argument to avoid the SSH env-dump bug (WI-046); round-trip tests execute the built command through `sh` to prove compound snippets + env injection (DOGFOOD-001).
- `TestServerInfo` tolerates sub-second uptime truncation (FLAKE-001): `UptimeSeconds == 0` only fails when `time.Since(serverStartTime) >= time.Second` — `serverStartTime` is set at package init, and `uint64(time.Since(...).Seconds())` truncates to 0 when the test binary is <1s old. Still catches a hardcoded-0 regression.
- Use `httptest.NewServer` for non-TLS tests and `httptest.NewTLSServer` for TLS tests where appropriate.

## Pitfalls

1. **The same router is used for both gRPC/Connect and REST traffic.** If `RESTAddr` differs from `GRPCAddr`, the same middleware/interceptor stack is applied to both listeners, which is usually desired but can cause double logging.
2. **Agent-scoped sub-keys must be rejected for server-level RPCs.** Mounting the `Bunkerd` service with the master-only interceptor is critical; otherwise a leaked agent key could spawn or destroy other agents.
3. **`ExecAgent` SSH quoting is subtle.** The remote command must be wrapped as `sh -c 'env PATH=... DOCKER_HOST=... <command> <args>'` passed as a single argument to `ssh`. Splitting `sh`, `-c`, and the script into separate args causes the SSH server to treat the env vars as positional parameters and echo them instead of executing the command. Compound snippets passed as the COMMAND token need the same wrap (DOGFOOD-001). Never dot-source a possibly-missing env file unguarded — dash exits 2 and kills the whole exec.
4. **mTLS auth requires the base HTTP request in context.** The `MTLSAuth` interceptor reads `http.ServerContextKey`; if the server or transport layer does not inject it, mTLS auth will always fail with "no TLS connection state".
5. **`certmagic` AutoTLS requires a valid domain and outbound ACME access.** Tests use self-signed or file-based certs to avoid network dependencies.
6. **Server shutdown is signal-driven, not context-driven.** `Run` blocks on a signal channel; callers must send `SIGINT`/`SIGTERM` to stop. The context passed to `Run` is not used to cancel listeners.
7. **`serverStartTime` is set at package init, not per-process start.** Anything asserting on `UptimeSeconds` must tolerate sub-second truncation (see FLAKE-001 test pattern) or it flakes on fast test binaries.
8. **Audit open failure is non-fatal but easy to miss.** `New` only warns (`"audit logging disabled"`) when the audit log cannot be opened — the daemon keeps running without audit. If you expect records and see none, check the daemon log for that warning (parent dirs are auto-created 0700, but a read-only filesystem or permission problem silently degrades to no audit).
9. **`QueryAudit` on a server with audit disabled returns `CodeUnavailable`, not empty results.** Treat it as "no trail exists", distinct from "no matching records". Also note `QueryAudit` is itself audited — querying the trail appends a record about the query.
10. **Audit `agent_id` is best-effort on streaming RPCs.** Interceptors cannot see server-stream request messages, so the record falls back to the caller's claims scope (empty for master callers). Unary RPCs extract the target from the request message (`DestroyAgentRequest`, `ExecAgentRequest`, etc.).
