package netmode

import (
	"strings"
	"testing"
)

// TestValidModesIsTheThreeModeVocabulary pins the vocabulary: exactly shared,
// systemd and procvis today (spec §1.1/§1.2/§1.8; NET-BUNKER-010/002/011).
// A future mode (NET-BUNKER-003/004/005/012) extends validModes — this test
// then changes WITH that row, never silently before it.
func TestValidModesIsTheThreeModeVocabulary(t *testing.T) {
	got := ValidModes()
	if len(got) != 3 {
		t.Fatalf("ValidModes() = %v, want exactly 3 entries (shared, systemd, procvis)", got)
	}
	if got[0] != ModeShared || got[1] != ModeSystemd || got[2] != ModeProcVis {
		t.Fatalf("ValidModes() = %v, want [%s %s %s]", got, ModeShared, ModeSystemd, ModeProcVis)
	}
}

// TestDefaultModeIsShared pins §5.1: the declared default is shared, unchanged
// until measured (NET-BUNKER-008 owns any change — never a rider on a PR).
func TestDefaultModeIsShared(t *testing.T) {
	if DefaultMode != ModeShared {
		t.Fatalf("DefaultMode = %q, want %q (spec §5.1: shared until measured)", DefaultMode, ModeShared)
	}
}

// TestResolve table-drives the selector: empty → default; both valid names
// pass through; an unknown name (including a future/typo'd one and a
// whitespace-padded one) is a named error carrying the offending value AND
// the valid set — never a silent fallback to shared (spec §5.2).
func TestResolve(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		want      string
		wantErr   bool
	}{
		{name: "empty defers to the default", requested: "", want: ModeShared},
		{name: "shared is valid", requested: ModeShared, want: ModeShared},
		{name: "systemd is valid", requested: ModeSystemd, want: ModeSystemd},
		{name: "unknown rootlesskit is refused (unimplemented)", requested: "rootlesskit", wantErr: true},
		{name: "unknown pasta is refused (unimplemented)", requested: "pasta", wantErr: true},
		{name: "typo systemd2 is refused", requested: "systemd2", wantErr: true},
		{name: "surrounding whitespace is trimmed (GAP-116 convention)", requested: " systemd ", want: ModeSystemd},
		{name: "whitespace-only is refused, not treated as unset", requested: "   ", wantErr: true},
		{name: "case-mismatch is refused", requested: "Systemd", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.requested)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q) = %q, want error", tt.requested, got)
				}
				// The refusal names the offending value...
				if !strings.Contains(err.Error(), tt.requested) {
					t.Errorf("refusal %q does not name the offending value %q", err, tt.requested)
				}
				// ...and the valid set...
				for _, m := range ValidModes() {
					if !strings.Contains(err.Error(), m) {
						t.Errorf("refusal %q does not name valid mode %q", err, m)
					}
				}
				// ...and never silently returns shared.
				if got == ModeShared {
					t.Errorf("Resolve(%q) fell back to shared on error — the §5.2 manufactured bound", tt.requested)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q) unexpected error: %v", tt.requested, err)
			}
			if got != tt.want {
				t.Fatalf("Resolve(%q) = %q, want %q", tt.requested, got, tt.want)
			}
		})
	}
}

// TestPropertiesForSharedIsNil is the zero-delta guard's foundation: shared
// adds NO unit property — the spawn argv must stay byte-identical (§5.1).
func TestPropertiesForSharedIsNil(t *testing.T) {
	props, err := PropertiesFor(ModeShared)
	if err != nil {
		t.Fatalf("PropertiesFor(shared): %v", err)
	}
	if len(props) != 0 {
		t.Fatalf("PropertiesFor(shared) = %v, want none — shared must not change the argv", props)
	}
	props, err = PropertiesFor("")
	if err != nil {
		t.Fatalf("PropertiesFor(empty): %v", err)
	}
	if len(props) != 0 {
		t.Fatalf("PropertiesFor(empty) = %v, want none", props)
	}
}

// TestPropertiesForSystemdAddsExactlyPrivateNetwork pins NET-BUNKER-002:
// systemd adds --property=PrivateNetwork=yes exactly once and NOTHING else.
func TestPropertiesForSystemdAddsExactlyPrivateNetwork(t *testing.T) {
	props, err := PropertiesFor(ModeSystemd)
	if err != nil {
		t.Fatalf("PropertiesFor(systemd): %v", err)
	}
	if len(props) != 1 {
		t.Fatalf("PropertiesFor(systemd) = %v, want exactly [PrivateNetwork=yes]", props)
	}
	if props[0] != "--property="+PropertyPrivateNetworkYes {
		t.Fatalf("property = %q, want %q", props[0], "--property="+PropertyPrivateNetworkYes)
	}
}

// TestPropertiesForUnknownRefuses keeps the builder's guard honest: an
// unknown mode is an error, not nil-and-success.
func TestPropertiesForUnknownRefuses(t *testing.T) {
	if _, err := PropertiesFor("pasta"); err == nil {
		t.Fatal("PropertiesFor(pasta) succeeded, want a named error")
	}
}

// TestBoundaryFor table-drives the §5.2 boundary strings: every mode states
// its honest limits — shared admits it isolates nothing; systemd states the
// private namespace, the outbound loss (and the image-pull cost), and that
// only unit processes are covered; empty/unknown state nothing (an
// unverified boundary is not knowable).
func TestBoundaryFor(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		wantSubstr  []string
		forbidden   []string
		wantNonZero bool
	}{
		{
			name:        "shared admits no network isolation",
			mode:        ModeShared,
			wantSubstr:  []string{"host network namespace", "no network isolation"},
			forbidden:   []string{"private"},
			wantNonZero: true,
		},
		{
			name:        "systemd states the private namespace",
			mode:        ModeSystemd,
			wantSubstr:  []string{"private network namespace", "loopback only"},
			wantNonZero: true,
		},
		{
			name:        "systemd states the outbound loss and image-pull cost plainly",
			mode:        ModeSystemd,
			wantSubstr:  []string{"NO outbound", "image pulls unavailable"},
			wantNonZero: true,
		},
		{
			name:        "systemd scopes the claim to the unit's processes",
			mode:        ModeSystemd,
			wantSubstr:  []string{"only processes"},
			wantNonZero: true,
		},
		{
			name:        "empty reports nothing",
			mode:        "",
			wantNonZero: false,
		},
		{
			name:        "unknown reports nothing",
			mode:        ModeUnknown,
			wantNonZero: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BoundaryFor(tt.mode)
			if tt.wantNonZero && got == "" {
				t.Fatalf("BoundaryFor(%q) is empty, want a boundary string", tt.mode)
			}
			if !tt.wantNonZero && got != "" {
				t.Fatalf("BoundaryFor(%q) = %q, want empty (unverified boundary is not knowable)", tt.mode, got)
			}
			for _, s := range tt.wantSubstr {
				if !strings.Contains(got, s) {
					t.Errorf("BoundaryFor(%q) = %q, missing %q", tt.mode, got, s)
				}
			}
			for _, s := range tt.forbidden {
				if strings.Contains(got, s) {
					t.Errorf("BoundaryFor(%q) = %q must not contain %q (a mode must never widen its claims)", tt.mode, got, s)
				}
			}
		})
	}
}

// TestContainmentMarker pins the GAP-067-idiom marker (§5.2): fixed,
// bracketed, greppable, self-describing. Explicit modes name the mode AND
// the boundary; empty and unknown render the explicit UNKNOWN marker — an
// older daemon's absence must never read as safe, and unknown must never
// claim shared.
func TestContainmentMarker(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		wantSubstr []string
	}{
		{
			name:       "shared marker names the mode and boundary",
			mode:       ModeShared,
			wantSubstr: []string{"[bunker:", "network isolation mode shared", BoundaryFor(ModeShared), "]"},
		},
		{
			name:       "systemd marker names the mode and boundary",
			mode:       ModeSystemd,
			wantSubstr: []string{"[bunker:", "network isolation mode systemd", "NO outbound", "]"},
		},
		{
			name:       "empty renders the UNKNOWN marker, never shared",
			mode:       "",
			wantSubstr: []string{"[bunker:", "unknown", "NOT verified"},
		},
		{
			name:       "unknown renders the UNKNOWN marker",
			mode:       ModeUnknown,
			wantSubstr: []string{"unknown", "NOT verified"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContainmentMarker(tt.mode)
			for _, s := range tt.wantSubstr {
				if !strings.Contains(got, s) {
					t.Errorf("ContainmentMarker(%q) = %q, missing %q", tt.mode, got, s)
				}
			}
		})
	}
	// The marker is single-line and greppable by construction.
	for _, m := range []string{ModeShared, ModeSystemd, "", ModeUnknown} {
		if strings.Contains(ContainmentMarker(m), "\n") {
			t.Errorf("marker for %q contains a newline — it must be one greppable line", m)
		}
	}
	if shared := ContainmentMarker(ModeShared); strings.Contains(shared, "unknown") {
		t.Errorf("shared marker reads as unknown: %q", shared)
	}
}

// TestRefusalErrorNamesValueAndSetAndNoFallback pins the canonical refusal
// text: it names the offending value, the valid set, and the no-silent-
// fallback promise — the §5.2 refuse-loudly law in one line.
func TestRefusalErrorNamesValueAndSetAndNoFallback(t *testing.T) {
	err := RefusalError("hidepid")
	if err == nil {
		t.Fatal("RefusalError(hidepid) = nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "hidepid") {
		t.Errorf("refusal %q does not name the offending value", msg)
	}
	for _, m := range SortedValidModes() {
		if !strings.Contains(msg, m) {
			t.Errorf("refusal %q does not name valid mode %q", msg, m)
		}
	}
	if !strings.Contains(msg, ModeShared) || !strings.Contains(msg, "refusing") {
		t.Errorf("refusal %q must state it refuses rather than falls back to shared", msg)
	}
}

// TestSortedValidModesIsSorted keeps refusal text stable across runs.
func TestSortedValidModesIsSorted(t *testing.T) {
	got := SortedValidModes()
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("SortedValidModes() = %v is not sorted", got)
		}
	}
}
