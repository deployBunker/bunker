package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// recorder is a fake host: it answers the probe commands the provisioners run
// and records every command they issue, so tests can assert BOTH the exact
// argv and that no host state was touched.
type recorder struct {
	t *testing.T

	// Simulated host state.
	groupExists  bool
	groupMembers map[string]bool // username -> already a member of the group
	mounts       map[string]bool // path -> is a mountpoint
	mountOpts    map[string]string
	failMount    bool // every `mount` invocation fails
	failUmount   bool
	tmpFSType    string
	tmpOptions   string

	// Scratch-root state: what `stat -c '%u:%g %a'` ANSWERS. It is per-path
	// because the exchange root and a per-agent directory are both stat-ed,
	// and a fake that answers every path identically cannot tell a correct
	// root from a wrong one — the whole point of the DF-BUNKER-40 gate.
	// Empty string means "no answer" (a non-zero exit from stat).
	scratchOwners map[string]string
	// scratchRootOwnerTo / scratchRootOctalTo carry out the host effect of a
	// chown/chmod on the exchange root so the stat-back after a repair sees
	// the repaired shape. A fake that records the command but leaves the
	// observation wrong makes every repair look like a failure.
	scratchRootOwnerTo string
	scratchRootOctalTo string
	// scratchRootRepairIneffective models a root whose owner and mode survive
	// the repair commands: the chown/chmod are accepted and the stat answer
	// does not change, so the observation can be asserted to disagree with the
	// argv (the reason the gate re-stats instead of trusting the command).
	scratchRootRepairIneffective bool
	// scratchRoot is the exchange root of this sandbox (Root + the documented
	// default), resolved once because Root is a per-recorder temp directory.
	scratchRoot string

	// root is a stable per-recorder sandbox directory: t.TempDir() returns a
	// NEW directory on every call, so the sandbox must be captured once.
	root string

	// Recorded commands, in order ("name arg1 arg2").
	calls []string
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	root := t.TempDir()
	return &recorder{
		t:             t,
		root:          root,
		scratchRoot:   filepath.Join(root, DefaultScratchRoot),
		groupMembers:  map[string]bool{},
		mounts:        map[string]bool{},
		mountOpts:     map[string]string{},
		scratchOwners: map[string]string{},
		tmpFSType:     "tmpfs",
		tmpOptions:    "rw,relatime,size=1073741824",
	}
}

// exitError builds the error shape a command that ran and exited non-zero
// produces (an *exec.ExitError), which the probes treat as a "no" answer.
func exitError(t *testing.T) error {
	t.Helper()
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("prime exit error: %v", err)
	}
	return &exec.ExitError{ProcessState: cmd.ProcessState}
}

func (r *recorder) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	switch name {
	case "getent":
		if len(args) >= 2 && args[0] == "group" {
			if r.groupExists {
				return []byte(args[1] + ":x:1001:"), nil
			}
			return nil, exitError(r.t)
		}
	case "groupadd":
		r.groupExists = true
		return nil, nil
	case "id":
		user := args[len(args)-1]
		if r.groupMembers[user] {
			// id -nG prints the supplementary groups INCLUDING the scratch
			// group, which is what the provisioner looks for.
			return []byte("users " + DefaultScratchGroup), nil
		}
		return []byte("users"), nil
	case "usermod":
		r.groupMembers[args[len(args)-1]] = true
		return nil, nil
	case "mountpoint":
		if r.mounts[args[len(args)-1]] {
			return nil, nil
		}
		return nil, exitError(r.t)
	case "findmnt":
		target := args[len(args)-1]
		if target == "/tmp" {
			return []byte(r.tmpFSType + " " + r.tmpOptions), nil
		}
		return []byte(r.mountOpts[target]), nil
	case "mount":
		if r.failMount {
			return []byte("mount: permission denied"), errors.New("exit status 32")
		}
		// Remember the mount so a second Ensure call sees it as provisioned,
		// and make a remount actually change the live options the way the
		// kernel would — otherwise a second Ensure looks like it must remount
		// forever.
		dir := args[len(args)-1]
		opts := mountOptionsFromArgs(args)
		if strings.Contains(opts, "remount") {
			if dir == "/tmp" {
				r.tmpOptions = MergeTmpMountOptions(r.tmpOptions, sizeFromMountOptions(opts))
			} else {
				r.mountOpts[dir] = MergeTmpMountOptions(r.mountOpts[dir], sizeFromMountOptions(opts))
			}
			return nil, nil
		}
		r.mounts[dir] = true
		r.mountOpts[dir] = opts
		return nil, nil
	case "umount":
		if r.failUmount {
			return nil, errors.New("exit status 32")
		}
		dir := args[len(args)-1]
		delete(r.mounts, dir)
		delete(r.mountOpts, dir)
		return nil, nil
	case "stat":
		// Per-path, and empty when the fake host has no answer: a fake that
		// returned one hard-coded root shape for every path could not
		// distinguish a correct exchange root from the measured defect.
		return []byte(r.scratchOwners[args[len(args)-1]]), nil
	case "systemctl":
		return []byte(""), nil
	case "chown":
		if len(args) == 2 && args[1] == r.scratchRoot {
			r.scratchRootOwnerTo = args[0]
			r.syncScratchRootShape()
		}
		return nil, nil
	case "chmod":
		if len(args) == 2 && args[1] == r.scratchRoot {
			r.scratchRootOctalTo = strings.TrimLeft(args[0], "0")
			r.syncScratchRootShape()
		}
		return nil, nil
	default: // ssh-keygen, …
		return nil, nil
	}
	return nil, nil
}

// syncScratchRootShape composes the `stat` answer for the exchange root from
// the ownership and mode the fake host has been given or has been told to
// apply, so the stat-back after a repair observes the repaired shape rather
// than the pre-repair one.
func (r *recorder) syncScratchRootShape() {
	if r.scratchRootRepairIneffective {
		// The host accepts the chown/chmod command and keeps reporting the
		// old shape — a filesystem that ignores the change (an NFS/SMB mount
		// with root_squash, or a sandbox that drops the call). This is the
		// case where "chmod exited 0" must not be read as "the root is now
		// correct", so the observation deliberately disagrees with the argv.
		return
	}
	owner := r.scratchRootOwnerTo
	if owner == "" {
		owner = "0:" + DefaultScratchGroup
	}
	octal := r.scratchRootOctalTo
	if octal == "" {
		octal = strings.TrimLeft(fmt.Sprintf("%04o", ScratchRootMode), "0")
	}
	r.scratchOwners[r.scratchRoot] = owner + " " + octal
}

// scratchRootOwner is the simulated `stat` answer for the exchange root.
func (r *recorder) scratchRootOwner() string { return r.scratchOwners[r.scratchRoot] }

// setScratchRootShape makes the fake host report the exchange root as
// owner:group mode-octal, exactly as `stat -c '%u:%g %a'` would, AND creates
// the directory on disk with the mode that octal describes — the mode half of
// the boundary is read from the real directory (verifyScratchRoot), so a
// fixture that only edited the `stat` answer would not model a real host. An
// empty owner makes the fake answer "no owner observable" (unprivileged stat).
func (r *recorder) setScratchRootShape(owner, group, octal string) {
	perm, setgid := diskModeFromOctal(octal)
	if err := os.MkdirAll(r.scratchRoot, perm); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Chmod(r.scratchRoot, setgid|perm); err != nil {
		r.t.Fatal(err)
	}
	if owner == "" {
		delete(r.scratchOwners, r.scratchRoot)
		return
	}
	r.scratchOwners[r.scratchRoot] = owner + ":" + group + " " + octal
}

// diskModeFromOctal translates an octal mode as reported by `stat -c '%a'`
// into the permission bits and setgid flag os.Chmod needs. The setgid bit is
// the leading digit of the octal spelling, which os.FileMode cannot express
// directly (ModeSetgid is not 0o2000) — the same trap the provisioner's
// in-process chmod documents.
func diskModeFromOctal(octal string) (perm os.FileMode, setgid os.FileMode) {
	perm = 0o750
	if octal == "" {
		return perm, 0
	}
	if len(octal) >= 3 {
		if v, err := strconv.ParseUint(octal[len(octal)-3:], 8, 32); err == nil {
			perm = os.FileMode(v)
		}
	}
	if len(octal) >= 4 && octal[len(octal)-4] == '2' {
		setgid = os.ModeSetgid
	}
	return perm, setgid
}

// mutating returns the recorded commands that change host state (probes such
// as findmnt/mountpoint/getent are excluded).
func (r *recorder) mutating() []string {
	var out []string
	for _, c := range r.calls {
		switch {
		case strings.HasPrefix(c, "findmnt"), strings.HasPrefix(c, "mountpoint"),
			strings.HasPrefix(c, "getent"), strings.HasPrefix(c, "id "),
			strings.HasPrefix(c, "stat "):
			continue
		}
		out = append(out, c)
	}
	return out
}

// sizeFromMountOptions extracts the numeric size= value from a mount option
// string (0 when absent).
func sizeFromMountOptions(options string) uint64 {
	for _, f := range strings.Split(options, ",") {
		if strings.HasPrefix(f, "size=") {
			var v uint64
			fmt.Sscanf(strings.TrimPrefix(f, "size="), "%d", &v)
			return v
		}
	}
	return 0
}

// mountOptionsFromArgs extracts the -o value from a recorded mount argv.
func mountOptionsFromArgs(args []string) string {
	for i, a := range args {
		if a == "-o" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// options returns Options wired to this recorder with a redirected root.
// Defaults are applied (the same call production makes) so the paths a test
// derives from the returned Options are the paths the provisioners will use.
func (r *recorder) options(configure func(*Options)) Options {
	o := Options{Runner: r.run, Root: r.root, ScratchEnabled: true}
	if configure != nil {
		configure(&o)
	}
	return o.WithDefaults()
}

// ran reports whether a command matching the exact string was issued.
func (r *recorder) ran(command string) bool {
	for _, c := range r.calls {
		if c == command {
			return true
		}
	}
	return false
}

// ranPrefix reports whether any command starts with prefix.
func (r *recorder) ranPrefix(prefix string) bool {
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// allArgsContains reports whether any issued command's full text contains s.
func (r *recorder) argumentsContain(s string) bool {
	for _, c := range r.calls {
		if strings.Contains(c, s) {
			return true
		}
	}
	return false
}

func (r *recorder) String() string {
	return fmt.Sprintf("%d commands: %v", len(r.calls), r.calls)
}
