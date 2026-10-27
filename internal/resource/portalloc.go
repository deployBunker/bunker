// Package resource — port range allocator for agent sub-ranges.
package resource

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// PortAllocator manages per-agent port range assignment from a configured pool.
// The full range [start, end] is divided into sub-ranges of size rangeSize.
// Each agent gets exactly one sub-range.
type PortAllocator struct {
	mu        sync.Mutex
	start     uint32
	end       uint32
	rangeSize uint32
	// allocated tracks agentID → assigned sub-range start port
	allocated map[string]uint32
	// free is a stack of free sub-range start ports
	free []uint32
}

// NewPortAllocator creates a new PortAllocator.
// start and end define the total port range (inclusive).
// rangeSize is the number of ports per agent sub-range.
func NewPortAllocator(start, end, rangeSize uint32) (*PortAllocator, error) {
	if start >= end {
		return nil, fmt.Errorf("port range start (%d) must be less than end (%d)", start, end)
	}
	if rangeSize == 0 {
		return nil, fmt.Errorf("range size must be > 0")
	}

	totalPorts := end - start + 1
	numRanges := totalPorts / rangeSize
	if numRanges == 0 {
		return nil, fmt.Errorf("port range %d-%d too small for range size %d: only %d ports available",
			start, end, rangeSize, totalPorts)
	}

	pa := &PortAllocator{
		start:     start,
		end:       end,
		rangeSize: rangeSize,
		allocated: make(map[string]uint32),
		free:      make([]uint32, 0, numRanges),
	}

	// Pre-populate free list in reverse order so the first allocated
	// range starts at 'start'.
	for i := numRanges; i > 0; i-- {
		rangeStart := start + (i-1)*rangeSize
		pa.free = append(pa.free, rangeStart)
	}

	return pa, nil
}

// Allocate assigns a free port sub-range to the given agentID.
// Returns (rangeStart, rangeEnd, error).
func (pa *PortAllocator) Allocate(agentID string) (uint32, uint32, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	if _, exists := pa.allocated[agentID]; exists {
		return 0, 0, fmt.Errorf("agent %q already has a port range allocated", agentID)
	}

	if len(pa.free) == 0 {
		// FLEET-INFRA-1: an exhaustion error that names nobody is
		// undebuggable — the field saw "exhausted with 2/8 agents live"
		// and had no way to ask who held the rest. Name every holder and
		// point at the two remediation paths (the tracker drift sweep on
		// reconcile, and the on-demand sweep) so the operator can act
		// instead of restarting blind.
		held := make([]string, 0, len(pa.allocated))
		holders := pa.heldRangesLocked()
		for _, h := range holders {
			held = append(held, fmt.Sprintf("%s=%d-%d", h.AgentID, h.Start, h.End))
		}
		return 0, 0, fmt.Errorf(
			"no free port ranges available (pool exhausted: capacity %d ranges, %d held: %s) — "+
				"if any holder has no tracked agent this is a leak and self-heals within a "+
				"minute (periodic drift sweep) or on the next reconcile; `bunkerd port-pool "+
				"check` compares pool capacity with agent.max_agents",
			pa.capacity(), len(holders), strings.Join(held, ", "))
	}

	// Pop from the free stack.
	rangeStart := pa.free[len(pa.free)-1]
	pa.free = pa.free[:len(pa.free)-1]
	pa.allocated[agentID] = rangeStart

	rangeEnd := rangeStart + pa.rangeSize - 1
	if rangeEnd > pa.end {
		rangeEnd = pa.end
	}

	return rangeStart, rangeEnd, nil
}

// Free releases the port sub-range assigned to agentID back into the pool.
func (pa *PortAllocator) Free(agentID string) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	rangeStart, exists := pa.allocated[agentID]
	if !exists {
		return // nothing to free
	}

	delete(pa.allocated, agentID)
	pa.free = append(pa.free, rangeStart)
}

// ErrRangeUnavailable is returned by Reserve/Restore when the requested
// sub-range is valid but not currently free in the pool.
var ErrRangeUnavailable = errors.New("port range unavailable")

// ValidateRange reports whether [start, end] is a legal sub-range of this
// allocator's pool: inside the configured bounds, aligned to rangeSize, and
// exactly rangeSize wide (the final range may be clipped by the pool end).
// Exported so callers restoring persisted metadata can pre-check it.
func (pa *PortAllocator) ValidateRange(start, end uint32) error {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return pa.validateRangeLocked(start, end)
}

func (pa *PortAllocator) validateRangeLocked(start, end uint32) error {
	if start < pa.start || end > pa.end {
		return fmt.Errorf("port range %d-%d outside pool %d-%d", start, end, pa.start, pa.end)
	}
	if end < start {
		return fmt.Errorf("port range %d-%d is inverted", start, end)
	}
	if (start-pa.start)%pa.rangeSize != 0 {
		return fmt.Errorf("port range start %d is not aligned to range size %d from pool start %d",
			start, pa.rangeSize, pa.start)
	}
	wantEnd := start + pa.rangeSize - 1
	if wantEnd > pa.end {
		wantEnd = pa.end
	}
	if end != wantEnd {
		return fmt.Errorf("port range %d-%d has the wrong size: expected %d-%d (range size %d)",
			start, end, start, wantEnd, pa.rangeSize)
	}
	return nil
}

// Bounds returns the allocator's inclusive port span. Callers use it to tell
// a sub-range that could belong to this pool from one that provably cannot.
func (pa *PortAllocator) Bounds() (start, end uint32) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return pa.start, pa.end
}

// Reserve claims an EXACT port sub-range for agentID, validating it against
// the pool geometry and against the currently-free list. It is idempotent
// for the same agent + same range (a repeated replay/adopt of the same
// persisted metadata succeeds), and fails if the range is held by another
// agent or has already been handed out.
func (pa *PortAllocator) Reserve(agentID string, start, end uint32) error {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	if existing, ok := pa.allocated[agentID]; ok {
		if existing == start {
			return nil // already reserved to this agent: idempotent
		}
		return fmt.Errorf("agent %q already holds a port range starting at %d", agentID, existing)
	}
	if err := pa.validateRangeLocked(start, end); err != nil {
		return err
	}
	idx := -1
	for i, v := range pa.free {
		if v == start {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("port range %d-%d: %w (held by another agent or already allocated)", start, end, ErrRangeUnavailable)
	}
	pa.free = append(pa.free[:idx], pa.free[idx+1:]...)
	pa.allocated[agentID] = start
	return nil
}

// Restore re-establishes a PERSISTED reservation for agentID after a daemon
// relaunch. It is the replay/adopt-facing form of Reserve: identical
// validation and idempotency, named for the durability contract it serves —
// an adopted agent must keep the exact ports it had, otherwise a later
// Allocate could hand them to a second agent.
func (pa *PortAllocator) Restore(agentID string, start, end uint32) error {
	return pa.Reserve(agentID, start, end)
}

// AllocatedRange returns the sub-range currently held by agentID.
func (pa *PortAllocator) AllocatedRange(agentID string) (start, end uint32, ok bool) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	s, exists := pa.allocated[agentID]
	if !exists {
		return 0, 0, false
	}
	e := s + pa.rangeSize - 1
	if e > pa.end {
		e = pa.end
	}
	return s, e, true
}

// Has reports whether agentID currently holds a sub-range.
func (pa *PortAllocator) Has(agentID string) bool {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	_, ok := pa.allocated[agentID]
	return ok
}

// HeldRange describes one currently-allocated sub-range for diagnostics.
type HeldRange struct {
	AgentID    string
	Start, End uint32
}

// HeldRanges snapshots every currently-allocated sub-range, sorted by start
// port. It exists so an exhaustion error can name its holders: an error that
// only says "pool exhausted" forced operators to restart the daemon blind
// (the field symptom was "exhausted with 2/8 agents live" and no way to ask
// who held the other ranges). Never large: at most one entry per sub-range.
func (pa *PortAllocator) HeldRanges() []HeldRange {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return pa.heldRangesLocked()
}

func (pa *PortAllocator) heldRangesLocked() []HeldRange {
	out := make([]HeldRange, 0, len(pa.allocated))
	for id, start := range pa.allocated {
		end := start + pa.rangeSize - 1
		if end > pa.end {
			end = pa.end
		}
		out = append(out, HeldRange{AgentID: id, Start: start, End: end})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// ReconcileFrom drops every reservation whose agentID fails exists — the
// drift sweep that keeps the in-memory pool honest. The allocator's state
// lives only in process memory, so a code path that tears an agent down
// without calling Free leaks its range until the daemon restarts, and the
// restart's registry replay can resurrect the very record that caused the
// leak: the pool never healed by itself (FLEET-INFRA-1: 27 exhaustion
// observations across 15 projects with 1-2 agents live). The sweep is
// authoritative about memory, never about the host: it releases
// reservations, it does not touch users or homes — whether an agent should
// exist is the registry/system reconciliation's decision (reconcile.go),
// which is why callers run this AFTER that walk settled.
//
// exists must be cheap and non-blocking (it runs under the allocator lock);
// the tracker lookup is the intended implementation. Released agent ids are
// returned sorted so logs and tests are deterministic; the count is
// len(released).
func (pa *PortAllocator) ReconcileFrom(exists func(agentID string) bool) (released []string) {
	if exists == nil {
		return nil // refuse to sweep against an oracle that knows nothing
	}
	pa.mu.Lock()
	defer pa.mu.Unlock()
	releasedStarts := make([]uint32, 0, len(pa.allocated))
	for id, start := range pa.allocated {
		if exists(id) {
			continue
		}
		// Capture the start BEFORE the delete: after it the lookup is gone.
		releasedStarts = append(releasedStarts, start)
		delete(pa.allocated, id)
		released = append(released, id)
	}
	if len(released) == 0 {
		return nil
	}
	pa.free = append(pa.free, releasedStarts...)
	// Deterministic release order: sort ids, then keep the free list's
	// membership correct regardless of map iteration order (the free list
	// is a stack — order affects only which range the next Allocate pops,
	// never correctness or capacity).
	sort.Strings(released)
	return released
}

// capacity returns the total number of sub-ranges available (caller must hold lock).
func (pa *PortAllocator) capacity() uint32 {
	return uint32(len(pa.allocated) + len(pa.free))
}

// Available returns the number of free sub-ranges.
func (pa *PortAllocator) Available() uint32 {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return uint32(len(pa.free))
}

// Allocated returns the number of currently assigned sub-ranges.
func (pa *PortAllocator) Allocated() uint32 {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return uint32(len(pa.allocated))
}

// MaxRanges returns the total number of sub-ranges the pool supports.
func (pa *PortAllocator) MaxRanges() uint32 {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return pa.capacity()
}
