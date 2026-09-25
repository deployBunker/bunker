package server

// Tests for the PERF-001 disk-usage snapshot cache (internal/server/diskusage.go).
//
// The walker and the clock are seams (package vars diskUsageWalk /
// diskUsageNow): every test here counts walks through the seam and drives TTL
// expiry with a fake clock, so nothing sleeps and no real agent home is
// touched. These tests deliberately do not use t.Parallel: the seams are
// package-level and swapping them is not safe across parallel tests.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

const perf001AgentID = "perf001-a"

// perf001Fixture swaps in the counting walker and the fake clock and returns
// the handles the tests use. Cleanup restores both seams.
type perf001Fixture struct {
	svc     *bunkerdService
	tracker *resource.Tracker
	walks   *int32
	clock   *time.Time
}

// swapFreshCache installs a brand-new cache for this test (the production
// var is package-level and shared by every test in the binary) and restores
// the previous one on cleanup.
func swapFreshCache(t *testing.T) {
	t.Helper()
	orig := agentDiskUsageCache
	agentDiskUsageCache = &diskUsageCache{ttl: diskUsageTTL}
	t.Cleanup(func() { agentDiskUsageCache = orig })
}

func newPerf001Fixture(t *testing.T) *perf001Fixture {
	t.Helper()
	swapFreshCache(t)
	logger := testDiscardLogger()
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{AgentID: perf001AgentID, Status: "running"}); err != nil {
		t.Fatalf("register %s: %v", perf001AgentID, err)
	}
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker}

	fix := &perf001Fixture{
		svc:     svc,
		tracker: tracker,
		walks:   new(int32),
		clock:   new(time.Time),
	}
	*fix.clock = time.Unix(1_700_000_000, 0)

	origWalk, origNow := diskUsageWalk, diskUsageNow
	diskUsageWalk = func(string) uint64 {
		atomic.AddInt32(fix.walks, 1)
		return 4242
	}
	diskUsageNow = func() time.Time { return *fix.clock }
	t.Cleanup(func() {
		diskUsageWalk, diskUsageNow = origWalk, origNow
	})
	return fix
}

// AC1 (PERF-001): the SECOND ListAgents/AgentMetrics call inside the TTL
// window must perform ZERO walks — the snapshot is served from the cache.
// Pre-fix every request walked the agent home, so the walk-count assertions
// below fail (that is the committed RED proof).
func TestDiskUsageCache_SecondRequestInsideTTLPerformsZeroWalks(t *testing.T) {
	fix := newPerf001Fixture(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		resp, err := fix.svc.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
		if err != nil {
			t.Fatalf("ListAgents call %d: %v", i+1, err)
		}
		if got := resp.Msg.Agents[0].DiskUsedBytes; got != 4242 {
			t.Fatalf("ListAgents call %d reported DiskUsedBytes=%d, want 4242", i+1, got)
		}
	}
	if got := atomic.LoadInt32(fix.walks); got != 1 {
		t.Fatalf("two ListAgents calls performed %d walks, want 1 (second call must serve the snapshot, not re-walk)", got)
	}

	for i := 0; i < 2; i++ {
		resp, err := fix.svc.AgentMetrics(ctx, connect.NewRequest(&v1.AgentMetricsRequest{AgentId: perf001AgentID}))
		if err != nil {
			t.Fatalf("AgentMetrics call %d: %v", i+1, err)
		}
		if got := resp.Msg.DiskUsedBytes; got != 4242 {
			t.Fatalf("AgentMetrics call %d reported DiskUsedBytes=%d, want 4242", i+1, got)
		}
	}
	if got := atomic.LoadInt32(fix.walks); got != 1 {
		t.Fatalf("two ListAgents + two AgentMetrics calls performed %d walks, want 1 (requests inside the TTL window must not walk)", got)
	}
}

// AC2 (PERF-001): after TTL expiry the next call refreshes and the reported
// value updates — the staleness bound is the TTL, not forever.
func TestDiskUsageCache_TTLExpiryTriggersRefresh(t *testing.T) {
	fix := newPerf001Fixture(t)
	ctx := context.Background()

	resp, err := fix.svc.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if got := resp.Msg.Agents[0].DiskUsedBytes; got != 4242 {
		t.Fatalf("first call reported DiskUsedBytes=%d, want 4242", got)
	}
	if got := atomic.LoadInt32(fix.walks); got != 1 {
		t.Fatalf("first call performed %d walks, want 1", got)
	}

	// Still inside the TTL: snapshot served, no new walk.
	*fix.clock = fix.clock.Add(4 * time.Minute)
	resp, err = fix.svc.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents inside TTL: %v", err)
	}
	if got := resp.Msg.Agents[0].DiskUsedBytes; got != 4242 {
		t.Fatalf("inside-TTL call reported DiskUsedBytes=%d, want 4242", got)
	}
	if got := atomic.LoadInt32(fix.walks); got != 1 {
		t.Fatalf("inside-TTL call performed %d walks total, want 1", got)
	}

	// Past the TTL (5 minutes): the next call refreshes and reports the NEW
	// value the walker now returns.
	*fix.clock = fix.clock.Add(1*time.Minute + time.Second)
	diskUsageWalk = func(string) uint64 {
		atomic.AddInt32(fix.walks, 1)
		return 9000
	}
	resp, err = fix.svc.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents after expiry: %v", err)
	}
	if got := resp.Msg.Agents[0].DiskUsedBytes; got != 9000 {
		t.Fatalf("after TTL expiry ListAgents reported DiskUsedBytes=%d, want 9000 (expired entry must refresh)", got)
	}
	if got := atomic.LoadInt32(fix.walks); got != 2 {
		t.Fatalf("after TTL expiry walks=%d, want 2 (exactly one refresh walk)", got)
	}

	// The refreshed entry is inside its new TTL: no further walk.
	resp, err = fix.svc.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents after refresh: %v", err)
	}
	if got := resp.Msg.Agents[0].DiskUsedBytes; got != 9000 {
		t.Fatalf("post-refresh call reported DiskUsedBytes=%d, want 9000", got)
	}
	if got := atomic.LoadInt32(fix.walks); got != 2 {
		t.Fatalf("post-refresh walks=%d, want 2 (refreshed entry must be served, not re-walked)", got)
	}
}

// PERF-001 zero-growth: entries for agents that disappear from the tracker
// are dropped, so a re-registered agent id is walked again instead of being
// served a snapshot captured for the destroyed home.
func TestDiskUsageCache_PruneDropsEntriesForVanishedAgents(t *testing.T) {
	fix := newPerf001Fixture(t)
	ctx := context.Background()

	// Populate the cache for perf001-a.
	if _, err := fix.svc.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{})); err != nil {
		t.Fatalf("ListAgents populate: %v", err)
	}
	if got := atomic.LoadInt32(fix.walks); got != 1 {
		t.Fatalf("populate walk count=%d, want 1", got)
	}

	// The agent disappears from the tracker; the next ListAgents prunes it.
	fix.tracker.Unregister(perf001AgentID)
	if _, err := fix.svc.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{})); err != nil {
		t.Fatalf("ListAgents after unregister: %v", err)
	}

	// Re-register the same id: the old snapshot must be gone (fresh walk).
	if err := fix.tracker.Register(&resource.AgentRecord{AgentID: perf001AgentID, Status: "running"}); err != nil {
		t.Fatalf("re-register %s: %v", perf001AgentID, err)
	}
	resp, err := fix.svc.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents after re-register: %v", err)
	}
	if got := atomic.LoadInt32(fix.walks); got != 2 {
		t.Fatalf("after destroy+re-register walks=%d, want 2 (snapshot for the vanished agent must be pruned)", got)
	}
	if got := resp.Msg.Agents[0].DiskUsedBytes; got != 4242 {
		t.Fatalf("after re-register reported DiskUsedBytes=%d, want 4242", got)
	}
}

// AC3 (PERF-001): concurrent readers must not issue concurrent walks (per-
// agent single-flight) and the cache must be race-clean. Exactly one walk is
// expected: the first caller's, shared with everyone who arrives while it is
// in flight; everyone after it is served from the fresh entry. The -race run
// over the package is the concurrency-safety proof.
func TestDiskUsageCache_ConcurrentRequestsShareOneWalk(t *testing.T) {
	logger := testDiscardLogger()
	tracker := resource.NewTracker(100, logger)
	const raceAgentID = "perf001-race"
	if err := tracker.Register(&resource.AgentRecord{AgentID: raceAgentID, Status: "running"}); err != nil {
		t.Fatalf("register %s: %v", raceAgentID, err)
	}
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker}
	swapFreshCache(t)

	var walks int32
	origWalk, origNow := diskUsageWalk, diskUsageNow
	diskUsageWalk = func(string) uint64 {
		atomic.AddInt32(&walks, 1)
		return 4242
	}
	diskUsageNow = time.Now
	t.Cleanup(func() {
		diskUsageWalk, diskUsageNow = origWalk, origNow
	})

	const goroutines, calls = 16, 25
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < calls; i++ {
				if g%2 == 0 {
					resp, err := svc.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
					if err != nil {
						errCh <- fmt.Errorf("goroutine %d ListAgents: %w", g, err)
						return
					}
					if got := resp.Msg.Agents[0].DiskUsedBytes; got != 4242 {
						errCh <- fmt.Errorf("goroutine %d ListAgents reported DiskUsedBytes=%d, want 4242", g, got)
						return
					}
				} else {
					resp, err := svc.AgentMetrics(ctx, connect.NewRequest(&v1.AgentMetricsRequest{AgentId: raceAgentID}))
					if err != nil {
						errCh <- fmt.Errorf("goroutine %d AgentMetrics: %w", g, err)
						return
					}
					if got := resp.Msg.DiskUsedBytes; got != 4242 {
						errCh <- fmt.Errorf("goroutine %d AgentMetrics reported DiskUsedBytes=%d, want 4242", g, got)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&walks); got != 1 {
		t.Fatalf("concurrent readers performed %d walks, want 1 (single-flight must collapse refreshes)", got)
	}
}
