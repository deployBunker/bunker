package systemd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderUnit(t *testing.T) {
	got := RenderUnit(InstallOptions{
		BinaryPath: "/usr/local/bin/bunkerd",
		ConfigPath: "/etc/bunkerd/config.yaml",
		User:       "root",
	})

	checks := []string{
		"Description=Bunker agent host daemon",
		"ExecStart=/usr/local/bin/bunkerd --config=/etc/bunkerd/config.yaml",
		"Restart=on-failure",
		"RestartSec=5",
		"User=root",
		"StandardOutput=append:/var/log/bunkerd.log",
		"StandardError=append:/var/log/bunkerd.log",
		"WantedBy=multi-user.target",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("unit file missing %q\n%s", want, got)
		}
	}
}

func TestRenderUnit_DefaultUser(t *testing.T) {
	got := RenderUnit(InstallOptions{
		BinaryPath: "/opt/bunker/bin/bunkerd",
		ConfigPath: "/etc/bunkerd/config.yaml",
	})
	if !strings.Contains(got, "User=root") {
		t.Errorf("expected default user root, got:\n%s", got)
	}
}

func TestRenderLogrotate(t *testing.T) {
	got := RenderLogrotate()
	checks := []string{
		"/var/log/bunkerd.log {",
		"daily",
		"rotate 14",
		"compress",
		"delaycompress",
		"missingok",
		"notifempty",
		"create 0640 root root",
		"systemctl reload bunkerd",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("logrotate config missing %q\n%s", want, got)
		}
	}
}

func TestInstallService(t *testing.T) {
	dir := t.TempDir()
	origUnit := UnitPath
	origLogrotate := LogrotatePath
	defer func() {
		UnitPath = origUnit
		LogrotatePath = origLogrotate
	}()
	UnitPath = filepath.Join(dir, "bunkerd.service")
	LogrotatePath = filepath.Join(dir, "bunkerd.logrotate")

	unitPath, logrotatePath, err := InstallService(InstallOptions{
		BinaryPath: "/usr/bin/bunkerd",
		ConfigPath: "/etc/bunkerd/config.yaml",
		User:       "bunker",
	})
	if err != nil {
		t.Fatalf("InstallService: %v", err)
	}
	if unitPath != UnitPath {
		t.Errorf("unitPath = %q, want %q", unitPath, UnitPath)
	}
	if logrotatePath != LogrotatePath {
		t.Errorf("logrotatePath = %q, want %q", logrotatePath, LogrotatePath)
	}

	unitBytes, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if !strings.Contains(string(unitBytes), "ExecStart=/usr/bin/bunkerd --config=/etc/bunkerd/config.yaml") {
		t.Errorf("unit missing expected ExecStart: %s", unitBytes)
	}
	if !strings.Contains(string(unitBytes), "User=bunker") {
		t.Errorf("unit missing expected User=bunker: %s", unitBytes)
	}

	lrBytes, err := os.ReadFile(logrotatePath)
	if err != nil {
		t.Fatalf("read logrotate: %v", err)
	}
	if !strings.Contains(string(lrBytes), "/var/log/bunkerd.log {") {
		t.Errorf("logrotate missing expected path: %s", lrBytes)
	}
}

func TestInstallService_Validation(t *testing.T) {
	if _, _, err := InstallService(InstallOptions{ConfigPath: "/etc/bunkerd/config.yaml"}); err == nil {
		t.Error("expected error for empty binary path")
	}
	if _, _, err := InstallService(InstallOptions{BinaryPath: "/usr/bin/bunkerd"}); err == nil {
		t.Error("expected error for empty config path")
	}
}

func TestUninstallService(t *testing.T) {
	dir := t.TempDir()
	origUnit := UnitPath
	origLogrotate := LogrotatePath
	defer func() {
		UnitPath = origUnit
		LogrotatePath = origLogrotate
	}()
	UnitPath = filepath.Join(dir, "bunkerd.service")
	LogrotatePath = filepath.Join(dir, "bunkerd.logrotate")

	if err := os.WriteFile(UnitPath, []byte("unit"), 0644); err != nil {
		t.Fatalf("write unit: %v", err)
	}
	if err := os.WriteFile(LogrotatePath, []byte("logrotate"), 0644); err != nil {
		t.Fatalf("write logrotate: %v", err)
	}

	if !IsInstalled() {
		t.Error("IsInstalled should be true before uninstall")
	}

	if err := UninstallService(); err != nil {
		t.Fatalf("UninstallService: %v", err)
	}

	if _, err := os.Stat(UnitPath); !os.IsNotExist(err) {
		t.Errorf("unit file still exists after uninstall")
	}
	if _, err := os.Stat(LogrotatePath); !os.IsNotExist(err) {
		t.Errorf("logrotate config still exists after uninstall")
	}
	if IsInstalled() {
		t.Error("IsInstalled should be false after uninstall")
	}
}

func TestDefaultInstallOptions(t *testing.T) {
	opts := DefaultInstallOptions()
	if opts.BinaryPath == "" {
		t.Error("DefaultInstallOptions BinaryPath should not be empty")
	}
	if opts.ConfigPath != "/etc/bunkerd/config.yaml" {
		t.Errorf("ConfigPath = %q, want /etc/bunkerd/config.yaml", opts.ConfigPath)
	}
	if opts.User != "root" {
		t.Errorf("User = %q, want root", opts.User)
	}
}
