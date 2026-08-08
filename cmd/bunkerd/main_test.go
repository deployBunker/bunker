package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
)

// TestBunkerdFlags exercises the entrypoint flag surface: --help, --version,
// --config, and unknown flags. run() is invoked directly with a temporary
// os.Args so the parsed values are observable without starting a server.
func TestBunkerdFlags(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		out := captureStdout(t, func() {
			setArgs(t, "--version")
			if err := run(); err != nil {
				t.Fatalf("run --version: %v", err)
			}
		})
		if !strings.Contains(out, "bunkerd ") {
			t.Errorf("version output missing binary name, got: %q", out)
		}
		if !strings.Contains(out, "commit:") {
			t.Errorf("version output missing commit line, got: %q", out)
		}
	})

	t.Run("version shorthand", func(t *testing.T) {
		out := captureStdout(t, func() {
			setArgs(t, "-v")
			if err := run(); err != nil {
				t.Fatalf("run -v: %v", err)
			}
		})
		if !strings.Contains(out, "bunkerd ") {
			t.Errorf("shorthand version output missing binary name, got: %q", out)
		}
	})

	t.Run("help", func(t *testing.T) {
		errOut := captureStderr(t, func() {
			setArgs(t, "--help")
			if err := run(); err != nil {
				t.Fatalf("run --help: %v", err)
			}
		})
		if !strings.Contains(errOut, "Usage:") {
			t.Errorf("help output missing Usage section, got: %q", errOut)
		}
		if !strings.Contains(errOut, "-config") {
			t.Errorf("help output missing -config flag, got: %q", errOut)
		}
	})

	t.Run("config loads explicit path", func(t *testing.T) {
		// A config that fails validation fast (empty grpc_addr) proves the
		// --config path was honored: run() reaches Validate() and errors on
		// the file's contents rather than starting a server.
		cfgPath := writeTestConfig(t, "server:\n  grpc_addr: \"\"\nauth:\n  enabled: false\n")
		setArgs(t, "--config", cfgPath)
		err := run()
		if err == nil {
			t.Fatal("expected error from config with empty grpc_addr")
		}
		if !strings.Contains(err.Error(), "grpc_addr") {
			t.Errorf("expected grpc_addr validation error, got: %v", err)
		}
	})

	t.Run("unknown flag", func(t *testing.T) {
		setArgs(t, "--definitely-not-a-flag")
		if err := run(); err == nil {
			t.Fatal("expected error for unknown flag")
		}
	})
}

// TestConfigLoad verifies a YAML config file parses into the expected struct
// fields. It loads the shipped example configs (GAP-019) in addition to a
// synthetic one, so the examples stay wired to the schema.
func TestConfigLoad(t *testing.T) {
	t.Run("example dev-noauth", func(t *testing.T) {
		cfg, err := loadConfigFrom(t, "../../examples/dev-noauth.yaml")
		if err != nil {
			t.Fatalf("load examples/dev-noauth.yaml: %v", err)
		}
		if cfg.Auth.Enabled {
			t.Error("dev-noauth example should have auth disabled")
		}
		if cfg.Server.GRPCAddr != "127.0.0.1:9090" {
			t.Errorf("grpc_addr = %q, want 127.0.0.1:9090", cfg.Server.GRPCAddr)
		}
		if cfg.Agent.MaxAgents != 10 {
			t.Errorf("max_agents = %d, want 10", cfg.Agent.MaxAgents)
		}
	})

	t.Run("example tls", func(t *testing.T) {
		cfg, err := loadConfigFrom(t, "../../examples/tls.yaml")
		if err != nil {
			t.Fatalf("load examples/tls.yaml: %v", err)
		}
		if !cfg.TLS.Enabled {
			t.Error("tls example should have TLS enabled")
		}
		if !cfg.TLS.SelfSigned {
			t.Error("tls example should use self_signed")
		}
		if !cfg.Auth.Enabled || cfg.Auth.Token == "" {
			t.Error("tls example should have auth with a token")
		}
	})

	t.Run("example tailscale", func(t *testing.T) {
		cfg, err := loadConfigFrom(t, "../../examples/tailscale.yaml")
		if err != nil {
			t.Fatalf("load examples/tailscale.yaml: %v", err)
		}
		if !cfg.Tailscale.Enabled {
			t.Error("tailscale example should have tailscale enabled")
		}
		if cfg.Tailscale.AuthKey == "" {
			t.Error("tailscale example should carry an authkey placeholder")
		}
	})

	t.Run("synthetic", func(t *testing.T) {
		cfgPath := writeTestConfig(t, `
server:
  grpc_addr: ":19090"
  rest_addr: ":18080"
auth:
  enabled: true
  token: "test-token"
agent:
  max_agents: 3
  default_ttl: "30m"
`)
		cfg, err := loadConfigFrom(t, cfgPath)
		if err != nil {
			t.Fatalf("load synthetic config: %v", err)
		}
		if cfg.Server.GRPCAddr != ":19090" {
			t.Errorf("grpc_addr = %q, want :19090", cfg.Server.GRPCAddr)
		}
		if cfg.Auth.Token != "test-token" {
			t.Errorf("token = %q, want test-token", cfg.Auth.Token)
		}
		if cfg.Agent.DefaultTTL != 30*time.Minute {
			t.Errorf("default_ttl = %v, want 30m", cfg.Agent.DefaultTTL)
		}
	})
}

// TestStartupShutdown boots the full daemon on a scratch port from a
// t.TempDir config, waits for /healthz, then sends SIGTERM and asserts a
// clean exit (nil error or context.Canceled from the signal-driven cancel).
func TestStartupShutdown(t *testing.T) {
	port := scratchPort(t)
	cfgPath := writeTestConfig(t, fmt.Sprintf(`
server:
  grpc_addr: "127.0.0.1:%d"
  rest_addr: ""
auth:
  enabled: true
  token: "test-token"
`, port))

	oldArgs := os.Args
	os.Args = []string{"bunkerd", "--config", cfgPath}
	defer func() { os.Args = oldArgs }()

	done := make(chan error, 1)
	go func() { done <- run() }()

	// Wait for the server to accept connections.
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	if !waitForHealth(t, healthURL, 10*time.Second) {
		// Drain the goroutine so we don't leak it before failing.
		select {
		case err := <-done:
			t.Fatalf("server exited before healthz: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("server did not become healthy on scratch port")
		}
	}

	// Graceful shutdown via SIGTERM (the same path systemd uses).
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error on SIGTERM shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not shut down within 15s of SIGTERM")
	}
}

// --- helpers ---

func setArgs(t *testing.T, args ...string) {
	t.Helper()
	os.Args = append([]string{"bunkerd"}, args...)
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

func loadConfigFrom(t *testing.T, path string) (*config.Config, error) {
	t.Helper()
	return config.Load(path)
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return capture(t, &os.Stdout, fn)
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	return capture(t, &os.Stderr, fn)
}

func capture(t *testing.T, slot **os.File, fn func()) string {
	t.Helper()
	orig := *slot
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	*slot = w
	defer func() { *slot = orig }()
	fn()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func scratchPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find scratch port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func waitForHealth(t *testing.T, url string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		client := &http.Client{Timeout: 500 * time.Millisecond}
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
