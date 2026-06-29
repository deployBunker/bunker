package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"

	"connectrpc.com/connect"
)

// MTLSAuth validates incoming requests using client certificate-based mTLS.
// It checks that the client presents a valid certificate signed by a trusted CA.
type MTLSAuth struct {
	clientCAs *x509.CertPool
	verifyCN  string // optional: require specific CommonName
}

// NewMTLSAuth creates an MTLSAuth from a PEM-encoded CA certificate file.
// If verifyCN is non-empty, client certs must have that CommonName.
func NewMTLSAuth(caCertFile string, verifyCN string) (*MTLSAuth, error) {
	if caCertFile == "" {
		return nil, fmt.Errorf("ca_cert_file is required for mTLS")
	}
	pem, err := os.ReadFile(caCertFile)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("failed to parse CA certificate(s) from %s", caCertFile)
	}
	return &MTLSAuth{clientCAs: pool, verifyCN: verifyCN}, nil
}

// WrapUnary validates the client certificate on unary requests.
func (m *MTLSAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := m.authenticate(ctx); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

// WrapStreamingHandler validates the client certificate on streaming requests.
func (m *MTLSAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := m.authenticate(ctx); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// WrapStreamingClient is a no-op — auth is server-side only.
func (m *MTLSAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (m *MTLSAuth) authenticate(ctx context.Context) error {
	// connect-go doesn't expose TLS state directly on the request,
	// so we rely on the HTTP server having already verified the client cert
	// and set the peer certificate info in the context via tls.Config.VerifyPeerCertificate.
	// However, standard net/http sets *tls.ConnectionState in the request context.
	// We use a helper that extracts it from the underlying http.Request in the context.
	state, ok := TLSConnectionStateFromContext(ctx)
	if !ok || state == nil {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("mTLS required: no TLS connection state"))
	}
	if len(state.PeerCertificates) == 0 {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("mTLS required: no client certificate presented"))
	}
	if m.verifyCN != "" {
		cert := state.PeerCertificates[0]
		if cert.Subject.CommonName != m.verifyCN {
			return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("mTLS CN mismatch: got %q, want %q", cert.Subject.CommonName, m.verifyCN))
		}
	}
	return nil
}

// TLSConnectionStateFromContext extracts the tls.ConnectionState from a context
// that was created with an http.Request containing TLS info.
func TLSConnectionStateFromContext(ctx context.Context) (*tls.ConnectionState, bool) {
	// The standard library stores *tls.ConnectionState in the context under
	// the key of the http.Request pointer. We can't access that directly,
	// but connect-go's handler sets the base http.Request in the context
	// under a specific key. We use a type assertion on the request to get TLS state.
	if req, ok := ctx.Value(http.ServerContextKey).(*http.Request); ok {
		if req.TLS != nil {
			return req.TLS, true
		}
	}
	return nil, false
}

// BuildMTLSConfig returns a tls.Config configured for mTLS (client cert verification).
// It uses the given CA cert file to build a client cert pool and sets ClientAuth to RequireAndVerifyClientCert.
func BuildMTLSConfig(caCertFile string) (*tls.Config, error) {
	pem, err := os.ReadFile(caCertFile)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("failed to parse CA certificate(s) from %s", caCertFile)
	}
	return &tls.Config{
		ClientCAs:  pool,
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS12,
	}, nil
}

// BuildMTLSConfigWithCert returns a tls.Config for mTLS that also includes the server's own certificate.
func BuildMTLSConfigWithCert(caCertFile, certFile, keyFile string) (*tls.Config, error) {
	mtlsCfg, err := BuildMTLSConfig(caCertFile)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server cert/key: %w", err)
	}
	mtlsCfg.Certificates = []tls.Certificate{cert}
	return mtlsCfg, nil
}

// NewMTLSInterceptor returns a connect.Interceptor that enforces mTLS.
// Returns an error if the CA cert file is missing or invalid.
func NewMTLSInterceptor(caCertFile string, verifyCN string) (connect.Interceptor, error) {
	return NewMTLSAuth(caCertFile, verifyCN)
}

// Ensure MTLSAuth satisfies the interface.
var _ connect.Interceptor = (*MTLSAuth)(nil)
