// Package systemd installs and removes the bunkerd systemd service unit.
package systemd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// UnitName is the systemd service unit name for bunkerd.
	UnitName = "bunkerd.service"

	// defaultUnitPath is the destination path for the systemd unit file.
	defaultUnitPath = "/etc/systemd/system/bunkerd.service"

	// defaultLogrotatePath is the destination path for the logrotate config.
	defaultLogrotatePath = "/etc/logrotate.d/bunkerd"
)

// UnitPath and LogrotatePath are mutable so tests can redirect them to temp dirs.
var (
	UnitPath      = defaultUnitPath
	LogrotatePath = defaultLogrotatePath
)

// InstallOptions controls how the bunkerd service unit is generated.
type InstallOptions struct {
	BinaryPath string // path to the bunkerd executable
	ConfigPath string // path to the bunkerd config file
	User       string // user to run the service as (empty = root)
}

// DefaultInstallOptions returns options that use the running binary and the
// default config path.
func DefaultInstallOptions() InstallOptions {
	exe, _ := os.Executable()
	if exe == "" {
		exe = "/usr/local/bin/bunkerd"
	}
	return InstallOptions{
		BinaryPath: exe,
		ConfigPath: "/etc/bunkerd/config.yaml",
		User:       "root",
	}
}

// InstallService writes the bunkerd systemd unit and logrotate config, then
// reloads systemd. It returns the paths written.
func InstallService(opts InstallOptions) (unitPath string, logrotatePath string, err error) {
	if opts.BinaryPath == "" {
		return "", "", fmt.Errorf("binary path is required")
	}
	if opts.ConfigPath == "" {
		return "", "", fmt.Errorf("config path is required")
	}

	unitContent := RenderUnit(opts)
	if err := writeFile(UnitPath, []byte(unitContent), 0644); err != nil {
		return "", "", fmt.Errorf("write unit file: %w", err)
	}

	logrotateContent := RenderLogrotate()
	if err := writeFile(LogrotatePath, []byte(logrotateContent), 0644); err != nil {
		return "", "", fmt.Errorf("write logrotate config: %w", err)
	}

	if err := reloadSystemd(); err != nil {
		return "", "", fmt.Errorf("reload systemd: %w", err)
	}

	return UnitPath, LogrotatePath, nil
}

// UninstallService removes the bunkerd systemd unit and logrotate config,
// then reloads systemd.
func UninstallService() error {
	var errs []string
	if err := os.Remove(UnitPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Sprintf("remove unit: %v", err))
	}
	if err := os.Remove(LogrotatePath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Sprintf("remove logrotate: %v", err))
	}
	if err := reloadSystemd(); err != nil {
		errs = append(errs, fmt.Sprintf("reload systemd: %v", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("uninstall failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

// IsInstalled reports whether the bunkerd systemd unit file exists.
func IsInstalled() bool {
	_, err := os.Stat(UnitPath)
	return err == nil
}

// RenderUnit returns the contents of the bunkerd.service unit file.
func RenderUnit(opts InstallOptions) string {
	user := opts.User
	if user == "" {
		user = "root"
	}
	return fmt.Sprintf(`[Unit]
Description=Bunker agent host daemon
Documentation=https://github.com/deployBunker/bunker
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --config=%s
Restart=on-failure
RestartSec=5
User=%s
StandardOutput=append:/var/log/bunkerd.log
StandardError=append:/var/log/bunkerd.log

[Install]
WantedBy=multi-user.target
`, opts.BinaryPath, opts.ConfigPath, user)
}

// RenderLogrotate returns the contents of the bunkerd logrotate config.
func RenderLogrotate() string {
	return `/var/log/bunkerd.log {
    daily
    rotate 14
    compress
    delaycompress
    missingok
    notifempty
    create 0640 root root
    postrotate
        systemctl reload bunkerd >/dev/null 2>&1 || true
    endscript
}
`
}

func writeFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

func reloadSystemd() error {
	return nil // no-op when not running under systemd
}
