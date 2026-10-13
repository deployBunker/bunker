package server

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── GAP-121: resource-event wiring (server side) ────────────────────────────

func eventTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestFillAgentResourceEvents_UnresolvedUserIsUnavailableNotClean proves the
// degradation contract: when the agent's Linux account cannot be resolved the
// response must say "unavailable", never present zero counters as a measured
// clean bill of health, and never read user-0's slice as if it were the agent.
func TestFillAgentResourceEvents_UnresolvedUserIsUnavailableNotClean(t *testing.T) {
	resp := &v1.AgentMetricsResponse{AgentId: "ghost"}
	fillAgentResourceEvents(resp, -1, "ghost", nil, eventTestLogger())

	ev := resp.GetResourceEvents()
	if ev == nil {
		t.Fatal("resource_events is nil for an unresolved agent user — a client cannot distinguish absent from clean")
	}
	if ev.GetAvailable() {
		t.Error("Available = true while the agent user did not resolve")
	}
	if !strings.Contains(ev.GetDetail(), "ghost") {
		t.Errorf("Detail = %q, want it to name the unresolved account", ev.GetDetail())
	}
	if ev.GetMemory().GetAvailable() || ev.GetPids().GetAvailable() {
		t.Error("event counters reported available while the cgroup was never located")
	}
}

func TestFillAgentResourceEvents_NilResponseIsSafe(t *testing.T) {
	fillAgentResourceEvents(nil, -1, "ghost", nil, nil) // must not panic
}

func TestFillAgentResourceEvents_NilTrackerDoesNotPanic(t *testing.T) {
	resp := &v1.AgentMetricsResponse{AgentId: "agent-x"}
	// uid -1 short-circuits before any read; a nil tracker must also be safe
	// on the observe path (PeakTracker's methods are nil-receiver safe).
	fillAgentResourceEvents(resp, -1, "agent-x", nil, nil)
	if resp.GetResourceEvents() == nil {
		t.Fatal("resource_events missing")
	}
}

// TestFillAgentResourceEvents_MergesDaemonPeakWhenCgroupIsGone proves the two
// peak sources are independent: even with no readable cgroup (the counter
// block is unavailable), a peak the daemon previously sampled is still
// reported — labelled as the daemon's, never as the kernel's.
func TestFillAgentResourceEvents_MergesDaemonPeakWhenCgroupIsGone(t *testing.T) {
	peaks := resource.NewPeakTracker()
	peaks.Observe("agent-x", 5*1024*1024, 44)

	resp := &v1.AgentMetricsResponse{AgentId: "agent-x"}
	// A uid with no matching user.slice: the read finds no cgroup files.
	fillAgentResourceEvents(resp, 999999999, "agent-x", peaks, eventTestLogger())

	ev := resp.GetResourceEvents()
	if ev.GetAvailable() {
		t.Fatal("Available = true for a cgroup that does not exist")
	}
	if ev.GetMemoryPeakBytes() != 5*1024*1024 {
		t.Errorf("memory peak = %d, want the daemon-observed 5MiB", ev.GetMemoryPeakBytes())
	}
	if ev.GetMemoryPeakSource() != resource.PeakSourceDaemon {
		t.Errorf("memory peak source = %q, want %q", ev.GetMemoryPeakSource(), resource.PeakSourceDaemon)
	}
	if ev.GetPidsPeak() != 44 || ev.GetPidsPeakSource() != resource.PeakSourceDaemon {
		t.Errorf("pids peak = (%d, %q), want (44, %q)", ev.GetPidsPeak(), ev.GetPidsPeakSource(), resource.PeakSourceDaemon)
	}
	if ev.GetDetail() == "" {
		t.Error("Detail is empty while the cgroup could not be read")
	}
}

// ── sampler ─────────────────────────────────────────────────────────────────

// withStubbedSamplingSeams swaps the uid resolver and the usage reader for the
// duration of a test.
func withStubbedSamplingSeams(t *testing.T, uidOf func(string) (int, bool), read func(int, string) (uint64, uint64, bool)) {
	t.Helper()
	origUID, origRead := agentUIDLookup, readAgentUsageFn
	t.Cleanup(func() { agentUIDLookup, readAgentUsageFn = origUID, origRead })
	agentUIDLookup = uidOf
	readAgentUsageFn = read
}

func TestSampleAgentResourcePeaks_ObservesRunningAgentsOnly(t *testing.T) {
	logger := eventTestLogger()
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{AgentID: "live", Status: "running"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := tracker.Register(&resource.AgentRecord{AgentID: "stopped", Status: "stopped"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	withStubbedSamplingSeams(t,
		func(string) (int, bool) { return 1001, true },
		func(_ int, agentID string) (uint64, uint64, bool) {
			if agentID == "live" {
				return 8 * 1024 * 1024, 12, true
			}
			return 0, 0, true
		},
	)

	peaks := resource.NewPeakTracker()
	if got := sampleAgentResourcePeaks(tracker, peaks); got != 1 {
		t.Errorf("observed %d agents, want 1 (only the running one)", got)
	}
	mem, pids, ok := peaks.Peaks("live")
	if !ok || mem != 8*1024*1024 || pids != 12 {
		t.Errorf("live peaks = (%d, %d, %v), want (8MiB, 12, true)", mem, pids, ok)
	}
	if _, _, ok := peaks.Peaks("stopped"); ok {
		t.Error("a stopped agent kept a daemon peak — its cgroup is gone, so the peak must not survive as if live")
	}
}

func TestSampleAgentResourcePeaks_PrunesDestroyedAgents(t *testing.T) {
	logger := eventTestLogger()
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{AgentID: "live", Status: "running"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	peaks := resource.NewPeakTracker()
	peaks.Observe("destroyed-long-ago", 1, 1)
	peaks.Observe("live", 2, 2)

	withStubbedSamplingSeams(t,
		func(string) (int, bool) { return 1001, true },
		func(int, string) (uint64, uint64, bool) { return 0, 0, true },
	)
	sampleAgentResourcePeaks(tracker, peaks)

	if peaks.Len() != 1 {
		t.Fatalf("tracked peaks = %d after a pass, want 1 — the tracker must stay bounded by the live set", peaks.Len())
	}
	if _, _, ok := peaks.Peaks("destroyed-long-ago"); ok {
		t.Error("a destroyed agent's peak survived the sampler pass")
	}
}

func TestSampleAgentResourcePeaks_UnresolvableUserDropsPeak(t *testing.T) {
	logger := eventTestLogger()
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{AgentID: "live", Status: "running"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	peaks := resource.NewPeakTracker()
	peaks.Observe("live", 4096, 4)

	withStubbedSamplingSeams(t,
		func(string) (int, bool) { return 0, false }, // the user is gone
		func(int, string) (uint64, uint64, bool) { return 1, 1, true },
	)
	sampleAgentResourcePeaks(tracker, peaks)

	if _, _, ok := peaks.Peaks("live"); ok {
		t.Error("a peak survived for an agent whose user no longer resolves")
	}
}

func TestSampleAgentResourcePeaks_NilInputs(t *testing.T) {
	if got := sampleAgentResourcePeaks(nil, resource.NewPeakTracker()); got != 0 {
		t.Errorf("nil tracker observed %d, want 0", got)
	}
	if got := sampleAgentResourcePeaks(resource.NewTracker(1, eventTestLogger()), nil); got != 0 {
		t.Errorf("nil peaks observed %d, want 0", got)
	}
}

// TestStartPeakSampler_ExitsOnContextCancel proves the sampler's lifetime is
// the context's: it must stop when cancelled, so a daemon shutdown (or a test)
// cannot leave a goroutine sampling a host forever.
func TestStartPeakSampler_ExitsOnContextCancel(t *testing.T) {
	logger := eventTestLogger()
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{AgentID: "live", Status: "running"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	var calls atomic.Int64
	withStubbedSamplingSeams(t,
		func(string) (int, bool) { return 1001, true },
		func(int, string) (uint64, uint64, bool) {
			calls.Add(1)
			return 1024, 2, true
		},
	)

	origInterval := peakSampleInterval
	peakSampleInterval = 2 * time.Millisecond
	defer func() { peakSampleInterval = origInterval }()

	ctx, cancel := context.WithCancel(context.Background())
	startPeakSampler(ctx, tracker, resource.NewPeakTracker(), logger)

	// Wait for at least one pass (the sampler runs its first pass immediately).
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("the sampler never ran a pass")
	}

	cancel()
	// Give an in-flight iteration time to finish, then prove no further passes.
	time.Sleep(20 * time.Millisecond)
	settled := calls.Load()
	time.Sleep(40 * time.Millisecond)
	if got := calls.Load(); got != settled {
		t.Errorf("sampler kept running after cancel: %d -> %d passes", settled, got)
	}
}

func TestStartPeakSampler_NilInputsStartNoGoroutine(t *testing.T) {
	// Nothing to sample: the function must return without starting anything.
	startPeakSampler(context.Background(), nil, resource.NewPeakTracker(), eventTestLogger())
	startPeakSampler(context.Background(), resource.NewTracker(1, eventTestLogger()), nil, eventTestLogger())
}
