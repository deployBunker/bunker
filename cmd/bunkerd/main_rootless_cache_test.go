//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
)

// TestRunArmsRootlessInstallerCacheDir covers GAP-091 at the daemon entry
// point: run() must arm agent.SetRootlessInstallerCacheDir from the resolved
// configuration before serving, with the precedence env >
// agent.rootless_installer_cache_dir > DefaultConfig default, and an EMPTY
// env var counts as unset (it never clobbers the config file). The one safe
// way to stop run() AFTER config.Load + arming without binding ports is the
// auth gate: auth.enabled with NO credential makes CheckAuth refuse (the
// same post-config stop TestBunkerdPositionalArgs's configPathSignatures
// marks as "refusing to start"), and the arming block sits immediately
// before that gate.
func TestRunArmsRootlessInstallerCacheDir(t *testing.T) {
	restoreArmed := func(t *testing.T) {
		t.Helper()
		prev := agent.GetRootlessInstallerCacheDir()
		t.Cleanup(func() { agent.SetRootlessInstallerCacheDir(prev) })
	}
	setEnv := func(t *testing.T, val string) {
		t.Helper()
		if val == "" {
			t.Setenv(agent.RootlessInstallerCacheDirEnv, "")
			return
		}
		t.Setenv(agent.RootlessInstallerCacheDirEnv, val)
	}
	runForConfig := func(t *testing.T, cfgPath string) {
		t.Helper()
		setArgs(t, "--config", cfgPath)
		err := run()
		if err == nil || !strings.Contains(err.Error(), "refusing to start") {
			t.Fatalf("run did not stop at the post-config auth gate (want 'refusing to start'), got: %v", err)
		}
	}

	tests := []struct {
		name string
		// config file fragment for the agent section ("" = omit the section)
		cfgAgent string
		env      string
		envEmpty bool
		want     string
	}{
		{
			name:     "default config arms the documented default",
			cfgAgent: "",
			want:     "/var/cache/bunker/rootless-installer",
		},
		{
			name:     "config file value wins over default",
			cfgAgent: "  rootless_installer_cache_dir: /data/cache/from-file\n",
			want:     "/data/cache/from-file",
		},
		{
			name:     "env wins over config file",
			cfgAgent: "  rootless_installer_cache_dir: /data/cache/from-file\n",
			env:      "/data/cache/from-env",
			want:     "/data/cache/from-env",
		},
		{
			name:     "empty env counts as unset, config value stays",
			cfgAgent: "  rootless_installer_cache_dir: /data/cache/from-file\n",
			envEmpty: true,
			want:     "/data/cache/from-file",
		},
		{
			name:     "empty env counts as unset, default stays",
			envEmpty: true,
			want:     "/var/cache/bunker/rootless-installer",
		},
		{
			// The documented opt-out: an explicit empty config value with no
			// env keeps the legacy uncached download path.
			name:     "explicit empty config value disarms the cache",
			cfgAgent: "  rootless_installer_cache_dir: \"\"\n",
			want:     "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restoreArmed(t)
			setEnv(t, tc.env)

			// auth.enabled=true with no token: config.Load + the GAP-091
			// arming run, then CheckAuth refuses — nothing binds a port.
			cfgPath := writeTestConfig(t, `
auth:
  enabled: true
agent:
`+tc.cfgAgent)
			runForConfig(t, cfgPath)

			if got := agent.GetRootlessInstallerCacheDir(); got != tc.want {
				t.Fatalf("armed rootless installer cache dir = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunArmsRootlessInstallerCacheDir_UnsetEnvKeepsConfig proves the env
// override is a real override and not a clobber: with the env var ABSENT (not
// merely empty) the config file value is what lands in the agent seam.
func TestRunArmsRootlessInstallerCacheDir_UnsetEnvKeepsConfig(t *testing.T) {
	if _, ok := os.LookupEnv(agent.RootlessInstallerCacheDirEnv); ok {
		t.Setenv(agent.RootlessInstallerCacheDirEnv, "") // normalize, empty = unset
	}
	prev := agent.GetRootlessInstallerCacheDir()
	t.Cleanup(func() { agent.SetRootlessInstallerCacheDir(prev) })

	cfgPath := writeTestConfig(t, `
auth:
  enabled: true
agent:
  rootless_installer_cache_dir: `+filepath.ToSlash(filepath.Join(t.TempDir(), "cfg-cache"))+`
`)
	setArgs(t, "--config", cfgPath)
	err := run()
	if err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("run did not stop at the post-config auth gate, got: %v", err)
	}

	want := config.DefaultConfig().Agent.RootlessInstallerCacheDir
	if got := agent.GetRootlessInstallerCacheDir(); got == want {
		t.Fatalf("config file value did not override the default: armed = %q == default %q", got, want)
	}
}
