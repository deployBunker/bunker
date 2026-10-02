package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultConfig_NetworkModeUnset pins §5.1: the default config does NOT
// name a mode — the zero value resolves through ResolveNetworkMode to the
// declared default ("shared"), keeping every spawn byte-identical.
func TestDefaultConfig_NetworkModeUnset(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Agent.NetworkMode != "" {
		t.Fatalf("DefaultConfig().Agent.NetworkMode = %q, want empty (unset = the declared default)", cfg.Agent.NetworkMode)
	}
	got, err := cfg.ResolveNetworkMode("")
	if err != nil {
		t.Fatalf("ResolveNetworkMode(empty): %v", err)
	}
	if got != NetworkModeShared {
		t.Fatalf("resolved default = %q, want %q", got, NetworkModeShared)
	}
}

// TestResolveNetworkMode table-drives the single precedence resolver:
// flag > BUNKERD_NETWORK_MODE env > config global > declared default, and
// an unknown name from ANY source is a hard error naming the source, the
// offending value and the valid set — never a silent fallback (§5.2).
func TestResolveNetworkMode(t *testing.T) {
	t.Run("flag wins over env and config", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Agent.NetworkMode = NetworkModeSystemd
		t.Setenv(NetworkModeEnv, NetworkModeShared)
		got, err := cfg.ResolveNetworkMode(NetworkModeSystemd)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != NetworkModeSystemd {
			t.Fatalf("flag did not win: got %q, want systemd", got)
		}
	})

	t.Run("env beats config global", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Agent.NetworkMode = NetworkModeShared
		t.Setenv(NetworkModeEnv, NetworkModeSystemd)
		got, err := cfg.ResolveNetworkMode("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != NetworkModeSystemd {
			t.Fatalf("env did not beat config: got %q, want systemd", got)
		}
	})

	t.Run("config global used when flag and env empty", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Agent.NetworkMode = NetworkModeSystemd
		t.Setenv(NetworkModeEnv, "")
		got, err := cfg.ResolveNetworkMode("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != NetworkModeSystemd {
			t.Fatalf("config global ignored: got %q, want systemd", got)
		}
	})

	t.Run("unknown flag refused naming source value and set", func(t *testing.T) {
		cfg := DefaultConfig()
		_, err := cfg.ResolveNetworkMode("pasta")
		if err == nil {
			t.Fatal("unknown flag resolved without error — a silent fallback (§5.2 violation)")
		}
		for _, want := range []string{"--network-mode", "pasta", NetworkModeShared, NetworkModeSystemd} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q missing %q", err, want)
			}
		}
	})

	t.Run("unknown env refused naming the env var", func(t *testing.T) {
		cfg := DefaultConfig()
		t.Setenv(NetworkModeEnv, "rootlesskit")
		_, err := cfg.ResolveNetworkMode("")
		if err == nil {
			t.Fatal("unknown env resolved without error")
		}
		if !strings.Contains(err.Error(), NetworkModeEnv) || !strings.Contains(err.Error(), "rootlesskit") {
			t.Errorf("refusal %q must name the env var and the offending value", err)
		}
	})

	t.Run("unknown config global refused naming the config key", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Agent.NetworkMode = "hidepid"
		t.Setenv(NetworkModeEnv, "")
		_, err := cfg.ResolveNetworkMode("")
		if err == nil {
			t.Fatal("unknown config global resolved without error")
		}
		if !strings.Contains(err.Error(), "agent.network_mode") || !strings.Contains(err.Error(), "hidepid") {
			t.Errorf("refusal %q must name the config key and the offending value", err)
		}
	})
}

// TestValidate_NetworkModeUnknownRefusesToStart pins the fail-at-boot law: a
// typoed agent.network_mode refuses config validation (the daemon never
// starts with a mode it cannot provide) — never a silent resolve at spawn.
func TestValidate_NetworkModeUnknownRefusesToStart(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agent.NetworkMode = "netns"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() accepted an unknown agent.network_mode")
	}
	for _, want := range []string{"agent.network_mode", "netns", NetworkModeShared, NetworkModeSystemd} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error %q missing %q", err, want)
		}
	}
	// Both valid names validate clean.
	for _, m := range []string{NetworkModeShared, NetworkModeSystemd} {
		cfg := DefaultConfig()
		cfg.Agent.NetworkMode = m
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() rejected valid mode %q: %v", m, err)
		}
	}
}

// TestLoad_NetworkModeFromEnv pins the viper surface: BUNKERD_NETWORK_MODE
// overrides the config file for the daemon-level default.
func TestLoad_NetworkModeFromEnv(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
agent:
  network_mode: shared
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("BUNKERD_NETWORK_MODE", NetworkModeSystemd)
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := cfg.ResolveNetworkMode("")
	if err != nil {
		t.Fatalf("ResolveNetworkMode: %v", err)
	}
	if got != NetworkModeSystemd {
		t.Fatalf("env override ignored: resolved %q, want systemd", got)
	}
}

// TestLoad_NetworkModeFromFileAndUnknownFails pins the file surface and its
// fail-at-boot refusal.
func TestLoad_NetworkModeFromFileAndUnknownFails(t *testing.T) {
	t.Run("file value loads and resolves", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		content := `
agent:
  network_mode: systemd
`
		if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		t.Setenv("BUNKERD_NETWORK_MODE", "")
		cfg, err := Load(cfgPath)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got, err := cfg.ResolveNetworkMode("")
		if err != nil {
			t.Fatalf("ResolveNetworkMode: %v", err)
		}
		if got != NetworkModeSystemd {
			t.Fatalf("file value ignored: resolved %q, want systemd", got)
		}
	})

	t.Run("unknown file value refuses to start", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		content := `
agent:
  network_mode: pasta
`
		if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		t.Setenv("BUNKERD_NETWORK_MODE", "")
		cfg, err := Load(cfgPath)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// The daemon's refuse-to-start gate is Config.Validate (server Run
		// calls it before any listener opens); a typoed mode must stop the
		// boot there (§5.2) — never resolve to a different boundary at spawn.
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate accepted an unknown agent.network_mode — must refuse to start (§5.2)")
		}
	})
}
