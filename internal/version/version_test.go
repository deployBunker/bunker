package version

import "testing"

// TestDefaults verifies the fallback values used when binaries are built
// without -ldflags injection (e.g. `go build ./cmd/bunker` directly).
func TestDefaults(t *testing.T) {
	if Version != "0.1.0" {
		t.Errorf("Version = %q, want 0.1.0", Version)
	}
	if Commit != "unknown" {
		t.Errorf("Commit = %q, want unknown", Commit)
	}
	if BuildDate != "unknown" {
		t.Errorf("BuildDate = %q, want unknown", BuildDate)
	}
}
