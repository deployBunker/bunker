package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"

	"github.com/deployBunker/bunker/internal/apikey"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// gap131Secret is the HS256 secret shared by the daemon-side interceptors and
// the test token minter.
const gap131Secret = "gap131-test-jwt-secret-must-be-at-least-32-bytes"

// heartbeaLiveStaticToken is the static master token the live harness's
// master-only interceptor is constructed with.
const heartbeaLiveStaticToken = "gap131-live-static-master-token"

// TestHeartbeatAgent_AgentKeyOwnership pins SEC-07 / GAP-131's handler half:
// an agent-scoped credential may only extend its OWN agent's TTL. A peer's id
// is CodePermissionDenied — the same ownership comparison Metrics already
// enforces. Master and static-token callers (empty claims AgentID) may extend
// any agent, exactly as before.
//
// The property is asserted behaviourally, not just by error code: the rejected
// rows also assert the peer's stored expiry is byte-for-byte unchanged, because
// "the peer's TTL was extended" is the actual damage the guard prevents.
func TestHeartbeatAgent_AgentKeyOwnership(t *testing.T) {
	logger := testDiscardLogger()
	baseline := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)

	newSvc := func() (*bunkerdService, *resource.Tracker) {
		tracker := resource.NewTracker(10, logger)
		if err := tracker.Register(&resource.AgentRecord{
			AgentID:   "victim-agent",
			Status:    "running",
			ExpiresAt: baseline,
		}); err != nil {
			t.Fatalf("register victim-agent: %v", err)
		}
		cfg := config.DefaultConfig()
		cfg.Agent.DefaultTTL = 6 * time.Hour
		return &bunkerdService{cfg: cfg, logger: logger, tracker: tracker}, tracker
	}

	tests := []struct {
		name     string
		claims   *auth.Claims // nil = master / static-token caller (no claims)
		agentID  string
		wantCode connect.Code // zero = expect success
	}{
		{
			name:    "agent-scoped claims on its OWN agent_id proceed",
			claims:  &auth.Claims{AgentID: "victim-agent"},
			agentID: "victim-agent",
		},
		{
			name:    "master claims on any agent_id proceed",
			agentID: "victim-agent",
		},
		{
			name:    "static-token claims (empty AgentID) on any agent_id proceed",
			claims:  &auth.Claims{AgentID: ""},
			agentID: "victim-agent",
		},
		{
			name:     "agent-scoped claims on a PEER agent_id are denied",
			claims:   &auth.Claims{AgentID: "peer-agent"},
			agentID:  "victim-agent",
			wantCode: connect.CodePermissionDenied,
		},
		{
			name:     "agent-scoped claims on an unknown agent_id are denied",
			claims:   &auth.Claims{AgentID: "peer-agent"},
			agentID:  "no-such-agent",
			wantCode: connect.CodePermissionDenied,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, tracker := newSvc()
			ctx := context.Background()
			if tt.claims != nil {
				ctx = auth.ContextWithClaims(ctx, tt.claims)
			}

			resp, err := svc.HeartbeatAgent(ctx, connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: tt.agentID}))

			rec := tracker.Get("victim-agent")
			if rec == nil {
				t.Fatal("victim-agent disappeared from the tracker")
			}

			if tt.wantCode != 0 {
				if err == nil {
					t.Fatalf("HeartbeatAgent() succeeded, want %v", tt.wantCode)
				}
				cerr, ok := err.(*connect.Error)
				if !ok {
					t.Fatalf("error is %T, want *connect.Error: %v", err, err)
				}
				if cerr.Code() != tt.wantCode {
					t.Errorf("code = %v, want %v", cerr.Code(), tt.wantCode)
				}
				// The peer's TTL must be untouched — that is the whole point.
				if !rec.ExpiresAt.Equal(baseline) {
					t.Errorf("peer TTL was extended by a foreign agent key: %v (want %v)", rec.ExpiresAt, baseline)
				}
				return
			}

			if err != nil {
				t.Fatalf("HeartbeatAgent() error: %v", err)
			}
			if !resp.Msg.GetAcknowledged() {
				t.Error("heartbeat not acknowledged")
			}
			if !rec.ExpiresAt.After(baseline) {
				t.Errorf("TTL not extended: %v (baseline %v)", rec.ExpiresAt, baseline)
			}
		})
	}
}

// TestHeartbeatAgent_MasterOnlyInterceptorRejectsAgentScoped is the DRIFT PIN
// for the SEC-07 property the handler guard above backstops.
//
// HeartbeatAgent is mounted behind the master-only interceptor
// (internal/server/server.go builds it with
// auth.NewMasterOnlyAuthInterceptorFromAuth, derived from the SHARED jwtAuth
// instance so rotations take effect live — DF-BUNKER-45).
// That interceptor — not the handler guard — is what actually rejects agent
// credentials today: auth.JWTAuth.authenticate refuses both an agent-scoped
// JWT and an agent-scoped opaque sub-key with CodeUnauthenticated BEFORE the
// handler is invoked. This test constructs the interceptor exactly as
// server.go does and drives the REAL HeartbeatAgent handler behind it, so the
// property cannot silently regress (e.g. if someone widens the mount to the
// permissive interceptor, or drops the masterKeyOnly branch, this fails).
//
// If this test ever needs changing, GAP-131's handler guard must be revisited
// in the same change — the two halves are one control.
func TestHeartbeatAgent_MasterOnlyInterceptorRejectsAgentScoped(t *testing.T) {
	const staticToken = "gap131-static-master-token"

	keyMgr := apikey.NewManager(gap131Secret)
	minter := auth.NewJWTAuth(gap131Secret, keyMgr)

	agentJWT, err := minter.IssueAgentToken("peer-agent", time.Hour)
	if err != nil {
		t.Fatalf("issue agent JWT: %v", err)
	}
	masterJWT, err := minter.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue master JWT: %v", err)
	}
	agentSubKey, _, err := keyMgr.Generate("peer-agent", time.Hour)
	if err != nil {
		t.Fatalf("generate agent sub-key: %v", err)
	}

	// Built exactly as server.go:178 does for the Bunkerd (master-only) mount.
	interceptor := auth.NewMasterOnlyAuthInterceptor(gap131Secret, keyMgr, staticToken, true)

	baseline := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	logger := testDiscardLogger()
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{
		AgentID:   "peer-agent",
		Status:    "running",
		ExpiresAt: baseline,
	}); err != nil {
		t.Fatalf("register peer-agent: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Agent.DefaultTTL = 6 * time.Hour
	svc := &bunkerdService{cfg: cfg, logger: logger, tracker: tracker}

	tests := []struct {
		name        string
		token       string
		wantReached bool
		wantCode    connect.Code // only meaningful when !wantReached
	}{
		{
			name:     "agent-scoped JWT is rejected and the handler is never reached",
			token:    agentJWT,
			wantCode: connect.CodeUnauthenticated,
		},
		{
			name:     "agent-scoped sub-key is rejected and the handler is never reached",
			token:    agentSubKey,
			wantCode: connect.CodeUnauthenticated,
		},
		{
			name:        "master JWT reaches the handler",
			token:       masterJWT,
			wantReached: true,
		},
		{
			name:        "static master token reaches the handler",
			token:       staticToken,
			wantReached: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset the TTL so each row starts from a known expiry.
			rec := tracker.Get("peer-agent")
			if rec == nil {
				t.Fatal("peer-agent missing before call")
			}
			rec.ExpiresAt = baseline

			reached := 0
			wrapped := interceptor.WrapUnary(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				reached++
				return svc.HeartbeatAgent(ctx, connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: "peer-agent"}))
			})

			req := connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: "peer-agent"})
			req.Header().Set("Authorization", "Bearer "+tt.token)
			resp, err := wrapped(context.Background(), req)

			if tt.wantReached {
				if err != nil {
					t.Fatalf("expected the interceptor to admit this credential, got %v", err)
				}
				if reached != 1 {
					t.Fatalf("handler reached %d times, want 1", reached)
				}
				hb, ok := resp.(*connect.Response[v1.HeartbeatAgentResponse])
				if !ok {
					t.Fatalf("response is %T, want *connect.Response[HeartbeatAgentResponse]", resp)
				}
				if !hb.Msg.GetAcknowledged() {
					t.Error("heartbeat not acknowledged")
				}
				if rec := tracker.Get("peer-agent"); rec == nil || !rec.ExpiresAt.After(baseline) {
					t.Errorf("master caller did not extend the TTL: %v", rec)
				}
				return
			}

			if err == nil {
				t.Fatal("expected the master-only interceptor to reject an agent-scoped credential")
			}
			if reached != 0 {
				t.Fatalf("handler was reached %d times behind a rejected credential, want 0", reached)
			}
			cerr, ok := err.(*connect.Error)
			if !ok {
				t.Fatalf("error is %T, want *connect.Error: %v", err, err)
			}
			if cerr.Code() != tt.wantCode {
				t.Errorf("code = %v, want %v", cerr.Code(), tt.wantCode)
			}
			if !strings.Contains(cerr.Message(), "agent-scoped tokens are not allowed") {
				t.Errorf("message %q must name the agent-scoped rejection", cerr.Message())
			}
			// SEC-07 in one assertion: the peer's TTL is untouched.
			if rec := tracker.Get("peer-agent"); rec == nil || !rec.ExpiresAt.Equal(baseline) {
				t.Errorf("rejected credential still extended the peer TTL: %v", rec)
			}
		})
	}
}

// TestHeartbeatAgent_InterceptorMountsArePinned is the source-invariant half of
// the drift pin: the property above only holds because server.go mounts the
// Bunkerd service (which carries HeartbeatAgent) with the MASTER-ONLY
// interceptor while the Agent service keeps the permissive one. Swapping the
// two mounts, or "simplifying" the construction, would leave the behavioural
// test green while the live server accepted agent credentials on HeartbeatAgent
// — exactly the silent drift this pins. Change this test and GAP-131's handler
// guard together, or not at all.
func TestHeartbeatAgent_InterceptorMountsArePinned(t *testing.T) {
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
			// DF-BUNKER-45: the master-only interceptor must be derived from
			// the SAME JWTAuth instance RotateJWTSecret mutates, so a rotation
			// takes effect on this mount without a restart. A private
			// string-built interceptor would freeze the boot secret here.
			needle: "bunkerdAuthInterceptor := auth.NewMasterOnlyAuthInterceptorFromAuth(s.jwtAuth,",
			why:    "the Bunkerd service must be built with the master-only interceptor derived from the SHARED jwtAuth instance",
		},
		{
			needle: "bunkerdInterceptors := []connect.Interceptor{bunkerdAuthInterceptor}",
			why:    "the Bunkerd mount must use the master-only interceptor",
		},
		{
			// DF-BUNKER-45: same instance-sharing rule for the permissive mount.
			needle: "agentAuthInterceptor := auth.NewJWTAuthInterceptorFromAuth(s.jwtAuth,",
			why:    "the Agent service must keep the permissive (agent-scoped-capable) interceptor derived from the SHARED jwtAuth instance",
		},
		{
			needle: "agentInterceptors := []connect.Interceptor{agentAuthInterceptor}",
			why:    "the Agent mount must use the permissive interceptor",
		},
	}
	for _, c := range checks {
		if !strings.Contains(text, c.needle) {
			t.Errorf("server.go no longer contains %q (%s); revisit SEC-07/GAP-131 before relaxing this pin",
				c.needle, c.why)
		}
	}
}

// TestAgentServiceHeartbeat_AgentKeyOwnership covers the SECOND heartbeat
// handler. internal/server/service.go mounts the Agent service with the
// permissive interceptor, so agent-scoped credentials really do reach
// agentService.Heartbeat — there the ownership guard is the only control, not
// defense in depth.
func TestAgentServiceHeartbeat_AgentKeyOwnership(t *testing.T) {
	logger := testDiscardLogger()
	baseline := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)

	newSvc := func() (*agentService, *resource.Tracker) {
		tracker := resource.NewTracker(10, logger)
		if err := tracker.Register(&resource.AgentRecord{
			AgentID:   "victim-agent",
			Status:    "running",
			ExpiresAt: baseline,
		}); err != nil {
			t.Fatalf("register victim-agent: %v", err)
		}
		return &agentService{logger: logger, tracker: tracker}, tracker
	}

	tests := []struct {
		name     string
		claims   *auth.Claims
		agentID  string
		wantCode connect.Code
	}{
		{name: "own agent_id proceeds", claims: &auth.Claims{AgentID: "victim-agent"}, agentID: "victim-agent"},
		{name: "master (no claims) proceeds", agentID: "victim-agent"},
		{
			name:     "PEER agent_id from an agent-scoped key is denied",
			claims:   &auth.Claims{AgentID: "peer-agent"},
			agentID:  "victim-agent",
			wantCode: connect.CodePermissionDenied,
		},
		{
			name:     "unknown agent_id from an agent-scoped key is denied",
			claims:   &auth.Claims{AgentID: "peer-agent"},
			agentID:  "no-such-agent",
			wantCode: connect.CodePermissionDenied,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, tracker := newSvc()
			ctx := context.Background()
			if tt.claims != nil {
				ctx = auth.ContextWithClaims(ctx, tt.claims)
			}
			_, err := svc.Heartbeat(ctx, connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: tt.agentID}))

			rec := tracker.Get("victim-agent")
			if rec == nil {
				t.Fatal("victim-agent disappeared from the tracker")
			}
			if tt.wantCode != 0 {
				cerr, ok := err.(*connect.Error)
				if !ok {
					t.Fatalf("error is %T, want *connect.Error: %v", err, err)
				}
				if cerr.Code() != tt.wantCode {
					t.Errorf("code = %v, want %v", cerr.Code(), tt.wantCode)
				}
				if !rec.ExpiresAt.Equal(baseline) {
					t.Errorf("peer TTL was extended by a foreign agent key: %v (want %v)", rec.ExpiresAt, baseline)
				}
				return
			}
			if err != nil {
				t.Fatalf("Heartbeat() error: %v", err)
			}
			if !rec.ExpiresAt.After(baseline) {
				t.Errorf("TTL not extended: %v (baseline %v)", rec.ExpiresAt, baseline)
			}
		})
	}
}

// heartbeatInterceptorHarness serves the REAL bunkerdService.HeartbeatAgent
// handler over a local httptest server behind the VAULT-identical master-only
// interceptor (the same constructor call and the same interceptor the Agent
// service gets), reached by a real connect client. It is the live, full-stack
// companion to TestHeartbeatAgent_MasterOnlyInterceptorRejectsAgentScoped: the
// in-process pin calls the interceptor directly, this one crosses the HTTP wire
// so a mount-level regression (interceptor dropped from connect.WithInterceptors,
// the handler re-registered on the permissive path) is caught too.
func heartbeatInterceptorHarness(t *testing.T) (*httptest.Server, *resource.AgentRecord, time.Time) {
	t.Helper()

	keyMgr := apikey.NewManager(gap131Secret)
	baseline := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)

	logger := testDiscardLogger()
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{
		AgentID:   "victim-agent",
		Status:    "running",
		ExpiresAt: baseline,
	}); err != nil {
		t.Fatalf("register victim-agent: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Agent.DefaultTTL = 6 * time.Hour
	svc := &bunkerdService{cfg: cfg, logger: logger, tracker: tracker}

	interceptor := auth.NewMasterOnlyAuthInterceptor(gap131Secret, keyMgr, heartbeaLiveStaticToken, true)
	path, handler := bunkerv1connect.NewBunkerdHandler(
		svc,
		connect.WithInterceptors(interceptor),
	)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rec := tracker.Get("victim-agent")
	return srv, rec, baseline
}

// TestHeartbeatAgent_LiveMasterOnlyInterceptor drives the real RPC over HTTP
// with each credential class and asserts the TTL itself — the ground truth a
// rejected request must never move.
func TestHeartbeatAgent_LiveMasterOnlyInterceptor(t *testing.T) {
	srv, rec, baseline := heartbeatInterceptorHarness(t)

	keyMgr := apikey.NewManager(gap131Secret)
	minter := auth.NewJWTAuth(gap131Secret, keyMgr)
	agentJWT, err := minter.IssueAgentToken("peer-agent", time.Hour)
	if err != nil {
		t.Fatalf("issue agent JWT: %v", err)
	}
	agentSubKey, _, err := keyMgr.Generate("peer-agent", time.Hour)
	if err != nil {
		t.Fatalf("generate sub-key: %v", err)
	}

	tests := []struct {
		name        string
		token       func() string
		wantOK      bool
		wantCode    connect.Code
		wantExtends bool
	}{
		{
			name:     "agent JWT over the wire is 401 and does not extend the peer TTL",
			token:    func() string { return agentJWT },
			wantCode: connect.CodeUnauthenticated,
		},
		{
			name:     "agent sub-key over the wire is 401 and does not extend the peer TTL",
			token:    func() string { return agentSubKey },
			wantCode: connect.CodeUnauthenticated,
		},
		{
			name: "master JWT over the wire succeeds and extends the TTL",
			token: func() string {
				tok, err := minter.IssueMasterToken(time.Hour)
				if err != nil {
					t.Fatalf("issue master JWT: %v", err)
				}
				return tok
			},
			wantOK:      true,
			wantExtends: true,
		},
		{
			name:        "static master token over the wire succeeds and extends the TTL",
			token:       func() string { return heartbeaLiveStaticToken },
			wantOK:      true,
			wantExtends: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Each row starts from the recorded baseline expiry.
			rec.ExpiresAt = baseline

			client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
			req := connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: "victim-agent"})
			req.Header().Set("Authorization", "Bearer "+tt.token())

			resp, err := client.HeartbeatAgent(context.Background(), req)

			if tt.wantOK {
				if err != nil {
					t.Fatalf("master credential rejected over the wire: %v", err)
				}
				if !resp.Msg.GetAcknowledged() {
					t.Error("heartbeat not acknowledged")
				}
				if !rec.ExpiresAt.After(baseline) {
					t.Errorf("master caller did not extend the TTL: %v (baseline %v)", rec.ExpiresAt, baseline)
				}
				return
			}

			if err == nil {
				t.Fatalf("agent-scoped credential was admitted over the wire: %+v", resp.Msg)
			}
			if got := connect.CodeOf(err); got != tt.wantCode {
				t.Errorf("code = %v, want %v (err: %v)", got, tt.wantCode, err)
			}
			// SEC-07 ground truth: a rejected credential never moves the TTL.
			if !rec.ExpiresAt.Equal(baseline) {
				t.Errorf("rejected credential extended the peer TTL over the wire: %v (want %v)", rec.ExpiresAt, baseline)
			}
		})
	}
}
