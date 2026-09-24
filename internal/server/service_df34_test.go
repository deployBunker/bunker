package server

// DF-BUNKER-34 server-side tests: the orphan-uid probe rides the info/list
// surfaces, and the renewal-drift RPC is wired to the manager.

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// TestListAgents_OrphanUIDDetailRidesSummary proves the list surface carries
// the manager's orphan verdict through the wire field unchanged (and an
// empty verdict stays empty — no fabricated detail).
func TestListAgents_OrphanUIDDetailRidesSummary(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{AgentID: "orphan-a", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Register(&resource.AgentRecord{AgentID: "healthy-b", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	svc := &bunkerdService{
		cfg:      config.DefaultConfig(),
		logger:   logger,
		tracker:  tracker,
		agentMgr: &fakeAgentManager{},
		// The probe is the SEAM the production wiring sets: here a stub so
		// the test proves the wiring, not the /proc read.
		orphanUIDSummarizer: func(agentID string) string {
			if agentID == "orphan-a" {
				return "ORPHANED UID: user record bunker-orphan-a is GONE from the host but 1 live process(es) under uid 1002: pid 477038: python offbyone_forward.py"
			}
			return ""
		},
	}

	resp, err := svc.ListAgents(context.Background(), connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	byID := map[string]*v1.AgentSummary{}
	for _, a := range resp.Msg.GetAgents() {
		byID[a.GetAgentId()] = a
	}
	got := byID["orphan-a"].GetOrphanUidDetail()
	if !strings.Contains(got, "ORPHANED UID") || !strings.Contains(got, "pid 477038") {
		t.Errorf("list summary for orphan-a = %q, want the manager's orphan verdict", got)
	}
	if got := byID["healthy-b"].GetOrphanUidDetail(); got != "" {
		t.Errorf("list summary for healthy-b = %q, want empty (absence must never fabricate a verdict)", got)
	}
}

// TestGetAgent_OrphanUIDDetailRidesSummary proves the info surface carries
// the same verdict for ONE agent.
func TestGetAgent_OrphanUIDDetailRidesSummary(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{AgentID: "zomb", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	svc := &bunkerdService{
		cfg:                 config.DefaultConfig(),
		logger:              logger,
		tracker:             tracker,
		agentMgr:            &fakeAgentManager{},
		orphanUIDSummarizer: func(string) string { return "ORPHANED UID: evidence" },
	}

	resp, err := svc.GetAgent(context.Background(), connect.NewRequest(&v1.GetAgentRequest{AgentId: "zomb"}))
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got := resp.Msg.GetAgent().GetOrphanUidDetail(); !strings.Contains(got, "ORPHANED UID") {
		t.Errorf("info summary orphan detail = %q, want the ORPHANED UID verdict", got)
	}

	// No summarizer wired (unmanaged service): no detail, no probe.
	svc2 := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker, agentMgr: &fakeAgentManager{}}
	resp2, err := svc2.GetAgent(context.Background(), connect.NewRequest(&v1.GetAgentRequest{AgentId: "zomb"}))
	if err != nil {
		t.Fatalf("GetAgent (unwired): %v", err)
	}
	if got := resp2.Msg.GetAgent().GetOrphanUidDetail(); got != "" {
		t.Errorf("unwired service carried orphan detail %q, want empty", got)
	}
}

// TestRenewalDriftReport_UnimplementedWithoutManager proves the drift RPC
// degrades with a NAMED error on a service whose manager predates the
// method (the fakeAgentManager), never a fabricated empty report.
func TestRenewalDriftReport_UnimplementedWithoutManager(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: resource.NewTracker(10, logger), agentMgr: &fakeAgentManager{}}

	_, err := svc.RenewalDriftReport(context.Background(), connect.NewRequest(&v1.RenewalDriftRequest{
		AgentId: "x", OldHome: "/home/bunker-x",
	}))
	if err == nil {
		t.Fatal("drift RPC on a manager without the probe must fail")
	}
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("code = %v, want Unimplemented", connect.CodeOf(err))
	}
}
