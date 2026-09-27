package fsclient

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestBFS037ProbeWaitedMS is a PROBE, not a cell: it prints the client's own
// slot-wait accounting under a saturating wave _through the read path the mount
// actually uses_ (HotRead), so the yield signal the refresh governor keys on is
// shown to come from a real admission conflict and not from a counter that
// cannot move (BFS-032).
//
// A previous revision drove Client.Get directly: that path does not acquire a
// slot, so the accounting stayed at zero and the probe was measuring nothing.
// The mount reads through the manager, so the probe reads through it too.
func TestBFS037ProbeWaitedMS(t *testing.T) {
	stub := newHotStub(t)
	stub.Throttle(200 * time.Millisecond)
	s := newHotSetup(t, stub,
		WithConcurrency(2),
		WithTick(5*time.Millisecond),
	)
	const wave = 8
	paths := make([]string, wave)
	for i := 0; i < wave; i++ {
		paths[i] = string(rune('a'+i)) + ".txt"
		stub.Set(paths[i], hotBody(2048, byte('a'+i)))
	}
	// One read, timed: the throttle must be observable or every timing arm in
	// this file is measuring nothing.
	oneStart := time.Now()
	s.seedRead(paths[0], false)
	t.Logf("PROBE: one throttled read took %s (throttle=200ms)", time.Since(oneStart))

	var wg sync.WaitGroup
	peak, peakWaiters := int64(0), int64(0)
	for i := 0; i < wave; i++ {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			_, _, oerr, _ := s.Manager.HotRead(context.Background(), p, "")
			if oerr != nil {
				t.Logf("PROBE read %s: %v", p, oerr)
			}
		}(paths[i])
	}
	for i := 0; i < 60; i++ {
		if v := s.Client.WaitedMS(); v > peak {
			peak = v
			peakWaiters = s.Client.fgWaiters.Load()
		}
		if i%10 == 0 {
			t.Logf("PROBE tick %d: waiters=%d waited=%d server_requests=%d",
				i, s.Client.fgWaiters.Load(), s.Client.WaitedMS(), stub.Requests())
		}
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()
	t.Logf("PROBE: peak slot wait %dms with %d waiters on a %d-slot pool (wave of %d reads)",
		peak, peakWaiters, 2, wave)
	if peak <= 0 {
		t.Fatal("PROBE: the slot-wait accounting never moved under a saturating read wave on a 2-slot pool: the yield signal is not observable through the read path")
	}
}
