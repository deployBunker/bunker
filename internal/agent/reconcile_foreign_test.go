package agent

// DF-BUNKER-13 regression tests: a second bunkerd on the same host shares
// the host's bunker-* user namespace, so the orphan walk sees OTHER daemons'
// agents. An orphan whose persisted port range is disjoint from this
// daemon's pool cannot collide with any port this daemon allocates — it is
// FOREIGN and must never be destroyed here, in adopt mode or in the default
// destroy mode. Unreadable/malformed metadata and in-pool-but-unreserveable
// ranges keep their existing fail-closed treatment (destroyed exactly as
// before).
//
// Every test injects the destroy seam: nothing here may touch host users.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
)

// foreignManager wires a manager to a temp durable registry with a real port
// allocator on the default test pool (10000-19999) and the recording destroy
// seam; log output is captured in buf so tests can assert the foreign-skip
// warning's fields. adoptMode selects the reconciliation policy (false = the
// destroy default).
func foreignManager(t *testing.T, path string, adoptMode bool, buf *bytes.Buffer) (*AgentManager, *destroyRecorder) {
	t.Helper()
	m := newRegistryManager(t, path)
	if adoptMode {
		m.cfg.Agent.Reconciliation.Mode = config.ReconcileModeAdopt
	}
	if m.portAlloc == nil {
		t.Fatal("test requires a configured port allocator")
	}
	m.logger = slog.New(slog.NewTextHandler(buf, nil))
	rec := &destroyRecorder{}
	m.destroyAgent = rec.seam()
	return m, rec
}

// assertForeignSkipWarning pins the loud-warning contract (AC4): the line
// names the agent id, its persisted range, this daemon's pool bounds, and a
// reason pointing at the owning daemon.
func assertForeignSkipWarning(t *testing.T, log string) {
	t.Helper()
	for _, want := range []string{
		"skipping foreign orphan agent",
		"agent_id=foreign-agent",
		"system_user=bunker-foreign-agent",
		"persisted_range=30000-30099",
		"pool=10000-19999",
		"another daemon instance",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("foreign-skip warning missing %q — log line:\n%s", want, log)
		}
	}
}

// TestReconcile_ForeignOrphanIsNeverDestroyed: an orphan whose
// `<home>/.bunker/ports` range (30000-30099) is disjoint from this daemon's
// pool (10000-19999) is skipped in BOTH modes — zero destroy calls,
// Foreign == 1, and no tracker record, port reservation or registry record
// created for it.
func TestReconcile_ForeignOrphanIsNeverDestroyed(t *testing.T) {
	cases := []struct {
		name      string
		adoptMode bool
	}{
		{name: "adopt mode", adoptMode: true},
		{name: "destroy mode (default)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			var buf bytes.Buffer
			m, rec := foreignManager(t, path, tc.adoptMode, &buf)
			defer m.registry.Close()

			home := t.TempDir()
			// 30000-30099 lies entirely outside this daemon's 10000-19999
			// pool: the agent belongs to another daemon instance (the live
			// dogfood defect: daemon #2 destroyed daemon #1's agent here).
			writePortMetadata(t, home, "30000-30099\n")
			m.listSystemAgents = func() ([]SystemAgent, error) {
				return []SystemAgent{{AgentID: "foreign-agent", Username: "bunker-foreign-agent", Home: home}}, nil
			}

			rep := m.Reconcile(context.Background())

			if got := rec.calls(); len(got) != 0 {
				t.Errorf("destroy calls = %v, want 0 — a foreign orphan must never be destroyed", got)
			}
			if rep.Foreign != 1 {
				t.Errorf("Foreign = %d, want 1 — report %+v", rep.Foreign, rep)
			}
			if rep.Destroyed != 0 || rep.Adopted != 0 {
				t.Errorf("report = %+v, want 0 destroyed / 0 adopted", rep)
			}
			// No half-managed state and no adoption side effects for a
			// foreign agent: it stays exactly as the OTHER daemon left it.
			assertNoHalfManagedState(t, m, "foreign-agent")
			if !poolIsEmpty(m.portAlloc) {
				t.Errorf("foreign skip leaked pool capacity: Available() = %d, MaxRanges() = %d",
					m.portAlloc.Available(), m.portAlloc.MaxRanges())
			}
			assertForeignSkipWarning(t, buf.String())
		})
	}
}

// TestOrphanIsForeignClassification pins the classifier's boundaries
// directly: only a PROVABLY disjoint range is foreign; boundary-touching
// ranges overlap the pool and stay fail-closed (overlapping pools remain
// unsupported), and unreadable metadata is never foreign.
func TestOrphanIsForeignClassification(t *testing.T) {
	cases := []struct {
		name      string
		metadata  string // written to <home>/.bunker/ports; "" writes nothing
		want      bool
		wantRange [2]uint32
	}{
		{name: "below the pool", metadata: "9000-9099\n", want: true, wantRange: [2]uint32{9000, 9099}},
		{name: "above the pool", metadata: "30000-30099\n", want: true, wantRange: [2]uint32{30000, 30099}},
		{name: "first sub-range touches pool start", metadata: "10000-10099\n", wantRange: [2]uint32{10000, 10099}},
		{name: "last sub-range touches pool end", metadata: "19900-19999\n", wantRange: [2]uint32{19900, 19999}},
		{name: "straddles pool start (overlap)", metadata: "9990-10050\n", wantRange: [2]uint32{9990, 10050}},
		{name: "straddles pool end (overlap)", metadata: "19950-20050\n", wantRange: [2]uint32{19950, 20050}},
		{name: "in-pool misaligned (overlap class)", metadata: "10050-10149\n", wantRange: [2]uint32{10050, 10149}},
		{name: "malformed metadata is not foreign", metadata: "not-a-range\n"},
		{name: "missing metadata is not foreign", metadata: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			m := newRegistryManager(t, path)
			defer m.registry.Close()

			home := t.TempDir()
			if tc.metadata != "" {
				writePortMetadata(t, home, tc.metadata)
			}

			got, start, end := m.orphanIsForeign(SystemAgent{
				AgentID: "orphan", Username: "bunker-orphan", Home: home,
			})
			if got != tc.want {
				t.Errorf("orphanIsForeign = %v, want %v", got, tc.want)
			}
			// The reported range must echo the persisted metadata (or 0,0
			// when nothing readable was found).
			if start != tc.wantRange[0] || end != tc.wantRange[1] {
				t.Errorf("reported range = %d-%d, want %d-%d", start, end, tc.wantRange[0], tc.wantRange[1])
			}
		})
	}
}

// TestReconcile_FailClosedOrphansAreStillDestroyed is the no-regression
// contract (AC3): orphans the daemon cannot prove safe — missing/malformed
// metadata, in-pool ranges that cannot be reserved — are STILL destroyed
// exactly as before, in both modes. Foreign must stay 0 for them.
func TestReconcile_FailClosedOrphansAreStillDestroyed(t *testing.T) {
	cases := []struct {
		name      string
		adoptMode bool
		metadata  string
		heldRange string
		wantHeld  bool
	}{
		{name: "missing metadata, adopt mode", adoptMode: true},
		{name: "malformed metadata, adopt mode", adoptMode: true, metadata: "not-a-range\n"},
		{name: "in-pool range already held, adopt mode", adoptMode: true,
			metadata: "12300-12399\n", heldRange: "12300-12399", wantHeld: true},
		{name: "in-pool misaligned range, adopt mode", adoptMode: true, metadata: "10050-10149\n"},
		{name: "missing metadata, destroy mode"},
		{name: "malformed metadata, destroy mode", metadata: "abc-def\n"},
		{name: "in-pool range already held, destroy mode",
			metadata: "12300-12399\n", heldRange: "12300-12399", wantHeld: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			var buf bytes.Buffer
			m, rec := foreignManager(t, path, tc.adoptMode, &buf)
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
			if tc.metadata != "" {
				writePortMetadata(t, home, tc.metadata)
			}
			m.listSystemAgents = func() ([]SystemAgent, error) {
				return []SystemAgent{{AgentID: "orphan", Username: "bunker-orphan", Home: home}}, nil
			}

			rep := m.Reconcile(context.Background())

			if rep.Destroyed != 1 {
				t.Errorf("Destroyed = %d, want 1 (fail-closed must keep destroying) — report %+v", rep.Destroyed, rep)
			}
			if rep.Foreign != 0 {
				t.Errorf("Foreign = %d, want 0 (unreadable/in-pool metadata is NOT foreign)", rep.Foreign)
			}
			if strings.Contains(buf.String(), "skipping foreign orphan agent") {
				t.Error("fail-closed destroy must not be logged as a foreign skip")
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
				t.Errorf("Available() = %d, want %d (pool accounting after the fail-closed destroy)", got, wantAvailable)
			}
		})
	}
}
