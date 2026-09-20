package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
	"github.com/go-chi/chi/v5"
)

// ---------------------------------------------------------------------------
// test certificates
// ---------------------------------------------------------------------------

// selfSignedTLSConfig builds a fresh self-signed ECDSA certificate for the test
// server. Each call returns a DISTINCT certificate, which is what lets the
// rotation cases below be honest: "the same URL now presents a different leaf"
// is the exact condition pinning has to survive.
func selfSignedTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Bunker Test"}, CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}
}

func pinOfTLSConfig(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	if len(cfg.Certificates) == 0 || len(cfg.Certificates[0].Certificate) == 0 {
		t.Fatal("tls config carries no certificate")
	}
	sum := sha256.Sum256(cfg.Certificates[0].Certificate[0])
	return hex.EncodeToString(sum[:])
}

// certRotator holds the certificate a test TLS server presents, so a test can
// ROTATE the leaf without restarting the listener (and therefore without
// changing the URL — which is exactly the condition pinning must detect).
type certRotator struct {
	mu  sync.RWMutex
	cfg *tls.Config
}

func (r *certRotator) Set(cfg *tls.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
}

func (r *certRotator) pin() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return pinHex(r.cfg.Certificates[0].Certificate[0])
}

// getCertificate lets one listener serve whichever certificate is current.
func (r *certRotator) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return &r.cfg.Certificates[0], nil
}

func pinHex(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// rawTLSServer is a TLS listener that owns its certificate, so a test can
// rotate the leaf under a stable URL.
type rawTLSServer struct {
	URL  string
	ln   net.Listener
	rot  *certRotator
	done chan struct{}
}

func (s *rawTLSServer) Close() {
	close(s.done)
	s.ln.Close()
}

// newRawTLSBunkerd starts a TLS listener serving the Bunkerd connect handler
// (not httptest, whose certificate cannot be swapped on a live listener).
func newRawTLSBunkerd(t *testing.T, tlsCfg *tls.Config, mock bunkerv1connect.BunkerdHandler) *rawTLSServer {
	t.Helper()
	rot := &certRotator{cfg: tlsCfg}

	r := chi.NewRouter()
	path, handler := bunkerv1connect.NewBunkerdHandler(mock)
	r.Mount(path, handler)
	// The CLI probes /health during trust-on-first-use; answer it so the probe
	// exercises the same listener the RPCs use.
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	base := &tls.Config{
		GetCertificate: rot.getCertificate,
		MinVersion:     tls.VersionTLS12,
	}
	httpSrv := &http.Server{Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.Serve(tls.NewListener(ln, base)) }()

	srv := &rawTLSServer{
		URL:  "https://" + ln.Addr().String(),
		ln:   ln,
		rot:  rot,
		done: make(chan struct{}),
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		srv.Close()
	})
	return srv
}

// newRotatableTLSBunkerd returns a TLS server whose certificate can be rotated
// in place under a stable URL.
func newRotatableTLSBunkerd(t *testing.T, tlsCfg *tls.Config, mock bunkerv1connect.BunkerdHandler) (*rawTLSServer, *certRotator) {
	t.Helper()
	srv := newRawTLSBunkerd(t, tlsCfg, mock)
	return srv, srv.rot
}

// newTLSBunkerd is newRotatableTLSBunkerd for tests that never rotate, and it
// ALSO proves the premise every pinning assertion rests on: the pin the test
// asserts against is the pin a client observes over the wire (and it is the
// pin the daemon would write to tls.cert_file, so the printed
// `openssl … -fingerprint` instruction reproduces it exactly).
func newTLSBunkerd(t *testing.T, tlsCfg *tls.Config, mock bunkerv1connect.BunkerdHandler) (*rawTLSServer, string) {
	t.Helper()
	srv, rot := newRotatableTLSBunkerd(t, tlsCfg, mock)
	observed, _, err := FetchLeafCertificate(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("test premise broken: cannot observe the served certificate: %v", err)
	}
	if observed != rot.pin() {
		t.Fatalf("test premise broken: observed pin %s != configured pin %s", observed, rot.pin())
	}
	if !tlsCfgMatchesLeaf(t, tlsCfg, srv.URL) {
		t.Fatalf("test premise broken: the served leaf is not the configured certificate")
	}
	return srv, observed
}

// tlsCfgMatchesLeaf reports whether the leaf served at url is the certificate
// in tlsCfg — the check that makes "the expected pin" a fact about the wire
// rather than about the local config struct.
func tlsCfgMatchesLeaf(t *testing.T, tlsCfg *tls.Config, url string) bool {
	t.Helper()
	_, leaf, err := FetchLeafCertificate(context.Background(), url)
	if err != nil {
		t.Fatalf("observe leaf: %v", err)
	}
	return bytes.Equal(leaf.Raw, tlsCfg.Certificates[0].Certificate[0])
}

func testServerInfo(hostname string) *v1.ServerInfoResponse {
	return &v1.ServerInfoResponse{
		Hostname:      hostname,
		Version:       "v0.3.0",
		UptimeSeconds: 42,
		AgentCount:    1,
		MaxAgents:     100,
	}
}

// useTempHome points the CLI config at a fresh directory and clears process
// state that would otherwise leak between cases.
func useTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	ResetCertPinCache()
	t.Cleanup(ResetCertPinCache)
	return home
}

// captureStderr captures os.Stderr while fn runs.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return buf.String()
}

// ---------------------------------------------------------------------------
// Certificate pinning: the trust decision
// ---------------------------------------------------------------------------

// TestBuildClientTLSConfig pins how each entry shape maps onto a transport.
// The important rows are the refusals: every one of them exists so no
// configuration can silently end up skipping verification.
func TestBuildClientTLSConfig(t *testing.T) {
	const goodPin = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// GAP-141: an insecure entry is only honored behind the explicit
	// environment acknowledgement. The refusal rows below assert the combined
	// semantics; the dedicated battery is tls_insecure_test.go.
	t.Setenv(TLSInsecureAckEnv, "1")

	tests := []struct {
		name        string
		entry       ServerEntry
		wantErr     string
		wantNilCfg  bool
		wantInsecur bool
		wantPinned  bool
	}{
		{
			name:       "plain http with no trust fields uses the default transport",
			entry:      ServerEntry{Name: "local", URL: "http://127.0.0.1:8080"},
			wantNilCfg: true,
		},
		{
			name:        "tls_insecure is the explicit opt-out",
			entry:       ServerEntry{Name: "dev", URL: "https://host:9090", TLSInsecure: true},
			wantInsecur: true,
		},
		{
			name:       "a pinned entry verifies against the pin",
			entry:      ServerEntry{Name: "pinned", URL: "https://host:9090", CertPin: goodPin, TLSMode: string(TLSModeSelfSigned)},
			wantPinned: true,
		},
		{
			name:       "a pin without an explicit mode still pins",
			entry:      ServerEntry{Name: "legacy", URL: "https://host:9090", CertPin: goodPin},
			wantPinned: true,
		},
		{
			name:       "tls_mode system verifies against the root store",
			entry:      ServerEntry{Name: "ca", URL: "https://host:9090", TLSMode: string(TLSModeSystem)},
			wantNilCfg: true,
		},
		{
			name:    "pin plus tls_insecure is contradictory",
			entry:   ServerEntry{Name: "bad", URL: "https://host:9090", CertPin: goodPin, TLSInsecure: true},
			wantErr: "contradictory TLS configuration",
		},
		{
			name:    "self-signed mode with no pin refuses with instructions",
			entry:   ServerEntry{Name: "unpinned", URL: "https://host:9090", TLSMode: string(TLSModeSelfSigned)},
			wantErr: "no certificate is pinned",
		},
		{
			name:    "a malformed pin refuses instead of unpinning",
			entry:   ServerEntry{Name: "short", URL: "https://host:9090", CertPin: "abcd"},
			wantErr: "unusable cert_pin",
		},
		{
			name:    "an unknown tls_mode refuses",
			entry:   ServerEntry{Name: "weird", URL: "https://host:9090", TLSMode: "trust-me"},
			wantErr: "unknown tls_mode",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := buildClientTLSConfig(tc.entry)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("buildClientTLSConfig(%+v) = nil error, want error containing %q", tc.entry, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				if cfg != nil {
					t.Fatalf("refusal returned a usable tls config: %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildClientTLSConfig(%+v): %v", tc.entry, err)
			}
			switch {
			case tc.wantNilCfg:
				if cfg != nil {
					t.Fatalf("expected the default transport (nil tls config), got %+v", cfg)
				}
			case tc.wantInsecur:
				if cfg == nil || !cfg.InsecureSkipVerify {
					t.Fatalf("expected InsecureSkipVerify, got %+v", cfg)
				}
				if cfg.VerifyPeerCertificate != nil {
					t.Fatal("insecure mode must not also pin")
				}
			case tc.wantPinned:
				if cfg == nil || cfg.VerifyPeerCertificate == nil {
					t.Fatalf("expected a pinning verify callback, got %+v", cfg)
				}
				if !cfg.InsecureSkipVerify {
					t.Fatal("a self-signed leaf has no chain to verify: InsecureSkipVerify must be set, with the pin doing the real check")
				}
			}
		})
	}
}

// TestVerifyPinnedLeaf pins the fingerprint comparison itself, including the
// two ways a pinned certificate can stop being usable (expiry, wrong pin).
func TestVerifyPinnedLeaf(t *testing.T) {
	tlsCfg := selfSignedTLSConfig(t)
	der := tlsCfg.Certificates[0].Certificate[0]
	good := pinOfTLSConfig(t, tlsCfg)
	other := selfSignedTLSConfig(t)
	otherPin := pinOfTLSConfig(t, other)
	otherDER := other.Certificates[0].Certificate[0]

	tests := []struct {
		name    string
		pin     string
		certs   [][]byte
		wantErr string
	}{
		{name: "matching pin passes", pin: good, certs: [][]byte{der}},
		{name: "matching pin is case-insensitive", pin: strings.ToUpper(good), certs: [][]byte{der}},
		{name: "mismatched pin fails naming both values", pin: good, certs: [][]byte{otherDER}, wantErr: "certificate pin mismatch"},
		{name: "a pin for a different certificate names the live one", pin: otherPin, certs: [][]byte{der}, wantErr: FormatCertPin(otherPin)},
		{name: "no certificate at all is a mismatch, not a pass", pin: good, certs: nil, wantErr: "presented no certificate"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyPinnedLeaf(tc.pin, "test-server", tc.certs)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyPinnedLeaf = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verifyPinnedLeaf = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}

	t.Run("mismatch message names both fingerprints and the re-pin command", func(t *testing.T) {
		err := verifyPinnedLeaf(good, "prod", [][]byte{otherDER})
		if err == nil {
			t.Fatal("expected a mismatch error")
		}
		msg := err.Error()
		for _, want := range []string{FormatCertPin(good), FormatCertPin(otherPin), "--accept-cert", "man-in-the-middle"} {
			if !strings.Contains(msg, want) {
				t.Errorf("mismatch message missing %q:\n%s", want, msg)
			}
		}
	})

	t.Run("expired pinned certificate is a named failure", func(t *testing.T) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("key: %v", err)
		}
		serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		tmpl := x509.Certificate{
			SerialNumber:          serial,
			Subject:               pkix.Name{CommonName: "expired"},
			NotBefore:             time.Now().Add(-48 * time.Hour),
			NotAfter:              time.Now().Add(-24 * time.Hour),
			BasicConstraintsValid: true,
		}
		expiredDER, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatalf("create expired certificate: %v", err)
		}
		sum := sha256.Sum256(expiredDER)
		expiredPin := hex.EncodeToString(sum[:])

		err = verifyPinnedLeaf(expiredPin, "stale", [][]byte{expiredDER})
		if err == nil {
			t.Fatal("expected an expiry error")
		}
		if !strings.Contains(err.Error(), "EXPIRED") {
			t.Fatalf("error = %q, want it to name the expiry", err.Error())
		}
	})
}

// TestCertPinHelpers covers the shared presentation/validation helpers, because
// a pin that validates wrongly would let an unpinned entry look pinned.
func TestCertPinHelpers(t *testing.T) {
	cert := selfSignedTLSConfig(t)
	der := cert.Certificates[0].Certificate[0]
	pin := CertPinHexDER(der)

	t.Run("hex pin is 64 lowercase hex chars", func(t *testing.T) {
		if len(pin) != certPinHexLen {
			t.Fatalf("pin length = %d, want %d", len(pin), certPinHexLen)
		}
		if pin != strings.ToLower(pin) {
			t.Fatalf("pin %q is not lowercase", pin)
		}
		if !ValidCertPin(pin) {
			t.Fatalf("ValidCertPin(%q) = false, want true", pin)
		}
	})

	invalid := []string{"", "abcd", strings.Repeat("z", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)}
	for _, s := range invalid {
		if ValidCertPin(s) {
			t.Errorf("ValidCertPin(%q) = true, want false", s)
		}
	}

	t.Run("format is the canonical sha256: prefix", func(t *testing.T) {
		if got, want := FormatCertPin(strings.ToUpper(pin)), CertPinPrefix+pin; got != want {
			t.Errorf("FormatCertPin = %q, want %q", got, want)
		}
	})

	t.Run("parsed and DER forms agree", func(t *testing.T) {
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if CertPinHex(leaf) != pin {
			t.Errorf("CertPinHex(parsed) = %q, want %q", CertPinHex(leaf), pin)
		}
		if CertPinHex(nil) != "" {
			t.Error("CertPinHex(nil) must be empty")
		}
	})
}

// ---------------------------------------------------------------------------
// Trust on first use, end to end over TLS
// ---------------------------------------------------------------------------

func TestRegisterServer_SelfSignedTrustOnFirstUse(t *testing.T) {
	useTempHome(t)
	tlsCfg := selfSignedTLSConfig(t)
	srv, wantPin := newTLSBunkerd(t, tlsCfg, &mockBunkerdServer{info: testServerInfo("tls-host")})

	stdout := captureStdout(t, func() {
		var err error
		stderr := captureStderr(t, func() {
			err = RegisterServerWithOptions(ConnectOptions{
				Name:    "tofu",
				URL:     srv.URL,
				TLSMode: TLSModeSelfSigned,
			})
		})
		if err != nil {
			t.Fatalf("first-use registration failed: %v", err)
		}
		if !strings.Contains(stderr, "TRUST ON FIRST USE") {
			t.Errorf("first-use warning missing from stderr:\n%s", stderr)
		}
		if !strings.Contains(stderr, FormatCertPin(wantPin)) {
			t.Errorf("first-use warning must name the fingerprint %s:\n%s", FormatCertPin(wantPin), stderr)
		}
	})

	cfg, err := LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}
	entry, ok := cfg.Servers["tofu"]
	if !ok {
		t.Fatalf("server %q not registered (have %v)", "tofu", cfg.Servers)
	}
	if entry.CertPin != wantPin {
		t.Errorf("stored pin = %q, want the served leaf pin %q", entry.CertPin, wantPin)
	}
	if entry.TLSMode != string(TLSModeSelfSigned) {
		t.Errorf("stored tls_mode = %q, want %q", entry.TLSMode, TLSModeSelfSigned)
	}
	if entry.CertPinSetAt == "" {
		t.Error("cert_pin_set_at must record when the pin was established")
	}
	if entry.TLSInsecure {
		t.Error("a pinned entry must not be marked tls_insecure")
	}
	if !strings.Contains(stdout, "pinned certificate") {
		t.Errorf("stdout must report the trust posture, got:\n%s", stdout)
	}

	// The pin must be usable by the SAME process for the registration dial, and
	// by a later process from the file.
	if got := CachedCertPin(srv.URL); got != wantPin {
		t.Errorf("in-process pin cache = %q, want %q", got, wantPin)
	}
	if _, err := ConnectServerWithEntry(entry); err != nil {
		t.Errorf("re-dial with the stored pin failed: %v", err)
	}
}

// TestRegisterServer_SelfSignedSecondConnectUsesPin is the "subsequent connect
// succeeds via the pin" case: the same certificate, a fresh process (pin cache
// cleared), verification driven by the stored fingerprint.
func TestRegisterServer_SelfSignedSecondConnectUsesPin(t *testing.T) {
	useTempHome(t)
	tlsCfg := selfSignedTLSConfig(t)
	srv, wantPin := newTLSBunkerd(t, tlsCfg, &mockBunkerdServer{info: testServerInfo("tls-host")})

	first := ConnectOptions{Name: "again", URL: srv.URL, TLSMode: TLSModeSelfSigned}
	if err := RegisterServerWithOptions(first); err != nil {
		t.Fatalf("first register: %v", err)
	}
	ResetCertPinCache() // simulate a later CLI invocation

	stdout := captureStdout(t, func() {
		var err error
		stderr := captureStderr(t, func() {
			err = RegisterServerWithOptions(first)
		})
		if err != nil {
			t.Fatalf("second register with the same certificate failed: %v", err)
		}
		if strings.Contains(stderr, "TRUST ON FIRST USE") {
			t.Errorf("second connect must not re-warn first use:\n%s", stderr)
		}
	})
	if !strings.Contains(stdout, FormatCertPin(wantPin)) {
		t.Errorf("stdout should report the retained pin %s:\n%s", FormatCertPin(wantPin), stdout)
	}

	entry := configEntry(t, "again")
	if entry.CertPin != wantPin {
		t.Errorf("pin after second connect = %q, want %q", entry.CertPin, wantPin)
	}
}

// TestRegisterServer_RotatedCertRefused is the loud-refusal case: same URL,
// different certificate, no --accept-cert.
func TestRegisterServer_RotatedCertRefused(t *testing.T) {
	useTempHome(t)
	srv, rot := newRotatableTLSBunkerd(t, selfSignedTLSConfig(t), &mockBunkerdServer{info: testServerInfo("tls-host")})
	firstPin := rot.pin()

	if err := RegisterServerWithOptions(ConnectOptions{Name: "rot", URL: srv.URL, TLSMode: TLSModeSelfSigned}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if entry := configEntry(t, "rot"); entry.CertPin != firstPin {
		t.Fatalf("premise: stored pin = %q, want %q", entry.CertPin, firstPin)
	}
	ResetCertPinCache()

	// Rotate: the SAME listener now presents a different self-signed leaf.
	rot.Set(selfSignedTLSConfig(t))
	rotatedPin := rot.pin()
	if rotatedPin == firstPin {
		t.Fatal("test premise broken: rotation produced the same certificate")
	}

	err := RegisterServerWithOptions(ConnectOptions{Name: "rot", URL: srv.URL, TLSMode: TLSModeSelfSigned})
	if err == nil {
		t.Fatal("connect against a rotated certificate must fail, got nil error")
	}
	msg := err.Error()
	for _, want := range []string{"certificate pin mismatch", FormatCertPin(firstPin), FormatCertPin(rotatedPin), "--accept-cert"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message missing %q:\n%s", want, msg)
		}
	}

	// The stored pin must be untouched by a refused connect.
	if entry := configEntry(t, "rot"); entry.CertPin != firstPin {
		t.Errorf("pin after refused connect = %q, want the original %q", entry.CertPin, firstPin)
	}

	// An RPC against the rotated server must also refuse: the pin is enforced
	// on every command, not just connect.
	entry := configEntry(t, "rot")
	if _, err := ConnectServerWithEntry(entry); err == nil {
		t.Fatal("ConnectServerWithEntry against a rotated certificate must fail")
	} else if !strings.Contains(err.Error(), "certificate pin mismatch") {
		t.Errorf("RPC error = %q, want a pin mismatch", err.Error())
	}
}

// TestRegisterServer_RotatedCertAcceptedWithFlag is the deliberate re-pin path.
func TestRegisterServer_RotatedCertAcceptedWithFlag(t *testing.T) {
	useTempHome(t)
	srv, rot := newRotatableTLSBunkerd(t, selfSignedTLSConfig(t), &mockBunkerdServer{info: testServerInfo("tls-host")})
	firstPin := rot.pin()

	opts := ConnectOptions{Name: "repin", URL: srv.URL, TLSMode: TLSModeSelfSigned}
	if err := RegisterServerWithOptions(opts); err != nil {
		t.Fatalf("first register: %v", err)
	}
	ResetCertPinCache()

	rot.Set(selfSignedTLSConfig(t))
	rotatedPin := rot.pin()

	opts.AcceptCert = true
	captureStdout(t, func() {
		var err error
		stderr := captureStderr(t, func() { err = RegisterServerWithOptions(opts) })
		if err != nil {
			t.Fatalf("--accept-cert re-pin failed: %v", err)
		}
		if !strings.Contains(stderr, "RE-PINNING") {
			t.Errorf("re-pin must announce itself loudly, got stderr:\n%s", stderr)
		}
		if !strings.Contains(stderr, FormatCertPin(firstPin)) {
			t.Errorf("re-pin warning should name the certificate being replaced (%s):\n%s", FormatCertPin(firstPin), stderr)
		}
	})
	if entry := configEntry(t, "repin"); entry.CertPin != rotatedPin {
		t.Errorf("pin after --accept-cert = %q, want %q", entry.CertPin, rotatedPin)
	}
}

// TestRegisterServer_SelfSignedWithoutAcceptAgainstInsecureEntry covers the
// cross-configuration refusals: an existing entry that disables verification
// (or verifies against the system roots) must not be silently converted into a
// pin, and vice versa.
func TestRegisterServer_SelfSignedWithoutAcceptAgainstInsecureEntry(t *testing.T) {
	// GAP-141: registering an insecure entry now needs the acknowledgement.
	t.Setenv(TLSInsecureAckEnv, "1")

	t.Run("existing tls_insecure entry refuses to be pinned", func(t *testing.T) {
		useTempHome(t)
		srv, _ := newTLSBunkerd(t, selfSignedTLSConfig(t), &mockBunkerdServer{info: testServerInfo("tls-host")})

		// Register it the historical (insecure) way first, then ask to pin.
		if err := RegisterServerWithOptions(ConnectOptions{Name: "mixed", URL: srv.URL, Insecure: true}); err != nil {
			t.Fatalf("insecure register: %v", err)
		}
		ResetCertPinCache()

		err := RegisterServerWithOptions(ConnectOptions{Name: "mixed", URL: srv.URL, TLSMode: TLSModeSelfSigned})
		if err == nil {
			t.Fatal("expected a refusal, got nil")
		}
		if !strings.Contains(err.Error(), "tls-insecure") {
			t.Errorf("refusal must name the existing insecure registration, got: %s", err.Error())
		}
		if entry := configEntry(t, "mixed"); entry.CertPin != "" {
			t.Errorf("refused connect must not have written a pin, got %q", entry.CertPin)
		}
	})

	t.Run("existing system-mode entry refuses to become pinned", func(t *testing.T) {
		useTempHome(t)
		srv, _ := newTLSBunkerd(t, selfSignedTLSConfig(t), &mockBunkerdServer{info: testServerInfo("tls-host")})

		// Register the entry with --tls-insecure so it exists, then rewrite its
		// recorded decision to tls_mode: system — the state an operator reaches
		// by registering a CA-signed daemon. (NormalizeTLSMode deliberately
		// records the insecure opt-out over an explicit system mode, so the
		// entry has to be written directly to model this.)
		if err := RegisterServerWithOptions(ConnectOptions{Name: "sys", URL: srv.URL, Insecure: true}); err != nil {
			t.Fatalf("initial register: %v", err)
		}
		cfg, err := LoadCLIConfig()
		if err != nil {
			t.Fatalf("LoadCLIConfig: %v", err)
		}
		entry := cfg.Servers["sys"]
		entry.TLSInsecure = false
		entry.TLSMode = string(TLSModeSystem)
		cfg.Servers["sys"] = entry
		if err := SaveCLIConfig(cfg); err != nil {
			t.Fatalf("SaveCLIConfig: %v", err)
		}
		ResetCertPinCache()

		err = RegisterServerWithOptions(ConnectOptions{Name: "sys", URL: srv.URL, TLSMode: TLSModeSelfSigned})
		if err == nil {
			t.Fatal("expected a refusal, got nil")
		}
		if !strings.Contains(err.Error(), "system root store") {
			t.Errorf("refusal must name the recorded trust decision, got: %s", err.Error())
		}
		if entry := configEntry(t, "sys"); entry.CertPin != "" {
			t.Errorf("refused connect must not have written a pin, got %q", entry.CertPin)
		}
	})
}

// TestRegisterServer_PinAndInsecureRefused covers the contradictory flag pair.
func TestRegisterServer_PinAndInsecureRefused(t *testing.T) {
	useTempHome(t)
	// GAP-141: even WITH the acknowledgement, the contradictory pair is refused.
	t.Setenv(TLSInsecureAckEnv, "1")
	srv, _ := newTLSBunkerd(t, selfSignedTLSConfig(t), &mockBunkerdServer{info: testServerInfo("tls-host")})

	err := RegisterServerWithOptions(ConnectOptions{
		Name:     "contradiction",
		URL:      srv.URL,
		TLSMode:  TLSModeSelfSigned,
		Insecure: true,
	})
	if err == nil {
		t.Fatal("--tls self-signed with --tls-insecure must be refused")
	}
	if !strings.Contains(err.Error(), "contradictory") {
		t.Errorf("error = %q, want it to name the contradiction", err.Error())
	}
	if cfg, _ := LoadCLIConfig(); len(cfg.Servers) != 0 {
		t.Errorf("a refused connect must not register anything, got %v", cfg.Servers)
	}
}

// TestRegisterServer_SelfSignedNeedsHTTPS keeps the mode from being applied to
// a plain-HTTP URL, where there is no certificate to pin.
func TestRegisterServer_SelfSignedNeedsHTTPS(t *testing.T) {
	useTempHome(t)
	err := RegisterServerWithOptions(ConnectOptions{Name: "plain", URL: "http://127.0.0.1:8080", TLSMode: TLSModeSelfSigned})
	if err == nil {
		t.Fatal("expected a refusal for --tls self-signed over http://")
	}
	if !strings.Contains(err.Error(), "https://") {
		t.Errorf("error = %q, want it to require https://", err.Error())
	}
}

// TestRegisterServer_UnpinnedSelfSignedEntryRefusesToDial is the "self-signed
// server, no pin, not in first-use mode → refusal with instruction" case, at
// the transport layer that every later command uses.
func TestRegisterServer_UnpinnedSelfSignedEntryRefusesToDial(t *testing.T) {
	useTempHome(t)
	srv, _ := newTLSBunkerd(t, selfSignedTLSConfig(t), &mockBunkerdServer{info: testServerInfo("tls-host")})

	entry := ServerEntry{Name: "unpinned", URL: srv.URL, TLSMode: string(TLSModeSelfSigned)}
	if _, err := ConnectServerWithEntry(entry); err == nil {
		t.Fatal("an unpinned self-signed entry must refuse to dial")
	} else if !strings.Contains(err.Error(), "no certificate is pinned") {
		t.Errorf("error = %q, want the pinning instruction", err.Error())
	}

	// Same through the client factory every other command uses.
	if _, err := newBunkerdClientChecked(entry, time.Second); err == nil {
		t.Fatal("newBunkerdClientChecked must surface the unusable trust configuration")
	}
}

// TestNewBunkerdClient_RefusesUnusableTrustConfig pins the fail-closed
// behaviour of the shared client factory: a contradictory entry yields a client
// whose requests refuse, so no command can reach the network with verification
// silently downgraded.
func TestNewBunkerdClient_RefusesUnusableTrustConfig(t *testing.T) {
	useTempHome(t)
	entry := ServerEntry{
		Name:        "bad",
		URL:         "https://127.0.0.1:1",
		CertPin:     strings.Repeat("a", 64),
		TLSInsecure: true,
	}
	client := newBunkerdClient(entry)
	if client == nil {
		t.Fatal("newBunkerdClient returned nil")
	}
	_, err := client.ServerInfo(t.Context(), connect.NewRequest(&v1.ServerInfoRequest{}))
	if err == nil {
		t.Fatal("a contradictory entry must refuse, got a successful RPC")
	}
	if !strings.Contains(err.Error(), "contradictory TLS configuration") {
		t.Errorf("error = %q, want it to name the contradiction", err.Error())
	}
}

// TestConnectServerWithEntry_UnknownAuthorityMentionsThePinFlow covers the
// no-trust-decision case: a https:// URL against a self-signed daemon with no
// --tls flag must fail verification AND tell the operator how to pin.
func TestConnectServerWithEntry_UnknownAuthorityMentionsThePinFlow(t *testing.T) {
	useTempHome(t)
	srv, _ := newTLSBunkerd(t, selfSignedTLSConfig(t), &mockBunkerdServer{info: testServerInfo("tls-host")})

	_, err := ConnectServerWithEntry(ServerEntry{Name: "bare", URL: srv.URL})
	if err == nil {
		t.Fatal("a self-signed server must fail system-root verification")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--tls self-signed") {
		t.Errorf("verification failure must point at the pinning flow:\n%s", msg)
	}
	if !strings.Contains(msg, "--tls-insecure") || !strings.Contains(msg, "Do not work around") {
		t.Errorf("hint should warn against the insecure workaround:\n%s", msg)
	}
}

// TestFetchLeafCertificate covers the observation half, including the failure
// classification that lets callers name a remedy instead of relaying net/http
// prose.
func TestFetchLeafCertificate(t *testing.T) {
	t.Run("observes the served leaf", func(t *testing.T) {
		tlsCfg := selfSignedTLSConfig(t)
		srv, wantPin := newTLSBunkerd(t, tlsCfg, &mockBunkerdServer{info: testServerInfo("tls-host")})
		pin, leaf, err := FetchLeafCertificate(t.Context(), srv.URL)
		if err != nil {
			t.Fatalf("FetchLeafCertificate: %v", err)
		}
		if pin != wantPin {
			t.Errorf("pin = %q, want %q", pin, wantPin)
		}
		if leaf == nil {
			t.Fatal("leaf certificate not returned")
		}
		if leaf.Subject.Organization[0] != "Bunker Test" {
			t.Errorf("unexpected leaf subject: %v", leaf.Subject)
		}
	})

	t.Run("plain http server is classified not-tls", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		_, _, err := FetchLeafCertificate(t.Context(), strings.Replace(srv.URL, "http://", "https://", 1))
		if err == nil {
			t.Fatal("expected an error for a plain-HTTP server")
		}
		var fetchErr *CertFetchError
		if !asCertFetchError(err, &fetchErr) {
			t.Fatalf("error %v is not a *CertFetchError", err)
		}
		if fetchErr.Class != CertFetchClassNotTLS {
			t.Errorf("class = %q, want %q (error: %v)", fetchErr.Class, CertFetchClassNotTLS, err)
		}
		if !strings.Contains(err.Error(), "not speaking TLS") {
			t.Errorf("error should say the server is not TLS: %v", err)
		}
	})

	t.Run("unresolvable host is classified dns and is unreachable-class", func(t *testing.T) {
		_, _, err := FetchLeafCertificate(t.Context(), "https://no-such-host.invalid:9090")
		if err == nil {
			t.Fatal("expected a DNS error")
		}
		var fetchErr *CertFetchError
		if !asCertFetchError(err, &fetchErr) {
			t.Fatalf("error %v is not a *CertFetchError", err)
		}
		if fetchErr.Class != CertFetchClassDNS {
			t.Errorf("class = %q, want %q (%v)", fetchErr.Class, CertFetchClassDNS, err)
		}
		if !isUnreachableClass(fetchErr.Class) {
			t.Error("a DNS failure must be unreachable-class so an existing pin is preserved")
		}
	})

	t.Run("http url is refused before any dial", func(t *testing.T) {
		_, _, err := FetchLeafCertificate(t.Context(), "http://127.0.0.1:1")
		if err == nil {
			t.Fatal("expected a refusal for a non-https URL")
		}
		if !strings.Contains(err.Error(), "not https://") {
			t.Errorf("error = %q, want it to require https://", err.Error())
		}
	})
}

// TestFetchLeafCertificate_KeepsExistingPinOnUnreachable covers the transient
// failure rule: an already-pinned entry must not silently lose its trust anchor
// because the host was briefly unreachable (the dial below still has to satisfy
// the pin, so this is strictly safer than dropping it).
func TestFetchLeafCertificate_KeepsExistingPinOnUnreachable(t *testing.T) {
	useTempHome(t)
	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"gone": {
				Name:         "gone",
				URL:          "https://no-such-host.invalid:9090",
				TLSMode:      string(TLSModeSelfSigned),
				CertPin:      strings.Repeat("a", 64),
				CertPinSetAt: "2026-09-01T00:00:00Z",
			},
		},
		ActiveServer: "gone",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	err := RegisterServerWithOptions(ConnectOptions{Name: "gone", URL: "https://no-such-host.invalid:9090", TLSMode: TLSModeSelfSigned})
	if err == nil {
		t.Fatal("registration against an unreachable host must still fail")
	}
	// The DNS failure surfaces first (nothing to pin), and the entry's pin is
	// intact on disk.
	entry := configEntry(t, "gone")
	if entry.CertPin != strings.Repeat("a", 64) {
		t.Errorf("pin = %q, want the original pin preserved", entry.CertPin)
	}
	if entry.CertPinSetAt != "2026-09-01T00:00:00Z" {
		t.Errorf("cert_pin_set_at = %q, want the original timestamp", entry.CertPinSetAt)
	}
}

// ---------------------------------------------------------------------------
// CLI surface
// ---------------------------------------------------------------------------

// TestConnectCommand_TLSSelfSignedFlag is the end-to-end CLI case for criterion
// (1): `bunker connect --tls self-signed https://…` against a live TLS server
// presenting a self-signed certificate.
func TestConnectCommand_TLSSelfSignedFlag(t *testing.T) {
	useTempHome(t)
	tlsCfg := selfSignedTLSConfig(t)
	srv, wantPin := newTLSBunkerd(t, tlsCfg, &mockBunkerdServer{info: testServerInfo("cli-tls")})

	cmd := NewConnectCommand()
	cmd.SetArgs([]string{"--tls", "self-signed", "--name", "cli", srv.URL})

	stdout := captureStdout(t, func() {
		stderr := captureStderr(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Fatalf("connect --tls self-signed: %v", err)
			}
		})
		if !strings.Contains(stderr, "TRUST ON FIRST USE") {
			t.Errorf("CLI must print the first-use warning:\n%s", stderr)
		}
	})
	if !strings.Contains(stdout, "Connected to cli-tls") {
		t.Errorf("stdout missing connection detail:\n%s", stdout)
	}
	if entry := configEntry(t, "cli"); entry.CertPin != wantPin {
		t.Errorf("pin = %q, want %q", entry.CertPin, wantPin)
	}
}

// TestConnectCommand_TLSModeParsing covers the mode spellings an operator may
// type and the flag pair that must be refused.
func TestConnectCommand_TLSModeParsing(t *testing.T) {
	tests := []struct {
		flag    string
		want    TLSMode
		wantErr string
	}{
		{flag: "self-signed", want: TLSModeSelfSigned},
		{flag: "self_signed", want: TLSModeSelfSigned},
		{flag: "pin", want: TLSModeSelfSigned},
		{flag: "tofu", want: TLSModeSelfSigned},
		{flag: "SELF-SIGNED", want: TLSModeSelfSigned},
		{flag: "system", want: TLSModeSystem},
		{flag: "ca", want: TLSModeSystem},
		{flag: "", want: ""},
		{flag: "insecure", want: TLSModeInsecure},
		{flag: "bogus", wantErr: "unknown --tls mode"},
	}
	for _, tc := range tests {
		t.Run("flag="+tc.flag, func(t *testing.T) {
			got, err := ParseTLSMode(tc.flag)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseTLSMode(%q) err = %v, want %q", tc.flag, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTLSMode(%q): %v", tc.flag, err)
			}
			if got != tc.want {
				t.Errorf("ParseTLSMode(%q) = %q, want %q", tc.flag, got, tc.want)
			}
		})
	}

	t.Run("a bogus --tls value fails the command without touching the network", func(t *testing.T) {
		useTempHome(t)
		cmd := NewConnectCommand()
		cmd.SetArgs([]string{"--tls", "bogus", "https://127.0.0.1:1"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "unknown --tls mode") {
			t.Fatalf("err = %v, want an unknown-mode refusal", err)
		}
	})
}

// TestConnectCommand_TLSInsecureAndSelfSignedRefused covers the contradictory
// FLAG pair at the command layer.
func TestConnectCommand_TLSInsecureAndSelfSignedRefused(t *testing.T) {
	useTempHome(t)
	cmd := NewConnectCommand()
	cmd.SetArgs([]string{"--tls", "self-signed", "--tls-insecure", "https://127.0.0.1:1"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected a refusal for --tls self-signed --tls-insecure")
	}
	if !strings.Contains(err.Error(), "skips certificate verification") {
		t.Errorf("error = %q, want it to name the contradiction", err.Error())
	}
	if cfg, _ := LoadCLIConfig(); len(cfg.Servers) != 0 {
		t.Errorf("refused connect must not register anything, got %v", cfg.Servers)
	}
}

// TestConfigRoundTrip_CertPin pins the on-disk shape: the new keys must
// round-trip, and an entry that has none must keep the historical layout so
// existing config files stay readable (and unchanged after a rewrite).
func TestConfigRoundTrip_CertPin(t *testing.T) {
	useTempHome(t)

	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"pinned": {
				Name:         "pinned",
				URL:          "https://bunker.example:9090",
				Token:        "tok",
				TLSMode:      string(TLSModeSelfSigned),
				CertPin:      strings.Repeat("a1", 32),
				CertPinSetAt: "2026-09-19T00:00:00Z",
				ConnectedAt:  "2026-09-19T00:00:00Z",
			},
			"legacy": {
				Name:        "legacy",
				URL:         "http://127.0.0.1:8080",
				Token:       "tok",
				ConnectedAt: "2026-01-01T00:00:00Z",
			},
		},
		ActiveServer: "pinned",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	loaded, err := LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}
	pinned := loaded.Servers["pinned"]
	if pinned.CertPin != strings.Repeat("a1", 32) {
		t.Errorf("CertPin = %q, want the stored pin", pinned.CertPin)
	}
	if pinned.TLSMode != string(TLSModeSelfSigned) {
		t.Errorf("TLSMode = %q, want %q", pinned.TLSMode, TLSModeSelfSigned)
	}
	if pinned.CertPinSetAt != "2026-09-19T00:00:00Z" {
		t.Errorf("CertPinSetAt = %q", pinned.CertPinSetAt)
	}
	legacy := loaded.Servers["legacy"]
	if legacy.CertPin != "" || legacy.TLSMode != "" {
		t.Errorf("legacy entry gained trust fields: %+v", legacy)
	}

	// The optional keys must be omitted, not written as empty strings.
	full, err := os.ReadFile(filepath.Join(testBunkerHome(t), "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(full), `cert_pin: ""`) {
		t.Errorf("empty cert_pin must be omitted from the config file:\n%s", full)
	}
	if strings.Contains(string(full), `tls_mode: ""`) {
		t.Errorf("empty tls_mode must be omitted from the config file:\n%s", full)
	}
}

// TestExistingServerEntryShapesKeepWorking pins that configs written before
// this change load unchanged and that a legacy entry's recorded decision still
// governs — with one deliberate exception introduced by GAP-141: an insecure
// entry is now refused until the operator acknowledges it in the environment.
// The migration is therefore visible here rather than discovered in production.
func TestExistingServerEntryShapesKeepWorking(t *testing.T) {
	useTempHome(t)
	const legacyYAML = `servers:
  old-insecure:
    name: old-insecure
    url: https://old.example:9090
    token: tok
    tls_insecure: true
    connected_at: "2026-09-01T12:00:00Z"
  old-plain:
    name: old-plain
    url: http://127.0.0.1:8080
    token: tok
    connected_at: "2026-09-01T12:00:00Z"
active_server: old-insecure
`
	writeConfigFile(t, legacyYAML)

	cfg, err := LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}
	insecure := cfg.Servers["old-insecure"]
	if !insecure.TLSInsecure {
		t.Error("legacy tls_insecure entry lost its flag")
	}
	if insecure.CertPin != "" || insecure.TLSMode != "" {
		t.Errorf("legacy entry invented trust fields: %+v", insecure)
	}

	// GAP-141: the legacy insecure entry is refused until acknowledged...
	t.Setenv(TLSInsecureAckEnv, "")
	if _, err := buildClientTLSConfig(insecure); err == nil {
		t.Error("an unacknowledged legacy insecure entry must refuse to dial")
	} else if !strings.Contains(err.Error(), TLSInsecureAckEnv) {
		t.Errorf("error = %q, want the acknowledgement refusal", err.Error())
	}

	// ...and behaves exactly as before once acknowledged.
	t.Setenv(TLSInsecureAckEnv, "1")
	tlsCfg, err := buildClientTLSConfig(insecure)
	if err != nil {
		t.Fatalf("acknowledged legacy insecure entry must still be dialable: %v", err)
	}
	if tlsCfg == nil || !tlsCfg.InsecureSkipVerify {
		t.Errorf("legacy insecure entry must keep skipping verification once acknowledged, got %+v", tlsCfg)
	}

	plain := cfg.Servers["old-plain"]
	if tlsCfg, err := buildClientTLSConfig(plain); err != nil || tlsCfg != nil {
		t.Errorf("legacy plain entry must use the default transport (got %+v, err %v)", tlsCfg, err)
	}
}

// TestContradictoryEntryFromYAMLIsRefused covers the file-shaped contradiction:
// a config written by hand (or by an older tool) carrying BOTH cert_pin and
// tls_insecure must be refused, not resolved in whichever direction runs first.
// It exercises the entry that every command loads, so the refusal is not
// confined to the connect path.
func TestContradictoryEntryFromYAMLIsRefused(t *testing.T) {
	useTempHome(t)
	// A real TLS server so the connect path gets past the certificate
	// observation and reaches the contradiction check.
	srv, pin := newTLSBunkerd(t, selfSignedTLSConfig(t), &mockBunkerdServer{info: testServerInfo("both")})

	contradictoryYAML := `servers:
  both:
    name: both
    url: ` + srv.URL + `
    token: tok
    tls_insecure: true
    tls_mode: self-signed
    cert_pin: "` + pin + `"
    connected_at: "2026-09-01T12:00:00Z"
active_server: both
`
	writeConfigFile(t, contradictoryYAML)

	entry := configEntry(t, "both")
	if entry.CertPin == "" || !entry.TLSInsecure {
		t.Fatalf("test premise broken: entry did not load both trust fields: %+v", entry)
	}

	// The transport builder itself must refuse (no direction chosen for us).
	if cfg, err := buildClientTLSConfig(entry); err == nil {
		t.Fatalf("buildClientTLSConfig accepted a contradictory entry (cfg=%+v)", cfg)
	} else if !strings.Contains(err.Error(), "contradictory TLS configuration") {
		t.Errorf("error = %q, want it to name the contradiction", err.Error())
	}

	// So must a real RPC through the shared client factory.
	client := newBunkerdClient(entry)
	if _, err := client.ServerInfo(t.Context(), connect.NewRequest(&v1.ServerInfoRequest{})); err == nil {
		t.Fatal("an RPC on a contradictory entry must be refused")
	} else if !strings.Contains(err.Error(), "contradictory TLS configuration") {
		t.Errorf("RPC error = %q, want the contradiction", err.Error())
	}

	// And the connect path must refuse before dialling, telling the operator how
	// to resolve it either way.
	err := RegisterServerWithOptions(ConnectOptions{Name: "both", URL: srv.URL, TLSMode: TLSModeSelfSigned})
	if err == nil {
		t.Fatal("connect against a contradictory entry must be refused")
	}
	msg := err.Error()
	for _, want := range []string{"contradictory", "--tls self-signed", "--tls-insecure"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal missing %q:\n%s", want, msg)
		}
	}

	// A refusal must not rewrite the entry.
	after := configEntry(t, "both")
	if after.CertPin != entry.CertPin || after.TLSInsecure != entry.TLSInsecure {
		t.Errorf("refused connect mutated the entry: %+v -> %+v", entry, after)
	}
}

// TestConnectCommand_HelpDocumentsTLSSurface keeps the pinning flow
// discoverable: the two flags an operator must learn are in --help, and the
// contradiction rule is stated there rather than only in an error message.
func TestConnectCommand_HelpDocumentsTLSSurface(t *testing.T) {
	useTempHome(t)
	cmd := NewConnectCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		_ = cmd.Execute()
	})
	for _, want := range []string{"--tls", "--accept-cert", "self-signed", "system", "fingerprint"} {
		if !strings.Contains(output, want) {
			t.Errorf("connect --help missing %q:\n%s", want, output)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func configEntry(t *testing.T, name string) ServerEntry {
	t.Helper()
	cfg, err := LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}
	entry, ok := cfg.Servers[name]
	if !ok {
		t.Fatalf("server %q not in config (have %v)", name, cfg.Servers)
	}
	return entry
}

func testBunkerHome(t *testing.T) string {
	t.Helper()
	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("HOME is not set")
	}
	return filepath.Join(home, ".bunker")
}

func writeConfigFile(t *testing.T, contents string) {
	t.Helper()
	dir := testBunkerHome(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func asCertFetchError(err error, target **CertFetchError) bool {
	return errors.As(err, target)
}
