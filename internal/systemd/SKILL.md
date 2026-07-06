# Package: `internal/systemd`

## Public API

- `UnitName` — `"bunkerd.service"`.
- `UnitPath` / `LogrotatePath` — mutable package vars defaulting to `/etc/systemd/system/bunkerd.service` and `/etc/logrotate.d/bunkerd`; tests can redirect them to temp directories.
- `InstallOptions` — controls binary path, config path, and service user.
- `DefaultInstallOptions()` — returns options using the current executable and default config path.
- `InstallService(opts) (unitPath, logrotatePath, error)` — writes the unit, writes the logrotate config, and reloads systemd.
- `UninstallService() error` — removes the unit and logrotate config and reloads systemd.
- `IsInstalled() bool` — checks whether the unit file exists.
- `RenderUnit(opts)` — returns the systemd unit file contents.
- `RenderLogrotate()` — returns the logrotate snippet contents.

## Conventions

- The unit is a `Type=simple` service with `Restart=on-failure` and `RestartSec=5`.
- Logs are written via `StandardOutput=append:/var/log/bunkerd.log` and `StandardError=append:/var/log/bunkerd.log`.
- Logrotate config rotates daily, keeps 14 compressed logs, and reloads bunkerd after rotation.
- Default user is `root`; the `User` field can be overridden in `InstallOptions` for non-root deployments.
- `InstallService` returns the paths written so callers can report them.
- `reloadSystemd()` is a no-op in this package; it is meant to be overridden or executed by the system package manager in real deployments.

## Dependencies

- Standard library: `fmt`, `os`, `path/filepath`, `strings`.
- No external dependencies.

## Test Patterns

- `systemd_test.go` redirects `UnitPath` and `LogrotatePath` to `t.TempDir()` and asserts the rendered contents.
- Tests verify that `RenderUnit` includes the binary path, config path, and user.
- Tests verify `RenderLogrotate` contains the expected log path, rotation, and `postrotate` reload command.
- `IsInstalled` is tested by creating a file at the redirected path.
- All write operations are tested against temp directories to avoid requiring root or modifying system directories.

## Pitfalls

1. **`InstallService` does not start the service.** It only writes files and reloads systemd unit files. The caller (or operator) must run `systemctl start bunkerd` separately.
2. **The `reloadSystemd()` function is a no-op.** In a real deployment this should be replaced with `systemctl daemon-reexec` or `systemctl daemon-reload`; the no-op keeps tests hermetic and avoids requiring systemd in CI.
3. **Writing to `/etc/systemd/system` and `/etc/logrotate.d` requires root.** The CLI command does not elevate privileges; running `bunker systemd install` as a non-root user will fail with permission denied.
4. **Logrotate's `create` directive assumes root ownership.** The snippet uses `create 0640 root root`; if bunkerd runs as a non-root user, logrotate may create files the service cannot read. Update `InstallOptions.User` and the logrotate `create` directive together.
5. **The unit does not set `WorkingDirectory`.** bunkerd loads `/etc/bunkerd/config.yaml` via the `--config` flag, so the working directory is unimportant. If relative paths are added to the config, set `WorkingDirectory` in the unit template.
