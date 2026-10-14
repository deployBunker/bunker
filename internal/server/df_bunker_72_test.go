package server

// DF-BUNKER-72 part 3, server side: the `bunker info` status surface carries
// the agent's rootless-docker health.
//
// Live-shaped regression: on the dogfood-21 incident the agent's docker daemon
// never came up (a recycled uid's stale rootlesskit state dir), and
// `bunker info` printed `Status: running` for the whole life of the agent —
// nothing on the status plane surfaced it. `bunker info` reads GetAgent, so
// that handler is where the qualification has to land.
//
// The probe itself is the SEAM the production wiring sets (server.go:
// agentMgr.DockerStatus), exactly like the DF-BUNKER-34 orphan probe above it:
// these tests prove the wiring and the additivity, not the systemd read.

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// dfb72Service builds a service holding one running agent.
func dfb72Service(t *testing.T, adjuster func(agentID, status string) string) *bunkerdService {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{AgentID: "df72c", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	return &bunkerdService{
		cfg:                  config.DefaultConfig(),
		logger:               logger,
		tracker:              tracker,
		agentMgr:             &fakeAgentManager{},
		dockerStatusAdjuster: adjuster,
	}
}

// TestGetAgent_DockerDegradedStatusRidesInfo covers criterion A3's server half:
// a failed docker.service probe yields the degraded status string on the
// `bunker info` surface, and a healthy probe leaves the status unchanged.
func TestGetAgent_DockerDegradedStatusRidesInfo(t *testing.T) {
	cases := []struct {
		name     string
		adjuster func(agentID, status string) string
		want     string
	}{
		{
			name:     "failed docker unit degrades the status",
			adjuster: func(agentID, status string) string { return status + " (docker degraded)" },
			want:     "running (docker degraded)",
		},
		{
			name:     "healthy docker leaves the status unchanged",
			adjuster: func(agentID, status string) string { return status },
			want:     "running",
		},
		{
			name:     "unwired service reports no verdict",
			adjuster: nil,
			want:     "running",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := dfb72Service(t, tc.adjuster)
			resp, err := svc.GetAgent(context.Background(), connect.NewRequest(&v1.GetAgentRequest{AgentId: "df72c"}))
			if err != nil {
				t.Fatalf("GetAgent: %v", err)
			}
			if got := resp.Msg.GetAgent().GetStatus(); got != tc.want {
				t.Errorf("GetAgent status = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGetAgent_DockerAdjusterSeesTheStoredStatus pins the interface the
// manager-side adjuster relies on: it is handed the agent's OWN stored status,
// not a hard-coded "running", so a stopped agent stays stopped when qualified.
func TestGetAgent_DockerAdjusterSeesTheStoredStatus(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{AgentID: "df72p", Status: "stopped"}); err != nil {
		t.Fatal(err)
	}
	seen := ""
	svc := &bunkerdService{
		cfg: config.DefaultConfig(), logger: logger, tracker: tracker, agentMgr: &fakeAgentManager{},
		dockerStatusAdjuster: func(agentID, status string) string {
			seen = status
			return status
		},
	}
	if _, err := svc.GetAgent(context.Background(), connect.NewRequest(&v1.GetAgentRequest{AgentId: "df72p"})); err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if seen != "stopped" {
		t.Errorf("adjuster saw status %q, want the agent's stored %q", seen, "stopped")
	}
}

// TestListAgents_DoesNotProbeDockerHealth is the "do not slow the info path"
// half of the contract: the LIST surface is the polled one, so it must not pay
// a per-agent subprocess. Only `bunker info` (GetAgent) goes through the
// adjuster.
func TestListAgents_DoesNotProbeDockerHealth(t *testing.T) {
	calls := 0
	svc := dfb72Service(t, func(agentID, status string) string {
		calls++
		return status
	})
	if _, err := svc.ListAgents(context.Background(), connect.NewRequest(&v1.ListAgentsRequest{})); err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if calls != 0 {
		t.Errorf("ListAgents made %d docker-health probe calls, want 0 (the polled list path must stay probe-free)", calls)
	}
}
