//go:build unix

package cli

// DF-BUNKER-91 regression tests: `bunker umount <agent-id>` must clear a mount
// whose MOUNTPOINT carries no agent-id component at all — a mount the operator
// placed at a path of their own choosing.
//
// The repro this closes: with the mount live at, say,
// /home/kara/.hermes/.../custom-mnt, `bunker umount f0901fd3` printed "already
// clean" (rc 0) while the mount stayed in /proc/self/mounts. The id-form lookup
// matched only mountpoints carrying the id as a path component
// (mountTargetHasAgent), so a custom path was invisible to it.
//
// The kernel's own record of the mount still carries the identity: an sshfs
// entry's DEVICE field is the sshfs source `<sshUser>@<host>:<remotePath>` and
// the ssh user is `bunker-<id>`. resolveAgentMounts therefore falls back to that
// field, and because a device string is an INFERENCE rather than a path the
// operator named, the detach is confirmed against the live table before success
// is reported — a mount still live afterwards is a non-zero exit, never a clean
// report.
//
// No root and no live host: the mount table is injected through the readMountTable
// seam and the unmount is recorded through the execCommand seam, so every cell
// asserts the EXACT path the resolution chose and what the command reported.

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// --- harness ----------------------------------------------------------------

// mountTableScript is a live mount table a test can REWRITE mid-run, so the table
// the confirmation step reads can differ from the table the resolution step read.
// A static injection cannot express the thing the confirmation exists to check
// ("was this entry actually detached?"), and it cannot express "the table became
// unreadable". Both are cells here.
type mountTableScript struct {
	lines []string
	err   error
}

// stubMountTableScript injects a script the test mutates through its own pointer.
func stubMountTableScript(t *testing.T, s *mountTableScript) {
	t.Helper()
	old := readMountTable
	readMountTable = func() ([]string, error) { return s.lines, s.err }
	t.Cleanup(func() { readMountTable = old })
}

// stubCommandRunnerOnRun is stubCommandRunner with a hook: it records the argv
// the unmount would have run and then lets the test make the WORLD react to it,
// the way a real detach removes the entry it detached. The injected argv is
// still served by `true` (exit 0), so nothing is ever executed against a path the
// fixture only names.
func stubCommandRunnerOnRun(t *testing.T, onRun func()) (calls *[][]string) {
	t.Helper()
	calls = &[][]string{}
	old := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd {
		*calls = append(*calls, append([]string{name}, arg...))
		if onRun != nil {
			onRun()
		}
		return exec.Command("true")
	}
	t.Cleanup(func() { execCommand = old })
	return calls
}

// sshfsEntry builds the mount-table line an agent's sshfs mount produces: the
// device is the sshfs source, whose user carries the identity the row relies on.
func sshfsEntry(agent, server, mountPoint string) string {
	return "bunker-" + agent + "@" + server + ":/home/bunker-" + agent + " " + mountPoint + " fuse.sshfs rw,nosuid,nodev,relatime,user_id=1000 0 0"
}

// tableShowsMount reports whether the (possibly rewritten) scripted table still
// lists a mount at path — the assertion behind criterion 1's "verify gone from
// /proc/mounts".
func tableShowsMount(s *mountTableScript, path string) bool {
	for _, line := range s.lines {
		if strings.Contains(line, " "+path+" ") {
			return true
		}
	}
	return false
}

// resolveWithTable drives the resolver against a scripted table and returns the
// resolution it produced.
func resolveWithTable(t *testing.T, agent string, lines ...string) []agentMount {
	t.Helper()
	stubMountTableScript(t, &mountTableScript{lines: lines})
	mounts, checked, err := resolveAgentMounts(agent)
	if err != nil {
		t.Fatalf("resolveAgentMounts: %v", err)
	}
	if !checked {
		t.Fatal("a readable table must be reported as checked")
	}
	return mounts
}

// --- the row's repro (acceptance criterion 1) -------------------------------

// TestUmount_CustomPathMountIsClearedByDeviceIdentity is the row's repro: the
// agent is mounted at a path that carries no agent-id component anywhere, and
// `umount <agent-id>` must find it by the identity in the device field, unmount
// THAT path, and leave it gone from the live table.
func TestUmount_CustomPathMountIsClearedByDeviceIdentity(t *testing.T) {
	const (
		agent  = "f0901fd3"
		server = "bunker-las-03"
		custom = "/home/kara/.hermes/df-bunker-91/custom-mnt"
	)
	isolateMountRoots(t)
	preferFusermount3(t)

	script := &mountTableScript{lines: []string{
		"tmpfs /run tmpfs rw,nosuid,nodev,size=12509376k 0 0",
		// The repro's mount: an sshfs mount at an operator-chosen path, so no
		// path component is the agent id and only the DEVICE names the agent.
		sshfsEntry(agent, server, custom),
		"/dev/nvme0n1p2 / ext4 rw,relatime 0 0",
	}}
	stubMountTableScript(t, script)

	// A real unmount removes the entry it detached; the table the confirmation
	// reads is therefore the POST-detach table.
	calls := stubCommandRunnerOnRun(t, func() {
		script.lines = []string{"tmpfs /run tmpfs rw,nosuid,nodev 0 0"}
	})

	var out bytes.Buffer
	if err := runUmount(&out, agent, true); err != nil {
		t.Fatalf("umount of a custom-path mount by agent id must succeed, got: %v (output %q)", err, out.String())
	}

	// The exact path the resolution chose is the thing that was wrong before.
	assertUnmountedAt(t, calls, custom)

	got := out.String()
	if strings.Contains(got, "already clean") {
		t.Errorf("claimed the mount was already clean while it was live at %s: %q", custom, got)
	}
	if strings.Contains(got, "Nothing mounted") {
		t.Errorf("claimed nothing is mounted while it was live at %s: %q", custom, got)
	}
	if !strings.Contains(got, "Unmounted "+custom) {
		t.Errorf("output should name the unmounted path, got %q", got)
	}
	if !strings.Contains(got, "by sshfs device identity") {
		t.Errorf("output should say the identity came from the sshfs device field, not from a path component, got %q", got)
	}

	// Criterion 1's verification: gone from the live table.
	if tableShowsMount(script, custom) {
		t.Errorf("%s is still in the live mount table after the unmount: %v", custom, script.lines)
	}
}

// TestUmount_MountCarryingBothSignalsIsCountedOnce pins the dedupe. A mount at
// the default layout carries the agent id BOTH as a path component and in the
// device field; counting it twice would look like two live mounts and turn a
// healthy cleanup into the "mounted at more than one path" refusal.
func TestUmount_MountCarryingBothSignalsIsCountedOnce(t *testing.T) {
	const (
		agent  = "f0901fd3"
		server = "bunker-las-03"
		point  = "/mnt/bunker/f0901fd3"
	)
	isolateMountRoots(t)
	preferFusermount3(t)
	stubMountTableScript(t, &mountTableScript{lines: []string{sshfsEntry(agent, server, point)}})
	calls := stubCommandRunnerOnRun(t, nil)

	var out bytes.Buffer
	if err := runUmount(&out, agent, true); err != nil {
		t.Fatalf("one mount carrying both signals must resolve to ONE mount, got: %v (output %q)", err, out.String())
	}
	assertUnmountedAt(t, calls, point)
	if strings.Contains(out.String(), "more than one path") {
		t.Errorf("a single mount found by both rules must not be reported as two: %q", out.String())
	}
}

// --- never report success over a live mount (criterion 2) -------------------

// TestUmount_CustomMountLeftBehindExitsNonZero is criterion 2 for the custom-path
// case: the unmount command returns success, but the mount is STILL in the live
// table afterwards. Reporting "Unmounted <path>" there would be the same class of
// false positive as the "already clean" this row is about, so the command must
// exit non-zero and name the path.
func TestUmount_CustomMountLeftBehindExitsNonZero(t *testing.T) {
	const (
		agent  = "f0901fd3"
		server = "bunker-las-03"
		custom = "/home/kara/.hermes/df-bunker-91/custom-mnt"
	)
	isolateMountRoots(t)
	preferFusermount3(t)
	// The table does NOT change: the detach silently failed on the host.
	stubMountTableScript(t, &mountTableScript{lines: []string{sshfsEntry(agent, server, custom)}})
	calls := stubCommandRunnerOnRun(t, nil)

	var out bytes.Buffer
	err := runUmount(&out, agent, true)
	if err == nil {
		t.Fatalf("a custom mount still live after the unmount must not be reported as success; output was %q", out.String())
	}
	if !strings.Contains(err.Error(), custom) {
		t.Errorf("the failure must name the mount it could not clear, got: %v", err)
	}
	if !strings.Contains(err.Error(), "STILL MOUNTED") {
		t.Errorf("the failure must say the mount is still live, got: %v", err)
	}
	if strings.Contains(out.String(), "Unmounted ") {
		t.Errorf("a still-live mount must not be announced as unmounted: %q", out.String())
	}
	// It did make the attempt, exactly once — the failure is the confirmation,
	// not a missing unmount.
	assertUnmountedAt(t, calls, custom)
}

// TestUmount_UnconfirmableDetachIsNotReportedAsSuccess is the fail-closed arm of
// the same criterion: if the live table cannot be re-read after detaching a
// device-identified mount, the command cannot vouch for the result. "I could not
// look" must not be reported as "it is gone".
func TestUmount_UnconfirmableDetachIsNotReportedAsSuccess(t *testing.T) {
	const (
		agent  = "f0901fd3"
		server = "bunker-las-03"
		custom = "/home/kara/.hermes/df-bunker-91/custom-mnt"
	)
	isolateMountRoots(t)
	preferFusermount3(t)

	script := &mountTableScript{lines: []string{sshfsEntry(agent, server, custom)}}
	stubMountTableScript(t, script)
	// The re-read fails: exactly what an unreadable /proc/self/mounts (or a
	// host where the mount(8) fallback is gone) looks like after the detach.
	calls := stubCommandRunnerOnRun(t, func() { script.err = os.ErrPermission })

	var out bytes.Buffer
	err := runUmount(&out, agent, true)
	if err == nil {
		t.Fatalf("an unconfirmed detach must not be reported as success; output was %q", out.String())
	}
	if !strings.Contains(err.Error(), "could not be re-read") {
		t.Errorf("the failure must say the confirmation could not be read, got: %v", err)
	}
	if strings.Contains(out.String(), "Unmounted ") {
		t.Errorf("an unconfirmed detach must not be announced as unmounted: %q", out.String())
	}
	assertUnmountedAt(t, calls, custom)
}

// TestUmount_ComponentAndDeviceMountsForOneAgentAreRefused: with the device
// fallback in place, an agent can have TWO live mounts — the default-layout one
// and one at an operator-chosen path. Unmounting the first and exiting 0 would
// leave the second live while claiming success, which is the false positive this
// row is about; the existing ambiguity rule refuses instead and names both.
func TestUmount_ComponentAndDeviceMountsForOneAgentAreRefused(t *testing.T) {
	const (
		agent  = "f0901fd3"
		server = "bunker-las-03"
		point  = "/mnt/bunker/f0901fd3"
		custom = "/home/kara/.hermes/df-bunker-91/custom-mnt"
	)
	isolateMountRoots(t)
	preferFusermount3(t)
	stubMountTableScript(t, &mountTableScript{lines: []string{
		sshfsEntry(agent, server, point),
		sshfsEntry(agent, server, custom),
	}})
	calls := stubCommandRunnerOnRun(t, nil)

	var out bytes.Buffer
	err := runUmount(&out, agent, true)
	if err == nil {
		t.Fatalf("two live mounts for one agent must be reported, not silently half-cleaned (output: %q)", out.String())
	}
	if !strings.Contains(err.Error(), point) || !strings.Contains(err.Error(), custom) {
		t.Errorf("the refusal must name both paths, got: %v", err)
	}
	assertNoUnmountExecuted(t, calls)
}

// --- matching rules for the device fallback ---------------------------------

// TestResolveAgentMounts_DeviceIdentity covers the rule that decides which custom
// path may be unmounted. It is a fail-loud-adjacent surface: a too-loose match
// here means a wrong-path unmount, so every near-miss is a cell.
func TestResolveAgentMounts_DeviceIdentity(t *testing.T) {
	const (
		agent  = "f0901fd3"
		server = "bunker-las-03"
		custom = "/home/kara/.hermes/df-bunker-91/custom-mnt"
	)
	cases := []struct {
		name string
		line string
		want string
		// byDevice is the evidence expected for the single match, when there is one.
		byDevice bool
	}{
		{
			name:     "custom path, sshfs device names the agent",
			line:     sshfsEntry(agent, server, custom),
			want:     custom,
			byDevice: true,
		},
		{
			// sshfs has historically written the source prefixed this way; the
			// needle is a substring of the device precisely so both spellings work.
			name:     "custom path, sshfs# prefixed device",
			line:     "sshfs#bunker-" + agent + "@" + server + ":/home/bunker-" + agent + " " + custom + " fuse.sshfs rw 0 0",
			want:     custom,
			byDevice: true,
		},
		{
			// The default layout: the path component is the stronger, operator-facing
			// evidence and is what the report attributes the resolution to.
			name: "default layout with the same device",
			line: sshfsEntry(agent, server, "/mnt/bunker/"+agent),
			want: "/mnt/bunker/" + agent,
		},
		{
			name: "another agent's device",
			line: sshfsEntry("aaaa1111", server, custom),
		},
		{
			// The `@` terminator is the whole reason a prefix cannot match: without
			// it, agent f0901fd3 would claim f0901fd3-backup's mount.
			name: "device of an agent whose id extends this one",
			line: sshfsEntry(agent+"-backup", server, custom),
		},
		{
			name: "device of an agent whose id this one prefixes",
			line: sshfsEntry(agent+"0", server, custom),
		},
		{
			// Identity is not enough: a plain disk filesystem carrying an
			// sshfs-shaped device string is not a bunker mount (this is also what
			// keeps the agent's own /home/bunker-<id> out of the candidates).
			name: "sshfs-shaped device on a non-FUSE filesystem",
			line: "bunker-" + agent + "@" + server + ":/home/bunker-" + agent + " " + custom + " ext4 rw 0 0",
		},
		{
			name: "unrelated fuse mount without the identity",
			line: "gvfsd-fuse /run/user/1000/gvfs fuse.gvfsd-fuse rw,nosuid,nodev 0 0",
		},
		{
			name: "a mount that only shares the home-directory path",
			line: "overlay /home/bunker-" + agent + " overlay rw,lowerdir=/x 0 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mounts := resolveWithTable(t, agent, tc.line)
			if tc.want == "" {
				if len(mounts) != 0 {
					t.Fatalf("matched %v, want no match", mounts)
				}
				return
			}
			if len(mounts) != 1 || mounts[0].mountPoint != tc.want {
				t.Fatalf("matched %v, want exactly [%s]", mounts, tc.want)
			}
			if mounts[0].deviceOnly != tc.byDevice {
				t.Errorf("evidence for %s: deviceOnly=%v, want %v (the report and the confirmation step both depend on it)",
					tc.want, mounts[0].deviceOnly, tc.byDevice)
			}
		})
	}
}

// TestDeviceNamesAgent pins the needle itself, including the boundary that makes
// it safe.
func TestDeviceNamesAgent(t *testing.T) {
	const agent = "f0901fd3"
	cases := []struct {
		device string
		want   bool
	}{
		{"bunker-" + agent + "@bunker-las-03:/home/bunker-" + agent, true},
		{"sshfs#bunker-" + agent + "@host:/path", true},
		{"bunker-" + agent + "@host:/path with spaces", true},
		{"bunker-" + agent + "-backup@host:/path", false},
		{"bunker-" + agent + "0@host:/path", false},
		{"bunker-aaaa1111@host:/path", false},
		{"/home/bunker-" + agent, false},
		{"", false},
		{"tmpfs", false},
	}
	for _, tc := range cases {
		if got := deviceNamesAgent(tc.device, agent); got != tc.want {
			t.Errorf("deviceNamesAgent(%q, %q) = %v, want %v", tc.device, agent, got, tc.want)
		}
	}
	// An empty agent id can never match, and must not panic or match by prefix.
	if deviceNamesAgent("bunker-@host:/path", "") {
		t.Error("an empty agent id must not match a device string")
	}
}
