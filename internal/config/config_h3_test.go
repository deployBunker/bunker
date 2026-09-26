package config

import (
	"strings"
	"testing"
)

// TestH3KeysLoad proves the two new server keys exist on the wire (YAML and
// env), since a knob the loader cannot see is not a knob.
func TestH3KeysLoad(t *testing.T) {
	path := writeConfig(t, `
server:
  grpc_addr: "127.0.0.1:19090"
  rest_addr: "127.0.0.1:18080"
  h3_enabled: true
  h3_addr: "127.0.0.1:18080"
tls:
  enabled: true
  cert_file: "/etc/bunkerd/tls/cert.pem"
  key_file: "/etc/bunkerd/tls/key.pem"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Server.H3Enabled || cfg.Server.H3Addr != "127.0.0.1:18080" {
		t.Fatalf("h3 keys did not load: enabled=%v addr=%q", cfg.Server.H3Enabled, cfg.Server.H3Addr)
	}

	// Env overrides beat the file, like every other server key.
	t.Setenv("BUNKERD_SERVER_H3_ENABLED", "false")
	t.Setenv("BUNKERD_SERVER_H3_ADDR", "127.0.0.1:18443")
	cfgEnv, err := Load(path)
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfgEnv.Server.H3Enabled {
		t.Fatal("env override to false ignored")
	}
	if cfgEnv.Server.H3Addr != "127.0.0.1:18443" {
		t.Fatalf("env override ignored: %q", cfgEnv.Server.H3Addr)
	}
}

// TestH3DefaultsAreOff proves the additive posture: a config that says nothing
// about HTTP/3 keeps a TCP-only daemon, exactly as before this row.
func TestH3DefaultsAreOff(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Server.H3Enabled || cfg.Server.H3Addr != "" {
		t.Fatalf("h3 keys are not off by default: %+v", cfg.Server)
	}
}

// TestH3ListenAddrDerivation proves the one-port default is derived, not
// duplicated: with no explicit h3_addr the UDP socket takes the REST listener's
// port number (O-5), falls back to the gRPC listener when no REST listener
// exists, and takes an explicit server.h3_addr verbatim when one is configured.
// The derivation is what keeps the Alt-Svc authority and the bound socket from
// drifting apart.
func TestH3ListenAddrDerivation(t *testing.T) {
	cases := []struct {
		name    string
		grpc    string
		rest    string
		h3      string
		want    string
		wantErr string
	}{
		{name: "rest listener wins", grpc: "127.0.0.1:19090", rest: "127.0.0.1:18080", want: "127.0.0.1:18080"},
		{name: "no rest listener falls back to grpc", grpc: "127.0.0.1:19090", rest: "", want: "127.0.0.1:19090"},
		{name: "identical rest and grpc", grpc: ":18080", rest: ":18080", want: ":18080"},
		{name: "explicit h3_addr wins", grpc: ":19090", rest: ":18080", h3: ":18443", want: ":18443"},
		{name: "wildcard binds are a valid authority", grpc: ":19090", rest: ":18080", h3: "0.0.0.0:18443", want: "0.0.0.0:18443"},
		{name: "malformed h3_addr is refused by Validate", grpc: ":19090", rest: ":18080", h3: "18443", want: "18443", wantErr: "server.h3_addr must be host:port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Server.GRPCAddr = tc.grpc
			cfg.Server.RESTAddr = tc.rest
			cfg.Server.H3Addr = tc.h3

			// The address is only ever derived for an enabled listener; the
			// validation arm needs TLS so the h3-without-TLS refusal (its own
			// case below) does not mask it.
			cfg.TLS.Enabled = true
			cfg.TLS.CertFile = "/etc/bunkerd/tls/cert.pem"
			cfg.TLS.KeyFile = "/etc/bunkerd/tls/key.pem"
			cfg.Server.H3Enabled = true

			if got := cfg.H3ListenAddr(); got != tc.want {
				t.Fatalf("H3ListenAddr() = %q, want %q", got, tc.want)
			}
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestH3RequiresTLS is the row's hard constraint at the config layer: QUIC
// always encrypts, so h3 on a cleartext listener is refused BEFORE any socket
// opens rather than quietly serving HTTP/1.1+HTTP/2 to an operator who believes
// HTTP/3 is up. The control arm proves the refusal is about the TLS pairing and
// not about the key existing.
func TestH3RequiresTLS(t *testing.T) {
	cases := []struct {
		name    string
		tlsOn   bool
		h3      bool
		wantErr string
	}{
		{name: "h3 off, cleartext daemon", tlsOn: false, h3: false},
		{name: "h3 off, TLS daemon", tlsOn: true, h3: false},
		{name: "h3 on with TLS", tlsOn: true, h3: true},
		{name: "h3 on over cleartext", tlsOn: false, h3: true, wantErr: "server.h3_enabled requires tls.enabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.TLS.Enabled = tc.tlsOn
			cfg.TLS.CertFile = "/etc/bunkerd/tls/cert.pem"
			cfg.TLS.KeyFile = "/etc/bunkerd/tls/key.pem"
			cfg.Server.H3Enabled = tc.h3
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
