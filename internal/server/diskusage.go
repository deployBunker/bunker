package server

// PERF-001: ListAgents and AgentMetrics used to walk every agent home
// (filepath.WalkDir + d.Info() per file) synchronously on every request —
// measured at 7.2s per ListAgents for one ~804k-file home on cube-las-00,
// paid by every `bunker list`, scheduler poll and QA preflight.
//
// This file adds a per-agent disk-usage snapshot cache. Requests inside the
// TTL window serve the snapshot with ZERO walking; an absent or expired
// entry is refreshed by exactly one walk per agent id (per-agent single-
// flight: concurrent callers share the in-flight walk's result), and that
// refresh blocks only the requests that need it — a walk never runs more
// than once per agent per TTL window.
//
// Field semantics are unchanged: values still come from agentDiskUsage
// (best-effort, 0 on error/unavailable) and DiskUsedBytes stays best-effort.
// agentDiskUsage itself is untouched; the cache wraps it.

import (
	"sync"
	"time"
)

// diskUsageTTL is how long a per-agent disk-usage snapshot is served before
// the next request refreshes it. Chosen so operator UIs and scheduler polls
// see at most five-minute-old usage while never paying a walk more often.
const diskUsageTTL = 5 * time.Minute

// diskUsageWalk and diskUsageNow are the seams tests use: the walker (so
// tests count walks and never touch a real agent home) and the clock (so
// tests drive TTL expiry deterministically without sleeping).
var (
	diskUsageWalk = agentDiskUsage
	diskUsageNow  = time.Now
)

// diskUsageEntry is one cached per-agent snapshot: bytes plus the instant the
// walk that produced it completed (TTL is measured from completion, so a slow
// walk does not shorten the served window).
type diskUsageEntry struct {
	bytes     uint64
	refreshed time.Time
}

// diskUsageFlight is a per-agent in-flight refresh. Waiters take the pointer
// under the cache mutex, release it, and block on done; the walker closes
// done after publishing the fresh entry.
type diskUsageFlight struct {
	done chan struct{}
}

// diskUsageCache caches per-agent disk-usage snapshots with a TTL and
// per-agent single-flight refresh. All map access happens under mu; the walk
// itself runs OUTSIDE mu so refreshes of different agents never serialize.
type diskUsageCache struct {
	mu       sync.Mutex
	entries  map[string]diskUsageEntry
	inFlight map[string]*diskUsageFlight
	ttl      time.Duration
}

// agentDiskUsageCache is the process-wide snapshot cache backing
// ListAgents/AgentMetrics. It needs no constructor: fields are lazy-init
// under mu, so plain `&bunkerdService{...}` constructions in tests (which
// share this package var) work unchanged.
var agentDiskUsageCache = &diskUsageCache{ttl: diskUsageTTL}

// usageFor returns the cached disk usage for agentID, refreshing it (exactly
// one walk per agent id) when the entry is absent or older than the TTL.
func (c *diskUsageCache) usageFor(agentID string) uint64 {
	for {
		c.mu.Lock()
		if c.entries == nil {
			c.entries = make(map[string]diskUsageEntry)
		}
		if entry, ok := c.entries[agentID]; ok && diskUsageNow().Sub(entry.refreshed) < c.ttl {
			c.mu.Unlock()
			return entry.bytes
		}
		// Absent or expired. If another goroutine is already walking this
		// agent's home, share its result instead of starting a second walk.
		if f, ok := c.inFlight[agentID]; ok {
			c.mu.Unlock()
			<-f.done
			continue // re-read: the walker published a fresh entry before closing done
		}
		f := &diskUsageFlight{done: make(chan struct{})}
		if c.inFlight == nil {
			c.inFlight = make(map[string]*diskUsageFlight)
		}
		c.inFlight[agentID] = f
		c.mu.Unlock()

		// The one walk for this agent this TTL window. Best-effort: a failed
		// walk yields 0, exactly as the pre-fix per-request call did, and the
		// snapshot is cached so the next request inside the TTL doesn't retry.
		bytes := diskUsageWalk(agentID)

		c.mu.Lock()
		c.entries[agentID] = diskUsageEntry{bytes: bytes, refreshed: diskUsageNow()}
		delete(c.inFlight, agentID)
		c.mu.Unlock()
		close(f.done)
		return bytes
	}
}

// pruneLive drops snapshots for agents not in the live id set (agents that
// disappeared from the tracker), so the cache cannot grow without bound and a
// re-registered agent id is never served a snapshot of a destroyed home.
// Returns the number of entries dropped.
func (c *diskUsageCache) pruneLive(live map[string]struct{}) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped := 0
	for id := range c.entries {
		if _, ok := live[id]; !ok {
			delete(c.entries, id)
			dropped++
		}
	}
	return dropped
}
