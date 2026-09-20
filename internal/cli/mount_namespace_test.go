package cli

// GAP-113: mount namespacing by server.
//
// The bug these tests pin down: the mountpoint used to be
// <root>/<agent-id> with no server dimension, so two servers that each have an
// agent named `dev` resolved to the same directory. The second mount found the
// first server's live mountpoint (or an operator read the wrong tree). These
// tests assert the separation, not just that a path is produced.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMountPointNamespacedByServer(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BUNKER_MOUNT_ROOT", root)
	t.Setenv("XDG_RUNTIME_DIR", "")

	a, err := defaultMountPointForServer("bunker-las-01", "dev")
	if err != nil {
		t.Fatalf("server A: %v", err)
	}
	b, err := defaultMountPointForServer("bunker-las-02", "dev")
	if err != nil {
		t.Fatalf("server B: %v", err)
	}

	// The bug: same agent id on two servers must NOT share a mountpoint.
	if a == b {
		t.Fatalf("same agent id on two servers resolved to ONE mountpoint: %s", a)
	}
	if !strings.Contains(a, "bunker-las-01") || !strings.Contains(b, "bunker-las-02") {
		t.Errorf("server not present in mountpoint: %q / %q", a, b)
	}
	if filepath.Base(a) != "dev" || filepath.Base(b) != "dev" {
		t.Errorf("agent id should remain the leaf component: %q / %q", a, b)
	}
	// Both must actually exist and be private.
	for _, p := range []string{a, b} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("mountpoint not created: %s: %v", p, err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v, want 0700 (private mountpoint)", p, fi.Mode().Perm())
		}
	}
}

func TestMountPointAgentIDCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BUNKER_MOUNT_ROOT", root)
	t.Setenv("XDG_RUNTIME_DIR", "")

	for _, bad := range []string{"../../etc", "..", "a/b", "a\x00b", "  ", "/etc/passwd"} {
		got, err := defaultMountPointForServer("srv", bad)
		if err != nil {
			continue // a named refusal is the right answer for an unusable id
		}
		clean := filepath.Clean(got)
		if !strings.HasPrefix(clean, filepath.Clean(root)+string(filepath.Separator)) {
			t.Errorf("agent id %q escaped the mount root: %s (root %s)", bad, clean, root)
		}
		if strings.Contains(clean, "..") {
			t.Errorf("agent id %q left a traversal marker in the path: %s", bad, clean)
		}
	}
}

func TestMountPointServerComponentSanitized(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BUNKER_MOUNT_ROOT", root)
	t.Setenv("XDG_RUNTIME_DIR", "")

	got, err := defaultMountPointForServer("../../../evil", "dev")
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	clean := filepath.Clean(got)
	if !strings.HasPrefix(clean, filepath.Clean(root)+string(filepath.Separator)) {
		t.Fatalf("server component escaped the mount root: %s", clean)
	}
}

func TestSanitizeMountComponentKeepsCleanIDs(t *testing.T) {
	cases := map[string]string{
		"dev":           "dev",
		"bunker-las-02": "bunker-las-02",
		"my_agent.1":    "my_agent.1",
		"a..b":          "a-b",
		"a/b":           "a-b",
		"../x":          "x",
		"..":            "",
	}
	for in, want := range cases {
		if got := sanitizeMountComponent(in); got != want {
			t.Errorf("sanitizeMountComponent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMountPointLegacySingleComponentStillWorks(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BUNKER_MOUNT_ROOT", root)
	t.Setenv("XDG_RUNTIME_DIR", "")

	got, err := defaultMountPoint("abc123")
	if err != nil {
		t.Fatalf("legacy resolver: %v", err)
	}
	if filepath.Base(got) != "abc123" {
		t.Errorf("legacy path leaf = %q, want abc123", filepath.Base(got))
	}
}
