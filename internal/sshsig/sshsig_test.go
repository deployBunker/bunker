package sshsig

import (
	"strings"
	"testing"
)

// TestClassifySessionDenial pins the recognition table: the diagnostic
// fires for the denial signature and for nothing else. This is the same
// table internal/server pins for its delegating wrapper — kept in sync so a
// behaviour change in the leaf package cannot slip past either surface.
func TestClassifySessionDenial(t *testing.T) {
	cases := []struct {
		name        string
		exitCode    int
		stderrBytes int
		stdoutBytes int
		wantFire    bool
	}{
		// The observed signature: ssh exit 254, banner-only stdout, empty stderr.
		{"254 banner only no stderr", 254, 0, 42, true},
		{"254 banner only single byte", 254, 0, 1, true},
		// ssh reported a real error on stderr — surface that, don't guess.
		{"254 with stderr and stdout", 254, 18, 42, false},
		{"254 with stderr only", 254, 18, 0, false},
		// ssh never reached a session at all: reachability, not denial.
		{"254 with no output", 254, 0, 0, false},
		// Ordinary command failures keep their own output as the cause.
		{"exit 1 with stdout", 1, 0, 12, false},
		{"exit 127 with stdout", 127, 0, 30, false},
		{"exit 0 with stdout", 0, 0, 5, false},
		{"exit 0 with no output", 0, 0, 0, false},
		{"exit 255 with stdout", 255, 0, 7, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, fired := ClassifySessionDenial(tc.exitCode, tc.stderrBytes, tc.stdoutBytes)
			if fired != tc.wantFire {
				t.Fatalf("ClassifySessionDenial(%d, %d, %d) fired = %v, want %v (msg %q)",
					tc.exitCode, tc.stderrBytes, tc.stdoutBytes, fired, tc.wantFire, msg)
			}
			if !tc.wantFire {
				if msg != "" {
					t.Fatalf("non-firing case returned non-empty diagnostic %q", msg)
				}
				return
			}
			// The firing message must be actionable: name the group check
			// and the host-provision status command.
			for _, want := range []string{
				"bunker: ",
				"getent group bunker-agents",
				"host-provision --status",
				"254",
				"PAM",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("diagnostic %q does not mention %q", msg, want)
				}
			}
			if strings.Contains(msg, "\n") {
				t.Errorf("diagnostic must be a single line, got %q", msg)
			}
		})
	}
}

// TestSessionDenialDiagnosticShape pins the parts of the wording that are
// acceptance text: the one-line shape, a single substantiated cause, and no
// inventing of a second one.
func TestSessionDenialDiagnosticShape(t *testing.T) {
	msg, fired := ClassifySessionDenial(254, 0, 10)
	if !fired {
		t.Fatal("expected the denial signature to fire")
	}
	if n := strings.Count(msg, "\n"); n != 0 {
		t.Errorf("diagnostic has %d newlines, want 0", n)
	}
	if !strings.HasPrefix(msg, "bunker: ") {
		t.Errorf("diagnostic must start with %q, got %q", "bunker: ", msg)
	}
	if got := strings.Count(msg, "Most likely cause:"); got != 1 {
		t.Errorf("diagnostic must state exactly one most-likely cause, found %d", got)
	}
	if !strings.Contains(msg, "isolation group") {
		t.Errorf("diagnostic must name the isolation group as the likely cause: %q", msg)
	}
}
