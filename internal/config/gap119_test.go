package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── GAP-119 hardened unit-property flag ───────────────────────────────
//
// The config surface of the guarded/hostile unit-sandboxing opt-in. The
// load-bearing property is the DEFAULT: off. A daemon that upgrades must not
// change a single systemd-run argv because a new key appeared in the struct,
// so the flag is false unless an operator writes it (file or env), and the
// shipped example documents it without arming it.

// TestGAP119_DefaultIsOff pins the off-by-default contract on both entry
// points: the built-in defaults and a config file that does not mention the
// key.
func TestGAP119_DefaultIsOff(t *testing.T) {
	if cfg := DefaultConfig(); cfg.Agent.HardenedUnitProperties {
		t.Error("DefaultConfig().Agent.HardenedUnitProperties = true, want false (off unless an operator arms it)")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("safety:\n  preset: standard\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUNKERD_AGENT_HARDENED_UNIT_PROPERTIES", "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.HardenedUnitProperties {
		t.Error("a config file without the key resolved the flag ON — the default must be off")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestGAP119_FileArmsTheFlag proves the file surface through the real loader.
func TestGAP119_FileArmsTheFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent:\n  hardened_unit_properties: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUNKERD_AGENT_HARDENED_UNIT_PROPERTIES", "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Agent.HardenedUnitProperties {
		t.Error("agent.hardened_unit_properties: true did not arm the flag")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestGAP119_EnvArmsTheFlag pins the documented env override, and that the env
// value is also the way to DISARM a file that armed it (the viper-bound key
// keeps the two surfaces in the same precedence order as every other agent
// knob).
func TestGAP119_EnvArmsTheFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent:\n  hardened_unit_properties: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUNKERD_AGENT_HARDENED_UNIT_PROPERTIES", "true")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Agent.HardenedUnitProperties {
		t.Error("BUNKERD_AGENT_HARDENED_UNIT_PROPERTIES=true did not beat the file's false")
	}
}

// TestGAP119_ExampleDocumentsTheFlagOff keeps the shipped example honest: it
// must teach the flag (key + env name + what the set contains and what it
// deliberately omits) while leaving it DISARMED, because the example is what an
// operator copy-pastes into a production config.
func TestGAP119_ExampleDocumentsTheFlagOff(t *testing.T) {
	path := exampleConfigPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc := string(raw)
	if !strings.Contains(doc, "hardened_unit_properties") {
		t.Errorf("%s must document agent.hardened_unit_properties", path)
	}
	if !strings.Contains(doc, "BUNKERD_AGENT_HARDENED_UNIT_PROPERTIES") {
		t.Errorf("%s must name the env override BUNKERD_AGENT_HARDENED_UNIT_PROPERTIES", path)
	}
	// The set that is armed, named in the doc so an operator can see it without
	// reading Go.
	for _, want := range []string{"RestrictSUIDSGID", "ProtectKernelTunables", "ProtectKernelModules", "ProtectClock", "ProtectHostname"} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s must name the armed property %s", path, want)
		}
	}
	// The measured spawn-breakers must be named as EXCLUDED: the example is
	// where an operator would otherwise "just add ProtectSystem=strict".
	for _, want := range []string{"NoNewPrivileges", "ProtectSystem", "ProtectHome", "RestrictNamespaces", "CapabilityBoundingSet"} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s must name %s as an excluded (start-path-breaking) property", path, want)
		}
	}
	// And it must not ARM the flag: a live (uncommented) key would ship the
	// hardened set to every operator who copies the example.
	for i, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "hardened_unit_properties:") {
			t.Errorf("%s line %d arms the flag in the shipped example (must stay commented/off): %q", path, i+1, trimmed)
		}
	}
	// The example must still parse, with the flag resolved off.
	t.Setenv("BUNKERD_AGENT_HARDENED_UNIT_PROPERTIES", "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	if cfg.Agent.HardenedUnitProperties {
		t.Errorf("%s parses with the flag ON, want OFF", path)
	}
}
