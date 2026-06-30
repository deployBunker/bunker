// Package tlsutil provides helpers for generating and managing TLS certificates.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// SelfSignedOptions controls generation of a self-signed certificate.
type SelfSignedOptions struct {
	Hosts []string // DNS names and/or IP addresses to include in the cert
	Years int      // Validity in years (default 1)
}

// GenerateSelfSignedCert creates a new self-signed ECDSA certificate and key.
// It writes PEM-encoded cert and key to certPath and keyPath, creating parent
// directories as needed. The certificate is valid for the given hosts.
func GenerateSelfSignedCert(certPath, keyPath string, opts SelfSignedOptions) error {
	if len(opts.Hosts) == 0 {
		opts.Hosts = []string{"localhost"}
	}
	if opts.Years <= 0 {
		opts.Years = 1
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Bunker"},
			CommonName:   opts.Hosts[0],
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(opts.Years, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	for _, h := range opts.Hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(certPath), 0750); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0750); err != nil {
		return fmt.Errorf("create key dir: %w", err)
	}

	certOut, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("open cert file: %w", err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}

	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open key file: %w", err)
	}
	defer keyOut.Close()
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return fmt.Errorf("write key: %w", err)
	}

	return nil
}

// LoadOrGenerateSelfSigned loads an existing cert/key pair or generates a new
// self-signed pair if either file is missing. It returns a tls.Config ready to
// use with net/http.Server.
func LoadOrGenerateSelfSigned(certPath, keyPath string, hosts []string) (*tls.Config, error) {
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		if err := GenerateSelfSignedCert(certPath, keyPath, SelfSignedOptions{Hosts: hosts}); err != nil {
			return nil, err
		}
	} else if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		if err := GenerateSelfSignedCert(certPath, keyPath, SelfSignedOptions{Hosts: hosts}); err != nil {
			return nil, err
		}
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load generated cert/key: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
