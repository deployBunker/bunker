package server

// PERF-001: ListAgents and AgentMetrics used to walk every agent home
// (filepath.WalkDir + d.Info() per file) synchronously on every request —
// measured at 7.2s per ListAgents for one ~804k-file home on cube-las-00,
// paid by every `bunker list`, scheduler poll and QA preflight.
//
// PERF-010: even with PERF-001's once-per-TTL-window refresh, the request
// that landed on an absent or expired entry paid the whole walk itself —
// ~9.9s on a 244k-file home, stalling every ListAgents/AgentMetrics poll
// that hit the expiry. This file now serves such calls IMMEDIATELY: a
// first-ever miss returns the 0 sentinel (the value the best-effort field
// already carries on walk failure, and which CLI consumers hide), an
// expired entry returns the stale snapshot; exactly one background refresh
// per agent (per-agent single-flight) walks the home and publishes the
// fresh entry for subsequent calls.
//
// A walk never runs more than once per agent per TTL window (PERF-001
// invariant, kept): the in-flight registration is the single-flight — every
// caller arriving while a refresh runs shares it, and none of them blocks.
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

// diskUsageFlight is a per-agent in-flight background refresh. Waiters never
// block on it; waitDiskUsageRefresh (in the PERF-010 tests) reads it to make
// the async publish deterministic, and the refresh goroutine closes done
// after publishing the fresh entry.
type diskUsageFlight struct {
	done chan struct{}
}

// diskUsageCache caches per-agent disk-usage snapshots with a TTL and
// per-agent single-flight background refresh. All map access happens under
// mu; the walk itself runs OUTSIDE mu (and outside the request path) so
// refreshes of different agents never serialize.
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

// usageFor returns the cached disk usage for agentID without ever blocking
// on a walk. A fresh entry is served directly; an expired entry returns its
// (stale) bytes; a first-ever miss returns 0 — the sentinel every caller
// already tolerates, because agentDiskUsage is best-effort and a failed walk
// always yielded 0 too. Absent-or-expired triggers exactly one background
// refresh per agent id: callers arriving while it runs share it (per-agent
// single-flight) and the refreshed value lands in the cache for subsequent
// calls. PERF-001's invariant is kept — a walk never runs more than once per
// agent per TTL window.
func (c *diskUsageCache) usageFor(agentID string) uint64 {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]diskUsageEntry)
	}
	if entry, ok := c.entries[agentID]; ok && diskUsageNow().Sub(entry.refreshed) < c.ttl {
		c.mu.Unlock()
		return entry.bytes
	}
	// Absent or expired: serve what we have (stale bytes, or 0 on a
	// first-ever miss) and make sure exactly one background refresh runs.
	stale := uint64(0)
	if entry, ok := c.entries[agentID]; ok {
		stale = entry.bytes
	}
	if _, ok := c.inFlight[agentID]; ok {
		c.mu.Unlock()
		return stale // a refresh is already running; serve stale, never wait
	}
	f := &diskUsageFlight{done: make(chan struct{})}
	if c.inFlight == nil {
		c.inFlight = make(map[string]*diskUsageFlight)
	}
	c.inFlight[agentID] = f
	c.mu.Unlock()

	// The one walk for this agent this TTL window, off the request path.
	// Best-effort: a failed walk yields 0, exactly as the pre-PERF-001
	// per-request call did, and the snapshot is cached so the next request
	// inside the TTL doesn't retry.
	go func() {
		defer func() {
			// Panic containment: a panicking walker must not take the
			// process down from a background goroutine; the request path
			// never observes it (best-effort 0 semantics preserved).
			if r := recover(); r != nil {
				c.mu.Lock()
				delete(c.inFlight, agentID)
				c.mu.Unlock()
				close(f.done)
			}
		}()
		bytes := diskUsageWalk(agentID)

		c.mu.Lock()
		c.entries[agentID] = diskUsageEntry{bytes: bytes, refreshed: diskUsageNow()}
		delete(c.inFlight, agentID)
		c.mu.Unlock()
		close(f.done)
	}()
	return stale
}

// pruneLive drops snapshots for agents not in the live id set (agents that
// disappeared from the tracker), so the cache cannot grow without bound and a
// re-registered agent id is never served a snapshot of a destroyed home.
// A refresh still in flight for a pruned agent is left alone: it publishes a
// fresh snapshot for that id, but until the agent re-registers and its id
// reaches this function's live set again, every pruneLive call drops the
// entry — and usageFor re-reads it only for live agents.
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
