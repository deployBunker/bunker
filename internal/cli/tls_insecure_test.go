package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/audit"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// GAP-141, the CLI half. The insecure dial path is now a NAMED, ACKNOWLEDGED
// decision that a configured pin always overrides, and an insecure session
// declares itself to the daemon so the audit trail records it. These tests
// drive the real gates (the transport builder every command uses, and the
// connect path) rather than the helper functions in isolation, so a future
// caller that bypasses one of them fails here.

// gap141Pin is a well-formed sha256 pin (64 hex chars) for entries that must be
// seen as configured-with-a-pin.
const gap141Pin = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestRequireInsecureAck covers the gate's decision table: which entries are
// refused, and why the refusal is named.
func TestRequireInsecureAck(t *testing.T) {
	tests := []struct {
		name    string
		ack     string // BUNKER_ALLOW_TLS_INSECURE value for this case
		entry   ServerEntry
		wantErr string
	}{
		{
			name:  "acknowledged and nothing to verify against is allowed",
			ack:   "1",
			entry: ServerEntry{Name: "dev", URL: "https://dev:9090", TLSInsecure: true},
		},
		{
			name:    "no acknowledgement is refused",
			entry:   ServerEntry{Name: "dev", URL: "https://dev:9090", TLSInsecure: true},
			wantErr: TLSInsecureAckEnv,
		},
		{
			name:    "an unrelated value is not an acknowledgement",
			ack:     "yes-please",
			entry:   ServerEntry{Name: "dev", URL: "https://dev:9090", TLSInsecure: true},
			wantErr: TLSInsecureAckEnv,
		},
		{
			name:    "a pinned certificate refuses outright even when acknowledged",
			ack:     "1",
			entry:   ServerEntry{Name: "pinned", URL: "https://host:9090", TLSInsecure: true, CertPin: gap141Pin},
			wantErr: "the pin wins",
		},
		{
			name:    "a pinned certificate refuses with no acknowledgement too",
			entry:   ServerEntry{Name: "pinned", URL: "https://host:9090", TLSInsecure: true, CertPin: gap141Pin},
			wantErr: "the pin wins",
		},
		{
			name:    "tls_mode insecure plus a pin is refused as well",
			ack:     "1",
			entry:   ServerEntry{Name: "pinned", URL: "https://host:9090", TLSMode: string(TLSModeInsecure), CertPin: gap141Pin},
			wantErr: "the pin wins",
		},
		{
			name:  "tls_mode insecure alone is refused without an acknowledgement",
			entry: ServerEntry{Name: "dev", URL: "https://dev:9090", TLSMode: string(TLSModeInsecure)},
			// The wantErr is set by the sub-test below, which is why this row
			// only asserts the refusal EXISTS: the entry requests insecure
			// trust by the tls_mode spelling instead of tls_insecure.
			wantErr: TLSInsecureAckEnv,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(TLSInsecureAckEnv, tc.ack)
			err := RequireInsecureAck(tc.entry)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("RequireInsecureAck(%+v) = %v, want nil", tc.entry, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("RequireInsecureAck(%+v) = nil, want an error naming %q", tc.entry, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}

	t.Run("tls_mode insecure alone is refused without ack once acknowledged is checked", func(t *testing.T) {
		entry := ServerEntry{Name: "dev", URL: "https://dev:9090", TLSMode: string(TLSModeInsecure)}
		t.Setenv(TLSInsecureAckEnv, "")
		err := RequireInsecureAck(entry)
		if err == nil || !strings.Contains(err.Error(), TLSInsecureAckEnv) {
			t.Fatalf("err = %v, want the acknowledgement refusal", err)
		}
	})

	t.Run("the acknowledgement accepts the truthy spellings", func(t *testing.T) {
		entry := ServerEntry{Name: "dev", URL: "https://dev:9090", TLSInsecure: true}
		for _, value := range []string{"1", "true", "TRUE", "yes", "on", " 1 "} {
			t.Setenv(TLSInsecureAckEnv, value)
			if err := RequireInsecureAck(entry); err != nil {
				t.Errorf("%s=%q refused: %v", TLSInsecureAckEnv, value, err)
			}
		}
		for _, value := range []string{"", "0", "false", "no", "off", "please"} {
			t.Setenv(TLSInsecureAckEnv, value)
			if err := RequireInsecureAck(entry); err == nil {
				t.Errorf("%s=%q was accepted as an acknowledgement", TLSInsecureAckEnv, value)
			}
		}
	})
}

// TestBuildClientTLSConfig_GAP141 is the transport-level gate: every command
// builds its client through buildClientTLSConfig, so a refusal here is a refusal
// for the whole CLI.
func TestBuildClientTLSConfig_GAP141(t *testing.T) {
	tests := []struct {
		name        string
		ack         string
		entry       ServerEntry
		wantErr     string
		wantInsecur bool
	}{
		{
			name:        "unacknowledged tls_insecure never yields an insecure transport",
			entry:       ServerEntry{Name: "dev", URL: "https://host:9090", TLSInsecure: true},
			wantErr:     TLSInsecureAckEnv,
			wantInsecur: false,
		},
		{
			name:        "acknowledged tls_insecure yields the insecure transport",
			ack:         "1",
			entry:       ServerEntry{Name: "dev", URL: "https://host:9090", TLSInsecure: true},
			wantInsecur: true,
		},
		{
			name:    "a pin wins over an acknowledged tls_insecure",
			ack:     "1",
			entry:   ServerEntry{Name: "pinned", URL: "https://host:9090", TLSInsecure: true, CertPin: gap141Pin},
			wantErr: "the pin wins",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(TLSInsecureAckEnv, tc.ack)
			cfg, err := buildClientTLSConfig(tc.entry)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("buildClientTLSConfig(%+v) = (%+v, nil), want an error containing %q", tc.entry, cfg, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				if cfg != nil {
					t.Fatalf("a refusal returned a usable tls config: %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildClientTLSConfig(%+v): %v", tc.entry, err)
			}
			if got := cfg != nil && cfg.InsecureSkipVerify; got != tc.wantInsecur {
				t.Errorf("InsecureSkipVerify = %v, want %v (cfg %+v)", got, tc.wantInsecur, cfg)
			}
		})
	}
}

// TestInsecureSessionWarnsLoudly proves the honored path is not silent: the
// warning goes to stderr (so it survives stdout capture), names the
// acknowledgement that authorized it, and names the audit marker the daemon
// will write.
func TestInsecureSessionWarnsLoudly(t *testing.T) {
	t.Setenv(TLSInsecureAckEnv, "1")
	entry := ServerEntry{Name: "dev", URL: "https://dev:9090", TLSInsecure: true}

	var cfgInsecure bool
	stderr := captureStderr(t, func() {
		cfg, err := buildClientTLSConfig(entry)
		if err != nil {
			t.Fatalf("buildClientTLSConfig: %v", err)
		}
		cfgInsecure = cfg != nil && cfg.InsecureSkipVerify
	})
	if !cfgInsecure {
		t.Fatal("premise broken: the acknowledged entry did not build an insecure transport")
	}
	for _, want := range []string{"INSECURE TLS SESSION", TLSInsecureAckEnv, audit.TLSUnverifiedMarker, "identity is NOT checked"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("warning missing %q:\n%s", want, stderr)
		}
	}
}

// TestInsecureSessionDeclaresItselfOnTheWire is the CLI half of the audit
// marker: the requests an insecure session sends carry the declaration header,
// and a verified (pinned) session's do not. The header is captured by a live
// TLS listener the CLI's OWN client factory dials, so the assertion is on the
// bytes the daemon would receive.
func TestInsecureSessionDeclaresItselfOnTheWire(t *testing.T) {
	tests := []struct {
		name    string
		entry   func(url, pin string) ServerEntry
		wantHdr bool
	}{
		{
			name: "insecure session declares itself",
			entry: func(url, _ string) ServerEntry {
				return ServerEntry{Name: "dev", URL: url, TLSInsecure: true}
			},
			wantHdr: true,
		},
		{
			name: "pinned session declares nothing",
			entry: func(url, pin string) ServerEntry {
				return ServerEntry{Name: "pinned", URL: url, TLSMode: string(TLSModeSelfSigned), CertPin: pin}
			},
			wantHdr: false,
		},
		{
			name: "system-root session declares nothing",
			entry: func(url, _ string) ServerEntry {
				return ServerEntry{Name: "unreached", URL: url, TLSMode: string(TLSModeSystem)}
			},
			wantHdr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			useTempHome(t)
			t.Setenv(TLSInsecureAckEnv, "1")

			var gotHeader string
			probe := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeader = r.Header.Get(audit.UnverifiedHeader)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"unimplemented","message":"probe"}`))
			}))
			t.Cleanup(probe.Close)
			probePin := CertPinHexDER(probe.Certificate().Raw)

			entry := tc.entry(probe.URL, probePin)
			// The client is built exactly as every command builds it.
			client := newBunkerdClient(entry)
			// A failure is expected (the probe answers 404 unimplemented): the
			// subject is the header the client set on the way out.
			_, _ = client.ServerInfo(context.Background(), connectRequestServerInfo())

			if tc.wantHdr {
				if gotHeader != "1" {
					t.Errorf("%s on the wire = %q, want %q", audit.UnverifiedHeader, gotHeader, "1")
				}
				return
			}
			if gotHeader != "" {
				t.Errorf("a verified session must not declare anything, got %s: %q", audit.UnverifiedHeader, gotHeader)
			}
		})
	}
}

// TestConnectCommand_TLSInsecureNeedsAck is the end-to-end CLI case: without the
// acknowledgement the command refuses and registers NOTHING; with it, the server
// is registered (and the warning is printed).
func TestConnectCommand_TLSInsecureNeedsAck(t *testing.T) {
	t.Run("without the acknowledgement nothing is registered", func(t *testing.T) {
		useTempHome(t)
		t.Setenv(TLSInsecureAckEnv, "")

		cmd := NewConnectCommand()
		cmd.SetArgs([]string{"--tls-insecure", "--name", "dev", "https://127.0.0.1:1"})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("--tls-insecure without an acknowledgement must be refused")
		}
		if !strings.Contains(err.Error(), TLSInsecureAckEnv) {
			t.Errorf("error = %q, want it to name %s", err.Error(), TLSInsecureAckEnv)
		}
		if cfg, _ := LoadCLIConfig(); len(cfg.Servers) != 0 {
			t.Errorf("a refused connect must not register anything, got %v", cfg.Servers)
		}
	})

	t.Run("with the acknowledgement the insecure server is registered and warned about", func(t *testing.T) {
		useTempHome(t)
		t.Setenv(TLSInsecureAckEnv, "1")

		srv, _ := newTLSBunkerd(t, selfSignedTLSConfig(t), &mockBunkerdServer{info: testServerInfo("insecure-host")})
		cmd := NewConnectCommand()
		cmd.SetArgs([]string{"--tls-insecure", "--name", "dev", srv.URL})

		stdout := captureStdout(t, func() {
			stderr := captureStderr(t, func() {
				if err := cmd.Execute(); err != nil {
					t.Fatalf("connect --tls-insecure with an acknowledgement: %v", err)
				}
			})
			if !strings.Contains(stderr, "INSECURE TLS SESSION") {
				t.Errorf("the honored insecure path must warn loudly:\n%s", stderr)
			}
		})
		if !strings.Contains(stdout, "VERIFICATION DISABLED") {
			t.Errorf("stdout must report the trust posture:\n%s", stdout)
		}
		entry := configEntry(t, "dev")
		if !entry.TLSInsecure {
			t.Error("the registered entry lost tls_insecure")
		}
		if entry.CertPin != "" {
			t.Errorf("an insecure registration must not pin, got %q", entry.CertPin)
		}
	})

	t.Run("an unacknowledged entry in the config refuses every later command", func(t *testing.T) {
		useTempHome(t)
		srv, _ := newTLSBunkerd(t, selfSignedTLSConfig(t), &mockBunkerdServer{info: testServerInfo("insecure-host")})
		writeConfigFile(t, `servers:
  legacy-insecure:
    name: legacy-insecure
    url: `+srv.URL+`
    token: tok
    tls_insecure: true
active_server: legacy-insecure
`)
		t.Setenv(TLSInsecureAckEnv, "")

		entry := configEntry(t, "legacy-insecure")
		client := newBunkerdClient(entry)
		_, err := client.ServerInfo(context.Background(), connectRequestServerInfo())
		if err == nil {
			t.Fatal("an unacknowledged insecure entry must refuse every RPC")
		}
		if !strings.Contains(err.Error(), TLSInsecureAckEnv) {
			t.Errorf("error = %q, want the acknowledgement refusal", err.Error())
		}

		// Same through the checked factory (the connect path).
		if _, err := newBunkerdClientChecked(entry, time.Second); err == nil {
			t.Fatal("newBunkerdClientChecked must surface the acknowledgement refusal")
		} else if !strings.Contains(err.Error(), TLSInsecureAckEnv) {
			t.Errorf("checked factory error = %q, want the acknowledgement refusal", err.Error())
		}
	})
}

// TestConnectCommand_PinnedEntryRefusesInsecureEvenWhenAcknowledged is criterion
// (2) at the command layer: pin wins, acknowledgement or not.
func TestConnectCommand_PinnedEntryRefusesInsecureEvenWhenAcknowledged(t *testing.T) {
	useTempHome(t)
	t.Setenv(TLSInsecureAckEnv, "1")

	srv, pin := newTLSBunkerd(t, selfSignedTLSConfig(t), &mockBunkerdServer{info: testServerInfo("tls-host")})
	writeConfigFile(t, `servers:
  pinned:
    name: pinned
    url: `+srv.URL+`
    token: tok
    tls_mode: self-signed
    cert_pin: "`+pin+`"
active_server: pinned
`)

	before := configEntry(t, "pinned")
	err := RegisterServerWithOptions(ConnectOptions{Name: "pinned", URL: srv.URL, Insecure: true})
	if err == nil {
		t.Fatal("--tls-insecure against a pinned entry must be refused, acknowledgement or not")
	}
	msg := err.Error()
	for _, want := range []string{"the pin wins", auditPinName(before.CertPin)} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal missing %q:\n%s", want, msg)
		}
	}

	// The pin must survive the refusal untouched.
	after := configEntry(t, "pinned")
	if after.CertPin != before.CertPin || after.TLSInsecure {
		t.Errorf("a refused connect mutated the entry: %+v -> %+v", before, after)
	}
}

// TestConnectCommand_HelpDocumentsTheInsecureGate keeps the new requirement
// discoverable: an operator reading --help learns that --tls-insecure needs an
// acknowledgement, instead of meeting the refusal for the first time in CI.
func TestConnectCommand_HelpDocumentsTheInsecureGate(t *testing.T) {
	useTempHome(t)
	cmd := NewConnectCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		_ = cmd.Execute()
	})
	for _, want := range []string{TLSInsecureAckEnv, "--tls-insecure"} {
		if !strings.Contains(output, want) {
			t.Errorf("connect --help missing %q:\n%s", want, output)
		}
	}
}

// connectRequestServerInfo builds the ServerInfo request envelope used by the
// probes below.
func connectRequestServerInfo() *connect.Request[v1.ServerInfoRequest] {
	return connect.NewRequest(&v1.ServerInfoRequest{})
}

// auditPinName renders a pin for assertions (the same canonical form the
// refusal uses).
func auditPinName(pin string) string {
	return FormatCertPin(pin)
}
