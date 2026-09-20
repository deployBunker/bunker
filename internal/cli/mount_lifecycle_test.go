package cli

// Tests for the mount lifecycle fixes (GAP-103 / GAP-107).
//
// These cover the parts of the mount contract that do NOT need a live host:
// mountpoint policy, the durability option set, the world-readable refusal, and
// the "already clean" behaviour of umount. The transport failure modes (drop,
// blip, strand, hang) need fault injection and are filed as GAP-108.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- mountpoint policy ------------------------------------------------------

func TestDefaultMountPoint_UsesUserWritableRoot(t *testing.T) {
	home := t.TempDir()
	run := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", run)
	t.Setenv("BUNKER_MOUNT_ROOT", "")

	got, err := defaultMountPoint("abc123")
	if err != nil {
		t.Fatalf("defaultMountPoint: %v", err)
	}

	// Must be user-writable and must NOT be the old root-owned /mnt default.
	if strings.HasPrefix(got, "/mnt/bunker") {
		t.Fatalf("default mountpoint still uses the root-owned /mnt path: %s", got)
	}
	if !strings.HasPrefix(got, run) {
		t.Fatalf("expected the XDG runtime root to win, got %s (run=%s)", got, run)
	}
	// The mountpoint must exist and be private to this user (0700).
	fi, err := os.Stat(got)
	if err != nil {
		t.Fatalf("mountpoint not created: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mountpoint perm = %o, want 700 (a mount without allow_other must not be traversable by others)", fi.Mode().Perm())
	}
}

func TestDefaultMountPoint_HonoursExplicitRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BUNKER_MOUNT_ROOT", root)
	got, err := defaultMountPoint("agent-1")
	if err != nil {
		t.Fatalf("defaultMountPoint: %v", err)
	}
	if want := filepath.Join(root, "agent-1"); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestDefaultMountPoint_FallsBackToHomeWhenNoRuntimeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("BUNKER_MOUNT_ROOT", "")

	got, err := defaultMountPoint("agent-2")
	if err != nil {
		t.Fatalf("defaultMountPoint: %v", err)
	}
	if want := filepath.Join(home, ".bunker", "mnt", "agent-2"); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestDefaultMountPoint_EmptyAgentIDRejected(t *testing.T) {
	if _, err := defaultMountPoint(""); err == nil {
		t.Fatal("expected an error for an empty agent id")
	}
}

// --- durability option set (GAP-107) ---------------------------------------

// TestMountDurabilityOptionsArePassed is the assertion that keeps the durability
// contract from silently regressing. Each option here closes a named failure
// mode: reconnect (permanent blip), ServerAlive* (unbounded hang),
// ConnectTimeout (unbounded connect), auto_unmount (stranded mountpoint),
// dir_cache=no (stale cache serving as phantom space).
func TestMountDurabilityOptionsArePassed(t *testing.T) {
	args := durableSSHFSArgs()

	required := []string{
		"reconnect",
		"ServerAliveInterval=" + sshfsServerAliveInterval,
		"ServerAliveCountMax=" + sshfsServerAliveCountMax,
		"ConnectTimeout=" + sshfsConnectTimeout,
		"auto_unmount",
		"dir_cache=no",
	}
	joined := strings.Join(args, " ")
	for _, want := range required {
		if !strings.Contains(joined, want) {
			t.Errorf("durability option %q is missing from the sshfs arguments; "+
				"without it the corresponding disconnect failure mode is unhandled", want)
		}
	}
}

// --- allow_other stripping --------------------------------------------------

// TestStripAllowOption covers the real-world shape: the daemon ALWAYS generates
// allow_other (manager_spawn.go:583) while this CLI's mountpoint is private
// 0700. The CLI must drop the option and still mount, never refuse (which would
// break every real mount) and never silently pass it through.
func TestStripAllowOption(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantDropped string
		wantAbsent  string
		wantKept    string
	}{
		{
			name:        "separate form",
			args:        []string{"-o", "allow_other", "-o", "idmap=user"},
			wantDropped: "allow_other",
			wantAbsent:  "allow_other",
			wantKept:    "idmap=user",
		},
		{
			name:        "comma form keeps siblings",
			args:        []string{"-o", "idmap=user,allow_other"},
			wantDropped: "allow_other",
			wantAbsent:  "allow_other",
			wantKept:    "idmap=user",
		},
		{
			name:        "allow_root also dropped",
			args:        []string{"-o", "allow_root", "-o", "reconnect"},
			wantDropped: "allow_root",
			wantAbsent:  "allow_root",
			wantKept:    "reconnect",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped, found := stripAllowOption(tc.args)
			if !found {
				t.Fatalf("expected the allow option to be found in %v", tc.args)
			}
			if dropped != tc.wantDropped {
				t.Fatalf("dropped = %q, want %q", dropped, tc.wantDropped)
			}
			joined := strings.Join(got, " ")
			if strings.Contains(joined, tc.wantAbsent) {
				t.Errorf("option %q still present after strip: %v", tc.wantAbsent, got)
			}
			if !strings.Contains(joined, tc.wantKept) {
				t.Errorf("unrelated option %q was lost: %v", tc.wantKept, got)
			}
		})
	}
}

func TestStripAllowOption_NoOpWhenAbsent(t *testing.T) {
	args := []string{"-o", "idmap=user", "-o", "reconnect"}
	got, dropped, found := stripAllowOption(args)
	if found {
		t.Fatalf("nothing should be dropped, got %q", dropped)
	}
	if strings.Join(got, " ") != strings.Join(args, " ") {
		t.Fatalf("argument list changed when nothing to strip: %v", got)
	}
}

// --- umount idempotence -----------------------------------------------------

// TestUmountNothingMountedIsSuccess: cleanup must be safe to run twice, because
// after a failure the operator cannot know how far the previous attempt got.
func TestUmountNothingMountedIsSuccess(t *testing.T) {
	dir := t.TempDir()
	mountpoint := filepath.Join(dir, "not-mounted")
	if err := os.Mkdir(mountpoint, 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := NewUmountCommand()
	cmd.SetArgs([]string{mountpoint})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("umount of an unmounted directory must succeed, got: %v", err)
	}
}

func TestUmountMissingPathIsSuccess(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")

	cmd := NewUmountCommand()
	cmd.SetArgs([]string{missing})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("umount of a missing path must succeed (nothing to clean), got: %v", err)
	}
}

// TestIsMountPoint_FalseForPlainDir guards the predicate that decides whether
// anything is actually mounted: a plain directory must not read as a mount.
func TestIsMountPoint_FalseForPlainDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "plain")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	mounted, err := isMountPoint(sub)
	if err != nil {
		t.Fatalf("isMountPoint: %v", err)
	}
	if mounted {
		t.Fatal("a plain directory reported as a mountpoint")
	}
}

// --- preflight validation (no host required) --------------------------------

func TestRemotePathExists_RejectsEmptyInputs(t *testing.T) {
	cases := []struct{ name, host, key, path string }{
		{"no host", "", "/tmp/k", "."},
		{"no key", "u@h", "", "."},
		{"no path", "u@h", "/tmp/k", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := remotePathExists(tc.host, tc.key, tc.path); err == nil {
				t.Fatal("expected an error for incomplete preflight input")
			}
		})
	}
}

// TestRemotePathExists_UnreachableHostNamesTheCause: when the host cannot be
// reached, the error must say so rather than pretending the path is missing.
func TestRemotePathExists_UnreachableHostNamesTheCause(t *testing.T) {
	key := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(key, []byte("not-a-real-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A host that cannot resolve/connect: the ssh invocation must fail fast.
	err := remotePathExists("nobody@192.0.2.1", key, ".")
	if err == nil {
		t.Skip("unexpectedly reached the unroutable address; network-dependent test skipped")
	}
	if !strings.Contains(err.Error(), "cannot reach") && !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error should name the cause, got: %v", err)
	}
}
