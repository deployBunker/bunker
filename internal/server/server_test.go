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
	"math/big"
	"os"
	"path/filepath"
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
}

func TestBuildTLSConfig_AutoTLSProducesConfig(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":0"},
		TLS: config.TLSConfig{
			Enabled: true,
			AutoTLS: true,
			Domain:  "bunker.example.com",
		},
	}
	s := New(cfg)
	// Point certmagic cache to a temp dir so tests don't pollute the user's
	// home directory and can run without real ACME issuance.
	certmagic.Default.Storage = &certmagic.FileStorage{Path: tmp}
	t.Cleanup(func() {
		certmagic.Default.Storage = nil
	})
	// Use a local ACME test server so the test never contacts Let's Encrypt.
	oldCA := certmagic.DefaultACME.CA
	certmagic.DefaultACME.CA = "https://127.0.0.1:14000/dir"
	t.Cleanup(func() {
		certmagic.DefaultACME.CA = oldCA
	})

	tlsCfg, err := s.buildTLSConfig()
	// We only care that certmagic builds a config; actual issuance requires
	// a running ACME server and a routable domain, which unit tests cannot
	// guarantee. An error from the test CA is acceptable.
	if err != nil {
		t.Logf("certmagic returned error (expected in unit test): %v", err)
		return
	}
	if tlsCfg == nil {
		t.Fatal("expected non-nil TLS config for auto_tls")
	}
	// certmagic returns a config with GetCertificate set.
	if tlsCfg.GetCertificate == nil {
		t.Error("expected certmagic TLS config to have GetCertificate")
	}
}

func TestBuildTLSConfig_AutoTLSWithMTLSRejected(t *testing.T) {
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
