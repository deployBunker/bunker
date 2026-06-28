package resource

import (
	"log/slog"
	"os"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

func newTestTracker(t *testing.T, maxAgents uint32) *Tracker {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewTracker(maxAgents, logger)
}

func TestTracker_Register(t *testing.T) {
	tr := newTestTracker(t, 10)
	rec := &AgentRecord{AgentID: "test-001", Status: "running"}
	if err := tr.Register(rec); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if tr.Count() != 1 {
		t.Errorf("expected 1 agent, got %d", tr.Count())
	}
}

func TestTracker_Register_Duplicate(t *testing.T) {
	tr := newTestTracker(t, 10)
	rec := &AgentRecord{AgentID: "dup", Status: "running"}
	_ = tr.Register(rec)
	if err := tr.Register(rec); err == nil {
		t.Error("expected error for duplicate agent_id")
	}
}

func TestTracker_Register_CapacityFull(t *testing.T) {
	tr := newTestTracker(t, 2)
	_ = tr.Register(&AgentRecord{AgentID: "a1", Status: "running"})
	_ = tr.Register(&AgentRecord{AgentID: "a2", Status: "running"})
	if err := tr.Register(&AgentRecord{AgentID: "a3", Status: "running"}); err == nil {
		t.Error("expected capacity full error")
	}
}

func TestTracker_Unregister(t *testing.T) {
	tr := newTestTracker(t, 10)
	rec := &AgentRecord{AgentID: "remove-me", Status: "running"}
	_ = tr.Register(rec)
	tr.Unregister("remove-me")
	if tr.Count() != 0 {
		t.Errorf("expected 0 agents after unregister, got %d", tr.Count())
	}
	if tr.Get("remove-me") != nil {
		t.Error("expected nil after unregister")
	}
}

func TestTracker_Get_NotFound(t *testing.T) {
	tr := newTestTracker(t, 10)
	if tr.Get("nonexistent") != nil {
		t.Error("expected nil for nonexistent agent")
	}
}

func TestTracker_List(t *testing.T) {
	tr := newTestTracker(t, 10)
	_ = tr.Register(&AgentRecord{AgentID: "a1", Status: "running"})
	_ = tr.Register(&AgentRecord{AgentID: "a2", Status: "stopped"})
	agents := tr.List()
	if len(agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(agents))
	}
}

func TestTracker_HasCapacity(t *testing.T) {
	tr := newTestTracker(t, 3)
	if !tr.HasCapacity(3) {
		t.Error("expected HasCapacity(3) on empty tracker")
	}
	_ = tr.Register(&AgentRecord{AgentID: "a1"})
	if !tr.HasCapacity(2) {
		t.Error("expected HasCapacity(2) with 1 agent")
	}
	if tr.HasCapacity(3) {
		t.Error("expected !HasCapacity(3) with 1 agent (only 2 slots left)")
	}
}

func TestTracker_UpdateStatus(t *testing.T) {
	tr := newTestTracker(t, 10)
	_ = tr.Register(&AgentRecord{AgentID: "a1", Status: "starting"})
	tr.UpdateStatus("a1", "running")
	if rec := tr.Get("a1"); rec == nil || rec.Status != "running" {
		t.Errorf("expected status 'running', got %v", rec)
	}
}

func TestAgentRecord_ToAgentSummary(t *testing.T) {
	rec := &AgentRecord{
		AgentID:        "test-agent",
		Status:         "running",
		Limits:         &v1.ResourceLimits{CpuQuota: 2.0, MemoryMaxBytes: 4294967296},
		PortRangeStart: 10000,
		PortRangeEnd:   10100,
	}
	summary := rec.ToAgentSummary()
	if summary.AgentId != "test-agent" {
		t.Errorf("expected test-agent, got %q", summary.AgentId)
	}
	if summary.Status != "running" {
		t.Errorf("expected running, got %q", summary.Status)
	}
	if summary.Limits.CpuQuota != 2.0 {
		t.Errorf("expected cpu_quota 2.0, got %f", summary.Limits.CpuQuota)
	}
	if summary.PortRangeStart != 10000 {
		t.Errorf("expected port start 10000, got %d", summary.PortRangeStart)
	}
}
