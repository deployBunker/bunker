//go:build unix

package cli

// DF-BUNKER-39 regression tests: the remote path must be RESOLVED (explicit
// --path > source path from the stored sshfs command > ".") BEFORE the
// preflight runs. The preflight refuses an empty path with "mount preflight:
// no remote path to check", so running it first made the DOCUMENTED default
// form `bunker mount <agent-id>` impossible. These tests drive the real mount
// RunE with the existing stubRemotePathCheck seam and assert the preflight
// was invoked with the resolved, non-empty path.

import (
	"io"
	"testing"
)

// TestMountDefaultPath_PreflightReceivesResolvedPath is the ordering
// contract: with --path unset and a stored command that embeds a source
// path, the preflight must be invoked with that source path — the same
// path the mount will actually use — and the mount must reach sshfs.
func TestMountDefaultPath_PreflightReceivesResolvedPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)
	sshfsCalls, restoreSSHFS := stubSSHFSRun(t, 0, nil)
	defer restoreSSHFS()
	calls, restorePreflight := stubRemotePathCheck(t, nil)
	defer restorePreflight()

	mountpoint := t.TempDir() + "/mnt"
	cmd := NewMountCommand()
	cmd.SetArgs([]string{"df0916a", mountpoint}) // --path deliberately unset
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("preflight called %d times, want exactly 1", len(*calls))
	}
	want := "bunker-agent@127.0.0.1|/home/bunker-agent"
	if got := (*calls)[0]; got != want {
		t.Fatalf("preflight called with %q, want %q (the stored command's source path)", got, want)
	}

	// The mount must reach sshfs: the documented default form must mount,
	// not abort at the preflight.
	if len(*sshfsCalls) == 0 {
		t.Fatal("sshfs seam never called: the default form still never mounts")
	}
}

// TestMountDefaultPath_PreflightReceivesDotWhenStoredCommandHasNoSourcePath
// covers the second default: a stored command whose target has no path
// component (host root, so lastRemoteSourcePath yields "") still must not
// reach the preflight with an empty path — it defaults to "." (the agent
// home).
func TestMountDefaultPath_PreflightReceivesDotWhenStoredCommandHasNoSourcePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, "sshfs -o idmap=user bunker-agent@bunker-host: /mnt/bunker/df0916a")
	writeMountClientKey(t)
	_, restoreSSHFS := stubSSHFSRun(t, 0, nil)
	defer restoreSSHFS()
	calls, restorePreflight := stubRemotePathCheck(t, nil)
	defer restorePreflight()

	mountpoint := t.TempDir() + "/mnt"
	cmd := NewMountCommand()
	cmd.SetArgs([]string{"df0916a", mountpoint})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (scenario should reach the mount attempt, not refuse at preflight)", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("preflight called %d times, want exactly 1", len(*calls))
	}
	want := "bunker-agent@127.0.0.1|."
	if got := (*calls)[0]; got != want {
		t.Fatalf("preflight called with %q, want %q (dot default)", got, want)
	}
}

// TestMountDefaultPath_ExplicitPathStillWins pins the override: an explicit
// --path must reach the preflight verbatim, never replaced by the stored
// command's source path or the dot default.
func TestMountDefaultPath_ExplicitPathStillWins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)
	_, restoreSSHFS := stubSSHFSRun(t, 0, nil)
	defer restoreSSHFS()
	calls, restorePreflight := stubRemotePathCheck(t, nil)
	defer restorePreflight()

	mountpoint := t.TempDir() + "/mnt"
	cmd := NewMountCommand()
	cmd.SetArgs([]string{"df0916a", mountpoint, "--path", "/srv/explicit-repo"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("preflight called %d times, want exactly 1", len(*calls))
	}
	want := "bunker-agent@127.0.0.1|/srv/explicit-repo"
	if got := (*calls)[0]; got != want {
		t.Fatalf("preflight called with %q, want %q (explicit --path must win)", got, want)
	}
}
