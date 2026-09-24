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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const umountTimeout = 20 * time.Second

// mountTableTimeout bounds the mount(8) fallback used where /proc is absent.
const mountTableTimeout = 10 * time.Second

// procSelfMountsPath is the live mount table on Linux. It is read directly
// rather than shelling out to mount(8) because it is the kernel's own view and
// needs no subprocess.
const procSelfMountsPath = "/proc/self/mounts"

// execCommand is exec.Command behind a seam.
//
// The unmount path is the one place where a mistake costs data, and the tests
// for it inject a live-mount table that names a path OUTSIDE any temp dir the
// test owns. Running the real fusermount3/umount against such a path would be
// catastrophic if the table ever named something real, so the runner is
// replaceable and the tests assert the recorded argv instead of executing it.
var execCommand = exec.Command

func NewUmountCommand() *cobra.Command {
	var (
		serverName string
		force      bool
	)
	cmd := &cobra.Command{
		Use:   "umount <agent-id|mountpoint>",
		Short: "Unmount an agent's SSHFS mount (idempotent)",
		Long: `Unmount an SSHFS mount created by 'bunker mount'.

Accepts either the agent id or an explicit mountpoint path. An agent id is
resolved against the default mount roots AND against the live mount table, so a
mount made at a custom path (bunker mount <agent> /mnt/bunker/<agent>) is still
found and unmounted by agent id. Running it when nothing is mounted is SUCCESS:
cleanup must be safe to run twice, and after a crash you cannot be expected to
know how far the previous attempt got.

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
			return runUmount(cmd.OutOrStdout(), args[0], force)
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (unused; accepted for symmetry with mount)")
	cmd.Flags().BoolVar(&force, "force", false, "Allow a lazy unmount when the filesystem cannot be reached")
	return cmd
}

// runUmount is the whole command as a function of (target, force, output) so the
// resolution order below is driven directly by tests, without a cobra command in
// the way.
//
// Resolution order — each step is only reached when the previous one found
// nothing, and none of them may report success while a live mount for the agent
// still exists (DF-BUNKER-50):
//
//  1. an existing path argument IS the mountpoint (the caller knows best);
//  2. an agent id resolved under the default mount roots (+ namespaced layout);
//  3. the LIVE MOUNT TABLE, matched on the mount TARGET only, for any mount
//     whose path carries the agent id — this is the step that was missing, and
//     the reason `bunker mount <agent> /mnt/bunker/<agent>` followed by
//     `bunker umount <agent>` used to print "already clean" over a live mount.
//
// Only when all three find nothing is "already clean" allowed, and it is
// SUCCESS: cleanup must stay safe to run twice.
func runUmount(out io.Writer, target string, force bool) error {
	// 1/2. The declared target: an existing path is used as-is, otherwise the
	// argument is an agent id resolved under the mount roots.
	mountPoint := ""
	rootsPoint := ""
	explicitPath := ""
	if fi, err := os.Stat(target); err == nil && fi.IsDir() {
		mountPoint = target
		explicitPath = target
	} else if resolved, rerr := findMountPointForAgent(target); rerr == nil {
		mountPoint = resolved
		rootsPoint = resolved
	}

	// 3. Before trusting anything, ask the kernel where this agent is actually
	// mounted. A live mount anywhere on the box wins over a resolved guess:
	// unmounting the guess and reporting success is the false positive this row
	// is about.
	live, checked, err := liveMountpointsFor(target, explicitPath)
	if err != nil {
		return fmt.Errorf("cannot read the live mount table: %w", err)
	}
	if checked && len(live) > 1 {
		return fmt.Errorf("agent %q is mounted at more than one path: %s (unmount the one you mean explicitly, by path)",
			target, strings.Join(live, ", "))
	}
	if checked {
		if len(live) == 1 {
			if mountPoint != live[0] {
				fmt.Fprintf(out, "Resolved %s from the live mount table (not the default root)\n", live[0])
			}
			mountPoint = live[0]
		} else if mountPoint != "" && !liveMountTableShows(mountPoint) {
			// The resolved path is not a mount according to the live table;
			// fall back to asking the filesystem before believing it.
			mounted, merr := isMountPoint(mountPoint)
			if merr != nil {
				return fmt.Errorf("inspect %s: %w", mountPoint, merr)
			}
			if !mounted {
				mountPoint = ""
			}
		}
	}

	if mountPoint == "" {
		// Nothing is mounted for this agent anywhere the kernel can see, and
		// nothing was resolved under the mount roots. Safe to run twice, so
		// this is SUCCESS — and it says WHERE we looked, because "already
		// clean" over a live mount was exactly the bug.
		if rootsPoint != "" {
			// Clear a leftover empty directory left by a previous run so the
			// next mount starts clean; a non-empty one is left alone.
			_ = os.Remove(rootsPoint)
			fmt.Fprintf(out, "Nothing mounted at %s (already clean%s)\n", rootsPoint, unverifiedNote(checked))
			return nil
		}
		if checked {
			fmt.Fprintf(out, "Nothing mounted for %s (mount table checked: %s)\n", target, procSelfMountsPath)
		} else {
			fmt.Fprintf(out, "Nothing mounted for %s (mount table unreadable; checked the default mount roots only)\n", target)
		}
		return nil
	}

	// The mountpoint is known. When the live table could not be read, confirm
	// with the filesystem layer rather than acting on an assumption: unmounting
	// the wrong path is worse than refusing.
	if !checked {
		mounted, merr := isMountPoint(mountPoint)
		if merr != nil {
			return fmt.Errorf("inspect %s: %w", mountPoint, merr)
		}
		if !mounted {
			_ = os.Remove(mountPoint)
			fmt.Fprintf(out, "Nothing mounted at %s (already clean%s)\n", mountPoint, unverifiedNote(checked))
			return nil
		}
	}

	if err := unmount(mountPoint, force); err != nil {
		return err
	}
	// Remove the do-not-build marker with the mount: a stale marker on a
	// now-ordinary directory would refuse builds that are fine.
	_ = os.Remove(filepath.Join(mountPoint, MountMarkerName))
	fmt.Fprintf(out, "Unmounted %s\n", mountPoint)
	return nil
}

// findMountPointForAgent looks for an existing mountpoint for agentID across the
// same roots defaultMountPointForServer considers. Returns an error when
// nothing exists.
//
// Two layouts are searched, newest first:
//
//	<root>/<server>/<agent>   the namespaced layout (GAP-113)
//	<root>/<agent>            the pre-GAP-113 layout, still honoured so mounts
//	                          created before the change can still be cleaned up
//
// When an agent id exists under several servers this reports the ambiguity by
// name rather than picking one: unmounting the wrong tree silently is exactly
// the failure the namespace was added to prevent.
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
	agent := sanitizeMountComponent(agentID)

	var legacy []string
	var namespaced []string
	for _, root := range roots {
		if legacyPath := filepath.Join(root, agentID); pathExists(legacyPath) {
			legacy = append(legacy, legacyPath)
		}
		servers, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, s := range servers {
			if !s.IsDir() {
				continue
			}
			candidate := filepath.Join(root, s.Name(), agent)
			if pathExists(candidate) {
				namespaced = append(namespaced, candidate)
			}
		}
	}

	if len(namespaced) == 1 {
		return namespaced[0], nil
	}
	if len(namespaced) > 1 {
		return "", fmt.Errorf("agent %q is mounted from more than one server: %s (name the mountpoint explicitly to choose one)",
			agentID, strings.Join(namespaced, ", "))
	}
	if len(legacy) > 0 {
		return legacy[0], nil
	}
	return "", fmt.Errorf("no mountpoint for agent %q", agentID)
}

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
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

// unverifiedNote is the parenthetical appended to a clean report when the live
// mount table could not be read. The claim is still save-to-run-twice SUCCESS,
// but it can only vouch for the mount roots — not for a custom mountpoint it
// was unable to look for.
func unverifiedNote(checked bool) string {
	if checked {
		return ""
	}
	return "; mount table unreadable, so a custom mountpoint could not be checked"
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

	// Prefer fusermount for FUSE mounts (normal first, then the lazy -uz
	// retry); fall back to plain umount (then umount -l) for the rest.
	// The fallback chain is only reachable because runWithTimeout executes
	// each attempt exactly once (DF-BUNKER-38).
	if _, err := exec.LookPath("fusermount3"); err == nil {
		if out, err := runWithTimeout(execCommand("fusermount3", "-u", mountPoint), umountTimeout); err != nil {
			_ = out
			// Escalate to lazy only when the normal path could not reach the
			// filesystem (the stranded case).
			if out2, err2 := runWithTimeout(execCommand("fusermount3", "-uz", mountPoint), umountTimeout); err2 != nil {
				return fmt.Errorf("unmount %s failed (normal: %v; lazy: %v, output: %s)", mountPoint, err, err2, strings.TrimSpace(out2))
			}
		}
		return nil
	}

	if out, err := runWithTimeout(execCommand("umount", mountPoint), umountTimeout); err != nil {
		if out2, err2 := runWithTimeout(execCommand("umount", "-l", mountPoint), umountTimeout); err2 != nil {
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
		out, _ := runWithTimeout(execCommand(path, tool.args...), 10*time.Second)
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

// --- live mount table (DF-BUNKER-50) ---------------------------------------
//
// The default-roots search is a GUESS about where a mount lives. A mount made
// with an explicit mountpoint (`bunker mount <agent> /mnt/bunker/<agent>`) is
// not under any root that search knows about, so a cleanup that trusted it
// alone reported success over a live mount. These helpers read the kernel's own
// mount table instead.

// readMountTable returns the live mount table, one line per mount.
//
// It is a package var so tests can inject a fake table: exercising the real
// path would mean mounting a filesystem, and a regression test that needs root
// is a test that does not run.
//
// Field reference (proc(5), mount(8) mtab_format): the per-line fields are
//
//	<device> <mount point> <fstype> <options> <dump> <pass>
//
// with octal escapes (\040 for space, \011 tab, \012 newline, \134 backslash)
// in the first three fields.
var readMountTable = func() ([]string, error) {
	if data, err := os.ReadFile(procSelfMountsPath); err == nil {
		return strings.Split(string(data), "\n"), nil
	} else if !os.IsNotExist(err) {
		// The path exists but could not be read: report, never guess.
		return nil, err
	}

	if _, err := exec.LookPath("mount"); err != nil {
		return nil, fmt.Errorf("neither %s nor mount(8) is available to list mounts", procSelfMountsPath)
	}
	out, err := runWithTimeout(execCommand("mount"), mountTableTimeout)
	if err != nil {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// mountTableEntry is one parsed mount.
type mountTableEntry struct {
	mountPoint string
	fsType     string
}

// parseMountTable reads the live mount table into entries. Lines that do not
// carry at least three fields are skipped: /proc/self/mounts is NUL-free but a
// `mount` invocation may print a header or a warning, and those are not mounts.
//
// A read failure returns the error and NO entries, so callers can tell "the
// table is empty" (safe to report clean) from "the table is unavailable"
// (report, never guess).
func parseMountTable() ([]mountTableEntry, error) {
	lines, err := readMountTable()
	if err != nil {
		return nil, err
	}
	entries := make([]mountTableEntry, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		entries = append(entries, mountTableEntry{
			mountPoint: unescapeMountField(fields[1]),
			fsType:     fields[2],
		})
	}
	return entries, nil
}

// unescapeMountField decodes the octal escapes mount(8) uses for whitespace in
// a path: a mount AT "/mnt/with space" is written "/mnt/with\040space", and
// comparing the escaped form against a real path would never match.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// liveMountTableShows reports whether path has a live entry in the kernel mount
// table, matched on the mount TARGET and path components (never a substring).
func liveMountTableShows(path string) bool {
	entries, err := parseMountTable()
	if err != nil {
		return false
	}
	for _, e := range entries {
		if sameMountPath(e.mountPoint, path) {
			return true
		}
	}
	return false
}

// couldBeAgentMount limits which live mounts are candidates for an agent.
//
// Without it, `bunker umount abc12345` would consider ANY path containing the
// id — starting with the agent's own home directory, /home/bunker-abc12345,
// whose device is the root filesystem and not a mount of its own. That is the
// "agent id is a substring" trap the row warns about, and a false candidate
// here means a wrong-path unmount.
//
// The live table is still the source of truth for WHERE; this only decides
// WHICH entries are plausible. FUSE/network filesystems are always candidates
// (an sshfs mount carries no /home/bunker-<id> component), and so is any
// mountpoint that sits inside a bunker mount root.
func couldBeAgentMount(e mountTableEntry) bool {
	if isFuseOrNetworkFS(e.fsType) {
		return true
	}
	for _, root := range mountRoots() {
		if sameMountPath(e.mountPoint, root) || isUnder(e.mountPoint, root) {
			return true
		}
	}
	return false
}

// isFuseOrNetworkFS reports whether an fstype is a FUSE or network filesystem —
// the transports a bunker mount can be. A plain disk mount is never one of ours.
func isFuseOrNetworkFS(fsType string) bool {
	switch fsType {
	case "fuseblk", "fusectl", "fuse", "nfs", "nfs4", "cifs", "smb3", "cifs2", "sshfs":
		return true
	}
	return strings.HasPrefix(fsType, "fuse.")
}

// mountRoots returns the mount roots in the same order the resolution search
// uses, so an entry's "inside a root" test answers the same question.
func mountRoots() []string {
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
	return roots
}

// liveMountpointsFor answers "what is live for the thing the operator named"?
//
// Two shapes, because the argument has two meanings:
//
//   - explicitPath != "" — the operator named a mountpoint. Only that path
//     counts. Looking for path components of the argument here would match
//     every ancestor directory (asking about /mnt/bunker/<agent> must not
//     return the /mnt mount), so the explicit form asks about the exact path.
//   - otherwise — the argument is an agent id, and any plausible bunker mount
//     whose mountpoint carries that id as a path component counts.
func liveMountpointsFor(target, explicitPath string) (points []string, checked bool, err error) {
	if explicitPath != "" {
		entries, perr := parseMountTable()
		if perr != nil {
			return nil, false, nil
		}
		for _, e := range entries {
			if sameMountPath(e.mountPoint, explicitPath) {
				points = append(points, e.mountPoint)
			}
		}
		return points, true, nil
	}
	return agentMountpoints(target)
}

// agentMountpoints returns every live mount that is a bunker mount for agentID,
// in table order.
//
// checked is false when the live table could not be read at all; the caller
// must then fall back to what it can prove locally rather than concluding
// anything from an empty result. An error is returned when the table is
// unavailable or the agent id cannot be used as a path component, because in
// both cases the honest answer is "I cannot tell", not "nothing is mounted".
func agentMountpoints(agentID string) (points []string, checked bool, err error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, false, nil
	}
	if sanitizeMountComponent(agentID) == "" {
		return nil, false, fmt.Errorf("agent id %q has no usable characters to match a mountpoint", agentID)
	}

	entries, perr := parseMountTable()
	if perr != nil {
		return nil, false, nil
	}
	for _, e := range entries {
		if !couldBeAgentMount(e) {
			continue
		}
		if mountTargetHasAgent(e.mountPoint, agentID) {
			points = append(points, e.mountPoint)
		}
	}
	return points, true, nil
}

// mountTargetHasAgent reports whether mountPoint carries agentID as a whole path
// component. Decision by component, never by substring: agent "f0901fd3" matches
// /mnt/bunker/f0901fd3 and /mnt/bunker/bunker-las-03/f0901fd3, and does not match
// /home/bunker-f0901fd3-host or /mnt/bunker/f0901fd3-backup.
func mountTargetHasAgent(mountPoint, agentID string) bool {
	if agentID == "" {
		return false
	}
	for _, part := range strings.Split(filepath.Clean(mountPoint), string(filepath.Separator)) {
		if part == agentID {
			return true
		}
	}
	return false
}

// sameMountPath compares two mount paths after cleaning, so "/a/b/" and "/a/b"
// are one path.
func sameMountPath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// isUnder reports whether path sits below root (strict: root itself is not
// "under" root).
func isUnder(path, root string) bool {
	root = filepath.Clean(root)
	return strings.HasPrefix(filepath.Clean(path), root+string(filepath.Separator))
}

// errNotMounted is returned by callers that need to distinguish "nothing there"
// from a real failure.
var errNotMounted = errors.New("not mounted")
