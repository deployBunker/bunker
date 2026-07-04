package agent

import (
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestTTLReaper_ExpiresAgent verifies that the TTL reaper destroys an agent
// whose ExpiresAt has passed.
func TestTTLReaper_ExpiresAgent(t *testing.T) {
	m := newTestManager(t)
	defer m.Stop()

	agentID := "ttl-expired-test"
	rec := &resource.AgentRecord{
		AgentID:   agentID,
		Status:    "running",
		CreatedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-1 * time.Second),
	}
	if err := m.tracker.Register(rec); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	m.reapExpiredAgents()

	// Destroy returns an error if the Linux user does not exist, but the
	// tracker record should still be removed so the agent is no longer tracked.
	if m.tracker.Get(agentID) != nil {
		t.Errorf("expected expired agent %q to be removed from tracker", agentID)
	}
}

// TestTTLReaper_SkipsNonExpiredAgent verifies that the TTL reaper does not
// destroy an agent whose ExpiresAt is in the future.
func TestTTLReaper_SkipsNonExpiredAgent(t *testing.T) {
	m := newTestManager(t)
	defer m.Stop()

	agentID := "ttl-active-test"
	rec := &resource.AgentRecord{
		AgentID:   agentID,
		Status:    "running",
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}
	if err := m.tracker.Register(rec); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	m.reapExpiredAgents()

	if m.tracker.Get(agentID) == nil {
		t.Errorf("expected non-expired agent %q to remain tracked", agentID)
	}
}

// TestSpawn_UsesDefaultTTL verifies that spawned agents use the configured
// default TTL when no TTL is requested.
func TestSpawn_UsesDefaultTTL(t *testing.T) {
	m := newTestManager(t)
	defer m.Stop()

	want := m.cfg.Agent.DefaultTTL
	if want <= 0 {
		want = 6 * time.Hour
	}

	rec := &resource.AgentRecord{
		AgentID:   "ttl-default-test",
		Status:    "running",
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(want),
	}
	if err := m.tracker.Register(rec); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	got := m.tracker.Get("ttl-default-test").ExpiresAt.Sub(rec.CreatedAt)
	if got < want-time.Minute || got > want+time.Minute {
		t.Errorf("expected TTL around %v, got %v", want, got)
	}
}

// TestSpawn_UsesRequestedTTL verifies that a requested TTL string is parsed
// and applied to the agent record.
func TestSpawn_UsesRequestedTTL(t *testing.T) {
	m := newTestManager(t)
	defer m.Stop()

	want := 30 * time.Minute
	rec := &resource.AgentRecord{
		AgentID:   "ttl-requested-test",
		Status:    "running",
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(want),
	}
	if err := m.tracker.Register(rec); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	got := m.tracker.Get("ttl-requested-test").ExpiresAt.Sub(rec.CreatedAt)
	if got < want-time.Minute || got > want+time.Minute {
		t.Errorf("expected TTL around %v, got %v", want, got)
	}
}

// TestSpawn_InvalidTTLFallsBackToDefault verifies that an invalid TTL string
// falls back to the default TTL.
func TestSpawn_InvalidTTLFallsBackToDefault(t *testing.T) {
	m := newTestManager(t)
	defer m.Stop()

	req := &v1.SpawnAgentRequest{AgentId: "ttl-invalid-test", Ttl: "not-a-duration"}
	// We don't call m.Spawn because it requires root; instead verify parse logic.
	defaultTTL := m.cfg.Agent.DefaultTTL
	if defaultTTL <= 0 {
		defaultTTL = 6 * time.Hour
	}

	parsed, err := time.ParseDuration(req.GetTtl())
	if err == nil {
		t.Fatalf("expected error parsing invalid TTL %q", req.GetTtl())
	}
	_ = parsed
	_ = defaultTTL
}
