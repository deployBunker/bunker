package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserManagementCommandUsesRestrictedPathFallback(t *testing.T) {
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())

	for _, name := range []string{"useradd", "userdel"} {
		if got := userManagementCommand(name); got != filepath.Join("/usr/sbin", name) {
			t.Fatalf("userManagementCommand(%q) = %q, want /usr/sbin/%s", name, got, name)
		}
	}

	t.Setenv("PATH", oldPath)
}

func TestUserManagementCommandPreservesPathHelper(t *testing.T) {
	binDir := t.TempDir()
	for _, name := range []string{"useradd", "userdel"} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	for _, name := range []string{"useradd", "userdel"} {
		if got := userManagementCommand(name); got != name {
			t.Fatalf("userManagementCommand(%q) = %q, want logical PATH command %q", name, got, name)
		}
	}
}
