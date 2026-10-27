package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/deployBunker/bunker/internal/resource"
)

// ── FLEET-INFRA-1: manager-side port-pool drift sweep ───────────────────

// simulateLeakedReservation builds the exact residue the field reported: the
// allocator holds a range for an agent id that has NO tracker record — the
// shape any teardown path that skips Free leaves behind — and the pool is
// filled to exhaustion ("exhausted with 2/8 agents live"). The live agent is
// the control: fully tracked, must never be touched.
func simulateLeakedReservation(t *testing.T, m *AgentManager, ghost, live string) {
	t.Helper()
	if _, _, err := m.portAlloc.Allocate(ghost); err != nil {
		t.Fatalf("seed ghost reservation: %v", err)
	}
	if _, _, err := m.portAlloc.Allocate(live); err != nil {
		t.Fatalf("seed live reservation: %v", err)
	}
	if err := m.tracker.Register(&resource.AgentRecord{AgentID: live, Status: "running"}); err != nil {
		t.Fatalf("register live agent: %v", err)
	}
	// Fill every remaining range with untracked holders: the exhausted-pool
	// shape where the pool is full but almost nobody is actually alive.
	for i := 0; ; i++ {
		if _, _, err := m.portAlloc.Allocate(fmt.Sprintf("filler-%d", i)); err != nil {
			break // pool full
		}
	}
	if got := m.portAlloc.Available(); got != 0 {
		t.Fatalf("could not exhaust the test pool: available = %d", got)
	}
}

// TestReconcile_SweepsReservationsWithoutTrackedAgents is the AC-1 core:
// "simulate agent death without cleanup, call reconcile, verify the pool is
// clean and ranges are available again". A leaked reservation (no tracker
// record) is released by the reconcile drift sweep and the freed capacity is
// immediately usable, while the live agent's reservation is untouched.
func TestReconcile_SweepsReservationsWithoutTrackedAgents(t *testing.T) {
	m := newRegistryManager(t, filepath.Join(t.TempDir(), "agents.jsonl"))
	simulateLeakedReservation(t, m, "ghost-agent", "live-agent")

	if got := m.portAlloc.Available(); got != 0 {
		t.Fatalf("pre-reconcile available = %d, want 0 (pool exhausted shape)", got)
	}

	rep := m.Reconcile(context.Background())

	// Released = every untracked holder: the ghost PLUS all the fillers the
	// exhaust helper added (pool capacity 100, one tracked agent). The count
	// is the proof the WHOLE pool healed, not just one range.
	if rep.PortPoolReleased != 99 {
		t.Fatalf("report.PortPoolReleased = %d, want 99 (ghost + 98 fillers)", rep.PortPoolReleased)
	}
	if m.portAlloc.Has("ghost-agent") {
		t.Fatal("ghost reservation survived the reconcile sweep — still leaked")
	}
	if !m.portAlloc.Has("live-agent") {
		t.Fatal("live agent's reservation was released by the sweep — overreach")
	}
	if got := m.portAlloc.Available(); got != 99 {
		t.Fatalf("post-reconcile available = %d, want 99 (everything but the live agent's range)", got)
	}
	// The recovered capacity is usable again: the next spawn gets a range.
	if s, _, err := m.portAlloc.Allocate("next-spawn"); err != nil {
		t.Fatalf("Allocate after sweep: %v", err)
	} else if s == 0 {
		t.Fatal("next spawn got an invalid range start")
	}
}

// TestReconcile_SweepDoesNotFightRestore proves the sweep ordering is safe:
// a REPLAYED agent is restored with its exact reservation AND a tracker
// record before the sweep runs, so the sweep must keep it (a sweep inside
// the restore window would release the reservation and let the next spawn
// double-allocate the agent's ports).
func TestReconcile_SweepDoesNotFightRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	first := newRegistryManager(t, path)
	rec := &resource.AgentRecord{
		AgentID:        "restored-agent",
		Status:         "running",
		PortRangeStart: 10000,
		PortRangeEnd:   10099,
	}
	if err := first.persistSpawn(rec); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	if err := first.registry.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := newRegistryManager(t, path)
	defer func() {
		if err := second.registry.Close(); err != nil {
			t.Logf("close second registry: %v", err)
		}
	}()
	second.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "restored-agent", Username: "bunker-restored-agent", Home: "/home/bunker-restored-agent"}}, nil
	}

	rep := second.Reconcile(context.Background())
	if rep.Restored != 1 {
		t.Fatalf("report.Restored = %d, want 1", rep.Restored)
	}
	if rep.PortPoolReleased != 0 {
		t.Fatalf("report.PortPoolReleased = %d, want 0 (a restored agent is not drift)", rep.PortPoolReleased)
	}
	if !second.portAlloc.Has("restored-agent") {
		t.Fatal("restored agent lost its exact reservation to the sweep")
	}
	if second.tracker.Get("restored-agent") == nil {
		t.Fatal("restored agent lost its tracker record")
	}
}

// TestTickMaintenance_SweepsReservationsWithoutTrackedAgents proves the
// periodic path: one TTL-reaper tick releases a leak that appeared between
// ticks — the convergence bound that keeps any future Free-skipping path
// from ever re-creating the "exhausted until restart" failure.
func TestTickMaintenance_SweepsReservationsWithoutTrackedAgents(t *testing.T) {
	m := newRegistryManager(t, filepath.Join(t.TempDir(), "agents.jsonl"))
	simulateLeakedReservation(t, m, "ghost-agent", "live-agent")

	m.tickMaintenance()

	if m.portAlloc.Has("ghost-agent") {
		t.Fatal("ghost reservation survived a maintenance tick — leak not bounded")
	}
	if !m.portAlloc.Has("live-agent") {
		t.Fatal("live agent's reservation was released by the tick — overreach")
	}
	// Idempotent: a second tick releases nothing new and stays safe.
	m.tickMaintenance()
	if !m.portAlloc.Has("live-agent") {
		t.Fatal("second tick released the live agent's reservation")
	}
}

// TestSweepPortPoolDrift_NilPortAllocIsSafe: hand-built managers (and pools
// disabled by bad geometry) run with a nil allocator; the sweep must no-op,
// not panic — every heartbeat path reaches it.
func TestSweepPortPoolDrift_NilPortAllocIsSafe(t *testing.T) {
	m := newCoverageTestManager(4) // no portAlloc wired
	if released := m.sweepPortPoolDrift(); released != nil {
		t.Fatalf("released = %v, want nil (nil allocator must no-op)", released)
	}
	m.tickMaintenance() // must not panic
}
