package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig materialises a YAML config for the loader.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestWebDAVAndH2CKeysLoad proves the three new server keys exist on the wire
// (YAML and env), since a knob the loader cannot see is not a knob.
func TestWebDAVAndH2CKeysLoad(t *testing.T) {
	path := writeConfig(t, `
server:
  grpc_addr: "127.0.0.1:19090"
  rest_addr: "127.0.0.1:18080"
  h2c_enabled: true
  webdav_enabled: true
  webdav_root: "/srv/served-tree"
tls:
  enabled: false
  insecure_dev: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Server.H2CEnabled {
		t.Fatal("h2c_enabled did not load")
	}
	if !cfg.Server.WebDAVEnabled || cfg.Server.WebDAVRoot != "/srv/served-tree" {
		t.Fatalf("webdav keys did not load: enabled=%v root=%q", cfg.Server.WebDAVEnabled, cfg.Server.WebDAVRoot)
	}

	// Env overrides beat the file, like every other server key.
	t.Setenv("BUNKERD_SERVER_WEBDAV_ROOT", "/srv/other-tree")
	t.Setenv("BUNKERD_SERVER_H2C_ENABLED", "false")
	cfgEnv, err := Load(path)
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfgEnv.Server.WebDAVRoot != "/srv/other-tree" {
		t.Fatalf("env override ignored: %q", cfgEnv.Server.WebDAVRoot)
	}
	if cfgEnv.Server.H2CEnabled {
		t.Fatal("env override to false ignored")
	}
}

// TestDefaultsKeepTheNewSurfacesOff proves the additive posture: an existing
// config that says nothing about WebDAV or h2c behaves exactly as before.
func TestDefaultsKeepTheNewSurfacesOff(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Server.H2CEnabled || cfg.Server.WebDAVEnabled || cfg.Server.WebDAVRoot != "" {
		t.Fatalf("new keys are not off by default: %+v", cfg.Server)
	}
}

// TestWebDAVRootValidation proves the fail-before-listen posture: opting in
// without a usable tree is refused at Validate, not discovered as a 500 per
// request.
func TestWebDAVRootValidation(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		root    string
		wantErr string
	}{
		{name: "off, no root", enabled: false, root: ""},
		{name: "on with an absolute root", enabled: true, root: "/srv/tree"},
		{name: "on without a root", enabled: true, root: "", wantErr: "server.webdav_root is required"},
		{name: "on with a relative root", enabled: true, root: "relative/tree", wantErr: "must be an absolute path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Server.WebDAVEnabled = tc.enabled
			cfg.Server.WebDAVRoot = tc.root
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestCheckH2CSaysWhatTheOptInDoes proves the startup note names the real
// caveat: h2c is prior-knowledge only, so the RFC 7540 Upgrade dance silently
// downgrades.
func TestCheckH2CSaysWhatTheOptInDoes(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.CheckH2C(); got != "" {
		t.Fatalf("CheckH2C warned while the opt-in is off: %q", got)
	}
	cfg.Server.H2CEnabled = true
	got := cfg.CheckH2C()
	for _, want := range []string{"h2c", "Upgrade", "HTTP/1.1", cfg.Server.GRPCAddr, cfg.Server.RESTAddr} {
		if !strings.Contains(got, want) {
			t.Fatalf("CheckH2C warning does not mention %q: %s", want, got)
		}
	}
}
