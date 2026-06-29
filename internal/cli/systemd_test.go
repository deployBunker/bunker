package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/systemd"
)

func TestSystemdInstallCommand(t *testing.T) {
	dir := t.TempDir()
	origUnit := systemd.UnitPath
	origLogrotate := systemd.LogrotatePath
	systemd.UnitPath = filepath.Join(dir, "bunkerd.service")
	systemd.LogrotatePath = filepath.Join(dir, "bunkerd.logrotate")
	defer func() {
		systemd.UnitPath = origUnit
		systemd.LogrotatePath = origLogrotate
	}()

	cmd := newSystemdInstallCommand()
	cmd.SetArgs([]string{
		"--binary", "/usr/local/bin/bunkerd",
		"--config", "/etc/bunkerd/config.yaml",
		"--user", "root",
	})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute install: %v\n%s", err, buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "Installed bunkerd systemd service") {
		t.Errorf("unexpected output: %q", out)
	}

	unitBytes, err := os.ReadFile(systemd.UnitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if !strings.Contains(string(unitBytes), "ExecStart=/usr/local/bin/bunkerd --config=/etc/bunkerd/config.yaml") {
		t.Errorf("unit missing expected ExecStart: %s", unitBytes)
	}
}

func TestSystemdUninstallCommand(t *testing.T) {
	dir := t.TempDir()
	origUnit := systemd.UnitPath
	origLogrotate := systemd.LogrotatePath
	systemd.UnitPath = filepath.Join(dir, "bunkerd.service")
	systemd.LogrotatePath = filepath.Join(dir, "bunkerd.logrotate")
	defer func() {
		systemd.UnitPath = origUnit
		systemd.LogrotatePath = origLogrotate
	}()

	if err := os.WriteFile(systemd.UnitPath, []byte("unit"), 0644); err != nil {
		t.Fatalf("write unit: %v", err)
	}
	if err := os.WriteFile(systemd.LogrotatePath, []byte("logrotate"), 0644); err != nil {
		t.Fatalf("write logrotate: %v", err)
	}

	cmd := newSystemdUninstallCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute uninstall: %v\n%s", err, buf.String())
	}

	if _, err := os.Stat(systemd.UnitPath); !os.IsNotExist(err) {
		t.Error("unit file still exists after uninstall")
	}
}

func TestSystemdStatusCommand_Installed(t *testing.T) {
	dir := t.TempDir()
	origUnit := systemd.UnitPath
	systemd.UnitPath = filepath.Join(dir, "bunkerd.service")
	defer func() {
		systemd.UnitPath = origUnit
	}()

	if err := os.WriteFile(systemd.UnitPath, []byte("unit"), 0644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	cmd := newSystemdStatusCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute status: %v\n%s", err, buf.String())
	}

	if !strings.Contains(buf.String(), "is installed") {
		t.Errorf("expected installed status, got: %s", buf.String())
	}
}

func TestSystemdStatusCommand_NotInstalled(t *testing.T) {
	dir := t.TempDir()
	origUnit := systemd.UnitPath
	systemd.UnitPath = filepath.Join(dir, "bunkerd.service")
	defer func() {
		systemd.UnitPath = origUnit
	}()

	cmd := newSystemdStatusCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute status: %v\n%s", err, buf.String())
	}

	if !strings.Contains(buf.String(), "is not installed") {
		t.Errorf("expected not installed status, got: %s", buf.String())
	}
}
