//go:build unix

package cli

// DF-BUNKER-50 regression tests: `bunker umount <agent-id>` must find a mount
// that lives at a CUSTOM mountpoint, i.e. one that is not under any of the
// default mount roots.
//
// The repro this closes: `bunker mount f0901fd3 /mnt/bunker/f0901fd3 --server
// bunker-las-03` mounts fine, and `bunker umount f0901fd3 --server bunker-las-03`
// then printed "Nothing mounted at /run/user/1000/bunker/mnt/f0901fd3 (already
// clean)" while `mount | grep f0901fd3` showed the mount still live at
// /mnt/bunker/f0901fd3. A cleanup verb that reports success over a live mount is
// a false positive the operator cannot see.
//
// No root and no live host: the mount table is injected through the
// readMountTable seam, and the unmount itself is recorded through the execCommand
// seam instead of being run. Every test therefore asserts on the EXACT path the
// resolution chose, which is the thing that was wrong.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- harness ----------------------------------------------------------------

// stubMountTable injects a fake live mount table (the /proc/self/mounts shape:
// device, mount point, fstype, options, dump, pass) and returns a restore func.
// Injecting is the only way to exercise this path without mounting anything.
func stubMountTable(t *testing.T, lines ...string) func() {
	t.Helper()
	old := readMountTable
	readMountTable = func() ([]string, error) { return lines, nil }
	return func() { readMountTable = old }
}

// stubBrokenMountTable makes the live table unavailable, which is the
// fail-closed path: the command must fall back to what it can prove locally
// instead of concluding anything from an empty result.
func stubBrokenMountTable(t *testing.T, err error) func() {
	t.Helper()
	old := readMountTable
	readMountTable = func() ([]string, error) { return nil, err }
	return func() { readMountTable = old }
}

// stubCommandRunner replaces the execCommand seam so a test can assert on the
// argv the unmount would have run, without running it. The injected mount tables
// name paths the test does not own, so executing for real would be
// catastrophic on a bad fixture; every recorded call is instead served by `true`
// (exit 0), which runWithTimeout treats as a successful unmount. Exactly one
// attempt is therefore made and no lazy escalation appears in the recording.
func stubCommandRunner(t *testing.T) (calls *[][]string, restore func()) {
	t.Helper()
	calls = &[][]string{}
	old := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd {
		*calls = append(*calls, append([]string{name}, arg...))
		return exec.Command("true")
	}
	return calls, func() { execCommand = old }
}

// preferFusermount3 puts a fusermount3 on PATH so the unmount takes the FUSE
// branch deterministically, whatever the test host happens to have installed.
func preferFusermount3(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	writeStubCommand(t, dir, "fusermount3", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// isolateMountRoots points every default mount root at a fresh empty directory,
// so a test about the LIVE MOUNT TABLE cannot accidentally resolve through the
// roots search. Returns the root it installed.
func isolateMountRoots(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BUNKER_MOUNT_ROOT", root)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	return root
}

// assertUnmountedAt is the assertion the whole file is built on: the command ran
// the unmount, and it named exactly the path we expected.
func assertUnmountedAt(t *testing.T, recorded *[][]string, want string) {
	t.Helper()
	calls := *recorded
	if len(calls) == 0 {
		t.Fatal("no unmount was executed at all")
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly one unmount attempt (the stubbed one succeeds, so nothing should escalate), got %d: %v", len(calls), calls)
	}
	argv := calls[0]
	if len(argv) < 2 {
		t.Fatalf("recorded argv is not an unmount invocation: %v", argv)
	}
	if got := argv[len(argv)-1]; got != want {
		t.Fatalf("unmounted %s, want %s (argv: %v)", got, want, argv)
	}
	if base := filepath.Base(argv[0]); base != "fusermount3" && base != "umount" {
		t.Fatalf("unexpected unmount binary %q in %v", argv[0], argv)
	}
}

func assertNoUnmountExecuted(t *testing.T, recorded *[][]string) {
	t.Helper()
	if calls := *recorded; len(calls) != 0 {
		t.Fatalf("nothing should have been unmounted, but the command ran: %v", calls)
	}
}

// --- the row's repro --------------------------------------------------------

// TestUmount_ResolvesCustomMountpointFromLiveMountTable is the regression test
// for DF-BUNKER-50 proper: the agent is mounted at a custom path outside every
// default root, exactly as the repro mounted it, and `umount <agent-id>` must
// find and unmount THAT path rather than reporting it already clean.
func TestUmount_ResolvesCustomMountpointFromLiveMountTable(t *testing.T) {
	const (
		agent  = "f0901fd3"
		custom = "/mnt/bunker/f0901fd3"
	)
	isolateMountRoots(t)
	preferFusermount3(t)
	defer stubMountTable(t,
		"tmpfs /run tmpfs rw,nosuid,nodev,size=12509376k 0 0",
		// The live sshfs mount the repro showed in `mount | grep f0901fd3`.
		"bunker-las-03:/home/bunker-"+agent+" "+custom+" fuse.sshfs rw,nosuid,nodev,relatime,user_id=1000 0 0",
		"/dev/nvme0n1p2 / ext4 rw,relatime 0 0",
	)()
	calls, restoreRun := stubCommandRunner(t)
	defer restoreRun()

	var out bytes.Buffer
	if err := runUmount(&out, agent, true); err != nil {
		t.Fatalf("umount of an agent mounted at a custom path must succeed, got: %v", err)
	}

	assertUnmountedAt(t, calls, custom)

	// Criterion 2: the false-positive branch must not have fired. Both the
	// "already clean" wording and the "nothing mounted" wording would be lies
	// about a live mount.
	got := out.String()
	if strings.Contains(got, "already clean") {
		t.Errorf("output claims the mount was already clean while it is live at %s: %q", custom, got)
	}
	if strings.Contains(got, "Nothing mounted") {
		t.Errorf("output claims nothing is mounted while it is live at %s: %q", custom, got)
	}
	if !strings.Contains(got, "Unmounted "+custom) {
		t.Errorf("output should name the unmounted path, got %q", got)
	}
	// The resolution must be attributed: it did NOT come from the default roots.
	if !strings.Contains(got, custom+" from the live mount table") {
		t.Errorf("output should say the path was resolved from the live mount table, got %q", got)
	}
}

// TestUmount_CustomMountpointNamespaceStillResolves drives the same custom
// mountpoint in the namespaced shape (GAP-113) with the server in the path, so
// the fix is not tied to one layout.
func TestUmount_CustomMountpointNamespaceStillResolves(t *testing.T) {
	const (
		agent  = "f0901fd3"
		custom = "/mnt/bunker/bunker-las-03/f0901fd3"
	)
	isolateMountRoots(t)
	preferFusermount3(t)
	defer stubMountTable(t,
		"bunker-las-03:/home/bunker-"+agent+" "+custom+" fuse.sshfs rw,nosuid,nodev 0 0",
	)()
	calls, restoreRun := stubCommandRunner(t)
	defer restoreRun()

	var out bytes.Buffer
	if err := runUmount(&out, agent, true); err != nil {
		t.Fatalf("umount: %v", err)
	}
	assertUnmountedAt(t, calls, custom)
	if strings.Contains(out.String(), "already clean") {
		t.Errorf("claimed already clean over a live mount: %q", out.String())
	}
}

// TestUmount_ExplicitCustomPathIsHonoured: the operator naming the custom path
// directly must unmount it, and the live table agreeing with it must not produce
// a second candidate (which the ambiguity guard would turn into a refusal).
func TestUmount_ExplicitCustomPathIsHonoured(t *testing.T) {
	isolateMountRoots(t)
	preferFusermount3(t)

	// The path must exist to take the "existing path" branch; the mount behind
	// it is the injected table entry.
	target := t.TempDir()
	defer stubMountTable(t, "bunker-las-03:/home/bunker-f0901fd3 "+target+" fuse.sshfs rw 0 0")()
	calls, restoreRun := stubCommandRunner(t)
	defer restoreRun()

	var out bytes.Buffer
	if err := runUmount(&out, target, true); err != nil {
		t.Fatalf("umount of an explicit custom mountpoint must succeed, got: %v", err)
	}
	assertUnmountedAt(t, calls, target)
	if strings.Contains(out.String(), "already clean") {
		t.Errorf("an explicit live mountpoint must not be reported clean: %q", out.String())
	}
}

// TestUmount_DefaultRootLayoutUnchanged is the other half of the same contract:
// an agent mounted under the ordinary default root still resolves, and the live
// table agreeing with the roots search must not produce a second, spurious
// candidate (which the ambiguity guard would turn into a refusal).
func TestUmount_DefaultRootLayoutUnchanged(t *testing.T) {
	agent := "f0901fd3"
	root := isolateMountRoots(t)
	namespaced := filepath.Join(root, "bunker-las-03", agent)
	if err := os.MkdirAll(namespaced, 0o700); err != nil {
		t.Fatal(err)
	}
	preferFusermount3(t)
	defer stubMountTable(t,
		"bunker-las-03:/home/bunker-"+agent+" "+namespaced+" fuse.sshfs rw 0 0",
	)()
	calls, restoreRun := stubCommandRunner(t)
	defer restoreRun()

	var out bytes.Buffer
	if err := runUmount(&out, agent, true); err != nil {
		t.Fatalf("umount of a default-root mount must succeed, got: %v", err)
	}
	assertUnmountedAt(t, calls, namespaced)
	if strings.Contains(out.String(), "from the live mount table") {
		t.Errorf("the roots search already found this path; it must not be reported as resolved from the table: %q", out.String())
	}
}

// --- the twice-safe contract ------------------------------------------------

// TestUmount_NothingMountedAnywhereIsStillSuccess pins the idempotence the help
// text promises: cleanup after a crash must stay safe to run twice, so an agent
// with no mount anywhere is SUCCESS — and the message says which source was
// consulted, because an unqualified "already clean" over a live mount was the
// bug.
func TestUmount_NothingMountedAnywhereIsStillSuccess(t *testing.T) {
	agent := "f0901fd3"
	isolateMountRoots(t)
	preferFusermount3(t)
	defer stubMountTable(t,
		"tmpfs /run tmpfs rw 0 0",
		"/dev/nvme0n1p2 / ext4 rw,relatime 0 0",
	)()
	calls, restoreRun := stubCommandRunner(t)
	defer restoreRun()

	var out bytes.Buffer
	if err := runUmount(&out, agent, true); err != nil {
		t.Fatalf("umount with nothing mounted must succeed, got: %v", err)
	}
	assertNoUnmountExecuted(t, calls)
	got := out.String()
	if !strings.Contains(got, "Nothing mounted for "+agent) {
		t.Errorf("output should report that nothing is mounted anywhere, got %q", got)
	}
	if !strings.Contains(got, procSelfMountsPath) {
		t.Errorf("output should name the live mount table it consulted, got %q", got)
	}
}

// TestUmount_EmptyMountTableIsStillClean covers the second guard the brief asks
// for: with a genuinely EMPTY table and a leftover mountpoint directory under
// the default root, the command must keep saying "already clean" and succeed
// (the directory is even swept), rather than treating "no live mount" as a
// failure or as a reason to run an unmount.
func TestUmount_EmptyMountTableIsStillClean(t *testing.T) {
	agent := "f0901fd3"
	preferFusermount3(t)

	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{name: "no lines at all", lines: []string{}},
		{name: "one blank line", lines: []string{""}},
		{name: "only unrelated mounts", lines: []string{"/dev/nvme0n1p2 / ext4 rw 0 0"}},
		{name: "a truncated line", lines: []string{"garbage"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A per-case root: the call under test removes the leftover
			// directory, so a shared one would make the later subtests vacuous.
			root := isolateMountRoots(t)
			leftover := filepath.Join(root, agent)
			if err := os.MkdirAll(leftover, 0o700); err != nil {
				t.Fatal(err)
			}

			defer stubMountTable(t, tc.lines...)()
			calls, restoreRun := stubCommandRunner(t)
			defer restoreRun()

			var out bytes.Buffer
			if err := runUmount(&out, agent, true); err != nil {
				t.Fatalf("umount must stay safe to run twice, got: %v", err)
			}
			assertNoUnmountExecuted(t, calls)
			if !strings.Contains(out.String(), "already clean") {
				t.Errorf("expected the already-clean message, got %q", out.String())
			}
			// The leftover directory is swept so the next mount starts clean.
			if _, err := os.Stat(leftover); !os.IsNotExist(err) {
				t.Errorf("leftover mountpoint directory %s was not cleared (stat err: %v)", leftover, err)
			}
		})
	}
}

// --- fail-closed ------------------------------------------------------------

// TestUmount_UnreadableMountTableDoesNotClaimMoreThanItChecked is the
// fail-closed disclosure: with an unreadable table the command may still report
// clean over the roots it could see, but it must not present that as a settled
// fact about a custom mountpoint it was never able to look for.
func TestUmount_UnreadableMountTableDoesNotClaimMoreThanItChecked(t *testing.T) {
	agent := "f0901fd3"
	root := isolateMountRoots(t)
	if err := os.MkdirAll(filepath.Join(root, agent), 0o700); err != nil {
		t.Fatal(err)
	}
	preferFusermount3(t)
	defer stubBrokenMountTable(t, os.ErrPermission)()
	calls, restoreRun := stubCommandRunner(t)
	defer restoreRun()

	var out bytes.Buffer
	if err := runUmount(&out, agent, true); err != nil {
		t.Fatalf("umount: %v", err)
	}
	assertNoUnmountExecuted(t, calls)
	got := out.String()
	if !strings.Contains(got, "already clean") {
		t.Errorf("expected the clean message, got %q", got)
	}
	if !strings.Contains(got, "mount table unreadable") {
		t.Errorf("an unverified clean claim must disclose that the mount table was unreadable, got %q", got)
	}
}

// TestUmount_UnreadableMountTableStillUnmountsARealMountpoint: when the live
// table cannot be read, the explicit path the operator named is still acted on
// if it really is a mount — the fallback is the filesystem check, not a refusal
// and not a guess.
//
// The path is a REAL mount boundary on this host (/proc, /sys, /dev are
// separate filesystems everywhere Linux runs this), but the unmount call itself
// is stubbed, so nothing is detached: it exists to assert WHICH path the
// resolution chose when the live table is unavailable.
func TestUmount_UnreadableMountTableStillUnmountsARealMountpoint(t *testing.T) {
	preferFusermount3(t)
	defer stubBrokenMountTable(t, os.ErrPermission)()
	calls, restoreRun := stubCommandRunner(t)
	defer restoreRun()

	mountpoint := ""
	for _, dir := range []string{"/proc", "/sys", "/dev"} {
		mounted, err := isMountPoint(dir)
		if err == nil && mounted {
			mountpoint = dir
			break
		}
	}
	if mountpoint == "" {
		t.Skip("no mount boundary (/proc, /sys, /dev) detected on this host")
	}

	var out bytes.Buffer
	if err := runUmount(&out, mountpoint, true); err != nil {
		t.Fatalf("with an unreadable table a real mountpoint must still be unmounted, got: %v", err)
	}
	assertUnmountedAt(t, calls, mountpoint)
	if strings.Contains(out.String(), "already clean") {
		t.Errorf("a real mountpoint must not be reported clean: %q", out.String())
	}
}

// TestUmount_AmbiguousLiveMountpointsAreRefused: a cleanup command that picks
// one of two live mounts guesses with the operator's data. It must report.
func TestUmount_AmbiguousLiveMountpointsAreRefused(t *testing.T) {
	agent := "f0901fd3"
	isolateMountRoots(t)
	preferFusermount3(t)
	defer stubMountTable(t,
		"one:/home/bunker-"+agent+" /mnt/bunker/f0901fd3 fuse.sshfs rw 0 0",
		"two:/home/bunker-"+agent+" /mnt/other/f0901fd3 fuse.sshfs rw 0 0",
	)()
	calls, restoreRun := stubCommandRunner(t)
	defer restoreRun()

	var out bytes.Buffer
	err := runUmount(&out, agent, true)
	if err == nil {
		t.Fatalf("two live mounts for one agent must be reported, not silently resolved (output: %q)", out.String())
	}
	if !strings.Contains(err.Error(), "/mnt/bunker/f0901fd3") || !strings.Contains(err.Error(), "/mnt/other/f0901fd3") {
		t.Errorf("the refusal must name both paths, got: %v", err)
	}
	assertNoUnmountExecuted(t, calls)
}

// TestUmount_AgentIDSubstringIsNotAMountpoint is the trap the row warns about:
// the agent id is a substring of its own home directory. A plain (non-FUSE)
// mount at /home/bunker-<agent> must never be mistaken for the agent's mount —
// unmounting the home directory would be the data-loss case.
func TestUmount_AgentIDSubstringIsNotAMountpoint(t *testing.T) {
	agent := "f0901fd3"
	isolateMountRoots(t)
	preferFusermount3(t)
	defer stubMountTable(t,
		"overlay /home/bunker-"+agent+" overlay rw,lowerdir=/x 0 0",
		"/dev/nvme0n1p2 / ext4 rw,relatime 0 0",
	)()
	calls, restoreRun := stubCommandRunner(t)
	defer restoreRun()

	var out bytes.Buffer
	if err := runUmount(&out, agent, true); err != nil {
		t.Fatalf("umount: %v", err)
	}
	assertNoUnmountExecuted(t, calls)
	if !strings.Contains(out.String(), "Nothing mounted for "+agent) {
		t.Errorf("a non-bunker mount containing the id is not the agent's mount, got %q", out.String())
	}
}

// --- matching rules (the part that decides which path gets unmounted) -------

func TestAgentMountpoints_Matching(t *testing.T) {
	const agent = "f0901fd3"
	const namespaced = "/mnt/bunker/bunker-las-03/f0901fd3"

	cases := []struct {
		name  string
		line  string
		want  string
		match bool
	}{
		{
			name:  "custom absolute mountpoint",
			line:  "host:/home/bunker-" + agent + " /mnt/bunker/" + agent + " fuse.sshfs rw 0 0",
			want:  "/mnt/bunker/" + agent,
			match: true,
		},
		{
			name:  "namespaced layout",
			line:  "host:/home/bunker-" + agent + " " + namespaced + " fuse.sshfs rw 0 0",
			want:  namespaced,
			match: true,
		},
		{
			name:  "space in the path (octal-escaped by mount)",
			line:  `host:/home/bunker-` + agent + ` /mnt/bunker/my\040mount/` + agent + ` fuse.sshfs rw 0 0`,
			want:  "/mnt/bunker/my mount/" + agent,
			match: true,
		},
		{
			name:  "agent home is not the mountpoint",
			line:  "overlay /home/bunker-" + agent + " overlay rw 0 0",
			want:  "",
			match: false,
		},
		{
			name:  "id as a path prefix is not the mountpoint",
			line:  "host:/x /mnt/bunker/" + agent + "-backup fuse.sshfs rw 0 0",
			want:  "",
			match: false,
		},
		{
			name:  "another agent's mount",
			line:  "host:/home/bunker-aaaa /mnt/bunker/aaaa fuse.sshfs rw 0 0",
			want:  "",
			match: false,
		},
		{
			name:  "unrelated gvfs mount without the id",
			line:  "gvfsd-fuse /run/user/1000/gvfs fuse.gvfsd-fuse rw 0 0",
			want:  "",
			match: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer stubMountTable(t, tc.line)()
			points, checked, err := agentMountpoints(agent)
			if err != nil {
				t.Fatalf("agentMountpoints: %v", err)
			}
			if !checked {
				t.Fatal("a readable table must be reported as checked")
			}
			if !tc.match {
				if len(points) != 0 {
					t.Fatalf("matched %v, want no match", points)
				}
				return
			}
			if len(points) != 1 || points[0] != tc.want {
				t.Fatalf("matched %v, want exactly [%s]", points, tc.want)
			}
		})
	}
}

// TestAgentMountpoints_UnreadableTableIsNotAnEmptyTable is the distinction the
// whole fail-closed design rests on: "I could not look" must not read as
// "nothing is there".
func TestAgentMountpoints_UnreadableTableIsNotAnEmptyTable(t *testing.T) {
	defer stubBrokenMountTable(t, os.ErrPermission)()
	points, checked, err := agentMountpoints("f0901fd3")
	if err == nil && checked {
		t.Fatalf("an unreadable table reported as checked (points=%v) — that is how a live mount gets called clean", points)
	}
	if len(points) != 0 {
		t.Fatalf("an unreadable table produced candidate mounts: %v", points)
	}
}

// TestAgentMountpoints_AgentIDMustBeUsable: an id with no usable characters
// cannot match a mountpoint, and saying nothing is mounted would be an
// unverifiable claim — the caller has to be told.
func TestAgentMountpoints_AgentIDMustBeUsable(t *testing.T) {
	defer stubMountTable(t, "/dev/nvme0n1p2 / ext4 rw 0 0")()
	if _, checked, err := agentMountpoints("../.."); err == nil {
		t.Fatalf("an unusable agent id must be reported (checked=%v)", checked)
	}
}

func TestMountTargetHasAgent(t *testing.T) {
	cases := []struct {
		path  string
		agent string
		want  bool
	}{
		{"/mnt/bunker/f0901fd3", "f0901fd3", true},
		{"/mnt/bunker/bunker-las-03/f0901fd3", "f0901fd3", true},
		{"/mnt/bunker/f0901fd3/", "f0901fd3", true},
		{"/home/bunker-f0901fd3", "f0901fd3", false},
		{"/mnt/bunker/f0901fd3-backup", "f0901fd3", false},
		{"/mnt/bunker/f0901fd3extra", "f0901fd3", false},
		{"/mnt/bunker/other/f0901fd3x/f0901fd3", "f0901fd3", true},
		{"/mnt/bunker/", "f0901fd3", false},
		{"/mnt/bunker/f0901fd3", "", false},
	}
	for _, tc := range cases {
		if got := mountTargetHasAgent(tc.path, tc.agent); got != tc.want {
			t.Errorf("mountTargetHasAgent(%q, %q) = %v, want %v", tc.path, tc.agent, got, tc.want)
		}
	}
}

func TestIsFuseOrNetworkFS(t *testing.T) {
	for _, want := range []string{"fuse.sshfs", "fuse", "fuse.gvfsd-fuse", "fuseblk", "nfs4", "sshfs"} {
		if !isFuseOrNetworkFS(want) {
			t.Errorf("%s should be treated as a possible bunker mount transport", want)
		}
	}
	for _, not := range []string{"ext4", "overlay", "tmpfs", "xfs"} {
		if isFuseOrNetworkFS(not) {
			t.Errorf("%s is not a FUSE/network transport and must not be a bunker-mount candidate", not)
		}
	}
}
