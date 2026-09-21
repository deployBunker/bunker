package cli

import (
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── GAP-116 CLI preset plumbing tests ─────────────────────────────────

// TestSpawnCommand_PresetLocalReject proves the spawn-side fail-fast rule:
// an unknown --preset is rejected LOCALLY before any config load or RPC —
// the error fires even with no server configured at all (same shape as the
// --ttl and --image-spec local rejections).
func TestSpawnCommand_PresetLocalReject(t *testing.T) {
	err := runSpawnWithArgs(t, "--preset", "ultra")
	if err == nil {
		t.Fatal("unknown --preset accepted locally")
	}
	if !strings.Contains(err.Error(), "invalid --preset") {
		t.Errorf("error should name the flag: %v", err)
	}
	if !strings.Contains(err.Error(), "open") || !strings.Contains(err.Error(), "hardened") {
		t.Errorf("error should list the vocabulary: %v", err)
	}
}

// TestSpawnCommand_PresetFlagRegistered pins the flag's existence and
// default: `spawn --preset` is a string flag defaulting to empty (defer to
// env > config global > built-in default).
func TestSpawnCommand_PresetFlagRegistered(t *testing.T) {
	cmd := NewSpawnCommand()
	f := cmd.Flags().Lookup("preset")
	if f == nil {
		t.Fatal("spawn is missing the --preset flag")
	}
	if f.DefValue != "" {
		t.Errorf("--preset default = %q, want empty", f.DefValue)
	}
}

// TestRunArgs_PresetAcceptedBothPositions proves the run grammar accepts
// --preset in BOTH the pre-agent-id and post-agent-id positions (space and
// inline forms), carrying the value into the parsed args.
func TestRunArgs_PresetAcceptedBothPositions(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "pre-agent-id space form", args: []string{"--preset", "hardened", "abc", "--", "echo", "hi"}, want: "hardened"},
		{name: "post-agent-id space form", args: []string{"abc", "--preset", "hardened", "--", "echo", "hi"}, want: "hardened"},
		{name: "post-agent-id inline form", args: []string{"abc", "--preset=standard", "--", "echo", "hi"}, want: "standard"},
		{name: "unset", args: []string{"abc", "--", "echo", "hi"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := parseRunArgs(tt.args)
			if err != nil {
				t.Fatalf("parseRunArgs: %v", err)
			}
			if parsed.preset != tt.want {
				t.Errorf("parsed.preset = %q, want %q", parsed.preset, tt.want)
			}
		})
	}
}

// TestRunArgs_PresetUnknownRejectedLocally proves the run-side fail-fast
// rule and the strict grammar: an unknown preset name in the PRE-agent-id
// position is a local rejection, not a silent command token.
func TestRunArgs_PresetUnknownRejectedLocally(t *testing.T) {
	_, err := parseRunArgs([]string{"--preset", "ultra", "abc", "--", "echo", "hi"})
	if err == nil {
		t.Fatal("unknown --preset accepted")
	}
	if !strings.Contains(err.Error(), "invalid --preset") {
		t.Errorf("error should name the flag: %v", err)
	}
}

// TestRunArgs_PresetPositionLeavesCommandFlagsAlone keeps the lenient
// post-agent-id grammar honest: a Docker-style command keeps its own flags
// even when --preset was peeled from that position.
func TestRunArgs_PresetPositionLeavesCommandFlagsAlone(t *testing.T) {
	parsed, err := parseRunArgs([]string{"abc", "--preset", "open", "--", "docker", "run", "--rm", "img"})
	if err != nil {
		t.Fatalf("parseRunArgs: %v", err)
	}
	if parsed.preset != "open" || parsed.command != "docker" || len(parsed.commandArgs) != 3 || parsed.commandArgs[0] != "run" {
		t.Fatalf("unexpected parse: %+v", parsed)
	}
}

// ── `bunker info` effective-set rendering (AC4) ───────────────────────

// infoGAP116Server wraps the existing infoMockServer harness with a GAP-116
// populated agent summary.
func infoGAP116Summary() *v1.AgentSummary {
	return &v1.AgentSummary{
		AgentId:      "gap116-agent",
		Status:       "running",
		SafetyPreset: "hardened",
		SystemdProperties: []*v1.SystemdProperty{
			{Name: "CPUQuota", Value: "200%"},
			{Name: "MemoryMax", Value: "4294967296"},
			{Name: "LimitFSIZE", Value: "21474836480"},
			{Name: "TasksMax", Value: "4096"},
			{Name: "LimitNOFILE", Value: "65536:65536"},
		},
	}
}

// TestInfoCommand_GAP116EffectiveSet proves `bunker info` renders the
// effective preset name and the five-knob set from the GetAgent response.
func TestInfoCommand_GAP116EffectiveSet(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &infoMockServer{agent: infoGAP116Summary()}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers:      map[string]ServerEntry{"test": {URL: srv.URL}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewInfoCommand()
	cmd.SetArgs([]string{"--server", "test", "gap116-agent"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	checks := []string{
		"Safety Preset:    hardened",
		"Safety Knobs:",
		"CPUQuota:",
		"200%",
		"MemoryMax:",
		"4294967296",
		"LimitFSIZE:",
		"21474836480",
		"TasksMax:",
		"4096",
		"LimitNOFILE:",
		"65536:65536",
	}
	for _, check := range checks {
		if !strings.Contains(output, check) {
			t.Errorf("output missing %q, got:\n%s", check, output)
		}
	}
}
