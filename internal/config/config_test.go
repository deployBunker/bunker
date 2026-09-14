package config

import (
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

func TestCheckAuth_WithToken(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Auth.Token = "test-token"
	warn, err := cfg.CheckAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if warn != "" {
		t.Errorf("expected no warning with token set, got %q", warn)
	}
}

func TestCheckAuth_WithJWTSecret(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Auth.JWTSecret = "test-jwt-secret-must-be-at-least-32-bytes-long"
	warn, err := cfg.CheckAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if warn != "" {
		t.Errorf("expected no warning with jwt_secret set, got %q", warn)
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
