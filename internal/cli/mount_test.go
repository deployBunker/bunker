package cli

import (
	"testing"
)

func TestNewMountCommand_Structure(t *testing.T) {
	cmd := NewMountCommand()

	if cmd.Use != "mount <agent-id> [mountpoint]" {
		t.Errorf("Use = %q, want %q", cmd.Use, "mount <agent-id> [mountpoint]")
	}
	if cmd.Short != "Mount an agent's home directory via SSHFS" {
		t.Errorf("Short = %q", cmd.Short)
	}
	// Args: cobra.RangeArgs(1, 2) — minimum 1, maximum 2
	if cmd.Args == nil {
		t.Fatal("Args is nil, expected RangeArgs(1, 2)")
	}
}

func TestNewMountCommand_ArgsValidation_NoArgs(t *testing.T) {
	cmd := NewMountCommand()
	// RangeArgs(1, 2) should reject 0 args
	if cmd.Args(cmd, []string{}) == nil {
		t.Error("expected error for 0 args, got nil")
	}
}

func TestNewMountCommand_ArgsValidation_OneArg(t *testing.T) {
	cmd := NewMountCommand()
	// RangeArgs(1, 2) should accept 1 arg
	if err := cmd.Args(cmd, []string{"agent-1"}); err != nil {
		t.Errorf("unexpected error for 1 arg: %v", err)
	}
}

func TestNewMountCommand_ArgsValidation_TwoArgs(t *testing.T) {
	cmd := NewMountCommand()
	// RangeArgs(1, 2) should accept 2 args
	if err := cmd.Args(cmd, []string{"agent-1", "/tmp/mnt"}); err != nil {
		t.Errorf("unexpected error for 2 args: %v", err)
	}
}

func TestNewMountCommand_ArgsValidation_ThreeArgs(t *testing.T) {
	cmd := NewMountCommand()
	// RangeArgs(1, 2) should reject 3 args
	if cmd.Args(cmd, []string{"agent-1", "/tmp/mnt", "extra"}) == nil {
		t.Error("expected error for 3 args, got nil")
	}
}

func TestNewMountCommand_ServerFlag(t *testing.T) {
	cmd := NewMountCommand()
	flag := cmd.Flags().Lookup("server")
	if flag == nil {
		t.Fatal("--server flag not registered")
	}
	if flag.Name != "server" {
		t.Errorf("flag name = %q, want %q", flag.Name, "server")
	}
}

func TestNewMountCommand_RunE_NoActiveServer(t *testing.T) {
	// Clear any active server config.
	cfg, err := LoadCLIConfig()
	if err != nil {
		t.Skipf("cannot load config: %v", err)
	}
	cfg.ActiveServer = ""
	cfg.Servers = map[string]ServerEntry{}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Skipf("cannot save config: %v", err)
	}
	defer func() {
		// Restore empty config
		cfg.ActiveServer = ""
		cfg.Servers = map[string]ServerEntry{}
		_ = SaveCLIConfig(cfg)
	}()

	cmd := NewMountCommand()
	cmd.SetArgs([]string{"test-agent"})
	err = cmd.Execute()
	if err == nil {
		t.Error("expected error for no active server, got nil")
	}
}

func TestNewMountCommand_RunE_ServerNotFound(t *testing.T) {
	cfg, err := LoadCLIConfig()
	if err != nil {
		t.Skipf("cannot load config: %v", err)
	}
	cfg.ActiveServer = "nonexistent"
	cfg.Servers = map[string]ServerEntry{}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Skipf("cannot save config: %v", err)
	}
	defer func() {
		cfg.ActiveServer = ""
		cfg.Servers = map[string]ServerEntry{}
		_ = SaveCLIConfig(cfg)
	}()

	cmd := NewMountCommand()
	cmd.SetArgs([]string{"test-agent"})
	err = cmd.Execute()
	if err == nil {
		t.Error("expected error for server not found, got nil")
	}
}
