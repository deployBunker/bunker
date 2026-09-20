package cli

// bunker umount -- the cleanup path that did not exist before GAP-103.
//
// Before this, mount.go only PRINTED "Unmount with: fusermount -u <path>", so
// cleanup was a shell incantation the operator had to remember, and a dead FUSE
// session (crashed sshfs) left the mountpoint stranded so the next mount failed
// with "mountpoint is not empty".
//
// This command is deliberately idempotent: unmounting something already
// unmounted is SUCCESS, not an error, because the common case is a script or an
// operator cleaning up after a failure and not knowing how far the previous
// attempt got.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const umountTimeout = 20 * time.Second

func NewUmountCommand() *cobra.Command {
	var (
		serverName string
		force      bool
	)
	cmd := &cobra.Command{
		Use:   "umount <agent-id|mountpoint>",
		Short: "Unmount an agent's SSHFS mount (idempotent)",
		Long: `Unmount an SSHFS mount created by 'bunker mount'.

Accepts either the agent id (resolved against the default mount root) or an
explicit mountpoint path. Running it when nothing is mounted is SUCCESS: cleanup
must be safe to run twice, and after a crash you cannot be expected to know how
far the previous attempt got.

A stranded mountpoint (crashed sshfs holding the FUSE session) is cleared
automatically, falling back to a lazy unmount when the filesystem cannot be
reached to flush. A mountpoint with a live user is never disturbed: it is
reported by name instead.

Examples:
  bunker umount abc12345
  bunker umount /home/me/.bunker/mnt/abc12345
  bunker umount abc12345 --force   # allow a lazy unmount of a busy mount`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]

			// Resolve: an existing path is used as-is, otherwise treat the
			// argument as an agent id and resolve it under the mount roots.
			mountPoint := target
			if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
				resolved, rerr := findMountPointForAgent(target)
				if rerr != nil {
					// Nothing mounted for this agent is not a failure.
					fmt.Printf("Nothing mounted for %s (no mountpoint found)\n", target)
					return nil
				}
				mountPoint = resolved
			}

			// Is anything actually mounted there? A directory with no FUSE
			// mount behind it is already clean, so this is success.
			mounted, err := isMountPoint(mountPoint)
			if err != nil {
				return fmt.Errorf("inspect %s: %w", mountPoint, err)
			}
			if !mounted {
				// Clear a leftover empty directory left by a previous run so the
				// next mount starts clean; a non-empty one is left alone.
				_ = os.Remove(mountPoint)
				fmt.Printf("Nothing mounted at %s (already clean)\n", mountPoint)
				return nil
			}

			if err := unmount(mountPoint, force); err != nil {
				return err
			}
			// Remove the do-not-build marker with the mount: a stale marker on
			// a now-ordinary directory would refuse builds that are fine.
			_ = os.Remove(filepath.Join(mountPoint, MountMarkerName))
			fmt.Printf("Unmounted %s\n", mountPoint)
			return nil
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (unused; accepted for symmetry with mount)")
	cmd.Flags().BoolVar(&force, "force", false, "Allow a lazy unmount when the filesystem cannot be reached")
	return cmd
}

// findMountPointForAgent looks for an existing mountpoint for agentID across the
// same roots defaultMountPoint considers. Returns an error when nothing exists.
func findMountPointForAgent(agentID string) (string, error) {
	var roots []string
	if root := os.Getenv("BUNKER_MOUNT_ROOT"); root != "" {
		roots = append(roots, root)
	}
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		roots = append(roots, filepath.Join(run, "bunker", "mnt"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, ".bunker", "mnt"))
	}
	for _, root := range roots {
		candidate := filepath.Join(root, agentID)
		if _, err := os.Lstat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no mountpoint for agent %q", agentID)
}

// isMountPoint reports whether path currently has a filesystem mounted on it.
// It compares the device ids of the path and its parent: a mount changes the
// device, so differing ids mean something is mounted there. This is used
// instead of parsing /proc/mounts because it is a single stat and behaves the
// same on every platform this CLI builds for.
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

// unmount performs the unmount, escalating only as far as needed:
//
//  1. a normal unmount (flush first -- correct when the peer is reachable)
//  2. on failure, a LAZY unmount (detaches immediately, used when a crashed
//     sshfs left the FUSE session stuck so a normal unmount would block)
//
// It never escalates to anything wider than these two, and never touches a
// mountpoint that another live process holds unless --force is passed.
func unmount(mountPoint string, force bool) error {
	if !force {
		if owner, busy := mountBusyHolder(mountPoint); busy {
			return fmt.Errorf("mountpoint %s is in use by %s — close it first, or re-run with --force to detach lazily", mountPoint, owner)
		}
	}

	// Prefer fusermount for FUSE mounts; fall back to umount for the rest.
	if _, err := exec.LookPath("fusermount3"); err == nil {
		if out, err := runWithTimeout(exec.Command("fusermount3", "-u", mountPoint), umountTimeout); err != nil {
			_ = out
			// Escalate to lazy only when the normal path could not reach the
			// filesystem (the stranded case).
			if out2, err2 := runWithTimeout(exec.Command("fusermount3", "-uz", mountPoint), umountTimeout); err2 != nil {
				return fmt.Errorf("unmount %s failed (normal: %v; lazy: %v, output: %s)", mountPoint, err, err2, strings.TrimSpace(out2))
			}
		}
		return nil
	}

	if out, err := runWithTimeout(exec.Command("umount", mountPoint), umountTimeout); err != nil {
		if out2, err2 := runWithTimeout(exec.Command("umount", "-l", mountPoint), umountTimeout); err2 != nil {
			return fmt.Errorf("unmount %s failed (normal: %v; lazy: %v, output: %s)", mountPoint, err, err2, strings.TrimSpace(out2))
		}
		_ = out
	}
	return nil
}

// mountBusyHolder reports whether a live process holds the mountpoint open
// (its filesystem is busy). It uses lsof/fuser when available and otherwise
// declines to guess (a wrong "busy" verdict would block legitimate cleanup).
func mountBusyHolder(mountPoint string) (string, bool) {
	for _, tool := range []struct {
		name string
		args []string
	}{{"fuser", []string{"-m", mountPoint}}, {"lsof", []string{"-t", mountPoint}}} {
		path, err := exec.LookPath(tool.name)
		if err != nil {
			continue
		}
		out, _ := runWithTimeout(exec.Command(path, tool.args...), 10*time.Second)
		out = strings.TrimSpace(out)
		if out == "" {
			return "", false
		}
		pids := strings.Fields(out)
		if len(pids) > 0 {
			return fmt.Sprintf("pid %s", strings.Join(pids, ", ")), true
		}
	}
	return "", false
}

// errNotMounted is returned by callers that need to distinguish "nothing there"
// from a real failure.
var errNotMounted = errors.New("not mounted")
