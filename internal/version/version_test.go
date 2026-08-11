package version

import "testing"

// TestDefaults verifies the fallback values used when binaries are built
// without -ldflags injection (e.g. `go build ./cmd/bunker` directly).
// Version must track the latest tagged release (v0.1.1, GAP-031).
func TestDefaults(t *testing.T) {
	if Version != "0.1.1" {
		t.Errorf("Version = %q, want 0.1.1", Version)
	}
	if Commit != "unknown" {
		t.Errorf("Commit = %q, want unknown", Commit)
	}
	if BuildDate != "unknown" {
		t.Errorf("BuildDate = %q, want unknown", BuildDate)
	}
}
