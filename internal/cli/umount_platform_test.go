package cli

// The `bunker umount` platform seam's executable half (BFS-028).
//
// WHAT THIS FILE IS FOR, in one sentence: the refusal a platform without an
// unmount mechanism gets is reachable from a platform that HAS one, so the
// behaviour is exercised here rather than asserted from a build tag.
//
// The problem it solves is not hypothetical. A seam whose `!unix` arm is the only
// place it is exercised is a seam nobody has run — that is exactly how
// internal/fsmount's first non-Linux file shipped broken (BFS-010: it declared
// `Mount` where its callers used `MountAt`, and every test in the repo ran on
// Linux, where the file is not compiled at all). The answer the repo settled on
// there is the one used here: put the SENTENCE and the DECISION in an untagged
// file, so a test on the ordinary platform drives the real code path, and let the
// platform arm be the thinnest possible binding to the build's own identity.
//
// THE SEAM. runUmount's first statement is
//
//	if refusal := umountPlatformRefusal(); refusal != nil { return refusal }
//
// and `umountPlatformRefusal` is a package var holding the platform's answer
// (nil on unix, the named refusal elsewhere). Substituting the var is therefore
// the ONLY difference between "Windows" and "Linux" as far as the code under test
// is concerned: the cells below run the REAL runUmount and the REAL cobra
// command, with the platform's answer supplied. No build tag, no skipped test, no
// test-only code path in production.
//
// NEGATIVE CONTROLS (docs/evidence/BFS-028-arms.sh), because a cell that cannot
// fail proves nothing:
//   - remove the gate from runUmount          -> the reachability cell goes RED
//   - reword the refusal into a generic one   -> the wording cell goes RED
//   - make the unix arm refuse as well        -> every POSIX cell in the package
//     goes RED (which is what "POSIX behaviour is unchanged" means when tested)
//
// This file is deliberately UNTAGGED: on Windows these same cells run against the
// real refusal the platform arm returns.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- harness ----------------------------------------------------------------

// stubPlatformRefusal makes this build answer like a platform with no unmount
// mechanism. It is the whole trick: the gate, the resolution order and the
// unmounting below are the production ones.
func stubPlatformRefusal(t *testing.T, refusal error) {
	t.Helper()
	old := umountPlatformRefusal
	umountPlatformRefusal = func() error { return refusal }
	t.Cleanup(func() { umountPlatformRefusal = old })
}

// stubSeamMountTable injects a live mount table through the same seam the
// production code reads /proc/self/mounts through. Named apart from the unix
// test file's helper so both can coexist in one package.
func stubSeamMountTable(t *testing.T, lines ...string) {
	t.Helper()
	old := readMountTable
	readMountTable = func() ([]string, error) { return lines, nil }
	t.Cleanup(func() { readMountTable = old })
}

// recordSeamCommands replaces the exec seam so the test can prove that NOTHING
// was executed. It returns the recording; the injected argv is served by `true`
// so that even a command the cell did not expect cannot touch a real path.
func recordSeamCommands(t *testing.T) *[][]string {
	t.Helper()
	calls := &[][]string{}
	old := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd {
		*calls = append(*calls, append([]string{name}, arg...))
		return exec.Command("true")
	}
	t.Cleanup(func() { execCommand = old })
	return calls
}

// isolateSeamMountRoots points every default mount root at fresh empty
// directories, so a cell about the platform gate cannot accidentally resolve
// through an ambient root on the test host. Returns the first root.
func isolateSeamMountRoots(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BUNKER_MOUNT_ROOT", root)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	return root
}

// --- the row's cell ---------------------------------------------------------

// TestUmount_UnsupportedPlatformRefusalReachesTheOperator is the cell that makes
// the Windows decision REACHABLE on a platform that can run tests.
//
// It runs the real runUmount with the platform's answer substituted, in the two
// states the accidentally-successful behaviour would have been invisible in, and
// requires the refusal in both WITH NO SIDE EFFECT:
//
//   - a LIVE MOUNT for the agent: the accidental behaviour would have DETACHED a
//     path this build cannot detach (or tried to), and said "Unmounted <path>";
//   - NOTHING ANYWHERE, plus the empty leftover directory a previous run left at
//     the resolved root: the accidental behaviour is the quiet one — it would
//     REMOVE that directory and print "Nothing mounted at ... (already clean)",
//     reporting success over a mount it cannot see. That is the DF-BUNKER-50
//     false positive one platform over, and it is the shape this cell exists for.
func TestUmount_UnsupportedPlatformRefusalReachesTheOperator(t *testing.T) {
	const agent = "f0901fd3"

	t.Run("a live mount for the agent is refused, not detached", func(t *testing.T) {
		isolateSeamMountRoots(t)
		// Outside every mount root, exactly as an explicit `bunker mount <agent>
		// /mnt/bunker/<agent>` would place it. Built from a temp dir so the cell
		// reads the same on every platform.
		live := filepath.Join(t.TempDir(), "custom", agent)
		stubSeamMountTable(t, strings.Join([]string{"sshfs#peer:/", live, "fuse.sshfs", "rw", "0", "0"}, " "))
		calls := recordSeamCommands(t)
		stubPlatformRefusal(t, umountUnsupportedRefusal("windows", "amd64"))

		var out bytes.Buffer
		err := runUmount(&out, agent, false)

		if err == nil {
			t.Fatalf("runUmount reported success on a platform with no unmount mechanism; output was %q", out.String())
		}
		if !errors.Is(err, ErrUmountUnsupported) {
			t.Errorf("the refusal does not wrap ErrUmountUnsupported, so a caller cannot match it: %v", err)
		}
		if out.Len() != 0 {
			t.Errorf("the command printed %q; a refusal must not print a report about a mount it never saw", out.String())
		}
		if got := *calls; len(got) != 0 {
			t.Errorf("an unmount was executed on a platform that has no unmount: %v", got)
		}
	})

	t.Run("the silent already-clean shape is refused, and the leftover directory survives", func(t *testing.T) {
		root := isolateSeamMountRoots(t)
		// The empty directory findMountPointForAgent resolves for this agent —
		// the "leftover from a previous run" the clean path removes.
		leftover := filepath.Join(root, agent)
		if err := os.MkdirAll(leftover, 0o700); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		stubSeamMountTable(t) // a readable table that shows nothing
		calls := recordSeamCommands(t)
		stubPlatformRefusal(t, umountUnsupportedRefusal("windows", "amd64"))

		var out bytes.Buffer
		err := runUmount(&out, agent, false)

		if err == nil {
			t.Fatalf("runUmount reported success (output %q) where the honest answer is that this build cannot look", out.String())
		}
		if !errors.Is(err, ErrUmountUnsupported) {
			t.Errorf("the refusal does not wrap ErrUmountUnsupported: %v", err)
		}
		if out.Len() != 0 {
			t.Errorf("the command printed %q; the accidental behaviour here is a clean report over an unreadable platform", out.String())
		}
		if _, statErr := os.Lstat(leftover); statErr != nil {
			t.Errorf("the resolved mount root %s was touched (%v); the refusal must precede every local side effect", leftover, statErr)
		}
		if got := *calls; len(got) != 0 {
			t.Errorf("an unmount was executed: %v", got)
		}
	})
}

// TestUmount_UnsupportedRefusalNamesTheCommandPlatformAndWayOut pins the SENTENCE,
// which is the entire product of the decision on the platform that gets it: the
// operator has no other signal. Built here (untagged) with a named platform so the
// wording is exercised on the host that runs tests — the same reason
// internal/fsmount's ErrPlatformUnsupported lives in an untagged file and is
// asserted by internal/mountdriver/platform_refusal_test.go.
//
// The four things this repo's standard requires of a refusal, each asserted
// separately so a mutation that drops one names the one it dropped:
func TestUmount_UnsupportedRefusalNamesTheCommandPlatformAndWayOut(t *testing.T) {
	err := umountUnsupportedRefusal("windows", "amd64")
	msg := err.Error()

	if !errors.Is(err, ErrUmountUnsupported) {
		t.Fatalf("the refusal does not wrap ErrUmountUnsupported, so it cannot be matched: %v", err)
	}
	cases := []struct {
		what string
		want string
	}{
		{"the COMMAND the operator ran", "bunker umount"},
		{"the PLATFORM this build is", "windows/amd64"},
		{"the statement that nothing happened", "Nothing was unmounted"},
		{"the WAY OUT", "Linux client"},
		{"the concrete remedy for this platform", "net use"},
	}
	for _, tc := range cases {
		if !strings.Contains(msg, tc.want) {
			t.Errorf("the refusal does not name %s (want %q in it), so it names the problem without the next step: %q",
				tc.what, tc.want, msg)
		}
	}
	// The platform is an ARGUMENT, not a constant: a build that reported the wrong
	// platform would send the operator to the wrong remedy.
	if other := umountUnsupportedRefusal("plan9", "riscv64").Error(); !strings.Contains(other, "plan9/riscv64") {
		t.Errorf("the refusal does not carry the platform it was built for: %q", other)
	}
}

// TestUmountCommand_RefusesAsACommand is the end-to-end half: the command is still
// REGISTERED (not excluded from the build, not hidden from `bunker --help`) and it
// refuses when executed, rather than printing a clean report and exiting 0.
//
// It executes the real cobra command with the root command's own output settings,
// which is how cmd/bunker/main.go runs it: SilenceErrors + SilenceUsage are set on
// the root, the error travels back to main, and main prints `bunker: <err>` to
// stderr and exits 1. So the two things asserted here are exactly the two a shell
// would see: a non-nil error, and no success text on the command's output.
func TestUmountCommand_RefusesAsACommand(t *testing.T) {
	stubPlatformRefusal(t, umountUnsupportedRefusal("windows", "amd64"))

	cmd := NewUmountCommand()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"f0901fd3"})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("`bunker umount` succeeded on a platform with no unmount mechanism; output was %q", out.String())
	}
	if !errors.Is(err, ErrUmountUnsupported) {
		t.Errorf("the command's error does not wrap ErrUmountUnsupported: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("the command printed %q; a refusal must not be dressed as a result", out.String())
	}
}
