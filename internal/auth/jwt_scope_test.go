package auth

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/apikey"
)

func TestMasterOnlyJWTAuth_RejectsAgentScopedJWT(t *testing.T) {
	// Create a master-only auth (should reject agent-scoped tokens)
	keyMgr := apikey.NewManager("master-secret")
	masterOnly := NewMasterOnlyJWTAuth("super-secret-32-bytes-long-key", keyMgr)
	regular := NewJWTAuth("super-secret-32-bytes-long-key", keyMgr)

	// Issue an agent-scoped JWT via the regular auth
	agentToken, err := regular.IssueAgentToken("agent-42", time.Hour)
	if err != nil {
		t.Fatalf("issue agent token: %v", err)
	}

	// Master-only should reject it
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := masterOnly.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+agentToken)
	_, err = wrapped(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for agent-scoped JWT on master-only auth")
	}
	if connectErr, ok := err.(*connect.Error); !ok || connectErr.Code() != connect.CodeUnauthenticated {
		t.Fatalf("expected Unauthenticated connect.Error, got %v", err)
	}
}

func TestMasterOnlyJWTAuth_AcceptsMasterJWT(t *testing.T) {
	keyMgr := apikey.NewManager("master-secret")
	masterOnly := NewMasterOnlyJWTAuth("super-secret-32-bytes-long-key", keyMgr)

	// Issue a master JWT (no agent scope)
	masterToken, err := masterOnly.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue master token: %v", err)
	}

	var gotClaims *Claims
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		gotClaims, _ = ClaimsFromContext(ctx)
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := masterOnly.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+masterToken)
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

func TestMasterOnlyJWTAuth_RejectsAgentOpaqueSubKey(t *testing.T) {
	keyMgr := apikey.NewManager("master-secret")
	masterOnly := NewMasterOnlyJWTAuth("super-secret-32-bytes-long-key", keyMgr)

	// Generate an agent-scoped opaque sub-key
	opaqueToken, _, err := keyMgr.Generate("agent-42", time.Hour)
	if err != nil {
		t.Fatalf("generate opaque key: %v", err)
	}

	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := masterOnly.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+opaqueToken)
	_, err = wrapped(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for agent-scoped opaque key on master-only auth")
	}
	if connectErr, ok := err.(*connect.Error); !ok || connectErr.Code() != connect.CodeUnauthenticated {
		t.Fatalf("expected Unauthenticated connect.Error, got %v", err)
	}
}

func TestMasterOnlyJWTAuth_AcceptsMasterOpaqueKey(t *testing.T) {
	keyMgr := apikey.NewManager("master-secret")
	masterOnly := NewMasterOnlyJWTAuth("super-secret-32-bytes-long-key", keyMgr)

	// Generate a top-level (master) opaque key with empty agent_id
	masterOpaqueToken, _, err := keyMgr.Generate("", time.Hour)
	if err != nil {
		t.Fatalf("generate master opaque key: %v", err)
	}

	var gotClaims *Claims
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		gotClaims, _ = ClaimsFromContext(ctx)
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := masterOnly.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+masterOpaqueToken)
	_, err = wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotClaims == nil {
		t.Fatal("expected claims in context")
	}
}

func TestMasterOnlyJWTAuth_AcceptsStaticTokenFallback(t *testing.T) {
	keyMgr := apikey.NewManager("master-secret")
	masterOnly := NewMasterOnlyJWTAuthWithStaticFallback("super-secret-32-bytes-long-key", "static-test-token", keyMgr)

	var gotClaims *Claims
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		gotClaims, _ = ClaimsFromContext(ctx)
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := masterOnly.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer static-test-token")
	_, err := wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error for static fallback: %v", err)
	}
	if gotClaims == nil {
		t.Fatal("expected claims in context")
	}
}

func TestRegularJWTAuth_AcceptsAgentScopedOpaqueKey(t *testing.T) {
	// The regular (non-master-only) JWT auth SHOULD accept agent-scoped keys.
	// This is the Agent service interceptor behavior.
	keyMgr := apikey.NewManager("master-secret")
	regular := NewJWTAuth("super-secret-32-bytes-long-key", keyMgr)

	opaqueToken, _, err := keyMgr.Generate("agent-99", time.Hour)
	if err != nil {
		t.Fatalf("generate opaque key: %v", err)
	}

	var gotClaims *Claims
	handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		gotClaims, _ = ClaimsFromContext(ctx)
		return connect.NewResponse(&dummyMsg{}), nil
	}
	wrapped := regular.WrapUnary(handler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+opaqueToken)
	_, err = wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotClaims == nil || gotClaims.AgentID != "agent-99" {
		t.Fatalf("expected agent_id agent-99 from opaque key on regular auth, got %+v", gotClaims)
	}
}

func TestMasterOnlyAuthInterceptor(t *testing.T) {
	keyMgr := apikey.NewManager("master-secret")
	masterInterceptor := NewMasterOnlyAuthInterceptor("super-secret-32-bytes-long-key", keyMgr, "", true)
	regularInterceptor := NewJWTAuthInterceptor("super-secret-32-bytes-long-key", keyMgr, "", true)

	// Issue an agent-scoped JWT
	regular := NewJWTAuth("super-secret-32-bytes-long-key", keyMgr)
	agentToken, err := regular.IssueAgentToken("agent-42", time.Hour)
	if err != nil {
		t.Fatalf("issue agent token: %v", err)
	}

	// Master-only should reject agent-scoped JWT
	masterCalled := false
	masterHandler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		masterCalled = true
		return connect.NewResponse(&dummyMsg{}), nil
	}
	masterWrapped := masterInterceptor.WrapUnary(masterHandler)

	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+agentToken)
	_, err = masterWrapped(context.Background(), req)
	if err == nil {
		t.Fatal("master-only interceptor should reject agent-scoped JWT")
	}
	if masterCalled {
		t.Error("handler should not be called on auth failure")
	}

	// Regular interceptor should accept agent-scoped JWT
	regularCalled := false
	regularHandler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		regularCalled = true
		return connect.NewResponse(&dummyMsg{}), nil
	}
	regularWrapped := regularInterceptor.WrapUnary(regularHandler)

	req2 := connect.NewRequest(&dummyMsg{})
	req2.Header().Set("Authorization", "Bearer "+agentToken)
	_, err = regularWrapped(context.Background(), req2)
	if err != nil {
		t.Fatalf("regular interceptor should accept agent-scoped JWT, got: %v", err)
	}
	if !regularCalled {
		t.Error("handler should be called on auth success")
	}
}
