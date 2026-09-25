package agent

// INT-CI-042 regression tests: a replayed live registry record whose
// persisted port range lies ENTIRELY OUTSIDE this daemon's pool is pool
// drift, not a defect, and must not destroy the agent.
//
// WHY the expectation flipped: this row used to live in
// reconcile_failclosed_test.go and expected a force-destroy — byte-for-byte
// the defect behind CI run 36182283120 (job `regression`, step `Regression
// suite`, exit 3). The battery started the daemon on pool 20000-20999 while
// the agent's registry record carried a reservation from the old
// 10000-10099 pool; restoreAgent's Restore() rejected the out-of-pool range
// and failClosedRestore force-destroyed a healthy agent (rootless dockerd
// SIGTERMed, home archived and deleted).
//
// The DF-BUNKER-13 orphan-walk precedent (orphanIsForeign) already encodes
// the correct rule for exactly this geometry: a range disjoint from this
// daemon's pool cannot collide with any port this daemon allocates, so
// destroying the agent buys nothing and loses a healthy agent. The registry
// replay walk now follows the same principle: restore the tracker record,
// leave the pool's live accounting alone, emit ONE warning naming the
// geometry, and count the agent in ReconcileReport.RestoredForeignPool.
// Skipping pool accounting is safe by construction: spawn allocates
// EXCLUSIVELY through the allocator, which only ever hands out in-pool
// ranges — an out-of-pool reservation that is not tracked in the allocator
// can therefore never be handed out again.
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
	"time"

	"github.com/deployBunker/bunker/internal/resource"
)

// poolDriftManager wires a manager to a temp durable registry with a port
// allocator on pool 20000-20999 (the CI battery geometry that red) and the
// recording destroy seam; log output is captured in buf so the test can
// assert the drift warning's fields.
func poolDriftManager(t *testing.T, path string, buf *bytes.Buffer) (*AgentManager, *destroyRecorder) {
	t.Helper()
	m := newRegistryManager(t, path)
	pa, err := resource.NewPortAllocator(20000, 20999, 100)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}
	m.portAlloc = pa
	m.logger = slog.New(slog.NewTextHandler(buf, nil))
	rec := &destroyRecorder{}
	m.destroyAgent = rec.seam()
	return m, rec
}

// TestReconcile_ForeignPoolRangeIsRestoredNotDestroyed replays a live agent
// whose persisted range is disjoint from the configured pool (one case
// entirely below the pool, one entirely above) and proves the flipped
// contract: the agent is restored, nothing is destroyed, the drift is
// counted in RestoredForeignPool (never double-counted under Restored), and
// one warning names agent id, persisted range, pool bounds and the cause.
func TestReconcile_ForeignPoolRangeIsRestoredNotDestroyed(t *testing.T) {
	const agentID = "drifted"

	cases := []struct {
		name  string
		start uint32
		end   uint32
	}{
		{name: "persisted range entirely below the pool", start: 10000, end: 10099},
		{name: "persisted range entirely above the pool", start: 30000, end: 30099},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &resource.AgentRecord{
				AgentID:        agentID,
				Status:         "running",
				CreatedAt:      time.Now().Add(-time.Hour),
				ExpiresAt:      time.Now().Add(time.Hour),
				PortRangeStart: tc.start,
				PortRangeEnd:   tc.end,
			}

			path := filepath.Join(t.TempDir(), "agents.jsonl")
			seed := newRegistryManager(t, path)
			if err := seed.persistSpawn(rec); err != nil {
				t.Fatalf("persistSpawn: %v", err)
			}
			if err := seed.registry.Close(); err != nil {
				t.Fatalf("close seed registry: %v", err)
			}

			var buf bytes.Buffer
			m, destroyRec := poolDriftManager(t, path, &buf)
			defer m.registry.Close()
			if m.registry.LiveCount() != 1 {
				t.Fatalf("replay live count = %d, want 1", m.registry.LiveCount())
			}
			m.listSystemAgents = func() ([]SystemAgent, error) {
				return []SystemAgent{{AgentID: agentID, Username: "bunker-" + agentID, Home: "/home/bunker-" + agentID}}, nil
			}

			rep := m.Reconcile(context.Background())

			if got := destroyRec.calls(); len(got) != 0 {
				t.Errorf("destroy calls = %v, want 0 — a disjoint persisted range cannot collide with this pool, the agent must not be destroyed", got)
			}
			if rep.Destroyed != 0 {
				t.Errorf("Destroyed = %d, want 0 — report %+v", rep.Destroyed, rep)
			}
			if rep.RestoredForeignPool != 1 {
				t.Errorf("RestoredForeignPool = %d, want 1 — report %+v", rep.RestoredForeignPool, rep)
			}
			if rep.Restored != 0 {
				t.Errorf("Restored = %d, want 0 (foreign-pool restorations are counted separately, never double-counted) — report %+v", rep.Restored, rep)
			}
			if rep.Purged != 0 || rep.Adopted != 0 || rep.Foreign != 0 {
				t.Errorf("unexpected reconcile actions: %+v", rep)
			}

			// The tracker record is restored with the agent's exact
			// persisted range...
			tracked := m.tracker.Get(agentID)
			if tracked == nil {
				t.Fatal("drifted agent was not restored into the tracker")
			}
			if tracked.PortRangeStart != tc.start || tracked.PortRangeEnd != tc.end {
				t.Errorf("restored range = %d-%d, want %d-%d",
					tracked.PortRangeStart, tracked.PortRangeEnd, tc.start, tc.end)
			}
			// ...the live registry record survives the restore...
			if m.registry.Get(agentID) == nil {
				t.Error("drifted agent's live registry record must survive the restore")
			}
			// ...and the pool accounting is untouched: the out-of-pool range
			// is NOT tracked in the allocator, which is safe because spawn
			// allocates exclusively through the allocator (in-pool only) and
			// can therefore never hand this range out again.
			if !poolIsEmpty(m.portAlloc) {
				t.Errorf("foreign-pool restore touched pool accounting: Available() = %d, MaxRanges() = %d",
					m.portAlloc.Available(), m.portAlloc.MaxRanges())
			}

			// The warning names the agent, its persisted range, this
			// daemon's pool and the cause.
			log := buf.String()
			for _, want := range []string{
				"foreign-pool port reservation",
				"agent_id=" + agentID,
				fmt.Sprintf("persisted_range=%d-%d", tc.start, tc.end),
				"pool=20000-20999",
				"disjoint",
			} {
				if !strings.Contains(log, want) {
					t.Errorf("foreign-pool warning missing %q — log:\n%s", want, log)
				}
			}
		})
	}
}
