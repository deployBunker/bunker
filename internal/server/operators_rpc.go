// operators_rpc.go — GAP-130 operator-lifecycle RPCs on the Bunkerd service.
//
// All three handlers sit on the Bunkerd service and are therefore behind the
// SAME master-only interceptor as every other admin-capable RPC. On top of
// that, the RBAC interceptor (internal/auth/rbac.go) denies operator-role
// callers with CodePermissionDenied — only the master credential and
// admin-role operators manage operators. Agent-scoped credentials (including
// agent:self operators) are rejected upstream with CodeUnauthenticated.
//
// Audit: the audit interceptor records one RPC entry per call with the
// caller identity; create additionally rides the same correlated-lifecycle
// record shape the GAP-132 key RPCs use (recordKeyLifecycle), so the trail
// shows the event and its actor even when read by procedure.
package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/operator"
)

// OperatorCreate provisions a named operator identity with an explicit RBAC
// role. The bearer token is returned EXACTLY ONCE — only its SHA-256 hash is
// stored (identity.TokenHash never leaves the registry).
func (s *bunkerdService) OperatorCreate(ctx context.Context, req *connect.Request[v1.OperatorCreateRequest]) (*connect.Response[v1.OperatorCreateResponse], error) {
	if s.opMgr == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("operator registry is not configured on this server"))
	}
	name := req.Msg.GetName()
	role, rerr := operator.ParseRole(req.Msg.GetRole())
	if rerr != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, rerr)
	}
	if role == operator.RoleAgentSelf && name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("an agent:self operator needs the agent id as its name"))
	}
	token, ident, err := s.opMgr.Create(name, role, req.Msg.GetTtlSeconds())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	s.recordOperatorLifecycle(ctx, "OperatorCreate",
		fmt.Sprintf("operator %s created role=%s agent_id=%s expires=%s (token shown once, only its sha256 stored)",
			ident.Name, ident.Role, ident.AgentID, ident.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")))
	return connect.NewResponse(&v1.OperatorCreateResponse{
		Name:       ident.Name,
		Role:       string(ident.Role),
		Token:      token,
		ExpiresAt:  ident.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		TtlSeconds: req.Msg.GetTtlSeconds(),
	}), nil
}

// OperatorList reports operator identities (metadata only — never a token,
// hash, or any other secret material), optionally scoped to one agent's
// agent:self identity.
func (s *bunkerdService) OperatorList(ctx context.Context, req *connect.Request[v1.OperatorListRequest]) (*connect.Response[v1.OperatorListResponse], error) {
	if s.opMgr == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("operator registry is not configured on this server"))
	}
	idents := s.opMgr.List(req.Msg.GetAgentId())
	out := make([]*v1.OperatorInfo, 0, len(idents))
	for _, ident := range idents {
		out = append(out, &v1.OperatorInfo{
			Name:      ident.Name,
			Role:      string(ident.Role),
			AgentId:   ident.AgentID,
			CreatedAt: ident.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			ExpiresAt: ident.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	return connect.NewResponse(&v1.OperatorListResponse{Operators: out}), nil
}

// OperatorDelete removes an operator identity by name. Deletion is immediate
// (the token stops validating before this response is sent) and durable.
func (s *bunkerdService) OperatorDelete(ctx context.Context, req *connect.Request[v1.OperatorDeleteRequest]) (*connect.Response[v1.OperatorDeleteResponse], error) {
	if s.opMgr == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("operator registry is not configured on this server"))
	}
	name := req.Msg.GetName()
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name is required"))
	}
	if err := s.opMgr.Delete(name); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("operator %q not found", name))
	}
	s.recordOperatorLifecycle(ctx, "OperatorDelete", fmt.Sprintf("operator %s deleted (its token no longer validates)", name))
	return connect.NewResponse(&v1.OperatorDeleteResponse{Name: name, Status: "deleted"}), nil
}

// recordOperatorLifecycle appends ONE correlated lifecycle record to the
// daemon's audit chain — the recordKeyLifecycle shape (GAP-132) with the
// GAP-130 method, so operator lifecycle events are hash-chained with the
// RPC record that caused them and carry the acting identity. Mirrors
// audit.RecordExecCommand's posture: same AuditLog (same hash chain), caller
// read from the request context, write failures logged and swallowed. A nil
// log (auditing disabled) is a no-op.
func (s *bunkerdService) recordOperatorLifecycle(ctx context.Context, method, summary string) {
	if s.auditLog == nil {
		return
	}
	claims, _ := auth.ClaimsFromContext(ctx)
	rec := audit.Record{
		TS:      time.Now().UTC().Format(time.RFC3339Nano),
		Caller:  audit.CallerFromClaims(claims),
		Method:  "/bunker.v1.Bunkerd/" + method,
		Outcome: "ok",
		Summary: summary,
	}
	if err := s.auditLog.Log(rec); err != nil {
		s.logger.Warn("operator lifecycle audit write failed", "method", method)
	}
}
