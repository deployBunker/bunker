// Package resource — port range allocator for agent sub-ranges.
package resource

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const sharedNamespace = "shared"

// namespacePool holds the allocator state for one network namespace. Port
// numbers are scoped by the kernel to a netns, so every namespace gets its own
// copy of the configured pool; the shared namespace retains the legacy global
// behavior.
type namespacePool struct {
	allocated map[string]uint32
	free      []uint32
}

// PortAllocator manages per-agent port range assignment from a configured pool.
// The full range [start, end] is divided into sub-ranges of size rangeSize.
// Each agent gets exactly one sub-range in its network namespace. Two agents
// in different namespaces may therefore hold the same port range safely.
type PortAllocator struct {
	mu        sync.Mutex
	start     uint32
	end       uint32
	rangeSize uint32
	numRanges uint32
	// namespaces tracks one independent pool per netns. Legacy methods use
	// sharedNamespace, preserving their original global-pool behavior.
	namespaces map[string]*namespacePool
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
		start:      start,
		end:        end,
		rangeSize:  rangeSize,
		numRanges:  numRanges,
		namespaces: make(map[string]*namespacePool),
	}
	// Create the legacy shared namespace eagerly. Additional netns pools are
	// created lazily when an isolated agent first requests a range.
	pa.namespaces[sharedNamespace] = newNamespacePool(start, rangeSize, numRanges)
	return pa, nil
}

func newNamespacePool(start, rangeSize, numRanges uint32) *namespacePool {
	pool := &namespacePool{
		allocated: make(map[string]uint32),
		free:      make([]uint32, 0, numRanges),
	}
	// Pre-populate free list in reverse order so the first allocation in every
	// network namespace starts at the configured pool start.
	for i := numRanges; i > 0; i-- {
		pool.free = append(pool.free, start+(i-1)*rangeSize)
	}
	return pool
}

func normalizeNamespace(namespace string) string {
	if namespace == "" {
		return sharedNamespace
	}
	return namespace
}

func (pa *PortAllocator) namespacePoolLocked(namespace string) *namespacePool {
	namespace = normalizeNamespace(namespace)
	pool := pa.namespaces[namespace]
	if pool == nil {
		pool = newNamespacePool(pa.start, pa.rangeSize, pa.numRanges)
		pa.namespaces[namespace] = pool
	}
	return pool
}

// Allocate assigns a free port sub-range in the shared network namespace.
// It preserves the original allocator API for shared-mode callers.
func (pa *PortAllocator) Allocate(agentID string) (uint32, uint32, error) {
	return pa.AllocateInNamespace(sharedNamespace, agentID)
}

// AllocateInNamespace assigns a free sub-range within namespace. Network
// namespaces have independent port spaces, so the same range can be allocated
// concurrently in different netns values.
func (pa *PortAllocator) AllocateInNamespace(namespace, agentID string) (uint32, uint32, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	pool := pa.namespacePoolLocked(namespace)

	if _, exists := pool.allocated[agentID]; exists {
		return 0, 0, fmt.Errorf("agent %q already has a port range allocated in namespace %q", agentID, normalizeNamespace(namespace))
	}
	if len(pool.free) == 0 {
		held := make([]string, 0, len(pool.allocated))
		holders := pa.heldRangesLocked(pool)
		for _, h := range holders {
			held = append(held, fmt.Sprintf("%s=%d-%d", h.AgentID, h.Start, h.End))
		}
		return 0, 0, fmt.Errorf(
			"no free port ranges available in namespace %q (pool exhausted: capacity %d ranges, %d held: %s) — "+
				"if any holder has no tracked agent this is a leak and self-heals within a minute (periodic drift sweep) or on the next reconcile; `bunkerd port-pool check` compares pool capacity with agent.max_agents",
			normalizeNamespace(namespace), pa.capacity(pool), len(holders), strings.Join(held, ", "))
	}

	rangeStart := pool.free[len(pool.free)-1]
	pool.free = pool.free[:len(pool.free)-1]
	pool.allocated[agentID] = rangeStart
	return rangeStart, pa.rangeEnd(rangeStart), nil
}

// Free releases the shared-namespace port sub-range assigned to agentID.
func (pa *PortAllocator) Free(agentID string) {
	pa.FreeInNamespace(sharedNamespace, agentID)
}

// FreeInNamespace releases the port sub-range assigned to agentID in namespace.
func (pa *PortAllocator) FreeInNamespace(namespace, agentID string) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	pool := pa.namespacePoolLocked(namespace)
	rangeStart, exists := pool.allocated[agentID]
	if !exists {
		return // nothing to free
	}

	delete(pool.allocated, agentID)
	pool.free = append(pool.free, rangeStart)
}

func (pa *PortAllocator) rangeEnd(rangeStart uint32) uint32 {
	rangeEnd := rangeStart + pa.rangeSize - 1
	if rangeEnd > pa.end {
		return pa.end
	}
	return rangeEnd
}

func (pa *PortAllocator) allocatedRangeLocked(pool *namespacePool, agentID string) (start, end uint32, ok bool) {
	start, ok = pool.allocated[agentID]
	if !ok {
		return 0, 0, false
	}
	return start, pa.rangeEnd(start), true
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

	pool := pa.namespacePoolLocked(sharedNamespace)
	if existing, ok := pool.allocated[agentID]; ok {
		if existing == start {
			return nil // already reserved to this agent: idempotent
		}
		return fmt.Errorf("agent %q already holds a port range starting at %d", agentID, existing)
	}
	if err := pa.validateRangeLocked(start, end); err != nil {
		return err
	}
	idx := -1
	for i, v := range pool.free {
		if v == start {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("port range %d-%d: %w (held by another agent or already allocated)", start, end, ErrRangeUnavailable)
	}
	pool.free = append(pool.free[:idx], pool.free[idx+1:]...)
	pool.allocated[agentID] = start
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

// AllocatedRange returns the shared-namespace sub-range currently held by agentID.
func (pa *PortAllocator) AllocatedRange(agentID string) (start, end uint32, ok bool) {
	return pa.AllocatedRangeInNamespace(sharedNamespace, agentID)
}

// AllocatedRangeInNamespace returns the range currently held by agentID in namespace.
func (pa *PortAllocator) AllocatedRangeInNamespace(namespace, agentID string) (start, end uint32, ok bool) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return pa.allocatedRangeLocked(pa.namespacePoolLocked(namespace), agentID)
}

// Has reports whether agentID currently holds a sub-range in the shared namespace.
func (pa *PortAllocator) Has(agentID string) bool {
	return pa.HasInNamespace(sharedNamespace, agentID)
}

// HasInNamespace reports whether agentID holds a sub-range in namespace.
func (pa *PortAllocator) HasInNamespace(namespace, agentID string) bool {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	_, ok := pa.namespacePoolLocked(namespace).allocated[agentID]
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
	return pa.HeldRangesInNamespace(sharedNamespace)
}

// HeldRangesInNamespace snapshots allocations for one network namespace.
func (pa *PortAllocator) HeldRangesInNamespace(namespace string) []HeldRange {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return pa.heldRangesLocked(pa.namespacePoolLocked(namespace))
}

func (pa *PortAllocator) heldRangesLocked(pool *namespacePool) []HeldRange {
	out := make([]HeldRange, 0, len(pool.allocated))
	for id, start := range pool.allocated {
		out = append(out, HeldRange{AgentID: id, Start: start, End: pa.rangeEnd(start)})
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
	return pa.ReconcileFromNamespace(sharedNamespace, exists)
}

// ReconcileFromNamespace releases reservations whose agent is absent from the
// supplied tracker for one network namespace.
func (pa *PortAllocator) ReconcileFromNamespace(namespace string, exists func(agentID string) bool) (released []string) {
	if exists == nil {
		return nil // refuse to sweep against an oracle that knows nothing
	}
	pa.mu.Lock()
	defer pa.mu.Unlock()
	pool := pa.namespacePoolLocked(namespace)
	releasedStarts := make([]uint32, 0, len(pool.allocated))
	for id, start := range pool.allocated {
		if exists(id) {
			continue
		}
		releasedStarts = append(releasedStarts, start)
		delete(pool.allocated, id)
		released = append(released, id)
	}
	if len(released) == 0 {
		return nil
	}
	pool.free = append(pool.free, releasedStarts...)
	sort.Strings(released)
	return released
}

// capacity returns the total number of sub-ranges available (caller must hold lock).
func (pa *PortAllocator) capacity(pool *namespacePool) uint32 {
	return uint32(len(pool.allocated) + len(pool.free))
}

// Available returns the number of free sub-ranges.
func (pa *PortAllocator) Available() uint32 {
	return pa.AvailableInNamespace(sharedNamespace)
}

// AvailableInNamespace returns the number of free sub-ranges in namespace.
func (pa *PortAllocator) AvailableInNamespace(namespace string) uint32 {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return uint32(len(pa.namespacePoolLocked(namespace).free))
}

// Allocated returns the number of currently assigned shared-namespace ranges.
func (pa *PortAllocator) Allocated() uint32 {
	return pa.AllocatedInNamespace(sharedNamespace)
}

// AllocatedInNamespace returns the number of assigned ranges in namespace.
func (pa *PortAllocator) AllocatedInNamespace(namespace string) uint32 {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return uint32(len(pa.namespacePoolLocked(namespace).allocated))
}

// MaxRanges returns the total number of sub-ranges the pool supports.
func (pa *PortAllocator) MaxRanges() uint32 {
	return pa.MaxRangesInNamespace(sharedNamespace)
}

// MaxRangesInNamespace returns the capacity of one network namespace pool.
func (pa *PortAllocator) MaxRangesInNamespace(namespace string) uint32 {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return pa.capacity(pa.namespacePoolLocked(namespace))
}
