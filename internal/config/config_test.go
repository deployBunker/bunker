package config

import (
	"os"
	"path/filepath"
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
	if cfg.Auth.Enabled {
		t.Error("auth should be disabled by default")
	}
	if cfg.Auth.JWTTTL != 6*time.Hour {
		t.Errorf("expected jwt_ttl 6h, got %v", cfg.Auth.JWTTTL)
	}
	if cfg.Agent.DefaultTTL != 6*time.Hour {
		t.Errorf("expected agent default_ttl 6h, got %v", cfg.Agent.DefaultTTL)
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
