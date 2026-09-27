//go:build !unix

// The refusal arm of the `bunker umount` platform seam (BFS-028).
//
// THE DECISION, and why it is this one. `bunker umount` exists to tear down a
// mount `bunker mount` created, and every mechanism it is made of is a unix
// mount interface:
//
//   - it FINDS the mount through the kernel mount table (/proc/self/mounts, or
//     mount(8) where /proc is absent);
//   - it DETACHES it with fusermount3, falling back to umount(8);
//   - it decides whether a path is a mountpoint by comparing device ids.
//
// On a platform with none of those, the ACCIDENTAL behaviour is the dangerous
// one: every lookup comes back empty, so the command would print "Nothing
// mounted for <agent> (mount table unreadable; checked the default mount roots
// only)" and return SUCCESS — a cleanup verb reporting success over a mount it
// cannot even see. That is the DF-BUNKER-50 false positive, one platform over.
//
// So this arm REFUSES, by name, before any resolution and before any local side
// effect (the gate is the first statement of runUmount). Not a compile error
// (the command stays in the build and in `bunker --help`), not a silent no-op,
// and not a fabricated "not a mountpoint".
//
// WHY NOT A REAL IMPLEMENTATION. A truthful one needs a mount implementation on
// this platform to be the counterpart of, and there is none: the opt-in
// bunker-fs driver refuses off Linux (internal/fsmount, ErrPlatformUnsupported;
// the Windows-mount decision is recorded in
// docs/evidence/BFS-010-windows-mint-decision.md and "implements nothing"), and
// the default sshfs driver's stored invocation is a POSIX one. A Win32 probe
// (volume serial numbers via GetFileInformationByHandle, a drive-letter table)
// could answer "is this a mountpoint", but "what should `bunker umount
// <agent-id>` DO with that answer" has no non-unix answer to give, so the probe
// would only make a command that cannot act look like one that can. Refusal
// now, implementation when the mount side has a binding, is the honest order.
//
// THE WAY OUT IS IN THE SENTENCE, deliberately (this repo's standard: a refusal
// names the command, the platform, and what to do instead). Nothing classifies
// this text — unlike the mount side, whose phrase internal/mountdriver/
// bunkerfs.go keys on, no classifier ever reads an unmount error (there is no
// umount retry loop) — so the words are free to be about the operator's
// situation rather than about a state machine.
package cli

import (
	"fmt"
	"runtime"
)

// platformUmountRefusal returns the named refusal for a platform this build has
// no unmount mechanism for.
//
// ONE function serves BOTH the gate in runUmount and isMountPoint below, so the
// command's refusal and the probe's refusal cannot drift into two sentences
// that say different things.
func platformUmountRefusal() error {
	return fmt.Errorf("%w — this build is %s/%s, and `bunker umount` detaches a mount with the platform's own unmount "+
		"(fusermount3/umount(8), and the kernel mount table to find it), which do not exist here. Nothing was unmounted. "+
		"Run `bunker umount` from a Linux client, where bunker's mounts live; to detach a path some other tool mounted "+
		"on this platform, use that tool's own unmount (on Windows a mapped or SSHFS-Win drive comes off with "+
		"`net use <drive>: /delete`, or from the WinFsp/SSHFS-Win tray)",
		ErrUmountUnsupported, runtime.GOOS, runtime.GOARCH)
}

// isMountPoint refuses on this platform instead of answering.
//
// A (false, nil) here would be a FABRICATED "not a mountpoint" — the claim that
// lets a caller delete a mountpoint directory and report the path clean. The
// caller must read the error.
func isMountPoint(path string) (bool, error) {
	return false, platformUmountRefusal()
}
