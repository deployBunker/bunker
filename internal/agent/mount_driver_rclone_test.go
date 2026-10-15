// Package agent: the MOUNT-007 rclone driver command-builder tests.
package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/mountdriver"
)

// TestBuildMountCommandRclone pins the rclone command shape the server
// stores at spawn: an inline :sftp:<home> backend (no rclone.conf, no
// credential file), the agent's EXISTING sshd identity (user, key, host —
// the agent installs nothing new), the sshfs mountpoint convention, the VFS
// cache mode, the FUSE mode pins (--file-perms 0777 --umask 022, DF-BUNKER-94
// — see TestMountDriverRcloneUmaskParity), and --daemon so `bunker mount`
// returns instead of blocking on the VFS loop. This is the byte-exact shape
// the client rewrite (internal/cli/mount_rclone.go) anchors on: change the
// shape here and that file's anchors are the tests that must move with it.
func TestBuildMountCommandRclone(t *testing.T) {
	d, err := resolveMountDriver(mountdriver.DriverRclone)
	if err != nil {
		t.Fatalf("resolveMountDriver(%q): %v", mountdriver.DriverRclone, err)
	}
	got, err := buildMountCommand(d, "/etc/bunkerd/ssh/abc123", "bunker-abc123", "myhost", "/home/bunker-abc123", "abc123")
	if err != nil {
		t.Fatalf("buildMountCommand: %v", err)
	}
	want := "rclone mount :sftp:/home/bunker-abc123 /mnt/bunker/abc123 --sftp-user bunker-abc123 --sftp-key-file /etc/bunkerd/ssh/abc123 --sftp-host myhost --vfs-cache-mode minimal --file-perms 0777 --umask 022 --daemon"
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
	// DF-BUNKER-94 acceptance (a): the stored command must carry the umask.
	if !strings.Contains(got, "--umask 022") {
		t.Errorf("stored rclone command must pin the umask (DF-BUNKER-94): %s", got)
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
	if !strings.HasSuffix(got, " --umask 022 --daemon") {
		t.Errorf("--daemon must be last so bunker mount never blocks on the VFS loop, with the mode pins before it: %s", got)
	}
}

// TestBuildMountCommandSSHFSUnchangedByRclone is the blast-radius pin: the
// sshfs default arm's output is byte-identical before and after the rclone
// arm was added (the rclone arm is a separate switch case; the sshfs case
// and its format string are untouched — DF-BUNKER-94 acceptance (d)).
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

// ---------------------------------------------------------------------------
// DF-BUNKER-94: FUSE mode parity (exec bits survive the rclone mount)
// ---------------------------------------------------------------------------

// rclone's FUSE layer decides what a mount REPORTS, and it never uses the
// SFTP object's mode: files surface as --file-perms masked by --umask. Its
// documented defaults are 0666 and 002 (rclone v1.75.1 `mount --help`:
// "--file-perms FileMode  File permissions (default 666)", "--umask FileMode
// Override the permission bits set by the filesystem (default 002)"), which
// is exactly the 0664 `-rw-rw-r--` the dogfood probe measured. The oracle
// below restates them because it must reproduce the measured behaviour to be
// worth anything.
const (
	rcloneDefaultFilePerms = os.FileMode(0o666)
	rcloneDefaultUmask     = os.FileMode(0o002)
)

// groupOtherReadExec is r-x for group and other: the bits --umask must never
// touch, or every file on the mount loses its exec bit for group/other the
// way the sshfs arm does not (DF-BUNKER-94 acceptance (b)).
const groupOtherReadExec = os.FileMode(0o040 | 0o004 | 0o010 | 0o001)

// rcloneMountOptions parses the stored mount command into its `--flag value`
// pairs (a valueless flag maps to ""). The parity oracle reads its inputs
// from THIS map, so it can only assert what the builder actually stores —
// never a copy of the expected command string.
func rcloneMountOptions(t *testing.T, mountCmd string) map[string]string {
	t.Helper()
	fields := strings.Fields(mountCmd)
	opts := make(map[string]string, len(fields))
	for i := 0; i < len(fields); i++ {
		if !strings.HasPrefix(fields[i], "--") {
			continue
		}
		if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "--") {
			opts[fields[i]] = fields[i+1]
			i++
			continue
		}
		opts[fields[i]] = ""
	}
	return opts
}

// rcloneModeOption returns the octal mode an rclone flag carries, falling
// back to the flag's documented default when the flag is absent. A value that
// is not an octal mode fails the test loudly instead of silently comparing
// against a zero mask.
func rcloneModeOption(t *testing.T, opts map[string]string, flag string, def os.FileMode) os.FileMode {
	t.Helper()
	v, ok := opts[flag]
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseUint(v, 8, 32)
	if err != nil {
		t.Fatalf("stored rclone command carries %s %q, which is not an octal mode: %v", flag, v, err)
	}
	return os.FileMode(n)
}

// TestMountDriverRcloneUmaskParity is the DF-BUNKER-94 parity contract: the
// mode the rclone arm's stored command makes a mount REPORT must carry the
// exec bits a chmod 755 agent file carried agent-side, and the umask must not
// mask read/exec for group or other. sshfs (idmap=user) passes the reported
// mode through, so `-rwxr-xr-x` is the parity target: before this row the
// rclone arm showed `-rw-rw-r--` and `./built-binary` off the mount died with
// `Permission denied`.
//
// The oracle applies rclone's own FUSE rule (filePerms &^ umask) to the
// options the BUILDER stored — a string-containment check would pass for a
// command that still cannot execute anything.
func TestMountDriverRcloneUmaskParity(t *testing.T) {
	d, err := resolveMountDriver(mountdriver.DriverRclone)
	if err != nil {
		t.Fatalf("resolveMountDriver(%q): %v", mountdriver.DriverRclone, err)
	}
	cmd, err := buildMountCommand(d, "/etc/bunkerd/ssh/abc123", "bunker-abc123", "myhost", "/home/bunker-abc123", "abc123")
	if err != nil {
		t.Fatalf("buildMountCommand: %v", err)
	}
	opts := rcloneMountOptions(t, cmd)
	filePerms := rcloneModeOption(t, opts, "--file-perms", rcloneDefaultFilePerms)
	umask := rcloneModeOption(t, opts, "--umask", rcloneDefaultUmask)

	// Acceptance (b), literally: the umask must not mask group/other READ or
	// EXEC ... and must still mask group/other WRITE, so the mount is not
	// widened to 0777 in exchange for the exec bit.
	if umask&groupOtherReadExec != 0 {
		t.Errorf("--umask %04o masks read/exec bits for group/other; a 0755 agent file cannot surface as rwxr-xr-x (DF-BUNKER-94): %s", umask, cmd)
	}
	if umask&0o022 == 0 {
		t.Errorf("--umask %04o does not mask group/other write — the mount gains a wider mode than sshfs reports: %s", umask, cmd)
	}

	for _, tc := range []struct {
		name      string
		agentMode os.FileMode // what the file carries on the AGENT side
		wantMode  os.FileMode // what the mount must report
	}{
		{"chmod 755 agent script (the dogfood probe)", 0o755, 0o755},
		{"chmod 700 agent file", 0o700, 0o755},
		{"chmod 644 agent data file", 0o644, 0o755},
	} {
		t.Run(tc.name, func(t *testing.T) {
			surfaced := filePerms &^ umask
			if surfaced != tc.wantMode {
				t.Errorf("a %04o agent file surfaces as %04o (%s) through the rclone mount, want %04o: %s",
					tc.agentMode, surfaced, surfaced, tc.wantMode, cmd)
			}
			if surfaced&0o100 == 0 {
				t.Errorf("a %04o agent file is not executable by its owner through the mount: %s", tc.agentMode, cmd)
			}
			if got := surfaced & groupOtherReadExec; got != groupOtherReadExec {
				t.Errorf("group/other read+exec missing from the surfaced mode (have %04o, want %04o): %s", got, groupOtherReadExec, cmd)
			}
		})
	}

	// Non-vacuity control: the PRE-FIX stored shape (no --file-perms, no
	// --umask) must fail the assertions above, or this oracle proves nothing.
	// Live-measured on the pre-fix shape: 0666 &^ 002 = 0664 `-rw-rw-r--` —
	// the dogfood observation, with `./x.sh` off the mount exiting 126.
	prefixSurfaced := rcloneDefaultFilePerms &^ rcloneDefaultUmask
	if prefixSurfaced != 0o664 {
		t.Errorf("pre-fix rclone default mode is %04o, want the dogfood-measured 0664 — re-derive the oracle before trusting it", prefixSurfaced)
	}
	if prefixSurfaced&0o111 != 0 {
		t.Error("the pre-fix shape must surface NO exec bits; if it does, this oracle cannot catch DF-BUNKER-94")
	}
	// And the umask-only arm the row hypothesised: --umask 022 without the
	// file-perms pin is STILL non-executable (0666 &^ 022 = 0644), which is
	// why both options are load-bearing in the stored command.
	if only := rcloneDefaultFilePerms &^ umask; only&0o111 != 0 {
		t.Errorf("--umask %04o alone must not be enough to make a file executable (0666 &^ %04o = %04o): the --file-perms pin is missing from the contract", umask, umask, only)
	}
}

// rcloneLogTail returns the tail of an rclone log for a failure message. A
// missing or empty log reports that reason instead of an empty string, so a
// skipped live arm still says why it could not run.
func rcloneLogTail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("unreadable (%v)", err)
	}
	text := strings.TrimRight(string(b), "\n")
	if text == "" {
		return "empty"
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return "\n" + strings.Join(lines, "\n")
}

// TestMountDriverRcloneUmaskParityLive is the live arm of the contract above:
// it mounts a real rclone mount with the EXACT option set the builder stores
// (only the :sftp: remote and the mountpoint are substituted) and stats +
// executes a chmod 755 file off the mount — the row's own PASS criterion.
//
// Opt-in (BUNKER_LIVE_RCLONE_PARITY=1) because it needs rclone and /dev/fuse;
// default runs get the hermetic oracle, which is the same rule applied to the
// same stored options. Environmental failures (no rclone, no FUSE, mount
// never becomes readable) SKIP with the reason: the suite must not go red for
// a host that cannot mount FUSE at all. A mount that comes up with the wrong
// mode FAILS — that is the defect.
func TestMountDriverRcloneUmaskParityLive(t *testing.T) {
	if os.Getenv("BUNKER_LIVE_RCLONE_PARITY") == "" {
		t.Skip("live arm disabled: set BUNKER_LIVE_RCLONE_PARITY=1 (needs rclone + /dev/fuse)")
	}
	rcloneBin, err := exec.LookPath("rclone")
	if err != nil {
		t.Skipf("rclone not installed on this host: %v", err)
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("no /dev/fuse on this host: %v", err)
	}

	d, err := resolveMountDriver(mountdriver.DriverRclone)
	if err != nil {
		t.Fatalf("resolveMountDriver(%q): %v", mountdriver.DriverRclone, err)
	}
	stored, err := buildMountCommand(d, "/etc/bunkerd/ssh/probe", "probe-user", "probe-host", "/home/probe-user", "probe-agent")
	if err != nil {
		t.Fatalf("buildMountCommand: %v", err)
	}

	src := t.TempDir()
	probe := filepath.Join(src, "x.sh")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\necho OK-FROM-MOUNT\n"), 0o755); err != nil {
		t.Fatalf("write probe script: %v", err)
	}
	// WriteFile is umask-filtered on some hosts; the probe must be 0755.
	if err := os.Chmod(probe, 0o755); err != nil {
		t.Fatalf("chmod probe script: %v", err)
	}

	// The MOUNTPOINT has to sit on a filesystem where this user may mount
	// FUSE. t.TempDir() honours TMPDIR, which on this fleet's sandboxed agents
	// is the scratch tree under /var/tmp — there fusermount3 answers
	// `mount failed: Permission denied` for every unprivileged mount
	// (measured: the same mount succeeds under /tmp and fails under
	// /var/tmp/hermes-scratch), which would turn a mode contract into a
	// permanent environment skip. Prefer /tmp, where the client's own
	// /mnt/bunker/<id> lives on a real filesystem, and fall back to
	// t.TempDir() when /tmp is unusable.
	mountRoot, err := os.MkdirTemp("/tmp", "bunker-rclone-parity-")
	if err != nil {
		t.Logf("mounting under t.TempDir(): /tmp is unusable (%v)", err)
		mountRoot = t.TempDir()
	} else {
		t.Cleanup(func() { _ = os.RemoveAll(mountRoot) })
	}
	mountPoint := filepath.Join(mountRoot, "mnt")
	if err := os.Mkdir(mountPoint, 0o755); err != nil {
		t.Fatalf("create mountpoint: %v", err)
	}
	// --log-file makes a daemon that refuses to start diagnosable: without it
	// rclone's forked daemon reports only "daemon exited with error code 1".
	// It must stay OUTSIDE the mountpoint, or rclone refuses a non-empty dir.
	logPath := filepath.Join(mountRoot, "rclone-live.log")

	// argv: the STORED command with the remote and mountpoint swapped, so the
	// live arm and the hermetic oracle can never drift apart.
	argv := strings.Fields(stored)
	backend := -1
	for i, f := range argv {
		if strings.HasPrefix(f, ":sftp:") {
			backend = i
			break
		}
	}
	if backend < 0 || backend+1 >= len(argv) {
		t.Fatalf("stored rclone command has no :sftp:<home> <mountpoint> pair: %s", stored)
	}
	argv[backend] = ":local:" + src
	argv[backend+1] = mountPoint
	argv = append(argv[:len(argv)-1], "--log-file", logPath, argv[len(argv)-1])

	// The child's stdio goes to /dev/null, NOT a CombinedOutput pipe:
	// --daemon forks a daemon that outlives the parent, and a daemon holding
	// the write end of that pipe keeps CombinedOutput blocked until the
	// context deadline kills the parent — turning a working mount into a
	// skip. rclone's own log file is where the diagnostics are.
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer func() { _ = devnull.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, rcloneBin, argv[1:]...)
	cmd.Stdout, cmd.Stderr = devnull, devnull
	if err := cmd.Run(); err != nil {
		t.Skipf("rclone mount did not start on this host (environmental): %v\nsrc: %s\nmountpoint: %s\nlog: %s",
			err, src, mountPoint, rcloneLogTail(logPath))
	}
	t.Cleanup(func() {
		uctx, ucancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ucancel()
		for _, bin := range []string{"fusermount3", "fusermount"} {
			if _, err := exec.LookPath(bin); err != nil {
				continue
			}
			if err := exec.CommandContext(uctx, bin, "-u", mountPoint).Run(); err == nil {
				return
			}
		}
	})

	// Poll for the mountpoint rather than sleeping blind; a mount that never
	// appears is environmental, not a mode defect.
	mounted := filepath.Join(mountPoint, "x.sh")
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(mounted); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Skipf("rclone mount never became readable within 20s (environmental): %s\nlog: %s", stored, rcloneLogTail(logPath))
		}
		time.Sleep(250 * time.Millisecond)
	}

	fi, err := os.Stat(mounted)
	if err != nil {
		t.Fatalf("stat %s: %v", mounted, err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("a chmod 755 agent file surfaces as %04o (%s) through the rclone mount, want 0755 rwxr-xr-x — the sshfs arm reports 0755 for the same file (DF-BUNKER-94)", fi.Mode().Perm(), fi.Mode().Perm())
	}
	execOut, err := exec.Command(mounted).CombinedOutput()
	if err != nil {
		t.Fatalf("executing the 0755 file straight off the rclone mount failed (the row's PASS criterion): %v\n%s", err, execOut)
	}
	if got := strings.TrimSpace(string(execOut)); got != "OK-FROM-MOUNT" {
		t.Errorf("script off the mount printed %q, want OK-FROM-MOUNT", got)
	}
}
