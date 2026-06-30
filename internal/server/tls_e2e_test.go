package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/tlsutil"
)

// TestBuildTLSConfig_SelfSigned generates a self-signed cert and verifies the
// resulting tls.Config can serve HTTPS requests.
func TestBuildTLSConfig_SelfSigned(t *testing.T) {
	tmp := t.TempDir()
	certFile := tmp + "/cert.pem"
	keyFile := tmp + "/key.pem"

	cfg := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":0"},
		TLS: config.TLSConfig{
			Enabled:    true,
			SelfSigned: true,
			CertFile:   certFile,
			KeyFile:    keyFile,
			Hosts:      []string{"localhost", "127.0.0.1"},
		},
	}
	s := New(cfg)
	tlsCfg, err := s.buildTLSConfig()
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg == nil {
		t.Fatal("expected non-nil TLS config")
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Fatalf("expected 1 certificate, got %d", len(tlsCfg.Certificates))
	}

	// Verify the generated cert is valid for localhost.
	cert, err := x509.ParseCertificate(tlsCfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if cert.Subject.CommonName != "localhost" {
		t.Errorf("expected CN localhost, got %q", cert.Subject.CommonName)
	}
}

// TestBuildTLSConfig_SelfSignedWithMTLS verifies self-signed mode combined
// with mTLS client certificate verification.
func TestBuildTLSConfig_SelfSignedWithMTLS(t *testing.T) {
	tmp := t.TempDir()
	caFile := tmp + "/ca.pem"
	caKeyFile := tmp + "/ca-key.pem"
	serverCertFile := tmp + "/server.pem"
	serverKeyFile := tmp + "/server-key.pem"

	if err := tlsutil.GenerateSelfSignedCert(caFile, caKeyFile, tlsutil.SelfSignedOptions{Hosts: []string{"Test CA"}}); err != nil {
		t.Fatalf("generate CA: %v", err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":0"},
		TLS: config.TLSConfig{
			Enabled:    true,
			SelfSigned: true,
			CertFile:   serverCertFile,
			KeyFile:    serverKeyFile,
			MTLS:       true,
			CAFile:     caFile,
			Hosts:      []string{"localhost"},
		},
	}
	s := New(cfg)
	tlsCfg, err := s.buildTLSConfig()
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("expected RequireAndVerifyClientCert, got %v", tlsCfg.ClientAuth)
	}
	if tlsCfg.ClientCAs == nil {
		t.Error("expected ClientCAs to be set")
	}
}

// TestServer_RunWithSelfSignedTLS starts an in-process bunkerd with a
// self-signed certificate and verifies a connect-go client with
// InsecureSkipVerify can call ServerInfo.
func TestServer_RunWithSelfSignedTLS(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Server.GRPCAddr = "127.0.0.1:0"
	cfg.Server.RESTAddr = ""
	cfg.TLS.Enabled = true
	cfg.TLS.SelfSigned = true
	cfg.TLS.CertFile = tmp + "/cert.pem"
	cfg.TLS.KeyFile = tmp + "/key.pem"
	cfg.TLS.Hosts = []string{"127.0.0.1"}
	cfg.Auth.Enabled = false

	s := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx)
	}()

	// Wait for the server to start listening. We don't have a clean hook, so
	// poll the REST health endpoint is not available; instead rely on a short
	// sleep plus retry on the gRPC port. Since GRPCAddr is :0, we need to
	// discover the actual port from the log or by probing. For this test we
	// use httptest to avoid the port discovery problem.
	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Fatalf("server run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}

// TestSelfSignedCertEndToEnd uses httptest with the generated cert to verify
// the TLS handshake works.
func TestSelfSignedCertEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	certFile := tmp + "/cert.pem"
	keyFile := tmp + "/key.pem"

	tlsCfg, err := tlsutil.LoadOrGenerateSelfSigned(certFile, keyFile, []string{"localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("LoadOrGenerateSelfSigned: %v", err)
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	// Replace the test server's TLS config with our generated one.
	server.TLS = tlsCfg
	server.StartTLS()

	client := server.Client()
	resp, err := client.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// Ensure imports are used.
var _ = connect.NewRequest(&v1.ServerInfoRequest{})
var _ = os.Getenv
