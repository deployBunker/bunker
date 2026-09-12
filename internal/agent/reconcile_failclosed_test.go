package agent

// Focused GAP-070 regression tests for the fail-closed adoption and restore
// contracts (foreman rework):
//
//  1. adoption requires readable, valid, free persisted port metadata. Missing
//     or malformed metadata, or a range that is invalid or already held, must
//     fail adoption BEFORE a tracker or registry record exists — Reconcile then
//     force-destroys the orphan instead of serving an agent whose ports the
//     next spawn could double-allocate;
//  2. a replayed live agent whose exact reservation cannot be re-established
//     is force-destroyed with no half-managed state left behind (no tracker
//     record, no port leak, no live registry record), and it is not processed
//     a second time by the orphan walk.
//
// Every test injects the destroy seam: nothing here may touch host users.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// destroyRecorder is the injected destroy seam. It records every call so a
// test can prove the destructive fallback ran exactly once, with force=true,
// and for the right agent — without ever touching a host user.
type destroyRecorder struct {
	mu    sync.Mutex
	ids   []string
	force []bool
	err   error
}

func (d *destroyRecorder) seam() func(context.Context, string, bool) (*v1.DestroyAgentResponse, error) {
	return func(_ context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.ids = append(d.ids, agentID)
		d.force = append(d.force, force)
		if d.err != nil {
			return nil, d.err
		}
		return &v1.DestroyAgentResponse{AgentId: agentID, Status: "destroyed"}, nil
	}
}

func (d *destroyRecorder) calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.ids...)
}

func (d *destroyRecorder) allForced() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, f := range d.force {
		if !f {
			return false
		}
	}
	return len(d.force) > 0
}

// failClosedManager wires a manager to a temp durable registry with a real
// port allocator and the recording destroy seam. adoptMode selects the
// reconciliation policy. The system probe still returns "no agents" until a
// test overrides it.
func failClosedManager(t *testing.T, path string, adoptMode bool) (*AgentManager, *destroyRecorder) {
	t.Helper()
	m := newRegistryManager(t, path)
	if adoptMode {
		m.cfg.Agent.Reconciliation.Mode = config.ReconcileModeAdopt
	}
	if m.portAlloc == nil {
		t.Fatal("test requires a configured port allocator")
	}
	rec := &destroyRecorder{}
	m.destroyAgent = rec.seam()
	return m, rec
}

// fullTrackerManager builds a manager whose tracker is already at capacity, so
// the next Register fails. That is the seam between "ports reserved" and
// "tracker record registered" in restoreAgent.
func fullTrackerManager(t *testing.T, path string) (*AgentManager, *destroyRecorder) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	cfg.Agent.Registry.Enabled = true
	cfg.Agent.Registry.Path = path
	cfg.Agent.Registry.MaxBytes = 1 << 20
	cfg.Agent.MaxAgents = 1
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	if err := tracker.Register(&resource.AgentRecord{AgentID: "filler", Status: "running"}); err != nil {
		t.Fatalf("seed tracker to capacity: %v", err)
	}
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	m.listSystemAgents = func() ([]SystemAgent, error) { return nil, nil }
	rec := &destroyRecorder{}
	m.destroyAgent = rec.seam()
	t.Cleanup(m.Stop)
	return m, rec
}

// writePortMetadata writes the per-agent `.bunker/ports` file spawn would
// have written.
func writePortMetadata(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".bunker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ports"), []byte(content), 0o644); err != nil {
		t.Fatalf("write port metadata: %v", err)
	}
}

// poolIsEmpty reports whether the allocator holds no reservation at all.
func poolIsEmpty(pa *resource.PortAllocator) bool { return pa.Available() == pa.MaxRanges() }

// assertNoHalfManagedState is the shared fail-closed contract: the agent has
// no tracker record, no port reservation and no live registry record.
func assertNoHalfManagedState(t *testing.T, m *AgentManager, agentID string) {
	t.Helper()
	if m.tracker.Get(agentID) != nil {
		t.Errorf("agent %q left a tracker record behind", agentID)
	}
	if m.portAlloc.Has(agentID) {
		t.Errorf("agent %q left a port reservation behind", agentID)
	}
	if m.registry.Get(agentID) != nil {
		t.Errorf("agent %q left a live registry record behind", agentID)
	}
}

// assertDestroyedOnce proves the destructive fallback ran exactly once, with
// force=true, for agentID.
func assertDestroyedOnce(t *testing.T, rec *destroyRecorder, agentID string) {
	t.Helper()
	got := rec.calls()
	if len(got) != 1 || got[0] != agentID {
		t.Fatalf("destroy calls = %v, want exactly one for %q", got, agentID)
	}
	if !rec.allForced() {
		t.Error("fail-closed destroy must run with force=true")
	}
}

// ── 1. Adoption is exact-port or nothing ───────────────────────

// TestReconcile_AdoptFailsClosedWithoutPortMetadata: an orphan whose
// `<home>/.bunker/ports` metadata is missing or malformed must NOT be adopted
// — no tracker record, no reservation, no live registry record — and must be
// destroyed instead.
func TestReconcile_AdoptFailsClosedWithoutPortMetadata(t *testing.T) {
	cases := []struct {
		name  string
		write func(t *testing.T, home string)
	}{
		{
			name:  "metadata file missing",
			write: func(t *testing.T, home string) {},
		},
		{
			name: "metadata file empty",
			write: func(t *testing.T, home string) {
				writePortMetadata(t, home, "")
			},
		},
		{
			name: "metadata not a range",
			write: func(t *testing.T, home string) {
				writePortMetadata(t, home, "not-a-port-range\n")
			},
		},
		{
			name: "metadata one-sided",
			write: func(t *testing.T, home string) {
				writePortMetadata(t, home, "12300\n")
			},
		},
		{
			name: "metadata non-numeric",
			write: func(t *testing.T, home string) {
				writePortMetadata(t, home, "abc-def\n")
			},
		},
		{
			name: "metadata path is a directory",
			write: func(t *testing.T, home string) {
				if err := os.MkdirAll(filepath.Join(home, ".bunker", "ports"), 0o755); err != nil {
					t.Fatalf("mkdir over metadata path: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			m, rec := failClosedManager(t, path, true)
			defer m.registry.Close()

			home := t.TempDir()
			tc.write(t, home)
			m.listSystemAgents = func() ([]SystemAgent, error) {
				return []SystemAgent{{AgentID: "orphan", Username: "bunker-orphan", Home: home}}, nil
			}

			rep := m.Reconcile(context.Background())

			if rep.Adopted != 0 {
				t.Errorf("Adopted = %d, want 0 (adoption without exact port metadata must fail)", rep.Adopted)
			}
			if rep.Destroyed != 1 {
				t.Errorf("Destroyed = %d, want 1 (failed adopt falls back to destroy) — report %+v", rep.Destroyed, rep)
			}
			if rep.Restored != 0 || rep.Purged != 0 {
				t.Errorf("unexpected reconcile actions: %+v", rep)
			}
			assertNoHalfManagedState(t, m, "orphan")
			assertDestroyedOnce(t, rec, "orphan")
			if !poolIsEmpty(m.portAlloc) {
				t.Errorf("port pool leaked: Available() = %d, MaxRanges() = %d",
					m.portAlloc.Available(), m.portAlloc.MaxRanges())
			}
		})
	}
}

// TestReconcile_AdoptFailsClosedOnInvalidOrCollidingRange: a persisted range
// that is not a legal, free pool sub-range must fail adoption (destroying the
// orphan) rather than registering the agent without its exact reservation.
func TestReconcile_AdoptFailsClosedOnInvalidOrCollidingRange(t *testing.T) {
	cases := []struct {
		name string
		// metadata written to <home>/.bunker/ports
		metadata string
		// heldRange pre-reserves a range for another agent before reconcile
		heldRange string
		// wantHeld reports whether the pre-existing reservation must survive
		wantHeld bool
	}{
		{
			name:     "misaligned range",
			metadata: "10050-10149\n",
		},
		{
			name:     "outside the pool",
			metadata: "30000-30099\n",
		},
		{
			name:     "inverted range",
			metadata: "10100-10000\n",
		},
		{
			name:     "wrong sub-range size",
			metadata: "12300-12398\n",
		},
		{
			name:      "range already held by another agent",
			metadata:  "12300-12399\n",
			heldRange: "12300-12399",
			wantHeld:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			m, rec := failClosedManager(t, path, true)
			defer m.registry.Close()

			if tc.heldRange != "" {
				var start, end uint32
				if _, err := fmt.Sscanf(tc.heldRange, "%d-%d", &start, &end); err != nil {
					t.Fatalf("parse held range: %v", err)
				}
				if err := m.portAlloc.Restore("holder", start, end); err != nil {
					t.Fatalf("pre-reserve holder range: %v", err)
				}
			}

			home := t.TempDir()
			writePortMetadata(t, home, tc.metadata)
			m.listSystemAgents = func() ([]SystemAgent, error) {
				return []SystemAgent{{AgentID: "orphan", Username: "bunker-orphan", Home: home}}, nil
			}

			rep := m.Reconcile(context.Background())

			if rep.Adopted != 0 {
				t.Errorf("Adopted = %d, want 0 (invalid/colliding range must not be adopted)", rep.Adopted)
			}
			if rep.Destroyed != 1 {
				t.Errorf("Destroyed = %d, want 1 — report %+v", rep.Destroyed, rep)
			}
			assertNoHalfManagedState(t, m, "orphan")
			assertDestroyedOnce(t, rec, "orphan")

			if tc.wantHeld {
				if !m.portAlloc.Has("holder") {
					t.Error("fail-closed cleanup released another agent's reservation")
				}
				if s, e, ok := m.portAlloc.AllocatedRange("holder"); !ok || s != 12300 || e != 12399 {
					t.Errorf("holder reservation = %d-%d (ok=%v), want it untouched at 12300-12399", s, e, ok)
				}
			}
			wantAvailable := m.portAlloc.MaxRanges()
			if tc.wantHeld {
				wantAvailable--
			}
			if got := m.portAlloc.Available(); got != wantAvailable {
				t.Errorf("Available() = %d, want %d (pool accounting after a failed adopt)", got, wantAvailable)
			}
		})
	}
}

// TestReconcile_AdoptStillWorksWithExactMetadata is the positive control: the
// fail-closed rules must not break a valid adoption.
func TestReconcile_AdoptStillWorksWithExactMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m, rec := failClosedManager(t, path, true)
	defer m.registry.Close()

	home := t.TempDir()
	writePortMetadata(t, home, "12300-12399\n")
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "orphan", Username: "bunker-orphan", Home: home}}, nil
	}

	rep := m.Reconcile(context.Background())
	if rep.Adopted != 1 || rep.Destroyed != 0 {
		t.Fatalf("report = %+v, want 1 adopted / 0 destroyed", rep)
	}
	if len(rec.calls()) != 0 {
		t.Errorf("valid adoption must not destroy the orphan (calls = %v)", rec.calls())
	}
	if s, e, ok := m.portAlloc.AllocatedRange("orphan"); !ok || s != 12300 || e != 12399 {
		t.Errorf("adopted reservation = %d-%d (ok=%v), want exact 12300-12399", s, e, ok)
	}
	if m.registry.Get("orphan") == nil {
		t.Error("adopted agent was not persisted to the registry")
	}
}

// ── 2. Restore is exact-port or destroy ────────────────────────

// replayRecord seeds a durable live record into path and returns a fresh
// manager that has replayed it.
func replayRecord(t *testing.T, rec *resource.AgentRecord) (*AgentManager, *destroyRecorder) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	seed := newRegistryManager(t, path)
	if err := seed.persistSpawn(rec); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	if err := seed.registry.Close(); err != nil {
		t.Fatalf("close seed registry: %v", err)
	}
	m, rec2 := failClosedManager(t, path, false)
	return m, rec2
}

// TestReconcile_RestoreFailsClosedOnUnrestorableRange: a replayed live agent
// whose exact reservation cannot be re-established must be force-destroyed,
// with the tracker record, port reservation and live registry record all
// absent afterwards.
func TestReconcile_RestoreFailsClosedOnUnrestorableRange(t *testing.T) {
	const agentID = "replayed"

	cases := []struct {
		name string
		// seed mutates the persisted record before it is written
		seed func(rec *resource.AgentRecord)
		// heldRange pre-reserves another agent's range before reconcile
		heldRange string
		wantHeld  bool
	}{
		{
			name: "no persisted range",
			seed: func(rec *resource.AgentRecord) {
				rec.PortRangeStart, rec.PortRangeEnd = 0, 0
			},
		},
		{
			name: "misaligned persisted range",
			seed: func(rec *resource.AgentRecord) {
				rec.PortRangeStart, rec.PortRangeEnd = 10050, 10149
			},
		},
		{
			name: "persisted range outside the pool",
			seed: func(rec *resource.AgentRecord) {
				rec.PortRangeStart, rec.PortRangeEnd = 30000, 30099
			},
		},
		{
			name: "persisted range collides with a held reservation",
			seed: func(rec *resource.AgentRecord) {
				rec.PortRangeStart, rec.PortRangeEnd = 12300, 12399
			},
			heldRange: "12300-12399",
			wantHeld:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &resource.AgentRecord{
				AgentID:        agentID,
				Status:         "running",
				CreatedAt:      time.Now().Add(-time.Hour),
				ExpiresAt:      time.Now().Add(time.Hour),
				PortRangeStart: 10000,
				PortRangeEnd:   10099,
			}
			tc.seed(rec)

			m, destroyRec := replayRecord(t, rec)
			defer m.registry.Close()
			if m.registry.LiveCount() != 1 {
				t.Fatalf("replay live count = %d, want 1", m.registry.LiveCount())
			}
			if tc.heldRange != "" {
				var start, end uint32
				if _, err := fmt.Sscanf(tc.heldRange, "%d-%d", &start, &end); err != nil {
					t.Fatalf("parse held range: %v", err)
				}
				if err := m.portAlloc.Restore("holder", start, end); err != nil {
					t.Fatalf("pre-reserve holder range: %v", err)
				}
			}
			m.listSystemAgents = func() ([]SystemAgent, error) {
				return []SystemAgent{{AgentID: agentID, Username: "bunker-" + agentID, Home: "/home/bunker-" + agentID}}, nil
			}

			rep := m.Reconcile(context.Background())

			if rep.Restored != 0 {
				t.Errorf("Restored = %d, want 0 (an unrestorable agent must not be restored)", rep.Restored)
			}
			if rep.Destroyed != 1 {
				t.Errorf("Destroyed = %d, want 1 — report %+v", rep.Destroyed, rep)
			}
			if rep.Purged != 0 || rep.Adopted != 0 {
				t.Errorf("unexpected reconcile actions: %+v", rep)
			}
			assertNoHalfManagedState(t, m, agentID)
			assertDestroyedOnce(t, destroyRec, agentID)

			wantAvailable := m.portAlloc.MaxRanges()
			if tc.wantHeld {
				wantAvailable--
				if !m.portAlloc.Has("holder") {
					t.Error("fail-closed cleanup released another agent's reservation")
				}
			}
			if got := m.portAlloc.Available(); got != wantAvailable {
				t.Errorf("Available() = %d, want %d (no port leak from a failed restore)", got, wantAvailable)
			}
			// The unsafe live record is gone, so a restart cannot resurrect it.
			if m.registry.LiveCount() != 0 {
				t.Errorf("LiveCount = %d, want 0 after a fail-closed destroy", m.registry.LiveCount())
			}
		})
	}
}

// TestReconcile_RestoreReleasesReservationWhenTrackerRegistrationFails pins
// the order contract inside restoreAgent: ports are reserved BEFORE the
// tracker record is registered, and a failed registration gives the
// reservation back instead of leaking pool capacity.
func TestReconcile_RestoreReleasesReservationWhenTrackerRegistrationFails(t *testing.T) {
	const agentID = "no-capacity"
	rec := &resource.AgentRecord{
		AgentID:        agentID,
		Status:         "running",
		CreatedAt:      time.Now().Add(-time.Hour),
		ExpiresAt:      time.Now().Add(time.Hour),
		PortRangeStart: 10000,
		PortRangeEnd:   10099,
	}

	path := filepath.Join(t.TempDir(), "agents.jsonl")
	seed := newRegistryManager(t, path)
	if err := seed.persistSpawn(rec); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	if err := seed.registry.Close(); err != nil {
		t.Fatalf("close seed registry: %v", err)
	}

	m, destroyRec := fullTrackerManager(t, path)
	defer m.registry.Close()
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: agentID, Username: "bunker-" + agentID, Home: "/home/bunker-" + agentID}}, nil
	}

	rep := m.Reconcile(context.Background())

	if rep.Restored != 0 {
		t.Errorf("Restored = %d, want 0 (the tracker had no capacity)", rep.Restored)
	}
	if rep.Destroyed != 1 {
		t.Errorf("Destroyed = %d, want 1 — report %+v", rep.Destroyed, rep)
	}
	assertNoHalfManagedState(t, m, agentID)
	assertDestroyedOnce(t, destroyRec, agentID)
	if !poolIsEmpty(m.portAlloc) {
		t.Errorf("failed tracker registration leaked a reservation: Available() = %d, MaxRanges() = %d",
			m.portAlloc.Available(), m.portAlloc.MaxRanges())
	}
}

// TestReconcile_FailClosedAgentIsNotReprocessedAsOrphan: dropping the unsafe
// live record makes the agent look like an orphan to the second walk. It must
// be consumed exactly once — one destroy, not a destroy plus a re-adopt (or a
// second destroy) in adopt mode.
func TestReconcile_FailClosedAgentIsNotReprocessedAsOrphan(t *testing.T) {
	const agentID = "unsafe-live"
	rec := &resource.AgentRecord{
		AgentID:        agentID,
		Status:         "running",
		CreatedAt:      time.Now().Add(-time.Hour),
		ExpiresAt:      time.Now().Add(time.Hour),
		PortRangeStart: 10050, // misaligned: not restorable
		PortRangeEnd:   10149,
	}

	path := filepath.Join(t.TempDir(), "agents.jsonl")
	seed := newRegistryManager(t, path)
	if err := seed.persistSpawn(rec); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	if err := seed.registry.Close(); err != nil {
		t.Fatalf("close seed registry: %v", err)
	}

	// Adopt mode: without the handled-set an agent whose live record was just
	// dropped would be re-adopted here (and destroyed a second time).
	m, destroyRec := failClosedManager(t, path, true)
	defer m.registry.Close()

	home := t.TempDir()
	writePortMetadata(t, home, "10050-10149\n") // still not a legal sub-range
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: agentID, Username: "bunker-" + agentID, Home: home}}, nil
	}

	rep := m.Reconcile(context.Background())

	if rep.Restored != 0 || rep.Adopted != 0 || rep.Destroyed != 1 {
		t.Errorf("report = %+v, want 0 restored / 0 adopted / 1 destroyed", rep)
	}
	assertDestroyedOnce(t, destroyRec, agentID)
	assertNoHalfManagedState(t, m, agentID)
}

// TestReconcile_FailClosedRestoreDestroyFailureStillFailsClosed: when the
// destroy seam itself fails, the agent must still not be left half-managed —
// no tracker record, no reservation, no live registry record.
func TestReconcile_FailClosedRestoreDestroyFailureStillFailsClosed(t *testing.T) {
	const agentID = "destroy-fails"
	rec := &resource.AgentRecord{
		AgentID:        agentID,
		Status:         "running",
		CreatedAt:      time.Now().Add(-time.Hour),
		ExpiresAt:      time.Now().Add(time.Hour),
		PortRangeStart: 10050,
		PortRangeEnd:   10149,
	}

	m, destroyRec := replayRecord(t, rec)
	defer m.registry.Close()
	destroyRec.err = fmt.Errorf("destroy seam unavailable")
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: agentID, Username: "bunker-" + agentID, Home: "/home/bunker-" + agentID}}, nil
	}

	rep := m.Reconcile(context.Background())

	if len(destroyRec.calls()) != 1 {
		t.Errorf("destroy calls = %v, want exactly one attempt", destroyRec.calls())
	}
	if rep.Destroyed != 0 {
		t.Errorf("Destroyed = %d, want 0 (the destroy seam failed)", rep.Destroyed)
	}
	if rep.Restored != 0 {
		t.Errorf("Restored = %d, want 0", rep.Restored)
	}
	assertNoHalfManagedState(t, m, agentID)
	if !poolIsEmpty(m.portAlloc) {
		t.Errorf("port pool leaked: Available() = %d, MaxRanges() = %d",
			m.portAlloc.Available(), m.portAlloc.MaxRanges())
	}
}
