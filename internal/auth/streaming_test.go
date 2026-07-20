package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
)

// mockStreamingConn implements connect.StreamingHandlerConn for testing
// streaming interceptors.
type mockStreamingConn struct {
	header http.Header
}

func (m *mockStreamingConn) Spec() connect.Spec {
	return connect.Spec{Procedure: "/test.Service/Stream", IsClient: false}
}

func (m *mockStreamingConn) Peer() connect.Peer {
	return connect.Peer{Addr: "127.0.0.1:1234", Protocol: connect.ProtocolConnect}
}

func (m *mockStreamingConn) RequestHeader() http.Header {
	if m.header == nil {
		m.header = http.Header{}
	}
	return m.header
}

func (m *mockStreamingConn) Receive(any) error { return nil }
func (m *mockStreamingConn) Send(any) error    { return nil }

func (m *mockStreamingConn) ResponseHeader() http.Header  { return http.Header{} }
func (m *mockStreamingConn) ResponseTrailer() http.Header { return http.Header{} }

func TestTokenAuth_WrapStreamingHandler_MissingHeader(t *testing.T) {
	auth := NewTokenAuth("secret-token")

	called := false
	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		called = true
		return nil
	}
	wrapped := auth.WrapStreamingHandler(next)

	conn := &mockStreamingConn{header: http.Header{}}
	err := wrapped(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for missing header")
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

func TestTokenAuth_WrapStreamingHandler_InvalidFormat(t *testing.T) {
	auth := NewTokenAuth("secret-token")

	called := false
	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		called = true
		return nil
	}
	wrapped := auth.WrapStreamingHandler(next)

	header := http.Header{}
	header.Set("Authorization", "secret-token") // no Bearer prefix
	conn := &mockStreamingConn{header: header}
	err := wrapped(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for invalid format")
	}
	if called {
		t.Error("handler should not be called on auth failure")
	}
	connectErr := err.(*connect.Error)
	if connectErr.Code() != connect.CodeUnauthenticated {
		t.Errorf("expected Unauthenticated, got %v", connectErr.Code())
	}
}

func TestTokenAuth_WrapStreamingHandler_WrongToken(t *testing.T) {
	auth := NewTokenAuth("correct-token")

	called := false
	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		called = true
		return nil
	}
	wrapped := auth.WrapStreamingHandler(next)

	header := http.Header{}
	header.Set("Authorization", "Bearer wrong-token")
	conn := &mockStreamingConn{header: header}
	err := wrapped(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for wrong token")
	}
	if called {
		t.Error("handler should not be called on auth failure")
	}
	connectErr := err.(*connect.Error)
	if connectErr.Code() != connect.CodeUnauthenticated {
		t.Errorf("expected Unauthenticated, got %v", connectErr.Code())
	}
}

func TestTokenAuth_WrapStreamingHandler_ValidToken(t *testing.T) {
	auth := NewTokenAuth("secret-token")

	called := false
	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		called = true
		return nil
	}
	wrapped := auth.WrapStreamingHandler(next)

	header := http.Header{}
	header.Set("Authorization", "Bearer secret-token")
	conn := &mockStreamingConn{header: header}
	err := wrapped(context.Background(), conn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("handler was not called")
	}
}

func TestTokenAuth_WrapStreamingClient(t *testing.T) {
	auth := NewTokenAuth("secret-token")

	next := connect.StreamingClientFunc(func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		return nil
	})
	wrapped := auth.WrapStreamingClient(next)
	if wrapped == nil {
		t.Fatal("expected non-nil client func")
	}
	// WrapStreamingClient must be a no-op returning next unchanged.
	if &wrapped != &next {
		// Compare the underlying function pointers.
		// Note: Go does not allow direct func comparison, so we call both
		// with identical inputs and compare outputs.
		got := wrapped(context.Background(), connect.Spec{})
		want := next(context.Background(), connect.Spec{})
		if got != want {
			t.Error("WrapStreamingClient should be a no-op returning next unchanged")
		}
	}
}

func TestNoAuth_WrapStreamingHandler(t *testing.T) {
	auth := NoAuth{}

	called := false
	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		called = true
		return nil
	}
	wrapped := auth.WrapStreamingHandler(next)

	header := http.Header{}
	header.Set("Authorization", "Bearer wrong-token") // should be ignored
	conn := &mockStreamingConn{header: header}
	err := wrapped(context.Background(), conn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("handler was not called")
	}
}

func TestNoAuth_WrapStreamingClient(t *testing.T) {
	auth := NoAuth{}

	next := connect.StreamingClientFunc(func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		return nil
	})
	wrapped := auth.WrapStreamingClient(next)
	got := wrapped(context.Background(), connect.Spec{})
	want := next(context.Background(), connect.Spec{})
	if got != want {
		t.Error("NoAuth WrapStreamingClient should be a no-op returning next unchanged")
	}
}

func TestJWTAuth_WrapStreamingHandler_MissingHeader(t *testing.T) {
	auth := NewJWTAuth("super-secret-32-bytes-long-key", nil)

	called := false
	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		called = true
		return nil
	}
	wrapped := auth.WrapStreamingHandler(next)

	conn := &mockStreamingConn{header: http.Header{}}
	err := wrapped(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for missing header")
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

func TestJWTAuth_WrapStreamingHandler_InvalidFormat(t *testing.T) {
	auth := NewJWTAuth("super-secret-32-bytes-long-key", nil)

	called := false
	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		called = true
		return nil
	}
	wrapped := auth.WrapStreamingHandler(next)

	header := http.Header{}
	header.Set("Authorization", "super-secret-32-bytes-long-key") // no Bearer prefix
	conn := &mockStreamingConn{header: header}
	err := wrapped(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for invalid format")
	}
	if called {
		t.Error("handler should not be called on auth failure")
	}
}

func TestJWTAuth_WrapStreamingHandler_ExpiredToken(t *testing.T) {
	auth := NewJWTAuth("super-secret-32-bytes-long-key", nil)
	token, err := auth.IssueMasterToken(-time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	called := false
	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		called = true
		return nil
	}
	wrapped := auth.WrapStreamingHandler(next)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn := &mockStreamingConn{header: header}
	err = wrapped(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	if called {
		t.Error("handler should not be called on expired token")
	}
}

func TestJWTAuth_WrapStreamingHandler_ValidToken(t *testing.T) {
	auth := NewJWTAuth("super-secret-32-bytes-long-key", nil)
	token, err := auth.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	var gotClaims *Claims
	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		gotClaims, _ = ClaimsFromContext(ctx)
		return nil
	}
	wrapped := auth.WrapStreamingHandler(next)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn := &mockStreamingConn{header: header}
	err = wrapped(context.Background(), conn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotClaims == nil {
		t.Fatal("expected claims in context")
	}
	if gotClaims.AgentID != "" {
		t.Errorf("master token should not have agent_id, got %q", gotClaims.AgentID)
	}
}

func TestJWTAuth_WrapStreamingClient(t *testing.T) {
	auth := NewJWTAuth("super-secret-32-bytes-long-key", nil)

	next := connect.StreamingClientFunc(func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		return nil
	})
	wrapped := auth.WrapStreamingClient(next)
	got := wrapped(context.Background(), connect.Spec{})
	want := next(context.Background(), connect.Spec{})
	if got != want {
		t.Error("JWTAuth WrapStreamingClient should be a no-op returning next unchanged")
	}
}

func TestMTLSAuth_WrapStreamingHandler_NoTLSState(t *testing.T) {
	tmp := t.TempDir()
	caFile := tmp + "/ca.pem"
	if err := generateCA(caFile, "Test CA"); err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	auth, err := NewMTLSAuth(caFile, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	called := false
	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		called = true
		return nil
	}
	wrapped := auth.WrapStreamingHandler(next)

	conn := &mockStreamingConn{header: http.Header{}}
	err = wrapped(context.Background(), conn)
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

func TestMTLSAuth_WrapStreamingClient(t *testing.T) {
	auth := &MTLSAuth{} // no CA file needed for client-side no-op

	next := connect.StreamingClientFunc(func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		return nil
	})
	wrapped := auth.WrapStreamingClient(next)
	got := wrapped(context.Background(), connect.Spec{})
	want := next(context.Background(), connect.Spec{})
	if got != want {
		t.Error("MTLSAuth WrapStreamingClient should be a no-op returning next unchanged")
	}
}
