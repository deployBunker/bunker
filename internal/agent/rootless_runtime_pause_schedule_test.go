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
//   - bounded: 3 attempts must span a total pause of at least 3s (the
//     teardown window CI measured) but far under the 300s request deadline,
//     and a hostile attempt index must not overflow into an absurd duration.
func TestRuntimeDirOwnershipPauseFor_ExponentialBounded(t *testing.T) {
	want := map[int]time.Duration{
		1: 600 * time.Millisecond,   // before attempt 2
		2: 2400 * time.Millisecond,  // before attempt 3
		3: 9600 * time.Millisecond,  // beyond the live budget (shift cap)
		4: 38400 * time.Millisecond, // saturated (shift capped at 4)
		5: 38400 * time.Millisecond, // saturated
	}
	for attempt, w := range want {
		if got := runtimeDirOwnershipPauseFor(attempt); got != w {
			t.Errorf("runtimeDirOwnershipPauseFor(%d) = %v, want %v", attempt, got, w)
		}
	}
	// Hostile index: must saturate, never overflow into a negative or
	// absurd duration.
	if got := runtimeDirOwnershipPauseFor(100); got != 38400*time.Millisecond {
		t.Errorf("runtimeDirOwnershipPauseFor(100) = %v, want saturated 38400ms", got)
	}
	// Total budget over the effective attempt count must ride out the
	// teardown window CI measured (>= 3s) and stay far under the 300s
	// request deadline.
	total := time.Duration(0)
	for a := 1; a < runtimeDirOwnershipAttempts; a++ {
		total += runtimeDirOwnershipPauseFor(a)
	}
	if total < 3*time.Second {
		t.Errorf("total convergence pause budget %v is below the 3s teardown window the CI failure measured", total)
	}
	if total > 4*time.Second {
		t.Errorf("total convergence pause budget %v exceeds the 4s sanity bound", total)
	}
}
