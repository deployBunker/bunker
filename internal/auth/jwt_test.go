package auth

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/deployBunker/bunker/internal/apikey"
)

func TestJWTAuth_MissingHeader(t *testing.T) {
	auth := NewJWTAuth("super-secret-32-bytes-long-key", nil)

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Del("Authorization")
	_, err := wrapped(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing header")
	}
	if called {
		t.Error("handler should not be called on auth failure")
	}
	if connectErr, ok := err.(*connect.Error); !ok || connectErr.Code() != connect.CodeUnauthenticated {
		t.Fatalf("expected Unauthenticated connect.Error, got %v", err)
	}
}

func TestJWTAuth_InvalidFormat(t *testing.T) {
	auth := NewJWTAuth("super-secret-32-bytes-long-key", nil)

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "secret-token")
	_, err := wrapped(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for invalid format")
	}
	if called {
		t.Error("handler should not be called on auth failure")
	}
}

func TestJWTAuth_WrongSecret(t *testing.T) {
	auth := NewJWTAuth("super-secret-32-bytes-long-key", nil)

	otherAuth := NewJWTAuth("different-secret-32-bytes-long", nil)
	token, err := otherAuth.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err = wrapped(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
	if called {
		t.Error("handler should not be called on auth failure")
	}
}

func TestJWTAuth_ExpiredToken(t *testing.T) {
	auth := NewJWTAuth("super-secret-32-bytes-long-key", nil)
	token, err := auth.IssueMasterToken(-time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err = wrapped(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	if called {
		t.Error("handler should not be called on expired token")
	}
}

func TestJWTAuth_ValidMasterToken(t *testing.T) {
	auth := NewJWTAuth("super-secret-32-bytes-long-key", nil)
	token, err := auth.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	var gotClaims *Claims
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		gotClaims, _ = ClaimsFromContext(ctx)
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err = wrapped(context.Background(), req)
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

func TestJWTAuth_ValidAgentToken(t *testing.T) {
	auth := NewJWTAuth("super-secret-32-bytes-long-key", nil)
	token, err := auth.IssueAgentToken("agent-42", time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	var gotClaims *Claims
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		gotClaims, _ = ClaimsFromContext(ctx)
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err = wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotClaims == nil || gotClaims.AgentID != "agent-42" {
		t.Fatalf("expected agent_id agent-42, got %+v", gotClaims)
	}
}

func TestJWTAuth_AgentTokenRequiresAgentID(t *testing.T) {
	auth := NewJWTAuth("super-secret-32-bytes-long-key", nil)
	_, err := auth.IssueAgentToken("", time.Hour)
	if err == nil {
		t.Fatal("expected error for empty agent_id")
	}
}

func TestJWTAuth_OpaqueSubKey(t *testing.T) {
	keyMgr := apikey.NewManager("master-secret")
	auth := NewJWTAuth("super-secret-32-bytes-long-key", keyMgr)

	opaqueToken, _, err := keyMgr.Generate("agent-99", time.Hour)
	if err != nil {
		t.Fatalf("generate opaque key: %v", err)
	}

	var gotClaims *Claims
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		gotClaims, _ = ClaimsFromContext(ctx)
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+opaqueToken)
	_, err = wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotClaims == nil || gotClaims.AgentID != "agent-99" {
		t.Fatalf("expected agent_id agent-99 from opaque key, got %+v", gotClaims)
	}
}

func TestJWTAuth_OpaqueSubKeyExpired(t *testing.T) {
	keyMgr := apikey.NewManager("master-secret")
	auth := NewJWTAuth("super-secret-32-bytes-long-key", keyMgr)

	opaqueToken, _, err := keyMgr.Generate("agent-99", -time.Hour)
	if err != nil {
		t.Fatalf("generate opaque key: %v", err)
	}

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+opaqueToken)
	_, err = wrapped(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for expired opaque key")
	}
	if called {
		t.Error("handler should not be called on expired opaque key")
	}
}

func TestJWTAuth_NoSecret(t *testing.T) {
	auth := NewJWTAuth("", nil)
	_, err := auth.IssueMasterToken(time.Hour)
	if err == nil {
		t.Fatal("expected error issuing token without secret")
	}

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer not-a-jwt")
	_, err = wrapped(context.Background(), req)
	if err == nil {
		t.Fatal("expected error without secret")
	}
	if called {
		t.Error("handler should not be called without secret")
	}
}

func TestGenerateSecret(t *testing.T) {
	secret, err := GenerateSecret(32)
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	if len(secret) == 0 {
		t.Fatal("secret should not be empty")
	}

	_, err = GenerateSecret(16)
	if err == nil {
		t.Fatal("expected error for short secret")
	}
}

func TestExtractBearerTokenFromHeader(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"valid", "Bearer abc123", "abc123", false},
		{"missing header", "", "", true},
		{"wrong scheme", "Basic abc123", "", true},
		{"no space", "Bearerabc123", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractBearerTokenFromHeader(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ExtractBearerTokenFromHeader(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ExtractBearerTokenFromHeader(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestJWTAuth_StaticTokenFallback(t *testing.T) {
	keyMgr := apikey.NewManager("master-secret")
	auth := NewJWTAuthWithStaticFallback("super-secret-32-bytes-long-key", "static-test-token", keyMgr)

	var gotClaims *Claims
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		gotClaims, _ = ClaimsFromContext(ctx)
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer static-test-token")
	_, err := wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error for static fallback: %v", err)
	}
	if gotClaims == nil || gotClaims.Subject != "static-token" {
		t.Fatalf("expected static-token subject, got %+v", gotClaims)
	}
}

func TestJWTAuth_StaticTokenFallbackRejectsWrongToken(t *testing.T) {
	keyMgr := apikey.NewManager("master-secret")
	auth := NewJWTAuthWithStaticFallback("super-secret-32-bytes-long-key", "static-test-token", keyMgr)

	called := false
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	}
	wrapped := auth.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer wrong-static-token")
	_, err := wrapped(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for wrong static token")
	}
	if called {
		t.Error("handler should not be called on wrong static token")
	}
}

func TestConstantTimeCompare(t *testing.T) {
	if !ConstantTimeCompare("abc", "abc") {
		t.Error("expected equal strings to compare true")
	}
	if ConstantTimeCompare("abc", "abd") {
		t.Error("expected different strings to compare false")
	}
}
