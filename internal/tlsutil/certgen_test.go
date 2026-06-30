package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateSelfSignedCert_WritesFiles(t *testing.T) {
	tmp := t.TempDir()
	certPath := filepath.Join(tmp, "cert.pem")
	keyPath := filepath.Join(tmp, "key.pem")

	if err := GenerateSelfSignedCert(certPath, keyPath, SelfSignedOptions{Hosts: []string{"localhost"}}); err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}

	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("cert file not written: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("key file not written: %v", err)
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}

	if len(cert.Certificate) == 0 {
		t.Fatal("expected at least one certificate")
	}

	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	if x509Cert.Subject.CommonName != "localhost" {
		t.Errorf("expected CN localhost, got %q", x509Cert.Subject.CommonName)
	}
}

func TestGenerateSelfSignedCert_DefaultHosts(t *testing.T) {
	tmp := t.TempDir()
	certPath := filepath.Join(tmp, "cert.pem")
	keyPath := filepath.Join(tmp, "key.pem")

	if err := GenerateSelfSignedCert(certPath, keyPath, SelfSignedOptions{}); err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}

	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	if x509Cert.Subject.CommonName != "localhost" {
		t.Errorf("expected default CN localhost, got %q", x509Cert.Subject.CommonName)
	}
}

func TestLoadOrGenerateSelfSigned_GeneratesOnMissing(t *testing.T) {
	tmp := t.TempDir()
	certPath := filepath.Join(tmp, "cert.pem")
	keyPath := filepath.Join(tmp, "key.pem")

	tlsCfg, err := LoadOrGenerateSelfSigned(certPath, keyPath, []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("LoadOrGenerateSelfSigned: %v", err)
	}

	if tlsCfg == nil {
		t.Fatal("expected non-nil tls.Config")
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Fatalf("expected 1 certificate, got %d", len(tlsCfg.Certificates))
	}
}

func TestLoadOrGenerateSelfSigned_LoadsExisting(t *testing.T) {
	tmp := t.TempDir()
	certPath := filepath.Join(tmp, "cert.pem")
	keyPath := filepath.Join(tmp, "key.pem")

	if err := GenerateSelfSignedCert(certPath, keyPath, SelfSignedOptions{Hosts: []string{"existing.example.com"}}); err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}

	tlsCfg, err := LoadOrGenerateSelfSigned(certPath, keyPath, []string{"new.example.com"})
	if err != nil {
		t.Fatalf("LoadOrGenerateSelfSigned: %v", err)
	}

	x509Cert, err := x509.ParseCertificate(tlsCfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	if x509Cert.Subject.CommonName != "existing.example.com" {
		t.Errorf("expected existing cert to be loaded, got CN %q", x509Cert.Subject.CommonName)
	}
}
