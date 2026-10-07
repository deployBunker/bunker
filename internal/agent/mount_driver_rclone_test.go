// Package agent: the MOUNT-007 rclone driver command-builder tests.
package agent

import (
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/mountdriver"
)

// TestBuildMountCommandRclone pins the rclone command shape the server
// stores at spawn: an inline :sftp:<home> backend (no rclone.conf, no
// credential file), the agent's EXISTING sshd identity (user, key, host —
// the agent installs nothing new), the sshfs mountpoint convention, the VFS
// cache mode, and --daemon so `bunker mount` returns instead of blocking on
// the VFS loop. This is the byte-exact shape the client rewrite
// (internal/cli/mount_rclone.go) anchors on: change the shape here and that
// file's anchors are the tests that must move with it.
func TestBuildMountCommandRclone(t *testing.T) {
	d, err := resolveMountDriver(mountdriver.DriverRclone)
	if err != nil {
		t.Fatalf("resolveMountDriver(%q): %v", mountdriver.DriverRclone, err)
	}
	got, err := buildMountCommand(d, "/etc/bunkerd/ssh/abc123", "bunker-abc123", "myhost", "/home/bunker-abc123", "abc123")
	if err != nil {
		t.Fatalf("buildMountCommand: %v", err)
	}
	want := "rclone mount :sftp:/home/bunker-abc123 /mnt/bunker/abc123 --sftp-user bunker-abc123 --sftp-key-file /etc/bunkerd/ssh/abc123 --sftp-host myhost --vfs-cache-mode minimal --daemon"
	if got != want {
		t.Errorf("rclone mount command shape:\n got  %s\n want %s", got, want)
	}
	// Acceptance (e), named explicitly: the output contains "rclone mount",
	// ":sftp:", and the right mountpoint.
	for _, want := range []string{"rclone mount", ":sftp:", "/mnt/bunker/abc123"} {
		if !strings.Contains(got, want) {
			t.Errorf("rclone command missing %q: %s", want, got)
		}
	}
}

// TestBuildMountCommandRcloneMountpointConvention asserts the mountpoint is
// filepath.Join("/mnt", "bunker", agentID) — the sshfs convention — so the
// client's mountpoint substitution stays driver-agnostic.
func TestBuildMountCommandRcloneMountpointConvention(t *testing.T) {
	d, err := resolveMountDriver(mountdriver.DriverRclone)
	if err != nil {
		t.Fatalf("resolveMountDriver: %v", err)
	}
	got, err := buildMountCommand(d, "/k", "u", "h", "/home/u", "agent-x")
	if err != nil {
		t.Fatalf("buildMountCommand: %v", err)
	}
	if !strings.Contains(got, " /mnt/bunker/agent-x ") {
		t.Errorf("rclone command must mount at /mnt/bunker/<id> (the sshfs convention): %s", got)
	}
	if !strings.HasSuffix(got, " --vfs-cache-mode minimal --daemon") {
		t.Errorf("--daemon must be last so bunker mount never blocks on the VFS loop: %s", got)
	}
}

// TestBuildMountCommandSSHFSUnchangedByRclone is the blast-radius pin: the
// sshfs default arm's output is byte-identical before and after the rclone
// arm was added (the rclone arm is a separate switch case; the sshfs case
// and its format string are untouched).
func TestBuildMountCommandSSHFSUnchangedByRclone(t *testing.T) {
	d, err := resolveMountDriver(mountdriver.DefaultDriver)
	if err != nil {
		t.Fatalf("resolveMountDriver: %v", err)
	}
	got, err := buildMountCommand(d, "/etc/bunkerd/ssh/abc123", "bunker-abc123", "myhost", "/home/bunker-abc123", "abc123")
	if err != nil {
		t.Fatalf("buildMountCommand: %v", err)
	}
	want := "sshfs -o IdentityFile=/etc/bunkerd/ssh/abc123 -o idmap=user -o allow_other bunker-abc123@myhost:/home/bunker-abc123 /mnt/bunker/abc123"
	if got != want {
		t.Errorf("sshfs mount command changed when the rclone arm was added:\n got  %s\n want %s", got, want)
	}
}
