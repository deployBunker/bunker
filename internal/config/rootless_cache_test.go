package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_RootlessInstallerCacheDir covers GAP-091: the rootless installer
// cache dir resolves env > config file > default, and an EMPTY env var counts
// as unset so it never clobbers an explicit config value. Every case starts
// from a config file that sets a distinctive value, so a default leaking
// through (or the default masquerading as a config hit) fails loudly.
func TestLoad_RootlessInstallerCacheDir(t *testing.T) {
	const fileValue = "/data/cache/from-file"

	writeCfg := func(t *testing.T) string {
		t.Helper()
		return writeTestConfigFile(t, `
auth:
  enabled: true
  token: "test-token"
agent:
  rootless_installer_cache_dir: "`+fileValue+`"
`)
	}

	tests := []struct {
		name     string
		env      string
		envEmpty bool // export the env var as "" (must count as unset)
		want     string
		wantErr  bool
	}{
		{name: "config file wins when env unset", env: "", want: fileValue},
		// An empty env var is UNSET by convention: it must not clobber the
		// config file with "" (which would silently disarm the cache).
		{name: "empty env counts as unset", env: "", envEmpty: true, want: fileValue},
		{name: "env wins over config file", env: "/data/cache/from-env", want: "/data/cache/from-env"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envEmpty {
				t.Setenv("BUNKERD_AGENT_ROOTLESS_INSTALLER_CACHE_DIR", "")
			} else if tc.env != "" {
				t.Setenv("BUNKERD_AGENT_ROOTLESS_INSTALLER_CACHE_DIR", tc.env)
			}

			cfg, err := Load(writeCfg(t))
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.Agent.RootlessInstallerCacheDir != tc.want {
				t.Errorf("rootless_installer_cache_dir = %q, want %q", cfg.Agent.RootlessInstallerCacheDir, tc.want)
			}
		})
	}

	t.Run("default arms the cache without any config file", func(t *testing.T) {
		cfg, err := Load("/nonexistent/path/config.yaml")
		if err != nil {
			t.Fatalf("load defaults: %v", err)
		}
		if cfg.Agent.RootlessInstallerCacheDir != "/var/cache/bunker/rootless-installer" {
			t.Errorf("default rootless_installer_cache_dir = %q, want /var/cache/bunker/rootless-installer", cfg.Agent.RootlessInstallerCacheDir)
		}
	})
}

// TestLoad_RootlessInstallerCacheDir_ExplicitEmptyOptsOut proves the
// documented opt-out: a config file that explicitly sets
// rootless_installer_cache_dir: "" (with no env) yields the empty string,
// which keeps the legacy uncached download path in package agent.
func TestLoad_RootlessInstallerCacheDir_ExplicitEmptyOptsOut(t *testing.T) {
	t.Setenv("BUNKER_ROOTLESS_INSTALLER_CACHE_DIR", "") // must NOT re-arm

	cfg, err := Load(writeTestConfigFile(t, `
auth:
  enabled: true
  token: "test-token"
agent:
  rootless_installer_cache_dir: ""
`))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Agent.RootlessInstallerCacheDir != "" {
		t.Fatalf("explicit empty rootless_installer_cache_dir = %q, want \"\" (legacy uncached path)", cfg.Agent.RootlessInstallerCacheDir)
	}
}

// writeTestConfigFile writes content to a temp config file (same shape as
// cmd/bunkerd's writeTestConfig, kept package-local so this file has no
// dependency on the command's test harness).
func writeTestConfigFile(t *testing.T, content string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}
