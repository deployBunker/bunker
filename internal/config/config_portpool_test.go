package config

import (
	"strings"
	"testing"
)

// ── FLEET-INFRA-1: max_agents must fit the port-pool capacity ───────────

// TestValidate_MaxAgentsFitsPortPool proves the defaults keep validating and
// that an explicit-but-equal override stays legal.
func TestValidate_MaxAgentsFitsPortPool(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	cfg = DefaultConfig()
	cfg.Agent.MaxAgents = 100 // exactly the pool capacity
	if err := cfg.Validate(); err != nil {
		t.Fatalf("max_agents == capacity must validate: %v", err)
	}
}

// TestValidate_MaxAgentsExceedsPortPool is the regression core: ANY override
// that pushes max_agents past the pool capacity must refuse to boot with a
// named remediation — never silently re-create the "port range pool
// exhausted with 2/8 agents live" shape.
func TestValidate_MaxAgentsExceedsPortPool(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agent.MaxAgents = 101 // pool: 10000-19999 @ 100 = 100 ranges
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error, got nil — the relation is not enforced")
	}
	for _, want := range []string{"max_agents", "pool capacity"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestValidate_PortPoolOverrideRecomputesCapacity proves the relation reads
// the OPERATOR's geometry, not hardcoded defaults: widening the range or
// shrinking the per-agent size raises the capacity that is compared against.
func TestValidate_PortPoolOverrideRecomputesCapacity(t *testing.T) {
	// 150 agents fit when the pool is widened to 150 ranges.
	cfg := DefaultConfig()
	cfg.Agent.MaxAgents = 150
	cfg.Agent.PortRangeEnd = 24999 // 15000 ports / 100 = 150 ranges
	if err := cfg.Validate(); err != nil {
		t.Fatalf("widened pool must fit 150 agents: %v", err)
	}
	// 150 agents do NOT fit when per-agent size doubles the range footprint.
	cfg = DefaultConfig()
	cfg.Agent.MaxAgents = 150
	cfg.Agent.PortRangePerAgent = 200 // 10000 ports / 200 = 50 ranges
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for per-agent override, got nil")
	}
	if !strings.Contains(err.Error(), "50 ranges") {
		t.Errorf("error %q should name the recomputed capacity (50 ranges)", err.Error())
	}
}

// TestValidate_DegenerateGeometryRefusesBoot: a geometry too small for even
// ONE complete sub-range means every agent would share the full configured
// range — the allocator disables itself, spawn's isolation metadata is bogus
// for every agent, and port collisions are guaranteed. The historic posture
// allowed this with a boot warning (nil allocator + full-range fallback);
// FLEET-INFRA-1 fails it closed per the repo's own doctrine (BFS-043: a knob
// the daemon cannot obey stops the boot with its name and value — never
// silently replaced). Hand-built managers without validation keep reaching
// the nil-allocator path (tests), but a config Load can no longer produce it.
func TestValidate_DegenerateGeometryRefusesBoot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agent.PortRangeStart = 10000
	cfg.Agent.PortRangeEnd = 10010
	cfg.Agent.PortRangePerAgent = 100 // only 10 ports → 0 complete ranges
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected boot refusal for degenerate pool geometry, got nil")
	}
	if !strings.Contains(err.Error(), "0 complete sub-ranges") {
		t.Errorf("error %q should name the degenerate geometry", err.Error())
	}
}
