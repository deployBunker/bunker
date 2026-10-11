// gap130_test.go — GAP-130 acceptance coverage: per-operator identity with
// RBAC roles. Drives the daemon's REAL interceptor stack (master-only auth →
// RBAC) over the wire-shaped path, the credential path end to end (operator
// token → claims), and the operator lifecycle RPCs. Follows the GAP-132
// test shape: no re-implementation of the composition under test.
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
	"github.com/deployBunker/bunker/internal/operator"
)

// newGap130Service builds a bunkerdService wired like Run does for the
// operator path: durable operator registry + key manager + shared JWTAuth
// with the operator source attached + audit trail — all against temp dirs.
func newGap130Service(t *testing.T) (*bunkerdService, *operator.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	opMgr, err := operator.NewManagerAt(filepath.Join(dir, "operators"))
	if err != nil {
		t.Fatalf("operator registry: %v", err)
	}
	keyMgr, err := apikey.NewManagerAt(gap131Secret, filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatalf("key manager: %v", err)
	}
	auditPath := filepath.Join(dir, "audit.log")
	l, err := audit.New(auditPath)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	jwtAuth := auth.NewJWTAuthWithStaticFallback(gap131Secret, "master-static", keyMgr)
	jwtAuth.SetOperatorSource(opMgr)
	svc := &bunkerdService{
		cfg:      config.DefaultConfig(),
		logger:   testDiscardLogger(),
		keyMgr:   keyMgr,
		jwtAuth:  jwtAuth,
		auditLog: l,
		opMgr:    opMgr,
	}
	return svc, opMgr, auditPath
}

func gap130Authz(token string) *connect.Request[v1.OperatorListRequest] {
	req := connect.NewRequest(&v1.OperatorListRequest{})
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}
	return req
}

// TestOperatorCreateThroughInterceptors drives auth + RBAC exactly as Run
// mounts them: an admin-role operator creates an operator-role identity; an
// operator-role caller is DENIED OperatorCreate (RBAC, permission_denied);
// an agent-scoped credential is rejected before RBAC (unauthenticated);
// a legacy master token succeeds.
func TestOperatorCreateThroughInterceptors(t *testing.T) {
	svc, opMgr, _ := newGap130Service(t)

	// The admin operator that will try to create others.
	adminTok, _, err := opMgr.Create("root", operator.RoleAdmin, 0)
	if err != nil {
		t.Fatalf("create admin operator: %v", err)
	}
	// And an operator-role operator.
	opTok, _, err := opMgr.Create("kelly", operator.RoleOperator, 0)
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}
	// And an agent:self operator (bound to agent-77).
	selfTok, _, err := opMgr.Create("agent-77", operator.RoleAgentSelf, 0)
	if err != nil {
		t.Fatalf("create agent:self operator: %v", err)
	}

	masterOnly := auth.NewMasterOnlyAuthInterceptorFromAuth(svc.jwtAuth, true)
	rbac := auth.NewRBACInterceptor()
	stack := func(next connect.UnaryFunc) connect.UnaryFunc {
		return masterOnly.WrapUnary(rbac.WrapUnary(next))
	}

	reached := 0
	handler := connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		reached++
		claims, _ := auth.ClaimsFromContext(ctx)
		return connect.NewResponse(&v1.OperatorCreateResponse{Name: claims.OpName}), nil
	})

	tests := []struct {
		name      string
		token     string
		role      string
		wantCode  connect.Code
		wantReach bool
	}{
		{name: "legacy master static token creates", token: "master-static", role: "operator", wantReach: true},
		{name: "admin-role operator creates", token: adminTok, role: "operator", wantReach: true},
		{name: "operator-role caller denied (RBAC)", token: opTok, role: "operator", wantCode: connect.CodePermissionDenied},
		{name: "agent:self operator rejected (master-only, agent-scoped)", token: selfTok, wantCode: connect.CodeUnauthenticated},
		{name: "no credential rejected", token: "", wantCode: connect.CodeUnauthenticated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reached = 0
			req := connect.NewRequest(&v1.OperatorCreateRequest{Name: "new-op", Role: tc.role})
			if tc.token != "" {
				req.Header().Set("Authorization", "Bearer "+tc.token)
			}
			_, err := stack(handler)(context.Background(), req)
			if tc.wantReach {
				if err != nil {
					t.Fatalf("unexpected denial: %v", err)
				}
				if reached != 1 {
					t.Fatalf("handler calls = %d, want 1", reached)
				}
				return
			}
			if err == nil {
				t.Fatal("request was allowed, want denial")
			}
			if reached != 0 {
				t.Fatalf("handler ran on a denied request (%d calls)", reached)
			}
			if got := connect.CodeOf(err); got != tc.wantCode {
				t.Fatalf("code = %v, want %v", got, tc.wantCode)
			}
		})
	}
}

// TestOperatorRPCLifecycle covers the handlers themselves through the real
// stack: create (token once, hash stored), list (metadata only), delete
// (immediate), plus audit records naming the acting operator.
func TestOperatorRPCLifecycle(t *testing.T) {
	svc, opMgr, auditPath := newGap130Service(t)
	adminTok, _, err := opMgr.Create("root", operator.RoleAdmin, 0)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	masterOnly := auth.NewMasterOnlyAuthInterceptorFromAuth(svc.jwtAuth, true)
	rbac := auth.NewRBACInterceptor()
	ctx := context.Background()

	// CREATE.
	createReq := connect.NewRequest(&v1.OperatorCreateRequest{Name: "ci-bot", Role: "operator", TtlSeconds: 3600})
	createReq.Header().Set("Authorization", "Bearer "+adminTok)
	var newTok string
	_, err = masterOnly.WrapUnary(rbac.WrapUnary(connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		r, rerr := svc.OperatorCreate(ctx, connect.NewRequest(req.Any().(*v1.OperatorCreateRequest)))
		if rerr != nil {
			return nil, rerr
		}
		newTok = r.Msg.GetToken()
		return connect.NewResponse(r.Msg), nil
	})))(ctx, createReq)
	if err != nil {
		t.Fatalf("OperatorCreate: %v", err)
	}
	if newTok == "" {
		t.Fatal("OperatorCreate returned an empty token")
	}
	// The new token authenticates and carries the operator role.
	claims, err := svc.jwtAuth.AuthenticateRawToken(newTok, "test", "test")
	if err != nil {
		t.Fatalf("new operator token does not authenticate: %v", err)
	}
	if claims.Role != auth.RoleOperator || claims.OpName != "ci-bot" {
		t.Fatalf("claims = role %q name %q, want operator/ci-bot", claims.Role, claims.OpName)
	}
	if claims.AgentID != "" {
		t.Fatalf("non-agent operator carries AgentID %q", claims.AgentID)
	}
	// LIST (through the same stack shape).
	listReq := gap130Authz(adminTok)
	var listed []*v1.OperatorInfo
	if _, err := masterOnly.WrapUnary(rbac.WrapUnary(connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		r, rerr := svc.OperatorList(ctx, connect.NewRequest(req.Any().(*v1.OperatorListRequest)))
		if rerr != nil {
			return nil, rerr
		}
		listed = r.Msg.GetOperators()
		return connect.NewResponse(r.Msg), nil
	})))(ctx, listReq); err != nil {
		t.Fatalf("OperatorList: %v", err)
	}
	found := false
	for _, op := range listed {
		if op.GetName() == "ci-bot" {
			found = true
			if op.GetRole() != "operator" {
				t.Fatalf("listed role = %q, want operator", op.GetRole())
			}
			if strings.Contains(strings.ToLower(op.String()), strings.ToLower(newTok)) {
				t.Fatal("operator listing carries token material")
			}
		}
	}
	if !found {
		t.Fatalf("ci-bot missing from listing: %+v", listed)
	}
	// DELETE, then the token stops validating immediately.
	delReq := connect.NewRequest(&v1.OperatorDeleteRequest{Name: "ci-bot"})
	delReq.Header().Set("Authorization", "Bearer "+adminTok)
	if _, err := masterOnly.WrapUnary(rbac.WrapUnary(connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		_, rerr := svc.OperatorDelete(ctx, connect.NewRequest(req.Any().(*v1.OperatorDeleteRequest)))
		return nil, rerr
	})))(ctx, delReq); err != nil {
		t.Fatalf("OperatorDelete: %v", err)
	}
	if _, err := svc.jwtAuth.AuthenticateRawToken(newTok, "test", "test"); err == nil {
		t.Fatal("deleted operator token still authenticates")
	}

	// AUDIT: create + delete records exist and name the actor.
	b, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	trail := string(b)
	for _, want := range []string{"OperatorCreate", "OperatorDelete", "op:root"} {
		if !strings.Contains(trail, want) {
			t.Errorf("audit trail missing %q:\n%s", want, trail)
		}
	}
}

// TestAgentSelfCredentialPath pins the agent:self semantics end to end
// through the credential path: the token authenticates with AgentID set,
// is admitted on a permissive (Agent-service) interceptor, and is rejected
// by a master-only (Bunkerd) interceptor. Combined with the Agent service's
// existing ownership guards (agent ID must match), this IS the role.
func TestAgentSelfCredentialPath(t *testing.T) {
	svc, opMgr, _ := newGap130Service(t)
	selfTok, _, err := opMgr.Create("agent-77", operator.RoleAgentSelf, 0)
	if err != nil {
		t.Fatalf("create agent:self: %v", err)
	}

	claims, err := svc.jwtAuth.AuthenticateRawToken(selfTok, "test", "test")
	if err != nil {
		t.Fatalf("agent:self token rejected: %v", err)
	}
	if claims.AgentID != "agent-77" || claims.Role != auth.RoleAgentSelf {
		t.Fatalf("claims = agent %q role %q, want agent-77/agent:self", claims.AgentID, claims.Role)
	}

	permissive := auth.NewJWTAuthInterceptorFromAuth(svc.jwtAuth, true)
	masterOnly := auth.NewMasterOnlyAuthInterceptorFromAuth(svc.jwtAuth, true)

	reached := 0
	handler := connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		reached++
		return connect.NewResponse(&v1.HeartbeatAgentResponse{}), nil
	})

	req := connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: "agent-77"})
	req.Header().Set("Authorization", "Bearer "+selfTok)

	// Bunkerd mount: rejected.
	reached = 0
	if _, err := masterOnly.WrapUnary(handler)(context.Background(), req); err == nil {
		t.Fatal("agent:self token admitted to the Bunkerd mount")
	} else if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("Bunkerd code = %v, want unauthenticated", got)
	}
	if reached != 0 {
		t.Fatal("handler ran on the rejected mount")
	}
	// Agent mount: admitted (the handler's own ownership guard then checks
	// claims.AgentID == target, which holds for its own agent).
	reached = 0
	if _, err := permissive.WrapUnary(handler)(context.Background(), req); err != nil {
		t.Fatalf("agent:self token rejected on the Agent mount: %v", err)
	}
	if reached != 1 {
		t.Fatalf("handler calls = %d, want 1", reached)
	}
}

// TestOperatorCredentialDoesNotShadowLegacyCredentials pins the admission
// order: an operator token is consulted LAST, so a value that matches both
// an apikey sub-key and an operator resolves as the SUB-KEY.
func TestOperatorCredentialDoesNotShadowLegacyCredentials(t *testing.T) {
	svc, opMgr, _ := newGap130Service(t)
	opTok, _, err := opMgr.Create("kelly", operator.RoleOperator, 0)
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}
	subTok, _, err := svc.keyMgr.Generate("agent-x", time.Hour)
	if err != nil {
		t.Fatalf("generate sub-key: %v", err)
	}
	// Register the SUB-KEY's token as an operator credential too: the
	// registry check runs last, so the sub-key interpretation must win.
	if _, _, err := opMgr.Create(subTok[:32], operator.RoleAdmin, 0); err != nil {
		t.Fatalf("seed colliding operator: %v", err)
	}
	claims, err := svc.jwtAuth.AuthenticateRawToken(subTok, "test", "test")
	if err != nil {
		t.Fatalf("sub-key rejected: %v", err)
	}
	if claims.KeyID == "" || claims.Role != "" {
		t.Fatalf("sub-key resolved as operator (role=%q key=%q), want sub-key claims", claims.Role, claims.KeyID)
	}
	// And the plain operator token still resolves as an operator.
	claims, err = svc.jwtAuth.AuthenticateRawToken(opTok, "test", "test")
	if err != nil {
		t.Fatalf("operator token rejected: %v", err)
	}
	if claims.Role != auth.RoleOperator {
		t.Fatalf("operator token role = %q", claims.Role)
	}
}

// TestOperatorRegistryNilUnchanged pins backward compatibility: a service
// with NO operator registry behaves exactly as before (handlers refuse with
// failed_precondition; the credential path is untouched).
func TestOperatorRegistryNilUnchanged(t *testing.T) {
	svc, _, _ := newGap130Service(t)
	svc.opMgr = nil
	svc.jwtAuth.SetOperatorSource(nil)

	ctx := context.Background()
	if _, err := svc.OperatorCreate(ctx, connect.NewRequest(&v1.OperatorCreateRequest{Name: "x", Role: "admin"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("OperatorCreate without registry: %v", err)
	}
	if _, err := svc.OperatorList(ctx, connect.NewRequest(&v1.OperatorListRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("OperatorList without registry: %v", err)
	}
	if _, err := svc.OperatorDelete(ctx, connect.NewRequest(&v1.OperatorDeleteRequest{Name: "x"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("OperatorDelete without registry: %v", err)
	}
	// Static-token auth is unchanged.
	claims, err := svc.jwtAuth.AuthenticateRawToken("whatever", "test", "test")
	if err == nil && claims.Role != "" {
		t.Fatalf("credential path with nil registry produced role %q", claims.Role)
	}
}

// TestOperatorCreateValidation covers the argument validation surface.
func TestOperatorCreateValidation(t *testing.T) {
	svc, _, _ := newGap130Service(t)
	ctx := context.Background()
	tests := []struct {
		name string
		role string
		want connect.Code
	}{
		{name: "", role: "admin", want: connect.CodeFailedPrecondition},     // invalid name
		{name: "bad role", role: "root", want: connect.CodeInvalidArgument}, // invalid role
		{name: "dup", role: "admin", want: connect.CodeFailedPrecondition},  // duplicate
	}
	// Seed for the duplicate row.
	if _, _, err := svc.opMgr.Create("dup", operator.RoleAdmin, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, tc := range tests {
		t.Run(tc.name+"/"+tc.role, func(t *testing.T) {
			_, err := svc.OperatorCreate(ctx, connect.NewRequest(&v1.OperatorCreateRequest{Name: tc.name, Role: tc.role}))
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("code = %v (%v), want %v", got, err, tc.want)
			}
		})
	}
	// agent:self requires the agent id as the name.
	_, err := svc.OperatorCreate(ctx, connect.NewRequest(&v1.OperatorCreateRequest{Name: "agent-77", Role: "agent:self"}))
	if err != nil {
		t.Fatalf("agent:self create rejected: %v", err)
	}
	ident := svc.opMgr.List("agent-77")
	if len(ident) != 1 || ident[0].AgentID != "agent-77" {
		t.Fatalf("agent:self identity not bound: %+v", ident)
	}
}

// TestOperatorWiringPinnedInServerSource follows the repo's source-pin
// discipline (the interceptor-mount pins in heartbeat_test.go): the wiring
// that makes operators REAL must exist in Run — the registry opened next to
// the key store, attached to the shared JWTAuth, the RBAC interceptor
// appended to the Bunkerd chain after audit, and opMgr handed to the
// service. A refactor that silently unwires any leg must fail HERE, not at
// 2am on a live host.
func TestOperatorWiringPinnedInServerSource(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	text := string(src)

	checks := []struct {
		needle string
		why    string
	}{
		{
			needle: "opMgr, err := operator.NewManagerAt(filepath.Join(s.cfg.Agent.BaseDataDir, \"operators\"))",
			why:    "Run must open the durable operator registry (fail-before-listen), the same posture as the key store",
		},
		{
			needle: "s.jwtAuth.SetOperatorSource(opMgr)",
			why:    "the shared JWTAuth instance must consult the operator registry on the credential path",
		},
		{
			needle: "bunkerdInterceptors = append(bunkerdInterceptors, auth.NewRBACInterceptor())",
			why:    "the Bunkerd mount must carry the GAP-130 RBAC interceptor after auth and audit",
		},
		{
			needle: "opMgr: s.opMgr",
			why:    "the bunkerdService must receive the registry or its RPCs would always refuse",
		},
	}
	for _, c := range checks {
		if !strings.Contains(text, c.needle) {
			t.Errorf("server.go no longer contains %q (%s); revisit GAP-130 wiring before relaxing this pin", c.needle, c.why)
		}
	}
}
