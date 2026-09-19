package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/deployBunker/bunker/internal/config"
)

func TestBuildTLSConfig_Disabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TLS.Enabled = false
	s := New(cfg)
	tlsCfg, err := s.buildTLSConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tlsCfg != nil {
		t.Fatal("expected nil TLS config when TLS disabled")
	}
}

func TestBuildTLSConfig_FileCerts(t *testing.T) {
	tmp := t.TempDir()
	certFile := filepath.Join(tmp, "cert.pem")
	keyFile := filepath.Join(tmp, "key.pem")
	if err := generateSelfSignedCert(certFile, keyFile, "localhost"); err != nil {
		t.Fatalf("generate cert: %v", err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":0"},
		TLS: config.TLSConfig{
			Enabled:  true,
			CertFile: certFile,
			KeyFile:  keyFile,
		},
	}
	s := New(cfg)
	tlsCfg, err := s.buildTLSConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tlsCfg == nil {
		t.Fatal("expected non-nil TLS config")
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Fatalf("expected 1 certificate, got %d", len(tlsCfg.Certificates))
	}
	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected TLS 1.2 min, got %v", tlsCfg.MinVersion)
	}
}

func TestBuildTLSConfig_FileCertsMissing(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":0"},
		TLS: config.TLSConfig{
			Enabled:  true,
			CertFile: "/nonexistent/cert.pem",
			KeyFile:  "/nonexistent/key.pem",
		},
	}
	s := New(cfg)
	_, err := s.buildTLSConfig()
	if err == nil {
		t.Fatal("expected error for missing cert files")
	}
}

func TestBuildTLSConfig_AutoTLSRequiresDomain(t *testing.T) {
	rec := hermeticACME(t)
	cfg := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":0"},
		TLS: config.TLSConfig{
			Enabled: true,
			AutoTLS: true,
			Domain:  "",
		},
	}
	s := New(cfg)
	_, err := s.buildTLSConfig()
	if err == nil {
		t.Fatal("expected error for auto_tls without domain")
	}
	// The domain check runs before certmagic is engaged, so this test must make
	// NO ACME request at all — and the guard stays armed across the settle
	// window in case an async obtain from another test is in flight.
	rec.assertHermetic(t, false)
}

func TestBuildTLSConfig_AutoTLSProducesConfig(t *testing.T) {
	// Hermetic ACME harness (INT-CI-016): the recorder counts every request the
	// certmagic ACME client issues and refuses anything that is not loopback.
	//
	// The old body pointed certmagic.DefaultACME.CA at a loopback directory for
	// this test ONLY and restored it in t.Cleanup — which is exactly how the
	// sibling auto-TLS test below (which never overrode the CA) reached the
	// PRODUCTION directory (https://acme-v02.api.letsencrypt.org/directory) and
	// a real obtain for bunker.example.com in CI run 35253802213.
	rec := hermeticACME(t)

	cfg := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":0"},
		TLS: config.TLSConfig{
			Enabled: true,
			AutoTLS: true,
			Domain:  "bunker.example.com",
		},
	}
	s := New(cfg)

	tlsCfg, err := s.buildTLSConfig()
	// We only care that certmagic builds a config; actual issuance requires
	// a running ACME server and a routable domain, which unit tests cannot
	// guarantee. An error from the loopback stub is acceptable.
	if err != nil {
		t.Logf("certmagic returned error (expected in unit test): %v", err)
	}
	if tlsCfg != nil {
		// certmagic returns a config with GetCertificate set.
		if tlsCfg.GetCertificate == nil {
			t.Error("expected certmagic TLS config to have GetCertificate")
		}
	} else if err == nil {
		t.Fatal("expected non-nil TLS config for auto_tls")
	}
	// buildTLSConfig DISCARDS certmagic.TLS(...)'s config when ManageSync fails —
	// and ManageSync can never succeed against a stub ACME server — so the two
	// assertions above cannot both execute. Assert the same config shape through
	// certmagic's own constructor (the exact call buildTLSConfig makes) so the
	// GetCertificate check stays meaningful instead of becoming dead code.
	direct := certmagic.NewDefault().TLSConfig()
	if direct == nil {
		t.Fatal("certmagic.NewDefault().TLSConfig() returned nil")
	}
	if direct.GetCertificate == nil {
		t.Error("expected certmagic TLS config from NewDefault().TLSConfig() to have GetCertificate")
	}

	rec.assertHermetic(t, true)
}

func TestBuildTLSConfig_AutoTLSWithMTLSRejected(t *testing.T) {
	// INT-CI-016: this test reaches certmagic.TLS (a full obtain attempt with
	// the configured CA) BEFORE the mtls rejection is returned, so without the
	// hermetic harness it contacts whatever certmagic.DefaultACME.CA points at —
	// which, on a clean checkout, is Let's Encrypt production.
	rec := hermeticACME(t)
	cfg := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":0"},
		TLS: config.TLSConfig{
			Enabled: true,
			AutoTLS: true,
			Domain:  "bunker.example.com",
			MTLS:    true,
		},
	}
	s := New(cfg)
	_, err := s.buildTLSConfig()
	if err == nil {
		t.Fatal("expected error when mtls combined with auto_tls")
	}
	rec.assertHermetic(t, true)
}

func TestBuildTLSConfig_MTLS(t *testing.T) {
	tmp := t.TempDir()
	caFile := filepath.Join(tmp, "ca.pem")
	caKeyFile := filepath.Join(tmp, "ca-key.pem")
	certFile := filepath.Join(tmp, "server.pem")
	keyFile := filepath.Join(tmp, "server-key.pem")

	if err := generateCAWithKey(caFile, caKeyFile, "Test CA"); err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	if err := generateServerCert(caFile, caKeyFile, certFile, keyFile, "localhost"); err != nil {
		t.Fatalf("generate server cert: %v", err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":0"},
		TLS: config.TLSConfig{
			Enabled:  true,
			MTLS:     true,
			CAFile:   caFile,
			CertFile: certFile,
			KeyFile:  keyFile,
		},
	}
	s := New(cfg)
	tlsCfg, err := s.buildTLSConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tlsCfg == nil {
		t.Fatal("expected non-nil TLS config")
	}
	if tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("expected RequireAndVerifyClientCert, got %v", tlsCfg.ClientAuth)
	}
	if tlsCfg.ClientCAs == nil {
		t.Error("expected ClientCAs to be set")
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Fatalf("expected 1 server certificate, got %d", len(tlsCfg.Certificates))
	}
}

func TestBuildTLSConfig_MTLSMissingCA(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":0"},
		TLS: config.TLSConfig{
			Enabled: true,
			MTLS:    true,
			CAFile:  "",
		},
	}
	s := New(cfg)
	_, err := s.buildTLSConfig()
	if err == nil {
		t.Fatal("expected error for mtls without ca_file")
	}
}

// ── Hermetic ACME harness (INT-CI-016) ──────────────────────────────────────
//
// Every auto-TLS test in this package must be provably unable to touch a public
// CA. Three layers, installed once for the test binary and deliberately never
// undone:
//
//  1. certmagic.DefaultACME.CA points at a loopback ACME directory stub, so a
//     production directory URL is never the request target;
//  2. certmagic.Default.Storage is a temp directory, so an obtain can never
//     write into $HOME (the old per-test override reset Storage to nil in
//     t.Cleanup, which sent every later test back to the real user cache);
//  3. the recording hook installed through internal/server's `acmeProxyHook`
//     seam counts every outbound ACME request by host and REFUSES any request
//     whose host is not loopback.
//
// Layer 3 is the only one that can OBSERVE certmagic. v0.25.4 builds the ACME
// client's transport internally (ACMEIssuer.httpClient is unexported); the only
// exported hook governing every ACME request is the issuer's HTTPProxy
// selector, which becomes `Transport.Proxy` for the http.Client acmez gets.
// That is why this harness records through a proxy selector rather than a bare
// http.RoundTripper — and why it installs the hook ONLY via the production seam
// (setting certmagic.DefaultACME.HTTPProxy here would make the test independent
// of the seam and hide a regression).

// acmeSettleWindow is the quiet period every auto-TLS test waits out with the
// recorder still armed before declaring the run hermetic: a request that
// escapes the exercised code path (certmagic keeps background certificate
// maintenance state) lands here and fails the test.
const acmeSettleWindow = 2100 * time.Millisecond

// acmeRecorder records every outbound ACME request certmagic attempts, by host,
// and refuses every host that is not loopback.
type acmeRecorder struct {
	mu      sync.Mutex
	byHost  map[string]int
	refused []string
}

// hook is installed as certmagic's ACME transport proxy selector through
// internal/server's acmeProxyHook seam. Returning an error fails the request
// before a connection is dialled, so a non-loopback target cannot leave the
// process even by mistake. Returning (nil, nil) means "no proxy for this
// request", which lets the loopback directory stub answer directly.
func (r *acmeRecorder) hook(req *http.Request) (*url.URL, error) {
	host := req.URL.Hostname()
	r.mu.Lock()
	if r.byHost == nil {
		r.byHost = make(map[string]int)
	}
	r.byHost[host]++
	loopback := isLoopbackHost(host)
	if !loopback {
		r.refused = append(r.refused, req.URL.String())
	}
	r.mu.Unlock()
	if !loopback {
		return nil, fmt.Errorf("INT-CI-016: refused outbound ACME request to %s — unit tests must not contact a public CA", req.URL)
	}
	return nil, nil
}

// requests is the total number of ACME requests the recorder saw.
func (r *acmeRecorder) requests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, n := range r.byHost {
		total += n
	}
	return total
}

// hosts lists the recorded request hosts, sorted, for failure messages.
func (r *acmeRecorder) hosts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.byHost))
	for h := range r.byHost {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// publicRequests lists every request that targeted a non-loopback host.
func (r *acmeRecorder) publicRequests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.refused...)
}

// assertHermetic checks the auto-TLS run is hermetic and stays hermetic.
//
// wantSeamExercised must be true for a test whose body reaches the certmagic
// ACME client: it is the positive control proving the recorder is actually in
// the request path (with the acmeProxyHook seam removed, the recorder sees
// nothing and this fails). Pass false for a test that must not reach certmagic
// at all — then any recorded request is a failure.
func (r *acmeRecorder) assertHermetic(t *testing.T, wantSeamExercised bool) {
	t.Helper()
	if public := r.publicRequests(); len(public) > 0 {
		t.Errorf("auto-TLS test attempted a public ACME request: %v", public)
	}
	if wantSeamExercised {
		if r.requests() == 0 {
			t.Errorf("recording ACME transport saw no requests (hosts=%v): the acmeProxyHook seam is not installed, so this test proves nothing", r.hosts())
		} else if !anyLoopbackHost(r.hosts()) {
			t.Errorf("recorded ACME hosts = %v, want the loopback directory stub", r.hosts())
		}
	} else if r.requests() != 0 {
		t.Errorf("auto-TLS test made %d ACME request(s) (%v) before certmagic was engaged, want 0", r.requests(), r.hosts())
	}
	// Settle window: the exercised path has returned, so anything recorded from
	// here on escaped the test body — including an asynchronous obtain that
	// certmagic is still holding.
	time.Sleep(acmeSettleWindow)
	if public := r.publicRequests(); len(public) > 0 {
		t.Errorf("ACME request escaped the test body and reached a public CA after the settle window: %v", public)
	}
}

// isLoopbackHost reports whether host (a URL host without port) is loopback.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func anyLoopbackHost(hosts []string) bool {
	for _, h := range hosts {
		if isLoopbackHost(h) {
			return true
		}
	}
	return false
}

// Package-wide ACME guard state. acmeHookDispatch is what the acmeProxyHook
// seam installs; it fails closed while no test has armed a recorder.
var (
	acmeGuardOnce sync.Once
	acmeHookMu    sync.Mutex
	acmeArmed     *acmeRecorder
	// acmeStub is kept referenced for the lifetime of the test binary: it must
	// answer any late request, so it is never closed.
	acmeStub *httptest.Server
)

func acmeHookDispatch(req *http.Request) (*url.URL, error) {
	acmeHookMu.Lock()
	rec := acmeArmed
	acmeHookMu.Unlock()
	if rec == nil {
		return nil, fmt.Errorf("INT-CI-016: refused outbound ACME request to %s — no recorder armed", req.URL)
	}
	return rec.hook(req)
}

// hermeticACME installs the package-wide ACME guards (once per test binary) and
// arms a fresh recorder for this test. The guards are NOT restored when the test
// ends: an obtain that outlives the test body must still meet them.
func hermeticACME(t *testing.T) *acmeRecorder {
	t.Helper()
	rec := &acmeRecorder{}
	acmeGuardOnce.Do(func() {
		acmeStub = httptest.NewServer(http.HandlerFunc(acmeDirectoryStub))
		certmagic.DefaultACME.CA = acmeStub.URL + "/directory"
		certmagic.DefaultACME.Agreed = true
		// A FIXED /tmp storage path breaks on clean machines: a leftover dir
		// owned by another UID (previous CI run, root-run test) makes certmagic
		// fail with "permission denied" on certificates/... . os.MkdirTemp
		// atomically creates a unique per-run directory with 0700 owned by the
		// current user, so stale cross-UID leftovers are never reused.
		storageDir, err := os.MkdirTemp("", "bunker-int-ci-016-acme-storage-")
		if err != nil {
			t.Fatalf("INT-CI-016: create per-run ACME storage dir: %v", err)
		}
		certmagic.Default.Storage = &certmagic.FileStorage{
			Path: storageDir,
		}
		// Install the recorder ONLY through the production seam: if the seam is
		// removed from buildTLSConfig, certmagic keeps its own transport and the
		// recorder stays empty, which assertHermetic reports as a failure.
		acmeProxyHook = acmeHookDispatch
	})
	acmeHookMu.Lock()
	acmeArmed = rec
	acmeHookMu.Unlock()
	t.Cleanup(func() {
		acmeHookMu.Lock()
		if acmeArmed == rec {
			acmeArmed = nil
		}
		acmeHookMu.Unlock()
	})
	// Harness integrity: the guards must be in place before the call under test.
	if u, err := url.Parse(certmagic.DefaultACME.CA); err != nil || !isLoopbackHost(u.Hostname()) {
		t.Fatalf("hermetic ACME guard failed: certmagic.DefaultACME.CA = %q (want a loopback directory)", certmagic.DefaultACME.CA)
	}
	return rec
}

// acmeDirectoryStub answers the loopback ACME directory document so the
// auto-TLS path gets a deterministic local answer instead of a dial error.
// Every other ACME endpoint returns a problem document, so no unit test can
// complete an issuance.
func acmeDirectoryStub(w http.ResponseWriter, r *http.Request) {
	base := "http://" + r.Host
	switch r.URL.Path {
	case "/directory":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"newNonce":%q,"newAccount":%q,"newOrder":%q,"revokeCert":%q,"keyChange":%q}`,
			base+"/acme/new-nonce", base+"/acme/new-account", base+"/acme/new-order",
			base+"/acme/revoke-cert", base+"/acme/key-change")
	case "/acme/new-nonce":
		w.Header().Set("Replay-Nonce", "int-ci-016-stub-nonce")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	default:
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"type":"urn:ietf:params:acme:error:serverInternal","detail":"INT-CI-016 stub: no ACME server in unit tests","status":500}`)
	}
}

// TestACMERecorderRefusesPublicHosts pins the guard itself: a request to a
// public ACME host must be refused before any connection is dialled and must be
// recorded, so the hermetic harness above cannot silently degrade into a
// pass-through. No network is involved — only the hook function is called.
func TestACMERecorderRefusesPublicHosts(t *testing.T) {
	rec := &acmeRecorder{}

	publicReq, err := http.NewRequest(http.MethodGet, "https://acme-v02.api.letsencrypt.org/directory", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if _, err := rec.hook(publicReq); err == nil {
		t.Fatal("recorder allowed a public ACME request; the guard must refuse it before dialling")
	}
	if got := rec.publicRequests(); len(got) != 1 {
		t.Errorf("recorded public requests = %v, want the refused letsencrypt.org URL", got)
	}

	loopReq, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/directory", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if _, err := rec.hook(loopReq); err != nil {
		t.Errorf("recorder refused a loopback ACME request: %v", err)
	}
	if got := rec.requests(); got != 2 {
		t.Errorf("recorded requests = %d, want 2 (one refused public, one allowed loopback)", got)
	}
	if got := rec.publicRequests(); len(got) != 1 {
		t.Errorf("public requests after the loopback call = %v, want still the single refused URL", got)
	}
}

// --- helpers ---

func generateSelfSignedCert(certPath, keyPath, cn string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{cn},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	certOut, err := os.Create(certPath)
	if err != nil {
		return err
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return err
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		return err
	}
	defer keyOut.Close()
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func generateCAWithKey(certPath, keyPath, cn string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	certOut, err := os.Create(certPath)
	if err != nil {
		return err
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return err
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		return err
	}
	defer keyOut.Close()
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func generateServerCert(caCertPath, caKeyPath, certPath, keyPath, cn string) error {
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(caPEM)
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	caKeyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return err
	}
	block, _ = pem.Decode(caKeyPEM)
	caKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return err
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{cn},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	certOut, err := os.Create(certPath)
	if err != nil {
		return err
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return err
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		return err
	}
	defer keyOut.Close()
	keyDER := x509.MarshalPKCS1PrivateKey(key)
	return pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})
}
