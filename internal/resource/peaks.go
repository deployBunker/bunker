package resource

import (
	"sync"
	"time"
)

// ── GAP-121: daemon-observed usage peaks ────────────────────────────────────
//
// The cgroup v2 filesystem only grew `memory.peak` (kernel >= 5.19) and
// `pids.peak` (kernel >= 6.1); before that, and on any host whose kernel
// lacks them, nothing on the host remembers how close an agent came to its
// ceiling. The daemon therefore keeps its own high-water mark per agent,
// updated by a periodic sampler and by every metrics read.
//
// The value is honestly a LOWER BOUND — usage between two samples is
// invisible, and a daemon restart forgets it — which is why the wire surface
// carries the peak SOURCE next to the peak (PeakSourceDaemon), never a bare
// number that reads as authoritative.

// PeakTracker keeps the highest memory and pids usage observed per agent
// since the daemon started. It is safe for concurrent use: the sampler
// goroutine, every RPC-served metrics read and (in tests) direct observers
// may all call it at once.
type PeakTracker struct {
	mu    sync.Mutex
	peaks map[string]*agentPeak
	// now is a seam for deterministic tests; nil means time.Now.
	now func() time.Time
}

// agentPeak is one agent's observed high-water mark.
type agentPeak struct {
	memBytes uint64
	pids     uint64
	seenAt   time.Time
}

// NewPeakTracker returns an empty tracker.
func NewPeakTracker() *PeakTracker {
	return &PeakTracker{peaks: make(map[string]*agentPeak)}
}

// Observe records one usage sample for agentID, keeping the maximum per
// field. A zero sample never lowers a stored peak, so a stopped agent whose
// cgroup reads zero does not erase what it already demonstrated.
func (p *PeakTracker) Observe(agentID string, memBytes, pids uint64) {
	if p == nil || agentID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.peaks == nil {
		p.peaks = make(map[string]*agentPeak)
	}
	cur, ok := p.peaks[agentID]
	if !ok {
		cur = &agentPeak{}
		p.peaks[agentID] = cur
	}
	if memBytes > cur.memBytes {
		cur.memBytes = memBytes
	}
	if pids > cur.pids {
		cur.pids = pids
	}
	cur.seenAt = p.timestamp()
}

// Peaks returns the observed high-water mark for agentID. ok is false when
// nothing was ever observed for that agent — a real distinction from
// "observed zero", so callers never render an unseen agent as a measured 0.
func (p *PeakTracker) Peaks(agentID string) (memBytes, pids uint64, ok bool) {
	if p == nil {
		return 0, 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, ok := p.peaks[agentID]
	if !ok {
		return 0, 0, false
	}
	return cur.memBytes, cur.pids, true
}

// SeenAt returns when agentID's peak was last updated. ok is false when the
// agent was never observed.
func (p *PeakTracker) SeenAt(agentID string) (time.Time, bool) {
	if p == nil {
		return time.Time{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, ok := p.peaks[agentID]
	if !ok {
		return time.Time{}, false
	}
	return cur.seenAt, true
}

// Forget drops one agent's peak. The destroy path calls it so a destroyed
// agent does not keep a stale peak forever.
func (p *PeakTracker) Forget(agentID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.peaks, agentID)
}

// Retain drops every tracked agent whose id is NOT in keep. The sampler calls
// it after each pass with the live agent set, so the map is bounded by the
// number of live agents and a destroyed agent's peak cannot outlive it.
func (p *PeakTracker) Retain(keep map[string]struct{}) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.peaks {
		if _, ok := keep[id]; !ok {
			delete(p.peaks, id)
		}
	}
}

// Len reports how many agents are tracked (used by tests and the sampler's
// own diagnostics).
func (p *PeakTracker) Len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.peaks)
}

func (p *PeakTracker) timestamp() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}
