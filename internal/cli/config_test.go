package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveAndLoadConfig(t *testing.T) {
	tmpDir := t.TempDir()

	// Override home for this test.
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpDir)
	defer func() { os.Setenv("HOME", origHome) }()

	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"test-server": {
				Name:        "test-server",
				URL:         "http://localhost:9090",
				Token:       "abc123",
				TLSInsecure: true,
				ConnectedAt: "2026-06-28T00:00:00Z",
			},
		},
		ActiveServer: "test-server",
	}

	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	// Verify file exists.
	cfgPath := filepath.Join(tmpDir, ".bunker", "config.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config file not found: %v", err)
	}

	loaded, err := LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}

	if loaded.ActiveServer != "test-server" {
		t.Errorf("ActiveServer = %q, want %q", loaded.ActiveServer, "test-server")
	}

	entry, ok := loaded.Servers["test-server"]
	if !ok {
		t.Fatal("expected server entry 'test-server'")
	}
	if entry.URL != "http://localhost:9090" {
		t.Errorf("URL = %q, want %q", entry.URL, "http://localhost:9090")
	}
	if entry.Token != "abc123" {
		t.Errorf("Token = %q, want %q", entry.Token, "abc123")
	}
	if !entry.TLSInsecure {
		t.Error("TLSInsecure should be true")
	}
}

func TestDefaultConfig(t *testing.T) {
	// Use a temp dir that has no .bunker/config.yaml.
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpDir)
	defer func() { os.Setenv("HOME", origHome) }()

	cfg, err := LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}

	if cfg.ActiveServer != "" {
		t.Errorf("ActiveServer = %q, want empty", cfg.ActiveServer)
	}
	if cfg.Servers == nil {
		t.Error("Servers map should be non-nil")
	}
	if len(cfg.Servers) != 0 {
		t.Errorf("Servers has %d entries, want 0", len(cfg.Servers))
	}
}

func TestConfigRoundTrip_MultipleServers(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpDir)
	defer func() { os.Setenv("HOME", origHome) }()

	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"alpha": {
				Name:        "alpha",
				URL:         "http://alpha:9090",
				Token:       "tok1",
				TLSInsecure: false,
				ConnectedAt: "2026-01-01T00:00:00Z",
			},
			"beta": {
				Name:        "beta",
				URL:         "https://beta.example.com",
				Token:       "tok2",
				TLSInsecure: false,
				ConnectedAt: "2026-06-01T00:00:00Z",
			},
		},
		ActiveServer: "beta",
	}

	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	loaded, err := LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}

	if loaded.ActiveServer != "beta" {
		t.Errorf("ActiveServer = %q", loaded.ActiveServer)
	}
	if len(loaded.Servers) != 2 {
		t.Errorf("expected 2 servers, got %d", len(loaded.Servers))
	}
}

// TestConfigRoundTrip_MixedCaseHostname guards against the viper
// lowercasing bug (DF-BUNKER-1): SaveCLIConfig must write and
// LoadCLIConfig must read back a server name whose case is preserved,
// and the active-server lookup must resolve.
func TestConfigRoundTrip_MixedCaseHostname(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const name = "karaHermes-mde-7840hs"

	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			name: {
				Name:        name,
				URL:         "http://karaHermes-mde-7840hs:9090",
				Token:       "tok-mixed",
				TLSInsecure: true,
				ConnectedAt: "2026-09-13T00:00:00Z",
			},
		},
		ActiveServer: name,
	}

	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	loaded, err := LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}

	if loaded.ActiveServer != name {
		t.Errorf("ActiveServer = %q, want exact case %q", loaded.ActiveServer, name)
	}

	entry, ok := loaded.Servers[name]
	if !ok {
		keys := make([]string, 0, len(loaded.Servers))
		for k := range loaded.Servers {
			keys = append(keys, k)
		}
		t.Fatalf("Servers map lost exact-case key %q (have %v)", name, keys)
	}
	if entry.URL != "http://karaHermes-mde-7840hs:9090" {
		t.Errorf("entry.URL = %q", entry.URL)
	}
	if entry.Token != "tok-mixed" {
		t.Errorf("entry.Token = %q", entry.Token)
	}

	// The lookup every command performs: Servers[ActiveServer].
	if resolved, ok := loaded.Servers[loaded.ActiveServer]; !ok {
		t.Fatalf("Servers[ActiveServer=%q] not found — mixed-case name does not resolve", loaded.ActiveServer)
	} else if resolved.Name != name {
		t.Errorf("resolved.Name = %q, want %q", resolved.Name, name)
	}
}

// TestLoadCLIConfig_HealsLegacyMixedCaseConfig verifies that a config file
// written by the old viper path (original-case map keys on disk) is read
// back with its case intact, so already-registered mixed-case servers
// start resolving again with no migration.
func TestLoadCLIConfig_HealsLegacyMixedCaseConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	const legacyYAML = `servers:
  karaHermes-mde-7840hs:
    name: karaHermes-mde-7840hs
    url: http://karaHermes-mde-7840hs:9090
    token: tok-legacy
    tls_insecure: true
    connected_at: "2026-09-01T12:00:00Z"
active_server: karaHermes-mde-7840hs
`
	cfgDir := filepath.Join(home, ".bunker")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(legacyYAML), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	loaded, err := LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}

	const name = "karaHermes-mde-7840hs"
	if loaded.ActiveServer != name {
		t.Errorf("ActiveServer = %q, want %q", loaded.ActiveServer, name)
	}
	entry, ok := loaded.Servers[name]
	if !ok {
		t.Fatalf("legacy exact-case server %q not found in loaded config", name)
	}
	if entry.Token != "tok-legacy" {
		t.Errorf("entry.Token = %q, want tok-legacy", entry.Token)
	}
	if !entry.TLSInsecure {
		t.Error("entry.TLSInsecure = false, want true")
	}
	if resolved, ok := loaded.Servers[loaded.ActiveServer]; !ok {
		t.Fatalf("Servers[ActiveServer=%q] not found — legacy config not healed", loaded.ActiveServer)
	} else if resolved.URL != "http://karaHermes-mde-7840hs:9090" {
		t.Errorf("resolved.URL = %q", resolved.URL)
	}
}

// TestConfigRoundTrip_LowercasePreserved pins existing behavior: an
// all-lowercase server name must round-trip exactly as before the fix.
func TestConfigRoundTrip_LowercasePreserved(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const name = "kara-hermes-mde-7840hs"

	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			name: {
				Name:        name,
				URL:         "http://127.0.0.1:9090",
				Token:       "tok-lower",
				TLSInsecure: false,
				ConnectedAt: "2026-09-13T00:00:00Z",
			},
		},
		ActiveServer: name,
	}

	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	loaded, err := LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}

	if loaded.ActiveServer != name {
		t.Errorf("ActiveServer = %q, want %q", loaded.ActiveServer, name)
	}
	entry, ok := loaded.Servers[name]
	if !ok {
		t.Fatalf("lowercase server %q not found after round-trip", name)
	}
	if entry.URL != "http://127.0.0.1:9090" {
		t.Errorf("entry.URL = %q", entry.URL)
	}
	if resolved, ok := loaded.Servers[loaded.ActiveServer]; !ok || resolved.Name != name {
		t.Fatalf("Servers[ActiveServer] did not resolve for lowercase name")
	}
}
