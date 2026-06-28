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
