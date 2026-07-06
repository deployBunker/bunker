# Package: `internal/config`

## Public API

- `Config` — top-level bunkerd configuration struct containing `Server`, `TLS`, `Auth`, `Agent`, `Tunnel`, `NamedTunnel`, and `Tailscale` sections.
- `ServerConfig` — `GRPCAddr` and `RESTAddr` listeners.
- `TLSConfig` — enabled, file certs, certmagic AutoTLS, self-signed, mTLS, CA file, CN verification, and hosts.
- `AuthConfig` — enabled, static token, JWT secret, JWT TTL.
- `AgentConfig` — base data dir, SSH dir, port ranges, max agents, default CPU/memory/disk/process/file/container limits, and TTL.
- `TunnelConfig` / `NamedTunnelConfig` / `TailscaleConfig` — networking settings.
- `DefaultConfig()` — returns a fully populated default config.
- `Load(path string) (*Config, error)` — reads YAML from `path`, applies env-var overrides (`BUNKERD_*`), and unmarshals into a default config.
- `(*Config) Validate() error` — checks required fields and TLS mode consistency.

## Conventions

- Config keys map to env vars with the `BUNKERD_` prefix and underscores replacing nested dots: `BUNKERD_SERVER_GRPC_ADDR`, `BUNKERD_TLS_CERT_FILE`, etc.
- `viper.AutomaticEnv()` and explicit `BindEnv` calls cover the same keys; explicit binds ensure consistent behavior even when nested defaults change.
- Default addresses are `":9090"` (gRPC/Connect) and `":8080"` (REST). Bunker production deployments typically run the REST port on `:18080`.
- Default resource limits: 2.0 CPU cores, 4 GiB memory, 20 GiB disk, 4096 processes, 65536 open files, 10 Docker containers, 6-hour TTL.
- TLS modes are mutually exclusive: `auto_tls`, `self_signed`, or file-based certs. `Validate` requires `domain` for AutoTLS, `cert_file`/`key_file` for file mode, and `ca_file` for mTLS.
- Durations are `time.Duration` and parsed from YAML strings like `"6h"` or `"24h"`.

## Dependencies

- `github.com/spf13/viper` — config parsing and env binding.
- Standard library: `fmt`, `os`, `strings`, `time`.

## Test Patterns

- `config_test.go` verifies defaults, env overrides, file loading, and validation error cases.
- Tests use `t.Setenv` to exercise `BUNKERD_*` env var bindings without touching real files.
- Validation tests cover all TLS modes: missing certs, AutoTLS without domain, mTLS without CA file, and valid combinations.
- Tests assert default values are populated even when the config file is absent.

## Pitfalls

1. **`viper` env binding requires underscores, not dots.** YAML keys like `server.grpc_addr` map to `BUNKERD_SERVER_GRPC_ADDR`. Passing `BUNKERD_SERVER.GRPC_ADDR` will not work because viper's replacer is configured with `strings.NewReplacer(".", "_")`.
2. **`Load` returns defaults when the file is missing.** This is intentional for container bootstrapping, but it means `Validate` must be called explicitly to catch missing required fields.
3. **TLS self-signed mode fills in default cert/key paths.** If `cert_file` or `key_file` are empty and `self_signed` is true, `Validate` sets them to `/etc/bunkerd/tls/cert.pem` and `/etc/bunkerd/tls/key.pem`; callers must generate the files before enabling TLS.
4. **`JWTSecret` is used as both signing key and apikey manager seed.** The `apikey.Manager` is initialized with `cfg.Auth.JWTSecret`; rotating the JWT secret invalidates all opaque agent sub-keys.
5. **Port ranges are `uint32`, not `int`.** Negative values or values > 65535 cannot be represented, but zero values can accidentally disable allocation if `PortRangePerAgent` is 0.
