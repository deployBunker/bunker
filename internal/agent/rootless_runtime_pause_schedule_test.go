package agent

import (
	"testing"
	"time"
)

// TestRuntimeDirOwnershipPauseFor_ExponentialBounded pins the convergence
// pause schedule introduced after CI (root-suite on the 058a477 board commit,
// 2026-10-03) measured a logind teardown window outlasting the flat 50ms
// budget on every attempt (INT-CI-035 → INT-CI-038 → this run).
//
// The schedule must be:
//   - exponential (pause * 4^(attempt-1)): teardown windows are in the
//     hundreds of milliseconds to seconds, so each retry must wait longer
//     than the one before;
//   - bounded: 5 attempts must span a total pause well under the seconds
//     scale of the request deadline, and a hostile attempt index must not
//     overflow the duration.
func TestRuntimeDirOwnershipPauseFor_ExponentialBounded(t *testing.T) {
	want := map[int]time.Duration{
		1: 50 * time.Millisecond,  // before attempt 2
		2: 200 * time.Millisecond, // before attempt 3
		3: 800 * time.Millisecond,
		4: 3200 * time.Millisecond,
		5: 12800 * time.Millisecond,
	}
	for attempt, w := range want {
		if got := runtimeDirOwnershipPauseFor(attempt); got != w {
			t.Errorf("runtimeDirOwnershipPauseFor(%d) = %v, want %v", attempt, got, w)
		}
	}
	// Hostile index: must saturate, never overflow into a negative or
	// absurd duration.
	if got := runtimeDirOwnershipPauseFor(100); got != 12800*time.Millisecond {
		t.Errorf("runtimeDirOwnershipPauseFor(100) = %v, want saturated 12800ms", got)
	}
	// Total budget over the effective attempt count must stay a rounding
	// error against the 300s request deadline.
	total := time.Duration(0)
	for a := 1; a < runtimeDirOwnershipAttempts; a++ {
		total += runtimeDirOwnershipPauseFor(a)
	}
	if total > 5*time.Second {
		t.Errorf("total convergence pause budget %v exceeds the 5s sanity bound", total)
	}
}
