package agent

// INT-CI-043 acceptance tests: startup reconciliation must not spend the
// daemon's readiness window on the orphan walk. The CI fingerprint (run
// 36180131056): a daemon starting against a stale registry (live=0,
// known=709) spent the ENTIRE 30s window archiving ONE orphan's home before
// its first listener existed. These tests scale that fingerprint down: the
// destroy seam sleeps slowOrphanDestroy (a stand-in for tar+userdel on a
// multi-GB home) and the readiness assertion requires ReconcileStartup to
// return while that destroy is still in flight. The property scales with the
// constant: if the synchronous phase returns before a 500ms orphan destroy,
// it returns before a 29s one.
//
// Readiness is asserted by SIGNAL (the destroy must not have completed when
// ReconcileStartup returns), never by a fixed wall-clock budget: the previous
// 150ms compare measured the synchronous phase against a constant and failed
// under parallel package load (864ms at HEAD) even though the semantics held.
import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

const (
	slowOrphanDestroy = 500 * time.Millisecond
)

// startupReadinessDeadline bounds how long the DEFERRED orphan cleanup may
// take to deliver the final report after startup returns. It is deliberately
// generous and is NOT a measure of the synchronous phase (readiness is
// asserted by SIGNAL, not wall-clock). Override with
// BUNKERD_TEST_STARTUP_DEADLINE for pathological CI hosts.
func startupReadinessDeadline() time.Duration {
	if v := os.Getenv("BUNKERD_TEST_STARTUP_DEADLINE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Second
}

// waitFinalReport blocks for the final reconcile report, failing the test if
// the deferred orphan cleanup does not complete within the bounded readiness
// deadline (deferred must never become skipped or stalled).
func waitFinalReport(t *testing.T, finalCh <-chan ReconcileReport) ReconcileReport {
	t.Helper()
	timer := time.NewTimer(startupReadinessDeadline())
	defer timer.Stop()
	select {
	case final := <-finalCh:
		return final
	case <-timer.C:
		t.Fatal("final reconcile report not delivered within the bounded deadline: deferred orphan cleanup stalled")
		return ReconcileReport{}
	}
}

// TestReconcileStartup_ReturnsBeforeOrphanWalk proves startup mode hands
// control back while a slow orphan destroy is still in flight, and that the
// TTL-reaper invariant survives: reconcileDone closes only AFTER the async
// walk completes.
func TestReconcileStartup_ReturnsBeforeOrphanWalk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)

	destroyStarted := make(chan struct{}, 1)
	destroyFinished := make(chan string, 1)
	m.destroyAgent = func(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
		destroyStarted <- struct{}{}
		time.Sleep(slowOrphanDestroy)
		destroyFinished <- agentID
		return &v1.DestroyAgentResponse{AgentId: agentID, Status: "destroyed"}, nil
	}
	// REV-BUNKER-002: the orphan must be destroyable (readable in-pool
	// metadata), not UNPROVEN, or the walk skips it and the async-destroy
	// property this test pins never runs.
	home := t.TempDir()
	writePortMetadata(t, home, "12300-12399\n")
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "orphan", Username: "bunker-orphan", Home: home}}, nil
	}

	rep, finalCh := m.ReconcileStartup(context.Background())
	// Deterministic ordering check: the destroy must not have completed when
	// startup returned (if it had, the walk ran synchronously — the exact
	// regression this row forbids).
	select {
	case <-destroyFinished:
		t.Fatal("orphan destroy completed before ReconcileStartup returned — the walk ran on the startup path")
	default:
	}
	if rep.Destroyed != 0 {
		t.Errorf("interim Destroyed = %d, want 0 (walk still pending at return)", rep.Destroyed)
	}

	final := waitFinalReport(t, finalCh)
	if final.Destroyed != 1 {
		t.Errorf("final Destroyed = %d, want 1 (orphan destroyed post-ready)", final.Destroyed)
	}
	select {
	case <-destroyFinished:
	default:
		t.Error("final report delivered but the orphan destroy never completed")
	}
	select {
	case <-m.reconcileDone:
	default:
		t.Error("reconcileDone must be closed once the async orphan walk completes (TTL reaper unblocked)")
	}
}

// TestReconcileStartup_StaleRegistryReadyWithinWindow is the readiness
// criterion from the board row: a daemon starting with a stale registry
// (live=0 after purge, known>0) plus an orphan home present becomes ready
// without waiting on the orphan walk, and the orphan is still destroyed
// post-ready (cleanup is deferred, never skipped).
func TestReconcileStartup_StaleRegistryReadyWithinWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")

	// Seed a stale record: live at write time, its system user gone at replay
	// (the CI runner's shape: hundreds of known records, zero live agents).
	seed := newRegistryManager(t, path)
	if err := seed.persistSpawn(&resource.AgentRecord{
		AgentID:   "stale",
		Status:    "running",
		CreatedAt: time.Now().Add(-time.Hour),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	if err := seed.registry.Close(); err != nil {
		t.Fatalf("registry close: %v", err)
	}

	m := newRegistryManager(t, path)
	destroyFinished := make(chan string, 1)
	m.destroyAgent = func(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
		time.Sleep(slowOrphanDestroy)
		destroyFinished <- agentID
		return &v1.DestroyAgentResponse{AgentId: agentID, Status: "destroyed"}, nil
	}
	// REV-BUNKER-002: the orphan must be destroyable (readable in-pool
	// metadata), not UNPROVEN, or the walk skips it post-ready.
	home := t.TempDir()
	writePortMetadata(t, home, "12300-12399\n")
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "orphan", Username: "bunker-orphan", Home: home}}, nil
	}

	rep, finalCh := m.ReconcileStartup(context.Background())
	// Readiness is a SIGNAL assertion, not a wall-clock budget: the destroy
	// must still be in flight when startup returned, or the stale-registry
	// walk ran synchronously on the startup path.
	select {
	case <-destroyFinished:
		t.Fatal("orphan destroy completed before ReconcileStartup returned — the stale-registry walk ran on the startup path")
	default:
	}
	if rep.Purged != 1 {
		t.Errorf("interim Purged = %d, want 1 (the stale record's purge is a synchronous phase)", rep.Purged)
	}
	if m.registry.LiveCount() != 0 {
		t.Errorf("live count = %d after startup, want 0 (stale-registry shape)", m.registry.LiveCount())
	}

	final := waitFinalReport(t, finalCh)
	if final.Destroyed != 1 {
		t.Errorf("final Destroyed = %d, want 1 (orphan destroyed post-ready)", final.Destroyed)
	}
}

// TestReconcileStartup_EarlyExitUnblocksReaper: a registry-less manager never
// reaches the orphan walk; startup mode must still unblock the TTL reaper and
// deliver the final report exactly once.
func TestReconcileStartup_EarlyExitUnblocksReaper(t *testing.T) {
	logger := testLogger()
	cfg := config.DefaultConfig()
	cfg.Agent.Registry.Enabled = false
	tracker := resource.NewTracker(10, logger)
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	defer m.Stop()
	m.listSystemAgents = func() ([]SystemAgent, error) {
		t.Error("disabled registry must not probe the system")
		return nil, nil
	}

	rep, finalCh := m.ReconcileStartup(context.Background())
	final := <-finalCh
	if rep.Destroyed != 0 || rep.Adopted != 0 || rep.Purged != 0 {
		t.Errorf("interim report = %+v, want no actions with the registry disabled", rep)
	}
	if final.Destroyed != 0 || final.Adopted != 0 || final.Purged != 0 {
		t.Errorf("final report = %+v, want no actions with the registry disabled", final)
	}
	select {
	case <-m.reconcileDone:
	default:
		t.Error("reaper must be unblocked on the early-exit path")
	}
}
