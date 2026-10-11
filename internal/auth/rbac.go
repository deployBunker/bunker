package auth

import (
	"context"
	"errors"

	"connectrpc.com/connect"
)

// RBAC enforcement (GAP-130).
//
// The RBAC interceptor runs AFTER authentication and AFTER the audit
// interceptor (auth first in connect.WithInterceptors, then audit, then
// RBAC — so an RBAC denial is authenticated AND hash-chained into the audit
// trail, naming the operator who was denied). It maps each RPC procedure to
// the minimum role that may call it and denies callers below that bar with
// CodePermissionDenied.
//
// Role semantics (GAP-130):
//
//   - Claims without a Role (master static token, unscoped master JWTs,
//     opaque apikey sub-keys) are LEGACY callers: unchanged behavior, every
//     Bunkerd RPC allowed. The master-only interceptor upstream already
//     rejects agent-scoped credentials on this mount, so "no role" here
//     means exactly the credentials that could always do everything.
//   - RoleAdmin — everything (same as legacy).
//   - RoleOperator — the agent-lifecycle surface; operator management
//     (OperatorCreate/List/Delete) and system configuration
//     (RotateJWTSecret, key lifecycle, audit query) are denied.
//   - RoleAgentSelf — never reaches this interceptor on the Bunkerd mount:
//     the master-only interceptor rejects any claim that carries an AgentID
//     before RBAC runs. The role is enforced on the Agent service by the
//     existing ownership guards (claims.AgentID must equal the target
//     agent), which an agent:self token satisfies for its OWN agent only —
//     that is the whole semantics of the role.
//
// The role vocabulary lives in internal/operator; this file deliberately
// re-declares only the STRING VALUES to avoid an import cycle
// (internal/operator imports auth for the claims plumbing). A test pins the
// two spellings together so they cannot silently drift.

// Role values mirroring operator.Role* (rbac_test.go pins the two spellings
// together — change both sides or break the build's drift test).
const (
	RoleAdmin     = "admin"
	RoleOperator  = "operator"
	RoleAgentSelf = "agent:self"
)

// agentLifecycleProcedures is the set of Bunkerd procedures an
// operator-role caller may invoke: the agent-lifecycle surface plus
// read-only observability of agents. Everything NOT in this set is denied
// for role-carrying non-admin claims: the default is DENY, so a future
// privileged RPC can never silently open up to the operator role — it is
// admin-only until a test pins it into this set.
var agentLifecycleProcedures = map[string]bool{
	"/bunker.v1.Bunkerd/SpawnAgent":         true,
	"/bunker.v1.Bunkerd/DestroyAgent":       true,
	"/bunker.v1.Bunkerd/StopAgent":          true,
	"/bunker.v1.Bunkerd/StartAgent":         true,
	"/bunker.v1.Bunkerd/RestartAgent":       true,
	"/bunker.v1.Bunkerd/ListAgents":         true,
	"/bunker.v1.Bunkerd/GetAgent":           true,
	"/bunker.v1.Bunkerd/AgentMetrics":       true,
	"/bunker.v1.Bunkerd/ExecAgent":          true,
	"/bunker.v1.Bunkerd/RunAgent":           true,
	"/bunker.v1.Bunkerd/HeartbeatAgent":     true,
	"/bunker.v1.Bunkerd/RenewalDriftReport": true,
	"/bunker.v1.Bunkerd/GetAgentKey":        true,
}

// adminOnlyProcedures names the surfaces an operator-role caller is told
// are admin-only in the denial itself (operator management + system
// configuration + host-level observability). Membership is documentation
// for the denial class; the enforcement default is already deny.
var adminOnlyProcedures = map[string]bool{
	"/bunker.v1.Bunkerd/OperatorCreate":  true,
	"/bunker.v1.Bunkerd/OperatorList":    true,
	"/bunker.v1.Bunkerd/OperatorDelete":  true,
	"/bunker.v1.Bunkerd/RotateJWTSecret": true,
	"/bunker.v1.Bunkerd/RevokeKey":       true,
	"/bunker.v1.Bunkerd/KeyList":         true,
	"/bunker.v1.Bunkerd/QueryAudit":      true,
	"/bunker.v1.Bunkerd/ServerInfo":      true,
	"/bunker.v1.Bunkerd/ServerMetrics":   true,
}

// operatorDeniedReason classifies a procedure for an operator-role caller:
// "" = allowed, otherwise the denial reason.
func operatorDeniedReason(procedure string) string {
	if agentLifecycleProcedures[procedure] {
		return ""
	}
	if adminOnlyProcedures[procedure] {
		return "operator management or system configuration requires the admin role"
	}
	return "procedure is outside the operator role's allowed surface"
}

// RBACInterceptor enforces GAP-130 role checks on the Bunkerd mount.
// Zero value is usable; construct with NewRBACInterceptor.
type RBACInterceptor struct{}

// NewRBACInterceptor builds the RBAC interceptor.
func NewRBACInterceptor() *RBACInterceptor { return &RBACInterceptor{} }

// allowedForRole answers "may claims call procedure?" for role-carrying
// claims. Legacy claims (no Role) are always allowed — the interceptors
// upstream already encode their admission (master-only on Bunkerd,
// permissive with ownership guards on Agent).
func allowedForRole(claims *Claims, procedure string) (bool, string) {
	if claims == nil || claims.Role == "" {
		return true, ""
	}
	switch claims.Role {
	case RoleAdmin:
		return true, ""
	case RoleOperator:
		why := operatorDeniedReason(procedure)
		return why == "", why
	case RoleAgentSelf:
		// agent:self tokens carry an AgentID, so the master-only
		// interceptor rejects them before RBAC on this mount; if one
		// arrives anyway (misconfiguration), deny rather than guess.
		return false, "agent:self credentials may only operate on their own agent"
	default:
		return false, "unknown role " + claims.Role
	}
}

// WrapUnary enforces the role check on unary requests.
func (r *RBACInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := r.check(ctx, req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

// WrapStreamingHandler enforces the role check on streaming requests
// (ExecAgent is the one streaming procedure on this mount, and it IS in the
// operator set, so an operator-role exec still works end to end).
func (r *RBACInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := r.check(ctx, conn.Spec().Procedure); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// WrapStreamingClient is a no-op — RBAC is server-side only.
func (r *RBACInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (r *RBACInterceptor) check(ctx context.Context, procedure string) error {
	claims, _ := ClaimsFromContext(ctx)
	allowed, why := allowedForRole(claims, procedure)
	if allowed {
		return nil
	}
	role := `""`
	if claims != nil && claims.Role != "" {
		role = `"` + claims.Role + `"`
	}
	return connect.NewError(connect.CodePermissionDenied,
		errors.New("role "+role+" is not permitted to call "+procedure+": "+why))
}

// Ensure the interceptor satisfies the interface at compile time.
var _ connect.Interceptor = (*RBACInterceptor)(nil)
