package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/registry"
	"github.com/deployBunker/bunker/internal/resource"
)

// isolateRegistry points a test config's durable registry at t.TempDir() so
// no test ever reads or writes the host's real /var/lib/bunkerd/agents.jsonl
// (and no test ever reconciles against the host's real bunker-* users).
func isolateRegistry(t *testing.T, cfg *config.Config) *config.Config {
	t.Helper()
	cfg.Agent.Registry.Enabled = true
	cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "agents.jsonl")
	cfg.Agent.Registry.MaxBytes = 1 << 20
	cfg.Agent.Registry.MaxBackups = 3
	cfg.Agent.Registry.KnownIDCap = 100
	return cfg
}

// newRegistryManager returns a manager wired to a temp registry, with the
// port pool and tracker ready for reconciliation tests.
//
// SAFETY: the system probe is stubbed to "no agents on this host" by default.
// The production probe reads /etc/passwd, and a reconcile in the default
// destroy mode would then run userdel against real bunker-* leftovers on the
// test host (observed: 9 stale test users on the dev box). Tests that need
// system agents MUST override m.listSystemAgents themselves.
func newRegistryManager(t *testing.T, path string) *AgentManager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	cfg.Agent.Registry.Enabled = true
	cfg.Agent.Registry.Path = path
	cfg.Agent.Registry.MaxBytes = 1 << 20
	if cfg.Agent.PortRangePerAgent == 0 {
		cfg.Agent.PortRangePerAgent = 100
	}
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	m.listSystemAgents = func() ([]SystemAgent, error) { return nil, nil }
	m.destroyAgent = func(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
		t.Errorf("test manager destroyed agent %q — tests must never touch host users", agentID)
		return nil, fmt.Errorf("destroy disabled in tests")
	}
	t.Cleanup(func() { m.Stop() })
	return m
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ── Replay + reconcile ─────────────────────────────────────────

// TestReconcile_RestoresReplayedAgent proves a relaunch re-registers a live
// agent from the durable log and reclaims its EXACT port reservation.
func TestReconcile_RestoresReplayedAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")

	first := newRegistryManager(t, path)
	rec := &resource.AgentRecord{
		AgentID:           "restore-me",
		Status:            "running",
		CreatedAt:         time.Now().Add(-time.Hour),
		ExpiresAt:         time.Now().Add(5 * time.Hour),
		PortRangeStart:    10000,
		PortRangeEnd:      10099,
		SshPrivateKeyPath: "/etc/bunkerd/ssh/restore-me",
	}
	if err := first.persistSpawn(rec); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	if err := first.registry.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Relaunch: a brand-new manager replays the same file.
	second := newRegistryManager(t, path)
	defer second.registry.Close()
	if second.registry.LiveCount() != 1 {
		t.Fatalf("replay live count = %d, want 1", second.registry.LiveCount())
	}
	second.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "restore-me", Username: "bunker-restore-me", Home: "/home/bunker-restore-me"}}, nil
	}

	rep := second.Reconcile(context.Background())
	if rep.Restored != 1 || rep.Purged != 0 {
		t.Fatalf("reconcile report = %+v, want 1 restored and 0 purged", rep)
	}
	tracked := second.tracker.Get("restore-me")
	if tracked == nil {
		t.Fatal("replayed agent was not restored into the tracker")
	}
	if tracked.PortRangeStart != 10000 || tracked.PortRangeEnd != 10099 {
		t.Errorf("restored range = %d-%d, want 10000-10099", tracked.PortRangeStart, tracked.PortRangeEnd)
	}
	if start, end, ok := second.portAlloc.AllocatedRange("restore-me"); !ok || start != 10000 || end != 10099 {
		t.Errorf("port reservation = %d-%d (ok=%v), want exact 10000-10099", start, end, ok)
	}
	// The restored range must not be handed out a second time.
	s2, _, err := second.portAlloc.Allocate("some-other-agent")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if s2 == 10000 {
		t.Error("restored range 10000 was double-allocated after replay")
	}
}

// TestReconcile_PurgesStaleRecord proves a registry record with no system
// user is dropped (and remembered as known).
func TestReconcile_PurgesStaleRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()

	if err := m.persistSpawn(&resource.AgentRecord{
		AgentID:   "stale-one",
		Status:    "running",
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	m.listSystemAgents = func() ([]SystemAgent, error) { return nil, nil }

	rep := m.Reconcile(context.Background())
	if rep.Purged != 1 {
		t.Fatalf("Purged = %d, want 1 (report %+v)", rep.Purged, rep)
	}
	if m.registry.LiveCount() != 0 {
		t.Error("stale record is still live after reconciliation")
	}
	if !m.registry.Known("stale-one") {
		t.Error("purged record must stay known so destroy remains idempotent")
	}
	if m.tracker.Get("stale-one") != nil {
		t.Error("stale record must not be restored into the tracker")
	}
}

// TestReconcile_DestroysOrphanByDefault proves reconciliation.mode=destroy
// removes system users the registry does not know.
func TestReconcile_DestroysOrphanByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()

	destroyed := make([]string, 0, 1)
	m.destroyAgent = func(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
		if !force {
			t.Errorf("reconciliation must destroy orphans with force=true")
		}
		destroyed = append(destroyed, agentID)
		return &v1.DestroyAgentResponse{AgentId: agentID, Status: "destroyed"}, nil
	}
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "orphan", Username: "bunker-orphan", Home: "/home/bunker-orphan"}}, nil
	}

	rep := m.Reconcile(context.Background())
	if rep.Mode != config.ReconcileModeDestroy {
		t.Errorf("mode = %q, want %q (default)", rep.Mode, config.ReconcileModeDestroy)
	}
	if rep.Destroyed != 1 || len(destroyed) != 1 || destroyed[0] != "orphan" {
		t.Fatalf("Destroyed = %d, destroy calls = %v; want one destroy of orphan", rep.Destroyed, destroyed)
	}
}

// TestReconcile_AdoptRestoresTrackerAndExactPorts is the adopt-mode contract:
// tracker record + EXACT port reservation from persisted metadata, so no
// post-relaunch double allocation is possible.
func TestReconcile_AdoptRestoresTrackerAndExactPorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()
	m.cfg.Agent.Reconciliation.Mode = config.ReconcileModeAdopt
	m.cfg.Agent.PortRangeStart = 10000
	m.cfg.Agent.PortRangeEnd = 19999
	m.cfg.Agent.PortRangePerAgent = 100
	pa, err := resource.NewPortAllocator(10000, 19999, 100)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}
	m.portAlloc = pa

	home := t.TempDir()
	metaDir := filepath.Join(home, ".bunker")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, "ports"), []byte("12300-12399\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.destroyAgent = func(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
		t.Errorf("adopt mode must not destroy orphan %q", agentID)
		return nil, nil
	}
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "adopt-me", Username: "bunker-adopt-me", Home: home}}, nil
	}

	rep := m.Reconcile(context.Background())
	if rep.Adopted != 1 {
		t.Fatalf("Adopted = %d, want 1 (report %+v)", rep.Adopted, rep)
	}
	tracked := m.tracker.Get("adopt-me")
	if tracked == nil {
		t.Fatal("adopted agent missing from the tracker")
	}
	if tracked.PortRangeStart != 12300 || tracked.PortRangeEnd != 12399 {
		t.Errorf("adopted range = %d-%d, want 12300-12399 from persisted metadata",
			tracked.PortRangeStart, tracked.PortRangeEnd)
	}
	if start, end, ok := m.portAlloc.AllocatedRange("adopt-me"); !ok || start != 12300 || end != 12399 {
		t.Fatalf("port reservation = %d-%d (ok=%v), want exact 12300-12399", start, end, ok)
	}
	// The exact range is reserved: a new agent must not get it.
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("fresh-%d", i)
		s, _, err := m.portAlloc.Allocate(id)
		if err != nil {
			t.Fatalf("Allocate(%s): %v", id, err)
		}
		if s == 12300 {
			t.Fatal("post-relaunch double allocation: adopted range 12300 was handed out again")
		}
	}
	// Adopted agents carry no durable TTL, so the reaper leaves them alone.
	if tracked.ExpiresAt.IsZero() == false {
		t.Errorf("adopted agent must have zero expiry (never reaped), got %v", tracked.ExpiresAt)
	}
	// And the adoption is durable: a replay knows the agent.
	if m.registry.Get("adopt-me") == nil {
		t.Error("adopted agent was not persisted to the registry")
	}
}

// TestReconcile_ProbeFailureIsNotDestructive: if the system probe fails we
// must do NOTHING — treating "no users" as truth would destroy every agent.
func TestReconcile_ProbeFailureIsNotDestructive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()

	if err := m.persistSpawn(&resource.AgentRecord{
		AgentID: "live-one", Status: "running", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	m.destroyAgent = func(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
		called = true
		return nil, nil
	}
	m.listSystemAgents = func() ([]SystemAgent, error) { return nil, fmt.Errorf("passwd unreadable") }

	rep := m.Reconcile(context.Background())
	if called {
		t.Error("reconciliation destroyed agents despite a failed system probe")
	}
	if rep.Purged != 0 || rep.Destroyed != 0 || rep.Adopted != 0 {
		t.Errorf("report = %+v, want no actions on probe failure", rep)
	}
	if m.registry.LiveCount() != 1 {
		t.Error("a failed probe must not purge live records")
	}
}

// TestReconcile_UnblocksReaperExactlyOnce pins the ordering contract.
func TestReconcile_UnblocksReaperExactlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()

	select {
	case <-m.reconcileDone:
		t.Fatal("reconcileDone was closed before Reconcile ran")
	default:
	}
	m.Reconcile(context.Background())
	select {
	case <-m.reconcileDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Reconcile did not unblock the TTL reaper")
	}
	// Idempotent: a second call must not panic on a double close.
	m.Reconcile(context.Background())
}

// TestReconcile_WithoutRegistryIsANoop keeps the disabled-feature contract.
func TestReconcile_WithoutRegistryIsANoop(t *testing.T) {
	logger := testLogger()
	cfg := config.DefaultConfig()
	cfg.Agent.Registry.Enabled = false
	tracker := resource.NewTracker(10, logger)
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	defer m.Stop()
	if m.registry != nil {
		t.Fatal("registry must be nil when disabled")
	}
	if err := m.RegistryError(); err != nil {
		t.Fatalf("disabled registry must not be an error: %v", err)
	}
	m.listSystemAgents = func() ([]SystemAgent, error) {
		t.Error("disabled registry must not probe the system")
		return nil, nil
	}
	rep := m.Reconcile(context.Background())
	if rep.Restored != 0 || rep.Destroyed != 0 || rep.Adopted != 0 || rep.Purged != 0 {
		t.Errorf("report = %+v, want no actions with the registry disabled", rep)
	}
}

// ── Idempotent destroy ─────────────────────────────────────────

// TestDestroy_TwiceSucceedsForKnownAgent is the core GAP-070 destroy
// contract: a repeat destroy of an agent the durable store knew (and whose
// system user is gone) succeeds, including after a compaction.
func TestDestroy_TwiceSucceedsForKnownAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()

	const id = "idem-destroy"
	if err := m.persistSpawn(&resource.AgentRecord{
		AgentID: id, Status: "running", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		PortRangeStart: 10000, PortRangeEnd: 10099,
	}); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	if _, _, err := m.portAlloc.Allocate(id); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := m.tracker.Register(&resource.AgentRecord{AgentID: id, Status: "running", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := m.Destroy(context.Background(), id, false)
		if err != nil {
			t.Fatalf("attempt %d: destroy of a known absent agent must succeed: %v", attempt, err)
		}
		if resp.Status != "destroyed" {
			t.Fatalf("attempt %d: status = %q, want destroyed", attempt, resp.Status)
		}
		if m.tracker.Get(id) != nil {
			t.Errorf("attempt %d: tracker slot not released", attempt)
		}
		if m.portAlloc.Has(id) {
			t.Errorf("attempt %d: port range not released", attempt)
		}
	}

	// The known-ID knowledge must survive a compaction (that is why
	// compaction persists a bounded index rather than nothing).
	if _, err := m.registry.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	replayed, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer replayed.Close()
	if !replayed.Known(id) {
		t.Fatal("known-ID record lost across compaction — a third destroy would report not_found")
	}
	m.registry = replayed
	if resp, err := m.Destroy(context.Background(), id, false); err != nil || resp.Status != "destroyed" {
		t.Fatalf("destroy after compaction = (%v, %v), want (destroyed, nil)", resp, err)
	}
}

// TestDestroy_NeverSeenStillNotFound keeps the other half of the contract.
func TestDestroy_NeverSeenStillNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()
	// The ID has no system user, so Destroy's host commands (systemctl,
	// pgrep, userdel) all fail fast — nothing on the host is touched.

	resp, err := m.Destroy(context.Background(), "never-seen-agent", false)
	if err == nil {
		t.Fatal("destroy of a never-seen ID must fail")
	}
	if resp == nil || resp.Status != "not_found" {
		t.Fatalf("status = %v, want not_found", resp)
	}
	if m.registry.Known("never-seen-agent") {
		t.Error("a never-seen ID must NOT be recorded as known (that would make it a false idempotent success)")
	}
}

// TestDestroy_NonForceFailureAlwaysReleasesResources covers both branches of
// the non-force failure path with a durable registry present: the tracker
// slot and port range must be released whether the agent was known or not.
func TestDestroy_NonForceFailureAlwaysReleasesResources(t *testing.T) {
	cases := []struct {
		name      string
		known     bool
		wantStat  string
		wantNoErr bool
	}{
		{name: "never seen", known: false, wantStat: "not_found", wantNoErr: false},
		{name: "previously known", known: true, wantStat: "destroyed", wantNoErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			m := newRegistryManager(t, path)
			defer m.registry.Close()

			const id = "release-check"
			if tc.known {
				if err := m.persistSpawn(&resource.AgentRecord{
					AgentID: id, Status: "running", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := m.portAlloc.Allocate(id); err != nil {
				t.Fatalf("Allocate: %v", err)
			}
			if err := m.tracker.Register(&resource.AgentRecord{AgentID: id, Status: "running", CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}

			resp, err := m.Destroy(context.Background(), id, false)
			if tc.wantNoErr && err != nil {
				t.Fatalf("want success, got error: %v", err)
			}
			if !tc.wantNoErr && err == nil {
				t.Fatal("want an error for a never-seen ID")
			}
			if resp == nil || resp.Status != tc.wantStat {
				t.Fatalf("status = %v, want %q", resp, tc.wantStat)
			}
			if m.tracker.Get(id) != nil {
				t.Error("tracker slot leaked on a non-force userdel failure")
			}
			if m.portAlloc.Has(id) {
				t.Error("port range leaked on a non-force userdel failure")
			}
			if got, want := m.portAlloc.Available(), m.portAlloc.MaxRanges(); got != want {
				t.Errorf("Available() = %d, want %d (full pool after release)", got, want)
			}
		})
	}
}

// ── Durable spawn + heartbeat ──────────────────────────────────

// TestHeartbeatPersistsExtension proves heartbeats reach the durable log.
func TestHeartbeatPersistsExtension(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()

	const id = "beat-me"
	expires := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	if err := m.persistSpawn(&resource.AgentRecord{
		AgentID: id, Status: "running", CreatedAt: time.Now(), ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.tracker.Register(&resource.AgentRecord{
		AgentID: id, Status: "running", CreatedAt: time.Now(), ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}

	rec, err := m.Heartbeat(id, 6*time.Hour)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !rec.ExpiresAt.After(expires) {
		t.Fatalf("heartbeat did not extend expiry: %v", rec.ExpiresAt)
	}
	if _, err := m.Heartbeat("no-such-agent", time.Hour); err == nil {
		t.Error("heartbeat for an unknown agent must error")
	}

	replayed, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer replayed.Close()
	stored := replayed.Get(id)
	if stored == nil {
		t.Fatal("replayed registry lost the agent")
	}
	if !stored.ExpiresAt.After(expires.Add(time.Minute)) {
		t.Errorf("durable expiry = %v, want the heartbeat extension persisted (%v -> %v)",
			stored.ExpiresAt, expires, stored.ExpiresAt)
	}
}

// TestSpawnRefusesWhenPersistenceFails proves the spawn durability gate: a
// store write failure must roll the spawn back rather than report success.
// It needs root because the gate runs after user creation.
func TestSpawnRefusesWhenPersistenceFails(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("spawn creates a real user; run as root")
	}
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()

	// Break the store: replacing the file with a directory makes every
	// append fail (EISDIR) while the store object stays "open".
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove registry file: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir over registry path: %v", err)
	}

	agentID := uniqueAgentID("gap070-persist")
	resp, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: agentID})
	if err == nil {
		t.Fatalf("spawn must fail when the registry cannot be written (resp=%v)", resp)
	}
	if m.tracker.Get(agentID) != nil {
		t.Error("failed spawn left a tracker record behind")
	}
	if got, want := m.portAlloc.Available(), m.portAlloc.MaxRanges(); got != want {
		t.Errorf("failed spawn leaked a port range: Available() = %d, want %d", got, want)
	}
	cleanupAgent(t, m, agentID)
}

// TestPersistSpawnFailureIsSurfaced covers the write-error plumbing without
// needing root.
func TestPersistSpawnFailureIsSurfaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	err := m.persistSpawn(&resource.AgentRecord{AgentID: "will-fail", Status: "running"})
	if err == nil {
		t.Fatal("persistSpawn must surface a write failure")
	}
	if m.registry.Get("will-fail") != nil {
		t.Error("a failed append must not be reflected in memory")
	}
}
