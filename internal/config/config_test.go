package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
