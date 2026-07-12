package server

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

type runAgentMockManager struct {
	spawnCalled   bool
	destroyCalled bool
	runCalled     bool
	runResp       *v1.RunAgentResponse
	runErr        error
}

func (m *runAgentMockManager) Spawn(ctx context.Context, req *v1.SpawnAgentRequest) (*v1.SpawnAgentResponse, error) {
	m.spawnCalled = true
	return nil, nil
}

func (m *runAgentMockManager) Destroy(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
	m.destroyCalled = true
	return nil, nil
}

func (m *runAgentMockManager) RunAgent(ctx context.Context, req *v1.RunAgentRequest) (*v1.RunAgentResponse, error) {
	m.runCalled = true
	if m.runErr != nil {
		return nil, m.runErr
	}
	return m.runResp, nil
}

func (m *runAgentMockManager) Stop() {}

func TestRunAgent_Detached(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	tracker := resource.NewTracker(10, logger)
	tracker.Register(&resource.AgentRecord{AgentID: "agent-1", Limits: &v1.ResourceLimits{}})

	mock := &runAgentMockManager{
		runResp: &v1.RunAgentResponse{
			RunId:    "run-123",
			Status:   "running",
			UnitName: "bunker-run-agent-1-run-123",
		},
	}

	svc := &bunkerdService{
		cfg:      config.DefaultConfig(),
		logger:   logger,
		agentMgr: mock,
		tracker:  tracker,
	}

	req := connect.NewRequest(&v1.RunAgentRequest{
		AgentId: "agent-1",
		Command: "docker",
		Args:    []string{"compose", "up"},
		Detach:  true,
	})
	resp, err := svc.RunAgent(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mock.runCalled {
		t.Fatal("expected RunAgent to be called on manager")
	}
	if resp.Msg.GetRunId() != "run-123" {
		t.Errorf("expected run_id run-123, got %q", resp.Msg.GetRunId())
	}
	if resp.Msg.GetUnitName() != "bunker-run-agent-1-run-123" {
		t.Errorf("expected unit bunker-run-agent-1-run-123, got %q", resp.Msg.GetUnitName())
	}
}

func TestRunAgent_RequiresDetach(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	tracker := resource.NewTracker(10, logger)
	tracker.Register(&resource.AgentRecord{AgentID: "agent-1", Limits: &v1.ResourceLimits{}})

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.RunAgentRequest{
		AgentId: "agent-1",
		Command: "docker",
		Detach:  false,
	})
	_, err := svc.RunAgent(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for non-detached request")
	}
}

func TestRunAgent_AgentNotFound(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.RunAgentRequest{
		AgentId: "missing",
		Command: "docker",
		Detach:  true,
	})
	_, err := svc.RunAgent(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
}

func TestRunAgent_MissingAgentID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.RunAgentRequest{
		Command: "docker",
		Detach:  true,
	})
	_, err := svc.RunAgent(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing agent_id")
	}
}

func TestRunAgent_MissingCommand(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	tracker := resource.NewTracker(10, logger)
	tracker.Register(&resource.AgentRecord{AgentID: "agent-1", Limits: &v1.ResourceLimits{}})

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.RunAgentRequest{
		AgentId: "agent-1",
		Detach:  true,
	})
	_, err := svc.RunAgent(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing command")
	}
}
