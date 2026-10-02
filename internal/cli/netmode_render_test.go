package cli

// NET-BUNKER-010 CLI-surface tests: the --network-mode flag refuses unknown
// names locally (naming the value and the valid set), and the reporting
// renderers keep the §5.2 three-state vocabulary distinct (empty is never
// rendered as safe/shared).

import (
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestSpawnCommand_NetworkModeLocalRefusal pins the fast-fail: an unknown
// --network-mode is refused locally with the offending value and the valid
// set — before any config load, server dial, or "Creating agent..." line.
func TestSpawnCommand_NetworkModeLocalRefusal(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewSpawnCommand()
	cmd.SetArgs([]string{"--network-mode", "pasta", "--server", "nonexistent"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("unknown --network-mode executed without error")
	}
	for _, want := range []string{"pasta", "shared", "systemd"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q missing %q", err, want)
		}
	}
}

// TestFormatNetworkIsolation_ThreeStates pins the renderer's honesty
// contract: explicit mode → mode + boundary; unknown → never claims shared;
// absent (pre-surface record) → says the boundary is NOT verified, never a
// safe-sounding default.
func TestFormatNetworkIsolation_ThreeStates(t *testing.T) {
	tests := []struct {
		name       string
		ni         *v1.NetworkIsolation
		wantSubstr []string
	}{
		{
			name:       "absent is not reported, never safe",
			ni:         nil,
			wantSubstr: []string{"not reported", "NOT verified"},
		},
		{
			name:       "empty mode is not reported, never safe",
			ni:         &v1.NetworkIsolation{},
			wantSubstr: []string{"not reported", "NOT verified"},
		},
		{
			name:       "unknown never claims shared",
			ni:         &v1.NetworkIsolation{Mode: "unknown"},
			wantSubstr: []string{"unknown", "could not verify"},
		},
		{
			name:       "shared states the mode and boundary",
			ni:         &v1.NetworkIsolation{Mode: "shared", Boundary: "host network namespace"},
			wantSubstr: []string{"shared", "host network namespace"},
		},
		{
			name:       "systemd states the mode and boundary",
			ni:         &v1.NetworkIsolation{Mode: "systemd", Boundary: "private network namespace"},
			wantSubstr: []string{"systemd", "private network namespace"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatNetworkIsolation(tt.ni)
			for _, s := range tt.wantSubstr {
				if !strings.Contains(got, s) {
					t.Errorf("formatNetworkIsolation(%v) = %q, missing %q", tt.ni, got, s)
				}
			}
		})
	}
	// The empty/unknown renders must NOT contain an affirmative "shared".
	for _, ni := range []*v1.NetworkIsolation{nil, {}, {Mode: "unknown"}} {
		if got := formatNetworkIsolation(ni); strings.Contains(got, "shared —") || strings.Contains(got, "mode shared") {
			t.Errorf("render %q asserts a shared boundary for a non-explicit state", got)
		}
	}
}

// TestFormatDefaultNetworkMode_ThreeStates pins the `bunker status` daemon
// default line with the same honesty contract.
func TestFormatDefaultNetworkMode_ThreeStates(t *testing.T) {
	if got := formatDefaultNetworkMode("", ""); !strings.Contains(got, "not reported") {
		t.Errorf("empty default rendered %q, want a not-reported line", got)
	}
	if got := formatDefaultNetworkMode("unknown", ""); !strings.Contains(got, "unknown") {
		t.Errorf("unknown default rendered %q, want an unknown line", got)
	}
	if got := formatDefaultNetworkMode("systemd", "private network namespace"); !strings.Contains(got, "systemd") || !strings.Contains(got, "private network namespace") {
		t.Errorf("explicit default rendered %q, want mode + boundary", got)
	}
}
