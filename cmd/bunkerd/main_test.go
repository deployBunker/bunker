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

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/hostsetup"
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

	t.Run("config.example", func(t *testing.T) {
		// GAP-026: the README quickstart template users copy to
		// /etc/bunkerd/config.yaml must stay CI-validated against the schema.
		cfg, err := loadConfigFrom(t, "../../config.example.yaml")
		if err != nil {
			t.Fatalf("load config.example.yaml: %v", err)
		}
		if cfg.Server.GRPCAddr != ":9090" {
			t.Errorf("grpc_addr = %q, want :9090", cfg.Server.GRPCAddr)
		}
		if cfg.Server.RESTAddr != ":8080" {
			t.Errorf("rest_addr = %q, want :8080", cfg.Server.RESTAddr)
		}
		if !cfg.Auth.Enabled {
			t.Error("config.example should have auth enabled")
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
	// This test exercises daemon boot/shutdown, NOT the GAP-070 durable
	// registry, so the registry stays OFF. Startup reconciliation is
	// destructive by design — an agent it cannot restore/adopt with its
	// EXACT port reservation is force-destroyed — so any live reconcile
	// here could delete the test host's real bunker-* agents. Adoption
	// mode is NOT a safe substitute: an orphan without readable port
	// metadata fails adoption and is destroyed. The registry-enabled start
	// path is covered by internal/server's "refuses without a durable
	// registry" test and the reconcile tests in internal/agent.
	cfgPath := writeTestConfig(t, fmt.Sprintf(`
server:
  grpc_addr: "127.0.0.1:%d"
  rest_addr: ""
auth:
  enabled: true
  token: "test-token"
agent:
  registry:
    enabled: false
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

// TestBunkerdPositionalArgs covers GAP-078: positional verbs must be resolved
// before anything touches config, and every other positional must be refused
// loudly. Pre-fix, `bunkerd version` fell silently through to the serve path —
// it loaded the config and raced the running daemon for ports and agent
// reconciliation (the entry path that made DF-BUNKER-13 reachable).
//
// Every case passes an explicit NON-EXISTENT --config (and the load-bearing
// case also exports BUNKERD_CONFIG to that same missing path) so a pre-fix run
// fails fast inside config.Load. That matters on this host: /etc/bunkerd/
// config.yaml exists and a bunkerd is already serving :8080/:9090, so without
// the override the pre-fix red run would boot a second daemon and race the live
// one — the exact incident this gap is about.
//
// Flags must precede the positional in every case: Go's flag package stops
// parsing at the first non-flag token, so `bunkerd bogus --config X` would
// leave cfgPath at the default and reach the real config file.
func TestBunkerdPositionalArgs(t *testing.T) {
	const missingCfg = "/nonexistent/definitely-no-config.yaml"

	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })

	tests := []struct {
		name       string
		args       []string
		envCfg     string   // when set, exported as BUNKERD_CONFIG for the case
		wantErrSub string   // substring the returned error MUST carry ("" => nil error)
		errNotSubs []string // substrings the returned error must NOT carry
		wantStdout []string // substrings required in captured stdout
		wantStderr []string // substrings required in captured stderr
	}{
		{
			name:       "version verb",
			args:       []string{"--config", missingCfg, "version"},
			wantStdout: []string{"bunkerd ", "commit:"},
		},
		{
			name:       "help verb",
			args:       []string{"--config", missingCfg, "help"},
			wantStderr: []string{"Usage:"},
		},
		{
			name:       "unknown positional",
			args:       []string{"--config", missingCfg, "bogus"},
			wantErrSub: "bogus",
			errNotSubs: configPathSignatures(),
		},
		{
			// The load-bearing case: BUNKERD_CONFIG points at a missing file
			// AND --config points at the same missing file. Refusal must
			// still precede config.Load, so the error names the argument
			// rather than the config file.
			name:       "unknown positional with env and flag config",
			args:       []string{"--config", missingCfg, "bogus"},
			envCfg:     missingCfg,
			wantErrSub: "bogus",
			errNotSubs: configPathSignatures(),
		},
		{
			// A second positional is still a refusal: the extra token is named
			// and nothing is served.
			name:       "second positional",
			args:       []string{"--config", missingCfg, "version", "extra"},
			wantErrSub: "extra",
			errNotSubs: configPathSignatures(),
		},
		{
			// Stronger than the missing-path cases: config.Load short-circuits
			// to DefaultConfig() when the path does not exist, so only an
			// EXISTING-but-malformed file can prove the refusal never read it.
			// Pre-fix this errors with "read config <path>: ...".
			name:       "unknown positional with malformed existing config",
			args:       []string{"--config", writeTestConfig(t, "server: [\n"), "bogus"},
			wantErrSub: "bogus",
			errNotSubs: append(configPathSignatures(), "read config"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envCfg != "" {
				prev, had := os.LookupEnv("BUNKERD_CONFIG")
				if err := os.Setenv("BUNKERD_CONFIG", tc.envCfg); err != nil {
					t.Fatalf("setenv BUNKERD_CONFIG: %v", err)
				}
				t.Cleanup(func() {
					if had {
						_ = os.Setenv("BUNKERD_CONFIG", prev)
						return
					}
					_ = os.Unsetenv("BUNKERD_CONFIG")
				})
			}

			setArgs(t, tc.args...)

			var (
				runErr error
				errOut string
			)
			out := captureStdout(t, func() {
				errOut = captureStderr(t, func() {
					runErr = run()
				})
			})

			if tc.wantErrSub == "" {
				if runErr != nil {
					t.Fatalf("run(%v) returned error, want nil: %v", tc.args, runErr)
				}
			} else {
				if runErr == nil {
					t.Fatalf("run(%v) returned nil, want error containing %q", tc.args, tc.wantErrSub)
				}
				if !strings.Contains(runErr.Error(), tc.wantErrSub) {
					t.Errorf("error %q does not name the offending argument %q", runErr, tc.wantErrSub)
				}
				for _, forbidden := range tc.errNotSubs {
					if strings.Contains(runErr.Error(), forbidden) {
						t.Errorf("error %q carries %q — refusal must precede config.Load (no config read, no auth gate, no serve path)", runErr, forbidden)
					}
				}
			}

			for _, want := range tc.wantStdout {
				if !strings.Contains(out, want) {
					t.Errorf("stdout missing %q, got: %q", want, out)
				}
			}
			for _, want := range tc.wantStderr {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr missing %q, got: %q", want, errOut)
				}
			}
		})
	}

	// The positional verb must reuse the --version implementation, so its whole
	// block is byte-identical to the flag form: six lines, same field order and
	// indentation, because internal/hostsetup.ParseDaemonVersionOutput parses
	// exactly that shape.
	t.Run("version verb output matches --version byte for byte", func(t *testing.T) {
		flagOut := captureStdout(t, func() {
			setArgs(t, "--version")
			if err := run(); err != nil {
				t.Fatalf("run --version: %v", err)
			}
		})
		posOut := captureStdout(t, func() {
			setArgs(t, "--config", missingCfg, "version")
			if err := run(); err != nil {
				t.Fatalf("run version: %v", err)
			}
		})

		if posOut != flagOut {
			t.Errorf("positional version output differs from --version:\n--version:  %q\npositional: %q", flagOut, posOut)
		}
		if got := strings.Count(strings.TrimRight(posOut, "\n"), "\n") + 1; got != 6 {
			t.Errorf("version block has %d lines, want 6: %q", got, posOut)
		}
		for _, field := range []string{"commit:", "built:", "caps:", "go version:", "platform:"} {
			if !strings.Contains(posOut, field) {
				t.Errorf("version block missing %q: %q", field, posOut)
			}
		}
	})

	// GAP-082: the block must ROUND-TRIP through the parser the installer's
	// daemon-skew probe uses, and it must advertise the spawn-side capability.
	// A version number cannot prove the grant, so this line is the proof the
	// probe reads; breaking it (a renamed prefix, a reordered line the parser
	// stops at, a dropped token) makes the installer refuse a healthy daemon.
	t.Run("advertised capability round-trips through the daemon probe parser", func(t *testing.T) {
		out := captureStdout(t, func() {
			setArgs(t, "--version")
			if err := run(); err != nil {
				t.Fatalf("run --version: %v", err)
			}
		})
		build, err := hostsetup.ParseDaemonVersionOutput([]byte(out))
		if err != nil {
			t.Fatalf("the version block does not parse as daemon version output: %v\n%s", err, out)
		}
		if !build.HasCapability(hostsetup.GrantCapability) {
			t.Errorf("parsed build does not report %s (caps=%v):\n%s", hostsetup.GrantCapability, build.Capabilities, out)
		}
		want := strings.Join(agent.SpawnCapabilities(), ",")
		if got := strings.Join(build.Capabilities, ","); got != want {
			t.Errorf("parsed caps = %q, want the advertised set %q", got, want)
		}
	})
}

// --- helpers ---

// configPathSignatures returns the error substrings that indicate an argument
// fell through to the config/startup path. Both appear only AFTER config.Load:
// "load config" / "read config" come from the loader, "refusing to start" from
// the auth gate that runs once a config object exists. A positional refusal
// must carry neither.
func configPathSignatures() []string {
	return []string{"load config", "refusing to start"}
}

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
