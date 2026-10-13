package resource

import (
	"sync"
	"testing"
	"time"
)

// ── GAP-121: PeakTracker ────────────────────────────────────────────────────

func TestPeakTracker_KeepsMaximum(t *testing.T) {
	p := NewPeakTracker()
	p.Observe("a", 100, 5)
	p.Observe("a", 50, 9) // lower memory must not lower the peak; higher pids must raise it
	mem, pids, ok := p.Peaks("a")
	if !ok {
		t.Fatal("Peaks returned ok=false after two observations")
	}
	if mem != 100 {
		t.Errorf("memory peak = %d, want 100 (a lower sample must not lower the peak)", mem)
	}
	if pids != 9 {
		t.Errorf("pids peak = %d, want 9", pids)
	}
}

func TestPeakTracker_UnseenAgentIsNotAZeroReading(t *testing.T) {
	p := NewPeakTracker()
	mem, pids, ok := p.Peaks("never-sampled")
	if ok {
		t.Error("Peaks returned ok=true for an agent that was never observed")
	}
	if mem != 0 || pids != 0 {
		t.Errorf("unseen peaks = (%d, %d), want (0, 0)", mem, pids)
	}
}

func TestPeakTracker_ZeroObservationDoesNotErasePeak(t *testing.T) {
	// A stopped agent's cgroup reads zero; that must not wipe what the agent
	// already demonstrated before it stopped.
	p := NewPeakTracker()
	p.Observe("a", 4096, 20)
	p.Observe("a", 0, 0)
	mem, pids, _ := p.Peaks("a")
	if mem != 4096 || pids != 20 {
		t.Errorf("peaks = (%d, %d), want (4096, 20) after a zero sample", mem, pids)
	}
}

func TestPeakTracker_Forget(t *testing.T) {
	p := NewPeakTracker()
	p.Observe("a", 10, 1)
	p.Forget("a")
	if _, _, ok := p.Peaks("a"); ok {
		t.Error("Peaks returned ok=true after Forget")
	}
	if p.Len() != 0 {
		t.Errorf("Len = %d after Forget, want 0", p.Len())
	}
}

func TestPeakTracker_RetainPrunesNonLiveAgents(t *testing.T) {
	p := NewPeakTracker()
	p.Observe("live", 1, 1)
	p.Observe("gone", 2, 2)
	p.Retain(map[string]struct{}{"live": {}})
	if p.Len() != 1 {
		t.Fatalf("Len = %d after Retain, want 1 (the tracker must stay bounded by the live set)", p.Len())
	}
	if _, _, ok := p.Peaks("live"); !ok {
		t.Error("Retain dropped a live agent's peak")
	}
	if _, _, ok := p.Peaks("gone"); ok {
		t.Error("Retain kept a destroyed agent's peak")
	}
}

func TestPeakTracker_SeenAt(t *testing.T) {
	p := NewPeakTracker()
	p.now = func() time.Time { return time.Unix(1000, 0) }
	p.Observe("a", 1, 1)
	got, ok := p.SeenAt("a")
	if !ok {
		t.Fatal("SeenAt returned ok=false after Observe")
	}
	if !got.Equal(time.Unix(1000, 0)) {
		t.Errorf("SeenAt = %v, want %v", got, time.Unix(1000, 0))
	}
}

func TestPeakTracker_EmptyAgentIDIsIgnored(t *testing.T) {
	p := NewPeakTracker()
	p.Observe("", 999, 999)
	if p.Len() != 0 {
		t.Errorf("Len = %d, want 0 — an empty agent id must not create an entry", p.Len())
	}
}

func TestPeakTracker_NilReceiverIsSafe(t *testing.T) {
	var p *PeakTracker
	p.Observe("a", 1, 1)
	if _, _, ok := p.Peaks("a"); ok {
		t.Error("nil tracker returned ok=true")
	}
	p.Forget("a")
	p.Retain(map[string]struct{}{})
	if p.Len() != 0 {
		t.Error("nil tracker Len != 0")
	}
}

// TestPeakTracker_ConcurrentObservers proves the tracker is safe under the
// daemon's real access pattern: the sampler goroutine and every metrics RPC
// observe the same agent at once. Run with -race to make it meaningful.
func TestPeakTracker_ConcurrentObservers(t *testing.T) {
	p := NewPeakTracker()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				p.Observe("agent", uint64(n*1000+j), uint64(j))
				p.Peaks("agent")
			}
		}(i)
	}
	wg.Wait()
	mem, _, ok := p.Peaks("agent")
	if !ok {
		t.Fatal("Peaks returned ok=false after concurrent observations")
	}
	// The maximum offered value: agent 15 at j=199 -> 15*1000+199.
	if mem != 15199 {
		t.Errorf("memory peak = %d, want 15199", mem)
	}
}
