package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── GAP-116 safety.preset tests ────────────────────────────────────────
// Table-driven coverage for: precedence resolution (flag > env > config >
// default, including conflicts), unknown-preset hard errors (config load AND
// spawn-shaped resolution), env-beats-config, flag-beats-env.

// TestGAP116_DefaultConfig_SafetyPresetUnset pins the hidden-by-default
// contract: the default config's preset is the EMPTY string, which resolves
// to the built-in default without ever naming it.
func TestGAP116_DefaultConfig_SafetyPresetUnset(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Safety.Preset != "" {
		t.Errorf("safety.preset must default to empty (unset), got %q", cfg.Safety.Preset)
	}
	resolved, err := cfg.ResolveSafetyPreset("")
	if err != nil {
		t.Fatalf("resolve with everything unset: %v", err)
	}
	if resolved != SafetyPresetDefault {
		t.Errorf("resolved preset = %q, want built-in default %q", resolved, SafetyPresetDefault)
	}
	if SafetyPresetDefault != SafetyPresetOpen {
		t.Errorf("built-in default must be %q, got %q", SafetyPresetOpen, SafetyPresetDefault)
	}
}

// TestGAP116_ResolvePrecedence is the AC3 precedence table: flag > env >
// config global > built-in default, including the conflict rows.
func TestGAP116_ResolvePrecedence(t *testing.T) {
	tests := []struct {
		name       string
		configVal  string // Safety.Preset on the config struct
		envVal     string // BUNKERD_SAFETY_PRESET
		flagVal    string // per-spawn flag / SpawnAgentRequest.SafetyPreset
		want       string
		wantErr    bool
		errContain string
	}{
		{name: "all unset resolves to built-in default", want: SafetyPresetOpen},
		{name: "config only", configVal: "hardened", want: "hardened"},
		{name: "env only", envVal: "standard", want: "standard"},
		{name: "flag only", flagVal: "hardened", want: "hardened"},
		{
			name:    "flag beats env",
			envVal:  "standard",
			flagVal: "hardened",
			want:    "hardened",
		},
		{
			name:      "env beats config",
			configVal: "hardened",
			envVal:    "standard",
			want:      "standard",
		},
		{
			name:      "flag beats env and config",
			configVal: "standard",
			envVal:    "hardened",
			flagVal:   "open",
			want:      "open",
		},
		{
			name:       "unknown env is hard error even when config is valid",
			configVal:  "open",
			envVal:     " Maximum", // junk with leading space: not in vocabulary
			wantErr:    true,
			errContain: "BUNKERD_SAFETY_PRESET",
		},
		{
			name:       "unknown flag is hard error naming the flag",
			flagVal:    "extreme",
			wantErr:    true,
			errContain: "--preset",
		},
		{
			name:       "unknown flag beats valid env (fail loud wins)",
			envVal:     "open",
			flagVal:    "nope",
			wantErr:    true,
			errContain: "--preset",
		},
		{
			name:       "unknown config is hard error",
			configVal:  "locked",
			wantErr:    true,
			errContain: "safety.preset",
		},
		{
			name:    "flag value is whitespace-trimmed",
			flagVal: "  hardened  ",
			want:    "hardened",
		},
		{
			name:    "empty-string flag defers to env",
			envVal:  "standard",
			flagVal: "",
			want:    "standard",
		},
		{
			name:      "whitespace-only env defers to config",
			configVal: "open",
			envVal:    "   ",
			want:      "open",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Safety.Preset = tt.configVal
			if tt.envVal != "" {
				t.Setenv(SafetyPresetEnv, tt.envVal)
			} else {
				t.Setenv(SafetyPresetEnv, "")
			}
			got, err := cfg.ResolveSafetyPreset(tt.flagVal)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got preset %q", got)
				}
				if !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errContain)
				}
				if !strings.Contains(err.Error(), "unknown safety preset") {
					t.Errorf("error %q should name the unknown preset", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolved preset = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGAP116_ConfigValidateRejectsUnknownPreset is the config-load half of
// the AC3 unknown-preset hard error: Validate refuses any value outside the
// vocabulary (and refuses to start via the daemon's Validate call).
func TestGAP116_ConfigValidateRejectsUnknownPreset(t *testing.T) {
	tests := []struct {
		preset string
		wantOK bool
	}{
		{"", true},
		{SafetyPresetOpen, true},
		{SafetyPresetStandard, true},
		{SafetyPresetHardened, true},
		{" hardened", true},      // surrounding whitespace is trimmed before matching
		{"standard ", true},      // (same normalization every source gets)
		{"OPEN", false},          // case-sensitive vocabulary
		{"ultra", false},         // outside vocabulary
		{"open,hardened", false}, // injection shape
		{"standar", false},       // typo
	}
	for _, tt := range tests {
		cfg := DefaultConfig()
		cfg.Safety.Preset = tt.preset
		err := cfg.Validate()
		if tt.wantOK && err != nil {
			t.Errorf("Validate(%q) = %v, want OK", tt.preset, err)
		}
		if !tt.wantOK {
			if err == nil {
				t.Errorf("Validate(%q) accepted an unknown preset", tt.preset)
				continue
			}
			if !strings.Contains(err.Error(), "safety.preset") {
				t.Errorf("Validate(%q) error should name the key: %v", tt.preset, err)
			}
		}
	}
}

// TestGAP116_Load_EnvBeatsFile proves the GAP-067 rule end to end through
// Load: BUNKERD_SAFETY_PRESET overrides the config-file value (env-beats-
// config at load time, observable via the viper-bound key).
func TestGAP116_Load_EnvBeatsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "safety:\n  preset: hardened\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SafetyPresetEnv, "standard")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Safety.Preset != "standard" {
		t.Errorf("env must beat the config file: Safety.Preset = %q, want standard", cfg.Safety.Preset)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	resolved, err := cfg.ResolveSafetyPreset("")
	if err != nil {
		t.Fatalf("ResolveSafetyPreset: %v", err)
	}
	if resolved != "standard" {
		t.Errorf("resolved = %q, want standard", resolved)
	}
}

// TestGAP116_Load_FilePresetOnly proves a config-file preset loads and
// validates (the env-unset path through the real loader).
func TestGAP116_Load_FilePresetOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("safety:\n  preset: hardened\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SafetyPresetEnv, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Safety.Preset != "hardened" {
		t.Errorf("Safety.Preset = %q, want hardened", cfg.Safety.Preset)
	}
}

// TestGAP116_Load_RejectsUnknownPresetFile proves the config-load hard error:
// an unknown preset in the file fails Validate — the daemon refuses to start.
func TestGAP116_Load_RejectsUnknownPresetFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("safety:\n  preset: ultra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SafetyPresetEnv, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load itself must not fail (Validate owns the rejection): %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted safety.preset: ultra from a config file")
	} else if !strings.Contains(err.Error(), "ultra") {
		t.Errorf("error should name the bad value: %v", err)
	}
}

// TestGAP116_ValidSafetyPreset_Vocabulary pins the vocabulary membership
// function: exact, case-sensitive, whitespace-not-folded, empty-not-valid.
func TestGAP116_ValidSafetyPreset_Vocabulary(t *testing.T) {
	for _, p := range ValidSafetyPresets() {
		if !ValidSafetyPreset(p) {
			t.Errorf("vocabulary member %q not accepted", p)
		}
	}
	for _, bad := range []string{"Open", "HARDENED", "open,hardened", "standar", "0"} {
		if ValidSafetyPreset(bad) {
			t.Errorf("non-member %q accepted", bad)
		}
	}
	if ValidSafetyPreset("") {
		t.Error("empty string is not a preset (absence is the empty string, handled by the resolver)")
	}
}
