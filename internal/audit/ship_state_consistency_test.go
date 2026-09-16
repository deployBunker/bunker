package audit

// INT-CI-004 regression tests: the persisted shipstate file (the product
// surface behind `bunker audit status`) must never contradict the live ship
// queue at write time, and a reader that observes the drained in-memory
// snapshot must never then read a file still carrying an older attempt's
// depth/result.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestShipStateQueueDepthMatchesLiveQueue (INT-CI-004, lying-depth
// property). Drives the production enqueue-side failure path — ShipSegment
// on an unreadable segment, the exact call site that hardcoded depth 0
// pre-fix — while one readable segment sits in the retry queue. The
// persisted state must report the LIVE queue depth, never a caller-supplied
// constant.
func TestShipStateQueueDepthMatchesLiveQueue(t *testing.T) {
	l, path := newTestLogOpts(t, Options{})
	_ = l
	dir := filepath.Dir(path)
	s, err := NewShipper(path, unreachableEndpoint, nil)
	if err != nil {
		t.Fatalf("NewShipper: %v", err)
	}
	t.Cleanup(s.Stop)
	// Long backoff: after the first failed attempt the worker must stay
	// asleep so it cannot rewrite the state behind the assertions.
	s.backoffBase = 60 * time.Second
	s.backoffMax = 60 * time.Second

	// 1. A readable segment: enqueued, attempted (unreachable endpoint →
	//    fails fast), stays queued at depth 1.
	good := filepath.Join(dir, "seg-good")
	if err := os.WriteFile(good, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.ShipSegment(good, "head-good", "")

	// Wait for the failed attempt's state write; the worker is now in its
	// 60s backoff and cannot write again.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := ReadShipState(path); err == nil && strings.Contains(st.LastResult, "error") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	// 2. The enqueue-side read failure (segment unreadable at enqueue):
	//    ShipSegment records the error and refreshes the state file — the
	//    same code path a real rotation hit when its segment read failed.
	s.ShipSegment(filepath.Join(dir, "seg-missing"), "head-missing", "")

	// That record is the LAST write (worker asleep in backoff, no wake was
	// sent): the file must carry it, with the live queue depth.
	deadline = time.Now().Add(5 * time.Second)
	var st *ShipState
	for time.Now().Before(deadline) {
		if cur, err := ReadShipState(path); err == nil && strings.Contains(cur.LastResult, "read segment") {
			st = cur
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if st == nil {
		t.Fatal("enqueue-side failure result never reached the shipstate file")
	}
	s.mu.Lock()
	live := len(s.queue)
	s.mu.Unlock()
	if live != 1 {
		t.Fatalf("live queue depth = %d, want 1", live)
	}
	if st.QueueDepth != live {
		t.Errorf("shipstate = %+v: queue_depth %d, want %d (live queue) — persisted state contradicts the queue", st, st.QueueDepth, live)
	}
}

// TestShipStateFileNeverLagsDrainedSnapshot (INT-CI-004, ordering
// property). Spins on the in-memory snapshot until it reports drained
// (depth 0 + result ok) and reads the state FILE immediately — no
// convergence grace. Pre-fix the worker released the shipper lock before
// writing the file, so the immediate read observed the previous attempt's
// snapshot; post-fix the write happens inside the publication critical
// section, so the read is always consistent.
func TestShipStateFileNeverLagsDrainedSnapshot(t *testing.T) {
	const iterations = 100
	stale, nodrain := 0, 0
	for i := 0; i < iterations; i++ {
		var calls atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		l, path := newTestLogOpts(t, Options{ShipTo: srv.URL})
		s := l.shipper
		s.backoffBase = time.Millisecond
		s.backoffMax = 2 * time.Millisecond
		l.rotateAt = 128
		writeRecords(t, l, 2) // one rotation → one shipped segment

		// Hot-spin (no sleep): observe the drain the instant it is
		// published, then touch nothing before reading the file.
		drained := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, _, res, depth := s.snapshot(); depth == 0 && res == "ok" {
				drained = true
				break
			}
		}
		st, err := ReadShipState(path)
		switch {
		case !drained:
			nodrain++
		case err != nil:
			stale++
			t.Logf("iter %d: shipstate unreadable after drained snapshot: %v", i, err)
		case st.LastResult != "ok" || st.QueueDepth != 0:
			stale++
			t.Logf("iter %d stale file: %+v", i, st)
		}
		_ = l.Close()
		srv.Close()
	}
	if nodrain > iterations/10 {
		t.Errorf("%d/%d iterations never drained — environment, not the defect under test", nodrain, iterations)
	}
	if stale > 0 {
		t.Errorf("shipstate lagged the drained in-memory snapshot in %d/%d iterations (%.1f%%) — state write is not ordered with the snapshot publication", stale, iterations, 100*float64(stale)/iterations)
	}
}
