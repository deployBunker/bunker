package main

// GAP-181 tests: the --show-paths diagnostic surface is WIRED into run()
// (the flag parses, prints, and exits without binding), the diagnostic never
// prints secret values, and the entrypoint creates nothing in the cwd.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
)

// TestRun_ShowPaths_PrintsAndExitsWithoutBinding: a config whose serve path
// WOULD bind (valid addresses) must print the path surface and return nil —
// the diagnostic exits before any listener — and must not have created
// anything in the process cwd.
func TestRun_ShowPaths_PrintsAndExitsWithoutBinding(t *testing.T) {
	base := t.TempDir()
	tokenFile := filepath.Join(base, "secrets", "token")
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		t.Fatal(err)
	}
	const daemonToken = "show-paths-token-value-9988aabbccddeeff"
	if err := os.WriteFile(tokenFile, []byte(daemonToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(base, "daemon", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := "server:\n  grpc_addr: \"127.0.0.1:0\"\n  rest_addr: \"127.0.0.1:0\"\n" +
		"auth:\n  enabled: true\n  token_file: " + tokenFile + "\n" +
		"agent:\n  base_data_dir: " + filepath.Join(base, "data") + "\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	// Isolate every ambient secret/env tier AND the CLI-side HOME (the CLI
	// rows must not read the operator's real config).
	t.Setenv("HOME", base)
	for _, k := range []string{config.AuthTokenFileEnv, config.AuthJWTSecretFileEnv, config.SecretsDirEnv} {
		prev, had := os.LookupEnv(k)
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(k, prev)
				return
			}
			_ = os.Unsetenv(k)
		})
	}

	setArgs(t, "--show-paths", "--config", cfgPath)
	var out string
	var runErr error
	out = captureStdout(t, func() {
		runErr = run()
	})
	if runErr != nil {
		t.Fatalf("run --show-paths: %v", runErr)
	}
	if !strings.Contains(out, "bunkerd --show-paths") {
		t.Errorf("output missing the diagnostic header:\n%s", out)
	}
	// The token file's LOCATION is printed; its VALUE never is. The table
	// truncates long paths, so assert on the row LABELS (which name each
	// component and its rule) plus the short system paths in full.
	if !strings.Contains(out, "auth token (master API credential)") {
		t.Errorf("output has no auth-token row:\n%s", out)
	}
	if strings.Contains(out, daemonToken) {
		t.Errorf("--show-paths LEAKED the auth token value:\n%s", out)
	}
	if !strings.Contains(out, "generated secrets directory") {
		t.Errorf("output has no secrets-dir row:\n%s", out)
	}
	// The rules vocabulary appears.
	for _, want := range []string{"config", "system", "xdg"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing the %q rule label:\n%s", want, out)
		}
	}
}

// TestRun_ShowPaths_ConfigError: a broken config refuses with the loader's
// error — the diagnostic never silently invents paths.
func TestRun_ShowPaths_ConfigError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("auth: [unclosed\n  :::yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	setArgs(t, "--show-paths", "--config", bad)
	err := run()
	if err == nil {
		t.Fatal("run --show-paths = nil on a broken config, want the load error")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Errorf("error = %v, want the load-config refusal", err)
	}
}

// TestRun_NoCWDStateFiles: a config error run (the cheapest full-entrypoint
// path that touches config.Load and the auth gate) leaves the cwd untouched
// — the fail-before-listen path writes nothing into the process folder.
func TestRun_NoCWDStateFiles(t *testing.T) {
	base := t.TempDir()
	// Empty token file: the auth *_FILE indirection must refuse loudly.
	tokenFile := filepath.Join(base, "token")
	if err := os.WriteFile(tokenFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.AuthTokenFileEnv, tokenFile)
	cfgPath := filepath.Join(base, "config.yaml")
	yaml := "server:\n  grpc_addr: \"127.0.0.1:0\"\nauth:\n  enabled: true\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	setArgs(t, "--config", cfgPath)
	err := run()
	if err == nil {
		t.Fatal("run = nil with an empty token file, want the fail-closed refusal")
	}
	if !strings.Contains(err.Error(), tokenFile) {
		t.Errorf("refusal does not name the exact token file %s: %v", tokenFile, err)
	}
	// The daemon config writer created config.yaml + token; the RUN itself
	// must not have created anything else (no jwt_secret, no state files).
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "token" && e.Name() != "config.yaml" {
			t.Errorf("run created %q next to the config — a cwd-state artifact", e.Name())
		}
	}
}
