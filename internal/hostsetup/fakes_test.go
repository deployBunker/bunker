package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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

	// root is a stable per-recorder sandbox directory: t.TempDir() returns a
	// NEW directory on every call, so the sandbox must be captured once.
	root string

	// Recorded commands, in order ("name arg1 arg2").
	calls []string
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	return &recorder{
		t:            t,
		root:         t.TempDir(),
		groupMembers: map[string]bool{},
		mounts:       map[string]bool{},
		mountOpts:    map[string]string{},
		tmpFSType:    "tmpfs",
		tmpOptions:   "rw,relatime,size=1073741824",
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
		return []byte("root:" + DefaultScratchGroup + " 2750"), nil
	case "systemctl":
		return []byte(""), nil
	default: // chown, chmod, ssh-keygen, …
		return nil, nil
	}
	return nil, nil
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
