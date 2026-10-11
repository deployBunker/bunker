package auth

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
)

// mkReg is a small helper for claims with only a subject.
func mkReg(subject string) jwt.RegisteredClaims {
	return jwt.RegisteredClaims{Subject: subject}
}

// mockStreamingConnRBAC implements connect.StreamingHandlerConn with a
// configurable procedure — the existing mockStreamingConn pins its spec,
// and RBAC's decision reads Spec().Procedure, so this test needs a
// parameterized twin rather than mutating the shared mock.
type mockStreamingConnRBAC struct {
	procedure string
}

func (m *mockStreamingConnRBAC) Spec() connect.Spec {
	return connect.Spec{Procedure: m.procedure, IsClient: false}
}
func (m *mockStreamingConnRBAC) Peer() connect.Peer { return connect.Peer{} }
func (m *mockStreamingConnRBAC) RequestHeader() http.Header {
	return http.Header{}
}
func (m *mockStreamingConnRBAC) Receive(any) error { return nil }
func (m *mockStreamingConnRBAC) Send(any) error    { return nil }

func (m *mockStreamingConnRBAC) ResponseHeader() http.Header  { return http.Header{} }
func (m *mockStreamingConnRBAC) ResponseTrailer() http.Header { return http.Header{} }

// GAP-130 RBAC enforcement tests. The allowedForRole table is the
// acceptance-matrix; the interceptor tests drive the real connect
// wrappers (unary specs are populated by the handler, so the full
// request-path behavior is proven in internal/server against a real
// in-process connect server — see gap130_test.go there).

func TestRBACRoleVocabularyPinned(t *testing.T) {
	// The string values rbac.go enforces must equal the wire vocabulary the
	// daemon speaks (mirrored in internal/operator; the operator package's
	// own test pins that side). If this fails, one side was renamed alone.
	if RoleAdmin != "admin" || RoleOperator != "operator" || RoleAgentSelf != "agent:self" {
		t.Fatalf("role vocabulary drifted: %q %q %q", RoleAdmin, RoleOperator, RoleAgentSelf)
	}
}

func TestRBACAllowedForRole(t *testing.T) {
	const (
		spawn  = "/bunker.v1.Bunkerd/SpawnAgent"
		exec   = "/bunker.v1.Bunkerd/ExecAgent"
		opCr   = "/bunker.v1.Bunkerd/OperatorCreate"
		opDel  = "/bunker.v1.Bunkerd/OperatorDelete"
		opLs   = "/bunker.v1.Bunkerd/OperatorList"
		rotate = "/bunker.v1.Bunkerd/RotateJWTSecret"
		revoke = "/bunker.v1.Bunkerd/RevokeKey"
		auditQ = "/bunker.v1.Bunkerd/QueryAudit"
		info   = "/bunker.v1.Bunkerd/ServerInfo"
		unkn   = "/bunker.v1.Bunkerd/SomeFuturePrivilegedRPC"
	)

	tests := []struct {
		name      string
		claims    *Claims
		procedure string
		wantAllow bool
	}{
		// Legacy credentials (no role): unchanged behavior — everything.
		{name: "master static token (no role) allowed everywhere", claims: &Claims{RegisteredClaims: mkReg("static-token")}, procedure: opCr, wantAllow: true},
		{name: "unscoped JWT (no role) allowed everywhere", claims: &Claims{}, procedure: rotate, wantAllow: true},
		{name: "apikey sub-key (no role) allowed everywhere", claims: &Claims{AgentID: "a1", KeyID: "bk_x"}, procedure: spawn, wantAllow: true},
		{name: "nil claims (auth disabled) allowed", claims: nil, procedure: opDel, wantAllow: true},

		// admin role: everything.
		{name: "admin creates operators", claims: &Claims{Role: RoleAdmin, OpName: "root"}, procedure: opCr, wantAllow: true},
		{name: "admin rotates secret", claims: &Claims{Role: RoleAdmin, OpName: "root"}, procedure: rotate, wantAllow: true},
		{name: "admin spawns", claims: &Claims{Role: RoleAdmin, OpName: "root"}, procedure: spawn, wantAllow: true},

		// operator role: agent lifecycle yes, operator management and
		// system configuration no.
		{name: "operator spawns", claims: &Claims{Role: RoleOperator, OpName: "kelly"}, procedure: spawn, wantAllow: true},
		{name: "operator execs", claims: &Claims{Role: RoleOperator, OpName: "kelly"}, procedure: exec, wantAllow: true},
		{name: "operator lists agents", claims: &Claims{Role: RoleOperator, OpName: "kelly"}, procedure: "/bunker.v1.Bunkerd/ListAgents", wantAllow: true},
		{name: "operator destroys", claims: &Claims{Role: RoleOperator, OpName: "kelly"}, procedure: "/bunker.v1.Bunkerd/DestroyAgent", wantAllow: true},
		{name: "operator CANNOT create operators", claims: &Claims{Role: RoleOperator, OpName: "kelly"}, procedure: opCr, wantAllow: false},
		{name: "operator CANNOT delete operators", claims: &Claims{Role: RoleOperator, OpName: "kelly"}, procedure: opDel, wantAllow: false},
		{name: "operator CANNOT list operators", claims: &Claims{Role: RoleOperator, OpName: "kelly"}, procedure: opLs, wantAllow: false},
		{name: "operator CANNOT rotate the jwt secret", claims: &Claims{Role: RoleOperator, OpName: "kelly"}, procedure: rotate, wantAllow: false},
		{name: "operator CANNOT revoke keys", claims: &Claims{Role: RoleOperator, OpName: "kelly"}, procedure: revoke, wantAllow: false},
		{name: "operator CANNOT query the audit trail", claims: &Claims{Role: RoleOperator, OpName: "kelly"}, procedure: auditQ, wantAllow: false},
		{name: "operator CANNOT read host server info", claims: &Claims{Role: RoleOperator, OpName: "kelly"}, procedure: info, wantAllow: false},
		// Default deny: an unclassified (future privileged) procedure is
		// admin-only until a test pins it into the operator set.
		{name: "operator CANNOT call an unclassified future RPC", claims: &Claims{Role: RoleOperator, OpName: "kelly"}, procedure: unkn, wantAllow: false},

		// agent:self on the Bunkerd mount: denied structurally — the role
		// carries an AgentID so the master-only interceptor rejects it
		// before RBAC; if it ever reached RBAC the answer is still deny.
		{name: "agent:self denied on Bunkerd surface", claims: &Claims{Role: RoleAgentSelf, OpName: "agent-9", AgentID: "agent-9"}, procedure: spawn, wantAllow: false},

		// Unknown role: deny.
		{name: "unknown role denied", claims: &Claims{Role: "superuser"}, procedure: spawn, wantAllow: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			allowed, why := allowedForRole(tc.claims, tc.procedure)
			if allowed != tc.wantAllow {
				t.Fatalf("allowedForRole(role=%q, %s) = %v (%s), want allow=%v", tc.claims.Role, tc.procedure, allowed, why, tc.wantAllow)
			}
			if !allowed && why == "" {
				t.Fatal("denial carries no reason — the audit trail needs the class")
			}
		})
	}
}

// TestRBACStreamingHandler drives the streaming wrapper through a mock conn
// whose spec names REAL procedures — ExecAgent (operator allowed) and
// RotateJWTSecret (operator denied, admin allowed).
func TestRBACStreamingHandler(t *testing.T) {
	rbac := NewRBACInterceptor()

	tests := []struct {
		name      string
		procedure string
		claims    *Claims
		wantCode  connect.Code
		wantCall  bool
	}{
		{
			name:      "operator execs a stream",
			procedure: "/bunker.v1.Bunkerd/ExecAgent",
			claims:    &Claims{Role: RoleOperator, OpName: "kelly"},
			wantCall:  true,
		},
		{
			name:      "operator cannot rotate the signing secret",
			procedure: "/bunker.v1.Bunkerd/RotateJWTSecret",
			claims:    &Claims{Role: RoleOperator, OpName: "kelly"},
			wantCode:  connect.CodePermissionDenied,
		},
		{
			name:      "admin can rotate the signing secret",
			procedure: "/bunker.v1.Bunkerd/RotateJWTSecret",
			claims:    &Claims{Role: RoleAdmin, OpName: "root"},
			wantCall:  true,
		},
		{
			name:      "legacy (no role) caller passes through unchanged",
			procedure: "/bunker.v1.Bunkerd/RotateJWTSecret",
			claims:    &Claims{},
			wantCall:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
				called = true
				return nil
			}
			conn := &mockStreamingConnRBAC{procedure: tc.procedure}
			ctx := context.Background()
			if tc.claims != nil {
				ctx = ContextWithClaims(ctx, tc.claims)
			}
			err := rbac.WrapStreamingHandler(next)(ctx, conn)
			if tc.wantCall {
				if err != nil {
					t.Fatalf("unexpected denial: %v", err)
				}
				if !called {
					t.Fatal("handler was not called on an allowed request")
				}
				return
			}
			if err == nil {
				t.Fatal("request was allowed, want denial")
			}
			if called {
				t.Fatal("handler ran on a denied request")
			}
			if got := connect.CodeOf(err); got != tc.wantCode {
				t.Fatalf("code = %v, want %v", got, tc.wantCode)
			}
		})
	}
}

// TestRBACAgentSelfStructuralDenial pins the agent:self semantics on the
// Bunkerd surface at the RBAC layer: even if such claims reached this
// interceptor (they must not — the master-only interceptor rejects them
// first, pinned in gap130_test.go in internal/server), RBAC denies.
func TestRBACAgentSelfStructuralDenial(t *testing.T) {
	rbac := NewRBACInterceptor()
	conn := &mockStreamingConnRBAC{procedure: "/bunker.v1.Bunkerd/GetAgent"}
	called := false
	err := rbac.WrapStreamingHandler(func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		called = true
		return nil
	})(ContextWithClaims(context.Background(), &Claims{Role: RoleAgentSelf, OpName: "agent-9", AgentID: "agent-9"}), conn)
	if err == nil || called {
		t.Fatalf("agent:self claims passed RBAC on the Bunkerd surface (err=%v, called=%v)", err, called)
	}
}
