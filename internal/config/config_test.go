package config

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/hostsetup"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Server.GRPCAddr != ":9090" {
		t.Errorf("expected :9090, got %q", cfg.Server.GRPCAddr)
	}
	if cfg.Server.RESTAddr != ":8080" {
		t.Errorf("expected :8080, got %q", cfg.Server.RESTAddr)
	}
	if cfg.TLS.Enabled {
		t.Error("TLS should be disabled by default")
	}
	if !cfg.Auth.Enabled {
		t.Error("auth should be enabled by default (secure-by-default, GAP-011)")
	}
	if cfg.Auth.JWTTTL != 6*time.Hour {
		t.Errorf("expected jwt_ttl 6h, got %v", cfg.Auth.JWTTTL)
	}
	if cfg.Agent.PortRangeEnd != 19999 {
		t.Errorf("expected port_range_end 19999, got %d", cfg.Agent.PortRangeEnd)
	}
	if cfg.Agent.PortRangePerAgent != 100 {
		t.Errorf("expected port_range_per_agent 100, got %d", cfg.Agent.PortRangePerAgent)
	}
	if cfg.Agent.MaxAgents != 100 {
		t.Errorf("expected max_agents 100, got %d", cfg.Agent.MaxAgents)
	}
	if cfg.Agent.DefaultTTL != 6*time.Hour {
		t.Errorf("expected agent default_ttl 6h, got %v", cfg.Agent.DefaultTTL)
	}
	if !cfg.Audit.Enabled {
		t.Error("audit should be enabled by default")
	}
	if cfg.Audit.Path != "/var/log/bunkerd/audit.log" {
		t.Errorf("expected audit path /var/log/bunkerd/audit.log, got %q", cfg.Audit.Path)
	}
	// GAP-091: the rootless installer cache is armed by default so fresh
	// agent spawns stop depending on get.docker.com throughput.
	if cfg.Agent.RootlessInstallerCacheDir != "/var/cache/bunker/rootless-installer" {
		t.Errorf("expected rootless_installer_cache_dir /var/cache/bunker/rootless-installer, got %q", cfg.Agent.RootlessInstallerCacheDir)
	}
}

func TestCheckAuth_DefaultRefusesWithoutCredential(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Auth.Enabled {
		t.Fatal("default auth should be enabled")
	}
	warn, err := cfg.CheckAuth()
	if err == nil {
		t.Fatal("expected error for auth enabled without token/jwt_secret")
	}
	if !strings.Contains(err.Error(), "auth.token") {
		t.Errorf("error should mention auth.token, got: %v", err)
	}
	if warn != "" {
		t.Errorf("expected no warning when refusing, got %q", warn)
	}
}

// TestCheckAuth_WithToken: a tokened config starts, but since GAP-129 an
// inline credential is called out as legacy storage (see
// TestCheckAuth_WarnsOnInlineSecrets and TestLoad_ResolvesTokenFromEnvFile for
// the file-backed, warning-free equivalent).
func TestCheckAuth_WithToken(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Auth.Token = "test-token"
	warn, err := cfg.CheckAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(warn, "legacy secret storage") {
		t.Errorf("expected a GAP-129 legacy-storage warning for an inline token, got %q", warn)
	}
}

// TestCheckAuth_WithJWTSecret: same contract as TestCheckAuth_WithToken — an
// inline jwt_secret starts the daemon and warns that it should move to a file.
func TestCheckAuth_WithJWTSecret(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Auth.JWTSecret = "test-jwt-secret-must-be-at-least-32-bytes-long"
	warn, err := cfg.CheckAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(warn, "auth.jwt_secret") {
		t.Errorf("expected a GAP-129 legacy-storage warning naming auth.jwt_secret, got %q", warn)
	}
}

func TestCheckAuth_ExplicitDisabledWarns(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Auth.Enabled = false
	warn, err := cfg.CheckAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(warn, "AUTH DISABLED") {
		t.Errorf("expected prominent AUTH DISABLED warning, got %q", warn)
	}
}

func TestLoad_Defaults(t *testing.T) {
	// Load from a non-existent path should still produce defaults
	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.GRPCAddr != ":9090" {
		t.Errorf("expected :9090, got %q", cfg.Server.GRPCAddr)
	}
}

func TestLoad_FromFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
server:
  grpc_addr: ":9999"
  rest_addr: ""
tls:
  enabled: true
  cert_file: "/etc/certs/cert.pem"
  key_file: "/etc/certs/key.pem"
auth:
  enabled: true
  token: "test-token"
agent:
  default_ttl: "30m"
audit:
  enabled: false
  path: "/tmp/custom-audit.log"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.GRPCAddr != ":9999" {
		t.Errorf("expected :9999, got %q", cfg.Server.GRPCAddr)
	}
	if cfg.Server.RESTAddr != "" {
		t.Errorf("expected empty rest_addr, got %q", cfg.Server.RESTAddr)
	}
	if !cfg.TLS.Enabled {
		t.Error("TLS should be enabled")
	}
	if cfg.TLS.CertFile != "/etc/certs/cert.pem" {
		t.Errorf("expected /etc/certs/cert.pem, got %q", cfg.TLS.CertFile)
	}
	if cfg.Auth.Token != "test-token" {
		t.Errorf("expected test-token, got %q", cfg.Auth.Token)
	}
	if cfg.Agent.DefaultTTL != 30*time.Minute {
		t.Errorf("expected default_ttl 30m, got %v", cfg.Agent.DefaultTTL)
	}
	if cfg.Audit.Enabled {
		t.Error("audit should be disabled by config file")
	}
	if cfg.Audit.Path != "/tmp/custom-audit.log" {
		t.Errorf("expected audit path /tmp/custom-audit.log, got %q", cfg.Audit.Path)
	}
}

func TestLoad_AuditEnvOverrides(t *testing.T) {
	// Env overrides follow the BUNKERD_<SECTION>_<KEY> convention, the same
	// mechanism as the other sections.
	t.Setenv("BUNKERD_AUDIT_ENABLED", "false")
	t.Setenv("BUNKERD_AUDIT_PATH", "/tmp/env-audit.log")

	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Audit.Enabled {
		t.Error("BUNKERD_AUDIT_ENABLED=false should disable audit")
	}
	if cfg.Audit.Path != "/tmp/env-audit.log" {
		t.Errorf("expected audit path from env /tmp/env-audit.log, got %q", cfg.Audit.Path)
	}
}

// GAP-067: containment disclosure default/env/file semantics.
func TestDefaultConfig_ContainmentDisclosureDisabled(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Containment.Disclosure {
		t.Error("containment.disclosure must default to false (hidden by default, GAP-067)")
	}
}

// TestContainmentSandboxEnvKeyMatchesValue pins that ContainmentSandboxEnvKey
// is the KEY of the KEY=VALUE pair ContainmentSandboxEnv — callers match the
// key alone against agent-supplied overrides, so the two must never drift.
func TestContainmentSandboxEnvKeyMatchesValue(t *testing.T) {
	const (
		val = ContainmentSandboxEnv
		key = ContainmentSandboxEnvKey
	)
	if !strings.HasPrefix(val, key+"=") {
		t.Errorf("ContainmentSandboxEnvKey %q is not the key of ContainmentSandboxEnv %q", key, val)
	}
	if strings.Contains(strings.TrimPrefix(val, key+"="), "=") {
		t.Errorf("ContainmentSandboxEnv value must not itself contain '=': %q", val)
	}
}

func TestLoad_ContainmentDisclosureDefaultsFalse(t *testing.T) {
	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Containment.Disclosure {
		t.Error("Load() without containment config must leave disclosure false")
	}
}

func TestLoad_ContainmentDisclosureEnvOverride(t *testing.T) {
	// BUNKERD_CONTAINMENT_DISCLOSURE=true must enable disclosure even when
	// the config file (or its absence) says nothing.
	t.Setenv("BUNKERD_CONTAINMENT_DISCLOSURE", "true")
	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Containment.Disclosure {
		t.Error("BUNKERD_CONTAINMENT_DISCLOSURE=true should enable disclosure")
	}
}

func TestLoad_ContainmentDisclosureEnvOverrideBeatsFile(t *testing.T) {
	// A config file with disclosure: false must be overridden by the env var.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
containment:
  disclosure: false
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("BUNKERD_CONTAINMENT_DISCLOSURE", "true")
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Containment.Disclosure {
		t.Error("BUNKERD_CONTAINMENT_DISCLOSURE=true should override config-file false")
	}
}

func TestLoad_ContainmentDisclosureFileTrue(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
containment:
  disclosure: true
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// No env var: the config-file true must stand on its own.
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Containment.Disclosure {
		t.Error("config-file containment.disclosure: true should enable disclosure")
	}
}

func TestLoad_ContainmentDisclosureFileFalseNoEnv(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
containment:
  disclosure: false
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Containment.Disclosure {
		t.Error("config-file containment.disclosure: false should keep disclosure off")
	}
}

// GAP-073: audit ship_to/seal_key parse from file and env, and default OFF.
func TestAuditConfigShipToAndSealKey(t *testing.T) {
	// Defaults: both off.
	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Audit.ShipTo != "" || cfg.Audit.SealKey != "" {
		t.Errorf("default ship_to/seal_key = %q/%q, want both empty (off by default)", cfg.Audit.ShipTo, cfg.Audit.SealKey)
	}

	// From config file.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "audit:\n  ship_to: \"https://collector.example.net/v1\"\n  seal_key: \"s3cret\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Audit.ShipTo != "https://collector.example.net/v1" {
		t.Errorf("ship_to from file = %q", cfg.Audit.ShipTo)
	}
	if cfg.Audit.SealKey != "s3cret" {
		t.Errorf("seal_key from file = %q", cfg.Audit.SealKey)
	}

	// From env (BUNKERD_AUDIT_SHIP_TO / BUNKERD_AUDIT_SEAL_KEY).
	t.Setenv("BUNKERD_AUDIT_SHIP_TO", "syslog://10.0.0.5:601")
	t.Setenv("BUNKERD_AUDIT_SEAL_KEY", "env-key")
	cfg, err = Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Audit.ShipTo != "syslog://10.0.0.5:601" {
		t.Errorf("ship_to from env = %q", cfg.Audit.ShipTo)
	}
	if cfg.Audit.SealKey != "env-key" {
		t.Errorf("seal_key from env = %q", cfg.Audit.SealKey)
	}
}

func TestValidate_Valid(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("default config should be valid: %v", err)
	}
}

func TestValidate_EmptyGRPCAddr(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.GRPCAddr = ""
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty grpc_addr")
	}
}

func TestValidate_TLSWithoutCerts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TLS.Enabled = true
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for TLS without cert_file")
	}
}

func TestValidate_TLSWithAutoTLS(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TLS.Enabled = true
	cfg.TLS.AutoTLS = true
	// Missing domain should error
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for auto_tls without domain")
	}

	cfg.TLS.Domain = "bunkerd.example.com"
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected valid with domain: %v", err)
	}
}

func TestValidate_TLSWithFileCerts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TLS.Enabled = true
	cfg.TLS.CertFile = "/etc/certs/cert.pem"
	cfg.TLS.KeyFile = "/etc/certs/key.pem"
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected valid: %v", err)
	}
}

// TestIsolationAgentGroupDefaults pins the board decision (group
// `bunker-agents`, not the rejected `bunker`) across every surface that can
// carry a default: the built-in default config, the zero-value Defaults()
// filler, and the hostsetup constants the installer and the daemon share.
func TestIsolationAgentGroupDefaults(t *testing.T) {
	if hostsetup.DefaultAgentGroup != "bunker-agents" {
		t.Errorf("hostsetup.DefaultAgentGroup = %q, want bunker-agents", hostsetup.DefaultAgentGroup)
	}
	if hostsetup.DefaultScratchGroup != hostsetup.DefaultAgentGroup {
		t.Errorf("shared-scratch group %q must be the same group as the isolation group %q",
			hostsetup.DefaultScratchGroup, hostsetup.DefaultAgentGroup)
	}

	cfg := DefaultConfig()
	if cfg.Agent.Isolation.AgentGroup != "bunker-agents" {
		t.Errorf("DefaultConfig() agent group = %q, want bunker-agents", cfg.Agent.Isolation.AgentGroup)
	}
	if cfg.Agent.Isolation.SharedScratchGroup != cfg.Agent.Isolation.AgentGroup {
		t.Errorf("default scratch group %q != agent group %q",
			cfg.Agent.Isolation.SharedScratchGroup, cfg.Agent.Isolation.AgentGroup)
	}

	// The zero value must land on the same group (the installer is often run
	// with no config file at all).
	var zero IsolationConfig
	zero.Defaults()
	if zero.AgentGroup != "bunker-agents" {
		t.Errorf("IsolationConfig{}.Defaults() agent group = %q, want bunker-agents", zero.AgentGroup)
	}
}

// TestIsolationAgentGroupLegacyAlias: the first GAP-075 revision called the
// single group `shared_scratch_group`, so an existing file keeps working while
// the canonical key is agent_group.
func TestIsolationAgentGroupLegacyAlias(t *testing.T) {
	var legacy IsolationConfig
	legacy.SharedScratchGroup = "legacy-bunker"
	legacy.Defaults()
	if legacy.AgentGroup != "legacy-bunker" {
		t.Errorf("legacy shared_scratch_group did not carry over: agent group = %q", legacy.AgentGroup)
	}

	var canonical IsolationConfig
	canonical.AgentGroup = "my-agents"
	canonical.SharedScratchGroup = "stale-ignore-me"
	canonical.Defaults()
	if canonical.AgentGroup != "my-agents" {
		t.Errorf("explicit agent_group was overridden: %q", canonical.AgentGroup)
	}
}

// TestLoad_IsolationAgentGroupFromFile proves the YAML key reaches the struct:
// the isolation group is what the sshd pam_exec precondition requires of every
// agent session, so a typo here would deny every session (a missing group is a
// denial, never a silent shared /tmp).
func TestLoad_IsolationAgentGroupFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bunkerd.yaml")
	body := "server:\n  grpc_addr: \":19090\"\nagent:\n  isolation:\n    agent_group: custom-agents\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Agent.Isolation.AgentGroup != "custom-agents" {
		t.Errorf("agent_group from file = %q, want custom-agents", cfg.Agent.Isolation.AgentGroup)
	}
}

// --- GAP-129: control-plane secret storage (_FILE/env indirection + ---- //
// --- auto-generated jwt_secret)                                      ---- //

// clearSecretEnv unsets every GAP-129 env var for the duration of a test so
// the ambient environment of an operator's shell can never leak a *_FILE
// indirection into an assertion about the config file.
func clearSecretEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{AuthTokenFileEnv, AuthJWTSecretFileEnv, SecretsDirEnv} {
		prev, had := os.LookupEnv(k)
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unsetenv %s: %v", k, err)
		}
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(k, prev)
				return
			}
			_ = os.Unsetenv(k)
		})
	}
}

// writeSecret writes a secret file the way an operator would (mode 0600) and
// returns its path.
func writeSecret(t *testing.T, dir, name, value string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("write secret %s: %v", path, err)
	}
	return path
}

// TestAuthConfigResolveSecrets_Precedence is the GAP-129 acceptance table:
// inline < config *_file path < env-file, for both credentials, plus the
// failure modes that must stay loud (missing file, empty file).
func TestAuthConfigResolveSecrets_Precedence(t *testing.T) {
	dir := t.TempDir()
	cfgFile := writeSecret(t, dir, "cfg-token", "from-config-file\n")
	envFile := writeSecret(t, dir, "env-token", "  from-env-file  \n")

	missing := filepath.Join(dir, "does-not-exist")
	if err := os.WriteFile(filepath.Join(dir, "empty"), []byte("\n  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")

	tests := []struct {
		name          string
		inline        string
		cfgPath       string
		envPath       string
		want          string
		wantErrSubstr string
	}{
		{
			name:   "inline only (legacy) is used as-is",
			inline: "inline-token",
			want:   "inline-token",
		},
		{
			name:    "config file path wins over inline",
			inline:  "inline-token",
			cfgPath: cfgFile,
			want:    "from-config-file",
		},
		{
			name:    "env file path wins over config file path and inline",
			inline:  "inline-token",
			cfgPath: cfgFile,
			envPath: envFile,
			want:    "from-env-file",
		},
		{
			name:    "env file path wins when it is the only source",
			envPath: envFile,
			want:    "from-env-file",
		},
		{
			name: "inline stays empty when nothing is configured",
			want: "",
		},
		{
			name:          "config path pointing at a missing file is a hard error",
			inline:        "inline-token",
			cfgPath:       missing,
			wantErrSubstr: "read secret file",
		},
		{
			name:          "env path pointing at a missing file is a hard error (no fallback)",
			inline:        "inline-token",
			cfgPath:       cfgFile,
			envPath:       missing,
			wantErrSubstr: "read secret file",
		},
		{
			name:          "empty secret file is a hard error, not an empty credential",
			cfgPath:       empty,
			wantErrSubstr: "is empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearSecretEnv(t)

			a := AuthConfig{Token: tc.inline, TokenFile: tc.cfgPath}
			if tc.envPath != "" {
				t.Setenv(AuthTokenFileEnv, tc.envPath)
			}
			err := a.ResolveSecrets()
			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("ResolveSecrets() = nil error, want one containing %q", tc.wantErrSubstr)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Errorf("error %q does not contain %q", err, tc.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSecrets() error = %v", err)
			}
			if a.Token != tc.want {
				t.Errorf("Token = %q, want %q", a.Token, tc.want)
			}
		})
	}

	// The jwt_secret side must honor the identical precedence table.
	t.Run("jwt_secret follows the same precedence", func(t *testing.T) {
		clearSecretEnv(t)
		envSecret := writeSecret(t, dir, "env-jwt", "env-jwt-secret")
		a := AuthConfig{JWTSecret: "inline-jwt", JWTSecretFile: cfgFile, Token: "t"}
		t.Setenv(AuthJWTSecretFileEnv, envSecret)
		if err := a.ResolveSecrets(); err != nil {
			t.Fatalf("ResolveSecrets() error = %v", err)
		}
		if a.JWTSecret != "env-jwt-secret" {
			t.Errorf("JWTSecret = %q, want env-jwt-secret (env-file must win)", a.JWTSecret)
		}
		if a.Token != "t" {
			t.Errorf("Token = %q, want t (unchanged)", a.Token)
		}
	})
}

// TestLoad_ResolvesTokenFromEnvFile is acceptance criterion 1 at the Load
// level: a config that carries NO inline secret starts with a populated
// credential because BUNKER_AUTH_TOKEN_FILE points at a file. The token value
// proves the file was read (whitespace trimmed), not merely that no error was
// returned.
func TestLoad_ResolvesTokenFromEnvFile(t *testing.T) {
	clearSecretEnv(t)
	tokenFile := writeSecret(t, t.TempDir(), "token", "file-supplied-token\n")
	t.Setenv(AuthTokenFileEnv, tokenFile)

	cfgPath := filepath.Join(t.TempDir(), "bunkerd.yaml")
	body := "auth:\n  enabled: true\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.Token != "file-supplied-token" {
		t.Fatalf("Auth.Token = %q, want file-supplied-token", cfg.Auth.Token)
	}
	// Acceptance criterion 2: the config file itself holds no plaintext
	// secret, so the loaded credential exists ONLY because of the file
	// indirection.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "file-supplied-token") {
		t.Error("config file contains the plaintext token — the supported path must keep it out")
	}
	// And the credential now satisfies the startup gate.
	warn, err := cfg.CheckAuth()
	if err != nil {
		t.Fatalf("CheckAuth() after file resolution = %v", err)
	}
	if warn != "" {
		t.Errorf("CheckAuth() warned %q; a file-sourced credential is not legacy inline storage", warn)
	}
}

// TestLoad_ConfigFilePathIndirection: the same indirection through the config
// file (no env var at all), plus the documented config-env var form.
func TestLoad_ConfigFilePathIndirection(t *testing.T) {
	clearSecretEnv(t)
	dir := t.TempDir()
	tokenFile := writeSecret(t, dir, "token", "cfg-path-token")
	jwtFile := writeSecret(t, dir, "jwt", "cfg-path-jwt-secret")

	cfgPath := filepath.Join(dir, "bunkerd.yaml")
	body := "auth:\n  enabled: true\n  token_file: " + filepath.ToSlash(tokenFile) +
		"\n  jwt_secret_file: " + filepath.ToSlash(jwtFile) + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.Token != "cfg-path-token" {
		t.Errorf("Auth.Token = %q, want cfg-path-token", cfg.Auth.Token)
	}
	if cfg.Auth.JWTSecret != "cfg-path-jwt-secret" {
		t.Errorf("Auth.JWTSecret = %q, want cfg-path-jwt-secret", cfg.Auth.JWTSecret)
	}
}

// TestLoad_EnvFilePathBeatsConfigFilePath pins the top of the precedence
// ladder at the Load level: both sources are set and the env one wins.
func TestLoad_EnvFilePathBeatsConfigFilePath(t *testing.T) {
	clearSecretEnv(t)
	dir := t.TempDir()
	cfgToken := writeSecret(t, dir, "cfg-token", "from-config")
	envToken := writeSecret(t, dir, "env-token", "from-env")
	t.Setenv(AuthTokenFileEnv, envToken)

	cfgPath := filepath.Join(dir, "bunkerd.yaml")
	body := "auth:\n  enabled: true\n  token_file: " + filepath.ToSlash(cfgToken) + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.Token != "from-env" {
		t.Errorf("Auth.Token = %q, want from-env (env-file outranks the config file path)", cfg.Auth.Token)
	}
}

// TestLoad_UnreadableSecretFileFailsBeforeListen: a *_file path that cannot be
// read must fail the load, never silently fall back to an inline value or to
// an empty credential.
func TestLoad_UnreadableSecretFileFailsBeforeListen(t *testing.T) {
	clearSecretEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bunkerd.yaml")
	body := "auth:\n  enabled: true\n  token: \"inline-fallback\"\n  token_file: " +
		filepath.ToSlash(filepath.Join(dir, "nope")) + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("Load() = nil error for an unreadable token_file, want a hard error")
	}
	if !strings.Contains(err.Error(), "read secret file") {
		t.Errorf("error %q does not name the failed file read", err)
	}
	if strings.Contains(err.Error(), "inline-fallback") {
		t.Error("error leaked the inline credential value")
	}
}

// TestEnsureJWTSecret_GeneratesPersistsAndReloads is acceptance criterion 3:
// first boot generates a 32-byte hex secret, persists it 0600 in a 0700
// directory, loads it back into the config, and a SECOND boot reuses the same
// value (signature continuity — never rotate silently).
func TestEnsureJWTSecret_GeneratesPersistsAndReloads(t *testing.T) {
	clearSecretEnv(t)
	dir := t.TempDir()
	t.Setenv(SecretsDirEnv, dir)

	cfg := DefaultConfig()
	cfg.Auth.Token = "static-token"

	notice, err := cfg.EnsureJWTSecret()
	if err != nil {
		t.Fatalf("EnsureJWTSecret() error = %v", err)
	}
	if !strings.Contains(notice, "GENERATED") {
		t.Errorf("first-boot notice = %q, want it to say a secret was generated", notice)
	}
	generated := cfg.Auth.JWTSecret
	if len(generated) != 2*GeneratedJWTSecretBytes {
		t.Fatalf("generated secret is %d chars, want %d (hex of %d bytes)",
			len(generated), 2*GeneratedJWTSecretBytes, GeneratedJWTSecretBytes)
	}
	if _, err := hex.DecodeString(generated); err != nil {
		t.Errorf("generated secret is not hex: %v", err)
	}

	// File mode 0600, dir mode 0700.
	path := filepath.Join(dir, JWTSecretFileName)
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat secret file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != SecretsFileMode {
		t.Errorf("secret file mode = %#o, want %#o", got, SecretsFileMode)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat secrets dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != SecretsDirMode {
		t.Errorf("secrets dir mode = %#o, want %#o", got, SecretsDirMode)
	}

	// On-disk content is the secret plus a single trailing newline; the
	// loaded value is trimmed.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != generated {
		t.Errorf("persisted file %q does not hold the generated secret", string(raw))
	}

	// Second boot: SAME value, no regeneration, no rewrite.
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := DefaultConfig()
	cfg2.Auth.Token = "static-token"
	notice2, err := cfg2.EnsureJWTSecret()
	if err != nil {
		t.Fatalf("second EnsureJWTSecret() error = %v", err)
	}
	if cfg2.Auth.JWTSecret != generated {
		t.Errorf("second boot produced a DIFFERENT jwt_secret (%q vs %q) — issued keys would break",
			cfg2.Auth.JWTSecret, generated)
	}
	if strings.Contains(notice2, "GENERATED") {
		t.Errorf("second boot regenerated a secret: %q", notice2)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("second boot rewrote the persisted secret file (rotation on restart is forbidden)")
	}
}

// TestEnsureJWTSecret_ConfiguredSecretIsNeverRotated: a secret from any
// configured source is used as-is and nothing is written.
func TestEnsureJWTSecret_ConfiguredSecretIsNeverRotated(t *testing.T) {
	clearSecretEnv(t)
	dir := t.TempDir()
	t.Setenv(SecretsDirEnv, dir)

	cfg := DefaultConfig()
	cfg.Auth.Token = "static-token"
	cfg.Auth.JWTSecret = "configured-secret-value"

	notice, err := cfg.EnsureJWTSecret()
	if err != nil {
		t.Fatalf("EnsureJWTSecret() error = %v", err)
	}
	if notice != "" {
		t.Errorf("notice = %q, want empty when a secret is already configured", notice)
	}
	if cfg.Auth.JWTSecret != "configured-secret-value" {
		t.Errorf("configured secret was replaced with %q", cfg.Auth.JWTSecret)
	}
	path := filepath.Join(dir, JWTSecretFileName)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a secret file was written at %s even though a secret was configured (stat err = %v)", path, err)
	}
}

// TestEnsureJWTSecret_UnreadablePersistedSecretIsFatal: an existing but
// unreadable file must refuse to start rather than generate a replacement over
// a live signing key.
func TestEnsureJWTSecret_UnreadablePersistedSecretIsFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}
	clearSecretEnv(t)
	dir := t.TempDir()
	t.Setenv(SecretsDirEnv, dir)

	path := writeSecret(t, dir, JWTSecretFileName, "existing-secret")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Auth.Token = "static-token"
	notice, err := cfg.EnsureJWTSecret()
	if err == nil {
		t.Fatalf("EnsureJWTSecret() = nil error for an unreadable persisted secret (notice %q)", notice)
	}
	if !strings.Contains(err.Error(), "refusing to start") {
		t.Errorf("error %q does not refuse to start", err)
	}
	if cfg.Auth.JWTSecret != "" {
		t.Errorf("config was seeded with a generated secret despite the failure: %q", cfg.Auth.JWTSecret)
	}
}

// TestEnsureJWTSecret_SkipsWithoutConsumers pins the generation gate: with
// auth disabled, or with no static token, nothing consumes a jwt_secret, so
// first boot must not persist one.
func TestEnsureJWTSecret_SkipsWithoutConsumers(t *testing.T) {
	tests := []struct {
		name  string
		auth  func(*Config)
		notOk string
	}{
		{
			name:  "auth explicitly disabled",
			auth:  func(c *Config) { c.Auth.Enabled = false; c.Auth.Token = "static-token" },
			notOk: "auth disabled must not mint a secret nothing uses",
		},
		{
			name:  "enabled but tokenless",
			auth:  func(c *Config) { c.Auth.Enabled = true; c.Auth.Token = "" },
			notOk: "tokenless config must not mint a secret (no apikey manager is built)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearSecretEnv(t)
			dir := t.TempDir()
			t.Setenv(SecretsDirEnv, dir)

			cfg := DefaultConfig()
			tc.auth(cfg)

			notice, err := cfg.EnsureJWTSecret()
			if err != nil {
				t.Fatalf("EnsureJWTSecret() error = %v", err)
			}
			if notice != "" {
				t.Errorf("notice = %q, want empty", notice)
			}
			if cfg.Auth.JWTSecret != "" {
				t.Errorf("JWTSecret = %q; %s", cfg.Auth.JWTSecret, tc.notOk)
			}
			if _, err := os.Stat(filepath.Join(dir, JWTSecretFileName)); !os.IsNotExist(err) {
				t.Errorf("a secret file was written: %v", err)
			}
		})
	}
}

// TestEnsureJWTSecret_TightensExistingDir: MkdirAll leaves a pre-existing
// directory's mode alone, so a 0755 secrets dir must be tightened to 0700 —
// otherwise an auto-generated secret is world-listable (and its content
// world-readable if the file mode were ever wrong too).
func TestEnsureJWTSecret_TightensExistingDir(t *testing.T) {
	clearSecretEnv(t)
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SecretsDirEnv, dir)

	cfg := DefaultConfig()
	cfg.Auth.Token = "static-token"
	if _, err := cfg.EnsureJWTSecret(); err != nil {
		t.Fatalf("EnsureJWTSecret() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != SecretsDirMode {
		t.Errorf("pre-existing dir mode = %#o, want tightened to %#o", got, SecretsDirMode)
	}
}

// TestCheckAuth_WarnsOnInlineSecrets is acceptance criterion 4's warning half:
// inline (legacy) startup works AND warns; file-backed startup works and does
// NOT warn.
func TestCheckAuth_WarnsOnInlineSecrets(t *testing.T) {
	clearSecretEnv(t)

	t.Run("inline token warns", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Auth.Token = "inline-token"
		warn, err := cfg.CheckAuth()
		if err != nil {
			t.Fatalf("CheckAuth() error = %v", err)
		}
		if !strings.Contains(warn, "legacy secret storage") || !strings.Contains(warn, "auth.token") {
			t.Errorf("warning = %q, want a legacy-storage warning naming auth.token", warn)
		}
		if strings.Contains(warn, "inline-token") {
			t.Error("warning leaked the credential value")
		}
	})

	t.Run("inline jwt_secret warns", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Auth.JWTSecret = "inline-jwt-secret"
		warn, err := cfg.CheckAuth()
		if err != nil {
			t.Fatalf("CheckAuth() error = %v", err)
		}
		if !strings.Contains(warn, "auth.jwt_secret") {
			t.Errorf("warning = %q, want it to name auth.jwt_secret", warn)
		}
	})

	t.Run("file-backed token does not warn", func(t *testing.T) {
		dir := t.TempDir()
		tokenFile := writeSecret(t, dir, "token", "file-token")
		cfg := DefaultConfig()
		cfg.Auth.Token = "file-token"
		cfg.Auth.TokenFile = tokenFile
		warn, err := cfg.CheckAuth()
		if err != nil {
			t.Fatalf("CheckAuth() error = %v", err)
		}
		if warn != "" {
			t.Errorf("warning = %q, want none for a file-backed credential", warn)
		}
	})

	t.Run("disabled still warns and never inspects credentials", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Auth.Enabled = false
		cfg.Auth.Token = "inline-token"
		warn, err := cfg.CheckAuth()
		if err != nil {
			t.Fatalf("CheckAuth() error = %v", err)
		}
		if !strings.Contains(warn, "AUTH DISABLED") {
			t.Errorf("warning = %q, want the AUTH DISABLED warning to take precedence", warn)
		}
	})
}

// TestSecretsDirOrDefault: env wins, otherwise $HOME/.config/bunkerd/secrets.
func TestSecretsDirOrDefault(t *testing.T) {
	clearSecretEnv(t)

	t.Setenv(SecretsDirEnv, "/tmp/custom-secrets")
	if got := SecretsDirOrDefault(); got != "/tmp/custom-secrets" {
		t.Errorf("SecretsDirOrDefault() = %q, want /tmp/custom-secrets", got)
	}

	_ = os.Unsetenv(SecretsDirEnv)
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, DefaultSecretsDir)
	if got := SecretsDirOrDefault(); got != want {
		t.Errorf("SecretsDirOrDefault() = %q, want %q", got, want)
	}
	if !strings.HasSuffix(want, filepath.Join(".config", "bunkerd", "secrets")) {
		t.Errorf("default secrets dir %q is not the documented location", want)
	}
}
