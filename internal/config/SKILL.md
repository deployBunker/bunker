# Package: `internal/config`

## Public API

- `Config` — top-level bunkerd configuration struct containing `Server`, `TLS`, `Auth`, `Agent`, `Tunnel`, `NamedTunnel`, and `Tailscale` sections.
- `ServerConfig` — `GRPCAddr` and `RESTAddr` listeners, plus `RequestTimeout` (default 300s, was hardcoded 60s — the agent-timeout root cause).
- `TLSConfig` — enabled, file certs, certmagic AutoTLS, self-signed, mTLS, CA file, CN verification, and hosts.
- `AuthConfig` — enabled (defaults TRUE — secure-by-default, GAP-011), static token, JWT secret, JWT TTL.
- `AgentConfig` — base data dir, SSH dir, port ranges, max agents, default CPU/memory/disk/process/file/container limits, and TTL.
- `TunnelConfig` / `NamedTunnelConfig` / `TailscaleConfig` — networking settings.
- `DefaultConfig()` — returns a fully populated default config.
- `Load(path string) (*Config, error)` — reads YAML from `path`, applies env-var overrides (`BUNKERD_*`), and unmarshals into a default config.
- `(*Config) Validate() error` — checks required fields and TLS mode consistency.
- `(*Config) CheckAuth() (string, error)` — startup authentication gate (GAP-011): returns a prominent `*** WARNING: AUTH DISABLED ***` message when `auth.enabled` is explicitly false, and an error when auth is enabled but neither `auth.token` nor `auth.jwt_secret` is set. `cmd/bunkerd` calls it in `run()` and refuses to start on error — the daemon never silently runs unauthenticated.

## Conventions

- Config keys map to env vars with the `BUNKERD_` prefix and underscores replacing nested dots: `BUNKERD_SERVER_GRPC_ADDR`, `BUNKERD_TLS_CERT_FILE`, etc.
- `viper.AutomaticEnv()` and explicit `BindEnv` calls cover the same keys; explicit binds ensure consistent behavior even when nested defaults change.
- Default addresses are `":9090"` (gRPC/Connect) and `":8080"` (REST). Bunker production deployments typically run the REST port on `:18080`.
- Default agent port range is `10000-19999` with 100 ports per agent (100-agent capacity, matching MaxAgents 100 — GAP-010). Was `10000-10100`/10 before 4df96ec; docs and `TestDefaultConfig` both assert `19999`/`100`.
- Default `max_agents` is 100, aligned across ALL three sources (code default, config.example.yaml, README inline config) since GAP-028 (5b3fe56) — before that the docs said 50 while code defaulted to 100, a silent cap discrepancy. `TestDefaultConfig` asserts `MaxAgents == 100`.
- `config.example.yaml` at the repo root mirrors these defaults (README inline config + `DefaultConfig`) and is referenced by `bunkerd --help` (`cp config.example.yaml /etc/bunkerd/config.yaml`) and the README Configure section (GAP-008).
- Default resource limits: 2.0 CPU cores, 4 GiB memory, 20 GiB disk, 4096 processes, 65536 open files, 10 Docker containers, 6-hour TTL.
- TLS modes are mutually exclusive: `auto_tls`, `self_signed`, or file-based certs. `Validate` requires `domain` for AutoTLS, `cert_file`/`key_file` for file mode, and `ca_file` for mTLS.
- Durations are `time.Duration` and parsed from YAML strings like `"6h"` or `"24h"`.

## Dependencies

- `github.com/spf13/viper` — config parsing and env binding.
- Standard library: `fmt`, `os`, `strings`, `time`.

## Test Patterns

- `config_test.go` verifies defaults, env overrides, file loading, and validation error cases.
- `TestDefaultConfig` asserts the full default surface: port range `19999`/`100` (GAP-010), `MaxAgents == 100` (GAP-028), auth enabled (GAP-011), 6h default TTL.
- Tests use `t.Setenv` to exercise `BUNKERD_*` env var bindings without touching real files.
- Validation tests cover all TLS modes: missing certs, AutoTLS without domain, mTLS without CA file, and valid combinations.
- Tests assert default values are populated even when the config file is absent.

## Pitfalls

1. **`viper` env binding requires underscores, not dots.** YAML keys like `server.grpc_addr` map to `BUNKERD_SERVER_GRPC_ADDR`. Passing `BUNKERD_SERVER.GRPC_ADDR` will not work because viper's replacer is configured with `strings.NewReplacer(".", "_")`.
2. **`Load` returns defaults when the file is missing.** This is intentional for container bootstrapping, but it means `Validate` must be called explicitly to catch missing required fields.
3. **TLS self-signed mode fills in default cert/key paths.** If `cert_file` or `key_file` are empty and `self_signed` is true, `Validate` sets them to `/etc/bunkerd/tls/cert.pem` and `/etc/bunkerd/tls/key.pem`; callers must generate the files before enabling TLS.
4. **`JWTSecret` is used as both signing key and apikey manager seed.** The `apikey.Manager` is initialized with `cfg.Auth.JWTSecret`; rotating the JWT secret invalidates all opaque agent sub-keys.
5. **Port ranges are `uint32`, not `int`.** Negative values or values > 65535 cannot be represented, but zero values can accidentally disable allocation if `PortRangePerAgent` is 0.
6. **Request timeout defaults to 300s but was historically hardcoded 60s.** `server.request_timeout` / `BUNKERD_SERVER_REQUEST_TIMEOUT` overrides it. Agent exec/timeout complaints on a deployed server are usually the old 60s binary — pull latest and rebuild.
7. **Auth is enabled by default — a credential-less daemon refuses to start (GAP-011).** With no config file, `bunkerd` exits with `refusing to start: auth.enabled is true but neither auth.token nor auth.jwt_secret is set`. Batteries and test fixtures set `auth.enabled: false` explicitly; an explicit disable prints `*** WARNING: AUTH DISABLED ***` to stderr. A default config without credentials is a STARTUP ERROR, not a silent unauthenticated run.
