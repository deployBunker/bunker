package agent

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureSubIDs_AlreadyConfigured(t *testing.T) {
	// A host that already carries entries must be left alone: this is the
	// idempotent re-spawn path. The fixture is the REAL /etc/subuid content (so
	// the test proves the allocator reads a real host's entry set), copied into
	// a temp database because allocation needs a writable file and a writable
	// lock directory — the same seam the spawn fixtures use.
	u, err := user.Current()
	if err != nil {
		t.Skipf("cannot determine current user: %v", err)
	}
	realUID, err := os.ReadFile("/etc/subuid")
	if err != nil || len(realUID) == 0 {
		t.Skipf("no readable /etc/subuid on this host: %v", err)
	}
	realGID, err := os.ReadFile("/etc/subgid")
	if err != nil || len(realGID) == 0 {
		t.Skipf("no readable /etc/subgid on this host: %v", err)
	}

	dir := t.TempDir()
	uidPath := filepath.Join(dir, "subuid")
	gidPath := filepath.Join(dir, "subgid")
	if err := os.WriteFile(uidPath, realUID, 0o644); err != nil {
		t.Fatalf("seed subuid fixture: %v", err)
	}
	if err := os.WriteFile(gidPath, realGID, 0o644); err != nil {
		t.Fatalf("seed subgid fixture: %v", err)
	}
	restoreUID := subUIDPath
	restoreGID := subGIDPath
	restoreLock := subIDLockDir
	subUIDPath, subGIDPath, subIDLockDir = uidPath, gidPath, dir
	t.Cleanup(func() {
		subUIDPath, subGIDPath, subIDLockDir = restoreUID, restoreGID, restoreLock
	})

	if err := configureSubIDs(t.Context(), u.Username); err != nil {
		t.Fatalf("configureSubIDs for current user: %v", err)
	}

	// The entry the host already had must survive byte-for-byte, and the
	// allocator must not have appended a second one for the same name.
	got, err := os.ReadFile(uidPath)
	if err != nil {
		t.Fatalf("read subuid fixture: %v", err)
	}
	if string(got) != string(realUID) {
		t.Errorf("existing subuid database was rewritten: got %q, want %q", got, realUID)
	}
	if n := strings.Count(string(got), u.Username+":"); n != strings.Count(string(realUID), u.Username+":") {
		t.Errorf("subuid entry count for %q changed: %d -> %d", u.Username, strings.Count(string(realUID), u.Username+":"), n)
	}
}

func TestEnsureSubIDEntry_AppendsNewEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subuid")
	if err := ensureSubIDEntry(path, "testuser", 100000); err != nil {
		t.Fatalf("ensureSubIDEntry: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp subuid: %v", err)
	}
	want := "testuser:100000:65536\n"
	if string(data) != want {
		t.Errorf("got %q, want %q", string(data), want)
	}

	// Second call should be a no-op (idempotent).
	if err := ensureSubIDEntry(path, "testuser", 100000); err != nil {
		t.Fatalf("ensureSubIDEntry second call: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp subuid after second call: %v", err)
	}
	if strings.Count(string(data), "testuser") != 1 {
		t.Errorf("expected exactly one entry for testuser, got:\n%s", string(data))
	}
}

func TestEnsureSubIDEntry_DifferentUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subgid")
	for _, tc := range []struct {
		name  string
		start int
	}{
		{"alpha", 100000},
		{"beta", 200000},
	} {
		if err := ensureSubIDEntry(path, tc.name, tc.start); err != nil {
			t.Fatalf("ensureSubIDEntry %s: %v", tc.name, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp subgid: %v", err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if !strings.Contains(string(data), name) {
			t.Errorf("missing entry for %s", name)
		}
	}
}
