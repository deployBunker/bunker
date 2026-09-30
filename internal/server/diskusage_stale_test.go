package server

// PERF-010 tests: the disk-usage cache must NEVER block a request on a walk.
//
// The walker seam (diskUsageWalk) is replaced by a walker that BLOCKS on a
// channel until the test releases it, so "the call returned" is proof the
// call did not include the walk — no timing assertions, no sleeps. The fake
// clock seam (diskUsageNow) drives TTL expiry deterministically. These tests
// deliberately do not use t.Parallel: the seams are package-level.
//
// waitDiskUsageRefresh makes the async hand-off deterministic: the flight's
// done channel is closed strictly AFTER the refreshed entry is published, so
// once it returns (or no flight exists) the next usageFor call is guaranteed
// to observe the refreshed value.

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitDiskUsageRefresh blocks until any in-flight background refresh for
// agentID has published its entry and closed its flight. Safe to call when
// no refresh is queued: publication happens before the flight is removed
// from the map, so an absent flight means the refresh (if any) already
// completed.
func waitDiskUsageRefresh(c *diskUsageCache, agentID string) {
	c.mu.Lock()
	f := c.inFlight[agentID]
	c.mu.Unlock()
	if f != nil {
		<-f.done
	}
}

// perf010Fixture is the cache-level fixture: a private cache (no service),
// the fake clock, walk bookkeeping, and the blocked-walker channels.
type perf010Fixture struct {
	cache   *diskUsageCache
	walks   *int32
	clock   *time.Time
	started chan struct{} // closed by the walker when a walk begins
	release chan struct{} // the walker blocks here until closed
}

func newPerf010Fixture(t *testing.T) *perf010Fixture {
	t.Helper()
	origWalk, origNow := diskUsageWalk, diskUsageNow
	t.Cleanup(func() {
		diskUsageWalk, diskUsageNow = origWalk, origNow
	})
	fix := &perf010Fixture{
		cache:   &diskUsageCache{ttl: diskUsageTTL},
		walks:   new(int32),
		clock:   new(time.Time),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	*fix.clock = time.Unix(1_700_000_000, 0)
	diskUsageNow = func() time.Time { return *fix.clock }
	return fix
}

// awaitWalk deterministically waits for the background walk to actually
// begin: the walker closes started as its first act, so reading the counter
// is ordered after the walk's start rather than racing it. This closes the
// loaded-scheduler window that hit QA-BUNKER-40 (all 16 callers had returned
// while the freshly spawned walk goroutine was still unscheduled — walks read
// 0). Blocking forever is correct here: the walk is spawned unconditionally
// before callers return, so a timeout would only ever misfire under a loaded
// scheduler (the exact defect) by abandoning a walk that is about to start;
// go test -timeout remains the ultimate bound.
func (fix *perf010Fixture) awaitWalk() {
	<-fix.started
}

// blockWalk installs the walker every PERF-010 test uses: it counts itself,
// announces it started, then blocks until the test releases it. A call that
// returns while the walker is still blocked therefore did not wait for it.
func (fix *perf010Fixture) blockWalk(t *testing.T) {
	t.Helper()
	diskUsageWalk = func(string) uint64 {
		atomic.AddInt32(fix.walks, 1)
		close(fix.started)
		<-fix.release
		return 9999
	}
}

// seed plants a snapshot entry aged by the given duration directly into the
// cache (age 0 = freshly refreshed; age > ttl = expired).
func (fix *perf010Fixture) seed(agentID string, bytes uint64, age time.Duration) {
	fix.cache.mu.Lock()
	fix.cache.entries = map[string]diskUsageEntry{
		agentID: {bytes: bytes, refreshed: diskUsageNow().Add(-age)},
	}
	fix.cache.mu.Unlock()
}

// AC (PERF-010 a): on an expired entry usageFor returns the STALE value
// immediately — provably before the walk completes, because the walk is
// blocked on a channel when the call returns. On a first-ever miss it
// returns the 0 sentinel (the value callers already tolerate: best-effort,
// hidden when 0, the same value a failed walk always produced). A fresh
// entry is served directly with no walk at all.
func TestDiskUsageCacheStale_UsageForReturnsBeforeWalkCompletes(t *testing.T) {
	cases := []struct {
		name     string
		seeded   bool
		seedAge  time.Duration // age of the seeded entry (0 = fresh)
		want     uint64        // the value the call must return
		wantWalk bool          // a background walk must be queued
	}{
		{"fresh entry served directly without a walk", true, 0, 7777, false},
		{"expired entry serves stale immediately", true, diskUsageTTL + time.Second, 7777, true},
		{"first-ever miss serves the 0 sentinel", false, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fix := newPerf010Fixture(t)
			const id = "perf010-stale"
			if tc.seeded {
				fix.seed(id, 7777, tc.seedAge)
			}
			fix.blockWalk(t)

			got := fix.cache.usageFor(id) // must return BEFORE the walk completes

			if got != tc.want {
				t.Fatalf("usageFor returned %d, want %d (the call must not include the walk's latency)", got, tc.want)
			}
			if !tc.wantWalk {
				// No walk may be queued for a fresh entry.
				select {
				case <-fix.started:
					t.Fatal("fresh entry triggered a walk; it must be served from the snapshot")
				default:
				}
				for i := 0; i < 3; i++ {
					runtime.Gosched()
				}
				if got := atomic.LoadInt32(fix.walks); got != 0 {
					t.Fatalf("walks=%d, want 0 for a fresh entry", got)
				}
				close(fix.release) // cleanup safety; nothing should be blocked on it
				return
			}
			// The call already returned; the walk WAS queued and is running
			// in the background, still blocked on release.
			<-fix.started
			if got := atomic.LoadInt32(fix.walks); got != 1 {
				t.Fatalf("walks=%d, want 1 (exactly one background walk queued)", got)
			}
			close(fix.release)
			waitDiskUsageRefresh(fix.cache, id) // don't leak the goroutine into cleanup
		})
	}
}

// AC (PERF-010 b): after the background refresh is unblocked and publishes,
// a subsequent call returns the NEW value — and only one walk was ever paid
// for the window (the refreshed entry is served, not re-walked).
func TestDiskUsageCacheStale_BackgroundRefreshLandsForSubsequentCalls(t *testing.T) {
	fix := newPerf010Fixture(t)
	const id = "perf010-refresh"
	fix.seed(id, 7777, diskUsageTTL+time.Second)
	fix.blockWalk(t)

	if got := fix.cache.usageFor(id); got != 7777 {
		t.Fatalf("expired call returned %d, want stale 7777", got)
	}
	<-fix.started
	close(fix.release)                  // let the background walk finish
	waitDiskUsageRefresh(fix.cache, id) // publish happens-before close(done)

	if got := fix.cache.usageFor(id); got != 9999 {
		t.Fatalf("post-refresh call returned %d, want 9999 (the refreshed value must land in the cache)", got)
	}
	if got := fix.cache.usageFor(id); got != 9999 {
		t.Fatalf("second post-refresh call returned %d, want 9999", got)
	}
	if got := atomic.LoadInt32(fix.walks); got != 1 {
		t.Fatalf("walks=%d, want 1 (a walk never runs more than once per agent per TTL window)", got)
	}
}

// AC (PERF-010 c): concurrent callers hitting an expired entry must trigger
// exactly ONE walk (single-flight dedupe) and every one of them must get the
// stale value without waiting — proven by counting walk invocations while
// the single walk is still BLOCKED after all callers have returned.
func TestDiskUsageCacheStale_ConcurrentExpiredCallersTriggerExactlyOneWalk(t *testing.T) {
	fix := newPerf010Fixture(t)
	const id = "perf010-singleflight"
	fix.seed(id, 7777, diskUsageTTL+time.Second)
	fix.blockWalk(t)

	const goroutines = 16
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := fix.cache.usageFor(id); got != 7777 {
				errCh <- fmt.Errorf("concurrent caller returned %d, want stale 7777 without blocking on the walk", got)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	// All callers have returned while the walk is still blocked on release:
	// exactly one walk was started, none duplicated it, none waited on it.
	// Count only AFTER deterministically awaiting the walk: all callers
	// returning does not imply the background walk goroutine has been
	// scheduled yet — on a loaded box (QA-BUNKER-40) it was still sitting
	// in the run queue and walks read 0. The walker closes fix.started as
	// its first act, so this await is a happens-before edge on the real
	// synchronization, not a sleep. It weakens nothing: when awaitWalk
	// returns, the walk has begun but is still parked on fix.release (it
	// cannot have completed or published), and a caller that had blocked
	// on the walk could never have reached wg.Wait's return.
	fix.awaitWalk()
	if got := atomic.LoadInt32(fix.walks); got != 1 {
		t.Fatalf("walks=%d while the walk is still in flight, want exactly 1 (single-flight must dedupe expired callers)", got)
	}
	close(fix.release)
	waitDiskUsageRefresh(fix.cache, id)

	if got := fix.cache.usageFor(id); got != 9999 {
		t.Fatalf("post-refresh call returned %d, want 9999", got)
	}
}
