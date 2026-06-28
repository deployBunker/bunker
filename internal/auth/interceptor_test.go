package auth

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestTokenAuth_MissingHeader(t *testing.T) {
	auth := NewTokenAuth("secret-token")

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Del("Authorization") // ensure no auth header
	_, err := wrapped(context.Background(), req)
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

func TestTokenAuth_InvalidFormat(t *testing.T) {
	auth := NewTokenAuth("secret-token")

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "secret-token") // no Bearer prefix
	_, err := wrapped(context.Background(), req)
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

func TestTokenAuth_WrongToken(t *testing.T) {
	auth := NewTokenAuth("correct-token")

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer wrong-token")
	_, err := wrapped(context.Background(), req)
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

func TestTokenAuth_CorrectToken(t *testing.T) {
	auth := NewTokenAuth("secret-token")

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer secret-token")
	_, err := wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("handler was not called")
	}
}

func TestTokenAuth_CaseInsensitiveBearer(t *testing.T) {
	auth := NewTokenAuth("secret-token")

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "bearer secret-token")
	_, err := wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("handler was not called")
	}
}

func TestNoAuth_PassesThrough(t *testing.T) {
	auth := NoAuth{}

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer wrong-token") // should be ignored
	_, err := wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("handler was not called")
	}
}

func TestNewAuthInterceptor_Disabled(t *testing.T) {
	interceptor := NewAuthInterceptor("", false)
	_, ok := interceptor.(NoAuth)
	if !ok {
		t.Errorf("expected NoAuth when disabled, got %T", interceptor)
	}
}

func TestNewAuthInterceptor_EmptyToken(t *testing.T) {
	interceptor := NewAuthInterceptor("", true)
	_, ok := interceptor.(NoAuth)
	if !ok {
		t.Errorf("expected NoAuth when token empty, got %T", interceptor)
	}
}

func TestNewAuthInterceptor_Enabled(t *testing.T) {
	interceptor := NewAuthInterceptor("secret", true)
	_, ok := interceptor.(*TokenAuth)
	if !ok {
		t.Errorf("expected *TokenAuth when enabled, got %T", interceptor)
	}
}

func TestExtractBearerToken_Success(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer my-token")

	token, err := ExtractBearerToken(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "my-token" {
		t.Errorf("expected 'my-token', got %q", token)
	}
}

func TestExtractBearerToken_Missing(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	_, err := ExtractBearerToken(r)
	if err == nil {
		t.Fatal("expected error for missing header")
	}
}

func TestExtractBearerToken_InvalidFormat(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	_, err := ExtractBearerToken(r)
	if err == nil {
		t.Fatal("expected error for invalid format")
	}
}

// dummyMsg is a minimal proto.Message for testing.
// connect.NewRequest requires a proto.Message.
type dummyMsg struct{}

func (d *dummyMsg) ProtoReflect() protoreflect.Message { return nil }
func (d *dummyMsg) Reset()                              {}
func (d *dummyMsg) String() string                      { return "dummy" }
