package cli

import (
	"strings"
	"testing"
)

// TestUseCommand tests the 'bunker use' command across multiple scenarios.
func TestUseCommand(t *testing.T) {
	// Helper to create a config with the given servers and active server.
	saveCfg := func(t *testing.T, servers map[string]ServerEntry, active string) {
		t.Helper()
		if servers == nil {
			servers = make(map[string]ServerEntry)
		}
		cfg := &CLIConfig{
			Servers:      servers,
			ActiveServer: active,
		}
		if err := SaveCLIConfig(cfg); err != nil {
			t.Fatalf("SaveCLIConfig: %v", err)
		}
	}

	twoServers := map[string]ServerEntry{
		"alpha": {
			Name:        "alpha",
			URL:         "http://alpha.example.com:9090",
			Token:       "tok1",
			ConnectedAt: "2026-01-01T00:00:00Z",
		},
		"beta": {
			Name:        "beta",
			URL:         "https://beta.example.com",
			Token:       "tok2",
			ConnectedAt: "2026-06-01T00:00:00Z",
		},
	}

	tests := []struct {
		name       string
		setup      func(t *testing.T)
		args       []string
		wantErr    bool
		wantSubstr string // substring expected in stdout (on success)
		wantErrSub string // substring expected in error message (on failure)
		verify     func(t *testing.T)
	}{
		{
			name: "success switches active server",
			setup: func(t *testing.T) {
				saveCfg(t, twoServers, "alpha")
			},
			args:       []string{"beta"},
			wantErr:    false,
			wantSubstr: `Active server set to "beta" (https://beta.example.com)`,
			verify: func(t *testing.T) {
				cfg, err := LoadCLIConfig()
				if err != nil {
					t.Fatalf("LoadCLIConfig: %v", err)
				}
				if cfg.ActiveServer != "beta" {
					t.Errorf("ActiveServer = %q, want %q", cfg.ActiveServer, "beta")
				}
			},
		},
		{
			name: "no args returns error",
			setup: func(t *testing.T) {
				saveCfg(t, twoServers, "alpha")
			},
			args:    []string{},
			wantErr: true,
		},
		{
			name: "non-existent server returns not found error",
			setup: func(t *testing.T) {
				saveCfg(t, twoServers, "alpha")
			},
			args:       []string{"nonexistent"},
			wantErr:    true,
			wantErrSub: `server "nonexistent" not found`,
		},
		{
			name: "empty config returns helpful message",
			setup: func(t *testing.T) {
				saveCfg(t, nil, "")
			},
			args:       []string{"anything"},
			wantErr:    true,
			wantErrSub: "no servers configured",
		},
		{
			name: "already active prints confirmation",
			setup: func(t *testing.T) {
				saveCfg(t, twoServers, "alpha")
			},
			args:       []string{"alpha"},
			wantErr:    false,
			wantSubstr: `Active server set to "alpha"`,
			verify: func(t *testing.T) {
				cfg, err := LoadCLIConfig()
				if err != nil {
					t.Fatalf("LoadCLIConfig: %v", err)
				}
				if cfg.ActiveServer != "alpha" {
					t.Errorf("ActiveServer = %q, want %q", cfg.ActiveServer, "alpha")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use a fresh temp dir for each subtest to avoid cross-test config leakage.
			t.Setenv("HOME", t.TempDir())
			tt.setup(t)

			cmd := NewUseCommand()
			cmd.SetArgs(tt.args)

			var output string
			output = captureStdout(t, func() {
				err := cmd.Execute()
				if tt.wantErr && err == nil {
					t.Fatal("expected error, got nil")
				}
				if !tt.wantErr && err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tt.wantErr && tt.wantErrSub != "" && err != nil {
					if !strings.Contains(err.Error(), tt.wantErrSub) {
						t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrSub)
					}
				}
			})

			if !tt.wantErr && tt.wantSubstr != "" {
				if !strings.Contains(output, tt.wantSubstr) {
					t.Errorf("output %q does not contain %q", output, tt.wantSubstr)
				}
			}

			if tt.verify != nil {
				tt.verify(t)
			}
		})
	}
}

// TestUseCommand_ListsAvailableOnError verifies the error message includes
// the sorted list of available servers.
func TestUseCommand_ListsAvailableOnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"zulu":  {Name: "zulu", URL: "http://z:9090"},
			"alpha": {Name: "alpha", URL: "http://a:9090"},
			"mike":  {Name: "mike", URL: "http://m:9090"},
		},
		ActiveServer: "alpha",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewUseCommand()
	cmd.SetArgs([]string{"charlie"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-existent server")
	}

	// Should list all three servers, sorted alphabetically.
	if !strings.Contains(err.Error(), "alpha") {
		t.Errorf("error should list 'alpha': %v", err)
	}
	if !strings.Contains(err.Error(), "mike") {
		t.Errorf("error should list 'mike': %v", err)
	}
	if !strings.Contains(err.Error(), "zulu") {
		t.Errorf("error should list 'zulu': %v", err)
	}
}
