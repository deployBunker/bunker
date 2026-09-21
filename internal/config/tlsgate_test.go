package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GAP-126 / REQ-T1: the TLS enforcement gate. These are the table-driven
// proofs the acceptance criteria name — refusal on non-loopback plaintext,
// the explicit insecure_dev opt-in (warning + audit marker predicate), and
// the silent loopback-only case.

// TestCheckTLS covers the three acceptance classes plus the classification
// edge cases that decide them.
func TestCheckTLS(t *testing.T) {
	tests := []struct {
		name        string
		tlsEnabled  bool
		insecureDev bool
		grpcAddr    string
		restAddr    string
		wantErr     bool   // refusal: the daemon must not start
		wantWarnSub string // non-empty substring required in the warning
		wantNoWarn  bool   // no warning at all (allowed + silent)
	}{
		// --- acceptance 1: non-loopback plaintext without the opt-in -> EXIT
		{
			name:     "wildcard grpc without opt-in refuses",
			grpcAddr: ":9090",
			restAddr: "",
			wantErr:  true,
		},
		{
			name:     "wildcard rest without opt-in refuses",
			grpcAddr: "127.0.0.1:9090",
			restAddr: ":8080",
			wantErr:  true,
		},
		{
			name:     "explicit all-interfaces address refuses",
			grpcAddr: "0.0.0.0:9090",
			restAddr: "0.0.0.0:8080",
			wantErr:  true,
		},
		{
			name:     "routable IP refuses",
			grpcAddr: "78.46.173.180:9090",
			restAddr: "",
			wantErr:  true,
		},
		{
			name:     "ipv6 all-interfaces refuses",
			grpcAddr: "[::]:9090",
			restAddr: "",
			wantErr:  true,
		},
		{
			name:     "unresolvable hostname refuses rather than trusting it",
			grpcAddr: "bunker.example.invalid:9090",
			restAddr: "",
			wantErr:  true,
		},
		{
			name:     "empty grpc_addr is not a pass",
			grpcAddr: "",
			restAddr: "127.0.0.1:8080",
			wantErr:  true,
		},

		// --- acceptance 2: explicit opt-in -> start, LOUD warning
		{
			name:        "wildcard with insecure_dev warns and starts",
			insecureDev: true,
			grpcAddr:    ":9090",
			restAddr:    ":8080",
			wantWarnSub: "INSECURE",
		},
		{
			name:        "routable IP with insecure_dev warns and starts",
			insecureDev: true,
			grpcAddr:    "78.46.173.180:9090",
			restAddr:    "",
			wantWarnSub: "INSECURE",
		},

		// --- acceptance 3: loopback-only plaintext -> allowed, SILENT
		{
			name:       "loopback only",
			grpcAddr:   "127.0.0.1:9090",
			restAddr:   "127.0.0.1:8080",
			wantNoWarn: true,
		},
		{
			name:       "localhost hostname",
			grpcAddr:   "localhost:9090",
			restAddr:   "localhost:8080",
			wantNoWarn: true,
		},
		{
			name:       "ipv6 loopback",
			grpcAddr:   "[::1]:9090",
			restAddr:   "",
			wantNoWarn: true,
		},
		{
			name:       "loopback grpc with rest aliasing it is one bind",
			grpcAddr:   "127.0.0.1:9090",
			restAddr:   "127.0.0.1:9090",
			wantNoWarn: true,
		},
		{
			name:       "127.0.0.2 is still loopback",
			grpcAddr:   "127.0.0.2:9090",
			restAddr:   "",
			wantNoWarn: true,
		},

		// --- TLS on: the gate is a no-op whatever the bind
		{
			name:       "tls enabled silences the gate on a wildcard bind",
			tlsEnabled: true,
			grpcAddr:   ":9090",
			restAddr:   ":8080",
			wantNoWarn: true,
		},
		{
			name:       "tls enabled silences the gate even with insecure_dev",
			tlsEnabled: true,
			// insecure_dev is meaningless with TLS on; it must not add a
			// warning to a secure configuration.
			insecureDev: true,
			grpcAddr:    ":9090",
			restAddr:    "",
			wantNoWarn:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.TLS.Enabled = tc.tlsEnabled
			cfg.TLS.InsecureDev = tc.insecureDev

			warn, err := cfg.CheckTLS(tc.grpcAddr, tc.restAddr)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("CheckTLS(%q, %q) = nil error, want refusal", tc.grpcAddr, tc.restAddr)
				}
				// The refusal must name the knob an operator has to change.
				for _, want := range []string{"tls.enabled", "tls.insecure_dev"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal error %q does not name %s", err, want)
					}
				}
				if warn != "" {
					t.Errorf("refusal must not also warn, got %q", warn)
				}
				return
			}

			if err != nil {
				t.Fatalf("CheckTLS(%q, %q) = %v, want nil error", tc.grpcAddr, tc.restAddr, err)
			}
			if tc.wantNoWarn && warn != "" {
				t.Errorf("expected silence, got warning %q", warn)
			}
			if tc.wantWarnSub != "" && !strings.Contains(warn, tc.wantWarnSub) {
				t.Errorf("warning %q does not contain %q", warn, tc.wantWarnSub)
			}
			if tc.wantWarnSub == "" && !tc.wantNoWarn && warn == "" {
				t.Errorf("warning path produced no warning")
			}
		})
	}
}

// TestCheckTLS_RefusalNamesTheOffendingAddress pins the operator-facing detail:
// an error that does not say WHICH listener is the problem forces a config
// reading round-trip.
func TestCheckTLS_RefusalNamesTheOffendingAddress(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TLS.Enabled = false

	_, err := cfg.CheckTLS("127.0.0.1:9090", ":8080")
	if err == nil {
		t.Fatal("expected refusal for a wildcard REST bind")
	}
	if !strings.Contains(err.Error(), ":8080") {
		t.Errorf("error %q does not name the offending listener :8080", err)
	}
	// The loopback listener must NOT be blamed.
	if strings.Contains(err.Error(), "127.0.0.1:9090") {
		t.Errorf("error %q blames the loopback listener too", err)
	}
}

// TestCheckTLS_InsecureWarningCarriesTheAuditMarker keeps the warning and the
// audit stamp in lockstep: an operator reading the startup log must be told
// exactly what string marks the trail.
func TestCheckTLS_InsecureWarningCarriesTheAuditMarker(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TLS.InsecureDev = true

	warn, err := cfg.CheckTLS(":9090", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(warn, InsecurePlaintextMarker) {
		t.Errorf("warning %q does not name the audit marker %q", warn, InsecurePlaintextMarker)
	}
}

// TestIsLoopbackAddr pins the address classification the gate depends on.
// A wrong answer here is a silent security hole (a wildcard bind read as
// loopback) or a false refusal (a loopback bind read as reachable).
func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		// loopback
		{"127.0.0.1:9090", true},
		{"127.0.0.2:9090", true},
		{"127.255.255.254:9090", true},
		{"localhost:9090", true},
		{"LOCALHOST:9090", true},
		{"[::1]:9090", true},
		{"::1", true},
		{"127.0.0.1", true},
		// NOT loopback
		{":9090", false},
		{"0.0.0.0:9090", false},
		{"[::]:9090", false},
		{"", false},
		{"78.46.173.180:9090", false},
		{"192.168.1.10:9090", false},
		{"bunker.example.invalid:9090", false},
		{"9090", false}, // a bare port is not loopback
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			got, _ := IsLoopbackAddr(tc.addr)
			if got != tc.want {
				t.Errorf("IsLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

// TestInsecurePlaintextActive pins the predicate the audit marker keys off, so
// the marker can never be armed by a configuration the startup gate rejected
// (or skipped).
func TestInsecurePlaintextActive(t *testing.T) {
	tests := []struct {
		name        string
		tlsEnabled  bool
		insecureDev bool
		grpcAddr    string
		restAddr    string
		want        bool
	}{
		{name: "non-loopback + opt-in", insecureDev: true, grpcAddr: ":9090", restAddr: ":8080", want: true},
		{name: "non-loopback no opt-in", grpcAddr: ":9090", restAddr: "", want: false},
		{name: "loopback + opt-in stays unmarked", insecureDev: true, grpcAddr: "127.0.0.1:9090", restAddr: "", want: false},
		{name: "tls on", tlsEnabled: true, insecureDev: true, grpcAddr: ":9090", restAddr: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.TLS.Enabled = tc.tlsEnabled
			cfg.TLS.InsecureDev = tc.insecureDev
			cfg.Server.GRPCAddr = tc.grpcAddr
			cfg.Server.RESTAddr = tc.restAddr
			if got := cfg.InsecurePlaintextActive(); got != tc.want {
				t.Errorf("InsecurePlaintextActive() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoad_TLSInsecureDevFromFile proves the YAML key reaches the struct —
// the opt-in is only real if the operator's config file can set it.
func TestLoad_TLSInsecureDevFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bunkerd.yaml")
	body := "server:\n  grpc_addr: \":19090\"\ntls:\n  insecure_dev: true\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.TLS.InsecureDev {
		t.Error("tls.insecure_dev from file = false, want true")
	}
	// The default must stay false: the insecure path is never entered by
	// omission.
	if DefaultConfig().TLS.InsecureDev {
		t.Error("DefaultConfig() has insecure_dev set — insecure-by-default is the whole defect")
	}
}

// TestLoad_TLSInsecureDevEnvOverride proves BUNKERD_TLS_INSECURE_DEV reaches
// the struct through the documented env convention.
func TestLoad_TLSInsecureDevEnvOverride(t *testing.T) {
	t.Setenv("BUNKERD_TLS_INSECURE_DEV", "true")
	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.TLS.InsecureDev {
		t.Error("BUNKERD_TLS_INSECURE_DEV=true did not set tls.insecure_dev")
	}
}

// TestCheckTLS_CIBatteryShape pins the CI battery daemon shape (INT-CI-028).
// Both battery suites bind the wildcard CI ports :29090/:28080 — an empty
// host is every interface, NON-loopback per IsLoopbackAddr — so since GAP-126
// the daemons refused to start until the generated battery configs carried
// the explicit tls.insecure_dev opt-in. The table proves BOTH directions:
// opted in, the daemon starts LOUDLY (warning, no error) and the audit
// predicate is armed; opted out, the exact same shape is still refused with
// an empty warning — the gate stays fail-closed for real deployments.
func TestCheckTLS_CIBatteryShape(t *testing.T) {
	tests := []struct {
		name        string
		insecureDev bool
		wantErr     bool // refusal: the daemon must not start
		wantMarked  bool // InsecurePlaintextActive() for the same shape
	}{
		{
			name:        "ci wildcard ports with insecure_dev warn and start",
			insecureDev: true,
			wantMarked:  true,
		},
		{
			name:    "ci wildcard ports without the opt-in still refuse",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.TLS.Enabled = false
			cfg.TLS.InsecureDev = tc.insecureDev

			warn, err := cfg.CheckTLS(":29090", ":28080")

			if tc.wantErr {
				if err == nil {
					t.Fatal("CheckTLS(:29090, :28080) = nil error, want refusal without the opt-in")
				}
				if warn != "" {
					t.Errorf("refusal must not also warn, got %q", warn)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckTLS(:29090, :28080) = %v, want nil error with the opt-in", err)
			}
			if warn == "" {
				t.Error("opted-in CI shape must start loudly: warning is empty")
			}
			if got := cfg.InsecurePlaintextActive(); got != tc.wantMarked {
				t.Errorf("InsecurePlaintextActive() = %v, want %v", got, tc.wantMarked)
			}
		})
	}
}
