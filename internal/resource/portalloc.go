// Package resource — port range allocator for agent sub-ranges.
package resource

import (
	"fmt"
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
		return 0, 0, fmt.Errorf("no free port ranges available (pool exhausted: %d ranges)", pa.capacity())
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
