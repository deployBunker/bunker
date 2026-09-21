package config

import (
	"strings"
	"testing"
)

// ── GAP-117 shipped-tier naming tests ─────────────────────────────────
//
// The spec (specs/safety-presets.md §1/§4) designates "standard" as the
// good-experience DEFAULT tier, and §3 pins the standard column to today's
// shipped five-knob defaults. GAP-117 reconciles the GAP-116 plumbing (which
// had named today's baseline "open") in favor of the spec: "standard" is the
// shipped tier and the built-in default; "open" stays a VALID, accepted name
// (no config break for safety.preset: open) until GAP-118/119 differentiate
// the tiers. Unknown names keep failing as hard errors.

// TestGAP117_DefaultTierIsStandard pins the reconciliation: the built-in
// default is "standard" (the spec's default tier), "standard" and "open" are
// both valid vocabulary members, and the resolver answers BOTH names without
// error (a config that already says safety.preset: open keeps working).
func TestGAP117_DefaultPresetIsStandard(t *testing.T) {
	if SafetyPresetDefault != SafetyPresetStandard {
		t.Fatalf("built-in default = %q, want %q (the spec's default tier)", SafetyPresetDefault, SafetyPresetStandard)
	}
	if SafetyPresetStandard != "standard" {
		t.Errorf("shipped-tier name = %q, want %q", SafetyPresetStandard, "standard")
	}
	if !ValidSafetyPreset(SafetyPresetOpen) {
		t.Errorf("safety.preset: open must remain a valid value (no config break)")
	}
	if !ValidSafetyPreset(SafetyPresetStandard) {
		t.Errorf("safety.preset: standard must be valid")
	}

	// Both names resolve without error through the precedence resolver.
	for _, name := range []string{SafetyPresetDefault, SafetyPresetStandard, SafetyPresetOpen} {
		cfg := DefaultConfig()
		cfg.Safety.Preset = name
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(safety.preset: %s) = %v, want OK", name, err)
		}
		t.Setenv(SafetyPresetEnv, "") // no env source
		got, err := cfg.ResolveSafetyPreset("")
		if err != nil {
			t.Errorf("ResolveSafetyPreset(config %q) errored: %v", name, err)
			continue
		}
		if got != name {
			t.Errorf("ResolveSafetyPreset(config %q) = %q", name, got)
		}
	}

	// With every source unset, the resolver answers the shipped tier.
	cfg := DefaultConfig()
	t.Setenv(SafetyPresetEnv, "")
	got, err := cfg.ResolveSafetyPreset("")
	if err != nil {
		t.Fatalf("resolve with everything unset: %v", err)
	}
	if got != SafetyPresetStandard {
		t.Errorf("unset resolution = %q, want shipped tier %q", got, SafetyPresetStandard)
	}
}

// TestGAP117_VocabularyUnchanged keeps the acceptance surface identical to
// GAP-116: exactly three names, displayed standard-first, and everything
// outside the vocabulary still rejected.
func TestGAP117_PresetVocabularyUnchanged(t *testing.T) {
	got := ValidSafetyPresets()
	want := []string{"standard", "open", "hardened"}
	if len(got) != len(want) {
		t.Fatalf("vocabulary = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("vocabulary[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"", "Open", "STANDARD", "guarded", "hostile", "standar"} {
		if ValidSafetyPreset(bad) {
			t.Errorf("non-member %q accepted", bad)
		}
	}
}

// TestGAP117_UnknownPresetStillHardError proves the reconciliation did not
// soften the fail-loud rule: an unknown name is a hard error at Validate
// (config load) AND at ResolveSafetyPreset (spawn-shaped resolution).
func TestGAP117_UnknownPresetStillHardError(t *testing.T) {
	for _, bad := range []string{"ultra", "locked", "guarded", "hostile"} {
		cfg := DefaultConfig()
		cfg.Safety.Preset = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate accepted safety.preset: %s", bad)
		} else if !strings.Contains(err.Error(), "safety.preset") {
			t.Errorf("Validate(%q) error should name the key: %v", bad, err)
		}
		t.Setenv(SafetyPresetEnv, "")
		if _, err := cfg.ResolveSafetyPreset(""); err == nil {
			t.Errorf("ResolveSafetyPreset accepted config %q", bad)
		}
		if _, err := cfg.ResolveSafetyPreset(bad); err == nil {
			t.Errorf("ResolveSafetyPreset accepted flag %q", bad)
		}
	}
}
