package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
)

// TestLongHomePrefix_QA73 reproduces the QA-BUNKER-73 scenario: a HOME whose
// prefix exceeds 80 bytes, then asserts the parent-dir prefix of the secret
// locations appears in the rendered output.
func TestLongHomePrefix_QA73(t *testing.T) {
	base := filepath.Join(t.TempDir(), strings.Repeat("verylonghomedirsegment/", 6))
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Logf("HOME length = %d bytes: %s", len(base), base)
	if len(base) <= 80 {
		t.Fatalf("test setup: HOME prefix is only %d bytes, need >80", len(base))
	}
	t.Setenv("HOME", base)
	setenvForTest(t, "BUNKER_HOME", "")
	setenvForTest(t, "XDG_CONFIG_HOME", "")
	resetPathOverrides(t)

	secretsDir := filepath.Join(base, "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(secretsDir, "token")
	jwtPath := filepath.Join(secretsDir, "jwt_secret")
	if err := os.WriteFile(tokenPath, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jwtPath, []byte("jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dCfg := filepath.Join(base, "daemon", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(dCfg), 0o700); err != nil {
		t.Fatal(err)
	}
	dYAML := "server:\n  grpc_addr: \":9090\"\nauth:\n  enabled: true\n  token_file: " + tokenPath + "\n  jwt_secret_file: " + jwtPath + "\n"
	if err := os.WriteFile(dCfg, []byte(dYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	SetDaemonConfigPathOverride(dCfg)
	t.Cleanup(ResetDaemonConfigPathOverride)

	daemonCfg, err := config.Load(dCfg)
	if err != nil {
		t.Fatal(err)
	}
	res := ResolveDaemonPaths(daemonCfg, dCfg)
	var out strings.Builder
	PrintPaths(&out, res, "bunker paths")
	rendered := out.String()
	if !strings.Contains(rendered, filepath.Dir(tokenPath)) {
		t.Errorf("MISSING token parent prefix %q\n%s", filepath.Dir(tokenPath), rendered)
	}
	if !strings.Contains(rendered, filepath.Dir(jwtPath)) {
		t.Errorf("MISSING jwt parent prefix %q", filepath.Dir(jwtPath))
	}
}
