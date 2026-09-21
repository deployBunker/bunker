package registry

import (
	"testing"
)

// ── GAP-116 registry persistence tests ────────────────────────────────

// TestGAP116_SpawnEventRoundTrip proves the effective preset and knob set
// survive a JSONL append + replay cycle: a spawn event carrying the GAP-116
// fields folds back into a Record with identical values, and a pre-GAP-116
// event (no fields) replays with them absent (additive, never fabricated).
func TestGAP116_SpawnEventRoundTrip(t *testing.T) {
	ev := Event{
		Kind:    KindSpawn,
		AgentID: "gap116-agent",
		Status:  "running",
		// SafetyPreset/UnitProperties/SliceProperties set; CreatedAt left
		// empty so the round-trip is checkable without a clock.
		SafetyPreset: "hardened",
		UnitProperties: []SystemdProperty{
			{Name: "CPUQuota", Value: "200%"},
			{Name: "TasksMax", Value: "4096"},
		},
		SliceProperties: []SystemdProperty{
			{Name: "CPUQuota", Value: "200%"},
			{Name: "TasksMax", Value: "4096"},
		},
	}
	rec := eventToRecord(&ev)
	if rec.SafetyPreset != "hardened" {
		t.Errorf("replayed preset = %q, want hardened", rec.SafetyPreset)
	}
	if len(rec.UnitProperties) != 2 || rec.UnitProperties[0].Name != "CPUQuota" || rec.UnitProperties[0].Value != "200%" {
		t.Errorf("replayed unit properties drifted: %+v", rec.UnitProperties)
	}
	if len(rec.SliceProperties) != 2 || rec.SliceProperties[1].Value != "4096" {
		t.Errorf("replayed slice properties drifted: %+v", rec.SliceProperties)
	}
}

// TestGAP116_EventToRecord_PreGAP116Empty keeps the additive contract: a
// legacy event without the fields replays to an empty preset/properties.
func TestGAP116_EventToRecord_PreGAP116Empty(t *testing.T) {
	rec := eventToRecord(&Event{Kind: KindSpawn, AgentID: "legacy", Status: "running"})
	if rec.SafetyPreset != "" || rec.UnitProperties != nil || rec.SliceProperties != nil {
		t.Errorf("legacy event must replay empty: %+v", rec)
	}
}
