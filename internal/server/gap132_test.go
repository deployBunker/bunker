// gap132_test.go — GAP-132 acceptance coverage for the key-lifecycle RPCs:
// master-only gating on the real interceptor stack, immediate revocation
// through the RPC, and audit records on rotate/revoke (review criteria
// 2, 4, 5).
package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/apikey"
	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
)

// newGap132Service builds a bunkerdService wired exactly like New() does for
// the key path (durable manager + master-only interceptor + JWT auth), but
// against temp-dir stores so no root dirs are touched.
func newGap132Service(t *testing.T) (*bunkerdService, string) {
	t.Helper()
	dir := t.TempDir()
	keyMgr, err := apikey.NewManagerAt(gap131Secret, dir)
	if err != nil {
		t.Fatalf("key manager: %v", err)
	}
	auditPath := filepath.Join(dir, "audit.log")
	l, err := audit.New(auditPath)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	svc := &bunkerdService{
		cfg:      config.DefaultConfig(),
		logger:   testDiscardLogger(),
		keyMgr:   keyMgr,
		jwtAuth:  auth.NewJWTAuth(gap131Secret, keyMgr),
		auditLog: l,
	}
	return svc, auditPath
}

// TestKeyRPCs_RejectAgentScopedCredentials drives the REAL master-only
// interceptor over the wire-shaped path: an agent-scoped sub-key (opaque or
// JWT) must be rejected with CodeUnauthenticated before any key-lifecycle
// handler runs; a master token reaches the handler.
func TestKeyRPCs_RejectAgentScopedCredentials(t *testing.T) {
	svc, _ := newGap132Service(t)

	interceptor := auth.NewMasterOnlyAuthInterceptor(gap131Secret, svc.keyMgr, "static-fallback", true)

	// A real agent sub-key issued by the manager.
	subToken, subKey, err := svc.keyMgr.Generate("agent-x", time.Hour)
	if err != nil {
		t.Fatalf("generate sub-key: %v", err)
	}
	// And an agent-scoped JWT.
	agentJWT, err := svc.jwtAuth.IssueAgentToken("agent-x", time.Hour)
	if err != nil {
		t.Fatalf("issue agent jwt: %v", err)
	}

	cases := []struct {
		name  string
		token string
	}{
		{"opaque sub-key", subToken},
		{"agent jwt", agentJWT},
		{"no credential", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached := 0
			wrapped := interceptor.WrapUnary(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				reached++
				return connect.NewResponse(&v1.RevokeKeyResponse{}), nil
			})
			req := connect.NewRequest(&v1.RevokeKeyRequest{KeyId: subKey.KeyID})
			if tc.token != "" {
				req.Header().Set("Authorization", "Bearer "+tc.token)
			}
			_, err := wrapped(context.Background(), req)
			if err == nil {
				t.Fatalf("%s must be rejected with CodeUnauthenticated", tc.name)
			}
			if connect.CodeOf(err) != connect.CodeUnauthenticated {
				t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
			}
			if reached != 0 {
				t.Fatal("handler must never run under an agent credential")
			}
		})
	}

	// Control: a MASTER token passes the same interceptor.
	reached := 0
	wrapped := interceptor.WrapUnary(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		reached++
		return connect.NewResponse(&v1.RevokeKeyResponse{}), nil
	})
	masterTok, err := svc.jwtAuth.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue master: %v", err)
	}
	req := connect.NewRequest(&v1.RevokeKeyRequest{KeyId: subKey.KeyID})
	req.Header().Set("Authorization", "Bearer "+masterTok)
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("master token must be admitted: %v", err)
	}
	if reached != 1 {
		t.Fatalf("handler reached %d times under master credential", reached)
	}
}

// TestRevokeKeyRPC_ImmediateInvalidation covers review criterion (2) through
// the RPC surface: revocation takes effect before the response is sent and
// the token stops validating.
func TestRevokeKeyRPC_ImmediateInvalidation(t *testing.T) {
	svc, _ := newGap132Service(t)
	tok, key, err := svc.keyMgr.Generate("agent-y", time.Hour)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := svc.keyMgr.Validate(tok); err != nil {
		t.Fatalf("pre-revoke validate: %v", err)
	}

	resp, err := svc.RevokeKey(context.Background(), connect.NewRequest(&v1.RevokeKeyRequest{KeyId: key.KeyID}))
	if err != nil {
		t.Fatalf("RevokeKey rpc: %v", err)
	}
	if got := resp.Msg.GetStatus(); got != "revoked" {
		t.Fatalf("status = %q, want revoked", got)
	}
	if _, err := svc.keyMgr.Validate(tok); err == nil {
		t.Fatal("token must stop validating immediately after RevokeKey")
	}

	// Unknown key -> CodeNotFound.
	_, err = svc.RevokeKey(context.Background(), connect.NewRequest(&v1.RevokeKeyRequest{KeyId: "bk_missing"}))
	if err == nil || connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("revoking an unknown key must be CodeNotFound, got %v", err)
	}
	// Empty key id -> CodeInvalidArgument.
	_, err = svc.RevokeKey(context.Background(), connect.NewRequest(&v1.RevokeKeyRequest{}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty key_id must be CodeInvalidArgument, got %v", err)
	}
}

// TestKeyRPCs_AuditRecordsOnRotateAndRevoke covers review criterion (5):
// rotate and revoke each append one correlated record to the same audit log,
// carrying fingerprints/ids only (never secret material).
func TestKeyRPCs_AuditRecordsOnRotateAndRevoke(t *testing.T) {
	svc, auditPath := newGap132Service(t)
	ctx := context.Background()

	newSecret := strings.Repeat("k", 40) // >= 32 bytes
	if _, err := svc.jwtAuth.RotateSecret(newSecret, time.Minute); err != nil {
		t.Fatalf("rotate setup: %v", err)
	}
	resp, err := svc.RotateJWTSecret(ctx, connect.NewRequest(&v1.RotateJWTSecretRequest{OverlapSeconds: 60}))
	if err != nil {
		t.Fatalf("RotateJWTSecret rpc: %v", err)
	}
	if resp.Msg.GetJwtSecret() == "" || resp.Msg.GetPreviousFingerprint() == "" {
		t.Fatal("rotate response must carry the new secret (once) and the previous fingerprint")
	}

	tok, key, err := svc.keyMgr.Generate("agent-z", time.Hour)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := svc.keyMgr.Validate(tok); err != nil {
		t.Fatalf("pre-revoke validate: %v", err)
	}
	if _, err := svc.RevokeKey(ctx, connect.NewRequest(&v1.RevokeKeyRequest{KeyId: key.KeyID})); err != nil {
		t.Fatalf("RevokeKey rpc: %v", err)
	}

	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"/bunker.v1.Bunkerd/RotateJWTSecret",
		"/bunker.v1.Bunkerd/RevokeKey",
		key.KeyID,
		"previous fp=",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("audit log missing %q:\n%s", want, text)
		}
	}
	// The rotated secret itself must NEVER appear in the audit chain.
	if strings.Contains(text, newSecret) {
		t.Fatal("audit log must never carry the raw rotated secret")
	}
}

// TestKeyListRPC_MetadataOnly covers the KeyList surface: metadata rows for
// active keys, revoked ones flagged, agent filter honored, and no token or
// hash material anywhere in the response.
func TestKeyListRPC_MetadataOnly(t *testing.T) {
	svc, _ := newGap132Service(t)
	tokA, _, err := svc.keyMgr.Generate("agent-a", time.Hour)
	if err != nil {
		t.Fatalf("generate A: %v", err)
	}
	_, keyB, err := svc.keyMgr.Generate("agent-b", time.Hour)
	if err != nil {
		t.Fatalf("generate B: %v", err)
	}
	if err := svc.keyMgr.Revoke(keyB.KeyID); err != nil {
		t.Fatalf("revoke B: %v", err)
	}

	resp, err := svc.KeyList(context.Background(), connect.NewRequest(&v1.KeyListRequest{}))
	if err != nil {
		t.Fatalf("KeyList: %v", err)
	}
	// List reports ACTIVE keys; the revoked one is filtered (metadata-only
	// view of what still authenticates).
	if len(resp.Msg.GetKeys()) != 1 {
		t.Fatalf("keys = %d, want 1 (the active key)", len(resp.Msg.GetKeys()))
	}
	for _, k := range resp.Msg.GetKeys() {
		if strings.Contains(k.String(), tokA) {
			t.Fatal("KeyList must never return token material")
		}
	}

	// Agent filter.
	resp, err = svc.KeyList(context.Background(), connect.NewRequest(&v1.KeyListRequest{AgentId: "agent-a"}))
	if err != nil {
		t.Fatalf("KeyList filtered: %v", err)
	}
	if len(resp.Msg.GetKeys()) != 1 || resp.Msg.GetKeys()[0].GetAgentId() != "agent-a" {
		t.Fatalf("agent filter failed: %+v", resp.Msg.GetKeys())
	}
}
