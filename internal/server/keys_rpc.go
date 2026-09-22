// keys_rpc.go — GAP-132 key-lifecycle RPCs on the Bunkerd service.
//
// All three handlers live on the Bunkerd service and are therefore
// authorized by the SAME master-only interceptor as every other
// admin-capable RPC: an agent-scoped sub-key or agent JWT is rejected
// upstream with CodeUnauthenticated before the handler runs (tested in
// gap132_test.go against the real interceptor stack).
//
// Audit: rotate and revoke append one correlated record each to the SAME
// AuditLog the audit interceptor uses (the GAP-142 RecordExecCommand
// pattern), so the lifecycle event is hash-chained with the RPC record that
// caused it. Summaries carry fingerprints and key IDs only — never secret
// or token material.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/auth"
)

// generatedSecretBytes is the byte length of a rotated jwt_secret (hex
// encoded). Matches the GAP-129 generation length so a rotated secret and a
// first-boot generated secret are indistinguishable in shape.
const generatedSecretBytes = 32

// generateRotateSecret mints a new HS256 secret: crypto-random bytes, hex
// encoded (same shape as config.generateJWTSecret — it round-trips through
// the 0600 secret file without quoting surprises).
func generateRotateSecret() (string, error) {
	b := make([]byte, generatedSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate jwt secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// RotateJWTSecret rotates the HS256 signing secret with a bounded
// dual-accept window (GAP-132). The new secret is authoritative immediately
// and is returned EXACTLY ONCE in this response; the retired secret keeps
// validating for overlap_seconds and is rejected afterwards.
func (s *bunkerdService) RotateJWTSecret(ctx context.Context, req *connect.Request[v1.RotateJWTSecretRequest]) (*connect.Response[v1.RotateJWTSecretResponse], error) {
	if s.jwtAuth == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("JWT auth is not configured on this server"))
	}
	newSecret, err := generateRotateSecret()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	prevFp, err := s.jwtAuth.RotateSecret(newSecret, time.Duration(req.Msg.GetOverlapSeconds())*time.Second)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	effective := auth.DefaultRotateOverlap
	if req.Msg.GetOverlapSeconds() > 0 {
		effective = time.Duration(req.Msg.GetOverlapSeconds()) * time.Second
		if effective > auth.MaxRotateOverlap {
			effective = auth.MaxRotateOverlap
		}
	}
	now := time.Now().UTC()
	s.recordKeyLifecycle(ctx, "/bunker.v1.Bunkerd/RotateJWTSecret", fmt.Sprintf("jwt_secret rotated; overlap=%s; previous fp=%s", effective, prevFp))
	return connect.NewResponse(&v1.RotateJWTSecretResponse{
		JwtSecret:           newSecret,
		RotatedAt:           now.Format(time.RFC3339),
		OverlapSeconds:      uint32(effective / time.Second),
		PreviousFingerprint: prevFp,
	}), nil
}

// RevokeKey revokes an API sub-key by key ID. Revocation is immediate: the
// credential stops validating before this response is sent, and the
// revocation marker is persisted so it survives a daemon restart.
func (s *bunkerdService) RevokeKey(ctx context.Context, req *connect.Request[v1.RevokeKeyRequest]) (*connect.Response[v1.RevokeKeyResponse], error) {
	keyID := req.Msg.GetKeyId()
	if keyID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("key_id is required"))
	}
	if err := s.keyMgr.Revoke(keyID); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("key %q not found", keyID))
	}
	s.recordKeyLifecycle(ctx, "/bunker.v1.Bunkerd/RevokeKey", fmt.Sprintf("key %s revoked", keyID))
	return connect.NewResponse(&v1.RevokeKeyResponse{
		KeyId:  keyID,
		Status: "revoked",
	}), nil
}

// KeyList reports active API sub-keys (metadata only — never a token,
// hash, or any other secret material), optionally scoped to one agent.
func (s *bunkerdService) KeyList(ctx context.Context, req *connect.Request[v1.KeyListRequest]) (*connect.Response[v1.KeyListResponse], error) {
	keys := s.keyMgr.List(req.Msg.GetAgentId())
	out := make([]*v1.KeyInfo, 0, len(keys))
	for _, k := range keys {
		out = append(out, &v1.KeyInfo{
			KeyId:     k.KeyID,
			AgentId:   k.AgentID,
			CreatedAt: k.CreatedAt.UTC().Format(time.RFC3339),
			ExpiresAt: k.ExpiresAt.UTC().Format(time.RFC3339),
			Revoked:   k.Revoked,
		})
	}
	return connect.NewResponse(&v1.KeyListResponse{Keys: out}), nil
}

// recordKeyLifecycle appends ONE correlated lifecycle record (rotate or
// revoke) to the daemon's audit chain. Mirrors audit.RecordExecCommand's
// posture: same AuditLog (same hash chain), caller read from the request
// context so it cannot drift from the interceptor's own record, write
// failures logged and swallowed. A nil log (auditing disabled) is a no-op.
func (s *bunkerdService) recordKeyLifecycle(ctx context.Context, method, summary string) {
	if s.auditLog == nil {
		return
	}
	claims, _ := auth.ClaimsFromContext(ctx)
	rec := audit.Record{
		TS:      time.Now().UTC().Format(time.RFC3339Nano),
		Caller:  audit.CallerFromClaims(claims),
		Method:  method,
		Outcome: "ok",
		Summary: summary,
	}
	if err := s.auditLog.Log(rec); err != nil {
		s.logger.Warn("key lifecycle audit write failed", "method", method)
	}
}
