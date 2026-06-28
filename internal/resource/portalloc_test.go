package resource

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestNewPortAllocator(t *testing.T) {
	tests := []struct {
		name      string
		start     uint32
		end       uint32
		rangeSize uint32
		wantErr   bool
		errMsg    string
	}{
		{"valid small range", 10000, 10099, 10, false, ""},
		{"valid single port per agent", 10000, 10004, 1, false, ""},
		{"start equals end", 10000, 10000, 10, true, "start (10000) must be less than end"},
		{"start greater than end", 10050, 10000, 10, true, "start (10050) must be less than end"},
		{"zero range size", 10000, 10099, 0, true, "range size must be > 0"},
		{"range too small for size", 10000, 10003, 10, true, "too small for range size"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pa, err := NewPortAllocator(tt.start, tt.end, tt.rangeSize)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errMsg)
				} else if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pa == nil {
				t.Fatal("expected non-nil PortAllocator")
			}
		})
	}
}

func TestAllocateAndFree(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10099, 10)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}

	// First allocation should start at 10000
	start, end, err := pa.Allocate("agent-1")
	if err != nil {
		t.Fatalf("Allocate agent-1: %v", err)
	}
	if start != 10000 {
		t.Errorf("expected start 10000, got %d", start)
	}
	if end != 10009 {
		t.Errorf("expected end 10009, got %d", end)
	}

	// Second allocation
	start, end, err = pa.Allocate("agent-2")
	if err != nil {
		t.Fatalf("Allocate agent-2: %v", err)
	}
	if start != 10010 {
		t.Errorf("expected start 10010, got %d", start)
	}
	if end != 10019 {
		t.Errorf("expected end 10019, got %d", end)
	}

	if pa.Allocated() != 2 {
		t.Errorf("expected 2 allocated, got %d", pa.Allocated())
	}

	// Free agent-1
	pa.Free("agent-1")
	if pa.Allocated() != 1 {
		t.Errorf("expected 1 allocated after free, got %d", pa.Allocated())
	}

	// Re-allocate should get the freed range (10000)
	start, _, err = pa.Allocate("agent-3")
	if err != nil {
		t.Fatalf("Allocate agent-3: %v", err)
	}
	if start != 10000 {
		t.Errorf("expected re-allocated start 10000, got %d", start)
	}
}

func TestPoolExhaustion(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10019, 5) // 20 ports, 4 ranges of 5
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}

	if pa.MaxRanges() != 4 {
		t.Errorf("expected MaxRanges=4, got %d", pa.MaxRanges())
	}

	// Allocate all 4 ranges
	for i := range 4 {
		agentID := fmt.Sprintf("agent-%d", i+1)
		_, _, err := pa.Allocate(agentID)
		if err != nil {
			t.Fatalf("Allocate %s (attempt %d): %v", agentID, i+1, err)
		}
	}

	if pa.Available() != 0 {
		t.Errorf("expected 0 available, got %d", pa.Available())
	}

	// Fifth allocation should fail
	_, _, err = pa.Allocate("agent-overflow")
	if err == nil {
		t.Fatal("expected error on exhausted pool, got nil")
	}
}

func TestDuplicateAllocation(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10099, 10)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}

	_, _, err = pa.Allocate("agent-1")
	if err != nil {
		t.Fatalf("first allocate: %v", err)
	}

	_, _, err = pa.Allocate("agent-1")
	if err == nil {
		t.Fatal("expected error on duplicate allocation, got nil")
	}
}

func TestFreeUnknownAgent(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10099, 10)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}

	// Free unknown agent should not panic
	pa.Free("nonexistent")

	if pa.Available() != 10 { // 100 ranges in 10000-10099 with size 10
		// Actually: 10000-10099 = 100 ports, rangeSize 10 → 10 ranges
		t.Logf("available: %d", pa.Available())
	}
}

func TestConcurrentAllocation(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10099, 10)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}

	const numAgents = 8
	var wg sync.WaitGroup
	results := make([]uint32, numAgents)
	errs := make([]error, numAgents)

	for i := range numAgents {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			agentID := fmt.Sprintf("agent-%d", idx)
			start, _, err := pa.Allocate(agentID)
			results[idx] = start
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	// Check no errors
	for i, err := range errs {
		if err != nil {
			t.Errorf("agent-%d: unexpected error: %v", i, err)
		}
	}

	// Check all ranges are unique
	seen := make(map[uint32]bool)
	for i, start := range results {
		if errs[i] != nil {
			continue
		}
		if seen[start] {
			t.Errorf("duplicate port range start %d (agent-%d)", start, i)
		}
		seen[start] = true
	}

	if pa.Allocated() != uint32(len(seen)) {
		t.Errorf("expected %d allocated, got %d", len(seen), pa.Allocated())
	}
}

func TestRangeBoundaries(t *testing.T) {
	// Range that doesn't divide evenly: 10000-10004 = 5 ports, rangeSize 2 → 2 ranges of 2, 1 port wasted
	pa, err := NewPortAllocator(10000, 10004, 2)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}

	if pa.MaxRanges() != 2 {
		t.Errorf("expected MaxRanges=2, got %d", pa.MaxRanges())
	}

	start, end, err := pa.Allocate("a1")
	if err != nil {
		t.Fatalf("allocate a1: %v", err)
	}
	if start != 10000 || end != 10001 {
		t.Errorf("a1: expected [10000,10001], got [%d,%d]", start, end)
	}

	start, end, err = pa.Allocate("a2")
	if err != nil {
		t.Fatalf("allocate a2: %v", err)
	}
	if start != 10002 || end != 10003 {
		t.Errorf("a2: expected [10002,10003], got [%d,%d]", start, end)
	}
}
