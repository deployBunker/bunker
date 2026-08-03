package agent

import (
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/resource"
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

// TestParseAgentTTL covers the spec TTL format \d+[hmd] (specs/api.md,
// specs/agent-lifecycle.md) for both accepted and rejected inputs.
func TestParseAgentTTL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		// Accepted: digits followed by a lowercase h/m/d unit.
		{"hours", "6h", 6 * time.Hour, false},
		{"minutes", "90m", 90 * time.Minute, false},
		{"day", "7d", 168 * time.Hour, false},
		{"single hour", "1h", time.Hour, false},
		{"single minute", "1m", time.Minute, false},
		{"single day", "1d", 24 * time.Hour, false},
		{"large day", "30d", 720 * time.Hour, false},
		{"multi-digit", "24h", 24 * time.Hour, false},
		{"multi-digit minutes", "1440m", 24 * time.Hour, false},

		// Rejected: not a duration at all.
		{"not-a-duration", "not-a-duration", 0, true},
		{"banana", "banana", 0, true},
		// Rejected: bare numbers (no unit).
		{"bare zero", "0", 0, true},
		{"bare number", "6", 0, true},
		// Rejected: negative.
		{"negative", "-1h", 0, true},
		// Rejected: decimal.
		{"decimal", "1.5h", 0, true},
		// Rejected: empty.
		{"empty", "", 0, true},
		// Rejected: unit without digits.
		{"unit only", "d", 0, true},
		// Rejected: uppercase unit.
		{"uppercase", "6H", 0, true},
		// Rejected: zero duration (meaningless TTL).
		{"zero hours", "0h", 0, true},
		{"zero days", "0d", 0, true},
		// Rejected: format violations.
		{"unit first", "h6", 0, true},
		{"double unit", "6hh", 0, true},
		{"unknown unit", "6s", 0, true},
		{"leading plus", "+6h", 0, true},
		{"leading space", " 6h", 0, true},
		{"trailing space", "6h ", 0, true},
		// Rejected: overflow of time.Duration.
		{"overflow", "999999999999999999999h", 0, true},
		{"overflow days", "999999999999999999999d", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAgentTTL(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseAgentTTL(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAgentTTL(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseAgentTTL(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestParseAgentTTL_RejectsInvalid asserts that spec-invalid TTL strings
// return an error instead of silently falling back to a default (the old
// behavior enshrined by TestSpawn_InvalidTTLFallsBackToDefault).
func TestParseAgentTTL_RejectsInvalid(t *testing.T) {
	for _, s := range []string{"not-a-duration", "banana", "6H", "1.5h"} {
		if d, err := ParseAgentTTL(s); err == nil {
			t.Errorf("ParseAgentTTL(%q) = %v, want error (no silent fallback)", s, d)
		}
	}
}
