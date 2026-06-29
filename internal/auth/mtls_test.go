package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
)

func TestNewMTLSAuth_MissingCAFile(t *testing.T) {
	_, err := NewMTLSAuth("/nonexistent/ca.pem", "")
	if err == nil {
		t.Fatal("expected error for missing CA file")
	}
}

func TestNewMTLSAuth_InvalidPEM(t *testing.T) {
	tmp := t.TempDir()
	badFile := filepath.Join(tmp, "bad.pem")
	if err := os.WriteFile(badFile, []byte("not a pem"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := NewMTLSAuth(badFile, "")
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestNewMTLSAuth_ValidCA(t *testing.T) {
	tmp := t.TempDir()
	caFile := filepath.Join(tmp, "ca.pem")
	if err := generateCA(caFile, "Test CA"); err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	auth, err := NewMTLSAuth(caFile, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth.clientCAs == nil {
		t.Fatal("expected clientCAs to be set")
	}
}

func TestMTLSAuth_Authenticate_NoTLSState(t *testing.T) {
	tmp := t.TempDir()
	caFile := filepath.Join(tmp, "ca.pem")
	if err := generateCA(caFile, "Test CA"); err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	auth, err := NewMTLSAuth(caFile, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	_, err = wrapped(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing TLS state")
	}
	if called {
		t.Error("handler should not be called on auth failure")
	}
	connectErr, ok := err.(*connect.Error)
	if !ok {
		t.Fatalf("expected connect.Error, got %T", err)
	}
	if connectErr.Code() != connect.CodeUnauthenticated {
		t.Errorf("expected Unauthenticated, got %v", connectErr.Code())
	}
}

func TestMTLSAuth_Authenticate_ValidCert(t *testing.T) {
	tmp := t.TempDir()
	caFile := filepath.Join(tmp, "ca.pem")
	if err := generateCA(caFile, "Test CA"); err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	auth, err := NewMTLSAuth(caFile, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := auth.WrapUnary(handler)

	// Create a client cert signed by the CA
	clientCert, clientKey, err := generateClientCert(caFile, filepath.Join(tmp, "ca-key.pem"), "client")
	if err != nil {
		t.Fatalf("generate client cert: %v", err)
	}

	ctx := withTLSState(context.Background(), clientCert, clientKey)
	req := connect.NewRequest(&dummyMsg{})
	_, err = wrapped(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("handler was not called")
	}
}

func TestMTLSAuth_Authenticate_CNMismatch(t *testing.T) {
	tmp := t.TempDir()
	caFile := filepath.Join(tmp, "ca.pem")
	if err := generateCA(caFile, "Test CA"); err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	auth, err := NewMTLSAuth(caFile, "expected-cn")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	}
	wrapped := auth.WrapUnary(handler)

	// Create a client cert with wrong CN
	clientCert, clientKey, err := generateClientCert(caFile, filepath.Join(tmp, "ca-key.pem"), "wrong-cn")
	if err != nil {
		t.Fatalf("generate client cert: %v", err)
	}

	ctx := withTLSState(context.Background(), clientCert, clientKey)
	req := connect.NewRequest(&dummyMsg{})
	_, err = wrapped(ctx, req)
	if err == nil {
		t.Fatal("expected error for CN mismatch")
	}
	if called {
		t.Error("handler should not be called on auth failure")
	}
	connectErr, ok := err.(*connect.Error)
	if !ok {
		t.Fatalf("expected connect.Error, got %T", err)
	}
	if connectErr.Code() != connect.CodePermissionDenied {
		t.Errorf("expected PermissionDenied, got %v", connectErr.Code())
	}
}

func TestBuildMTLSConfig(t *testing.T) {
	tmp := t.TempDir()
	caFile := filepath.Join(tmp, "ca.pem")
	if err := generateCA(caFile, "Test CA"); err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	cfg, err := BuildMTLSConfig(caFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("expected RequireAndVerifyClientCert, got %v", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Fatal("expected ClientCAs to be set")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected TLS 1.2 min, got %v", cfg.MinVersion)
	}
}

func TestBuildMTLSConfigWithCert(t *testing.T) {
	tmp := t.TempDir()
	caFile := filepath.Join(tmp, "ca.pem")
	caKeyFile := filepath.Join(tmp, "ca-key.pem")
	if err := generateCAWithKey(caFile, caKeyFile, "Test CA"); err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	serverCertFile := filepath.Join(tmp, "server.pem")
	serverKeyFile := filepath.Join(tmp, "server-key.pem")
	if err := generateServerCert(caFile, caKeyFile, serverCertFile, serverKeyFile, "localhost"); err != nil {
		t.Fatalf("generate server cert: %v", err)
	}
	cfg, err := BuildMTLSConfigWithCert(caFile, serverCertFile, serverKeyFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("expected 1 server certificate, got %d", len(cfg.Certificates))
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("expected RequireAndVerifyClientCert, got %v", cfg.ClientAuth)
	}
}

func TestNewMTLSInterceptor(t *testing.T) {
	tmp := t.TempDir()
	caFile := filepath.Join(tmp, "ca.pem")
	if err := generateCA(caFile, "Test CA"); err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	interceptor, err := NewMTLSInterceptor(caFile, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if interceptor == nil {
		t.Fatal("expected non-nil interceptor")
	}
	_, ok := interceptor.(*MTLSAuth)
	if !ok {
		t.Fatalf("expected *MTLSAuth, got %T", interceptor)
	}
}

func TestTLSConnectionStateFromContext_NoRequest(t *testing.T) {
	_, ok := TLSConnectionStateFromContext(context.Background())
	if ok {
		t.Error("expected false for context without request")
	}
}

// --- helpers (package-local to avoid duplicate dummyMsg) ---

func generateCA(path, cn string) error {
	keyFile := path[:len(path)-4] + "-key.pem"
	return generateCAWithKey(path, keyFile, cn)
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

func generateClientCert(caCertPath, caKeyPath, cn string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(caPEM)
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	caKeyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return nil, nil, err
	}
	block, _ = pem.Decode(caKeyPEM)
	caKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
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
		SerialNumber: big.NewInt(3),
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

// withTLSState injects a tls.ConnectionState into the context so that
// MTLSAuth.authenticate can find it. We use the http.ServerContextKey trick
// that TLSConnectionStateFromContext looks for.
func withTLSState(parent context.Context, cert *x509.Certificate, key *ecdsa.PrivateKey) context.Context {
	// Build a real TLS connection state with the peer certificate
	state := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{cert},
		Version:          tls.VersionTLS12,
	}
	// We need to attach this to an *http.Request in the context.
	req, _ := http.NewRequest("POST", "/", nil)
	req.TLS = state
	return context.WithValue(parent, http.ServerContextKey, req)
}
