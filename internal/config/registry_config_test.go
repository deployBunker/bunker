package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/deployBunker/bunker/internal/registry"
)

// TestRegistryConfigDefaults pins the documented defaults.
func TestRegistryConfigDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Agent.Registry.Enabled {
		t.Error("the durable registry must be enabled by default")
	}
	if cfg.Agent.Registry.Path != "/var/lib/bunkerd/agents.jsonl" {
		t.Errorf("registry path = %q, want /var/lib/bunkerd/agents.jsonl", cfg.Agent.Registry.Path)
	}
	if cfg.Agent.Registry.MaxBytes != 5<<20 {
		t.Errorf("registry max_bytes = %d, want 5 MiB", cfg.Agent.Registry.MaxBytes)
	}
	if cfg.Agent.Registry.MaxBackups != 3 {
		t.Errorf("registry max_backups = %d, want 3", cfg.Agent.Registry.MaxBackups)
	}
	if cfg.Agent.Registry.KnownIDCap <= 0 {
		t.Error("registry known_id_cap must default to a positive bound")
	}
	if cfg.Agent.Reconciliation.Mode != ReconcileModeDestroy {
		t.Errorf("reconciliation mode = %q, want %q (default)", cfg.Agent.Reconciliation.Mode, ReconcileModeDestroy)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("default config must validate: %v", err)
	}
}

// TestConfigConstantsMatchRegistryPackage keeps the duplicated defaults from
// drifting apart (config cannot import the registry package's constants
// without a cycle, so the equality is pinned by test).
func TestConfigConstantsMatchRegistryPackage(t *testing.T) {
	if DefaultRegistryPath != registry.DefaultPath {
		t.Errorf("config path %q != registry path %q", DefaultRegistryPath, registry.DefaultPath)
	}
	if DefaultRegistryMaxBytes != registry.DefaultMaxBytes {
		t.Errorf("config max bytes %d != registry %d", DefaultRegistryMaxBytes, registry.DefaultMaxBytes)
	}
	if DefaultRegistryMaxBackups != registry.DefaultMaxBackups {
		t.Errorf("config backups %d != registry %d", DefaultRegistryMaxBackups, registry.DefaultMaxBackups)
	}
	if DefaultRegistryKnownIDCap != registry.DefaultKnownIDCap {
		t.Errorf("config known cap %d != registry %d", DefaultRegistryKnownIDCap, registry.DefaultKnownIDCap)
	}
}

// TestRegistryConfigDefaultsHelperFillsZeros covers files that only set a path.
func TestRegistryConfigDefaultsHelperFillsZeros(t *testing.T) {
	r := RegistryConfig{Path: "/tmp/other.jsonl"}
	r.Defaults()
	if r.Path != "/tmp/other.jsonl" {
		t.Errorf("Defaults() overwrote an explicit path: %q", r.Path)
	}
	if r.MaxBytes != DefaultRegistryMaxBytes || r.MaxBackups != DefaultRegistryMaxBackups || r.KnownIDCap != DefaultRegistryKnownIDCap {
		t.Errorf("Defaults() did not fill zero caps: %+v", r)
	}
}

// TestReconciliationModeValidation accepts the two documented modes and
// rejects anything else.
func TestReconciliationModeValidation(t *testing.T) {
	cases := []struct {
		mode    string
		wantErr bool
		want    string
	}{
		{mode: "", wantErr: false, want: ReconcileModeDestroy},
		{mode: ReconcileModeDestroy, wantErr: false, want: ReconcileModeDestroy},
		{mode: ReconcileModeAdopt, wantErr: false, want: ReconcileModeAdopt},
		{mode: "banana", wantErr: true},
	}
	for _, tc := range cases {
		r := ReconciliationConfig{Mode: tc.mode}
		err := r.Validate()
		if tc.wantErr {
			if err == nil {
				t.Errorf("mode %q: want a validation error", tc.mode)
			}
			continue
		}
		if err != nil {
			t.Errorf("mode %q: unexpected error %v", tc.mode, err)
		}
		if r.Mode != tc.want {
			t.Errorf("mode %q normalised to %q, want %q", tc.mode, r.Mode, tc.want)
		}
	}
	if got := (&ReconciliationConfig{}).ModeOrDestroy(); got != ReconcileModeDestroy {
		t.Errorf("ModeOrDestroy() = %q, want %q", got, ReconcileModeDestroy)
	}
}

// TestConfigValidateRejectsBadMode proves a hand-written config fails loudly at
// startup instead of silently reconciling with an unknown policy.
func TestConfigValidateRejectsBadMode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agent.Reconciliation.Mode = "explode"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate must reject an unknown reconciliation mode")
	}
}

// TestLoadRegistryFromFileAndEnv covers YAML and env-var wiring.
func TestLoadRegistryFromFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "agent:\n" +
		"  registry:\n" +
		"    enabled: true\n" +
		"    path: /tmp/from-file.jsonl\n" +
		"    max_bytes: 1024\n" +
		"    max_backups: 2\n" +
		"  reconciliation:\n" +
		"    mode: adopt\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.Registry.Path != "/tmp/from-file.jsonl" {
		t.Errorf("path = %q, want /tmp/from-file.jsonl", cfg.Agent.Registry.Path)
	}
	if cfg.Agent.Registry.MaxBytes != 1024 || cfg.Agent.Registry.MaxBackups != 2 {
		t.Errorf("caps = %d/%d, want 1024/2", cfg.Agent.Registry.MaxBytes, cfg.Agent.Registry.MaxBackups)
	}
	if cfg.Agent.Reconciliation.Mode != ReconcileModeAdopt {
		t.Errorf("mode = %q, want adopt", cfg.Agent.Reconciliation.Mode)
	}

	// Env override wins over the file (BUNKERD_AGENT_REGISTRY_PATH).
	t.Setenv("BUNKERD_AGENT_REGISTRY_PATH", "/tmp/from-env.jsonl")
	cfg2, err := Load(path)
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg2.Agent.Registry.Path != "/tmp/from-env.jsonl" {
		t.Errorf("env override not applied: %q", cfg2.Agent.Registry.Path)
	}
	if err := cfg2.Validate(); err != nil {
		t.Errorf("loaded config must validate: %v", err)
	}
}
