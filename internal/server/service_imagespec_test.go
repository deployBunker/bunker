package server

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// newImageSpecTestService returns a bunkerdService with the real default
// config. The image-spec gate runs BEFORE anything that needs an agent
// manager, so a zero-value agentMgr suffices for rejection tests.
func newImageSpecTestService() *bunkerdService {
	return &bunkerdService{cfg: config.DefaultConfig()}
}

// TestSpawnAgent_InvalidImageSpecIsInvalidArgument pins the GAP-064 RPC
// contract: a spec the grammar rejects comes back as CodeInvalidArgument —
// not internal/unimplemented — and the manager is never reached.
func TestSpawnAgent_InvalidImageSpecIsInvalidArgument(t *testing.T) {
	svc := newImageSpecTestService()
	req := connect.NewRequest(&v1.SpawnAgentRequest{
		AgentId: "imgspec-bad",
		ImageSpec: &v1.ImageSpec{
			Packages: []*v1.PackageAdd{{Manager: "apt", Packages: []string{"curl|sh"}}},
		},
	})
	resp, err := svc.SpawnAgent(context.Background(), req)
	if err == nil {
		t.Fatalf("invalid spec accepted: %v", resp)
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want %s (err: %v)", connect.CodeOf(err), connect.CodeInvalidArgument, err)
	}
}

// TestSpawnAgent_UnknownBaseIsInvalidArgument covers the base-image policy at
// the RPC boundary.
func TestSpawnAgent_UnknownBaseIsInvalidArgument(t *testing.T) {
	svc := newImageSpecTestService()
	req := connect.NewRequest(&v1.SpawnAgentRequest{
		AgentId:   "imgspec-base",
		ImageSpec: &v1.ImageSpec{Base: "evil.example.com/rootkit:latest"},
	})
	_, err := svc.SpawnAgent(context.Background(), req)
	if err == nil {
		t.Fatal("unknown base accepted")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want invalid_argument", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "base") {
		t.Errorf("error should mention base image: %v", err)
	}
}

// TestSpawnAgent_DisabledFeatureIsInvalidArgument proves the feature flag
// rejects specs with the same RPC code.
func TestSpawnAgent_DisabledFeatureIsInvalidArgument(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agent.ImageSpec.Enabled = false
	svc := &bunkerdService{cfg: cfg}
	req := connect.NewRequest(&v1.SpawnAgentRequest{
		AgentId:   "imgspec-off",
		ImageSpec: &v1.ImageSpec{},
	})
	_, err := svc.SpawnAgent(context.Background(), req)
	if err == nil {
		t.Fatal("spec accepted while feature disabled")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want invalid_argument", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error should mention disabled feature: %v", err)
	}
}

// TestSpawnAgent_NoImageSpecUnaffected proves the gate is invisible to spawns
// without a spec: the error must come from the nil/empty agent manager
// plumbing, never from the image-spec gate itself.
func TestSpawnAgent_NoImageSpecUnaffected(t *testing.T) {
	cfg := config.DefaultConfig()
	logger := testDiscardLogger()
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	agentMgr := agent.NewAgentManager(cfg, logger, tracker, nil, nil)
	defer agentMgr.Stop()
	svc := &bunkerdService{cfg: cfg, logger: logger, agentMgr: agentMgr, tracker: tracker}

	req := connect.NewRequest(&v1.SpawnAgentRequest{AgentId: "imgspec-none"})
	_, err := svc.SpawnAgent(context.Background(), req)
	if err == nil {
		t.Skip("spawn unexpectedly succeeded on this host")
	}
	if strings.Contains(err.Error(), "image spec") {
		t.Errorf("no-spec spawn hit the image-spec gate: %v", err)
	}
}

// testDiscardLogger returns a logger that swallows output.
func testDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
