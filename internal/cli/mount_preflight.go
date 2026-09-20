package cli

// Mount durability and mountpoint policy helpers (GAP-103, GAP-107).
//
// These exist so the mount is a claim that stays TRUE rather than a one-time
// event: a user-writable mountpoint a non-root operator can actually use, a
// durability option set that is asserted present, and a preflight that refuses
// to mount something that is not there.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Durability option values. Kept as constants so the tests assert the exact
// contract rather than a copy of today's numbers.
const (
	// sshfsServerAliveInterval/Count give a dead peer a bound: with 15s × 3
	// an unreachable host is noticed in ~45s instead of hanging forever.
	sshfsServerAliveInterval = "15"
	sshfsServerAliveCountMax = "3"
	// sshfsConnectTimeout bounds the initial connect, matching cp/ssh/deploy.
	sshfsConnectTimeout = "10"
	// sshfsPreflightTimeout bounds the remote path check before mounting.
	sshfsPreflightTimeout = 20 * time.Second
)

// remotePathCheck is the seam the mount preflight runs through. It is a
// package var so tests can stub it exactly like sshfsRun, because the preflight
// shells out to ssh and no unit test can reach a real host.
var remotePathCheck = remotePathExists

// lastRemoteSourcePath returns the source path embedded in a stored sshfs
// command — the "<host>:<path>" argument. Used to default the preflight's
// remote path when the operator passes a mountpoint but not --path.
func lastRemoteSourcePath(mountCmd string) string {
	parts := strings.Fields(mountCmd)
	if len(parts) < 2 {
		return ""
	}
	src := parts[len(parts)-2] // the argument before the mountpoint
	if _, path, ok := strings.Cut(src, ":"); ok {
		return path
	}
	return ""
}

// durableSSHFSArgs returns the option set every mount must carry. It is a
// function rather than an inline literal so the test can assert the real
// contract instead of a copy of today's numbers.
//
// Each option closes a named disconnect failure mode:
//
//	reconnect                  a transport blip must not be permanent
//	ServerAliveInterval/Count  a dead peer must be noticed in ~45s, not hang
//	ConnectTimeout             the initial connect must be bounded
//	auto_unmount               a crash must not strand the mountpoint
//	dir_cache=no               a dead mount must not serve stale entries as if
//	                           the tree were live (the phantom-space failure)
func durableSSHFSArgs() []string {
	return []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "reconnect",
		"-o", "ServerAliveInterval=" + sshfsServerAliveInterval,
		"-o", "ServerAliveCountMax=" + sshfsServerAliveCountMax,
		"-o", "ConnectTimeout=" + sshfsConnectTimeout,
		"-o", "auto_unmount",
		"-o", "dir_cache=no",
	}
}

// safeDefaultMountPoint is the path policy used for help text; the real
// resolution lives in defaultMountPoint.
func safeDefaultMountPoint(agentID string) string {
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		return filepath.Join(run, "bunker", "mnt", agentID)
	}
	return filepath.Join("~", ".bunker", "mnt", agentID)
}

// defaultMountPoint resolves the mountpoint used when the operator does not
// pass one. The previous default was /mnt/bunker/<agent-id>, which is
// root-owned on a default host, so MkdirAll failed for every non-root user and
// the documented default was unusable (DF-BUNKER-14 residual).
//
// Preference order, each validated as writable before it is returned:
//  1. $BUNKER_MOUNT_ROOT (explicit operator override)
//  2. $XDG_RUNTIME_DIR/bunker/mnt/<agent-id> (per-user, per-boot, private)
//  3. ~/.bunker/mnt/<agent-id> (per-user, persistent)
//
// A root-owned /mnt path is never chosen: an operator who wants it can pass it
// explicitly, and then the error names the cause instead of failing obscurely.
func defaultMountPoint(agentID string) (string, error) {
	if agentID == "" {
		return "", fmt.Errorf("mount: agent id is required")
	}

	var candidates []string
	if root := os.Getenv("BUNKER_MOUNT_ROOT"); root != "" {
		candidates = append(candidates, filepath.Join(root, agentID))
	}
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		candidates = append(candidates, filepath.Join(run, "bunker", "mnt", agentID))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".bunker", "mnt", agentID))
	}

	var lastErr error
	for _, dir := range candidates {
		// Create the mountpoint itself, private to this user. 0700 (not 0755)
		// is deliberate: only this user can traverse it, which is what keeps a
		// mount without allow_other private.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			lastErr = fmt.Errorf("create mount point %s: %w", dir, err)
			continue
		}
		if err := checkWritableDir(dir); err != nil {
			lastErr = err
			continue
		}
		return dir, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable per-user mount root; set BUNKER_MOUNT_ROOT")
	}
	return "", fmt.Errorf("mount: no writable mountpoint available (%w). Set BUNKER_MOUNT_ROOT to a directory you own, or pass an explicit mountpoint", lastErr)
}

// checkWritableDir verifies the directory is writable by actually creating and
// removing a probe file. os.Stat-based checks lie across NFS/FUSE boundaries.
func checkWritableDir(dir string) error {
	probe := filepath.Join(dir, ".bunker-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("mount point %s is not writable by this user: %w", dir, err)
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return nil
}

// stripAllowOption removes any of the "allow" family (allow_other, allow_root,
// ...) from an sshfs argument list, returning the cleaned list and the option
// it dropped.
//
// It exists because the daemon's generated command always requests allow_other
// (manager_spawn.go:583) while this CLI's mountpoint is private 0700. Passing
// it through would expose the agent's files to every local user for no benefit;
// refusing outright would break every real mount. Stripping is the correct
// middle: the mount still works, and the dropped option is reported.
func stripAllowOption(args []string) ([]string, string, bool) {
	out := make([]string, 0, len(args))
	var dropped string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-o" && i+1 < len(args) {
			kept, hit := filterAllowParts(args[i+1])
			if hit != "" {
				dropped = hit
			}
			if kept == "" {
				i++ // drop the now-empty -o <value> pair entirely
				continue
			}
			out = append(out, a, kept)
			i++
			continue
		}
		// Handle the joined "-oallow_other" / "-o allow_other,idmap=user" shapes.
		if kept, hit := filterAllowParts(a); hit != "" {
			dropped = hit
			if kept == "" {
				continue
			}
			out = append(out, kept)
			continue
		}
		out = append(out, a)
	}
	if dropped == "" {
		return args, "", false
	}
	return out, dropped, true
}

// filterAllowParts removes allow-family entries from a comma-separated option
// value, returning the remaining value and the first dropped option name.
func filterAllowParts(value string) (string, string) {
	parts := strings.Split(value, ",")
	var kept []string
	var dropped string
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if strings.HasPrefix(t, "allow_other") || strings.HasPrefix(t, "allow_root") {
			if dropped == "" {
				dropped = t
			}
			continue
		}
		kept = append(kept, p)
	}
	if dropped == "" {
		return value, ""
	}
	return strings.Join(kept, ","), dropped
}

// unsafeAllowOption reports whether an sshfs argument list requests any of the
// "allow" family (allow_other, allow_root, ...). Those widen access to the
// agent's files to other local users and cannot be un-done once the mount is
// up, so the mount refuses instead of silently producing a world-readable
// mountpoint.
func unsafeAllowOption(args []string) (string, bool) {
	for i, a := range args {
		v := a
		// Handle both "-o allow_other" and "-oallow_other" shapes.
		if a == "-o" && i+1 < len(args) {
			v = args[i+1]
		}
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "allow_other") ||
				strings.HasPrefix(part, "allow_root") ||
				part == "allow_other" {
				return part, true
			}
		}
	}
	return "", false
}

// remotePathExists checks the remote path exists and is a directory, using the
// same ssh identity/host the mount will use. This is the preflight that kills
// the silent-empty-tree failure: without it, mounting an agent whose home is
// empty (a re-created agent) or a --path that does not exist SUCCEEDS and
// presents an empty tree that looks mounted and correct.
//
// Returns a named error suitable for surfacing to the operator.
func remotePathExists(userAtHost, keyPath, remotePath string) error {
	if userAtHost == "" {
		return fmt.Errorf("mount preflight: no ssh target resolved")
	}
	if remotePath == "" {
		return fmt.Errorf("mount preflight: no remote path to check")
	}
	if keyPath == "" {
		return fmt.Errorf("mount preflight: no ssh key resolved")
	}

	// Single-quote the remote path so a space or shell metacharacter cannot
	// change the command shape; the path itself is validated by test -d.
	q := "'" + strings.ReplaceAll(remotePath, "'", `'\''`) + "'"
	cmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "ConnectTimeout="+sshfsConnectTimeout,
		"-i", keyPath,
		userAtHost,
		"test", "-d", q,
	)
	out, err := runWithTimeout(cmd, sshfsPreflightTimeout)
	if err != nil {
		// Distinguish "does not exist / not a directory" (test -d returned
		// non-zero) from "could not reach the host at all": they call for
		// different operator action.
		details := strings.TrimSpace(out)
		if details == "" {
			return fmt.Errorf("mount preflight: remote path %q does not exist or is not a directory on %s", remotePath, userAtHost)
		}
		return fmt.Errorf("mount preflight: cannot reach %s to verify %q: %s", userAtHost, remotePath, details)
	}
	return nil
}

// runWithTimeout runs a command with a hard deadline, returning combined output.
func runWithTimeout(cmd *exec.Cmd, timeout time.Duration) (string, error) {
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() {
		out, err := cmd.CombinedOutput()
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return string(r.out), r.err
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("timed out after %s", timeout)
	}
}
