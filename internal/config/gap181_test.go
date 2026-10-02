package config

// GAP-181 tests: the generator-side inline-secret guard and the daemon
// config resolution's no-cwd guarantee (Load never resolves or writes a
// path relative to the process working directory).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmitInlineSecret_RefusesValues is the writer-side guard: a generator
// that would embed a credential is refused (the *_FILE remedy is named);
// an empty value or a path reference passes.
func TestEmitInlineSecret_RefusesValues(t *testing.T) {
	err := EmitInlineSecret("auth.token", "some-real-token-value")
	if err == nil {
		t.Fatal("EmitInlineSecret = nil for an inline value, want the refusal")
	}
	if !strings.Contains(err.Error(), "auth.token_file") || !strings.Contains(err.Error(), "not part of the backup set") {
		t.Errorf("refusal does not name the remedy: %v", err)
	}

	// Empty value: nothing secret, allowed.
	if err := EmitInlineSecret("auth.token", ""); err != nil {
		t.Errorf("EmitInlineSecret(\"\") = %v, want nil", err)
	}
	// Whitespace-only: same.
	if err := EmitInlineSecret("auth.jwt_secret", "   "); err != nil {
		t.Errorf("EmitInlineSecret(blank) = %v, want nil", err)
	}
}

// TestLoad_DaemonConfigIsolationFromCWD: loading a config from an absolute
// path with the process cwd inside an empty repo-free directory resolves
// every documented path to its per-user/system location and creates NOTHING
// in the cwd — the daemon side of the zero-cwd-artifacts guarantee.
func TestLoad_DaemonConfigIsolationFromCWD(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	cfgPath := filepath.Join(base, "daemon", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := "server:\n  grpc_addr: \":9090\"\nauth:\n  enabled: true\n  token: tok\n" +
		"agent:\n  base_data_dir: " + filepath.Join(base, "data") + "\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	// Move the process into the repo-free dir for the Load.
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The secrets dir resolves INSIDE the configured state tree, never the
	// cwd and never a bare relative name.
	dir := cfg.SecretsDirOrDefault()
	if !filepath.IsAbs(dir) {
		t.Errorf("SecretsDirOrDefault = %q, want an absolute path", dir)
	}
	if strings.HasPrefix(dir, base+string(os.PathSeparator)) {
		// Inside the configured tree is correct — but only under data/,
		// which is the state tree the config named.
		if !strings.HasPrefix(dir, filepath.Join(base, "data")) {
			t.Errorf("secrets dir %q escaped the configured state tree", dir)
		}
	} else {
		t.Errorf("secrets dir %q is not inside the configured state tree %q", dir, filepath.Join(base, "data"))
	}

	// Nothing appeared in the cwd.
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "daemon" && e.Name() != "data" {
			t.Errorf("Load created %q in the process cwd", e.Name())
		}
	}
}

// TestLoad_MissingFileFallsBackToDefaultsIsPinned: Load on a missing path
// returns the compiled defaults BY DESIGN (TestLoad_Defaults pins it) — the
// fail-closed boundary for a daemon whose config is MISSING is the
// entrypoint + gates, not the loader. What Load must never do is HIDE a
// present-but-unreadable file: that stays a hard error naming the path.
func TestLoad_MissingFileFallsBackToDefaultsIsPinned(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there.yaml")
	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("Load on a missing path = %v; the defaults fallback is the pinned contract", err)
	}
	if !cfg.Auth.Enabled {
		t.Error("defaults fallback lost auth.enabled=true")
	}

	// A path that EXISTS but is unreadable is a hard error naming it.
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("auth: [unclosed\n  :::yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(bad)
	if err == nil {
		t.Fatal("Load on a malformed file = nil, want a hard error")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error does not name the malformed config path %s: %v", bad, err)
	}
}

// TestResolveSecrets_EmptyTokenFileFailsClosed: the *_FILE indirection with
// an empty file refuses with the exact path — a misconfigured secret file
// never degrades to a silent empty credential.
func TestResolveSecrets_EmptyTokenFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty-token")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &AuthConfig{Enabled: true, TokenFile: empty}
	err := a.ResolveSecrets()
	if err == nil {
		t.Fatal("ResolveSecrets with an empty token file = nil, want the fail-closed refusal")
	}
	if !strings.Contains(err.Error(), empty) {
		t.Errorf("refusal does not name the exact file %s: %v", empty, err)
	}
	// A missing file names the path too.
	a2 := &AuthConfig{Enabled: true, TokenFile: filepath.Join(dir, "gone")}
	err = a2.ResolveSecrets()
	if err == nil {
		t.Fatal("ResolveSecrets with a missing token file = nil, want an error")
	}
	if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "gone") {
		t.Errorf("refusal neither wraps ErrNotExist nor names the path: %v", err)
	}
}
