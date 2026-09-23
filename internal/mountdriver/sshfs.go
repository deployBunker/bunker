// Package mountdriver: the sshfs driver registration (MOUNT-006).
//
// sshfs is the DEFAULT driver. Its classifier is the exact logic the CLI's
// mount path used before the seam (classifySSHFSFailure): permanent output
// fragments first (retrying an auth failure only adds noise), then transient
// fragments (session-limit retries), then EOF/signal shapes. Moving it here
// changes nothing about which fragments match or which class wins — the
// mount path's behaviour for default requests is unchanged, and the CLI's
// sshfs classifier now delegates to this one.
package mountdriver

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
)

// sshfsPermanentFragments are lowercase output fragments that indicate a
// permanent sshfs failure. Retrying cannot help, so the mount command fails
// immediately on the first attempt without any session-limit hint.
// Unchanged from the pre-seam CLI constants.
var sshfsPermanentFragments = []string{
	"permission denied",
	"no such file or directory",
	"mountpoint is not empty",
	"fuse: device not found",
	"transport endpoint is not connected",
}

// sshfsTransientFragments are lowercase output fragments that indicate a
// transient connection failure worth retrying. These were observed against
// healthy agents whose host limits parallel SSH sessions (sshd MaxStartups):
// each sshfs attempt opens a fresh connection while tunnels stay alive.
// Unchanged from the pre-seam CLI constants.
var sshfsTransientFragments = []string{
	"connection reset by peer",
	"remote host has disconnected",
	"connection closed",
}

// classifySSHFS is the sshfs driver's failure classifier. Permanent causes
// take precedence: real ssh transcripts often contain both a permanent
// fragment and a transient one (e.g. "Permission denied ... Connection
// closed by host"), and retrying an auth failure would only add noise.
func classifySSHFS(output string, err error) (ClassifierClass, string) {
	lower := strings.ToLower(output)
	for _, frag := range sshfsPermanentFragments {
		if strings.Contains(lower, frag) {
			return ClassPermanent, frag
		}
	}
	for _, frag := range sshfsTransientFragments {
		if strings.Contains(lower, frag) {
			return ClassTransient, frag
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ClassTransient, "unexpected EOF"
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
			if ws, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return ClassTransient, fmt.Sprintf("killed by signal %s", ws.Signal())
			}
		}
	}
	return ClassUnknown, ""
}

// TransientFragmentsSSHFS exposes the sshfs transient fragments so the mount
// loop can keep gating the session-limit hint on the captured output (an
// evidenced hint, not an unconditional one) without importing the fragments
// directly.
func TransientFragmentsSSHFS() []string {
	out := make([]string, len(sshfsTransientFragments))
	copy(out, sshfsTransientFragments)
	return out
}

// ContainsAnyFragment reports whether s contains any of the (lowercase)
// fragments. Shared by the mount loop's evidenced session-limit hint.
func ContainsAnyFragment(s string, fragments []string) bool {
	lower := strings.ToLower(s)
	for _, frag := range fragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

func init() {
	Register(Driver{
		Name:     DefaultDriver,
		Classify: classifySSHFS,
	})
}
