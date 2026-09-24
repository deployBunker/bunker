package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/config"
)

// GAP-126 / REQ-T1 at the daemon boundary: Run must REFUSE before binding a
// non-loopback plaintext listener, the explicit opt-in must start + warn, and
// the audit log the server opens must carry the insecure marker exactly when
// the traffic it will record is plaintext on a reachable address.

// gateTestConfig returns a config that is safe to hand to Run on a test host:
// no auth-gate noise, no audit file, and the durable registry OFF (startup
// reconciliation is destructive by design — see TestStartupShutdown).
func gateTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = false
	cfg.Audit.Enabled = false
	cfg.Agent.Registry.Enabled = false
	// GAP-132: the durable key store lives under base_data_dir; the default
	// (/var/lib/bunkerd) is root-owned on a test host and would refuse to
	// open before any TLS gate is exercised. Point it at a scratch dir.
	cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "data")
	// Run validates the config, so TLS knobs must be inert unless the case
	// turns them on.
	cfg.TLS.Enabled = false
	return cfg
}

// scratchListenPort returns a port that was free a moment ago. It is only
// used to give an allowed case a real address to bind. The close it performs
// opens a close→rebind window another process can win; the allowed cases
// absorb that with a bounded FRESH-port retry (see TestServerRun_TLSGate) so
// a lost port race does not fail the acceptance table. A genuine gate
// regression still fails loudly rather than silently pass.
func scratchListenPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find scratch port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return strconv.Itoa(port)
}

// waitForTCPListen reports whether something accepted a TCP connection on addr
// (a plain dial, so it works for both the TLS and the plaintext listener).
func waitForTCPListen(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// isShutdownErr accepts the errors a cancelled or deadline-bounded Run
// legitimately returns.
func isShutdownErr(err error) bool {
	return err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		err.Error() == "server error: http: Server closed"
}

// isBindRace reports whether err is the lost close→rebind race of a scratch
// port (INT-FLAKE-001): another process bound the address between the probe
// close and Run's rebind. The errno check is primary (Run wraps bind errors
// with %w); the message match is a fallback in case the error chain is ever
// re-shaped.
func isBindRace(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EADDRINUSE) ||
		strings.Contains(err.Error(), "address already in use")
}

// TestServerRun_TLSGate is the acceptance table at the Run boundary: refused
// configs must return an error naming the knob BEFORE anything binds, and
// allowed configs must reach a live listener (which a refused one can never
// do).
func TestServerRun_TLSGate(t *testing.T) {
	port := scratchListenPort(t)

	tests := []struct {
		name        string
		tlsEnabled  bool
		insecureDev bool
		grpcAddr    string
		restAddr    string
		dialAddr    string // where the listener should appear when allowed
		wantRefuse  bool
	}{
		{
			name:       "acceptance 1: wildcard gRPC plaintext exits",
			grpcAddr:   ":9090",
			restAddr:   "",
			wantRefuse: true,
		},
		{
			name:       "acceptance 1: wildcard REST plaintext exits",
			grpcAddr:   "127.0.0.1:9090",
			restAddr:   ":8080",
			wantRefuse: true,
		},
		{
			name:        "acceptance 2: explicit opt-in starts",
			insecureDev: true,
			grpcAddr:    "0.0.0.0:" + port,
			restAddr:    "",
			dialAddr:    "127.0.0.1:" + port,
			wantRefuse:  false,
		},
		{
			name:       "acceptance 3: loopback-only starts silently",
			grpcAddr:   "127.0.0.1:" + port,
			restAddr:   "",
			dialAddr:   "127.0.0.1:" + port,
			wantRefuse: false,
		},
		{
			name:       "tls on starts on a non-loopback bind",
			tlsEnabled: true,
			grpcAddr:   "0.0.0.0:" + port,
			restAddr:   "",
			dialAddr:   "127.0.0.1:" + port,
			wantRefuse: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := gateTestConfig(t)
			cfg.Server.GRPCAddr = tc.grpcAddr
			cfg.Server.RESTAddr = tc.restAddr
			cfg.TLS.Enabled = tc.tlsEnabled
			cfg.TLS.InsecureDev = tc.insecureDev
			if tc.tlsEnabled {
				tmp := t.TempDir()
				cfg.TLS.SelfSigned = true
				cfg.TLS.CertFile = tmp + "/cert.pem"
				cfg.TLS.KeyFile = tmp + "/key.pem"
				cfg.TLS.Hosts = []string{"127.0.0.1"}
			}

			if tc.wantRefuse {
				err := New(cfg).Run(context.Background())
				if err == nil {
					t.Fatal("Run returned nil, want a refusal before any listener opened")
				}
				if !strings.Contains(err.Error(), "refusing to start") {
					t.Errorf("error %q does not read as a startup refusal", err)
				}
				for _, want := range []string{"tls.enabled", "tls.insecure_dev"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %s", err, want)
					}
				}
				return
			}

			// Allowed: the daemon must reach a live listener. Run blocks, so
			// it goes to a goroutine and a cancelled context asserts the
			// clean shutdown a refused config would never have reached.
			//
			// INT-FLAKE-001: the scratch port was closed by scratchListenPort a
			// moment ago, so another process (or a concurrent test binary) can
			// bind it before Run rebinds. Losing that race surfaces as
			// EADDRINUSE — a different failure from the TLS-gate defect under
			// test — so on EADDRINUSE re-scratch a FRESH port (never retry the
			// same one) within a bounded budget. A genuine gate regression
			// refuses before any bind and still fails loudly on attempt 1.
			grpcHost, _, err := net.SplitHostPort(tc.grpcAddr)
			if err != nil {
				t.Fatalf("parse allowed-case grpcAddr %q: %v", tc.grpcAddr, err)
			}
			dialHost, _, err := net.SplitHostPort(tc.dialAddr)
			if err != nil {
				t.Fatalf("parse allowed-case dialAddr %q: %v", tc.dialAddr, err)
			}
			const maxBindAttempts = 5
			for attempt := 1; ; attempt++ {
				cfg.Server.GRPCAddr = net.JoinHostPort(grpcHost, port)
				dialAddr := net.JoinHostPort(dialHost, port)

				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- New(cfg).Run(ctx) }()

				if !waitForTCPListen(dialAddr, 10*time.Second) {
					cancel()
					var runErr error
					select {
					case runErr = <-done:
					case <-time.After(5 * time.Second):
					}
					if attempt < maxBindAttempts && isBindRace(runErr) {
						port = scratchListenPort(t) // fresh port, not the lost one
						continue
					}
					if runErr != nil && !isShutdownErr(runErr) {
						t.Fatalf("Run failed before serving: %v", runErr)
					}
					t.Fatalf("Run never opened %s — the gate refused a config it must allow", dialAddr)
				}
				cancel()
				select {
				case runErr := <-done:
					if attempt < maxBindAttempts && isBindRace(runErr) {
						port = scratchListenPort(t) // fresh port, not the lost one
						continue
					}
					if !isShutdownErr(runErr) {
						t.Fatalf("Run after cancel: %v", runErr)
					}
				case <-time.After(15 * time.Second):
					t.Fatal("Run did not shut down after cancel")
				}
				break // bound, served, and shut down cleanly
			}
		})
	}
}

// TestServerRun_GatePrecedesListeners is the fail-before-listen invariant as a
// source-level proof: in Run, the CheckTLS call must appear BEFORE the first
// listener, so no configuration can bind a port and THEN be refused.
func TestServerRun_GatePrecedesListeners(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	body := string(src)

	gate := strings.Index(body, "CheckTLS(")
	if gate < 0 {
		t.Fatal("Run no longer calls CheckTLS — the transport gate is gone")
	}
	firstListener := strings.Index(body, "ListenAndServe")
	if firstListener < 0 {
		t.Fatal("no listener found in server.go; this guard can no longer prove ordering")
	}
	if gate > firstListener {
		t.Errorf("CheckTLS appears at offset %d, after the first listener at %d — a refused config could bind a port first", gate, firstListener)
	}
	// The transport refusal must precede the durable-registry gate, so the
	// most security-relevant check runs first.
	if reg := strings.Index(body, "RegistryError()"); reg >= 0 && gate > reg {
		t.Errorf("CheckTLS at %d runs after the registry gate at %d", gate, reg)
	}
}

// TestServerRun_InsecureWarnsOnStderr pins the loud half of acceptance 2: the
// opt-in path must emit the INSECURE warning on the daemon's own startup path,
// not silently.
func TestServerRun_InsecureWarnsOnStderr(t *testing.T) {
	tests := []struct {
		name        string
		insecureDev bool
		grpcAddr    string
		wantWarn    bool
	}{
		{name: "opt-in warns", insecureDev: true, grpcAddr: ":0", wantWarn: true},
		{name: "loopback is silent", grpcAddr: "127.0.0.1:0", wantWarn: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := gateTestConfig(t)
			cfg.Server.GRPCAddr = tc.grpcAddr
			cfg.Server.RESTAddr = ""
			cfg.TLS.InsecureDev = tc.insecureDev

			stderr := captureStderr(t, func() {
				ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				defer cancel()
				err := New(cfg).Run(ctx)
				if !isShutdownErr(err) {
					t.Fatalf("Run: %v", err)
				}
			})

			hasWarn := strings.Contains(stderr, "INSECURE") &&
				strings.Contains(stderr, config.InsecurePlaintextMarker)
			if hasWarn != tc.wantWarn {
				t.Fatalf("stderr warning present = %v, want %v; stderr: %q", hasWarn, tc.wantWarn, stderr)
			}
		})
	}
}

// TestServerNew_MarksAuditWhenInsecure is the wiring proof for acceptance 2's
// second half: the audit log the SERVER opens carries the marker exactly when
// the transport is insecure — not merely when insecure_dev is set.
func TestServerNew_MarksAuditWhenInsecure(t *testing.T) {
	tests := []struct {
		name        string
		tlsEnabled  bool
		insecureDev bool
		grpcAddr    string
		wantMarked  bool
	}{
		{
			name:        "non-loopback + opt-in marks the trail",
			insecureDev: true,
			grpcAddr:    ":9090",
			wantMarked:  true,
		},
		{
			name:       "loopback does not mark the trail",
			grpcAddr:   "127.0.0.1:9090",
			wantMarked: false,
		},
		{
			name:       "plaintext non-loopback without opt-in does not open a marked log",
			grpcAddr:   ":9090",
			wantMarked: false,
		},
		{
			name:        "tls on does not mark the trail",
			tlsEnabled:  true,
			insecureDev: true,
			grpcAddr:    ":9090",
			wantMarked:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := gateTestConfig(t)
			cfg.Server.GRPCAddr = tc.grpcAddr
			cfg.Server.RESTAddr = ""
			cfg.TLS.Enabled = tc.tlsEnabled
			cfg.TLS.InsecureDev = tc.insecureDev
			cfg.Audit.Enabled = true
			cfg.Audit.Path = filepath.Join(t.TempDir(), "audit.log")

			s := New(cfg)
			if s.auditLog == nil {
				t.Fatal("audit log was not opened")
			}

			if got := s.auditLog.InsecurePlaintext(); got != tc.wantMarked {
				t.Fatalf("audit log insecure marker = %v, want %v", got, tc.wantMarked)
			}

			// Drive one real record through the write path so the assertion
			// is on the written bytes, not only on the accessor.
			if err := s.auditLog.Log(audit.Record{
				TS: "2026-01-01T00:00:00Z", Method: "/test/op", Outcome: "ok", Summary: "spawn agent_id=a1",
			}); err != nil {
				t.Fatalf("Log: %v", err)
			}
			_ = s.auditLog.Close()

			raw, err := os.ReadFile(cfg.Audit.Path)
			if err != nil {
				t.Fatalf("read audit log: %v", err)
			}
			marked := strings.Contains(string(raw), config.InsecurePlaintextMarker)
			if marked != tc.wantMarked {
				t.Fatalf("written record marked = %v, want %v (log: %s)", marked, tc.wantMarked, raw)
			}
			if tc.wantMarked && !strings.HasPrefix(summaryOf(t, raw), config.InsecurePlaintextMarker) {
				t.Errorf("summary %q does not start with the marker", summaryOf(t, raw))
			}
		})
	}
}

// TestServerRun_RefusesBeforeServing pins the ordering the fail-fast gate buys:
// a refused configuration must not have served anything, so its audit file
// holds no record.
func TestServerRun_RefusesBeforeServing(t *testing.T) {
	cfg := gateTestConfig(t)
	cfg.Server.GRPCAddr = ":0"
	cfg.Server.RESTAddr = ""
	cfg.Audit.Enabled = true
	cfg.Audit.Path = filepath.Join(t.TempDir(), "audit.log")

	if err := New(cfg).Run(context.Background()); err == nil {
		t.Fatal("expected a refusal for a non-loopback plaintext bind")
	}
	raw, err := os.ReadFile(cfg.Audit.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return // no file at all is also fine
		}
		t.Fatalf("read audit log: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "" {
		t.Errorf("a refused daemon wrote audit records: %q", raw)
	}
}

func summaryOf(t *testing.T, raw []byte) string {
	t.Helper()
	line := strings.TrimSpace(string(raw))
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("record is not JSON (%v): %q", err, line)
	}
	s, _ := rec["summary"].(string)
	return s
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what
// was written. The startup warning is asserted on the stream a supervisor
// would capture.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(out)
}
