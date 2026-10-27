package resource

import (
	"fmt"
	"strings"
	"testing"
)

// ── FLEET-INFRA-1: pool drift reconciliation + diagnostics ──────────────

// newDriftAllocator builds a 4-range pool (10000-10019, 5 ports per range).
func newDriftAllocator(t *testing.T) *PortAllocator {
	t.Helper()
	pa, err := NewPortAllocator(10000, 10019, 5)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}
	return pa
}

// TestReconcileFrom_ReleasesRangesWithoutTrackedAgents is the leak regression
// core: agents whose tracker record disappeared WITHOUT Free (a teardown
// path that skips it) have their ranges handed back by the sweep, and the
// pool returns to full capacity.
func TestReconcileFrom_ReleasesRangesWithoutTrackedAgents(t *testing.T) {
	pa := newDriftAllocator(t)
	tracked := map[string]bool{"live-1": true, "live-2": true}
	for _, id := range []string{"live-1", "live-2", "dead-1", "dead-2"} {
		if _, _, err := pa.Allocate(id); err != nil {
			t.Fatalf("Allocate %s: %v", id, err)
		}
	}
	if got := pa.Available(); got != 0 {
		t.Fatalf("pre-sweep available = %d, want 0 (pool fully allocated)", got)
	}

	released := pa.ReconcileFrom(func(id string) bool { return tracked[id] })
	if len(released) != 2 || released[0] != "dead-1" || released[1] != "dead-2" {
		t.Fatalf("released = %v, want [dead-1 dead-2] (sorted, deterministic)", released)
	}
	if got := pa.Available(); got != 2 {
		t.Fatalf("post-sweep available = %d, want 2", got)
	}
	if got := pa.Allocated(); got != 2 {
		t.Fatalf("post-sweep allocated = %d, want 2 (live agents untouched)", got)
	}
	// The freed capacity is USABLE again: the exact failure shape from the
	// field ("exhausted with 2/8 live") must no longer be terminal.
	if _, _, err := pa.Allocate("fresh-agent"); err != nil {
		t.Fatalf("Allocate after sweep: %v", err)
	}
}

// TestReconcileFrom_KeepsLiveReservations proves the sweep never touches a
// reservation whose agent is still tracked — including a simulation of the
// real restart ordering, where restoreAgent re-Reserves every replayed
// record before the tracker is consulted for anything else.
func TestReconcileFrom_KeepsLiveReservations(t *testing.T) {
	pa := newDriftAllocator(t)
	tracked := map[string]bool{"a": true, "b": true, "c": true}
	for _, id := range []string{"a", "b", "c"} {
		if _, _, err := pa.Allocate(id); err != nil {
			t.Fatalf("Allocate %s: %v", id, err)
		}
	}
	released := pa.ReconcileFrom(func(id string) bool { return tracked[id] })
	if len(released) != 0 {
		t.Fatalf("released = %v, want nothing (all agents tracked)", released)
	}
	if got := pa.Available(); got != 1 {
		t.Fatalf("available = %d, want 1 (3 allocated of 4)", got)
	}
}

// TestReconcileFrom_FreedRangesAreExact proves a swept range is returned
// WHOLE: the next Allocate gets the same start the dead agent held, so a
// leak cycle cannot permanently lose range geometry.
func TestReconcileFrom_FreedRangesAreExact(t *testing.T) {
	pa := newDriftAllocator(t)
	start, _, err := pa.Allocate("ghost")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	pa.ReconcileFrom(func(string) bool { return false }) // nothing tracked
	gotStart, gotEnd, err := pa.Allocate("next")
	if err != nil {
		t.Fatalf("Allocate after sweep: %v", err)
	}
	if gotStart != start || gotEnd != start+4 {
		t.Fatalf("re-allocated range = %d-%d, want the exact freed range %d-%d",
			gotStart, gotEnd, start, start+4)
	}
}

// TestReconcileFrom_NilOracleIsANoop guards the safety rail: a nil exists
// oracle must refuse to sweep (it knows nothing — sweeping against it would
// release EVERY reservation).
func TestReconcileFrom_NilOracleIsANoop(t *testing.T) {
	pa := newDriftAllocator(t)
	for i := range 4 {
		if _, _, err := pa.Allocate(fmt.Sprintf("agent-%d", i)); err != nil {
			t.Fatalf("Allocate: %v", err)
		}
	}
	if released := pa.ReconcileFrom(nil); released != nil {
		t.Fatalf("released = %v, want nil (nil oracle must not sweep)", released)
	}
	if got := pa.Allocated(); got != 4 {
		t.Fatalf("allocated = %d, want 4 (nothing released)", got)
	}
}

// TestReconcileFrom_Idempotent proves a second sweep against the same state
// releases nothing new (the drain check a periodic tick relies on).
func TestReconcileFrom_Idempotent(t *testing.T) {
	pa := newDriftAllocator(t)
	_, _, _ = pa.Allocate("a")
	_, _, _ = pa.Allocate("ghost")
	first := pa.ReconcileFrom(func(id string) bool { return id == "a" })
	if len(first) != 1 {
		t.Fatalf("first sweep released = %v, want [ghost]", first)
	}
	if second := pa.ReconcileFrom(func(id string) bool { return id == "a" }); len(second) != 0 {
		t.Fatalf("second sweep released = %v, want nothing", second)
	}
}

// TestExhaustionErrorNamesHolders proves the diagnostics half: when the pool
// is exhausted the error must name EVERY holder with its exact range (the
// field could only see "exhausted with 2/8 live" and no way to ask who held
// the rest).
func TestExhaustionErrorNamesHolders(t *testing.T) {
	pa := newDriftAllocator(t)
	want := map[string]bool{"a": false, "b": false, "c": false, "d": false}
	for id := range want {
		if _, _, err := pa.Allocate(id); err != nil {
			t.Fatalf("Allocate %s: %v", id, err)
		}
	}
	_, _, err := pa.Allocate("overflow")
	if err == nil {
		t.Fatal("expected exhaustion error, got nil")
	}
	for id := range want {
		if !strings.Contains(err.Error(), id+"=") {
			t.Errorf("exhaustion error %q does not name holder %q", err.Error(), id)
		}
	}
	if !strings.Contains(err.Error(), "pool exhausted") {
		t.Errorf("error %q lost the 'pool exhausted' marker the field greps for", err)
	}
}

// TestHeldRangesSortedSnapshot checks the HeldRanges diagnostic shape.
func TestHeldRangesSortedSnapshot(t *testing.T) {
	pa := newDriftAllocator(t)
	// Allocate in reverse range order; HeldRanges must still sort by start.
	for _, id := range []string{"d", "c", "b", "a"} {
		if _, _, err := pa.Allocate(id); err != nil {
			t.Fatalf("Allocate %s: %v", id, err)
		}
	}
	held := pa.HeldRanges()
	if len(held) != 4 {
		t.Fatalf("held = %v, want 4 entries", held)
	}
	for i := 1; i < len(held); i++ {
		if held[i].Start <= held[i-1].Start {
			t.Fatalf("held not sorted by start: %v", held)
		}
	}
	if held[0].AgentID != "d" || held[0].Start != 10000 || held[0].End != 10004 {
		t.Errorf("first held = %+v, want agent d at 10000-10004", held[0])
	}
}
