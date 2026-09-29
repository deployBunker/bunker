package agent

// REV-BUNKER-002 regression tests: orphan classification must not fail OPEN.
//
// Before this row, an orphan whose <home>/.bunker/owner or <home>/.bunker/ports
// metadata was MISSING or UNREADABLE was classified "ours" and destroyed; only
// the readable-foreign shape was protected. This is fail-open data loss:
// unreadable metadata is indistinguishable from another daemon's root-only
// agent.
//
// The fix adds a third disposition — UNPROVEN — for orphans whose decisive
// metadata is missing or unreadable. They are skipped (never destroyed),
// counted in ReconcileReport.Unproven, and reported in ONE warning naming the
// agent id and the cause. Malformed-but-readable metadata keeps the fail-closed
// destroyed treatment (a readable garbage file is this daemon's own corrupted
// write, not a foreign daemon's root-only metadata).

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unprovenOrphanManager wires a manager with an OWN daemon identity (so the
// ownership marker branch is live) plus the recording destroy seam and a
// captured log — the same shape ownerManager uses.
func unprovenOrphanManager(t *testing.T, path string, adoptMode bool, buf *bytes.Buffer) (*AgentManager, *destroyRecorder) {
	t.Helper()
	return ownerManager(t, path, ownInstanceID, adoptMode, buf)
}

// writePortMetadataDir turns the `<home>/.bunker/ports` path into a directory,
// so a subsequent read fails with EISDIR (unreadable) rather than not-exist.
func writePortMetadataDir(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".bunker", "ports"), 0o755); err != nil {
		t.Fatalf("mkdir over metadata path: %v", err)
	}
}

// writeOwnerMarkerDir turns the `<home>/.bunker/owner` path into a directory,
// so a subsequent read fails with EISDIR (unreadable) rather than not-exist.
func writeOwnerMarkerDir(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".bunker", "owner"), 0o755); err != nil {
		t.Fatalf("mkdir over marker path: %v", err)
	}
}

// TestOrphanClassification pins the three-way disposition directly, covering
// the five shapes the row names: readable-foreign (disjoint range and a foreign
// marker), metadata-missing, metadata-unreadable, in-pool-unaligned (ours), and
// malformed (ours).
func TestOrphanClassification(t *testing.T) {
	cases := []struct {
		name     string
		owner    string // "" = absent; "dir" = directory (unreadable); "other" = foreign id; "own" = this id
		ports    string // "" = absent; "dir" = directory (unreadable); otherwise a range string
		want     orphanDisposition
		wantLow  uint32
		wantHigh uint32
	}{
		{name: "readable foreign: disjoint range, no marker", ports: "30000-30099\n", want: orphanForeign, wantLow: 30000, wantHigh: 30099},
		{name: "readable foreign: foreign marker, in-pool range", owner: "other", ports: "12300-12399\n", want: orphanForeign, wantLow: 12300, wantHigh: 12399},
		{name: "ours: in-pool range, no marker", ports: "12300-12399\n", want: orphanOurs, wantLow: 12300, wantHigh: 12399},
		{name: "ours: own marker", owner: "own", ports: "12300-12399\n", want: orphanOurs, wantLow: 12300, wantHigh: 12399},
		{name: "ours: in-pool unaligned range", ports: "10050-10149\n", want: orphanOurs, wantLow: 10050, wantHigh: 10149},
		{name: "ours: malformed port metadata", ports: "not-a-range\n", want: orphanOurs},
		{name: "unproven: port metadata missing", want: orphanUnproven},
		{name: "unproven: port metadata unreadable", ports: "dir", want: orphanUnproven},
		{name: "unproven: owner marker unreadable, even with in-pool ports", owner: "dir", ports: "12300-12399\n", want: orphanUnproven},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newRegistryManager(t, filepath.Join(t.TempDir(), "agents.jsonl"))
			defer m.registry.Close()
			m.instanceID = ownInstanceID

			home := t.TempDir()
			switch tc.ports {
			case "":
			case "dir":
				writePortMetadataDir(t, home)
			default:
				writePortMetadata(t, home, tc.ports)
			}
			switch tc.owner {
			case "":
			case "dir":
				writeOwnerMarkerDir(t, home)
			case "other":
				seedOwnerMarker(t, home, otherInstanceID, "10000-19999")
			case "own":
				seedOwnerMarker(t, home, ownInstanceID, "10000-19999")
			}

			got, start, end, _ := m.classifyOrphan(SystemAgent{AgentID: "orphan", Username: "bunker-orphan", Home: home})
			if got != tc.want {
				t.Errorf("classifyOrphan = %v, want %v", got, tc.want)
			}
			if start != tc.wantLow || end != tc.wantHigh {
				t.Errorf("reported range = %d-%d, want %d-%d", start, end, tc.wantLow, tc.wantHigh)
			}
		})
	}
}

// TestReconcile_UnprovenOrphansAreSkippedNotDestroyed is the reconcile-level
// contract: an orphan whose ownership/port metadata is missing or unreadable is
// SKIPPED in both modes — never destroyed, never adopted, never foreign — and
// counted in Unproven with ONE warning naming the agent and the cause.
func TestReconcile_UnprovenOrphansAreSkippedNotDestroyed(t *testing.T) {
	cases := []struct {
		name      string
		adoptMode bool
		owner     string // "" = absent; "dir" = directory (unreadable)
		ports     string // "" = absent; "dir" = directory (unreadable); otherwise a range
	}{
		{name: "port metadata missing, adopt mode", adoptMode: true},
		{name: "port metadata missing, destroy mode"},
		{name: "port metadata unreadable, adopt mode", adoptMode: true, ports: "dir"},
		{name: "port metadata unreadable, destroy mode", ports: "dir"},
		{name: "owner marker unreadable, in-pool ports, adopt mode", adoptMode: true, owner: "dir", ports: "12300-12399\n"},
		{name: "owner marker unreadable, in-pool ports, destroy mode", owner: "dir", ports: "12300-12399\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			var buf bytes.Buffer
			m, rec := unprovenOrphanManager(t, path, tc.adoptMode, &buf)
			defer m.registry.Close()

			home := t.TempDir()
			switch tc.ports {
			case "":
			case "dir":
				writePortMetadataDir(t, home)
			default:
				writePortMetadata(t, home, tc.ports)
			}
			if tc.owner == "dir" {
				writeOwnerMarkerDir(t, home)
			}
			m.listSystemAgents = func() ([]SystemAgent, error) {
				return []SystemAgent{{AgentID: "orphan", Username: "bunker-orphan", Home: home}}, nil
			}

			rep := m.Reconcile(context.Background())

			if rep.Unproven != 1 {
				t.Errorf("Unproven = %d, want 1 — report %+v", rep.Unproven, rep)
			}
			if rep.Destroyed != 0 || rep.Adopted != 0 || rep.Foreign != 0 {
				t.Errorf("report = %+v, want 1 unproven and 0 destroyed/adopted/foreign", rep)
			}
			if got := rec.calls(); len(got) != 0 {
				t.Errorf("destroy calls = %v, want 0 — an unproven orphan must never be destroyed", got)
			}
			assertNoHalfManagedState(t, m, "orphan")
			log := buf.String()
			if !strings.Contains(log, "skipping unproven orphan agent") {
				t.Errorf("unproven skip warning missing — log:\n%s", log)
			}
			if !strings.Contains(log, "agent_id=orphan") {
				t.Errorf("unproven skip warning must name the agent id — log:\n%s", log)
			}
		})
	}
}
