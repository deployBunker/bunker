package version

import (
	"runtime/debug"
	"testing"
)

// TestDefaults verifies the fallback values used when binaries are built
// without -ldflags injection (e.g. `go build ./cmd/bunker` directly).
// Version must track the latest tagged release (v0.1.2, GAP-031). Commit and
// BuildDate must be non-empty: builds from a VCS checkout or a tagged module
// install derive them from the embedded build info (GAP-042).
func TestDefaults(t *testing.T) {
	if Version != "0.1.2" {
		t.Errorf("Version = %q, want 0.1.2", Version)
	}
	if Commit == "" {
		t.Errorf("Commit = %q, want non-empty", Commit)
	}
	if BuildDate == "" {
		t.Errorf("BuildDate = %q, want non-empty", BuildDate)
	}
}

// TestBuildInfoFallback verifies the VCS-metadata fallback: vcs.revision
// (shortened to 7 chars) and vcs.time are used when ldflags are absent.
func TestBuildInfoFallback(t *testing.T) {
	settings := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
		{Key: "vcs.time", Value: "2026-08-14T05:46:44Z"},
	}
	commit, buildDate := deriveFromBuildInfo(settings, "(devel)")
	if commit != "0123456" {
		t.Errorf("commit = %q, want 0123456 (7-char short rev)", commit)
	}
	if buildDate != "2026-08-14T05:46:44Z" {
		t.Errorf("buildDate = %q, want 2026-08-14T05:46:44Z", buildDate)
	}
}

// TestModuleVersionFallback verifies the module-proxy install path: no VCS
// settings, so the module version (v0.1.2) fills the commit field.
func TestModuleVersionFallback(t *testing.T) {
	commit, buildDate := deriveFromBuildInfo(nil, "v0.1.2")
	if commit != "v0.1.2" {
		t.Errorf("commit = %q, want v0.1.2", commit)
	}
	if buildDate != "" {
		t.Errorf("buildDate = %q, want empty", buildDate)
	}
}

// TestNoBuildInfo verifies the last-resort sentinels: nothing derivable, so
// the caller keeps the "unknown" defaults.
func TestNoBuildInfo(t *testing.T) {
	commit, buildDate := deriveFromBuildInfo(nil, "(devel)")
	if commit != "" {
		t.Errorf("commit = %q, want empty", commit)
	}
	if buildDate != "" {
		t.Errorf("buildDate = %q, want empty", buildDate)
	}
}

func TestShortRev(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"0123456789abcdef", "0123456"},
		{"abc1234", "abc1234"},
		{"abc", "abc"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := shortRev(tt.in); got != tt.want {
			t.Errorf("shortRev(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
