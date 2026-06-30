package agent

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureSubIDs_AlreadyConfigured(t *testing.T) {
	// Use the current user — it almost certainly already has subuid/subgid entries.
	u, err := user.Current()
	if err != nil {
		t.Skipf("cannot determine current user: %v", err)
	}

	if err := configureSubIDs(t.Context(), u.Username); err != nil {
		t.Fatalf("configureSubIDs for current user: %v", err)
	}

	// Verify a line for the user exists in /etc/subuid.
	data, err := os.ReadFile("/etc/subuid")
	if err != nil {
		t.Fatalf("read /etc/subuid: %v", err)
	}
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) >= 1 && fields[0] == u.Username {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no subuid entry for user %q", u.Username)
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
