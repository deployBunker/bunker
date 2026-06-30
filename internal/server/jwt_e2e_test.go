package server

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/apikey"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// TestJWTAuthInterceptorEndToEnd validates the JWT auth interceptor and the
// agent-scoped opaque sub-key path that SpawnAgent uses. It runs in-process so
// it does not require a real agent user.
func TestJWTAuthInterceptorEndToEnd(t *testing.T) {
	secret := "test-jwt-secret-must-be-at-least-32-bytes-long"
	keyMgr := apikey.NewManager(secret)
	jwtAuth := auth.NewJWTAuth(secret, keyMgr)

	// 1. Missing token should fail.
	req := connect.NewRequest(&v1.ServerInfoRequest{})
	_, err := jwtAuth.WrapUnary(func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&v1.ServerInfoResponse{}), nil
	})(context.Background(), req)
	if err == nil {
		t.Fatal("expected Unauthenticated error for missing token")
	}
	if connectErr, ok := err.(*connect.Error); !ok || connectErr.Code() != connect.CodeUnauthenticated {
		t.Fatalf("expected CodeUnauthenticated, got %v", err)
	}

	// 2. Invalid token should fail.
	req.Header().Set("Authorization", "Bearer invalid-token")
	_, err = jwtAuth.WrapUnary(func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&v1.ServerInfoResponse{}), nil
	})(context.Background(), req)
	if err == nil {
		t.Fatal("expected Unauthenticated error for invalid token")
	}

	// 3. Generate an agent-scoped sub-key (same path SpawnAgent uses).
	agentID := "jwt-e2e-agent"
	token, key, err := keyMgr.Generate(agentID, 5*time.Minute)
	if err != nil {
		t.Fatalf("generate agent sub-key: %v", err)
	}
	if key.AgentID != agentID {
		t.Fatalf("expected key scoped to %q, got %q", agentID, key.AgentID)
	}
	if token == "" {
		t.Fatal("expected non-empty sub-key token")
	}

	// 4. Valid sub-key should succeed and carry agent_id in claims.
	req2 := connect.NewRequest(&v1.ServerInfoRequest{})
	req2.Header().Set("Authorization", "Bearer "+token)
	var gotClaims bool
	_, err = jwtAuth.WrapUnary(func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
		claims, ok := auth.ClaimsFromContext(ctx)
		if !ok {
			t.Error("expected claims in context")
			return nil, connect.NewError(connect.CodeInternal, nil)
		}
		if claims.AgentID != agentID {
			t.Errorf("expected agent_id %q, got %q", agentID, claims.AgentID)
		}
		gotClaims = true
		return connect.NewResponse(&v1.ServerInfoResponse{}), nil
	})(context.Background(), req2)
	if err != nil {
		t.Fatalf("unexpected error with valid sub-key: %v", err)
	}
	if !gotClaims {
		t.Fatal("handler was not called with valid sub-key")
	}

	// 5. Expired sub-key should fail.
	expiredToken, _, err := keyMgr.Generate(agentID, -time.Minute)
	if err != nil {
		t.Fatalf("generate expired sub-key: %v", err)
	}
	req3 := connect.NewRequest(&v1.ServerInfoRequest{})
	req3.Header().Set("Authorization", "Bearer "+expiredToken)
	_, err = jwtAuth.WrapUnary(func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&v1.ServerInfoResponse{}), nil
	})(context.Background(), req3)
	if err == nil {
		t.Fatal("expected Unauthenticated error for expired sub-key")
	}
}

// TestSpawnAgent_GeneratesAPIKey verifies that the service layer generates an
// API sub-key when JWT auth is enabled. It uses a fake agent record in the
// tracker and bypasses the real agent manager so it does not require root.
func TestSpawnAgent_GeneratesAPIKey(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = true
	cfg.Auth.JWTSecret = "test-jwt-secret-must-be-at-least-32-bytes-long"

	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	keyMgr := apikey.NewManager(cfg.Auth.JWTSecret)
	jwtAuth := auth.NewJWTAuth(cfg.Auth.JWTSecret, keyMgr)

	svc := &bunkerdService{
		cfg:     cfg,
		logger:  logger,
		tracker: tracker,
		keyMgr:  keyMgr,
		jwtAuth: jwtAuth,
		// Use a stub agent manager that returns a valid response without root.
		agentMgr: &stubAgentManager{
			resp: &v1.SpawnAgentResponse{
				AgentId:        "test-agent",
				DockerHostSsh:  "DOCKER_HOST=ssh://bunker-test-agent@localhost",
				SshPrivateKey:  "-----BEGIN OPENSSH PRIVATE KEY-----\nstub\n-----END OPENSSH PRIVATE KEY-----\n",
				PortRangeStart: 10000,
				PortRangeEnd:   10010,
				ExpiresAt:      time.Now().Add(time.Hour).Format(time.RFC3339),
			},
		},
	}

	req := connect.NewRequest(&v1.SpawnAgentRequest{AgentId: "test-agent"})
	resp, err := svc.SpawnAgent(context.Background(), req)
	if err != nil {
		t.Fatalf("SpawnAgent failed: %v", err)
	}
	if resp.Msg.ApiKey == "" {
		t.Fatal("expected ApiKey in spawn response when JWT auth is enabled")
	}

	// Validate the returned key through the interceptor.
	infoReq := connect.NewRequest(&v1.ServerInfoRequest{})
	infoReq.Header().Set("Authorization", "Bearer "+resp.Msg.ApiKey)
	_, err = jwtAuth.WrapUnary(func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
		claims, ok := auth.ClaimsFromContext(ctx)
		if !ok {
			t.Error("expected claims in context")
		}
		if claims.AgentID != "test-agent" {
			t.Errorf("expected agent_id test-agent, got %q", claims.AgentID)
		}
		return connect.NewResponse(&v1.ServerInfoResponse{}), nil
	})(context.Background(), infoReq)
	if err != nil {
		t.Fatalf("returned ApiKey failed auth: %v", err)
	}
}

// TestSpawnAgent_NoAPIKeyWhenAuthDisabled verifies no ApiKey is returned when
// auth is disabled.
func TestSpawnAgent_NoAPIKeyWhenAuthDisabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = false

	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	svc := &bunkerdService{
		cfg:     cfg,
		logger:  logger,
		tracker: tracker,
		agentMgr: &stubAgentManager{
			resp: &v1.SpawnAgentResponse{
				AgentId:        "test-agent-noauth",
				DockerHostSsh:  "DOCKER_HOST=ssh://bunker-test-agent-noauth@localhost",
				SshPrivateKey:  "-----BEGIN OPENSSH PRIVATE KEY-----\nstub\n-----END OPENSSH PRIVATE KEY-----\n",
				PortRangeStart: 10000,
				PortRangeEnd:   10010,
				ExpiresAt:      time.Now().Add(time.Hour).Format(time.RFC3339),
			},
		},
	}

	req := connect.NewRequest(&v1.SpawnAgentRequest{AgentId: "test-agent-noauth"})
	resp, err := svc.SpawnAgent(context.Background(), req)
	if err != nil {
		t.Fatalf("SpawnAgent failed: %v", err)
	}
	if resp.Msg.ApiKey != "" {
		t.Error("expected no ApiKey when auth is disabled")
	}
}

// stubAgentManager implements just enough of *agent.AgentManager for the
// service-layer SpawnAgent tests. It returns a canned response and ignores
// the request.
type stubAgentManager struct {
	resp *v1.SpawnAgentResponse
	err  error
}

func (m *stubAgentManager) Spawn(ctx context.Context, req *v1.SpawnAgentRequest) (*v1.SpawnAgentResponse, error) {
	return m.resp, m.err
}

func (m *stubAgentManager) Destroy(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
	return &v1.DestroyAgentResponse{AgentId: agentID, Status: "destroyed"}, nil
}

func (m *stubAgentManager) Stop() {}
