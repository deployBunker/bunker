//go:build unix

// The POSIX arm of the `bunker umount` platform seam (BFS-028).
//
// WHY THERE IS A SEAM AT ALL. `bunker umount` detached a mount with
// fusermount3/umount(8) and decided whether a path was a mountpoint by
// comparing its DEVICE ID with its parent's (syscall.Stat). Neither that
// syscall nor those two binaries exist on Windows, so the single file that held
// them did not compile there — and a cross-platform build is a supported
// promise of this repo: probes/cross-GOOS-build.sh builds `./...` for
// linux/amd64, linux/arm64, windows/amd64 and windows/arm64. That file was the
// one site holding the Windows target hostage, which is the row.
//
// WHAT LIVES HERE: the unix answers. platformUmountRefusal returns nil ("this
// build can unmount") and isMountPoint is the real device-id probe, moved
// verbatim from umount.go so its behaviour is unchanged.
//
// The refusal arm is umount_nonunix.go. The seam's middle — the named sentinel,
// the replaceable var, and the gate inside runUmount — is umount.go, so it is
// compiled AND testable on every platform.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// platformUmountRefusal returns nil: this build has the mechanisms `bunker
// umount` is made of (the kernel mount table, fusermount3/umount(8)), so the
// command proceeds exactly as it always has.
func platformUmountRefusal() error { return nil }

// isMountPoint reports whether path currently has a filesystem mounted on it.
// It compares the device ids of the path and its parent: a mount changes the
// device, so differing ids mean something is mounted there. This is used
// instead of parsing /proc/mounts because it is a single stat and needs no
// subprocess.
//
// A device id is a unix concept, so this probe is unix-only by nature; the
// platform that has nothing to compare refuses in umount_nonunix.go rather than
// answering "not a mountpoint".
func isMountPoint(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("mountpoint %s is a symlink", path)
	}
	var st, parent syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false, err
	}
	parentPath := filepath.Dir(path)
	if err := syscall.Stat(parentPath, &parent); err != nil {
		return false, err
	}
	return st.Dev != parent.Dev, nil
}
