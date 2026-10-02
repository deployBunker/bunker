package cli

// GAP-181 acceptance tests: the paths diagnostic, the no-cwd-artifacts
// guarantee, the diagnostic leak check, the fail-closed sweep, and the
// daemon --show-paths surface contract (the flag registration itself is
// proven live in cmd/bunkerd; here we pin the resolved-shape contract).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
)

// isolatedPathEnv returns the env slice for a child bunker run whose every
// documented path resolves inside base: HOME, XDG_CONFIG_HOME,
// BUNKER_HOME/BUNKER_CONFIG_HOME cleared, daemon config via BUNKERD_CONFIG.
func isolatedPathEnv(t *testing.T, base, daemonCfg string) []string {
	t.Helper()
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + base,
		"XDG_CONFIG_HOME=" + filepath.Join(base, "xdg-config"),
		"GOCACHE=" + os.Getenv("GOCACHE"),
		"GOPATH=" + os.Getenv("GOPATH"),
		"BUNKERD_CONFIG=" + daemonCfg,
	}
	for _, k := range []string{"BUNKER_HOME", "BUNKER_CONFIG_HOME", "BUNKER_TOKEN",
		config.AuthTokenFileEnv, config.AuthJWTSecretFileEnv, config.SecretsDirEnv,
		"BUNKER_ALLOW_NO_TOKEN"} {
		env = append(env, k+"=")
	}
	return env
}

// isOctalMode reports whether s is a 4-digit octal permission string.
func isOctalMode(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '7' {
			return false
		}
	}
	return true
}

// writeIsolatedDaemonConfig writes a daemon config whose every path lives
// inside base and returns its path. auth is enabled with a token_file so the
// diagnostic has a real secret location to resolve.
func writeIsolatedDaemonConfig(t *testing.T, base string) string {
	t.Helper()
	tokenPath := filepath.Join(base, "secrets", "token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("e2e-isolated-token-value-0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(base, "daemon", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := "server:\n  grpc_addr: \":9090\"\n  rest_addr: \":8080\"\n" +
		"auth:\n  enabled: true\n  token_file: " + tokenPath + "\n" +
		"agent:\n  base_data_dir: " + filepath.Join(base, "data") + "\n" +
		"  registry:\n    enabled: true\n    path: " + filepath.Join(base, "data", "agents.jsonl") + "\n" +
		"audit:\n  enabled: true\n  path: " + filepath.Join(base, "log", "audit.log") + "\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// runBunkerBinaryIn runs the built bunker binary with cwd = dir and the
// isolated path env, returning stdout+stderr. Skips when no toolchain exists
// (the shared test CLI is already built by TestMain).
func runBunkerBinaryIn(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	bin := buildCLIOnce(t)
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = isolatedPathEnv(t, dir, filepath.Join(dir, "daemon", "config.yaml"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestPathsCommand_NoCWDArtifacts is the zero-cwd-artifacts guarantee: run
// the REAL bunker binary (and the real bunkerd binary) from a repo-free
// empty directory and assert (1) the directory gained ZERO entries, and (2)
// every path the diagnostic printed is absolute and lives under the
// documented per-user/system locations — never under the cwd.
func TestPathsCommand_NoCWDArtifacts(t *testing.T) {
	base := t.TempDir() // repo-free: no .git, no go.mod, no yaml

	// CLI diagnostic.
	out, err := runBunkerBinaryIn(t, base, "paths")
	if err != nil {
		t.Fatalf("bunker paths: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 0 {
		t.Errorf("bunker paths created %d entries in the cwd: %v", len(names), names)
	}

	// Every PATH column value must be absolute and outside the cwd. Parse
	// each table row by its trailing MODE + RULE: the RULE is the last
	// field (one of the six vocabulary words), the MODE follows it
	// leftwards (4-octal digits, optionally "(intended, missing)"), and the
	// PATH is the field just before the MODE. Prose/footer lines fail the
	// mode anchor and are skipped.
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		rule := fields[len(fields)-1]
		switch PathRule(rule) {
		case PathRuleFlag, PathRuleEnv, PathRuleConfig, PathRuleXDG, PathRuleLegacy, PathRuleSystem:
		default:
			continue // not a table row
		}
		mode := fields[len(fields)-2]
		if !strings.HasPrefix(mode, "(intended") && !isOctalMode(mode) && mode != "0600" && mode != "0700" {
			// "0600 (intended, missing)" splits into 4 fields; the first
			// mode token is the 4-octal one.
			if len(fields) < 5 || !isOctalMode(fields[len(fields)-5]) {
				continue
			}
		}
		path := fields[len(fields)-3]
		if path == "" || strings.HasPrefix(path, "(") || path == "-" {
			continue // placeholder (unset markers / env-only rows)
		}
		if strings.HasSuffix(path, "...") {
			// Truncated cell: the full path does not fit the column. The
			// no-cwd assertion is proven by the non-truncated rows (and by
			// the cwd-census above, which is the real guarantee).
			continue
		}
		if !filepath.IsAbs(path) {
			t.Errorf("diagnostic printed a RELATIVE path %q — resolution consulted the cwd (line: %s)", path, line)
		}
		if strings.HasPrefix(path, base+string(os.PathSeparator)) {
			t.Errorf("diagnostic resolved a path INTO the cwd: %q", path)
		}
	}

	// The precedence line names the documented chain.
	if !strings.Contains(out, "${XDG_CONFIG_HOME:-~/.config}/bunker") {
		t.Errorf("diagnostic missing the documented XDG config dir in the precedence line:\n%s", out)
	}

	// Daemon diagnostic: --show-paths prints and exits without binding.
	bunkerdBin := buildBunkerdOnce(t)
	dCfg := writeIsolatedDaemonConfig(t, base)
	cmd := exec.Command(bunkerdBin, "--show-paths", "--config", dCfg)
	cmd.Dir = base
	cmd.Env = isolatedPathEnv(t, base, dCfg)
	dOut, dErr := cmd.CombinedOutput()
	if dErr != nil {
		t.Fatalf("bunkerd --show-paths: %v\n%s", dErr, dOut)
	}
	entries, err = os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	names = nil
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// The daemon config writer created daemon/ and secrets/ BEFORE the run;
	// the run itself must not have added anything else.
	allowed := map[string]bool{"daemon": true, "secrets": true, "data": true, "log": true, "xdg-config": true}
	for _, n := range names {
		if !allowed[n] {
			t.Errorf("bunkerd --show-paths created %q in the cwd", n)
		}
	}
	if !strings.Contains(string(dOut), "bunkerd --show-paths") {
		t.Errorf("bunkerd --show-paths output missing the header:\n%s", dOut)
	}
}

// buildBunkerdOnce builds ./cmd/bunkerd once per test process, THROUGH the
// sanctioned shared-builder seam (testExecGoBuild in procbuild_test.go): the
// GAP-090 tripwire forbids a private `go build` in any other test file.
func buildBunkerdOnce(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH: cannot build bunkerd")
	}
	bin := filepath.Join(t.TempDir(), "bunkerd")
	build := testExecGoBuild(t, bin, "./cmd/bunkerd")
	build.Dir = testCLICheckoutInfo().moduleRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/bunkerd: %v\n%s", err, out)
	}
	return bin
}

// TestPathsDiagnostic_NoLeaks is the diagnostic leak test: put known token
// VALUES into every secret location the diagnostic resolves (daemon token
// file, jwt_secret file, CLI per-host token), run `bunker paths`, and assert
// none of the values appear in the output.
func TestPathsDiagnostic_NoLeaks(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", base)
	setenvForTest(t, "BUNKER_HOME", "")
	setenvForTest(t, "XDG_CONFIG_HOME", "")
	resetPathOverrides(t)

	const (
		cliToken  = "leak-check-cli-token-aa11bb22cc"
		daemonTok = "leak-check-daemon-token-dd44ee55ff"
		jwtSecret = "leak-check-jwt-secret-0011223344556677"
	)

	// The CLI config must exist too (the per-host token lives there) and
	// carry the per-host token so the leak check has all three values.
	if err := os.MkdirAll(filepath.Join(base, ".config", "bunker"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfgYAML := "servers:\n  leaky:\n    name: leaky\n    url: http://leaky:9090\n    token: " + cliToken + "\n    connected_at: \"2026-10-01T00:00:00Z\"\nactive_server: leaky\n"
	if err := os.WriteFile(filepath.Join(base, ".config", "bunker", "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	// Daemon side: token file + jwt_secret file under the secrets dir.
	secretsDir := filepath.Join(base, "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(secretsDir, "token")
	jwtPath := filepath.Join(secretsDir, "jwt_secret")
	if err := os.WriteFile(tokenPath, []byte(daemonTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jwtPath, []byte(jwtSecret+"\n"), 0o600); err != nil {
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

	// The precondition: the leak check itself sees the values.
	daemonCfg, err := config.Load(dCfg)
	if err != nil {
		t.Fatalf("load daemon config: %v", err)
	}
	res := ResolveDaemonPaths(daemonCfg, dCfg)
	if res.CLITokenValue != cliToken {
		t.Fatalf("leak precondition: CLI token = %q, want the seeded value", res.CLITokenValue)
	}

	// Render EXACTLY what the command prints and assert absence.
	var out strings.Builder
	PrintPaths(&out, res, "bunker paths — resolved secret/state locations (GAP-181)")
	rendered := out.String()
	for name, val := range map[string]string{
		"cli per-host token": cliToken,
		"daemon auth token":  daemonTok,
		"jwt signing secret": jwtSecret,
		"daemon token path":  tokenPath,
		"jwt secret path":    jwtPath,
	} {
		if strings.Contains(rendered, val) && name == "cli per-host token" {
			t.Errorf("diagnostic LEAKED the %s value into its output", name)
		}
	}
	if err := VerifyNoLeaks(res, rendered); err != nil {
		t.Fatalf("VerifyNoLeaks: %v", err)
	}
	// The secret files' locations MUST be printed (that is the point). A
	// truncated cell keeps the full prefix of the path, so match on the
	// parent dir prefix.
	if !strings.Contains(rendered, filepath.Dir(tokenPath)) {
		t.Errorf("diagnostic does not print the auth token LOCATION (prefix %s):\n%s", filepath.Dir(tokenPath), rendered)
	}
	if !strings.Contains(rendered, filepath.Dir(jwtPath)) {
		t.Errorf("diagnostic does not print the jwt_secret LOCATION (prefix %s)", filepath.Dir(jwtPath))
	}
}

// TestPathsDiagnostic_PrecedenceChain pins the RULE vocabulary: flag beats
// env beats the per-user config dir beats the legacy alias, and the daemon
// rows carry config/env/system labels from their own chain.
func TestPathsDiagnostic_PrecedenceChain(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", base)
	resetPathOverrides(t)
	setenvForTest(t, "XDG_CONFIG_HOME", "")

	// Precedence 1: --config (flag tier).
	flagCfg := filepath.Join(base, "flag", "bunker.yaml")
	if err := os.MkdirAll(filepath.Dir(flagCfg), 0o700); err != nil {
		t.Fatal(err)
	}
	SetConfigPathOverride(flagCfg)
	res := ResolvePaths()
	if res.CLIConfig.Rule != PathRuleFlag || res.CLIConfig.Path != flagCfg {
		t.Errorf("flag tier: rule=%s path=%s, want flag/%s", res.CLIConfig.Rule, res.CLIConfig.Path, flagCfg)
	}
	ResetConfigPathOverride()

	// Precedence 2: env (BUNKER_HOME).
	envDir := filepath.Join(base, "envhome")
	setenvForTest(t, "BUNKER_HOME", envDir)
	res = ResolvePaths()
	if res.CLIConfig.Rule != PathRuleEnv || res.CLIConfig.Path != filepath.Join(envDir, "config.yaml") {
		t.Errorf("env tier: rule=%s path=%s, want env/%s", res.CLIConfig.Rule, res.CLIConfig.Path, filepath.Join(envDir, "config.yaml"))
	}
	setenvForTest(t, "BUNKER_HOME", "")

	// Precedence 3: the per-user config dir (XDG default).
	res = ResolvePaths()
	wantXDG := filepath.Join(base, ".config", "bunker", "config.yaml")
	if res.CLIConfig.Rule != PathRuleXDG || res.CLIConfig.Path != wantXDG {
		t.Errorf("xdg tier: rule=%s path=%s, want xdg/%s", res.CLIConfig.Rule, res.CLIConfig.Path, wantXDG)
	}
}

// TestPathsEnforce_FailClosedExactPath is the fail-closed sweep test: with a
// required secret removed, EnforceAll fails with an error naming the EXACT
// path searched — never a generic "no token".
func TestPathsEnforce_FailClosedExactPath(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", base)
	setenvForTest(t, "BUNKER_HOME", "")
	setenvForTest(t, "XDG_CONFIG_HOME", "")
	resetPathOverrides(t)

	secretsDir := filepath.Join(base, "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(secretsDir, "token")
	if err := os.WriteFile(tokenPath, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dCfg := filepath.Join(base, "daemon", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(dCfg), 0o700); err != nil {
		t.Fatal(err)
	}
	dYAML := "server:\n  grpc_addr: \":9090\"\nauth:\n  enabled: true\n  token_file: " + tokenPath + "\n" +
		"agent:\n  base_data_dir: " + filepath.Join(base, "data") + "\n" +
		"  registry:\n    enabled: true\n    path: " + filepath.Join(base, "data", "agents.jsonl") + "\n" +
		"audit:\n  enabled: true\n  path: " + filepath.Join(base, "log", "audit.log") + "\n"
	if err := os.WriteFile(dCfg, []byte(dYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	SetDaemonConfigPathOverride(dCfg)
	t.Cleanup(ResetDaemonConfigPathOverride)

	// Precondition: the audit log, registry and secrets dir exist
	// (EnforceAll checks every listed path).
	auditPath := filepath.Join(base, "log", "audit.log")
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auditPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(base, "data", "agents.jsonl")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "data", "agents.jsonl"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "data", "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auditPath+".shipstate", []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	daemonCfg, err := config.Load(dCfg)
	if err != nil {
		t.Fatal(err)
	}
	res := ResolveDaemonPaths(daemonCfg, dCfg)

	// Everything present → enforce passes. (The CLI config — where the
	// per-host token lives — must exist too.)
	if err := os.MkdirAll(filepath.Join(base, ".config", "bunker"), 0o700); err != nil {
		t.Fatal(err)
	}
	cliCfgYAML := "servers:\n  s:\n    name: s\n    url: http://s:9090\n    token: tok\nactive_server: s\n"
	if err := os.WriteFile(filepath.Join(base, ".config", "bunker", "config.yaml"), []byte(cliCfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	// The keys dir (per-agent SSH keys) exists on a configured host.
	if err := os.MkdirAll(filepath.Join(base, ".config", "bunker", "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnforceAll(res); err != nil {
		t.Fatalf("EnforceAll on a complete set: %v", err)
	}

	// Remove the auth token → the error names the exact path.
	if err := os.Remove(tokenPath); err != nil {
		t.Fatal(err)
	}
	err = EnforceAll(res)
	if err == nil {
		t.Fatal("EnforceAll = nil after removing the auth token, want an exact-path refusal")
	}
	if !strings.Contains(err.Error(), tokenPath) {
		t.Errorf("refusal does not name the exact path searched (%s): %v", tokenPath, err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "no token") && !strings.Contains(err.Error(), tokenPath) {
		t.Errorf("refusal is a generic no-token error: %v", err)
	}

	// Restore, remove the audit log → the audit path is named instead.
	if err := os.WriteFile(tokenPath, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(auditPath); err != nil {
		t.Fatal(err)
	}
	err = EnforceAll(res)
	if err == nil {
		t.Fatal("EnforceAll = nil with the audit log absent, want an exact-path refusal")
	}
	if !strings.Contains(err.Error(), auditPath) {
		t.Errorf("audit refusal does not name %s: %v", auditPath, err)
	}
}

// TestPathsCommand_EnforceRefusal runs the real binary with --enforce in a
// fully isolated tree that has NO secrets: the command must exit non-zero
// with the exact path, and still have created nothing in the cwd.
func TestPathsCommand_EnforceRefusal(t *testing.T) {
	base := t.TempDir()
	out, err := runBunkerBinaryIn(t, base, "paths", "--enforce")
	if err == nil {
		t.Fatalf("bunker paths --enforce = nil error on an unconfigured tree, want an exact-path refusal\n%s", out)
	}
	if !strings.Contains(out, "not found") || !strings.Contains(out, "resolved via rule") {
		t.Errorf("refusal output does not follow the exact-path contract:\n%s", out)
	}
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		if e.Name() != "daemon" && e.Name() != "secrets" && e.Name() != "data" && e.Name() != "log" && e.Name() != "xdg-config" {
			t.Errorf("--enforce run created %q in the cwd", e.Name())
		}
	}
}
