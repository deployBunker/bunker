package agent

// REV-BUNKER-P1-PATCH arms — the behaviour cells, written against surface that
// existed BEFORE the row.
//
// This file deliberately uses NO symbol the row adds (no ReconcileReport.Refused,
// no store provenance accessor, no reconciliation knob beyond `mode`), so the RED
// is reproducible with the FINAL test text: the arms script swaps the base blobs
// of the three product files in and sets the row's GREEN-only files aside, and the
// cells below still compile and still exercise the orphan walk.
//
// What the cells are:
//
//   - UnprovenMassDestroyIsRefused / EmptyRegistryFileIsRefused — the DEFECT
//     cells (RED on the tree as filed): a daemon whose durable registry cannot
//     vouch for the host — the file did not exist and Open fabricated an empty
//     one, or it exists but replayed ZERO live records — finds unknown bunker-*
//     users and destroys them. On the filed tree these FAIL with a populated
//     destroy list; with the guard they pass with an empty one.
//
//   - SmallUnprovenSweepStillHappens / ProvenRegistrySweepsLargeSets /
//     AdoptModeStillAdopts — the NON-VACUITY controls. They pass on both trees,
//     which is what attributes the two failures above to the row's defect rather
//     than to a harness that refuses everything, and they pin the two properties
//     the guard must not break: a handful of genuine leftovers is still swept, and
//     a registry that CAN vouch for the host still sweeps any number of leftovers.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/resource"
)

// sweepOrphanHost fabricates n unknown bunker-* users with real home
// directories under one temp root. Real homes (not bare structs) matter: the
// adopt/adopt-failure paths read `<home>/.bunker/ports` off them.
func sweepOrphanHost(t *testing.T, n int) []SystemAgent {
	t.Helper()
	root := t.TempDir()
	out := make([]SystemAgent, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("host%02d", i)
		home := filepath.Join(root, "bunker-"+id)
		if err := os.MkdirAll(home, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", home, err)
		}
		out = append(out, SystemAgent{AgentID: id, Username: "bunker-" + id, Home: home})
	}
	return out
}

// stubUsers resolves uid/gid for every name in names (and only those) — the
// adopt stage looks the agent's user up, and the single-name stubUser helper in
// reconcile_adopt_limits_test.go cannot serve eight orphans.
func stubUsers(t *testing.T, names ...string) {
	t.Helper()
	known := make(map[string]bool, len(names))
	for _, n := range names {
		known[n] = true
	}
	prev := lookupAgentUser
	lookupAgentUser = func(name string) (*user.User, error) {
		if !known[name] {
			return nil, fmt.Errorf("user %q not found (test stub only knows %d names)", name, len(known))
		}
		return &user.User{Username: name, Uid: "4242", Gid: "4242", HomeDir: "/home/" + name}, nil
	}
	t.Cleanup(func() { lookupAgentUser = prev })
}

// TestReconcileSweepGuardArms_UnprovenMassDestroyIsRefused is the DEFECT cell in
// its original shape: the registry file does not exist, so Open creates it EMPTY
// and the daemon believes it has no agents, while the host carries four unknown
// bunker-* users. Nothing may be destroyed.
func TestReconcileSweepGuardArms_UnprovenMassDestroyIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("precondition: the registry file must not exist yet (stat err = %v)", err)
	}
	var buf bytes.Buffer
	m, rec := foreignManager(t, path, false, &buf)
	defer m.registry.Close()

	m.listSystemAgents = func() ([]SystemAgent, error) { return sweepOrphanHost(t, 4), nil }
	rep := m.Reconcile(context.Background())

	if got := rec.calls(); len(got) != 0 {
		t.Errorf("the unproven sweep destroyed %d unknown bunker-* users (%v): a registry that does not exist on a host "+
			"this daemon has never seen must destroy NOTHING", len(got), got)
	}
	if rep.Destroyed != 0 {
		t.Errorf("Destroyed = %d, want 0 — an absent registry is not evidence that the host has no agents", rep.Destroyed)
	}
}

// TestReconcileSweepGuardArms_EmptyRegistryFileIsRefused is the SECOND provenance
// clause in isolation: the file exists (a previous boot created it, or it was
// truncated) but replayed ZERO live records. "The durable state says no agents"
// and "there is no durable state" are both unproven against a host full of users
// this daemon does not know.
func TestReconcileSweepGuardArms_EmptyRegistryFileIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	seed := newRegistryManager(t, path)
	if err := seed.registry.Close(); err != nil {
		t.Fatalf("close seeded registry: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("precondition: the seeded registry file must EXIST (stat err = %v)", err)
	}

	var buf bytes.Buffer
	m, rec := foreignManager(t, path, false, &buf)
	defer m.registry.Close()
	if m.registry.LiveCount() != 0 {
		t.Fatalf("precondition: the replayed live set must be empty, got %d", m.registry.LiveCount())
	}

	m.listSystemAgents = func() ([]SystemAgent, error) { return sweepOrphanHost(t, 4), nil }
	rep := m.Reconcile(context.Background())

	if got := rec.calls(); len(got) != 0 {
		t.Errorf("an EMPTY (but present) registry destroyed %d unknown bunker-* users (%v): an empty live set is the "+
			"absence of evidence, not evidence of absence", len(got), got)
	}
	if rep.Destroyed != 0 {
		t.Errorf("Destroyed = %d, want 0", rep.Destroyed)
	}
}

// TestReconcileSweepGuardArms_SmallUnprovenSweepStillHappens is the control that
// keeps the guard from becoming a no-op-with-a-clean-conscience: a handful of
// leftovers (three) on an unproven registry is STILL swept. Without this cell the
// two defect cells above could be passed by refusing every sweep.
func TestReconcileSweepGuardArms_SmallUnprovenSweepStillHappens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	var buf bytes.Buffer
	m, rec := foreignManager(t, path, false, &buf)
	defer m.registry.Close()

	m.listSystemAgents = func() ([]SystemAgent, error) { return sweepOrphanHost(t, 3), nil }
	rep := m.Reconcile(context.Background())

	if got := rec.calls(); len(got) != 3 {
		t.Errorf("destroy calls = %v, want the 3 real leftovers swept", got)
	}
	if rep.Destroyed != 3 {
		t.Errorf("Destroyed = %d, want 3 — a handful of genuine leftovers must still be swept", rep.Destroyed)
	}
}

// TestReconcileSweepGuardArms_ProvenRegistrySweepsLargeSets is the control for the
// OTHER direction: when the registry pre-existed and replayed a live record the
// host still has, provenance is proven and an arbitrarily large leftover set is
// swept normally. The guard is scoped to unproven provenance, not to size.
func TestReconcileSweepGuardArms_ProvenRegistrySweepsLargeSets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	first := newRegistryManager(t, path)
	if err := first.persistSpawn(&resource.AgentRecord{
		AgentID:           "live-one",
		Status:            "running",
		CreatedAt:         time.Now().Add(-time.Hour),
		ExpiresAt:         time.Now().Add(5 * time.Hour),
		PortRangeStart:    10000,
		PortRangeEnd:      10099,
		SshPrivateKeyPath: "/etc/bunkerd/ssh/live-one",
	}); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	if err := first.registry.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var buf bytes.Buffer
	m, rec := foreignManager(t, path, false, &buf)
	defer m.registry.Close()
	if m.registry.LiveCount() != 1 {
		t.Fatalf("precondition: the registry must replay 1 live record, got %d", m.registry.LiveCount())
	}

	orphans := sweepOrphanHost(t, 8)
	withLive := append([]SystemAgent{{
		AgentID: "live-one", Username: "bunker-live-one", Home: t.TempDir(),
	}}, orphans...)
	m.listSystemAgents = func() ([]SystemAgent, error) { return withLive, nil }

	rep := m.Reconcile(context.Background())

	if rep.Restored != 1 {
		t.Errorf("Restored = %d, want 1 (the replayed live agent)", rep.Restored)
	}
	if rep.Destroyed != 8 {
		t.Errorf("Destroyed = %d, want 8 — a registry that CAN vouch for the host sweeps its leftovers", rep.Destroyed)
	}
	if got := rec.calls(); len(got) != 8 {
		t.Errorf("destroy calls = %v, want the 8 leftovers", got)
	}
}

// TestReconcileSweepGuardArms_AdoptModeStillAdopts is the control that pins adopt
// mode as untouched: adopting re-registers an orphan and deletes nothing, so the
// mass-destroy shape the guard exists for does not exist there. Eight adoptable
// orphans (readable, valid, free in-pool port metadata) are all adopted, and the
// destroy seam is never called.
func TestReconcileSweepGuardArms_AdoptModeStillAdopts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	var buf bytes.Buffer
	m, rec := foreignManager(t, path, true, &buf)
	defer m.registry.Close()

	orphans := sweepOrphanHost(t, 8)
	names := make([]string, 0, len(orphans))
	for i, sa := range orphans {
		writePortMetadata(t, sa.Home, fmt.Sprintf("%d-%d\n", 11000+i*100, 11099+i*100))
		names = append(names, sa.Username)
	}
	stubUsers(t, names...)
	installNoopAdoptSeams(t, m)
	m.listSystemAgents = func() ([]SystemAgent, error) { return orphans, nil }

	rep := m.Reconcile(context.Background())

	if got := rec.calls(); len(got) != 0 {
		t.Errorf("adopt mode destroyed %v — adoption is not a destroy path", got)
	}
	if rep.Adopted != 8 {
		t.Errorf("Adopted = %d, want 8 (adopt mode is untouched by the sweep guard); log:\n%s", rep.Adopted, buf.String())
	}
	if rep.Destroyed != 0 {
		t.Errorf("Destroyed = %d, want 0 in adopt mode", rep.Destroyed)
	}
}
